// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package macbinding

import (
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
)

// portInfo identifies a UDN GR external port and its datapath.
type portInfo struct {
	LogicalPort  string
	DatapathUUID string
}

type macSyncer struct {
	sbClient libovsdbclient.Client
}

// macBindingSyncOps abstracts the SB MAC_Binding writes used to mirror a
// designated Gateway Router's binding onto target Gateway Routers.
type macBindingSyncOps interface {
	// AddMACBinding inserts a MAC_Binding row in SBDB for each UDN GR
	// external port in ports. Used when syncedTimestamp == 0 (entry
	// never synced).
	AddMACBinding(ip, mac string, timestamp int, ports []portInfo) error

	// UpdateMACBinding conditionally updates existing MAC_Binding rows
	// for each UDN GR external port in ports. The OVSDB update must
	// include a timestamp < new_timestamp condition to handle the
	// residual race between computing time.Now and transaction commit:
	// the UDN GR's own statctrl may refresh the timestamp in that
	// window. A no-op update (0 rows, condition not met) is not an
	// error and must not trigger recovery.
	UpdateMACBinding(ip, mac string, timestamp int, ports []portInfo) error

	// DeleteAndAddMACBinding performs a conditional delete followed by
	// a create in a single OVSDB transaction. Used for error recovery
	// when the controller's cache has diverged from SBDB state (e.g.
	// AddMACBinding failed with duplicate, or UpdateMACBinding failed
	// with row missing). Like UpdateMACBinding, the delete must be
	// conditioned on timestamp < new_timestamp to avoid replacing an
	// entry that the UDN's own statctrl has already refreshed to a
	// newer value. A no-op (condition not met) is not an error.
	DeleteAndAddMACBinding(ip, mac string, timestamp int, ports []portInfo) error

	// SetMACBindings writes (ip, mac) as a MAC_Binding row for each target
	// port in a single transaction, replacing any existing row for the same
	// (logical_port, ip). The caller passes a fresh timestamp (time.Now) so
	// mirrored entries never move a target's timestamp backwards.
	SetMACBindings(macBindings map[string]string, timestamp int, ports []portInfo) error
}

// NewMACBindingSyncOps returns a MacBindingSyncOps that operates on the SB MAC_Binding table.
func newMACBindingSyncOps(sbClient libovsdbclient.Client) macBindingSyncOps {
	return &macSyncer{sbClient: sbClient}
}

// AddMACBinding inserts a MAC_Binding for each port in a single transaction.
func (b *macSyncer) AddMACBinding(ip, mac string, timestamp int, ports []portInfo) error {
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
func (b *macSyncer) UpdateMACBinding(ip, mac string, timestamp int, ports []portInfo) error {
	allOps := make([]ovsdb.Operation, 0, len(ports))
	for _, port := range ports {
		mb := &sbdb.MACBinding{
			LogicalPort: port.LogicalPort,
			IP:          ip,
			MAC:         mac,
			Datapath:    port.DatapathUUID,
			Timestamp:   timestamp,
		}
		updateOps, err := b.sbClient.WhereAll(mb,
			model.Condition{
				Field:    &mb.Timestamp,
				Function: ovsdb.ConditionLessThan,
				Value:    timestamp,
			},
			model.Condition{
				Field:    &mb.LogicalPort,
				Function: ovsdb.ConditionEqual,
				Value:    port.LogicalPort,
			},
			model.Condition{
				Field:    &mb.IP,
				Function: ovsdb.ConditionEqual,
				Value:    ip,
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
func (b *macSyncer) DeleteAndAddMACBinding(ip, mac string, timestamp int, ports []portInfo) error {
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

// SetMACBindings removes the MAC_Binding for each port and re-creates it in a
// single transaction, so the write is idempotent regardless of whether a row
// already exists for the (logical_port, ip) pair.
func (b *macSyncer) SetMACBindings(macBindings map[string]string, timestamp int, ports []portInfo) error {
	allOps := make([]ovsdb.Operation, 0, 2*len(ports))
	for _, port := range ports {
		for ip, mac := range macBindings {
			deleteOps, err := b.sbClient.Where(&sbdb.MACBinding{
				LogicalPort: port.LogicalPort,
				IP:          ip,
			}).Delete()
			if err != nil {
				return err
			}
			allOps = append(allOps, deleteOps...)

			createOps, err := b.sbClient.Create(&sbdb.MACBinding{
				IP:          ip,
				MAC:         mac,
				LogicalPort: port.LogicalPort,
				Datapath:    port.DatapathUUID,
				Timestamp:   timestamp,
			})
			if err != nil {
				return err
			}
			allOps = append(allOps, createOps...)
		}
	}
	_, err := ops.TransactAndCheck(b.sbClient, allOps)
	return err
}
