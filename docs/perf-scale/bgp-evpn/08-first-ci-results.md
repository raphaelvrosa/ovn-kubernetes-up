# 08 — First CI Results

The first measured run of the `bgp-ra-density` lane. Confirms bottleneck hypothesis 1 and 12
from [03 — Bottleneck Analysis](03-bottlenecks.md) at six nodes, rules out eight others at this
scale, and records four harness defects the run exposed.

## Contents

1. [Run identity](#run-identity)
2. [What the lane produced](#what-the-lane-produced)
3. [Finding: the RouteAdvertisements queue is starved, not busy](#finding-the-routeadvertisements-queue-is-starved-not-busy)
4. [Negative results](#negative-results)
5. [Harness defects exposed](#harness-defects-exposed)
6. [Open anomaly](#open-anomaly)
7. [Hypothesis scoreboard](#hypothesis-scoreboard)
8. [Next lanes](#next-lanes)

---

## Run identity

| Field | Value |
|---|---|
| Workflow run | [37926123305](https://github.com/ovn-kubernetes/ovn-kubernetes/actions/runs/37926123305) |
| Pull request | [#7077](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/7077), commit `63b2c2b` |
| Date | 2026-10-09, 12:13–12:18 UTC |
| kube-burner | v2.5.0 (`6eb71c4`) |
| Runner | `oracle-vm-32cpu-128gb-x86-64` |
| Cluster | kind, **6 nodes** (1 control-plane + 3 workers + 2 infra) |
| Mode | local gateway, IPv4, interconnect single-node zones, network segmentation on |
| Workload | 20 namespaces, 20 Layer3 CUDNs, 20 RouteAdvertisements (`advertise-cudn`) |
| Result | both jobs `passed: true`, kube-burner rc=0 |

**Node count is not `num-workers`.** `.github/workflows/performance-test.yml` sets
`KIND_NUM_INFRA: "2"` globally, so the node total is `num-workers + 2 + 1`. Every derived
quantity below scales with 6, not 3.

---

## What the lane produced

The fan-out identity predicted in [01 — Methodology](01-methodology.md) held exactly.

| Quantity | Measured | Identity |
|---|---|---|
| RouteAdvertisements accepted | 20 / 20 | — |
| Generated FRRConfigurations | **120** (6 per RA) | RAs x nodes x source configs = 20 x 6 x 1 |
| Advertised prefixes | 120 (6 per RA, IPv4 only) | one `/24` per node per Layer3 CUDN |
| `frrconfigurations` objects in etcd | 1 to **121** | 120 generated + 1 source |
| Generated object bytes | 133,728 total, 1,128 max | — |
| All 20 RAs created to `Accepted=True` | **42 s** | approx 2.1 s per RA |

Twenty-five of 25 queries in `contrib/perf/metrics-ovnk.yml` fired, 24 with data. Seventy pprof
profiles captured. The out-of-band hook wrote `bgp-state-bgp-ra-density.json`.

---

## Finding: the RouteAdvertisements queue is starved, not busy

| Queue | add rate (peak) | queue wait p99 | work p99 | wait/work |
|---|---|---|---|---|
| **clustermanager routeadvertisements controller** | 0.22 /s | **16.08 s** | **1.02 s** | **15.8x** |
| clustermanager-node-node | 19.85 /s | 0.23 s | 0.27 s | 0.9x |
| `[clustermanager-nad-controller network controller]` | 5.11 /s | 0.10 s | 0.10 s | 1.0x |
| cluster-user-defined-network-controller | 0.81 /s | 0.17 s | 0.12 s | 1.4x |
| clustermanager routeadvertisements node controller | 1.63 /s | 0.002 s | 0.007 s | — |

Peak depth on the RA controller queue: **15 of 20 RAs waiting at once**.

The shape matters more than the magnitude. The RA controller has the **lowest** add rate of any
queue doing real work and the **highest** wait by two orders of magnitude. Every other queue
has wait approximately equal to work, meaning no backlog. This is not event volume. It is 20
items at roughly 1 s each through `Threadiness: 1`, so 16 s of the 42 s run is pure queueing.

This confirms:

- **Hypothesis 1** (`ReconcileAll()` fan-out) — the `clustermanager-node-node` queue peaking at
  19.85 adds/s while the RA queue takes 0.22/s is the amplification path in miniature.
- **Hypothesis 12** (hardcoded `Threadiness: 1`) — directly, and it is now the cheapest
  experiment available with a number to beat.

**The widened histogram buckets are what made this visible.** Under the previous
`ExponentialBuckets(10e-3, 10, 6)` scheme in `go-controller/pkg/metrics/workqueue.go`, both
16.08 s and 1.02 s fall in the same "1 s to 100 s" bucket and the ratio cannot be seen. The
bucket change shipped in the P1 instrumentation work is load-bearing for this result.

---

## Negative results

Nothing else is near a limit at this size. Recording this matters as much as the finding above,
because it says where **not** to spend the next sprint.

| Subsystem | Measured | Verdict |
|---|---|---|
| Cluster manager CPU | avg 0.75%, max 3.34% | not CPU-bound |
| Cluster manager RSS | 139 to 161 MB, heap delta 8.5 MB | no leak; top allocations are gzip, regexp and apimachinery decode, nothing RA-specific |
| libovsdb NB transactions | 95 txn/s peak, p99 **13 ms**, 13 ops/txn | fine |
| OVN NB / SB growth | +53 KiB / +124 KiB for 20 CUDNs | approx 2.7 KiB NB per CUDN |
| northd loop p95 | 19 ms | idle |
| ovn-controller flow installation p95 | 1 ms, 35,575 br-int flows | idle |
| Route import sync p99 | 16 ms, 126 routes each from `bgp` and `ovn`, 42 series | fine |
| Workqueue retries | zero | no reconcile failures |
| OVSDB try-again | zero | no contention |
| ovnkube-controller (node) CPU | avg 2.44%, max 19.79% | highest consumer, but this lane has no pods |

Hypotheses 2 (generation cost), 3 (object explosion), 4 (`FRRNodeState` size), 7 (route import)
and 9 to 11 are all **untested** rather than refuted: six nodes and 20 RAs is far below where
any of them was predicted to bite.

---

## Harness defects exposed

All four are in the lane, not the product. None invalidates the sections above.

### a. pprof missed the busy window

`pprofInterval: 1m` against a `?seconds=30` profile is a 50 percent duty cycle. Captures cover
12:14:25–12:14:55 and 12:15:55–12:16:25; the RA work ran 12:14:55–12:15:37 and fell in the gap.
Both captured profiles show 30 to 40 ms of samples over 30 s, i.e. the idle periods were
profiled.

**Fix** — `pprofInterval: 30s`, matching the profile duration.

### b. The state hook snapshots before convergence

kube-burner runs `beforeCleanup` immediately after object creation and **before** `jobPause`
(`pkg/burner/job.go:201` against `:222`). The hook fired 12 s into the job. Evidence it
matters:

| Quantity | At snapshot | Settled (local reference) |
|---|---|---|
| Prefixes received by peer | 54 | 120 advertised |
| `FRRNodeState.status.runningConfig` | approx 2,306 B/node | approx 11,400 B/node |

Those two numbers in the first run are pre-convergence artifacts, not results.

**Fix** — `collect-bgp-state.sh` now polls until the generated object count and total
`runningConfig` size hold still across two consecutive checks, then snapshots. There is no
post-pause hook in kube-burner, so the wait has to live in the script.

### c. Per-RA `rate()` queries cannot work on a one-shot workload

`raReconcileDuration99th`, `raNADWriteRate` and `routeImportOpRate` returned nothing, and
`raFRRConfigurationWriteRate` returned 3 documents for exactly one RA. Grouping `by (name, ...)`
gives each RA its own counter that is born mid-window and increments once; `rate()` over such a
series is 0 and the `> 0` filter drops it.

**Fix** — aggregate by `result` / `op` instead of by RA name. Per-queue workqueue metrics keep
`by (name)` because there `name` is a long-lived queue, not an RA.

### d. Node count is not what the matrix says

Covered under [Run identity](#run-identity). Documented in the matrix comment rather than
changed, because the two infra nodes are wanted.

---

## Open anomaly

`bgpSessionStates: {count: 12, established: 6}` — twelve objects for six nodes, half
established, while the peer reports `peerCount: 6, failedPeers: 0`. Two session-state objects
per node with only one up, on a run where nothing requested a second session.

Candidate explanations, none confirmed: an address-family or VRF artifact of frr-k8s status
reporting, or the same pre-convergence timing as defect (b). Not diagnosable from the
artifacts. Re-check once (b) is in effect; if it persists, it is relevant to hypothesis 5
(`BGPSessionState` cardinality = nodes x peers x VRFs).

---

## Hypothesis scoreboard

| # | Hypothesis | Status after this run |
|---|---|---|
| 1 | `ReconcileAll()` fan-out | **Confirmed** at six nodes |
| 2 | `generateFRRConfigurations` recomputed from scratch | Untested — needs the node axis |
| 3 | Generated object explosion | Untested — 121 objects is trivial |
| 4 | `FRRNodeState.status.runningConfig` size | Untested — measurement was premature |
| 5 | `BGPSessionState` object count | Possible early signal, see anomaly |
| 6 | frr-k8s metrics exporter scrape cost | Untested |
| 7 | `routeimport syncNetwork` | Not a factor at 126 routes (16 ms p99) |
| 8 | EVPN per-pod datapath | Out of scope for this lane |
| 9 | `getEgressIPsByNodesByNetworks` | Not exercised (no EgressIP) |
| 10 | `addAdvertisedNetworkIsolation` | Untested — needs strict/loose axis |
| 11 | Periodic full resyncs | Not visible at this size |
| 12 | Hardcoded `Threadiness: 1` | **Confirmed** — 15.8x wait/work ratio |

---

## Next lanes

Fix (a) to (c) first. Without (b) the fabric-side column is unusable at any scale, and without
(c) the per-RA series stay empty however long the run gets.

Then **separate the two cost axes**. The most valuable thing the next runs can produce is the
slope of reconcile cost, which needs the axes varied independently rather than together.

| Lane | Nodes | CUDNs / RAs | Question |
|---|---|---|---|
| baseline (done) | 6 | 20 | — |
| `bgp-ra-density-wide` | 6, 12, 24 | 20 fixed | how does per-reconcile **work p99** scale with node count? `generateFRRConfigurations` is O(nodes x sourceConfigs x routers x ...), so this should be the steeper curve |
| `bgp-ra-density-deep` | 6 fixed | 20, 50, 100, 200 | how does **queue wait** scale with RA count at `Threadiness: 1`? Expect linear, with depth tracking RA count |
| `bgp-ra-churn` | 6 fixed | 50, churn 20 percent per cycle | steady-state cost of `ReconcileAll()` under unrelated node and NAD churn — the production-relevant number |

Twenty-four nodes is roughly the kind ceiling on the 32c/128G runner. Beyond that, use the
real-hardware procedure in [04 — CI and kube-burner](04-ci-kube-burner.md). At 200 RAs x 24
nodes the generated count reaches 4,800, which is where hypotheses 3 and 4 start to bite.

**Then run the one experiment this data already justifies.** Threadiness is hardcoded to 1 at
30-plus call sites. The measured 15.8x wait-to-work ratio is the precondition for that fix
mattering. Re-running `bgp-ra-density-deep` with threadiness 4 against the same workload is a
clean A/B with an obvious success criterion: wait p99 approaching work p99.

### Extrapolation, stated as a hypothesis

Per-RA reconcile is approximately 1 s at six nodes, and the generation path is linear in nodes.
If that holds, 500 nodes gives approximately 83 s per RA; 1,000 RAs serialised through one
worker is then roughly 23 hours to converge, against 500,000 generated FRRConfigurations.

This is a crude linear model and almost certainly wrong in its constant. It is recorded because
even a far gentler slope leaves the serialisation alone sufficient to put the 1,000-CUDN target
out of reach without either threadiness or the `ReconcileAll()` narrowing. The
`bgp-ra-density-wide` lane exists to replace this arithmetic with a measured slope.
