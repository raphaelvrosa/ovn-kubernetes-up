# 18 — Performance and scale characteristics

What grows with what, where the ceilings are, and which code paths dominate.
This page is the code-level companion to the benchmarking document set under
`docs/perf-scale/bgp-evpn/`, which owns the methodology, the measured
results and the roadmap. Numbers quoted from that set are marked as
measured; everything else is derived from the code.

## Contents

1. [Dimensions](#dimensions)
2. [Object counts](#object-counts)
3. [Control plane cost model](#control-plane-cost-model)
4. [Zone controller cost model](#zone-controller-cost-model)
5. [Node agent cost model](#node-agent-cost-model)
6. [Fabric cost model](#fabric-cost-model)
7. [Hard ceilings](#hard-ceilings)
8. [Concurrency and rate limiting](#concurrency-and-rate-limiting)
9. [Observability available today](#observability-available-today)
10. [What has been measured](#what-has-been-measured)
11. [Where to look before changing hot paths](#where-to-look-before-changing-hot-paths)

---

## Dimensions

| Symbol | Dimension | Typical large target (from [01-methodology](../../perf-scale/bgp-evpn/01-methodology.md)) |
|---|---|---|
| N | nodes | 500 |
| K | advertised networks (CUDNs plus default) | 1000 |
| A | `RouteAdvertisements` objects | 1 to K |
| S | source `FRRConfiguration`s matching an RA and a node | 1 to few |
| P | BGP peers per node | 2 to 8 |
| R | routes learned per VRF | up to 500,000 |
| Q | pods per node | 200 |
| E, M | EgressIPs, namespaces | |

---

## Object counts

| Object | Count | Source |
|---|---|---|
| generated `FRRConfiguration` | A x N x S, minus nodes where a network is inactive under dynamic allocation | `generateFRRConfigurations` per (node, template) |
| routers per generated object (default VRF target) | 1 + (K_selected - 1) reciprocal import routers | `controller.go:1494-1531` |
| prefixes per neighbor per object | host subnets of selected networks on that node (Layer3) or whole subnets (Layer2), copied once per neighbor | `controller.go:1423-1437` |
| NAD annotation writes per RA reconcile | only NADs whose list changed, but **all** NADs are read and parsed | `controller.go:2033-2089` |
| `FRRNodeState` | N, each carrying the full rendered configuration as a string | frr-k8s |
| `BGPSessionState` | N x P x VRFs with sessions | frr-k8s |
| LRSRs per gateway router | R (filtered) | route import |
| EVPN Type-2 routes | 1 or 2 per pod IP cluster-wide | zebra from the pod controller's entries |
| EVPN Type-3 routes | N per MAC-VRF VNI | FRR |
| EVPN Type-5 routes | N per Layer3 IP-VRF (host subnets), 1 per Layer2 IP-VRF per node | FRR |
| VLAN IDs | 1 per MAC-VRF plus 1 per IP-VRF, cluster-wide out of 4093 | CUDN controller allocator |

---

## Control plane cost model

Per RA reconcile (details in
[07, Complexity](07-cm-routeadvertisements-controller.md#complexity)):

| Phase | Cost |
|---|---|
| selection and template mapping | O(NADs) + O(S x N) |
| VTEP claims (EVPN) | O(A x NADs) + O(A x N) + O(all generated objects) |
| generation | O(N x S x routers x P) struct copies, O(N x K) annotation parses memoised per node |
| apply | O(N x S) `DeepEqual` on full specs; API writes only for changes, serialised, one at a time |
| NAD annotation | O(all NADs in the cluster) |
| EgressIP | O(E x M) when advertising egress IPs |

Fan-out multipliers:

- A node, NAD, FRRConfiguration or UplinkState event reconciles **every** RA
  (`ReconcileAll`), de-duplicated per burst by the workqueue.
- Under EVPN, every RA spec change or deletion also reconciles every RA.
- Every generated object the controller writes comes back as an informer
  event; `frrConfigurationNeedsUpdate` drops own updates except, under EVPN,
  for generated objects, which again means `ReconcileAll`.

Measured ([08-first-ci-results](../../perf-scale/bgp-evpn/08-first-ci-results.md),
20 RAs on a 6-node kind cluster): each reconcile was dominated by the
apiserver round trips of its six object creates (p99 65 to 127 ms each),
the controller's CPU time was negligible, and because the RA controller has
one worker the queue wait p99 was about 16 s against a work p99 of about 1 s.
Twenty RAs produced exactly twenty reconciles and no redundant writes in a
create-only lane; the `ReconcileAll` fan-out has not yet been measured under
churn.

The cluster manager's VTEP controller is O(VTEPs) per VTEP reconcile and
O(VTEPs) reconciles per node annotation change; the CUDN controller's
notifier callbacks are O(CUDNs) per RA or VTEP event.

---

## Zone controller cost model

| Path | Cost | Trigger |
|---|---|---|
| advertisement flip | one node gateway re-sync: SNAT rows, pass ACL, address set update, port group update | per network, per flip |
| route import sync | O(R) netlink dump + O(LRSRs) NB read + O(delta) transaction, 500 ms coalesced | per BGP route event per tracked table |
| isolation drop ACL | constant match referencing two address sets; OVN expands membership into flows, so the datapath cost grows with the number of advertised subnets squared in the worst case | per advertised subnet |
| network manager `syncRunningNetworks` | O(networks) syncs, each O(NADs + RAs x N) | per accepted-RA change, per node label change |

Measured: route import at 126 routes synced in 16 ms p99 (same lane).

---

## Node agent cost model

| Component | Steady state | Event cost |
|---|---|---|
| VRF manager | 60 s `reconcile` over all VRFs plus `repair` listing all links | link events per VRF |
| rule manager | 5 min resync of owned priority | per rule change |
| UDN gateway `reconcileAdvertisedState` | none | flow cache rebuild O(networks on the bridge) + services resync O(services in namespaces) |
| netlink device manager | 60 s re-enqueue of every VXLAN device with jitter; each costs three bridge dumps | per managed device |
| EVPN VTEP controller | none | `collectEVPNNetworks` O(networks) under the network manager lock; OVS port list; rate-limited re-queue on every NAD event |
| EVPN pod controller | none | O(1) netlink per pod event; startup scan O(local pods) plus two table dumps per network |
| nftables isolation set | none | O(subnets of the network) elements per flip; `deleteAdvertisedUDNIsolationRules` lists both sets |

All of this is per node, so cluster-wide it is multiplied by N but spread
across N machines.

---

## Fabric cost model

| Shape | Prefixes in the fabric | Sessions per node |
|---|---|---|
| default VRF, Layer3 | N per network | P |
| default VRF, Layer2 | 1 per network (anycast from every node), N paths | P |
| VRF-Lite | as above, per VRF | P per VRF |
| EVPN | Type-2 per pod IP (x2 with IP-VRF), Type-3 per VNI per node, Type-5 per host subnet | P (one session carries all VNIs) |
| no-overlay managed | N per network, received by every node | N - 1 |

EVPN moves the dominant term from nodes to pods: at Q x N pods with both
VRF kinds that is 2 x Q x N Type-2 routes in every peer's EVPN table, and in
every node's zebra and bridge FDB and neighbor tables.

---

## Hard ceilings

| Ceiling | Value | Mechanism | Code |
|---|---|---|---|
| VLAN IDs for EVPN VRFs | 4093 usable (0 and 1 reserved), cluster-wide | SVD on a VLAN-aware bridge; the allocator is one per cluster manager, not per VTEP | `userdefinednetwork/controller.go:71-77`, `:306-314` |
| VNI | 1 to 16777215, unique per VTEP | 24-bit field, `reservedVNIs` | `evpn.go:310-312`, `controller.go:528-556` |
| interface names | 15 bytes: CUDN names longer than that get ID-derived VRF and device names | `util/multi_network.go:2197-2205`, `evpn/utils.go:62-70`, `:91-99` |
| route target | 6-byte value: auto-derived RT only works with VNI <= 65535 under a 4-byte ASN | `evpn.go:291` |
| `FRRNodeState.status.runningConfig` | etcd object size, grows with routers x neighbors x prefixes per node | frr-k8s |
| condition message | 32768 bytes; the VTEP `AllocationFailed` message is one line per failing node | `vtep/status.go:603-606` |
| RA nodeSelector | must be empty for `PodNetwork`: no way to shard the node dimension except dynamic allocation | `types.go:31` |
| managed routing | full mesh only: N - 1 sessions per node | `managedbgp/controller.go:418-420` |

---

## Concurrency and rate limiting

| Controller | Workers | Rate limiter | Max attempts |
|---|---|---|---|
| RA controller and its seven sub-controllers | 1 each | `DefaultTypedControllerRateLimiter` (exponential 5 ms to 1000 s plus 10 qps / 100 burst bucket) | default 15, then `HandleError` writes `InternalError` |
| managed BGP, VTEP, no-overlay validation, notifiers | 1 each | same | default |
| network manager per-network reconciler | 5 | same | infinite |
| route import | 1 | default | default |
| EVPN VTEP controller, NAD reconciler, pod controller | 1 each | same | VTEP default, NAD reconciler infinite |
| netlink device manager | 1 | same | infinite |

None of these are configurable. The perf-scale set's hypothesis 12 names
the single RA worker as the best-evidenced bottleneck measured so far and
proposes a threadiness knob, with the caveat that the RA reconcile reads
other RAs' generated objects for VTEP conflicts, so raising concurrency there
needs a race analysis first.

---

## Observability available today

| Signal | Where |
|---|---|
| `ovnkube_clustermanager_route_advertisement_condition{name,condition,status}` | `metrics/cluster_manager.go:136-143` |
| `ovnkube_clustermanager_vtep_condition{name,condition,status}` | `:154-161` |
| `ovnkube_clustermanager_cluster_user_defined_network_condition` (includes `TransportAccepted`) | `:145-152` |
| generic workqueue metrics per controller name (`workqueue_depth`, `_adds_total`, `_queue_duration_seconds`, `_work_duration_seconds`) | `pkg/controller` registration |
| `Finished syncing routeadvertisements "<name>", took <d>` at V(4) | `routeadvertisements/controller.go:396-398` |
| `Reconciled network ... took` at V(5) | `routeimport/route_import.go:431` |
| `NetlinkDeviceManager: reconcile <dev> took` at V(5) | `netlinkdevicemanager/controller.go:419-423` |
| frr-k8s `frrk8s_bgp_session_up`, `_announced_prefixes_total`, `_received_prefixes_total`, `_updates_total` | frr-k8s exporter |
| `FRRNodeState.status.lastConversionResult / lastReloadResult` | frr-k8s |

There is no histogram for RA reconcile duration, generated object counts or
writes, route import sync duration, or EVPN programming; the proposed set is
[02-metrics.md, table 2](../../perf-scale/bgp-evpn/02-metrics.md#table-2-missing-with-code-sites).

---

## What has been measured

The benchmark set has run the `bgp-ra-density` kube-burner lane several
times on a small kind cluster. The scoreboard in
[08-first-ci-results.md](../../perf-scale/bgp-evpn/08-first-ci-results.md#hypothesis-scoreboard)
and the November briefing ([briefing-nov2026.md](../../perf-scale/bgp-evpn/briefing-nov2026.md))
are the current record. In short: the single-worker RA queue starvation is
confirmed; the generation recompute cost is real but small next to apiserver
round trips; object explosion, fan-out under churn, EVPN per-pod cost and
node resync cost are untested at scale.

---

## Where to look before changing hot paths

| If you change | Re-read |
|---|---|
| anything in `generateFRRConfigurations` | the determinism requirements (sorted inputs) and the `DeepEqual` apply path: a non-deterministic output rewrites every object on every reconcile |
| `ObjNeedsUpdate` filters | every added field is a new `ReconcileAll` trigger |
| the NAD annotation format | `updateNADs` parses every NAD on every reconcile |
| route import filtering | `routesFromNetlinkRoute` runs per route per sync |
| EVPN pod entries | one netlink call per pod per event is the floor; batching is the only way down |
| VID allocation | cluster-wide, not per VTEP; changing that changes the node mapping contract |
