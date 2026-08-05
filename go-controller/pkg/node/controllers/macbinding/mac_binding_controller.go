// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package macbinding implements the MAC Binding controller for OKEP-6691
// (Scalable ARP and NDP Broadcast Handling for UDN).
//
// This controller watches CDN (Cluster Default Network) Gateway Router
// MAC_Binding entries in OVN's Southbound database and makes the resolved MAC
// addresses available to UDN (User Defined Network) Gateway Routers. This
// avoids the need for each UDN GR to independently resolve next-hop MAC
// addresses via broadcast ARP/NDP, which does not scale as the number of UDNs
// grows.
//
// # Sync actions
//
// Two distinct sync actions are possible, abstracted behind the
// [macBindingSyncOps] interface:
//
//   - ARP responder flows: Program ARP responder flows on br-ex
//     via the openflowManager. This allows br-ex to answer ARP requests
//     from UDN GRs locally without forwarding broadcasts to the physical
//     network. Can be used for IPv4 IP family only.
//
//   - MAC_Binding mirroring: Mirror MAC_Binding entries to
//     UDN GR external ports in SBDB. OVN handles NDP natively, so the
//     controller propagates the resolved MAC binding directly into each
//     UDN's MAC_Binding table entries with a fresh timestamp (time.Now),
//     not the CDN entry's timestamp. Can be used for both IPv4 and IPv6 IP families.
//
// # Architecture
//
// The controller uses a [sync.Map] as a local cache of CDN MAC_Binding state,
// populated by a libovsdb event handler on the MAC_Binding table that reacts to
// adds, deletes, and MAC changes. Timestamp-only updates from the event handler
// are ignored. A periodic scan iterates the cache and enqueues any entry whose
// UDN counterpart may expire before the next scan for entries usiing
// MAC_Binding mirroring.
//
// Threre are two reconcilers:
//
//   - bootstrapReconciler (priority): handles new entries or mac updates at runtime.
//     These require prompt propagation so that UDN GR traffic is not black-holed.
//
//   - refreshReconciler: handles timestamp refreshes and deletes at runtime and initial sync.
//
// # UDN GR port tracking
//
// A separate [sync.Map] tracks known UDN GR external ports and their datapath
// UUIDs. The network controller adds and removes entries when UDN networks are
// created or deleted on this node. A libovsdb event handler on the PortBinding
// table only updates the datapath UUID for ports already in the map. The
// reconcile function reads the port map at reconcile time to build the list of
// target ports. If a port was deleted between enqueue and reconcile, it is
// simply absent from the list. Cleanup of MAC_Binding entries for deleted ports
// is handled by OVN: the datapath column is a strong UUID reference, so
// deleting the Datapath_Binding auto-deletes associated MAC_Binding rows.
//
// When a new network is added, all existing cache entries are enqueued for
// re-sync so that MAC_Binding rows are created for the new port. A future
// optimization could use per-port reconcile keys to sync only the new port's
// bindings without re-syncing all entries.
package macbinding

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// resyncAllMACBindings is a sentinel reconcile key that triggers a
// full cache iteration, enqueuing all [macBindingCacheEntry] entries
// for sync. Used when a structural change (new port, datapath update)
// requires re-syncing all entries without blocking the caller's
// thread.
const resyncAllMACBindings = "*"

// resyncNeighbors is a sentinel reconcile key that triggers a full
// neighbor re-sync. Used when the bridge link index changes (bridge
// recreation).
const resyncNeighbors = "**"

// scanPeriod is 3/16 of [types.GRMACBindingAgeThreshold] (~56s at
// 300s). It controls both the scan ticker interval and the refresh
// margin: now + scanPeriod >= syncedTimestamp + ageThreshold.
func scanPeriod() time.Duration {
	ageThreshold, _ := strconv.Atoi(types.GRMACBindingAgeThreshold)
	return time.Duration((3*ageThreshold)/16) * time.Second
}

// staggerSyncTimestamp returns a timestamp randomly spread across the
// age threshold window, leaving a margin of one scan period. This
// avoids a thundering herd at startup: entries with an earlier
// staggered timestamp will be refreshed sooner, spreading the SBDB
// write load across scan intervals.
func staggerSyncTimestamp(now int, scanPeriod time.Duration) int {
	ageThreshold, _ := strconv.Atoi(types.GRMACBindingAgeThreshold)
	margin := int(scanPeriod.Seconds())
	return now - rand.IntN(ageThreshold-margin)
}

// openFlowManager abstracts the openflow manager methods needed to
// program ARP responder flows on br-ex.
type openFlowManager interface {
	UpdateFlowCacheEntry(key string, flows []string)
	DeleteFlowsByKey(key string)
	RequestFlowSync()
}

// FlowManagerOps is a function-based implementation of openFlowManager.
// Callers in pkg/node/ construct it by binding the openflowManager methods.
type FlowManagerOps struct {
	UpdateExBridgeFlowCacheEntryFn func(key string, flows []string)
	DeleteExBridgeFlowsByKeyFn     func(key string)
	RequestFlowSyncFn              func()
}

func (f *FlowManagerOps) UpdateFlowCacheEntry(key string, flows []string) {
	f.UpdateExBridgeFlowCacheEntryFn(key, flows)
}

func (f *FlowManagerOps) DeleteFlowsByKey(key string) {
	f.DeleteExBridgeFlowsByKeyFn(key)
}

func (f *FlowManagerOps) RequestFlowSync() {
	f.RequestFlowSyncFn()
}

// PortInfo identifies a UDN GR external port and the datapath it
// belongs to. Both are needed to write MAC_Binding rows in SBDB: the
// datapath column is a mandatory strong UUID reference to
// Datapath_Binding, and logical_port is a free-form string.
type PortInfo struct {
	LogicalPort  string
	DatapathUUID string
}

// cacheEntry holds the IP and MAC of a CDN GR MAC_Binding. Used
// directly as the cache value for ARP flow reconciliation (IPv4),
// where sync is idempotent and needs no additional state.
// Embedded by [macBindingCacheEntry] for MAC_Binding reconciliation.
// Stored as a pointer in [sync.Map]; mac is accessed atomically.
type cacheEntry struct {
	ip  string
	mac atomic.Pointer[string]
}

// getMAC returns the current MAC address.
func (e *cacheEntry) getMAC() string {
	return *e.mac.Load()
}

// setMAC atomically updates the MAC address.
func (e *cacheEntry) setMAC(mac *string) {
	e.mac.Store(mac)
}

// macBindingCacheEntry extends [cacheEntry] with sync state for
// MAC_Binding reconciliation. Stored as a pointer in a [sync.Map];
// all mutable fields are accessed atomically.
//
// State encoding:
//   - Entry absent from cache: the SBDB entry was deleted; pending
//     propagation of delete to [macBindingSyncOps].
//   - syncedTimestamp == 0: the entry has never been synced; needs
//     bootstrap.
type macBindingCacheEntry struct {
	cacheEntry
	syncedTimestamp atomic.Int64
	// syncPending indicates the entry is enqueued for sync.
	// Ownership is claimed by the reconciler via
	// [macBindingCacheEntry.claimSync]; only the goroutine that
	// succeeds performs the sync.
	syncPending atomic.Bool
	// syncRecovery indicates the next sync must use
	// DeleteAndAddMACBinding. Set on sync errors and during initial
	// sync (where SBDB state is unknown).
	syncRecovery atomic.Bool
}

// getSyncTimestamp returns the SBDB timestamp at last successful sync.
// Zero means never synced.
func (e *macBindingCacheEntry) getSyncTimestamp() int {
	return int(e.syncedTimestamp.Load())
}

// markSync marks the entry as pending sync if not already pending.
// Returns true if newly marked; callers should only enqueue a
// reconcile when true.
func (e *macBindingCacheEntry) markSync() bool {
	return e.syncPending.CompareAndSwap(false, true)
}

// claimSync atomically clears syncPending, claiming ownership of the
// sync work. Returns true if claimed; only the caller that gets
// true should proceed with sync.
func (e *macBindingCacheEntry) claimSync() bool {
	return e.syncPending.CompareAndSwap(true, false)
}

// completeSync records a successful sync.
func (e *macBindingCacheEntry) completeSync(timestamp int) {
	e.syncedTimestamp.Store(int64(timestamp))
	e.syncRecovery.Store(false)
}

// failSync marks the entry for recovery and re-enqueues it.
func (e *macBindingCacheEntry) failSync() {
	e.syncRecovery.Store(true)
	e.syncPending.Store(true)
}

// MACBindingController watches CDN Gateway Router MAC_Binding entries
// in the OVN Southbound DB and makes the resolved MAC addresses
// available to UDN Gateway Routers using the configured
// [MacBindingSyncOps] implementation.
//
// It is safe to run exactly one instance per node.
type MACBindingController struct {
	sbClient       libovsdbclient.Client
	networkManager networkmanager.Interface
	syncOps        MacBindingSyncOps

	// cache stores *cacheEntry or *macBindingCacheEntry values keyed
	// by IP string. Written by event handlers and read by the
	// periodic scan; both run concurrently with the reconciler
	// workers. Pointer is swapped atomically on bridge recreation.
	cache atomic.Pointer[sync.Map]

	// ports stores PortInfo values keyed by logical port name.
	// Tracks known UDN GR external ports. Maintained by the
	// [networkmanager.NetworkRefReconciler] (add/remove) and the
	// PortBinding event handler (datapath UUID updates).
	ports sync.Map

	ipv4Enabled     bool
	ipv4UseARPFlows bool
	ipv6Enabled     bool

	// cdnGatewayPort is the CDN GR external port name for this node
	// (e.g. "rtoe-GR_<nodeName>"). Set at construction, never changes.
	cdnGatewayPort string

	// bridgeName is the external bridge name (e.g. "breth0") used to
	// filter neighbor events from the host neighbor table.
	bridgeName string

	// bridgeLinkIndex is the netlink link index of bridgeName.
	// Accessed via getBridgeLinkIndex/setBridgeLinkIndex so the
	// neighbor event goroutine sees changes when the bridge is
	// recreated.
	bridgeLinkIndex atomic.Int32

	nodeName string

	bootstrapReconciler controller.Reconciler
	refreshReconciler   controller.Reconciler
}

func (c *MACBindingController) getBridgeLinkIndex() int {
	return int(c.bridgeLinkIndex.Load())
}

func (c *MACBindingController) setBridgeLinkIndex(index int) {
	c.bridgeLinkIndex.Store(int32(index))
}

func (c *MACBindingController) getCache() *sync.Map {
	return c.cache.Load()
}

// NewMACBindingController creates a new MACBindingController.
//
// The caller must ensure the sbClient's monitor includes the
// MAC_Binding and PortBinding tables before calling Run. The event
// handlers are registered in Run so they are only active while the
// reconciler workers are ready.
func NewMACBindingController(
	sbClient libovsdbclient.Client,
	networkManager networkmanager.Interface,
	ofm openFlowManager,
	nodeName string,
	bridgeName string,
	ipv4Enabled bool,
	ipv4UseARPFlows bool,
	ipv6Enabled bool,
) *MACBindingController {
	c := &MACBindingController{
		sbClient:        sbClient,
		networkManager:  networkManager,
		syncOps:         NewMACBindingSyncOps(sbClient, ofm),
		nodeName:        nodeName,
		bridgeName:      bridgeName,
		ipv4Enabled:     ipv4Enabled,
		ipv4UseARPFlows: ipv4UseARPFlows,
		ipv6Enabled:     ipv6Enabled,
		cdnGatewayPort:  types.GWRouterToExtSwitchPrefix + (&util.DefaultNetInfo{}).GetNetworkScopedGWRouterName(nodeName),
	}
	c.cache.Store(&sync.Map{})
	c.bootstrapReconciler = controller.NewReconciler(
		"mac-binding-bootstrap",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcile,
			Threadiness: 1,
		},
	)
	c.refreshReconciler = controller.NewReconciler(
		"mac-binding-refresh",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcile,
			Threadiness: 1,
		},
	)
	return c
}

// Run starts the controller. It registers libovsdb event handlers,
// performs an initial sync of existing MAC_Binding entries, starts the
// reconciler workers, and runs the periodic scan. It blocks until
// stopCh is closed.
func (c *MACBindingController) Run(stopCh <-chan struct{}) error {
	klog.Infof("Starting the node mac-binding controller on %s with %s ipv4Enabled %v, ipv4UseARPFlows %v, ipv6Enabled %v", c.nodeName, c.bridgeName, c.ipv4Enabled, c.ipv4UseARPFlows, c.ipv6Enabled)
	link, err := netlink.LinkByName(c.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", c.bridgeName, err)
	}
	c.setBridgeLinkIndex(link.Attrs().Index)

	// c.registerMACBindingEventHandler()
	c.registerPortEventHandler()

	if err := c.registerNeighEventHandler(stopCh); err != nil {
		return err
	}

	if err := controller.StartWithInitialSync(
		func() error { return c.neighborSync(c.getBridgeLinkIndex()) },
		c.bootstrapReconciler,
		c.refreshReconciler,
	); err != nil {
		return err
	}

	period := scanPeriod()
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.scan()
		case <-stopCh:
			controller.Stop(c.bootstrapReconciler, c.refreshReconciler)
			return nil
		}
	}
}

// registerMACBindingEventHandler registers libovsdb event handlers
// for the MAC_Binding table, filtered to this node's CDN GR port.
// Only structural changes (add, delete, MAC change) are acted on;
// timestamp-only updates are left to the periodic scan.
func (c *MACBindingController) registerMACBindingEventHandler() {
	handleEntryAndEnqueue := func(ip string, mac *string, reconciler controller.Reconciler, create bool) {
		old, loaded := c.getCache().Load(ip)
		if !loaded {
			if !create {
				return
			}
			c.getCache().Store(ip, c.newCacheEntry(ip, mac))
			reconciler.Reconcile(ip)
			return
		}

		switch entry := old.(type) {
		case *cacheEntry:
			entry.setMAC(mac)
			reconciler.Reconcile(ip)
		case *macBindingCacheEntry:
			entry.setMAC(mac)
			if entry.markSync() {
				reconciler.Reconcile(ip)
			}
		}
	}

	c.sbClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
		AddFunc: func(table string, m model.Model) {
			if table != sbdb.MACBindingTable {
				return
			}
			mb := m.(*sbdb.MACBinding)
			if !c.isCDNGatewayPort(mb.LogicalPort) || !c.isIPFamilyEnabled(mb.IP) {
				return
			}
			handleEntryAndEnqueue(mb.IP, &mb.MAC, c.bootstrapReconciler, true)
		},
		UpdateFunc: func(table string, old model.Model, new model.Model) {
			if table != sbdb.MACBindingTable {
				return
			}
			oldMB := old.(*sbdb.MACBinding)
			newMB := new.(*sbdb.MACBinding)
			if !c.isCDNGatewayPort(newMB.LogicalPort) || !c.isIPFamilyEnabled(newMB.IP) {
				return
			}
			if oldMB.MAC == newMB.MAC {
				return
			}
			handleEntryAndEnqueue(newMB.IP, &newMB.MAC, c.bootstrapReconciler, false)
		},
		DeleteFunc: func(table string, m model.Model) {
			if table != sbdb.MACBindingTable {
				return
			}
			mb := m.(*sbdb.MACBinding)
			if !c.isCDNGatewayPort(mb.LogicalPort) || !c.isIPFamilyEnabled(mb.IP) {
				return
			}
			c.getCache().Delete(mb.IP)
			c.refreshReconciler.Reconcile(mb.IP)
		},
	})
}

// registerPortEventHandler registers libovsdb event handlers for the
// PortBinding table. It only updates the datapath UUID for ports
// already in the ports map — add/remove is done by
// [MACBindingController.Reconcile].
func (c *MACBindingController) registerPortEventHandler() {
	updateDatapath := func(logicalPort, datapath string) {
		old, loaded := c.ports.Load(logicalPort)
		if !loaded {
			return
		}
		pi := old.(PortInfo)
		if pi.DatapathUUID == datapath {
			return
		}
		pi.DatapathUUID = datapath
		c.ports.Store(logicalPort, pi)
		c.bootstrapReconciler.Reconcile(resyncAllMACBindings)
	}

	c.sbClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
		AddFunc: func(table string, m model.Model) {
			if table == sbdb.PortBindingTable {
				pb := m.(*sbdb.PortBinding)
				updateDatapath(pb.LogicalPort, pb.Datapath)
			}
		},
		UpdateFunc: func(table string, _ model.Model, new model.Model) {
			if table == sbdb.PortBindingTable {
				pb := new.(*sbdb.PortBinding)
				updateDatapath(pb.LogicalPort, pb.Datapath)
			}
		},
	})
}

// Reconcile implements [networkmanager.NetworkRefReconciler]. It adds
// or removes UDN GR external ports from the ports map based on
// network activity. When a new port is added, all cache entries are
// enqueued for re-sync.
func (c *MACBindingController) Reconcile(node, networkName string) {
	if node != c.nodeName {
		return
	}

	netInfo := c.networkManager.GetNetwork(networkName)
	if netInfo == nil {
		return
	}
	portName := types.GWRouterToExtSwitchPrefix + netInfo.GetNetworkScopedGWRouterName(c.nodeName)

	if c.networkManager.NodeHasNetwork(c.nodeName, networkName) {
		if _, loaded := c.ports.LoadOrStore(portName, PortInfo{LogicalPort: portName}); !loaded {
			c.bootstrapReconciler.Reconcile(resyncAllMACBindings)
		}
	} else {
		c.ports.Delete(portName)
	}
}

// HandleLinkEvent should be called from the linkManager callback when
// the external bridge link changes. If the link index changed (bridge
// was recreated), it enqueues a [resyncNeighbors] sentinel for the
// reconciler to handle.
func (c *MACBindingController) HandleLinkEvent(link netlink.Link) {
	if link.Attrs().Name != c.bridgeName {
		return
	}
	if link.Attrs().Index == c.getBridgeLinkIndex() {
		return
	}
	c.bootstrapReconciler.Reconcile(resyncNeighbors)
}

// initialSync lists all existing CDN GR MAC_Binding rows from SBDB
// and populates the cache with syncRecovery and syncPending flags.
// Uses the refreshReconciler — same as [MACBindingController.scan].
func (c *MACBindingController) initialSync() error {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()

	var mbs []*sbdb.MACBinding
	err := c.sbClient.WhereCache(func(mb *sbdb.MACBinding) bool {
		return c.isCDNGatewayPort(mb.LogicalPort) && c.isIPFamilyEnabled(mb.IP)
	}).List(ctx, &mbs)
	if err != nil {
		return err
	}

	entries := make(map[string]string, len(mbs))
	for _, mb := range mbs {
		entries[mb.IP] = mb.MAC
	}

	return c.syncEntries(entries)
}

// isValidNeighState returns true if the neighbor state indicates a
// resolved entry with a valid MAC address.
func isValidNeighState(state int) bool {
	return state&(netlink.NUD_REACHABLE|netlink.NUD_STALE|netlink.NUD_DELAY|netlink.NUD_PROBE) != 0
}

// neighborSync lists all existing neighbors on the external bridge
// and populates the cache via [MACBindingController.syncEntries].
func (c *MACBindingController) neighborSync(linkIndex int) error {
	neighs, err := netlink.NeighList(linkIndex, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("failed to list neighbors: %w", err)
	}

	entries := make(map[string]string)
	for i := range neighs {
		neigh := &neighs[i]
		if !isValidNeighState(neigh.State) || len(neigh.HardwareAddr) == 0 {
			continue
		}
		ip := neigh.IP.String()
		if !c.isIPFamilyEnabled(ip) {
			continue
		}
		entries[ip] = neigh.HardwareAddr.String()
	}

	return c.syncEntries(entries)
}

// syncEntries populates the cache from a map of IP→MAC entries.
// ARP flow entries are synced in batch via
// [macBindingSyncOps.EnsureARPFlows]. MAC_Binding entries are marked
// for recovery and enqueued via [resyncAllMACBindings].
func (c *MACBindingController) syncEntries(entries map[string]string) error {
	arpFlows := make(map[string]string)
	hasMACBindings := false

	for ip, mac := range entries {
		entry := c.newCacheEntry(ip, &mac)
		if mbe, ok := entry.(*macBindingCacheEntry); ok {
			// State is unknown from a previous run; use recovery
			// (DeleteAndAddMACBinding) to handle both exists and
			// not-exists.
			mbe.failSync()
			hasMACBindings = true
		} else {
			arpFlows[ip] = mac
		}
		c.getCache().Store(ip, entry)
	}

	if len(arpFlows) > 0 {
		if err := c.syncOps.SyncARPFlows(arpFlows); err != nil {
			return err
		}
	}

	if hasMACBindings {
		c.refreshReconciler.Reconcile(resyncAllMACBindings)
	}

	return nil
}

// registerNeighEventHandler subscribes to netlink neighbor events on
// the external bridge and updates the cache for adds, MAC changes,
// and deletes. State transitions without MAC change are ignored.
func (c *MACBindingController) registerNeighEventHandler(stopCh <-chan struct{}) error {
	neighCh := make(chan netlink.NeighUpdate)
	if err := netlink.NeighSubscribeWithOptions(neighCh, stopCh, netlink.NeighSubscribeOptions{
		ErrorCallback: func(err error) {
			klog.Warningf("Neighbor subscribe error: %v", err)
		},
	}); err != nil {
		return fmt.Errorf("failed to subscribe to neighbor events: %w", err)
	}

	go func() {
		for update := range neighCh {
			if update.LinkIndex != c.getBridgeLinkIndex() {
				continue
			}
			ip := update.IP.String()
			if !c.isIPFamilyEnabled(ip) {
				continue
			}

			if update.Type == unix.RTM_DELNEIGH || !isValidNeighState(update.State) {
				c.getCache().Delete(ip)
				c.refreshReconciler.Reconcile(ip)
				continue
			}

			if len(update.HardwareAddr) == 0 {
				continue
			}

			mac := update.HardwareAddr.String()
			old, loaded := c.getCache().Load(ip)
			if !loaded {
				c.getCache().Store(ip, c.newCacheEntry(ip, &mac))
				c.bootstrapReconciler.Reconcile(ip)
				continue
			}

			switch entry := old.(type) {
			case *cacheEntry:
				if entry.getMAC() == mac {
					continue
				}
				entry.setMAC(&mac)
				c.bootstrapReconciler.Reconcile(ip)
			case *macBindingCacheEntry:
				if entry.getMAC() == mac {
					continue
				}
				entry.setMAC(&mac)
				if entry.markSync() {
					c.bootstrapReconciler.Reconcile(ip)
				}
			}
		}
		klog.Warning("Neighbor event channel closed")
	}()

	return nil
}

// scan iterates the cache and enqueues MAC_Binding entries that need
// a timestamp refresh. ARP flow entries ([cacheEntry]) are skipped
// since they are idempotent and need no periodic refresh.
func (c *MACBindingController) scan() {
	ageThreshold, _ := strconv.Atoi(types.GRMACBindingAgeThreshold)
	scanSecs := int(scanPeriod().Seconds())
	now := int(time.Now().Unix())

	c.getCache().Range(func(_, value any) bool {
		entry, ok := value.(*macBindingCacheEntry)
		if !ok {
			return true
		}

		if now+scanSecs >= entry.getSyncTimestamp()+ageThreshold {
			if entry.markSync() {
				c.refreshReconciler.Reconcile(entry.ip)
			}
		}

		return true
	})
}

// reconcile is the reconcile function shared by both the bootstrap and
// refresh reconcilers. It dispatches to
// [MACBindingController.reconcileARPFlow] or
// [MACBindingController.reconcileMACBinding] based on the configured
// reconciliation mode for the IP family.
func (c *MACBindingController) reconcile(ip string) error {
	if ip == resyncAllMACBindings {
		return c.reconcileAllMACBindings()
	}
	if ip == resyncNeighbors {
		return c.reconcileNeighbors()
	}

	isIPv6 := utilnet.IsIPv6String(ip)
	if isIPv6 && !c.ipv6Enabled {
		return nil
	}
	if !isIPv6 && !c.ipv4Enabled {
		return nil
	}
	if !isIPv6 && c.ipv4UseARPFlows {
		return c.reconcileARPFlow(ip)
	}
	return c.reconcileMACBinding(ip)
}

// reconcileAllMACBindings iterates the cache and enqueues all
// [macBindingCacheEntry] entries for sync via the bootstrap
// reconciler. Called when a structural change (new port, datapath
// update) requires re-syncing all entries.
func (c *MACBindingController) reconcileAllMACBindings() error {
	c.getCache().Range(func(_, value any) bool {
		if entry, ok := value.(*macBindingCacheEntry); ok {
			if entry.markSync() {
				c.bootstrapReconciler.Reconcile(entry.ip)
			}
		}
		return true
	})
	return nil
}

// reconcileNeighbors handles bridge recreation by clearing the cache
// and re-syncing from the host neighbor table. [syncEntries] calls
// [macBindingSyncOps.SyncARPFlows] which replaces all ARP flows,
// implicitly removing stale entries.
func (c *MACBindingController) reconcileNeighbors() error {
	link, err := netlink.LinkByName(c.bridgeName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", c.bridgeName, err)
	}
	newIndex := link.Attrs().Index
	if newIndex == c.getBridgeLinkIndex() {
		return nil
	}
	klog.Infof("Bridge %s link index changed from %d to %d, re-syncing neighbors",
		c.bridgeName, c.getBridgeLinkIndex(), newIndex)
	c.setBridgeLinkIndex(0)
	c.cache.Store(&sync.Map{})
	c.setBridgeLinkIndex(newIndex)
	return c.neighborSync(newIndex)
}

// reconcileARPFlow reconciles a cache entry by ensuring the ARP
// responder flow is programmed on br-ex for the entry's IP and MAC.
// ARP flows are idempotent, so no claiming, timestamps, or recovery
// logic is needed. If the entry is absent from the cache, the flow
// is deleted.
func (c *MACBindingController) reconcileARPFlow(ip string) error {
	old, ok := c.getCache().Load(ip)
	if !ok {
		return c.syncOps.DeleteARPFlow(ip)
	}
	entry := old.(*cacheEntry)
	return c.syncOps.EnsureARPFlow(ip, entry.getMAC())
}

// reconcileMACBinding reconciles a cache entry by syncing the
// MAC_Binding to all UDN GR external ports.
//
// It claims the entry via [macBindingCacheEntry.claimSync]. If the
// entry is absent from the cache or not pending, this is a no-op.
//
// On error, calls [macBindingCacheEntry.failSync] (sets syncRecovery
// and re-enqueues). On success, calls
// [macBindingCacheEntry.completeSync] (sets syncedTimestamp, clears
// syncRecovery).
func (c *MACBindingController) reconcileMACBinding(ip string) error {
	old, ok := c.getCache().Load(ip)
	if !ok {
		return nil
	}

	entry := old.(*macBindingCacheEntry)
	if !entry.claimSync() {
		return nil
	}

	mac := entry.getMAC()
	recovery := entry.syncRecovery.Load()
	syncedTimestamp := entry.getSyncTimestamp()

	var ports []PortInfo
	c.ports.Range(func(_, value any) bool {
		pi := value.(PortInfo)
		if pi.DatapathUUID != "" {
			ports = append(ports, pi)
		}
		return true
	})

	if len(ports) == 0 {
		return nil
	}

	now := int(time.Now().Unix())
	var err error
	if recovery {
		err = c.syncOps.DeleteAndAddMACBinding(ip, mac, now, ports)
	} else if syncedTimestamp == 0 {
		err = c.syncOps.AddMACBinding(ip, mac, now, ports)
	} else {
		err = c.syncOps.UpdateMACBinding(ip, mac, now, ports)
	}

	if err != nil {
		entry.failSync()
		return err
	}

	if syncedTimestamp == 0 {
		entry.completeSync(staggerSyncTimestamp(now, scanPeriod()))
	} else {
		entry.completeSync(now)
	}
	return nil
}

// newCacheEntry creates the appropriate cache entry type based on the
// controller's IP family configuration. Returns a *[cacheEntry] for
// ARP flow reconciliation or a *[macBindingCacheEntry] with
// syncPending for MAC_Binding reconciliation.
func (c *MACBindingController) newCacheEntry(ip string, mac *string) any {
	isIPv6 := utilnet.IsIPv6String(ip)
	if !isIPv6 && c.ipv4UseARPFlows {
		e := &cacheEntry{ip: ip}
		e.mac.Store(mac)
		return e
	}
	e := &macBindingCacheEntry{
		cacheEntry: cacheEntry{ip: ip},
	}
	e.mac.Store(mac)
	e.markSync()
	return e
}

// isIPFamilyEnabled returns true if the controller is configured to
// handle the IP family of the given address.
func (c *MACBindingController) isIPFamilyEnabled(ip string) bool {
	if utilnet.IsIPv6String(ip) {
		return c.ipv6Enabled
	}
	return c.ipv4Enabled
}

// isCDNGatewayPort returns true if the logical port name matches the
// CDN Gateway Router external port for this node (e.g. "rtoe-GR_node1").
// UDN GR ports include a network name segment
// (e.g. "rtoe-GR_blue_node1") and are excluded.
func (c *MACBindingController) isCDNGatewayPort(logicalPort string) bool {
	return logicalPort == c.cdnGatewayPort
}
