# 01 — Benchmarking Methodology

How to construct, run and interpret a BGP/EVPN scale benchmark for OVN-Kubernetes.

## Contents

1. [Principles](#principles)
2. [Topology models](#topology-models)
3. [Constraints that bound the matrix](#constraints-that-bound-the-matrix)
4. [Scale ladder](#scale-ladder)
5. [Derived quantities](#derived-quantities)
6. [Workload matrix](#workload-matrix)
7. [Run protocols](#run-protocols)
8. [Service level indicators](#service-level-indicators)
9. [Reporting rules](#reporting-rules)

---

## Principles

1. **State the topology model before the numbers.** "1000 CUDNs" means nothing until you say
   how they map onto nodes. The mapping changes the derived object count by an order of
   magnitude. See [Topology models](#topology-models).
2. **Climb a ladder, do not jump to target.** Run each rung, record the result, stop at the
   first failure. A benchmark that only runs at 500 nodes tells you it failed, not where.
3. **Derive, do not assume.** Advertised prefix counts, generated object counts and session
   counts are consequences of the inputs. Compute them, publish the derivation, then check the
   measurement against it. A mismatch is itself a finding.
4. **Measure the incremental case.** For RouteAdvertisements, steady-state cost is a function
   of unrelated cluster churn, so a cold-start-only benchmark measures the wrong thing.
5. **Separate ceilings from slopes.** Some limits degrade gracefully and some stop dead.
   Report the second kind as a number with the failure mode, not as a curve.
6. **Every result names the hop it exercises.** The path from RA to packet crosses four
   projects. A number without a hop attribution cannot drive a fix.

---

## Topology models

The single most important decision. All three models are legitimate deployments and they
stress different code.

| Model | Definition | What it stresses | Primary risk exposed |
|---|---|---|---|
| **Dense** | Few CUDNs, each active on all nodes | Per-node prefix list length; OVN logical topology per node; the union of prefixes carried in every generated object | Prefix-list size, `DeepEqual` cost, nbdb size |
| **Sparse** | Many CUDNs, each active on a small node subset. Requires `EnableDynamicUDNAllocation` so the `NodeHasNetwork` skip in `generateFRRConfigurations` keeps output sparse | Object count, controller reconcile count, NAD annotation churn | Generated `FRRConfiguration` explosion, apiserver/etcd pressure |
| **Mesh** | `[bgp-managed] topology=full-mesh` with `transport=no-overlay`. `managedbgp` builds one base `FRRConfiguration` listing every node's v4 and v6 address as a neighbour, then the RA controller fans it out per node | Neighbour count, per-neighbour prefix copies, O(N^2) growth | `FRRNodeState` size, BGP session count, FRR reload time |

**Sparse is the realistic shape for the 1000-CUDN target.** Dense is the worst case for a
smaller CUDN count. Mesh is a different product configuration and must be benchmarked
separately rather than combined with the other two.

Recommended assignment for the standard runs:

- Dense: 20 CUDNs x all nodes
- Sparse: 1000 CUDNs x 50 nodes each
- Mesh: default network only, all nodes, no external fabric

---

## Constraints that bound the matrix

Every one of these is documented in the feature pages' Known Limitations
([route-advertisements](../../features/bgp-integration/route-advertisements.md),
[evpn](../../features/bgp-integration/evpn.md)). They are listed here because each one either
caps a dimension or dictates the shape of a workload template, and discovering them during a
run wastes a run.

| Constraint | Consequence for the benchmark |
|---|---|
| `PodNetwork` advertisement requires a nodeSelector matching **every** node. The CRD's CEL rule rejects a non-empty `nodeSelector` when `PodNetwork` is advertised | The RA x node fan-out **cannot** be reduced by node selection. Only `EnableDynamicUDNAllocation` reduces it, and only for generated output, not for the reconcile loop |
| **4094 MAC-VRF + IP-VRF combinations per VTEP** (SVD VLAN ceiling) | 1000 CUDNs with both a MAC-VRF and an IP-VRF is 2000 VLANs — **roughly half the hard ceiling**. The ladder must cross it deliberately using multiple VTEPs, not trip over it. `test/e2e/allocators/bgp.go` already encodes `vidMax = 4094` |
| Advertised CUDN names must be **under 16 characters** (VRF name predictability) | kube-burner templates must generate short unique names such as `c{% raw %}{{.Replica}}{% endraw %}`, not the long names used by the existing density workloads |
| VRF-Lite and EVPN are **local gateway mode only** | The existing perf lane is already `gateway-mode: local`, so no change is needed. Shared-gateway BGP without VRFs is a separate lane |
| EgressIP advertisement is unsupported on Layer2 CUDNs and in VRF-Lite | The EgressIP axis combines only with Layer3 and only with `targetVRF: default` |
| Interconnect mode only | Matches the existing perf lane's `ic-single-node-zones` |
| EVPN requires route advertisements enabled, and is validated as such in config | `--enable-evpn` implies `--enable-route-advertisements` |
| VXLAN port fixed at 4789 | No port-multiplexing variable |
| Per-node IP maps in `VTEP` status are already flagged as non-scaling at ~5000 nodes (`docs/okeps/okep-5088-evpn.md:1001`) | Measure `VTEP` object size across the node ladder to find where the curve starts, well before 5000 |

---

## Scale ladder

Run in this order. Each dimension is climbed with the others held at the previous rung's
passing value.

| Dimension | Rungs | Notes |
|---|---|---|
| Nodes | 24 → 60 → 120 → 250 → 500 | Kind caps out around 10; see [04](04-ci-kube-burner.md#the-kind-ceiling) |
| CUDNs | 10 → 100 → 400 → 1000 | Under the chosen topology model |
| Pods per node | 50 → 120 → 200 | 200 x 500 = 100,000 pods at target |
| eBGP peers per node | 1 → 2 → 4 → 8 | Fabric model. Mesh model is separately at 2(N-1) |
| Imported fabric routes | 1k → 10k → 100k → 500k | Injected from the fabric side, not from the cluster |
| VTEPs | 1 → 2 → 4 | Only needed once MAC-VRF + IP-VRF count approaches 4094 |
| Isolation mode | `strict` → `loose` | A first-class axis: `loose` skips the entire `udn_isolation` ACL and address-set path |

Advertised prefix count is **not** a ladder dimension. It is derived — see below.

---

## Derived quantities

Compute these before running, publish them alongside results, and treat a divergence between
predicted and observed as a finding in its own right.

### Advertised prefixes

| Advertisement | Prefixes generated | Per |
|---|---|---|
| `PodNetwork`, Layer3 | Node's host subnets for the network, one per IP family | node x network |
| `PodNetwork`, Layer2 | The whole network subnet, identically from every node (anycast) | node x network |
| `EgressIP` | One `/32` or `/128` per EgressIP, only from the node currently hosting it | EgressIP |
| EVPN underlay | One `/32` or `/128` VTEP host route | node |

There is **no per-pod prefix in the BGP path.** Per-pod advertisement exists only under EVPN,
via Type-2 routes originated by zebra from the per-pod FDB and neighbour entries programmed by
the node agent. This is the single most important scale difference between the two features:

> Plain route advertisements scale with **nodes x networks**.
> EVPN scales with **pods**.

### Generated FRRConfiguration objects

```
generated = RAs x nodes_where_network_is_active x matching_source_FRRConfigurations
```

Worked examples at the 500-node / 1000-CUDN target:

| Scenario | Count | Comment |
|---|---|---|
| Sparse, one RA per CUDN, **without** dynamic UDN allocation | 1000 x 500 x 1 = **500,000** | `PodNetwork` forces an all-node selector, so every RA is evaluated against every node |
| Sparse, one RA per CUDN, **with** dynamic UDN allocation | 1000 x 50 x 1 = **50,000** | The `NodeHasNetwork` skip elides inactive nodes |
| Dense, one RA selecting all CUDNs | 1 x 500 x 1 = **500** | But each object carries the union of every active network's prefixes |

**Quantifying that 10x delta is one of the headline results of the whole exercise**, because it
is the difference between a plausible deployment and an unusable one, and it is controlled by
a single feature flag.

### Prefix entries cluster-wide

Dense, 20 CUDNs x 500 nodes x 2 families x (1 + peers) copies of the prefix list per generated
object. With 4 peers this is on the order of **1,000,000 prefix strings** held in etcd across
generated objects. The prefix list is duplicated per neighbour inside
`ToAdvertise.Allowed.Prefixes`, so peer count multiplies storage, not just session count.

### BGP sessions and BGPSessionState objects

```
sessions = nodes x peers_per_node x VRFs
```

| Model | Sessions | `BGPSessionState` objects |
|---|---|---|
| Fabric, 2 peers, default VRF only | 500 x 2 x 1 = 1,000 | 1,000 |
| Fabric, 2 peers, 1000 VRFs | 500 x 2 x 1000 = **1,000,000** | **1,000,000** |
| Mesh, no VRFs | 500 x 998 = **499,000** | 499,000 |

The million-object cases are frr-k8s design limits, not ovn-kubernetes ones, and must be
reported as such. Measure the point at which `BGPSessionState` write rate saturates the
apiserver.

### FRRNodeState size

`status.runningConfig` is the entire rendered FRR configuration as a single string. It grows
with VRF count, neighbour count and EVPN raw-config blocks. Measure at 10 / 100 / 400 / 1000
VRFs and extrapolate against the **1.5 MB default etcd object limit**. Report the VRF count at
which the object becomes unwritable — this is a ceiling, not a slope.

### EVPN VLAN consumption

```
VLANs_per_VTEP = MAC-VRFs + IP-VRFs assigned to that VTEP,  ceiling 4094
```

---

## Workload matrix

Five workloads. Each maps to one kube-burner configuration in `contrib/perf/workloads/`.

| Workload | Purpose | Topology | Key objects |
|---|---|---|---|
| `bgp-ra-density` | Control-plane cost of advertising many networks | Sparse and Dense | CUDN, RA, base FRRConfiguration, namespaces, pods |
| `bgp-ra-churn` | Leak and stale-state detection under repeated create/delete | Sparse | Same, with `churnConfig` |
| `bgp-route-import` | Node-agent cost of importing fabric routes | Any | Routes injected from the fabric; no cluster objects |
| `evpn-density` | Cost of many EVPN networks (VLAN/VNI consumption, VTEP reconcile) | Sparse | VTEP, CUDN with `transport: EVPN`, RA with `targetVRF: auto` |
| `evpn-pod-density` | Cost of many pods on EVPN networks (Type-2 route count, FDB/neigh programming) | Dense, few networks | Pods on EVPN CUDNs |

Cross-cutting variants applied to `bgp-ra-density`:

| Variant | Why |
|---|---|
| `strict` vs `loose` isolation | Isolates the `udn_isolation` O(subnets^2) path |
| dynamic UDN allocation on/off | Isolates the 10x generated-object delta |
| `PodNetwork` vs `PodNetwork + EgressIP` | Isolates `getEgressIPsByNodesByNetworks`, O(EIPs x namespaces) |
| Layer2 vs Layer3 CUDN | Layer2 advertises one anycast subnet, Layer3 advertises per-node subnets |
| overlay vs `no-overlay` | `no-overlay` changes the `ToReceive` filters and enables route import of pod subnets |

---

## Run protocols

Three protocols, measured and reported separately. Mixing them produces numbers nobody can act
on.

### Cold start

Bring a component up into a cluster already holding the target state. Restart
ovnkube-control-plane, the frr-k8s daemon, or a node agent, and measure time to steady state.

Measures: initial sync cost, `cleanStalePodEntries`-style boot scans, informer cache fill,
first full `generateFRRConfigurations` pass.

### Incremental

With the cluster at the target rung, apply a single change and measure its cost and latency.
**This is the primary protocol for RouteAdvertisements** because `ReconcileAll()` makes the
cost of a change depend on how much unrelated activity is happening at the same time.

Variants:

| Change | Expected to trigger |
|---|---|
| Add one CUDN + RA | One reconcile of one RA, plus a NAD list of all NADs |
| Add one node | `ReconcileAll()` of every RA |
| Relabel one node | `ReconcileAll()` of every RA, via `nodeNeedsUpdate` |
| Add one pod | Nothing in the BGP path; a Type-2 route in the EVPN path |
| Move an EgressIP | Reconcile of every EgressIP-advertising RA |
| Edit a base FRRConfiguration | `ReconcileAll()` of every RA |

The "relabel one node" case deserves its own graph. It is semantically a no-op for BGP and
should cost nothing.

**Background-churn variant**: run the incremental measurement while a separate churn job adds
and removes unrelated pods and nodes. The delta between quiet and churning is the direct
measure of `ReconcileAll()` amplification.

### Failure recovery

| Scenario | Measure |
|---|---|
| Node reboot | Time to re-establish sessions and re-advertise; whether routes are withdrawn cleanly |
| frr-k8s daemon restart | Reload time, session flap duration, dataplane impact |
| ToR / peer flap | Route withdrawal and reconvergence; the `routeimport` debounce behaviour under a full-table withdrawal and re-announce |
| ovnkube-control-plane leader change | Time to first correct `FRRConfiguration` write; whether spurious rewrites occur |
| OVN nbdb leader change | Transaction failure and retry behaviour under `--db-txn-timeout` |

The ToR flap case is the one most likely to find a real defect: it produces the largest single
`routeimport` transaction the system will ever see.

---

## Service level indicators

Defined in [the glossary](glossary.md#sli-naming-convention). Targets below are **proposals to
be ratified against the first measured run**, not established requirements. They exist so the
first run has something to fail against.

| SLI | Proposed target at 500 nodes | Rationale |
|---|---|---|
| `ra_accepted_latency` p99 | < 30 s | A status condition is cheap; anything slower indicates queue starvation |
| `frrconfig_rendered_latency` p99 | < 120 s | Bounded by FRR reload across all nodes |
| `route_advertised_latency` p99 | < 180 s | The user-visible number: declare intent, see the route |
| `route_imported_latency` p99 | < 10 s | Debounce is 500 ms; the rest is one nbdb transaction |
| `evpn_pod_reachable_latency` p99 | < 15 s over pod-ready | Should be close to normal pod-to-pod setup |
| ovnkube-control-plane RSS | Published, not capped | Report the curve; set a cap once the shape is known |
| ovnkube-control-plane CPU steady state | < 1 core with no cluster churn | Non-zero steady-state CPU with an idle cluster indicates a resync loop |

The last row is a deliberate trap for hypothesis 1: with an idle cluster and no intent changes,
BGP control-plane CPU should be approximately zero.

---

## Reporting rules

1. Every published number carries its topology model, ladder position, protocol, gateway mode,
   IP family, isolation mode and commit SHA.
2. Predicted derived quantities are published next to measured ones.
3. Failures are reported with the failure mode, not just the rung: "500 nodes: apiserver
   `etcdserver: request is too large` on FRRNodeState write" is useful; "500 nodes: failed" is
   not.
4. Ceilings are reported as a number and a mechanism, and are linked from
   [03](03-bottlenecks.md).
5. Kind-based results are labelled **relative** and may only be used for regression comparison.
   Absolute ceilings come from real-hardware runs only.
