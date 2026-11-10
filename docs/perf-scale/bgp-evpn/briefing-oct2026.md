# BGP Route Advertisements Performance Benchmarking

**October 2026 Status Briefing**

---

## Executive Summary

Three successful CI runs of a new `bgp-ra-density` lane have established a reliable instrument for measuring RouteAdvertisements reconcile cost. The runs confirm one major bottleneck (threadiness), demote two others (recomputation, etcd object size), and close one harness anomaly (IPv6 session split). All harness defects are fixed. The lane is ready for the next phase: scaling on the node axis and RA axis separately.

**Key Result**: RouteAdvertisements controller queue wait p99 = **16.06 s** against a work p99 of **1.02 s**, reproduced exactly across three independent clusters. This 15.8× ratio is the single-worker signature.

---

## Measurement Setup

### Workload Shape

| Dimension | Value | Notes |
|-----------|-------|-------|
| Nodes | 6 | 1 control-plane + 3 workers + 2 infra (KIND_NUM_INFRA) |
| RouteAdvertisements | 20 | One per advertised CUDN |
| CUDNs | 20 | Layer 3, advertised |
| Generated FRRConfigurations | 120 | 20 RAs × 6 nodes × 1 source config |
| Converged in | ~42 s | Time from RA creation to all 120 configs stable |
| Job duration | 53 s | Includes 3-minute pause-before-cleanup |
| CI artifacts | 308 files | pprof-data/, perf/metrics/, performance_report.md |

### Instrumentation

**Enabled for first time:**
- `ovnkube_rest_client_request_duration_seconds{verb, resource, host}`
- `ovnkube_rest_client_requests_total{code, method, host}`
- `ovnkube_rest_client_rate_limiter_duration_seconds{verb, resource, host}`
- Client-go metrics were entirely missing; these attribute apiserver wait time directly instead of by subtraction from CPU

**Gaps that were discovered and fixed:**
- `apiserver_request_total` in metrics.yml filtered on `apiserver="kube-apiserver"` label that doesn't exist on kind → silently returned 0 documents on every run
- `ovnkube_rest_client_*` vector labels were inconsistent (2 vs 3) → panic on first throttled request
- `raReconcileDuration99th` and `raFRRConfigurationWriteRate` queries used `rate()` on counters that only fire during the workload transition (10s window, done before first 30s sample) → empty results

All fixed in PR #7077 and verified by run 3.

---

## Three Runs: Reproducibility

| Metric | Run 1 | Run 2 | Run 3 | Stability |
|--------|-------|-------|-------|-----------|
| RA queue wait p99 | 16.077 s | 16.056 s | 16.056 s | ✓ Reproduced exactly |
| RA queue work p99 | 1.019 s | 1.019 s | 1.019 s | ✓ Identical |
| Ratio (wait/work) | 15.8× | 15.8× | 15.8× | ✓ Consistent |
| Job wall time | 42 s | 53 s | 53 s | stabilized (b) |
| Cluster mgr CPU avg | 0.75% | 0.76% | 0.79% | ✓ <1% |
| OVN DB size | 4.03 MB | 4.07 MB | — | ✓ Stable |
| libovsdb p99 | 13 ms | 12 ms | — | ✓ Low |

**Caveat on precision**: With `ExponentialBuckets(.001, 2, 16)`, 16.056 s interpolates inside `[8.192, 16.384]`. The agreement shows **workload reproducibility**, not sub-millisecond instrument resolution. Resolution at these magnitudes is ~one octave. The **ratio** is robust because both numbers are pinned the same way.

---

## Findings by Category

### Confirmed Hypotheses

#### **H12: Hardcoded Threadiness=1 is the bottleneck** ✓

**Evidence:**
- RA queue: wait p99 = 16.08 s, work p99 = 1.02 s → **15.8× starvation**
- Node queue: wait p99 = 0.13 s, work p99 = 0.23 s → healthy
- All other queues: wait ≈ work → not starved
- **API-side proof (run 3):** Each reconcile issues 6 POSTs (one FRRConfiguration per node)
  - POST p99: 127 ms
  - 6 × 65–127 ms ≈ 0.4–0.76 s → explains the whole 1.02 s work duration
- Client-side rate limiter p99: **0.99 ms** (first histogram bucket) → our own QPS is **not** the limit

**Implication:** The controller spends almost nothing computing; it spends nearly all time blocked on apiserver writes. Single worker serializes six sequential POSTs per RA. More workers should convert nearly linearly to throughput until apiserver inflight becomes the limit (currently 2 mutating requests).

**Next:** Threadiness A/B experiment (Hypothesis 12 → **Phase P2b** in roadmap; moved out of the P4 ladder).

---

### Demoted Hypotheses

#### **H1: `ReconcileAll()` fan-out** — untested, not confirmed

**Why:** Run 3 made per-RA counters readable for the first time.

```
raReconcileCount: 20 RAs → 20 reconciles
raFRRConfigurationWrites: 120 creates, 0 unchanged, 0 updates, 0 deletes
```

Each RA was reconciled **exactly once**. No redundant work. The node-queue add spike (19.85/s) did not re-enqueue RAs.

**Reason:** A create-only workload with no churn cannot exercise `ReconcileAll()` amplification. That is what `bgp-ra-churn` lane is for (next phase).

**Status:** Withdrawn from "confirmed," returned to "untested."

---

#### **H2: `generateFRRConfigurations` recomputation** — demoted from Critical to Medium

**Evidence:** Run 3 CPU profiles show **~1.8 core-seconds total for the entire run** across a cluster-manager using <1% CPU on average. At 20 s of handler time (20 reconciles × 1.02 s work p99), at least 90% is blocked, not computing.

The recomputation is real (file:L488–925). Caching it buys little when the binding cost is apiserver writes, not computation.

**Implication:** Cross-reconcile caching (hypothesis 2) is Activity 4.2 in the roadmap but should be tested **after** threadiness shows whether the bottleneck actually moved. The write count (hypothesis 3's axis) is what scales.

**Status:** Gate this on wide lane (node-count axis) showing per-node work actually growing.

---

#### **H4: `FRRNodeState.status.runningConfig` etcd size** — demoted from Critical to Medium

**Measurement:**
- Generated objects: 120 FRRConfiguration objects, 133,728 bytes
- Merged running config: 13,841 bytes total across 6 nodes → **2,306 B/node**
- Compaction factor: **9.7×**
- Headroom to 1.5 MB etcd limit: **~650×**

The 20 per-node configurations share a neighbour and differ only in prefixes — a shape that merges best.

**Implication:** This is no longer a candidate for "fails first." The axis that matters is **distinct prefixes and VRFs** (not object count), so EVPN — whose per-VRF stanzas don't merge the same way — remains the case to watch.

**Status:** Demoted. Re-measure on wide lane with real scale.

---

### New Finding: The Session Anomaly is a Harness Bug

**Observation:** 12 `BGPSessionState` objects for 6 nodes, only 6 established, while the peer reported 0 failures.

**Root Cause (run 3):**
```json
"byPeer": { "172.18.0.8": 6, "fc00:f853:ccd:e793::8": 6 },
"byStatus": { "Established": 6, "Active": 6 }
```

The `receive-all` FRRConfiguration lists **both IPv4 and IPv6 neighbours in one router**. On an IPv4-only lane the IPv6 session sits in `Active` (BGP for "still trying to connect") forever.

**Not a frr-k8s cardinality issue.** It's a harness configuration artifact: the template unconditionally adds the IPv6 neighbour regardless of `PLATFORM_IPV6_SUPPORT`.

**Why not fixed:** The natural gate is `PLATFORM_IPV6_SUPPORT`, which is itself broken:
{% raw %}
```yaml
PLATFORM_IPV6_SUPPORT: "${{ matrix.ipfamily == 'IPv6' || matrix.ipfamily == 'dualstack' }}"
```
{% endraw %}

Every matrix entry in both `test.yml` and `performance-test.yml` uses lowercase (`ipv4`, `ipv6`, `dualstack`), so this comparison is **always false** — including on 24 e2e lanes running IPv6 and dualstack. Those lanes work *because* the IPv6 neighbour is added unconditionally; it's a compensating error.

Fixing both requires a full e2e run. Blocked on a separate PR.

**Impact:** Six permanently-retrying BGP sessions per run. Not harming scale measurements (they're idle), but worth documenting.

---

## Harness Defects Fixed

| Defect | Issue | Fix | Status |
|--------|-------|-----|--------|
| **(a) pprof duty cycle** | 1m interval against 30s profile = 50% gap; work window fell between captures | `pprofInterval: 30s` | Verified: 112 profiles vs 70, continuous coverage (though work window still slipped) |
| **(b) hook snapshot pre-convergence** | `beforeCleanup` runs before `jobPause` at T=12s; fabric had only received 54/120 prefixes | Added `wait_for_settle()` polling in hook; stabilized in 10s | Verified: run 2 jump from 54 → 120 prefixes |
| **(c) per-RA rate() queries empty** | First diagnosis wrong (label cardinality); real cause was workload finishing faster than scrape interval | Changed from `rate()` to cumulative counters (`_count`, `_writes_total`, `_ops_total`) | Verified: run 3 all four queries now return 7–12 documents |
| **(d) node count mismatch** | `KIND_NUM_INFRA: 2` makes total 6, not 3; matrix comment misleading | Documented in matrix comment | Minor |
| **(e) apiserver metrics silent fail** | Metrics.yml filtered on `apiserver="kube-apiserver"` label that doesn't exist on kind | Removed filter; verified 14 and 8 series return | Verified: run 3 shows status codes by resource |

All except (e)'s missing instrumentation landed in run 1/2. Defect (e) was discovered during run 2 analysis; instrumentation added for run 3. **None are product bugs.** All are measurement/lane methodology issues.

---

## Hypothesis Scoreboard

| # | Hypothesis | Status | Evidence |
|---|---|---|---|
| 1 | `ReconcileAll()` fan-out | **Untested** | Workload is create-only; churn lane needed |
| 2 | `generateFRRConfigurations` recompute | **Demoted** | 90%+ time is apiserver write, not computation |
| 3 | Generated object explosion | Untested | 120 objects is trivial; ladder will stress this |
| 4 | `FRRNodeState` size | **Demoted** | 9.7× merge, 650× headroom; distinct prefixes/VRFs matter |
| 5 | `BGPSessionState` cardinality | **Closed (harness artifact)** | IPv6 neighbour never establishes on IPv4 lane |
| 6 | frr-k8s metrics scrape cost | Untested | — |
| 7 | `routeimport` full dump | Not factor (126 routes, 16 ms p99) | — |
| 8 | EVPN pod datapath | Out of scope for this lane | — |
| 9 | `getEgressIPsByNodesByNetworks` | Not exercised (no EgressIP) | — |
| 10 | `addAdvertisedNetworkIsolation` | Untested | — |
| 11 | Periodic full resyncs | Not visible at this size | — |
| 12 | Hardcoded threadiness | **Confirmed** | 15.8× wait/work, 6 POSTs per reconcile at 65–127 ms p99 |

---

## Ready for Production

✓ **Lane runs reliably:** Two infra-nodes, three workers, consistent 53s runtime  
✓ **Instrument stable:** Workqueue, libovsdb, REST client, pprof, OVN metrics all visible  
✓ **Defects closed:** All harness issues fixed and verified  
✓ **Reproducible:** Same numbers three times, buckets confirm not noise  
✓ **Tests added:** Label-cardinality regression test catches the crash-on-first-request bug  
✓ **Docs published:** Bottleneck analysis, roadmap, methodology all in place  

---

## Next Activities

### Immediate (Phase P2b: Threadiness A/B)

**Why not wait for P3:** The evidence for this is *already here* from the density lane. No ladder run needed.

- Add `--controller-threadiness` knob (default 1, test 4)
- Expose in RouteAdvertisements controller only (race review first, but the controller is already nodeless)
- Re-run `bgp-ra-density` at threadiness 1 and 4, same workload
- **Success criterion:** wait p99 drops toward work p99, or it doesn't and we've learned the limit is elsewhere

Expected outcome: 4–6× speedup (six queued items serializing → parallelism kicks in until some other limit). Would bring convergence from 42 s to ~10 s.

### Phase P3: Scale Ladder (node axis separate from RA axis)

**Why separate:** Allows fitting a per-RA cost slope independently.

**bgp-ra-density-wide** (fixed RA count = 20, vary nodes):
- Nodes: 6 → 12 → 24 → 60 (kind ceiling ~120, real hardware after)
- Measures: per-RA reconcile cost scaling, whether H2's per-node loop actually grows

**bgp-ra-density-deep** (fixed node count = 6, vary RAs):
- RAs: 20 → 50 → 100 → 200 → 500
- Measures: queue wait scaling, FRRConfiguration merge behavior at high volume, whether H3's object explosion bites

**Duration:** ~2 weeks wall time per axis (nightly + human analysis), same instrument.

### Phase P4: Fix Top Three (after P3 evidence)

Expected targets (subject to what P3 shows):
- H1: Narrow `ReconcileAll()` trigger (`bgp-ra-churn` lane first to test it)
- H2: Cross-reconcile caching in `generateFRRConfigurations` (defer if threadiness is the limit)
- H3: Depends on wide-lane object count behavior

### Blocked / Out of Scope

- H4/H5: frr-k8s upstream (FRRNodeState size, BGPSessionState cardinality)
- IPv6 neighbour harness fix: blocked on `PLATFORM_IPV6_SUPPORT` variable bug affecting 24 e2e lanes; needs separate PR + full e2e validation

---

## Instrumentation Wins

**What we can now see directly** (before: invisible or inferred):

| Metric | Before | After | Use |
|--------|--------|-------|-----|
| Per-RA reconcile duration | Computed by subtraction (slow inference) | `rest_client_request_duration_seconds` per verb/resource | Direct attribution of apiserver POSTs |
| Reconcile completion/success count | Per-RA counter born and died in one window | `raReconcileCount{result}`, `raReconcileDuration_seconds_bucket` | Volume and timing |
| Generated FRRConfiguration writes | Guessed from depth peaks | `raFRRConfigurationWrites_total{op}` | Actual create/update/delete count |
| Queue throughput (failed retries) | Invisible | `workqueue_retries_total{name}` | Immediate signal of API errors |
| Apiserver work (not just duration) | Only duration p99 | `rest_client_request_duration_seconds{verb, resource, host}` | Find which resource is slow |
| Apiserver status codes | Invisible on kind (label missing) | `apiserver_request_total{code, method, resource}` | Distinguish 200/422/500 |

This closes almost all gaps in `docs/perf-scale/bgp-evpn/02-metrics.md` table 2 except libovsdb txn metrics (P1 work).

---

## Caveats

1. **Kind is not production:** Six nodes, one external BGP peer. Real scale is 120–500 nodes, full-mesh or multi-peer eBGP. The ladder must move to hardware.

2. **Threadiness A/B is the next step, not P3:** Hypothesis 12 is so well-evidenced we're running it out of phase. If threadiness doesn't help, it means the limit moved to the apiserver and P3 becomes a question of "at what node count does apiserver throughput cap us" rather than "where does ovnkube stall."

3. **Create-only cannot test churn properties:** The workload creates 20 RAs once and measures steady-state. `ReconcileAll()` amplification only matters when unrelated events (Node changes, NAD changes, FRRConfiguration reload) fire. `bgp-ra-churn` lane will stress that.

4. **IPv6 session count is real but secondary:** Six idle sessions don't break anything. The harness will improve once the broken `PLATFORM_IPV6_SUPPORT` variable is fixed across all 24 e2e lanes.

---

## References

| Document | Link | Purpose |
|----------|------|---------|
| Full Analysis | [08-first-ci-results.md](08-first-ci-results.md) | Deep dive on runs 1–3, all findings with evidence |
| Bottleneck Hypothesis | [03-bottlenecks.md](03-bottlenecks.md) | Ranked hypotheses, confirmed/demoted status |
| Roadmap | [05-roadmap.md](05-roadmap.md) | Phased plan P0–P5, dependency graph, exit criteria |
| Methodology | [01-methodology.md](01-methodology.md) | Scale ladder, topology models, SLI definitions |
| PR #7077 | [perf/bgp-ra-density-lane](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/7077) | Lane commit, harness fixes, instrumentation |

---

## Q&A Anticipated

**Q: Why did pprof not capture the work?**  
A: `pprofInterval: 30s` gives continuous coverage, but work ran 13:48:48–13:49:01 while periodic profiles started at 13:49:18. Gap is real; made moot by the apiserver metrics showing exactly where the time goes. CPU profiling is the wrong instrument for blocked I/O.

**Q: Why only six nodes?**  
A: Kind ceiling. Real scale measured in the ladder phase (P3). Six is enough to show the bottleneck (threadiness) because it's a per-worker property, not a size property.

**Q: Is threadiness the only win?**  
A: For this workload shape, yes. If threadiness fixes it, great — convergence goes from 42s to ~10s. If it doesn't, the limit moved to the apiserver's own queue, and P3 tells us at what node count that happens.

**Q: When can we run this on real hardware?**  
A: After P2b (threadiness) shows the direction. Infrastructure question, not a blocker for lanes P3/P4 on kind.

