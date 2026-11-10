# 11 — Network manager: propagating advertisement state to zone and node controllers

The ovnkube-controller (zone) and ovnkube-node processes never read
`FRRConfiguration` objects and never evaluate `RouteAdvertisements` selectors
themselves. They learn "this network is advertised on node X in VRF Y" from
the network manager, which derives it from the NAD annotation written by the
cluster manager and the RA status. This page is the bridge between the
cluster-manager docs and the datapath docs.

## Contents

1. [The data being propagated](#the-data-being-propagated)
2. [Where the network manager runs](#where-the-network-manager-runs)
3. [Event sources in the network controller](#event-sources-in-the-network-controller)
4. [`setAdvertisements`: from annotation to NetInfo](#setadvertisements-from-annotation-to-netinfo)
5. [Deadlock avoidance: `preserveAdvertisements`](#deadlock-avoidance-preserveadvertisements)
6. [What a NetInfo change triggers downstream](#what-a-netinfo-change-triggers-downstream)
7. [Node activity notifications back to the cluster manager](#node-activity-notifications-back-to-the-cluster-manager)
8. [Complexity](#complexity)
9. [Review notes](#review-notes)

---

## The data being propagated

Two maps on `mutableNetInfo` (`util/multi_network.go:354-422`):

| Field | Shape | Meaning |
|---|---|---|
| `podNetworkAdvertisements` | `node -> sorted unique []vrf` | On `node`, the pod subnets of this network are advertised in each `vrf` (`default` or a VRF name) |
| `eipAdvertisements` | `node -> sorted unique []vrf` | On `node`, egress IPs serving this network are advertised in each `vrf` |

Read through `GetPodNetworkAdvertisedOnNodeVRFs(node)` and the two
predicates `IsPodNetworkAdvertisedAtNode` /
`IsPodNetworkAdvertisedAtNodeDefaultVRF` (`multi_network.go:2157-2177`,
quoted in [05](05-api-cudn-transport.md#netinfo-accessors)). The VRF value
stored is the RA's `targetVRF` with `""` normalised to `default`
(`network_controller.go:636-639`); the literal string `auto` is stored as is,
which is why the "default VRF" predicate checks for the string `default`.

---

## Where the network manager runs

`pkg/networkmanager` is instantiated three times with different roles:

| Process | Constructor | `node` argument | Reads RA state? |
|---|---|---|---|
| ovnkube-cluster-manager | `networkmanager.NewForCluster` | `""` (all nodes managed) | yes (same code), but the cluster manager's consumers do not use it |
| ovnkube-controller | `NewForZone` | `""` | yes |
| ovnkube-node | `NewForNode` (`node_controller_manager.go:311-316`), required when route advertisements are enabled (`isNetworkManagerRequiredForNode`, `:277-282`) | node name: only that node's entries are stored (`isNodeManaged`, `:675-682`) | yes |

---

## Event sources in the network controller

`newNetworkController` (`network_controller.go:42-104`) adds two watchers
only when `util.IsRouteAdvertisementsEnabled()`:

| Watcher | `ObjNeedsUpdate` | Handler |
|---|---|---|
| RouteAdvertisements | `raNeedsUpdate` (`:684-715`): **ignores add and delete** (the NAD annotation carries those), ignores resyncs and deleting objects, and requires the new `Accepted` condition to exist, be `True`, have `observedGeneration == generation` and differ from the old condition in status or generation | `syncRunningNetworks()` |
| Node | `nodeNeedsUpdate` (`:717-729`): labels changed (node selectors may match differently) | `syncRunningNetworks()` |

`syncRunningNetworks` (`:416-424`) re-queues the default network and every
running network controller into the per-network reconciler, which has
`Threadiness: 5` and infinite retries (`:34-39`, `:57-62`).

The NAD controller itself treats a change of the
`k8s.ovn.org/route-advertisements` annotation as a reason to reconcile the
network (`nad_controller.go:1326-1330`), alongside spec and ID changes.

---

## `setAdvertisements`: from annotation to NetInfo

Called from `syncNetwork` through `gatherNetwork` for the **desired**
`NetInfo` of every default or primary network (`:426-475`, `:548-553`).

```text
setAdvertisements(network, current):                       network_controller.go:555
  skip unless default or primary network
  skip unless route advertisements enabled
  raNames := union over NADs of the network of the JSON list in annotation   (:571-586)
  for ra in raNames:
    ra := raLister.Get(ra)
    if NotFound:            preserveAdvertisements(network, current); return  (:591-598)
    if no PodNetwork:       continue                                           (:604-607)
    accepted := condition Accepted
    if nil:                 continue                                           (:609-612)
    if not True or observedGeneration != generation:
                            preserveAdvertisements(network, current); return  (:613-623)
    nodes := nodeLister.List(ra.spec.nodeSelector)                             (:625-633)
    vrf := targetVRF or "default"
    for node in nodes managed by this process:
      podAdvertisements[node] += vrf     if PodNetwork                         (:641-648)
      eipAdvertisements[node] += vrf     if EgressIP
  network.SetPodNetworkAdvertisedVRFs(podAdvertisements)
  network.SetEgressIPAdvertisedVRFs(eipAdvertisements)
```

Observations:

- The NAD annotation is the source of **which** RAs apply; the RA object is
  the source of **what** they advertise and **where**. Both are needed.
- The node selector is re-evaluated here, independently from the cluster
  manager; both use the same lister semantics.
- RAs that do not advertise `PodNetwork` are skipped entirely (`:604-607`),
  so `eipAdvertisements` is only filled from RAs that advertise both. An RA
  with only `EgressIP` neither marks the network as advertised for the node
  nor populates `eipAdvertisements`. As of this commit no controller reads
  `GetEgressIPAdvertisedOnNodeVRFs` or `GetEgressIPAdvertisedNodes` outside
  `pkg/util`; egress IP advertisement is realised entirely by the generated
  `FRRConfiguration` prefixes, and OVN keeps SNATing to the egress IP as
  before.

---

## Deadlock avoidance: `preserveAdvertisements`

`:663-669`. Two situations would otherwise block a network controller from
starting:

1. A stale annotation names an RA that no longer exists.
2. The RA is not accepted yet because the network has no subnets, but
   subnets are only allocated once the network controller runs.

In both cases the previously known maps are carried over (empty for a brand
new network) and the controller starts. When the RA transitions to accepted,
`raNeedsUpdate` fires and `syncRunningNetworks` fills in the real VRFs.

---

## What a NetInfo change triggers downstream

`syncNetwork` decides between recreating the controller
(`!AreNetworksCompatible`) and reconciling it in place
(`DoesNetworkNeedReconciliation`). Advertisement maps are part of the
reconcilable, dynamic state, so a change goes through
`ReconcilableNetworkController.Reconcile(ctx, netInfo)` (`:477-490`).

| Process | Reconcile implementation | What it does with advertisement changes |
|---|---|---|
| ovnkube-controller, default network | `DefaultNetworkController.Reconcile` -> `BaseNetworkController.reconcile` (`ovn/base_network_controller.go:190-215`) | compares `IsPodNetworkAdvertisedAtNode(old/new, oc.nodeName)`; if changed, marks the local node gateway failed and re-queues the node so SNAT, isolation ACLs and GR state are rebuilt ([12](12-ovnkube-controller-advertised-networks.md)) |
| ovnkube-controller, UDNs | same base path through the Layer2/Layer3 controllers | same |
| ovnkube-node, default network | `DefaultNodeNetworkController.Reconcile` (`node/default_node_network_controller.go:243-262`) | `Gateway.SetDefaultPodNetworkAdvertised` and `Gateway.Reconcile()`: bridge flows and nftables masquerade ([15](15-ovnkube-node-vrf-and-host-datapath.md)) |
| ovnkube-node, UDNs | `UserDefinedNodeNetworkController` -> `UserDefinedNetworkGateway.Reconcile()` -> `reconcileAdvertisedState` (`node/gateway_udn.go:1611-1704`) | VRF routes and rules, isolation nftables set, bridge flows, service marks |

Only the **local node's** entry matters to a node process; the zone process
looks at every node it manages (in interconnect mode, one).

---

## Node activity notifications back to the cluster manager

With dynamic UDN allocation, the direction is reversed for one signal: the
cluster manager's RA controller must know on which nodes a network is active.
The NAD controller tracks per-node activity (`nad_controller.go:300-331`) and
calls registered `NetworkRefReconciler`s (`:833-855`) when it changes. The RA
controller registers one at start (`routeadvertisements/controller.go:296-306`)
and maps the network to the RAs that select it. No Kubernetes object is
involved, which is why this path exists in addition to the informers.

---

## Complexity

`setAdvertisements` is O(NADs of the network + RAs in the annotation x nodes
selected). It runs on every network sync, and `syncRunningNetworks` runs it
for every network on every accepted-RA status change and every node label
change. On ovnkube-node the node filter keeps the maps to one entry; on the
zone controller they hold all nodes. For 1000 CUDNs each selected by one RA,
one RA acceptance costs 1000 syncs of O(nodes) each on the zone controller.

---

## Review notes

- Anything that makes `Accepted` toggle (a new validation in the RA
  controller) toggles the advertised state on every node for every selected
  network. Flapping acceptance means flapping SNAT and isolation rules.
- `raNeedsUpdate` requires `observedGeneration` to match; a status write
  that omits it (for example from a different field manager) would be
  ignored here.
- `preserveAdvertisements` means a deleted RA whose NAD annotation was not
  yet cleaned keeps the network advertised until the cluster manager removes
  the name. The cluster manager does so in the same reconcile that deletes the
  generated objects (`updateNADs` with a nil RA), so the window is short.
