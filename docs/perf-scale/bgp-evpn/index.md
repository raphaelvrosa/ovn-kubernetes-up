# BGP and EVPN Performance and Scale

Benchmarking methodology, metric catalogue, bottleneck analysis, CI design and delivery
roadmap for the OVN-Kubernetes BGP route-advertisements and EVPN features.

## Contents

1. [Summary](#summary)
2. [Why this document set exists](#why-this-document-set-exists)
3. [What we believe will break first](#what-we-believe-will-break-first)
4. [Index](#index)
5. [Reading guide by role](#reading-guide-by-role)
6. [Scope and non-goals](#scope-and-non-goals)
7. [Status and maintenance](#status-and-maintenance)

---

## Summary

OVN-Kubernetes supports advertising cluster networks over BGP
([Route Advertisements](../../features/bgp-integration/route-advertisements.md)),
running networks without encapsulation
([No-Overlay](../../features/bgp-integration/no-overlay.md)), and integrating with an
EVPN fabric ([EVPN](../../features/bgp-integration/evpn.md)). All three delegate the BGP
speaker to [frr-k8s](https://github.com/metallb/frr-k8s) and translate Kubernetes intent into
`FRRConfiguration` objects.

These features have **full functional CI coverage and zero performance coverage**. No numbers
exist for how many nodes, ClusterUserDefinedNetworks (CUDNs), BGP peers, advertised prefixes or
imported routes the implementation supports, nor for how long convergence takes at any of those
points.

This document set defines the methodology to obtain those numbers, the metrics needed to
interpret them, the hypotheses they should confirm or refute, the CI mechanism to run them
continuously, and a phased roadmap to deliver all of it.

The target operating point used throughout is a **large cluster**: 500 nodes, 1000 CUDNs,
200 pods per node (100,000 pods), 2-8 BGP peers per node, and up to 500,000 imported fabric
routes.

---

## Why this document set exists

`test/GAPS.md` Gap 15 ("Scale and Performance E2E") already records that the existing
kube-burner lane covers only UDN/CUDN pod density, and that node scale beyond 10, OVN DB size,
and OVS flow-count limits are unmeasured. The BGP and EVPN enhancement proposals each defer
scale work to a single line:

| Source | Deferred item |
|---|---|
| `docs/okeps/okep-5296-bgp.md:629` | "Scale testing to determine impact of FRR-K8S footprint on large scale deployments" |
| `docs/okeps/okep-5296-bgp.md:646` | RIB size, communities, route-aggregation scale testing |
| `docs/okeps/okep-5088-evpn.md:1001` | Per-node IP maps in VTEP status do not scale at ~5000 nodes |

Nothing else in `docs/` covers performance for these features. This is that work, specified.

Two facts make the work cheaper than it looks:

- A **kube-burner harness already exists** — `contrib/perf/` plus
  `.github/workflows/performance-test.yml` — with Prometheus, pprof capture, Elasticsearch
  indexing, baseline comparison and automated PR comments. It needs extension, not construction.
- `performance-test.yml` **already plumbs** `ENABLE_ROUTE_ADVERTISEMENTS`,
  `ADVERTISE_DEFAULT_NETWORK` and `ADVERTISED_UDN_ISOLATION_MODE`, and already runs
  `modprobe vrf`. No matrix entry sets any of them. The wiring is there; the workloads are not.

---

## What we believe will break first

Stated up front so the benchmarks can be designed to falsify them. Full analysis with code
pointers in [Bottleneck Analysis](03-bottlenecks.md).

| # | Hypothesis | Lands in |
|---|---|---|
| 1 | `ReconcileAll()` fan-out makes RouteAdvertisements cost a function of *unrelated* cluster churn, serialised on a single worker | ovn-kubernetes |
| 2 | `generateFRRConfigurations` recomputes everything from scratch, O(nodes x sourceConfigs x routers x neighbors x prefixes) per reconcile | ovn-kubernetes |
| 3 | Generated `FRRConfiguration` count is RA x node x sourceConfig — up to 500,000 objects at target scale | ovn-kubernetes / etcd |
| 4 | `FRRNodeState.status.runningConfig` carries the whole FRR config as a string per node and will cross the etcd object size limit | frr-k8s |
| 5 | `BGPSessionState` cardinality is nodes x peers x VRFs — up to 1,000,000 objects | frr-k8s |
| 6 | `routeimport` does a full netlink dump plus full static-route scan per network per 500 ms debounce | ovn-kubernetes |
| 7 | EVPN route count scales with **pods**, not nodes, because each pod IP becomes an FDB plus neighbour entry and hence a Type-2 route | ovn-kubernetes / FRR |
| 8 | 4094 MAC-VRF + IP-VRF combinations per VTEP is a hard wall, and 1000 CUDNs with both sits at roughly half of it | EVPN / SVD design |

Numbers 4, 5 and 8 are ceilings rather than slopes: they do not degrade, they stop.

---

## Index

| Document | Contents |
|---|---|
| [Glossary](glossary.md) | Terms, acronyms and the naming convention used for SLIs |
| [00 — Background](00-background.md) | What CUDNs, VRFs, BGP and EVPN actually are, with live-cluster output showing what each creates and which process creates it |
| [01 — Benchmarking Methodology](01-methodology.md) | Topology models, hard constraints, scale ladder, derived quantities, run protocols, SLI definitions |
| [02 — Metrics and Instrumentation](02-metrics.md) | What is exported today, what is missing and where to add it, what must be measured outside Prometheus |
| [03 — Bottleneck Analysis](03-bottlenecks.md) | Twelve ranked hypotheses with mechanism, confirming metric, candidate fix, and which project owns each |
| [04 — CI and kube-burner](04-ci-kube-burner.md) | Harness design, what stock kube-burner already does, genuine upstream asks, kind's ceiling |
| [05 — Roadmap](05-roadmap.md) | Six phases with goals, activities, deliverables, exit criteria, risks and success criteria |
| [06 — Local Lab](06-local-lab.md) | Hands-on kind walkthrough: bring up BGP and EVPN, drive RAs by hand, read FRR, enable metrics, iterate on code |

---

## Reading guide by role

| Role | Read |
|---|---|
| Engineer running a benchmark | [01](01-methodology.md) then [04](04-ci-kube-burner.md) |
| Engineer interpreting results | [02](02-metrics.md) then [03](03-bottlenecks.md) |
| Engineer fixing a bottleneck | [03](03-bottlenecks.md) — every entry names the file and function |
| Engineer adding instrumentation | [02](02-metrics.md) table 2 — each row is one small PR |
| Planning a sprint | [05](05-roadmap.md), then [03](03-bottlenecks.md) for sizing |
| Reviewing the approach | This page, then [01](01-methodology.md) |
| **New to the feature** | **[00](00-background.md) then [06](06-local-lab.md)** — understand the objects, then build them in kind, before reading about how they fail |

---

## Scope and non-goals

**In scope**: control-plane and node-agent scalability of RouteAdvertisements, No-Overlay and
EVPN; the ovn-kubernetes / frr-k8s / FRR / OVN integration seams; CI mechanisms to detect
regressions.

**Out of scope**:

- Dataplane throughput and packet-per-second benchmarking. Advertised networks change *where*
  packets go, and in shared-gateway and no-overlay modes remove encapsulation, but forwarding
  performance is an OVS/OVN concern measured by existing traffic-flow tests.
- FRR's own BGP scalability in isolation. We measure FRR as integrated, and report upstream
  when it is the limiter.
- MetalLB. `test/GAPS.md` C1 records `MetalLB|BGP: None` as an interaction gap; it is real but
  it is a functional gap first.

---

## Status and maintenance

All measured numbers in this set are marked as such. Anything unmarked is a derivation or a
hypothesis, not a result.

Every code reference is given as `path:symbol` and, where a line number is used, it is accurate
as of the commit that introduced this document. Line numbers rot; the symbol names are the
durable part. [Roadmap](05-roadmap.md) P0 includes a grep-based staleness check for these
pointers.
