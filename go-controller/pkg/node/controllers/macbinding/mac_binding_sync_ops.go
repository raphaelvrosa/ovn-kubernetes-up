// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"fmt"
	"strings"

	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
)

const (
	ARPResponderTable    = 0
	ARPResponderPriority = 40
	ARPResponderCookie   = "0x0306"
)

// macBindingSyncOps abstracts the operations used to propagate a CDN
// Gateway Router MAC_Binding entry to UDN Gateway Routers. A single
// implementation provides both ARP flow and MAC_Binding methods.
//
// The reconciler passes a fresh timestamp (time.Now) to MAC_Binding
// methods, not the CDN entry's timestamp, to avoid moving UDN
// timestamps backwards.
//
// There is no DeleteMACBinding: UDN entries age out naturally via
// OVN's mac_binding_age_threshold.
//
// The implementation must be safe for concurrent use from multiple
// reconciler workers.
type MacBindingSyncOps interface {
	// EnsureARPFlow programs an OpenFlow ARP responder rule on br-ex via
	// the openflowManager for the given (IP, MAC) pair. When a UDN GR
	// sends an ARP request for this IP, br-ex replies directly with
	// the known MAC, avoiding broadcast.
	EnsureARPFlow(ip, mac string) error
	SyncARPFlows(iptomac map[string]string) error

	// DeleteARPFlow removes the ARP responder flow for the given IP.
	DeleteARPFlow(ip string) error

	// AddMACBinding inserts a MAC_Binding row in SBDB for each UDN GR
	// external port in ports. Used when syncedTimestamp == 0 (entry
	// never synced).
	AddMACBinding(ip, mac string, timestamp int, ports []PortInfo) error

	// UpdateMACBinding conditionally updates existing MAC_Binding rows
	// for each UDN GR external port in ports. The OVSDB update must
	// include a timestamp < new_timestamp condition to handle the
	// residual race between computing time.Now and transaction commit:
	// the UDN GR's own statctrl may refresh the timestamp in that
	// window. A no-op update (0 rows, condition not met) is not an
	// error and must not trigger recovery.
	UpdateMACBinding(ip, mac string, timestamp int, ports []PortInfo) error

	// DeleteAndAddMACBinding performs a conditional delete followed by
	// a create in a single OVSDB transaction. Used for error recovery
	// when the controller's cache has diverged from SBDB state (e.g.
	// AddMACBinding failed with duplicate, or UpdateMACBinding failed
	// with row missing). Like UpdateMACBinding, the delete must be
	// conditioned on timestamp < new_timestamp to avoid replacing an
	// entry that the UDN's own statctrl has already refreshed to a
	// newer value. A no-op (condition not met) is not an error.
	DeleteAndAddMACBinding(ip, mac string, timestamp int, ports []PortInfo) error
}

type macSyncer struct {
	sbClient    libovsdbclient.Client
	flowManager openFlowManager
}

// NewMACBindingSyncOps returns a MacBindingSyncOps that operates on the SB MAC_Binding table
// and programs ARP responder flows on the external gateway bridge via flowManager.
func NewMACBindingSyncOps(sbClient libovsdbclient.Client, flowManager openFlowManager) MacBindingSyncOps {
	return &macSyncer{sbClient: sbClient, flowManager: flowManager}
}

// AddMACBinding inserts a MAC_Binding for each port in a single transaction.
func (b *macSyncer) AddMACBinding(ip, mac string, timestamp int, ports []PortInfo) error {
	allOps := make([]ovsdb.Operation, 0, len(ports))
	for _, port := range ports {
		AddMACBindingOps, err := b.sbClient.Create(&sbdb.MACBinding{
			IP:          ip,
			MAC:         mac,
			LogicalPort: port.LogicalPort,
			Datapath:    port.DatapathUUID,
			Timestamp:   timestamp,
		})
		if err != nil {
			return err
		}
		allOps = append(allOps, AddMACBindingOps...)
	}
	_, err := ops.TransactAndCheck(b.sbClient, allOps)
	return err
}

// UpdateMACBinding conditionally updates existing MAC_Binding entries for each port in a single transaction.
// The update only applies when the existing row's timestamp is strictly less than the new timestamp,
// preventing overwrites if the UDN GR's own statctrl has already refreshed to a newer value.
func (b *macSyncer) UpdateMACBinding(ip, mac string, timestamp int, ports []PortInfo) error {
	allOps := make([]ovsdb.Operation, 0, len(ports))
	for _, port := range ports {
		mb := &sbdb.MACBinding{
			LogicalPort: port.LogicalPort,
			IP:          ip,
			MAC:         mac,
			Datapath:    port.DatapathUUID,
			Timestamp:   timestamp,
		}
		updateOps, err := b.sbClient.WhereAll(mb, model.Condition{
			Field:    &mb.Timestamp,
			Function: ovsdb.ConditionLessThan,
			Value:    timestamp,
		}).Update(mb, &mb.MAC, &mb.Timestamp)
		if err != nil {
			return err
		}
		allOps = append(allOps, updateOps...)
	}
	_, err := ops.TransactAndCheck(b.sbClient, allOps)
	return err
}

// DeleteAndAddMACBinding removes the MAC_Binding entries for each port
// and adds them again in a single transaction.
func (b *macSyncer) DeleteAndAddMACBinding(ip, mac string, timestamp int, ports []PortInfo) error {
	allOps := make([]ovsdb.Operation, 0, len(ports))
	for _, port := range ports {
		mb := &sbdb.MACBinding{
			LogicalPort: port.LogicalPort,
			IP:          ip,
		}
		deleteOps, err := b.sbClient.Where(mb).Delete()
		if err != nil {
			return err
		}
		allOps = append(allOps, deleteOps...)

		AddMACBindingOps, err := b.sbClient.Create(&sbdb.MACBinding{
			IP:          ip,
			MAC:         mac,
			LogicalPort: port.LogicalPort,
			Datapath:    port.DatapathUUID,
			Timestamp:   timestamp,
		})
		if err != nil {
			return err
		}
		allOps = append(allOps, AddMACBindingOps...)
	}
	_, err := ops.TransactAndCheck(b.sbClient, allOps)
	return err
}

func macToHex(mac string) string {
	return strings.ReplaceAll(mac, ":", "")
}

func arpFlowCacheKey(ip string) string {
	return "MAC_BINDING_ARP_" + ip
}

func arpReplyFlow(ip, mac string) string {
	macHex := macToHex(mac)
	flow := fmt.Sprintf(
		"cookie=%s,table=%d,priority=%d,arp,arp_op=1,arp_tpa=%s,"+
			"actions=move:NXM_OF_ETH_SRC[]->NXM_OF_ETH_DST[],"+
			"mod_dl_src:%s,"+
			"load:0x2->NXM_OF_ARP_OP[],"+
			"move:NXM_NX_ARP_SHA[]->NXM_NX_ARP_THA[],"+
			"load:0x%s->NXM_NX_ARP_SHA[],"+
			"move:NXM_OF_ARP_TPA[]->NXM_NX_REG0[],"+
			"move:NXM_OF_ARP_SPA[]->NXM_OF_ARP_TPA[],"+
			"move:NXM_NX_REG0[]->NXM_OF_ARP_SPA[],"+
			"IN_PORT",
		ARPResponderCookie, ARPResponderTable, ARPResponderPriority,
		ip, mac, macHex,
	)
	return flow
}
func (b *macSyncer) EnsureARPFlow(ip, mac string) error {
	flow := arpReplyFlow(ip, mac)
	b.flowManager.UpdateFlowCacheEntry(arpFlowCacheKey(ip), []string{flow})
	b.flowManager.RequestFlowSync()
	return nil
}

func (b *macSyncer) SyncARPFlows(iptomac map[string]string) error {

	for ip, mac := range iptomac {
		flow := arpReplyFlow(ip, mac)
		b.flowManager.UpdateFlowCacheEntry(arpFlowCacheKey(ip), []string{flow})
	}
	b.flowManager.RequestFlowSync()
	return nil
}

func (b *macSyncer) DeleteARPFlow(ip string) error {
	b.flowManager.DeleteFlowsByKey(arpFlowCacheKey(ip))
	b.flowManager.RequestFlowSync()
	return nil
}
