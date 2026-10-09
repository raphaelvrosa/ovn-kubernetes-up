# 08 — First CI Results

The first two measured runs of the `bgp-ra-density` lane. Confirms bottleneck hypotheses 1 and
12 from [03 — Bottleneck Analysis](03-bottlenecks.md) at six nodes, demotes 2 and 4 on measured
evidence, rules out six others at this scale, and records the harness defects the runs exposed.

!!! info "Two runs"

    **Run 1** [37926123305](https://github.com/ovn-kubernetes/ovn-kubernetes/actions/runs/37926123305),
    commit `63b2c2b`. **Run 2**
    [37936933421](https://github.com/ovn-kubernetes/ovn-kubernetes/actions/runs/37936933421),
    commit `d038946`, with the run-1 harness fixes applied. Same six-node shape and workload.
    Both succeeded. Sections below give run 1 → run 2 where the figure moved.

## Contents

1. [Run identity](#run-identity)
2. [What the lane produced](#what-the-lane-produced)
3. [Finding: the RouteAdvertisements queue is starved, not busy](#finding-the-routeadvertisements-queue-is-starved-not-busy)
4. [Negative results](#negative-results)
5. [The reconcile is I/O-bound, not CPU-bound](#the-reconcile-is-io-bound-not-cpu-bound)
6. [frr-k8s compacts the generated configs 9.7x](#frr-k8s-compacts-the-generated-configs-97x)
7. [Harness defects exposed](#harness-defects-exposed)
8. [Open anomaly](#open-anomaly)
9. [Corrections](#corrections)
10. [Hypothesis scoreboard](#hypothesis-scoreboard)
11. [Next lanes](#next-lanes)

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

**Reproduced in run 2 almost exactly**, on an independently built cluster: wait p99
16.077 → **16.056 s**, work p99 1.019 → **1.019 s**. Supporting figures are equally stable:
libovsdb 95 → 97 txn/s, p99 13 → 12 ms, OVN DB total 4.03 → 4.07 MB. The lane is a reliable
instrument, which is the precondition for using it as a regression gate later.

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

## The reconcile is I/O-bound, not CPU-bound

Run 2 restored continuous pprof coverage, and the arithmetic that came out of it is the most
consequential thing in either run.

| Term | Value |
|---|---|
| RA handler wall time | 20 items x 1.019 s work p99 = **approx 20 s** |
| Cluster manager CPU over the window | 0.76% avg of a core over approx 240 s = **approx 1.8 core-seconds** |
| Busiest control-plane profile | 210 ms of samples, **entirely Go runtime GC and scheduler, zero ovn-kubernetes frames** |
| apiserver `APIInflightRequests` | never above **2 mutating / 2 readOnly** |

So **at least 90% of the time inside `reconcileRouteAdvertisements` is spent blocked on the
apiserver, not computing.** The inflight ceiling of 2 is the single-worker signature seen from
the server side: one writer at a time, no server-side contention, pure serialisation.

Two consequences:

- **Hypothesis 2 is demoted.** The recompute-from-scratch and the `// TODO perhaps cache across
  reconciles` at `controller.go:765`/`:786` are real, but they are not where the second goes.
  Caching cheap CPU buys little.
- **Hypothesis 12 is promoted.** Parallel blocking I/O is exactly what more workers fix, and the
  expected gain is near-linear until apiserver inflight becomes the limit — currently 2. This is
  now [roadmap P2b](05-roadmap.md#phase-p2b-threadiness-ab) rather than Activity 4.3 behind the
  whole ladder.

It also exposed a gap: ovnkube registered **no** client-go request metrics at all.
`k8s.io/client-go/tools/metrics` was vendored but nothing called `metrics.Register`, so the
dominant term could only be reached by subtracting CPU from workqueue work duration. Now added
as [P1 Activity 1.4b](05-roadmap.md#activity-14b-client-go-request-metrics).

---

## frr-k8s compacts the generated configs 9.7x

| Quantity | Value |
|---|---|
| Generated FRRConfigurations | 120 objects, 133,728 bytes |
| Merged `FRRNodeState.status.runningConfig` | 13,841 bytes total, **2,306 B/node** |
| Compaction | **9.7x** |
| Headroom to the 1.5 MB etcd object limit | approx **650x** current per-node size |

The 20 per-node configurations share a neighbour and differ only in prefixes, which is the
shape that merges best. **Hypothesis 4 is demoted from Critical to Medium**: it is no longer a
candidate for "fails first". The axis that matters is distinct prefixes and VRFs rather than
object count, so EVPN — whose `rawconfig.go` emits per-VRF stanzas that do not merge the same
way — remains the case to watch.

---

## Harness defects exposed

All four are in the lane, not the product. None invalidates the sections above.

### a. pprof missed the busy window — partly fixed, now moot

`pprofInterval: 1m` against a `?seconds=30` profile is a 50 percent duty cycle. In run 1,
captures covered 12:14:25–12:14:55 and 12:15:55–12:16:25 while the RA work ran
12:14:55–12:15:37, so it fell in the gap.

**Fixed to `pprofInterval: 30s`.** Run 2 captured 112 profiles against 70, with continuous 30 s
coverage. The work window is *still* not covered — `start.pprof` ends at 13:48:48 and the first
periodic profile begins 13:49:18, while the work ran 13:48:48–13:49:01 — but this no longer
matters, because there is approximately 1.8 core-seconds of CPU in the whole run to find. See
[I/O-bound](#the-reconcile-is-io-bound-not-cpu-bound). Chasing CPU profiles for this controller
is the wrong instrument; client-go request metrics are the right one.

### b. The state hook snapshots before convergence — fixed

kube-burner runs `beforeCleanup` immediately after object creation and **before** `jobPause`
(`pkg/burner/job.go:201` against `:222`). The hook fired 12 s into the job. Evidence it
matters:

| Quantity | Run 1 (at snapshot) | Run 2 (settled) |
|---|---|---|
| Prefixes received by peer | 54 | **120**, matching the 120 advertised |
| Peer RIB count | 113 | 245 |
| Prefixes sent to nodes | 342 | 738 |

**Fixed.** `collect-bgp-state.sh` polls until the generated object count and total
`runningConfig` size hold still across two consecutive checks, then snapshots. There is no
post-pause hook in kube-burner, so the wait has to live in the script. Run 2 logged
`settled after 10s at 120/13841`; job time went 42 s to 53 s.

### c. `rate()` cannot see a one-shot workload — first diagnosis was wrong

`raReconcileDuration99th`, `raNADWriteRate` and `routeImportOpRate` returned nothing, and
`raFRRConfigurationWriteRate` returned 3 documents for exactly one RA.

**The first diagnosis, per-RA label cardinality, was wrong.** Run 2 shipped the aggregated form
and three queries were still empty, with `raFRRConfigurationWriteRate` going from 3 documents
to zero.

The real cause is visible in the sample timestamps: run 2's first in-window sample is 13:49:17
and it **already reads 120 generated configs and 20 accepted RAs**, while the transition
finished at approximately 13:49:01. Any counter that fires only during the transition is
therefore flat across every sample in the window, so `rate()` is zero everywhere and `> 0`
drops it. Run 1's three documents were scrape-phase luck: its first sample caught the ramp
mid-flight at 30 of 120. Same query, different alignment.

**Fix** — drop `rate()` for these and read the cumulative counters directly, as
`raGeneratedFRRConfigurations` and `raNADsListed` already do. For the histogram, take
`histogram_quantile` over the raw buckets: on a cluster built for the run,
cumulative-since-start *is* the workload's distribution. Cumulative totals are the more useful
number for a density lane anyway, being how many writes the workload caused rather than writes
per second. The workqueue queries keep `rate()`, because those queues are continuously
active.

### d. Node count is not what the matrix says

Covered under [Run identity](#run-identity). Documented in the matrix comment rather than
changed, because the two infra nodes are wanted.

---

## Open anomaly

`bgpSessionStates: {count: 12, established: 6}` — twelve objects for six nodes, half
established, while the peer reports `peerCount: 6, failedPeers: 0`.

**Identical in run 2 after a proper settle, so it is not a convergence-timing artifact.**

What has been ruled out and established since:

| Check | Result |
|---|---|
| Stale objects from a previous frr-k8s daemon generation | **Ruled out.** A daemonset rollout on a lab cluster replaces them cleanly; the count stays constant. They are owned by the daemon Pod and garbage-collected with it. |
| Does it track RA count? | **No.** A three-node lab with 21 RouteAdvertisements has 3 objects, all established. |
| Does it track node count? | **Yes.** Six nodes gives twelve, i.e. two per node. |
| Source configuration neighbours (lab) | One: a single router with a single neighbour. |

The `receive-all` template in `contrib/frr-k8s/patches/0001-Improvements-to-the-demo.patch`
defines a **second router block** with `SsFrr*` neighbours alongside the primary `Frr*` ones.
The leading explanation is therefore that CI populates both and the second neighbour never
establishes — a harness configuration artifact, **not** evidence for
[hypothesis 5](03-bottlenecks.md#5-bgpsessionstate-cardinality).

Not confirmed, because it is not reproducible on the lab cluster. The run hook now records
`bgpSessionStates.byStatus`, `.byPeer`, `.byVRF` and `.nodes`, plus `sourceFRRConfigurations`
with each source config's routers and neighbour addresses. The next run settles it without
guesswork.

---

## Corrections

Two claims made after run 1 did not survive run 2. Recorded rather than silently edited,
because both were used to justify a change.

**`FRRNodeState` was never captured pre-convergence.** Byte counts are identical in both runs
(2312/2305/2306/2306/2306/2306). The "approximately 11,400 B/node settled" figure came from a
local cluster with different content — a `default` RouteAdvertisements advertising the pod
network on top of 21 others — not from a settled version of this workload. Defect (b) was real
and worth fixing, but only the **peer-side prefix counts** were genuinely premature; the
`runningConfig` row of that table was wrong. The corrected reading is in
[the compaction section](#frr-k8s-compacts-the-generated-configs-97x), and it is what demotes
hypothesis 4.

**The diagnosis of defect (c) was wrong**, and the fix shipped for it did not work. See
[defect c](#c-rate-cannot-see-a-one-shot-workload-first-diagnosis-was-wrong).

---

## Hypothesis scoreboard

| # | Hypothesis | Status after this run |
|---|---|---|
| 1 | `ReconcileAll()` fan-out | **Confirmed** at six nodes, both runs |
| 2 | `generateFRRConfigurations` recomputed from scratch | **Demoted** — real but not the cost; approx 20 s of handler time against under 2 core-seconds of CPU |
| 3 | Generated object explosion | Untested — 121 objects is trivial |
| 4 | `FRRNodeState.status.runningConfig` size | **Demoted** — 9.7x merge compaction, 650x headroom at this shape |
| 5 | `BGPSessionState` object count | Anomaly is real but probably a harness artifact; see [Open anomaly](#open-anomaly) |
| 6 | frr-k8s metrics exporter scrape cost | Untested |
| 7 | `routeimport syncNetwork` | Not a factor at 126 routes (16 ms p99) |
| 8 | EVPN per-pod datapath | Out of scope for this lane |
| 9 | `getEgressIPsByNodesByNetworks` | Not exercised (no EgressIP) |
| 10 | `addAdvertisedNetworkIsolation` | Untested — needs strict/loose axis |
| 11 | Periodic full resyncs | Not visible at this size |
| 12 | Hardcoded `Threadiness: 1` | **Confirmed, promoted to [P2b](05-roadmap.md#phase-p2b-threadiness-ab)** — 15.8x wait/work, reproduced within 0.02 s |

---

## Next lanes

Fixes (a), (b) and (d) are in. Fix (c) shipped wrong and has been redone; the next run
verifies it.

**Run the threadiness A/B before the ladder.** It was Activity 4.3, behind all of P3. Two runs
have already produced the evidence that gates it and the blocked time is I/O, so added workers
should convert almost directly into throughput. It is now
[P2b](05-roadmap.md#phase-p2b-threadiness-ab), with a measured baseline to beat and a clean
success criterion: wait p99 approaching work p99.

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

### Extrapolation, stated as a hypothesis

Per-RA reconcile is approximately 1 s at six nodes, and the generation path is linear in nodes.
If that holds, 500 nodes gives approximately 83 s per RA; 1,000 RAs serialised through one
worker is then roughly 23 hours to converge, against 500,000 generated FRRConfigurations.

This is a crude linear model and almost certainly wrong in its constant. It is recorded because
even a far gentler slope leaves the serialisation alone sufficient to put the 1,000-CUDN target
out of reach without either threadiness or the `ReconcileAll()` narrowing. The
`bgp-ra-density-wide` lane exists to replace this arithmetic with a measured slope.
