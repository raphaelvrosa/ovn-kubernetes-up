# BGP Route Advertisements Performance Benchmarking

**November 2026 Status Briefing**

---

## Executive Summary

The scale ladder ran for the first time: four lanes in one CI run, each adjacent pair isolating one variable. Three results matter.

**Hypothesis 1 is confirmed, with a number.** Changing 4 of 40 RouteAdvertisements causes 25 of them to reconcile. Over the churn phase, 20 RouteAdvertisements were touched and the controller performed 127 reconciles producing 1,524 configuration writes, **none of which changed anything**. That is the `ReconcileAll()` fan-out, measured rather than inferred. The October briefing withdrew a premature confirmation of this hypothesis; this run earns it properly.

**Reconcile cost is exactly linear in node count.** Doubling nodes at fixed RA count doubled every cost term to within 2%. The model "one reconcile = one apiserver write per node" now has two clean points.

**The instrument hit its ceiling.** The two 40-RA lanes pin the queue-wait histogram at exactly 32.768 s, the last finite bucket. Those numbers are floors, not measurements. The ladder cannot go further up without wider buckets.

---

## Measurement Setup

One CI run, [42220334668](https://github.com/ovn-kubernetes/ovn-kubernetes/actions/runs/42220334668), commit `694a014d3`. All four lanes **succeeded**.

| Lane | Nodes | RAs | Generated objects | Isolates |
|---|---|---|---|---|
| `bgp-ra-density` | 6 | 20 | 120 | baseline |
| `bgp-ra-density-wide` | 12 | 20 | 240 | node count, vs density |
| `bgp-ra-density-deep` | 12 | 40 | 480 | RA count, vs wide |
| `bgp-ra-churn` | 12 | 40 | 480 | churn, vs deep |

The fan-out identity `RAs x nodes` held exactly in all four, and every RouteAdvertisements reached `Accepted` in every lane — including after five churn cycles, with no leaked or missing generated objects.

**Query coverage is complete.** 26 of 27 queries returned data in every lane. The one exception, `ovnNorthdTxnTryAgainRate`, is genuinely zero: northd had no transaction retries. `ovnkubeWorkqueueDepth`, which was structurally empty at 6 nodes because the backlog drained faster than the 30 s scrape, now returns data — the backlog outlasts a scrape at this size, exactly as [08](08-first-ci-results.md#harness-defects-exposed) predicted it would.

---

## Result 1: node count is exactly linear

Holding RAs at 20 and doubling nodes from 6 to 12:

| Quantity | 6 nodes | 12 nodes | Ratio |
|---|---|---|---|
| Queue wait p99 | 16.302 s | 32.178 s | **1.97x** |
| Queue work p99 | 1.019 s | 2.038 s | **2.00x** |
| RA reconcile p99 | 0.637 s | 1.274 s | **2.00x** |
| Generated objects | 120 | 240 | 2.00x |
| libovsdb txn rate | 91.8/s | 218.1/s | 2.38x |

Per-reconcile cost is **0.106 s per node** at both points. This is the clean confirmation of the model built in October from the client-go metrics: a reconcile is one apiserver write per node, serialised, and nothing else of consequence.

All four numbers above are interpolated *inside* a bucket rather than pinned to a boundary, so this comparison is sound. That is not true further up the ladder.

**Extrapolation.** At 0.106 s per node, a single reconcile costs ~53 s at 500 nodes. With `Threadiness: 1`, 100 RouteAdvertisements converge in ~88 minutes and 1000 in ~14.7 hours. Those figures assume the linearity continues, which is exactly what the ladder exists to keep testing.

---

## Result 2: hypothesis 1 confirmed — the fan-out is real and wasteful

The churn lane holds 40 RouteAdvertisements steady and deletes and recreates 4 of them, five times over. Isolating the churn phase from the creation phase:

| | At creation complete | At run end | Churn-attributable |
|---|---|---|---|
| Reconciles | 76 | 203 | **+127** |
| `unchanged` writes | 432 | 1,956 | **+1,524** |
| `create` writes | 480 | 480 | **0** |

Three things follow, and the arithmetic is exact rather than approximate:

**Every churn reconcile was pure waste.** 1,524 / 127 = **12.0 writes per reconcile**, which is precisely the node count, and zero of them were creates. Each of those 127 reconciles regenerated the full configuration for its RouteAdvertisements, compared it, found it identical, and wrote nothing.

**The amplification factor is 6.35.** 20 RouteAdvertisements were touched by churn and 127 reconciles resulted. Per cycle, changing 4 of 40 RAs — a 10% change — caused **25.4 reconciles, 64% of the population**.

**Across the whole run, 163 of 203 reconciles (80%) produced no configuration change.**

This is what [03 hypothesis 1](03-bottlenecks.md#1-reconcileall-fan-out) predicted: any NAD or FRRConfiguration event re-enqueues every RouteAdvertisements, so steady-state cost is a function of unrelated churn rather than of the change itself.

!!! note "Why the density lanes could not show this"

    They create each RouteAdvertisements once and nothing else changes, so there is no
    amplification to observe: all three reported exactly one reconcile per RA and zero
    `unchanged` writes. The October briefing marked hypothesis 1 confirmed on indirect
    evidence and then [withdrew it](08-first-ci-results.md#corrections). The churn lane was
    built specifically to settle it, and it has.

**Extrapolation.** Combining with Result 1: a 10% churn event against 1000 RAs on 500 nodes would trigger ~635 reconciles at ~53 s each, serialised — about **9.3 hours** of redundant work, during which real changes queue behind it.

**One caveat that likely understates the problem.** kube-burner recreates a deleted object about one second after deleting it. The controller appears to coalesce the pair: `create` writes never increased during churn, meaning it never observed the gap. Real churn, where a delete and a create are seconds or minutes apart, would produce two separate fan-out events rather than one.

---

## Result 3: the instrument hit its ceiling

`ExponentialBuckets(.001, 2, 16)` gives a last finite bucket of 0.001 x 2^15 = **32.768 s**.

| Lane | Queue wait p99 | Samples pinned at 32.768 |
|---|---|---|
| density | 16.302 s | 0 of 4 |
| density-wide | 32.178 s | 0 of 4 |
| **density-deep** | **32.768 s** | **3 of 4** |
| **churn** | **32.768 s** | **3 of 13** |

The two 40-RA lanes are at the ceiling: their true queue wait is **at least** 32.768 s and the histogram cannot say how much more.

This has a direct consequence: **the RA axis is not measurable from queue wait.** It appears to move only from 32.178 to 32.768, but that is the instrument saturating, not the system behaving well.

!!! note "Only this one histogram saturated"

    `route_advertisements_reconcile_duration_seconds` uses a different base,
    `ExponentialBuckets(.005, 2, 14)`, so it reaches 40.96 s. Its deep value of 2.048 s is
    interpolated inside `[1.28, 2.56]`, not pinned. The same is true of workqueue *work*
    duration, 2.038 s inside `[1.024, 2.048]`. Only queue *wait* is at its ceiling, because
    it is the one term that grows with the whole queue rather than with a single item.

The buckets were widened in this cycle from 6 spanning 10 ms to 1000 s, to 16 spanning 1 ms to 32.768 s. That was the right change for 6 nodes and is already insufficient at 12 nodes with 40 RAs. The ladder needs roughly 20 buckets, reaching ~500 s, before the next rung.

---

## Result 4: two measurements to distrust

**`FRRNodeState` sizes in the deep lane are pre-convergence.** Deep settled after 10 s while wide, with half the objects, took 25 s. Its per-node config sizes are heterogeneous — three distinct clusters around 14.98 kB, 16.29 kB and 18.90 kB — while the churn lane, identical in shape but with six minutes of activity before its snapshot, is uniform at ~20.55 kB on every node. A converged cluster should be uniform, because every node carries all 40 networks.

The settle heuristic requires two consecutive stable polls 5 s apart. At this size frr-k8s reloads nodes in waves, and the total plateaus between waves. **Use the churn lane's 20.55 kB per node as the real figure for 40 RAs on 12 nodes; deep's 16.95 kB is an underestimate.**

**Per-node config size grew roughly 5x since October**, and this is unexplained. The 6-node 20-RA lane is unchanged in shape between the two runs, yet:

| | October | November |
|---|---|---|
| Generated bytes | 133,728 | 139,968 |
| Merged config per node | **2,306 B** | **11,609 B** |
| Compaction | 9.7x | 2.0x |

The generated objects are nearly the same size; the merged FRR running configuration is five times larger. Something in ovn-kubernetes or frr-k8s changed between early October and now. This needs a bisect before anything is concluded from it — but it also directly undercuts the October decision to demote hypothesis 4.

---

## Hypothesis 4 demotion: partially retracted

October demoted `FRRNodeState.status.runningConfig` size from Critical to Medium on a measured 9.7x compaction and 650x headroom to the 1.5 MB etcd limit. This run does not support that.

| Shape | Config per node | Compaction | Headroom to 1.5 MB |
|---|---|---|---|
| 6 nodes, 20 RAs (Oct) | 2,306 B | 9.7x | 650x |
| 6 nodes, 20 RAs (Nov) | 11,609 B | 2.0x | 129x |
| 12 nodes, 20 RAs | 11,454 B | 2.0x | 131x |
| 12 nodes, 40 RAs (churn) | 20,587 B | 2.3x | **73x** |

Two things are now visible that were not in October. Compaction is **2.0–2.3x, not 9.7x**. And config size tracks **RA count, not node count**: doubling nodes at fixed RAs left it flat (11,609 → 11,454 B), while doubling RAs at fixed nodes raised it 80% (11,454 → 20,587 B).

Headroom is 73x at 40 networks. If the per-network slope holds, the 1.5 MB limit arrives somewhere around 1,500–3,000 networks — short of the 1000-CUDN target only by a small factor, and well inside it once EVPN's per-VRF stanzas are added. **Hypothesis 4 should go back to High.** The axis to ladder is network count, and the October conclusion that object count is almost free still stands.

---

## Supporting measurements

| Quantity | density | wide | deep | churn |
|---|---|---|---|---|
| Cluster manager CPU, avg | 0.29% | 0.68% | 2.29% | 1.65% |
| Cluster manager CPU, max | 0.94% | 3.62% | 12.07% | 5.81% |
| Cluster manager RSS, max | 140 MB | 153 MB | 167 MB | 181 MB |
| OVN DB total | 4.03 MB | 4.62 MB | 8.84 MB | 8.87 MB |
| libovsdb txn rate, peak | 91.8/s | 218.1/s | 410.0/s | 397.6/s |
| libovsdb txn p99 | 13.3 ms | 30.2 ms | 40.5 ms | 38.4 ms |
| ovn-controller txn retries | present | present | present | present |

CPU remains under 2.3% average everywhere, consistent with the October finding that the reconcile is I/O-bound rather than CPU-bound. OVN DB size tracks RA count, not node count, the same pattern as the FRR config.

`routeImportSyncDuration99th` peaked at **4.05 s** in the deep lane, but only during the network-creation burst, falling to 0.03 s within 90 seconds. The churn lane, identical in shape, peaked at 0.51 s in the same window. This is a creation transient rather than a steady-state cost, and the difference between the two lanes is unexplained; not worth acting on yet, worth watching as the network count rises.

---

## Hypothesis Scoreboard

| # | Hypothesis | Status change | Evidence |
|---|---|---|---|
| 1 | `ReconcileAll()` fan-out | **Untested → CONFIRMED** | 10% change reconciles 64% of the population; 80% of all reconciles write nothing |
| 2 | `generateFRRConfigurations` recompute | Demoted, unchanged | CPU still under 2.3%; the 1,524 wasted writes are apiserver cost, not compute |
| 3 | Generated object explosion | Untested | 480 objects is still trivial; identity exact in all four lanes |
| 4 | `FRRNodeState` size | **Medium → High (retract demotion)** | compaction 2.0–2.3x not 9.7x; headroom 73x at 40 networks |
| 5 | `BGPSessionState` cardinality | Closed, harness artifact | reconfirmed: 24 objects, 12 established, IPv4 up and IPv6 `Active` |
| 7 | `routeimport` full dump | Untested | 4 s transient during creation only, 0.03 s steady state |
| 12 | Hardcoded `Threadiness: 1` | CONFIRMED, unchanged | wait/work ratio 16x at every rung |

---

## Next Activities

**Widen the histogram buckets before the next rung.** From 16 buckets to roughly 20, reaching ~500 s. Two of four lanes are currently reporting a floor rather than a value, and every rung above this one will be worse. This is the cheapest and most urgent item.

**Strengthen the settle heuristic.** Two stable polls 5 s apart is not enough when frr-k8s reloads in waves. Require three consecutive stable polls, or poll for per-node uniformity rather than a total. Without it, every lane above 20 RAs reports `FRRNodeState` figures that are too low.

**Bisect the 5x config-size growth** between the October and November runs on the identical 6-node 20-RA shape. Until that is explained, the hypothesis 4 numbers cannot be trusted in either direction.

**Then run the threadiness A/B** ([P2b](05-roadmap.md#phase-p2b-threadiness-ab)). It is still the best-evidenced single change, and the churn result strengthens the case: with 80% of reconciles redundant and all of them serialised, parallelism helps the waste as much as the real work. The honest sequencing is that narrowing `ReconcileAll()` — [Activity 4.1](05-roadmap.md#activity-41-narrow-reconcileall) — now has equally good evidence and a larger ceiling, because it removes the work rather than parallelising it.

**Add a churn lane at the 6-node shape.** The current churn lane confounds node count with churn. A 6-node 40-RA churn lane would let the amplification factor be separated from the per-reconcile cost.

---

## Caveats

**The RA axis is only partly measured.** Queue wait saturated, so that comparison is unusable. Reconcile duration did not, and reads 1.274 s at 20 RAs against 2.048 s at 40 on the same 12 nodes — a 1.61x rise for a term that should not depend on RA count at all, since each reconcile still writes one object per node. That hints at contention, but both values carry roughly one octave of bucket error, so the ratio could plausibly be anywhere from flat to 4x. It is a lead, not a result.

**Churn amplification is likely understated**, because delete and recreate one second apart appear to coalesce into one fan-out event rather than two.

**All of this is kind.** Twelve nodes sharing one kernel, one disk and one apiserver on a 32-core runner. The linearity in Result 1 is a property worth extrapolating; the absolute latencies are not.

**Single run per lane.** October established that this workload is reproducible across three runs at the 6-node shape, but the three new lanes have one data point each. The node-axis linearity rests on two points, which is a line by assumption rather than by fit.

---

## References

| Document | Purpose |
|---|---|
| [08 — First CI Results](08-first-ci-results.md) | Runs 1–3 at the 6-node shape, instrumentation gaps, corrections |
| [briefing-oct2026](briefing-oct2026.md) | The previous briefing, whose hypothesis 1 claim this run supersedes |
| [03 — Bottleneck Analysis](03-bottlenecks.md) | Ranked hypotheses |
| [05 — Roadmap](05-roadmap.md) | Phases, including P2b threadiness |
| [PR #7077](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/7077) | The four lanes |
| [PR #7062](https://github.com/ovn-kubernetes/ovn-kubernetes/pull/7062) | The instrumentation they depend on |
