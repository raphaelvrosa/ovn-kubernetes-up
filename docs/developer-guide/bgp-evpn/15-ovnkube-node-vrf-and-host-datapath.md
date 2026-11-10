# 15 — ovnkube-node: VRFs, host routing, nftables and bridge flows for advertised networks

This page covers what the node agent programs in the Linux host for a
network that is advertised: the per-network VRF and its routes, the policy
routing rules, the nftables masquerade and isolation rules, and the OpenFlow
rules on the external bridge. It applies to every advertised network; the
EVPN devices that sit on top are in [16](16-ovnkube-node-evpn-datapath.md).

Line numbers refer to files in `go-controller/pkg/node` unless stated.

## Contents

1. [How the node learns about advertisement](#how-the-node-learns-about-advertisement)
2. [Startup wiring](#startup-wiring)
3. [The default network path](#the-default-network-path)
4. [The UDN path: `UserDefinedNetworkGateway`](#the-udn-path-userdefinednetworkgateway)
5. [VRF creation and the VRF manager](#vrf-creation-and-the-vrf-manager)
6. [VRF routes](#vrf-routes)
7. [Policy routing rules](#policy-routing-rules)
8. [nftables: masquerade](#nftables-masquerade)
9. [Advertised UDN isolation on the host](#advertised-udn-isolation-on-the-host)
10. [OpenFlow on the external bridge](#openflow-on-the-external-bridge)
11. [Service marks](#service-marks)
12. [Uplink and VRF-Lite enslavement](#uplink-and-vrf-lite-enslavement)
13. [OVN setting required in local gateway mode](#ovn-setting-required-in-local-gateway-mode)
14. [Complexity](#complexity)
15. [Review notes](#review-notes)

---

## How the node learns about advertisement

The node runs its own network manager (`networkmanager.NewForNode`,
`controllermanager/node_controller_manager.go:311-316`), required when
route advertisements are enabled (`isNetworkManagerRequiredForNode`,
`:277-282`). It fills the advertised maps for the local node only
([11](11-network-manager-propagation.md)). Two predicates then drive every
decision below:

```go
// gateway_udn.go:2003-2006
udng.isNetworkAdvertised = util.IsPodNetworkAdvertisedAtNode(udng.NetInfo, udng.node.Name)
udng.isNetworkAdvertisedToDefaultVRF = util.IsPodNetworkAdvertisedAtNodeDefaultVRF(udng.NetInfo, udng.node.Name)
```

| `isNetworkAdvertised` | `isNetworkAdvertisedToDefaultVRF` | Meaning |
|---|---|---|
| false | false | not advertised: overlay only |
| true | true | advertised in the default VRF (`targetVRF` unset or `default`) |
| true | false | advertised only in its own VRF: VRF-Lite or EVPN |

---

## Startup wiring

`node_controller_manager.go`:

| Line | What |
|---|---|
| `:333-339` | `vrfmanager.NewController` when network segmentation is enabled; `iprulemanager.NewController` unless DPU mode |
| `:502-521` | `vrfManager.Run` and `ruleManager.Run` (5 minute resync, owns rule priority `UDNMasqueradeIPRulePriority` = 2000) |
| `:254-262` | `vrfManager.Repair(validVRFDevices)` on sync so VRFs of deleted networks are removed |
| `:110-118` | each UDN node controller receives the VRF manager, rule manager and the default gateway |

`default_node_network_controller.go:218-222`: when route advertisements are
enabled, `configureAdvertisedUDNIsolationNFTables()` creates the isolation
chain and sets once at startup (full and DPU-host modes).

---

## The default network path

`DefaultNodeNetworkController.Reconcile` (`:243-275`):

```go
reconcilePodNetwork := wasAdvertised != isAdvertised            // :244-246
if reconcilePodNetwork && oc.Gateway != nil {
	oc.Gateway.SetDefaultPodNetworkAdvertised(isAdvertised)      // gateway.go:504-509: stores in the bridge netConfig
	oc.Gateway.Reconcile()                                       // gateway.go:546-548: rebuild flows, SNAT, resync all services
}
```

`gateway.Reconcile` refreshes the OpenFlow cache (so the flows in
[OpenFlow on the external bridge](#openflow-on-the-external-bridge) are
recomputed with the new `Advertised` bit), reapplies the local gateway
masquerade rules (`gateway.go:655-664` ->
`addOrUpdateLocalGatewayPodSubnetNFTRules(advertised, subnets...)`) and
resyncs every service. The initial value is read at gateway creation
(`gateway_shared_intf.go:1709-1711`).

The default network has no VRF: it owns the main table, so there are no
routes or rules to manage.

---

## The UDN path: `UserDefinedNetworkGateway`

`UserDefinedNodeNetworkController.Reconcile` (`user_defined_node_network_controller.go:154-166`)
calls `gateway.Reconcile()` when either predicate changed
(`shouldReconcileNetworkChange`, `:140-148`). That enqueues the network name
into a per-gateway single-worker reconciler (`gateway_udn.go:246`, `:1611`),
whose handler is `reconcileGateway` -> `reconcileAdvertisedState`
(`:1614-1704`). Initial programming happens in
`addNetworkWithResolvedUplink` (`:648-760`) and calls the same helpers.

`reconcileAdvertisedState` in order:

1. `updateAdvertisementStatus()` refreshes the two flags.
2. Store `isNetworkAdvertised` in the bridge `netConfig.Advertised`
   (`:1650-1655`).
3. `reconcileUplinkGatewayVRFSlave` (Uplink only).
4. `applyAdvertisedRoutingState()` = `updateUDNVRFIPRules()` +
   `updateUDNVRFIPRoute()` (`:1710-1718`).
5. `openflowManager.updateBridgeFlowCache` + `requestFlowSync` (`:1675-1683`).
6. `updateAdvertisedUDNIsolationRules()` (`:1687-1689`).
7. `syncUplinkBridgeFlows` (Uplink only).
8. `resyncNetworkServices` so service mark rules follow the new state
   (`:1696-1703`).

---

## VRF creation and the VRF manager

A primary UDN always gets a VRF, advertised or not, in both gateway modes
(`gateway_udn.go:696-712`):

```go
vrfTableId := util.CalculateRouteTableID(mplink.Attrs().Index)   // ifindex + config.OvnKubeNode.RoutingTableIDStart
vrfDeviceName := util.GetNetworkVRFName(udng.NetInfo)            // CUDN name, or mp<id>-udn-vrf
udng.vrfManager.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(udng.vrfTableId), nil)
```

The management port `ovn-k8s-mp<id>` is the VRF's only slave by default.
DPU mode uses a distinct table range (`dpuUDNVRFRouteTableID`, `:1732-1734`).

`pkg/node/vrfmanager/vrf_manager.go`:

| Element | Code |
|---|---|
| Desired state map keyed by VRF ifindex, `AddVRF` / `AddVRFSlave` / `DeleteVRFSlave` / `AddVRFRoutes` / `DeleteVRFRoutes` / `DeleteVRF` | `:299-595` |
| Link event subscription plus a 60 s periodic `reconcile()` (`reconcilePeriod`, `:33`) that re-syncs every VRF and runs `repair` to delete unknown VRF devices | `:72-162`, `:164-192` |
| Enslaving an interface captures its routes and restores them into the VRF table (`enslaveInterfaceToVRF`, `restoreRoutesToTable`) | `:609-858` |

Routes handed to `AddVRFRoutes` are marked with `types.OVNKProtocol` so that
the manager, and `removeManagedDefaultRoutesFromVRF`, can tell managed routes
from kernel or BGP routes (`markOVNKRoutes`, `:290-297`).

---

## VRF routes

`computeRoutesForUDN` (`gateway_udn.go:1236-1450`) builds the managed route
set of the VRF table; the advertised state changes only the default route.

| Route | Purpose | Advertised dependency |
|---|---|---|
| service CIDRs via the dummy next hop masquerade IP on the gateway bridge (or Uplink bridge) | host and UDN CNI to services | none |
| default route via the node's gateway next hops | north/south through the default routing domain | **omitted** when `isNetworkAdvertised && !isNetworkAdvertisedToDefaultVRF` (`getDefaultRouteExceptIfVRFLite`, `:1452-1460`) |
| per-network masquerade IP (`169.254.0.<n>`) dev mp | reply traffic into OVN | none |
| ETP-local masquerade IP via the OVN gateway IP dev mp; cluster subnet (L3) or node-local subnet (local gateway no-overlay L3) via mp | reach pods through OVN | none |
| `unreachable default metric 4278198272` (and IPv6 on loopback) | stop lookups leaking into the main table | none |

`updateUDNVRFIPRoute` (`:1840-1868`) applies the flip at runtime: remove the
managed default routes for VRF-Lite and EVPN (`removeManagedDefaultRoutesFromVRF`,
`:1886-1915`, matches by `OVNKProtocol`, nil destination and a gateway), add
them back otherwise.

With the default route gone, a VRF-Lite network can only leave the node
through routes FRR installs into the VRF, and an EVPN network through Type-5
routes on the L3 SVI. That is the isolation guarantee of
[route-advertisements.md, "VRF-Lite isolation"](../../features/bgp-integration/route-advertisements.md#vrf-lite-isolation).

---

## Policy routing rules

`constructUDNVRFIPRules` (`:1499-1542`), applied through the rule manager at
priority 2000:

| Rule | Always | Only when advertised to the default VRF |
|---|---|---|
| `from all fwmark <pktMark> lookup <table>` | yes | |
| `from all to <masquerade IP> lookup <table>` | yes | |
| `from all to <UDN subnet> lookup <table>` | | yes, one per subnet |

The subnet rule is what makes "traffic arriving at the host for the UDN's
advertised subnet" land in the right table in local gateway mode (the bridge
flow sends it to `LOCAL`, see below). It is deliberately absent for VRF-Lite
and EVPN, where arrival is already inside the VRF through its own
interfaces, and where subnets may overlap across networks. The `default`
branch of the switch deletes the subnet rules when the state flips
(`:1532-1541`).

---

## nftables: masquerade

Local gateway mode SNATs pod traffic to the node IP in the
`ovn-kube-pod-subnet-masq` chain. `getLocalGatewayPodSubnetMasqueradeNFTRule`
(`gateway_nftables.go:820-900`):

| Case | Rule |
|---|---|
| not advertised | `ip saddr <subnet> masquerade` |
| advertised | `ip saddr <subnet> ip daddr @remote-node-ips-v4 masquerade` (only to other nodes) |
| no-overlay default network with `outbound-snat: enabled` | `ip saddr <subnet> ip daddr @no-overlay-snat-exempt-v4 return` then `ip saddr <subnet> masquerade` |

`addOrUpdateLocalGatewayPodSubnetNFTRules` (`:1103-1130`) flushes and
rebuilds the chain whenever the advertised state toggles. The feature doc
shows the resulting ruleset in
[route-advertisements.md, "Host SNAT behavior"](../../features/bgp-integration/route-advertisements.md#host-snat-behavior-with-bgp-advertisement).

For UDNs the host-side masquerade is to the node IP of traffic already SNATed
by OVN to the network's masquerade IP; the advertised conditional is in OVN
([12](12-ovnkube-controller-advertised-networks.md#snat-rules-for-advertised-networks)).

---

## Advertised UDN isolation on the host

`configureAdvertisedUDNIsolationNFTables` (`gateway_shared_intf.go:2423-2470`)
creates at startup:

```text
set advertised-udn-subnets-v4 { type ipv4_addr; flags interval; }
set advertised-udn-subnets-v6 { type ipv6_addr; flags interval; }
chain udn-bgp-drop {
  type filter hook output priority filter; policy accept;
  ct state new ip  daddr @advertised-udn-subnets-v4 drop
  ct state new ip6 daddr @advertised-udn-subnets-v6 drop
}
```

`updateAdvertisedUDNIsolationRules` (`gateway_udn.go:1936-2001`) adds the
network's full subnets to the sets, with the network name as element comment,
when `isNetworkAdvertised`, and deletes the elements carrying its comment
otherwise. The chain is on the `output` hook: it blocks **locally generated**
traffic (host processes, host-networked pods) from reaching advertised UDN
subnets, including host subnets of other nodes reachable through the fabric.
The in-OVN counterpart is the drop ACL described in
[12](12-ovnkube-controller-advertised-networks.md#advertised-network-isolation-acls).

Both are skipped when `advertised-udn-isolation-mode` is `loose`: the node
side checks `isNetworkAdvertised` only, so with `loose` the nftables sets are
still populated, but the OVN ACLs that implement pod-to-pod isolation are
not. The e2e isolation table and the `bgp-loose-isolation` lane cover the
two modes.

---

## OpenFlow on the external bridge

`bridgeconfig/bridgeflows.go` reads `netConfig.Advertised` for every patched
network. The flows that differ when advertised:

| Table, priority | Match | Action | Code |
|---|---|---|---|
| 0, 300 | `in_port=<phys>, ip, nw_dst=<node subnet of the network>` | `output:<patch port>` in shared gateway, `output:LOCAL` in local gateway | `:1143-1155` |
| 0, 104 | `in_port=<patch or LOCAL>, dl_src=<bridge MAC>, nw_src=<node subnet>` | `output:<phys>` (leave without SNAT) | `:1157-1170` |
| 0, 550 | `in_port=LOCAL, nw_src=<UDN subnet>, nw_dst=<service CIDR>` | `ct(commit, table=2)` without SNAT (host to service from an advertised UDN) | `:298-320` |
| 0, 490 | `in_port=<default patch>, nw_src=<kapi or DNS cluster IP>` | `ct(zone=host masq, nat, table=3)` so replies from default-network services re-enter the UDN VRF | `gateway_shared_intf.go:468-478` |
| 2, 200 | `nw_src=<UDN subnet>` | `drop` (a UDN pod reaching a service in another network) | `:442-461`, `:485-500` |

The priority 300 ingress flow is what makes a prefix advertised by BGP
actually deliverable: traffic the fabric sends to the node for a pod subnet
is steered into OVN (shared gateway) or into the host stack and then the VRF
(local gateway). The feature doc shows dumps in
[route-advertisements.md, "Ingress OVS flows"](../../features/bgp-integration/route-advertisements.md#ingress-ovs-flows).

---

## Service marks

Host service access for a primary UDN uses packet marks to steer replies
into the right VRF. `shouldAddUDNServiceMarkRules` (`gateway_nftables.go:748-757`)
enables them for unadvertised networks and networks advertised to the
default VRF, and disables them for VRF-Lite and EVPN, mirroring the default
route decision. `reconcileAdvertisedState` resyncs only this network's
services so the rules converge without a Service event; the e2e
`BGP: UDN service marks after advertisement updates`
(`test/e2e/route_advertisements_service_marks.go`) checks that.

---

## Uplink and VRF-Lite enslavement

With the Uplink feature, a network's gateway interface can be a dedicated
bridge. `shouldEnslaveUplinkGatewayToVRF` (`gateway_udn.go:1747-1751`) is
true when the network is advertised only in its own VRF; then
`reconcileUplinkGatewayVRFSlave` (`:1753-1817`) removes the managed default
routes first, sets `keep_addr_on_down` for IPv6 and enslaves the Uplink
gateway interface to the VRF so FRR can peer over it inside the VRF. This is
the supported way to run VRF-Lite in shared gateway mode (OKEP-6019). Without
Uplinks, the administrator attaches an interface to the VRF by hand, which is
why `route-advertisements.md` limits VRF-Lite to local gateway mode.

---

## OVN setting required in local gateway mode

`dist/images/ovnkube.sh:893-896` sets `NB_Global.options:use_ct_inv_match=false`
when route advertisements are enabled in local gateway mode. Without it,
OVN's conntrack-invalid match drops asymmetric flows that arise when reply
traffic for advertised pods enters through the host instead of the gateway
router.

---

## Complexity

- Per network, per advertisement flip: a handful of netlink and nftables
  operations and one bridge flow cache rebuild; the flow rebuild is O(all
  networks on the bridge) because `updateBridgeFlowCache` regenerates every
  network's flows.
- `resyncNetworkServices` is O(services in the network's namespaces).
- The VRF manager's 60 s `reconcile` and the rule manager's 5 minute resync
  are O(VRFs) and O(rules) regardless of change; hypothesis 11 in the
  perf-scale set.
- Route and rule state is per node; nothing here scales with the number of
  other nodes except the `remote-node-ips` set used by the masquerade rule.

---

## Review notes

- The three outcomes of the two flags must stay consistent across
  `getDefaultRouteExceptIfVRFLite`, `constructUDNVRFIPRules`,
  `shouldAddUDNServiceMarkRules` and `shouldEnslaveUplinkGatewayToVRF`; a
  change to one is a change to the routing domain decision and must update
  all four.
- `netConfig.Advertised` is an atomic read by the flow generator; the flow
  cache must be rebuilt after storing it, which `reconcileAdvertisedState`
  does explicitly.
- Managed routes are identified by `types.OVNKProtocol`; a route added without
  that protocol will never be cleaned up by the VRF manager.
