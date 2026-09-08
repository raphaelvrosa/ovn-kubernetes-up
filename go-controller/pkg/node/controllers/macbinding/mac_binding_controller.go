// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package macbinding implements the MAC Binding mirror controller for OKEP-6691
// (Scalable ARP and NDP Broadcast Handling for UDN).
//
// Networks that share a physical uplink form a group. Within each group a
// single designated network ("source") resolves ARP/ND on the wire; OVN records
// the result in its Gateway Router's SB MAC_Binding rows. This controller
// mirrors those rows onto the SB MAC_Binding of every other ("target") Gateway
// Router in the group, so the targets never need to broadcast ARP/ND
// themselves.
//
//   - The default group (networks on the shared breth0, i.e. Uplink()=="")
//     always designates the CDN Gateway Router; its targets are the primary
//     L2/L3 UDNs and CUDNs present on the node.
//   - An uplink group (Uplink()!="") contains only CUDNs; openflow-manager
//     designates the source and informs the controller about it.
//
// The controller dynamically establishes SB MAC_Binding monitors for the
// sources. These monitors are only canceled when the datapath is deleted to
// ensure the related rows in the client cache have already been removed.
//
// Reconciliations happen on the following events:
// TODO: describe them
package macbinding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	libovsdbcache "github.com/ovn-kubernetes/libovsdb/cache"
	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/controller"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/sbdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// MACBindingController mirrors MAC bindings across networks.
type MACBindingController struct {
	sbClient        libovsdbclient.Client
	networkManager  networkmanager.Interface
	syncOps         macBindingSyncOps
	openflowManager node.OpenflowManager

	nodeName       string
	ipv4Enabled    bool
	ipv6Enabled    bool
	cdnGatewayPort string

	macBindingReconciler controller.Reconciler
	networkReconciler    controller.Reconciler
	monitorReconciler    controller.Reconciler

	// mutex for the maps
	sync.RWMutex
	// followers maps sources to followers. They are external GW router port
	// names. The controller syncs mac bindings from the sources to the
	// corresponding followers. The source is the CDN for the default bridge or
	// a designated CUDN by openflow manager for uplink bridges. Designated CUDN
	// sources can change dynamically and followers might not have known sources
	// in which case they are tracked with "unknown" key. While there is an
	// unknown source the controller does full network reconciliations since it
	// needs the complete picture to re-allocate.
	followers map[string]sets.Set[string]
	// ports maps network names to external GR port names existing in the SB
	// database. The controller ignores network events for networks already
	// tracked on this map. If a port binding is deleted from the SB database,
	// the entry on this map is deleted and the corresponding network reconciled
	// to handle its deletion.
	ports map[string]string
	// cookies maps datapath UUID to monitor cookie. The controller dynamically
	// establishes monitors
	cookies map[string]libovsdbclient.MonitorCookie
}

// NewMACBindingController creates a new MACBindingController.
func NewMACBindingController(
	sbClient libovsdbclient.Client,
	networkManager networkmanager.Interface,
	openflowManager node.OpenflowManager,
	nodeName string,
	ipv4Enabled bool,
	ipv6Enabled bool,
) *MACBindingController {
	c := &MACBindingController{
		sbClient:        sbClient,
		networkManager:  networkManager,
		syncOps:         newMACBindingSyncOps(sbClient),
		openflowManager: openflowManager,
		nodeName:        nodeName,
		ipv4Enabled:     ipv4Enabled,
		ipv6Enabled:     ipv6Enabled,
		cdnGatewayPort:  util.GetNetworkScopedGWRouterExtPortName(types.DefaultNetworkName, nodeName),
		followers:       map[string]sets.Set[string]{},
		ports:           map[string]string{},
		cookies:         map[string]libovsdbclient.MonitorCookie{},
	}

	c.macBindingReconciler = controller.NewReconciler(
		"mac-binding-reconciler",
		&controller.ReconcilerConfig{
			RateLimiter: controller.DefaultRateLimiter[string](),
			Reconcile:   c.reconcileMacBindings,
			Threadiness: 1,
			MaxAttempts: 11, // with default rate limiter, retry during ~10s
		},
	)

	c.networkReconciler = controller.NewReconciler(
		"mac-binding-network-reconciler",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcileNetwork,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)

	c.monitorReconciler = controller.NewReconciler(
		"mac-binding-monitor-reconciler",
		&controller.ReconcilerConfig{
			Reconcile:   c.reconcileMonitor,
			Threadiness: 1,
			MaxAttempts: controller.InfiniteAttempts,
		},
	)

	return c
}

// networkRefReconcilerFunc adapts a function to the
// networkmanager.NetworkRefReconciler interface.
type networkRefReconcilerFunc func(node, networkName string)

func (f networkRefReconcilerFunc) Reconcile(node, networkName string) { f(node, networkName) }

// Run starts the controller and blocks until stopCh is closed.
func (c *MACBindingController) Run(stopCh <-chan struct{}) error {
	klog.Info("Running MAC Binding controller...")

	// enqueue full network reconcile first
	c.enqueueAllNetworks()

	// register for events
	c.networkManager.RegisterNetworkRefReconciler(networkRefReconcilerFunc(func(node, networkName string) {
		if node != c.nodeName {
			return
		}
		c.enqueueNetwork(networkName)
	}))
	c.networkManager.RegisterNADReconciler(c.networkReconciler)
	c.openflowManager.RegisterUplinkCallback(c.ReconcileUplinkSource)
	c.registerSouthBoundEventHandlers()

	// start the reconcilers
	reconcilers := []controller.Reconciler{
		c.macBindingReconciler,
		c.networkReconciler,
		c.monitorReconciler,
	}
	if err := controller.Start(reconcilers...); err != nil {
		return fmt.Errorf("failed to start MAC Binding controller: %w", err)
	}
	defer controller.Stop(reconcilers...)

	<-stopCh
	klog.Info("Stopping MAC Binding controller...")
	return nil
}

func (c *MACBindingController) ReconcileUplinkSource(network string) {
	port := util.GetNetworkScopedGWRouterExtPortName(network, c.nodeName)
	if c.setUnknownSource(port) {
		c.enqueueNetwork(network)
	}
}

func (c *MACBindingController) ipFamilyEnabled(ip string) bool {
	if utilnet.IsIPv6String(ip) {
		return c.ipv6Enabled
	}
	return c.ipv4Enabled
}

func (c *MACBindingController) registerSouthBoundEventHandlers() {
	handleMacBinding := func(m model.Model) {
		mb := m.(*sbdb.MACBinding)
		if !c.ipFamilyEnabled(mb.IP) {
			return
		}
		if !c.tracksSource(mb.LogicalPort) {
			return
		}
		c.enqueueIP(mb.LogicalPort, mb.IP)
	}
	handleDatapathBinding := func(m model.Model) {
		dp := m.(*sbdb.DatapathBinding)
		c.enqueueMonitor(dp.UUID)
	}
	handlePortBinding := func(m model.Model, op string) {
		pb := m.(*sbdb.PortBinding)
		if !strings.HasPrefix(pb.LogicalPort, types.GWRouterToExtSwitchPrefix) {
			return
		}
		network := pb.ExternalIDs[types.NetworkExternalID]
		topology := pb.ExternalIDs[types.TopologyExternalID]
		if network != "" && !shouldTrackTopology(topology) {
			return
		}
		if pb.LogicalPort == c.cdnGatewayPort {
			network = types.DefaultNetworkName
		}
		if network == "" {
			return
		}
		if op != "d" || c.removePort(network) {
			c.enqueueNetwork(network)
		}
	}
	handle := func(op, table string, m model.Model) {
		switch {
		case table == sbdb.MACBindingTable && op != "d":
			handleMacBinding(m)
		case table == sbdb.PortBindingTable && op != "u":
			handlePortBinding(m, op)
		case table == sbdb.DatapathBindingTable && op == "d":
			handleDatapathBinding(m)
		}
	}
	c.sbClient.Cache().AddEventHandler(&libovsdbcache.EventHandlerFuncs{
		AddFunc:    func(table string, model model.Model) { handle("a", table, model) },
		UpdateFunc: func(table string, _, new model.Model) { handle("u", table, new) },
		DeleteFunc: func(table string, model model.Model) { handle("d", table, model) },
	})
}

func (c *MACBindingController) enqueueNetwork(network string) {
	c.networkReconciler.Reconcile(network)
}

func (c *MACBindingController) enqueueAllNetworks() {
	c.networkReconciler.Reconcile("")
}

func (c *MACBindingController) enqueueMonitor(uuid string) {
	c.monitorReconciler.Reconcile(uuid)
}

func (c *MACBindingController) enqueueFollower(follower string) {
	c.macBindingReconciler.Reconcile(follower)
}

// keySep separates the designated port from the IP in a reconcile key.
// GR external port names never contain it.
const keySep = "|"

func (c *MACBindingController) enqueueIP(source, ip string) {
	c.macBindingReconciler.Reconcile(source + keySep + ip)
}

// reconcileMacBindings mirrors either a single (designated, ip) binding or, when no IP is
// given, all of a designated's bindings.
func (c *MACBindingController) reconcileMacBindings(key string) error {
	port, ip, _ := strings.Cut(key, keySep)
	if ip == "" {
		return c.reconcileMacBindingsForFollower(port)
	}
	return c.reconcileMacBindingsFromSourceForIP(port, ip)
}

// reconcileMacBindingsFromSourceForIP mirrors the designated's (ip, mac) onto every target port.
func (c *MACBindingController) reconcileMacBindingsFromSourceForIP(source, ip string) error {
	followers := c.getFollowers(source)
	if len(followers) == 0 {
		return nil
	}

	mb := &sbdb.MACBinding{LogicalPort: source, IP: ip}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err := c.sbClient.Get(ctx, mb)
	if errors.Is(err, libovsdbclient.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get MAC_Binding for port %q IP %q: %w", source, ip, err)
	}

	klog.V(5).Infof("Mirroring %s -> %s from %s to %d follower(s)", ip, mb.MAC, source, len(followers))
	return c.setMacBindings(source, map[string]string{mb.IP: mb.MAC}, followers)
}

func (c *MACBindingController) reconcileMacBindingsForFollower(follower string) error {
	source := c.getSourceForFollower(follower)
	if source == "" {
		return nil
	}
	macBindings := map[string]string{}
	noop := []sbdb.MACBinding{}
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err := c.sbClient.WhereCache(func(mb *sbdb.MACBinding) bool {
		if mb.LogicalPort != source || !c.ipFamilyEnabled(mb.IP) {
			return false
		}
		macBindings[mb.IP] = mb.MAC
		return false
	}).List(ctx, &noop)
	if err != nil {
		return fmt.Errorf("failed to list MAC_Bindings for port %q: %w", source, err)
	}
	if len(macBindings) == 0 {
		return nil
	}
	klog.V(5).Infof("Mirroring %d IPs from %s to follower %s", len(macBindings), source, follower)
	return c.setMacBindings(source, macBindings, []string{follower})
}

func (c *MACBindingController) setMacBindings(source string, macBindings map[string]string, followers []string) error {
	var portsInfos []portInfo
	for _, follower := range followers {
		if source == follower {
			continue
		}
		pb, err := ops.GetPortBinding(c.sbClient, &sbdb.PortBinding{LogicalPort: follower})
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("failed to get port binding for port %q: %w", follower, err)
		}
		if pb.Datapath == "" {
			continue
		}
		portsInfos = append(portsInfos, portInfo{LogicalPort: follower, DatapathUUID: pb.Datapath})
	}
	if len(portsInfos) == 0 {
		return nil
	}
	nowMs := int(time.Now().UnixMilli())
	return c.syncOps.SetMACBindings(macBindings, nowMs, portsInfos)
}

func (c *MACBindingController) reconcileMonitor(key string) error {
	// key is either a datapath uuid for which to cancel a monitor
	cookie, monitored := c.isDatapathMonitored(key)
	if monitored {
		return c.cancelMonitor(key, cookie)
	}
	// or a source port to monitor mac bindings for
	err := c.ensureMonitor(key)
	if err != nil {
		return err
	}
	return nil
}

func (c *MACBindingController) ensureMonitor(port string) error {
	if !c.tracksSource(port) {
		return nil
	}

	pb, err := ops.GetPortBinding(c.sbClient, &sbdb.PortBinding{LogicalPort: port})
	if err != nil {
		return fmt.Errorf("failed to get port binding for port %q: %w", port, err)
	}

	datapath := pb.Datapath
	if datapath == "" {
		return nil
	}

	if _, monitored := c.isDatapathMonitored(datapath); monitored {
		return nil
	}

	mb := sbdb.MACBinding{}
	db := sbdb.DatapathBinding{}
	monitors := []libovsdbclient.MonitorOption{
		libovsdbclient.WithConditionalTable(
			&mb,
			[]model.Condition{
				{
					Field:    &mb.LogicalPort,
					Function: ovsdb.ConditionEqual,
					Value:    port,
				},
			},
			&mb.LogicalPort, &mb.IP, &mb.MAC, &mb.Timestamp,
		),
		libovsdbclient.WithConditionalTable(
			&db,
			[]model.Condition{
				{
					Field:    &db.UUID,
					Function: ovsdb.ConditionEqual,
					Value:    datapath,
				},
			},
			&db.TunnelKey,
		),
	}

	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	cookie, err := c.sbClient.Monitor(ctx, c.sbClient.NewMonitor(monitors...))
	if err != nil {
		return fmt.Errorf("failed to monitor MAC_Binding for port %q on datapath %s: %w", port, datapath, err)
	}
	klog.V(5).Infof("Established MAC_Binding monitor for source %q on datapath %s", port, datapath)

	c.Lock()
	defer c.Unlock()
	c.cookies[datapath] = cookie

	return nil
}

func (c *MACBindingController) cancelMonitor(datapath string, cookie libovsdbclient.MonitorCookie) error {
	ctx, cancel := context.WithTimeout(context.Background(), config.Default.OVSDBTxnTimeout)
	defer cancel()
	err := c.sbClient.MonitorCancel(ctx, cookie)
	if err != nil {
		return fmt.Errorf("failed to cancel MAC_Binding monitor for datapath %s: %w", datapath, err)
	}
	klog.V(5).Infof("Canceled MAC_Binding monitor for datapath %s", datapath)

	c.Lock()
	defer c.Unlock()
	delete(c.cookies, datapath)

	return nil
}

func (c *MACBindingController) isDatapathMonitored(datapath string) (libovsdbclient.MonitorCookie, bool) {
	c.RLock()
	defer c.RUnlock()
	cookie, exists := c.cookies[datapath]
	return cookie, exists
}

// reconcileNetwork reacts to a NAD change. When the network still resolves, its
// uplink tells us which group to update; when it is gone we cannot tell, so a
// GC sweep re-checks every tracked network.
func (c *MACBindingController) reconcileNetwork(key string) error {
	if key == "" {
		// special key "" reconciles all
		return c.doReconcileAllNetworks()
	}

	// the key can be a NAD namespaced name or a network name
	var netInfo util.NetInfo
	namespace, _, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("failed to split meta namespace key %q: %w", key, err)
	}
	switch namespace {
	case "":
		netInfo = c.networkManager.GetNetwork(key)
	default:
		netInfo = c.networkManager.GetNetInfoForNADKey(key)
	}

	if netInfo == nil && namespace != "" {
		// deletes are handled with port binding events queueing networks, so
		// ignore NAD events
		return nil
	}

	// we can assume the key is the network name for our own cache lookups if it
	// doesn't exist
	networkName := key
	if netInfo != nil {
		networkName = netInfo.GetNetworkName()
	}
	portName := util.GetNetworkScopedGWRouterExtPortName(networkName, c.nodeName)

	if netInfo != nil {
		// already aware of the network, noop
		if c.tracksNetwork(networkName) {
			return nil
		}
		// this might be a new source for our unknown source ports, full reconcile (enqueued
		// to dedup)
		if c.hasUnknownSource() {
			c.enqueueAllNetworks()
		}
	}

	// a port acting as source might have been deleted (even if network manager
	// isn't aware yet), full reconcile (enqueued to dedup)
	if c.tracksSource(portName) {
		c.enqueueAllNetworks()
	}

	return c.doReconcileNetworks(networkName)
}

func (c *MACBindingController) doReconcileAllNetworks() error {
	return c.doReconcileNetworks()
}

func (c *MACBindingController) doReconcileNetworks(networks ...string) error {
	ports := sets.New[string]()
	portToUplink := map[string]string{}
	portToNetwork := map[string]string{}
	var err error
	switch {
	case len(networks) == 0:
		// no network means reconcile all networks
		err = c.getAllNetworkInfo(ports, portToUplink, portToNetwork)
	default:
		err = c.getNetworksInfo(networks, ports, portToUplink, portToNetwork)
	}
	if err != nil {
		return fmt.Errorf("failed to gather network info: %w", err)
	}

	// TODO openflow manager needs to give us this map of uplink->mirreored network
	uplinkToSource := c.openflowManager.GetMacBindingSourceForUplinks()
	// add CDN as its static
	uplinkToSource[""] = c.cdnGatewayPort

	// track ports for added and removed networks
	knownPorts := c.getPorts()
	newPorts := ports.Difference(knownPorts)
	unknownPorts := c.getAllFollowers().Difference(knownPorts)

	// filter out ports that don't exist in the SB DB.
	validNewPorts, err := c.validatePorts(newPorts)
	if err != nil {
		return err
	}

	// skip on no additions or deletions and no unknown sources
	if unknownPorts.Len() == 0 && validNewPorts.Len() == 0 && !c.hasUnknownSource() {
		return nil
	}

	newSources, newFollowers := c.updateFollowers(validNewPorts, unknownPorts, portToUplink, portToNetwork, uplinkToSource)

	// monitor mac bindings of new sources
	// will also sync followers for these sources
	for _, source := range newSources.UnsortedList() {
		c.enqueueMonitor(source)
	}

	// sync mac bindings of new followers with an already known source
	for follower := range newFollowers {
		c.enqueueFollower(follower)
	}

	return nil
}

func (c *MACBindingController) getAllNetworkInfo(ports sets.Set[string], portToUplink, portToNetwork map[string]string) error {
	// pre-fill default network info, network manager won't iterate through it
	ports.Insert(c.cdnGatewayPort)
	portToUplink[c.cdnGatewayPort] = ""
	portToNetwork[c.cdnGatewayPort] = types.DefaultNetworkName

	return c.networkManager.DoWithLock(func(network util.NetInfo) error {
		if !shouldTrackNetwork(network) {
			return nil
		}
		port := types.GWRouterToExtSwitchPrefix + network.GetNetworkScopedGWRouterName(c.nodeName)
		ports.Insert(port)
		portToUplink[port] = network.Uplink()
		portToNetwork[port] = network.GetNetworkName()
		return nil
	})
}

func (c *MACBindingController) getNetworksInfo(networks []string, ports sets.Set[string], portToUplink, portToNetwork map[string]string) error {
	for _, network := range networks {
		netInfo := c.networkManager.GetNetwork(network)
		if netInfo == nil || !shouldTrackNetwork(netInfo) {
			continue
		}
		port := types.GWRouterToExtSwitchPrefix + netInfo.GetNetworkScopedGWRouterName(c.nodeName)
		ports.Insert(port)
		portToUplink[port] = netInfo.Uplink()
		portToNetwork[port] = netInfo.GetNetworkName()
	}
	return nil
}

func (c *MACBindingController) validatePorts(ports sets.Set[string]) (sets.Set[string], error) {
	validPorts := sets.New[string]()
	for port := range ports {
		_, err := ops.GetPortBinding(c.sbClient, &sbdb.PortBinding{LogicalPort: port})
		if errors.Is(err, libovsdbclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("failed to get port binding for port %q: %w", port, err)
		}
		validPorts.Insert(port)
	}
	return validPorts, nil
}

// shouldTrackNetwork reports whether a network takes part in MAC binding mirroring:
// a primary L2/L3 network present on this node.
func shouldTrackNetwork(netInfo util.NetInfo) bool {
	if netInfo == nil {
		return false
	}
	if !netInfo.IsPrimaryNetwork() && !netInfo.IsDefault() {
		return false
	}
	if !shouldTrackTopology(netInfo.TopologyType()) {
		return false
	}
	return true
}

func shouldTrackTopology(topology string) bool {
	return topology == types.Layer2Topology || topology == types.Layer3Topology
}

func (c *MACBindingController) updateFollowers(
	addPorts, removePorts sets.Set[string],
	portToUplink, portToNetwork, uplinkToSource map[string]string,
) (newSources, newFollowers sets.Set[string]) {
	// prepare the inverse of uplinkToSource for convenience
	sourceToUplink := make(map[string]string, len(uplinkToSource))
	for uplink, source := range uplinkToSource {
		sourceToUplink[source] = uplink
	}

	// track where ports land this round, for logging
	var logAddedBySource map[string][]string
	if klog.V(5).Enabled() {
		logAddedBySource = map[string][]string{}
	}

	c.Lock()
	defer c.Unlock()

	// gather new ports by uplink, while also adding them to the ports map
	newPortsByUplink := map[string][]string{}
	for port := range addPorts {
		uplink := portToUplink[port]
		newPortsByUplink[uplink] = append(newPortsByUplink[uplink], port)

		// add new ports as known
		c.ports[portToNetwork[port]] = port
	}

	newFollowers = sets.New[string]()
	newSources = sets.New[string]()
	removePortsList := removePorts.UnsortedList()
	followersWithNoSource := sets.New[string]()

	// big task: update followers map
	for source, followers := range c.followers {
		if source == "unknown" {
			// these are followers for which we don't know a source yet, handle
			// later
			continue
		}

		hadFollowers := followers.Len() > 0

		// remove followers
		followers.Delete(removePortsList...)

		// add followers on the same uplink as source
		uplink, isSource := sourceToUplink[source]
		if !isSource || removePorts.Has(source) {
			// source changed or removed, handle later
			followersWithNoSource.Insert(followers.UnsortedList()...)
			followersWithNoSource.Insert(source)
			delete(c.followers, source)
			continue
		}

		added := newPortsByUplink[uplink]
		followers.Insert(added...)
		followers.Delete(source)
		newFollowers.Insert(added...)
		newFollowers.Delete(source)
		delete(newPortsByUplink, uplink)

		if !hadFollowers && followers.Len() > 0 {
			newSources.Insert(source)
		}

		if logAddedBySource != nil && len(added) > 0 {
			logAddedBySource[source] = append(logAddedBySource[source], added...)
		}
	}

	// Before handling new ports, treat followers with unknown source as if they
	// were new ports too. For the most part, the controller is only able to
	// relocate followers on full reconciliations where portToUplink has a
	// complete map of all networks.
	followersWithNoSource.Insert(c.followers["unknown"].UnsortedList()...)
	followersWithNoSource.Delete(removePortsList...)
	delete(c.followers, "unknown")
	for follower := range followersWithNoSource {
		uplink, knownUplink := portToUplink[follower]
		if !knownUplink {
			continue
		}
		newPortsByUplink[uplink] = append(newPortsByUplink[uplink], follower)
		delete(followersWithNoSource, follower)
	}

	// add new ports with known sources as followers
	for uplink, ports := range newPortsByUplink {
		source := uplinkToSource[uplink]
		portSet := sets.New(ports...)
		if source == "" || !portSet.Has(source) {
			// we don't have a source on the same uplink as this port
			followersWithNoSource.Insert(ports...)
			continue
		}

		c.followers[source] = portSet
		newFollowers.Insert(ports...)
		c.followers[source].Delete(source)
		newFollowers.Delete(source)

		if c.followers[source].Len() > 0 {
			newSources.Insert(source)
		}

		if logAddedBySource != nil && len(ports) > 0 {
			logAddedBySource[source] = append(logAddedBySource[source], c.followers[source].UnsortedList()...)
		}
	}

	// if we still have ports with unknown sources, set them back as such
	if followersWithNoSource.Len() > 0 {
		c.followers["unknown"] = followersWithNoSource
		if logAddedBySource != nil {
			logAddedBySource["unknown"] = followersWithNoSource.UnsortedList()
		}
	}

	if logAddedBySource != nil {
		klog.V(5).Infof("Follower allocation changed: added ports: %v, removed ports: %v, new sources: %v, new followers: %v",
			logAddedBySource,
			removePorts.UnsortedList(),
			newSources.UnsortedList(),
			newFollowers.UnsortedList(),
		)
	}

	return newSources, newFollowers
}

func (c *MACBindingController) tracksNetwork(network string) bool {
	c.RLock()
	defer c.RUnlock()
	return c.ports[network] != ""
}

func (c *MACBindingController) tracksSource(source string) bool {
	c.RLock()
	defer c.RUnlock()
	return len(c.followers[source]) > 0
}

func (c *MACBindingController) getPorts() sets.Set[string] {
	c.RLock()
	defer c.RUnlock()
	portSet := sets.New[string]()
	for _, port := range c.ports {
		portSet.Insert(port)
	}
	return portSet
}

func (c *MACBindingController) removePort(network string) bool {
	c.Lock()
	defer c.Unlock()
	_, hasPort := c.ports[network]
	delete(c.ports, network)
	return hasPort
}

func (c *MACBindingController) getAllFollowers() sets.Set[string] {
	c.RLock()
	defer c.RUnlock()
	followerSet := sets.New[string]()
	for _, followers := range c.followers {
		followerSet.Insert(followers.UnsortedList()...)
	}
	return followerSet
}

func (c *MACBindingController) getFollowers(source string) []string {
	c.RLock()
	defer c.RUnlock()
	followers := c.followers[source]
	if followers == nil {
		return nil
	}
	return followers.UnsortedList()
}

func (c *MACBindingController) getSourceForFollower(follower string) string {
	c.RLock()
	defer c.RUnlock()
	for source, followers := range c.followers {
		if followers.Has(follower) {
			return source
		}
	}
	return ""
}

func (c *MACBindingController) hasUnknownSource() bool {
	c.RLock()
	defer c.RUnlock()
	_, hasUnknown := c.followers["unknown"]
	return hasUnknown
}

func (c *MACBindingController) setUnknownSource(source string) bool {
	c.Lock()
	defer c.Unlock()
	if c.followers[source] != nil {
		return false
	}
	c.followers["unknown"] = sets.Set[string]{}
	return true
}
