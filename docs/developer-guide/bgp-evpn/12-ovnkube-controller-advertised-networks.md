# 12 — ovnkube-controller: OVN state for advertised networks

This page covers what the zone controller (`ovnkube-controller`, package
`go-controller/pkg/ovn`) changes in the OVN northbound database when a
network is advertised: gateway router SNAT, the advertised-network isolation
ACLs, the no-overlay gateway router routes, and the EgressIP interactions. The
route import controller has its own page ([13](13-ovnkube-controller-route-import.md)),
as does the EVPN logical topology ([14](14-ovnkube-controller-evpn-topology.md)).

## Contents

1. [How the zone controller learns about advertisement](#how-the-zone-controller-learns-about-advertisement)
2. [Startup wiring](#startup-wiring)
3. [The reconcile path for an advertisement change](#the-reconcile-path-for-an-advertisement-change)
4. [SNAT rules for advertised networks](#snat-rules-for-advertised-networks)
5. [Advertised-network isolation ACLs](#advertised-network-isolation-acls)
6. [No-overlay gateway router routes](#no-overlay-gateway-router-routes)
7. [EgressIP interactions](#egressip-interactions)
8. [Per-topology entry points](#per-topology-entry-points)
9. [Complexity](#complexity)
10. [Review notes](#review-notes)

---

## How the zone controller learns about advertisement

Through `util.NetInfo` only: `IsPodNetworkAdvertisedAtNode(netInfo, node)`
and `IsPodNetworkAdvertisedAtNodeDefaultVRF`. The maps behind those
predicates are filled by the network manager
([11](11-network-manager-propagation.md)). The zone controller has no RA
informer handlers of its own.

In interconnect mode each zone manages one node, so `oc.nodeName` is the
only node whose entry matters, but the code is written per node and works
for multi-node zones too.

---

## Startup wiring

`controllermanager/controller_manager.go`:

| Line | What |
|---|---|
| `:293-297` | Create the route import manager (`routeimport.New`) when route advertisements are enabled, with the `UplinkState` lister when the Uplink feature is on |
| `:236-240` | On startup, `ovn.CleanupStaleAdvertisedNetworkSubnets` removes subnets of networks that no longer exist from the global advertised-subnets address set |
| `:450-452` | `ovn.ConfigureAdvertisedNetworkIsolation(nbClient)` creates the global address set, the drop ACL and the port group used for isolation |
| `:470-471`, `:503-504` | Start and stop the route import manager |
| `:90-97`, `:121-124`, `:364` | The route import manager is injected into the default, Layer3 and Layer2 controllers |

---

## The reconcile path for an advertisement change

```mermaid
sequenceDiagram
    participant NM as network manager (zone)
    participant BC as BaseNetworkController.reconcile
    participant NR as node reconciler
    participant GW as GatewayManager / addNode
    NM->>BC: Reconcile(ctx, netInfo with new advertised maps)
    BC->>BC: wasAdvertised != isAdvertised for oc.nodeName?
    BC->>BC: ReconcileNetInfo (point of no return)
    BC->>NR: setNodeFailed(node); nodeReconciler.ReconcileNetwork(node, network)
    NR->>GW: node sync with syncGw: SyncGateway, SNAT, isolation ACLs
    BC->>BC: routeImportManager.ReconcileNetwork if Uplink changed
```

`BaseNetworkController.reconcile` (`ovn/base_network_controller.go:190-215`):

- `reconcileLocalNode` is true when the advertised predicate flipped for the
  local node, or when cluster subnets changed and the network is advertised
  (or EgressIP is on).
- `reconcileRoutes` is true when the route import manager reports that the
  network's Uplink changed (`routeimport.NeedsReconciliation`).
- `doReconcile` (`:233-270`) marks the local node's gateway as failed and
  re-queues it, so the normal node add path reruns with the new state.

The node add path is where every OVN artifact below is (re)built.

---

## SNAT rules for advertised networks

Normally pod traffic leaving the gateway router is SNATed to the node IP.
When the network is advertised, pod IPs are reachable from outside, so SNAT
must stop for north/south traffic while **remaining** for traffic to other
cluster nodes (otherwise NodePort reply paths and node-to-pod symmetry break).
The implementation is a conditional SNAT keyed on an address set of all node
IPs.

### The node IP address set

`addressSetManager.EnsureClusterNodeIPsAddressSet(ClusterNodeIPsRouteAdvertisementsBackRef)`
returns the DB IDs of a shared address set that tracks every node's IPs
(`ovn/addresssetmanager`). Each consumer registers a back-reference so the set
lives as long as anyone needs it.

### The match

`GetNetworkScopedClusterSubnetSNATMatch` (`ovn/gateway.go:1415-1444`):

| Case | Match string |
|---|---|
| not advertised, not legacy Layer2 | `""` (unconditional SNAT) |
| not advertised, legacy Layer2 (no transit router) | `outport == "rtoe-GR_<net>_<node>"` |
| advertised and no-overlay SNAT exemption applies (`IsNoOverlaySNATExemptionNeeded`: no-overlay, `outboundSNAT: Enabled`, shared gateway) | `""` |
| advertised, not legacy Layer2 | `ip4.dst == $<node-ips-v4>` (or `ip6`) |
| advertised, legacy Layer2 | `outport == "rtoe-GR_..." && ip4.dst == $<node-ips>` |

### Where the SNATs are built

| Network | Function | Notes |
|---|---|---|
| Default network, per-node cluster subnet SNAT | `GatewayManager.SyncGateway` -> `ovn/gateway.go:1095-1185` | `isRoutingAdvertised(nodeName)`; no-overlay with SNAT enabled uses `exempted_ext_ips` with the no-overlay exemption address set instead of a match (`:1160-1185`) |
| Default network, per-pod SNAT when `DisableSNATMultipleGWs` | `DefaultNetworkController.AddPodSNATOps` (`ovn/gateway.go:2017-2063`) | same match helper |
| UDNs, Layer3 per-node host subnet / Layer2 cluster subnet | `buildUDNEgressSNAT` (`ovn/base_network_controller_user_defined.go:825-935`), called from `addOrUpdateUDNNodeSubnetEgressSNAT` (L3, `layer3_user_defined_network_controller.go:1006`) and `addOrUpdateUDNClusterSubnetEgressSNAT` (L2, `layer2_user_defined_network_controller.go:1054`) | SNAT to the network's **masquerade IP** (the host then SNATs to the node IP in local gateway mode) |

The UDN variant has two advertised sub-cases (`:885-918`):

- **Shared gateway**: OVN `NAT.allowed_ext_ips` accepts one address set, so
  one SNAT per allowed destination set is created: node IPs and the
  "UDN-enabled services" cluster IPs (`BuildSNATWithAllowedExtIPs`). The
  service set exists so that replies to the default network's kapi and DNS
  services hit the masquerade IP rule on the host rather than the UDN subnet
  rule, which matters for VRF-Lite with overlapping subnets.
- **Local gateway**: the destination match is appended to the management port
  MAC match (`getClusterNodesDestinationBasedSNATMatch`).

The feature doc shows the resulting `nbdb.NAT` rows in
[route-advertisements.md](../../features/bgp-integration/route-advertisements.md#ovn-snat-behavior-with-bgp-advertisement).

---

## Advertised-network isolation ACLs

Advertised CUDNs in the same VRF could reach each other through the provider
network: A's pods send to B's subnet, the packet egresses, the fabric routes
it back to B. The isolation promise of UDNs must still hold. With
`advertised-udn-isolation-mode: strict` (the default) this is enforced in OVN
by two ACLs and one port group (`ovn/udn_isolation.go`):

| Object | Built by | Where attached | Match | Action, priority, tier |
|---|---|---|---|---|
| Address set `advertised-network-subnets` (`GetAdvertisedNetworkSubnetsAddressSetDBIDs`, `:272-276`) | `ConfigureAdvertisedNetworkIsolation` at startup (`:311-347`) | referenced by the drop ACL | contains every advertised UDN subnet | |
| Drop ACL (`BuildAdvertisedNetworkSubnetsDropACL`, `:451-470`) | same | port group below | `(ip4.src == $set && ip4.dst == $set) \|\| (ip6...)` | drop, `AdvertisedNetworkDenyPriority` 1050, isolation tier, `to-lport` after LB |
| Port group `advertised-network-subnets` (`GetAdvertisedNetworkSubnetsDropPGdbIDs`) | same | holds the **switch-to-router** port of every node switch of every advertised network | | |
| Pass ACL per network (`GetAdvertisedNetworkSubnetsPassACLdbIDs`, `:302-308`) | `addAdvertisedNetworkIsolation(node)` (`:482-565`) | the network's node switch | `(ip4.src == <own subnets>) && (ip4.dst == <own subnets>)` | pass, `AdvertisedNetworkPassPriority` 1100 |

Reading the two together: traffic between two advertised subnets is dropped
at the router port, unless both ends belong to the same network, in which case
the higher-priority pass ACL on that network's switch wins. The drop ACL is on
a port group rather than on each switch to avoid a race that produced
duplicate ACLs; `migrateAdvertisedNetworkDropACLToPortGroup` (`:349-416`)
moves older installations over.

Add and delete are driven per node from the topology controllers:

```go
// layer3_user_defined_network_controller.go:1040-1053 (same shape at layer2 :722-736)
isUDNAdvertised := util.IsPodNetworkAdvertisedAtNode(oc, node.Name)
...
shouldIsolate := isUDNAdvertised && config.OVNKubernetesFeature.AdvertisedUDNIsolationMode == config.AdvertisedUDNIsolationModeStrict
if shouldIsolate { oc.addAdvertisedNetworkIsolation(node.Name) } else { oc.deleteAdvertisedNetworkIsolation(node.Name) }
```

`loose` mode simply never calls `addAdvertisedNetworkIsolation`, so the
address set stays empty and the drop ACL matches nothing. The host-side
counterpart (nftables `udn-bgp-drop`) is described in
[15](15-ovnkube-node-vrf-and-host-datapath.md#advertised-udn-isolation-on-the-host).

The e2e `BGP: isolation` table (`test/e2e/route_advertisements.go:1433-2566`)
enumerates the allowed and denied paths.

---

## No-overlay gateway router routes

For a no-overlay network there is no transit switch, so the gateway router
must not forward remote pod subnets to the `ovn_cluster_router`
(`ovn/gateway.go:545-615`):

- Only this node's host subnets are routed to the distributed router
  (`drRoutedSubnets = gwConfig.hostSubnets`), instead of the whole cluster
  subnet.
- Stale full-cluster-subnet routes left from overlay mode are removed
  (`deleteStaleDRRoutes`), covering a transport switch on upgrade.
- Remote pod subnets then follow the gateway router's default route to the
  physical network, where BGP-learned routes (imported by
  [13](13-ovnkube-controller-route-import.md), which for no-overlay networks
  does **not** ignore pod subnets) deliver them to the right node.

`hasInterconnectTransport()` (`base_network_controller.go:991-998`) gates
every interconnect resource (transit switch ports, remote ports, remote pod
LSPs) for both `no-overlay` and `evpn`.

---

## EgressIP interactions

`ovn/egressip.go:3077-3125`: when `DisableSNATMultipleGWs` is set and a pod
loses its egress IP, the controller normally re-adds a per-pod SNAT to the
node IP. For an advertised network it skips that (`:3082-3085`), and when it
does add one it uses the same conditional match as the gateway
(`:3100-3118`).

The EgressIP prefix itself is advertised by FRR only; OVN keeps SNATing the
pod to the egress IP on the egress node as it does without BGP. There is no
OVN-side consumer of `GetEgressIPAdvertisedOnNodeVRFs` today.

---

## Per-topology entry points

| Topology | Node add / gateway sync | Advertised-specific work |
|---|---|---|
| Default | `DefaultNetworkController` node sync -> `GatewayManager.SyncGateway` | cluster subnet SNAT match (`gateway.go:1115-1140`), per-pod SNAT (`AddPodSNATOps`), route import registration at `Start` (`default_network_controller.go:648-652`) |
| Layer3 UDN | `addNode` (`layer3_user_defined_network_controller.go:1025-1056`) | host subnet SNAT, isolation ACLs; route import registration at start (`:560-563`) and `ForgetNetwork` on stop (`:385-387`) |
| Layer2 UDN | node sync with `syncGw` (`layer2_user_defined_network_controller.go:700-745`) | cluster subnet SNAT, isolation ACLs; route import registration **only in shared gateway mode** (`base_secondary_layer2_network_controller.go:151-156`) |

---

## Complexity

- SNAT and ACL work is per (network, node) and runs on node add and on every
  advertisement flip: O(subnets of the network) ACL match terms, a handful of
  NB transactions.
- The global address set grows with the number of advertised subnets; the
  drop ACL match is a single pair of set references so its size is constant,
  but OVN expands set membership into flows, which is the O(subnets^2)
  concern recorded as hypothesis 10 in
  [03-bottlenecks.md](../../perf-scale/bgp-evpn/03-bottlenecks.md#10-advertised-network-isolation-is-osubnets2).
- `CleanupStaleAdvertisedNetworkSubnets` at startup is O(address set size).

---

## Review notes

- Every place that builds a cluster-subnet or pod SNAT must call
  `GetNetworkScopedClusterSubnetSNATMatch`; a new SNAT path that ignores the
  advertised state silently breaks the "no SNAT when advertised" contract.
- `isolationTier` and the priorities 1050/1100 are shared with UDN isolation
  ACLs; changing them changes precedence against network policy ACLs.
- The drop ACL lives under the default network controller name
  (`types.DefaultNetworkControllerName`) even though UDN controllers populate
  it; cleanup belongs to the default controller.
- `loose` mode is documented as broken when several no-overlay UDNs share a
  VRF (`no-overlay.md`, known limitations); do not rely on it for isolation.
