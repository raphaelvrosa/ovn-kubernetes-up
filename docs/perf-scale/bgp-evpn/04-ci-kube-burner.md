# 04 — CI and kube-burner

How to run these benchmarks continuously, what stock kube-burner already covers, what genuinely
needs upstream work, and where kind stops being useful.

## Contents

1. [What already exists](#what-already-exists)
2. [Works today with stock kube-burner](#works-today-with-stock-kube-burner)
3. [In-repo work required](#in-repo-work-required)
4. [Workload skeletons](#workload-skeletons)
5. [Upstream asks](#upstream-asks)
6. [The fabric-side harness gap](#the-fabric-side-harness-gap)
7. [The kind ceiling](#the-kind-ceiling)

---

## What already exists

**This is an extension, not a build.** `.github/workflows/performance-test.yml` already runs
nightly and per-PR on a 32-core / 128 GB runner with a 240-minute timeout, and already does
everything structurally hard:

| Capability | Where |
|---|---|
| kind cluster with 3 workers plus 2 infra nodes | `KIND_NUM_WORKER`, `KIND_NUM_INFRA: "2"` |
| Prometheus stack via helm | `KIND_INSTALL_PROMETHEUS: "true"`, `contrib/prometheus-values.yaml` |
| kube-burner **v2.5.0**, pinned | workflow; SHA256-pinned in `performance-report.yml` |
| pprof capture at 1-minute intervals | `global.measurements[].pprof` in each workload |
| Elasticsearch indexing | `ES_SERVER` secret, `contrib/perf/metric-endpoint.yml` |
| Baseline lookup and regression comparison | `get-baseline-run.py`, `compare-reports.py` |
| Automated PR comments | `post-pr-comment.py`, `generate_perf_report.py` |
| Seven density workloads | `contrib/perf/workloads/*.yml` |

And critically, the BGP knobs **are already plumbed** into the same workflow:

{% raw %}
```yaml
ENABLE_ROUTE_ADVERTISEMENTS: "${{ matrix.routeadvertisements != '' }}"
ADVERTISE_DEFAULT_NETWORK:  "${{ matrix.routeadvertisements == 'advertise-default' }}"
ADVERTISED_UDN_ISOLATION_MODE: "${{ matrix.advertised-udn-isolation-mode }}"
```
{% endraw %}

No matrix entry sets any of them. The `OVN_MULTICAST_ENABLE` and `OVN_EMPTY_LB_EVENTS`
expressions already test for `matrix.target == 'bgp'`, so somebody anticipated this. The gateway
mode is already `local`, which is what VRF-Lite and EVPN require, and the lane already runs
`modprobe vrf`.

**The missing pieces are workloads, templates, a metrics profile and matrix entries** — not
infrastructure.

---

## Works today with stock kube-burner

No upstream change needed for any of the following. Verified against the v2.5.0 documentation
and the existing in-repo usage.

| Need | Stock mechanism |
|---|---|
| Create RAs, CUDNs, VTEPs, FRRConfigurations at scale | `objects[].objectTemplate` — these are all plain CRs, no different from the existing CUDN templates |
| Wait until an RA is accepted | `waitOptions.customStatusPaths` with a jq path: `'(.conditions.[] \| select(.type == "Accepted")).status'` expecting `"True"`. Same shape for `VTEP` and for CUDN `NetworkCreated` |
| Churn protocol | `churnConfig` with `percent`, `cycles`, `duration`, `mode: objects` |
| CPU and heap profiles | `pprof` measurement — copy the existing block verbatim, it already targets `:9410` and `:9411` |
| Pass/fail gate | `--alert-profile` with `expr` / `description` / `severity`; `severity: error` sets exit code 1 |
| Split indexing | `metricsEndpoints` already does timeseries-local / quantiles-to-ES in `metric-endpoint*.yml` |
| Out-of-band measurements from [02 table 3](02-metrics.md#table-3-measured-outside-prometheus) | `beforeJobExecution` / `afterJobExecution` / `beforeCleanup` hooks running a shell script that dumps `FRRNodeState` sizes, generated-object counts and `vtysh` output into the artifact directory |

The hooks row matters more than it looks. It is the **lazy path to the out-of-band numbers**:
a 20-line shell script gets `FRRNodeState.status.runningConfig` sizes and peer route tables into
the report without writing any Go. Do that before considering a custom measurement.

---

## In-repo work required

Ordered by dependency. None of it needs upstream anything.

### 1. Make ovnkube scrapable

**Problem.** `KIND_PROMETHEUS_INFRA_ONLY: "true"` and a `contrib/prometheus-values.yaml`
containing nothing but node selectors. There is no `ServiceMonitor` or `PodMonitor` for
ovnkube, OVN, OVS or frr-k8s. Consequently `contrib/perf/metrics.yml` contains **not one**
`ovnkube_`, `ovn_`, `ovs_` or `frrk8s_` query — it could not, because there is nothing to query.

**Impact.** The entirety of [02 table 1](02-metrics.md#table-1-exists-and-usable-today) is
invisible in the current perf lane. Everything else in this roadmap is blocked on this.

**Deliverable.** ServiceMonitor or PodMonitor definitions for `ovnkube-node` (`:9410`),
`ovnkube-control-plane` (`:9411`), the OVN DB and northd exporters, and the frr-k8s metrics
service, plus whatever relabeling the kind deployment needs.

### 2. Turn on the flags

`--metrics-enable-scale-metrics` and `--metrics-enable-config-duration` in the perf lane's
ovnkube deployment. See [02 required flags](02-metrics.md#required-flags). Without the first
one there are no workqueue metrics, which is most of what [03](03-bottlenecks.md) needs.

Consider lowering `--collection-interval` below its 30 s default for short runs.

### 3. `contrib/perf/metrics-ovnk.yml`

The ovnkube/OVN/OVS/frr-k8s PromQL profile, from
[02 useful PromQL](02-metrics.md#useful-promql). Wire it as a second entry rather than editing
the existing profile, so the platform metrics stay comparable against historical baselines:

```yaml
- endpoint: http://localhost:9090
  metrics:
  - metrics.yml
  - metrics-ovnk.yml
  indexer:
    type: local
    metricsDirectory: metrics
```

### 4. `contrib/perf/alerts-bgp.yml`

The CI gate. Candidate alerts, all `severity: error` so a breach fails the job:

| Condition | Rationale |
|---|---|
| Any `frrk8s_bgp_session_up == 0` at end of run | A session that never came up invalidates the run |
| `ra_accepted_latency` p99 above the [01 target](01-methodology.md#service-level-indicators) | The primary SLI |
| ovnkube-control-plane CPU above threshold with an idle cluster | The [hypothesis 1](03-bottlenecks.md#1-reconcileall-fan-out) trap |
| `rate(ovn_db_txn_try_again[5m])` non-trivial | Transaction contention |
| `scrape_duration_seconds{job=~".*frr.*"}` above half the scrape interval | [Hypothesis 6](03-bottlenecks.md#6-frr-k8s-metrics-exporter-scrape-cost) |

Start with `severity: warning` for everything on the first few runs, promote to `error` once the
baseline is known. An alert profile that fires on run one teaches people to ignore it.

### 5. Matrix entries

Four new rows in `performance-test.yml`, following the existing row shape exactly and adding a
`routeadvertisements` key so the already-plumbed env vars activate:

```yaml
- {"target": "bgp", perf-test: "bgp-ra-density", "routeadvertisements": "advertise-all",
   "gateway-mode": "local", "ipfamily": "ipv4", "disable-snat-multiple-gws": "noSnatGW",
   "second-bridge": "1br", "ic": "ic-single-node-zones", "num-workers": "3",
   "network-segmentation": "enable-network-segmentation"}
```

plus `bgp-ra-churn`, `evpn-density` and `evpn-pod-density`. `ENABLE_EVPN` needs adding to the
env block alongside the existing three; `ADVERTISED_UDN_ISOLATION_MODE` is already there and
becomes the strict-vs-loose axis for free.

Run these **nightly only**, not per-PR. Four extra lanes on a 240-minute timeout is not a
per-PR cost anyone will thank you for.

---

## Workload skeletons

Five kube-burner configurations under `contrib/perf/workloads/`, with templates under
`workloads/templates/bgp/`, mapping 1:1 to the
[workload matrix](01-methodology.md#workload-matrix).

!!! warning "Not yet executed"
    These skeletons are structurally correct and config-parse clean, but **have not been run
    against a cluster**. First-run validation is the [P2 exit criterion](05-roadmap.md), not
    something claimed here.

| File | Purpose |
|---|---|
| `bgp-ra-density.yml` | N CUDNs + N RAs, wait on `Accepted`, then pods |
| `bgp-ra-churn.yml` | Same objects with `churnConfig` |
| `bgp-route-import.yml` | Routes injected fabric-side; measures the node agent |
| `evpn-density.yml` | VTEP + EVPN CUDNs + RA with `targetVRF: auto` |
| `evpn-pod-density.yml` | Few EVPN networks, many pods — the Type-2 scaling case |

Templates in `workloads/templates/bgp/`: `ra-per-cudn.yml`, `ra-shared.yml`,
`cudn_l3_advertised.yml`, `cudn_l2_advertised.yml`, `cudn_evpn_l2.yml`, `vtep.yml`,
`frrconfiguration-receiver.yml`.

**Reuse rather than fork.** `templates/udn-density/deployment-{client,server}.yml`,
`service.yml` and `np-*.yml` are already generic and are referenced directly. The base
FRRConfiguration shape comes from
`test/e2e/testdata/routeadvertisements/frr-k8s/frrconf.yaml.tmpl`. VNI/VID/subnet arithmetic
mirrors `test/e2e/allocators/bgp.go` (`AllocateBGP`, `BGPAllocation`, `vidMin = 2`,
`vidMax = 4094`) rather than inventing a second allocation scheme — two schemes that disagree
about VNI ranges is a debugging afternoon nobody needs.

Two template constraints worth restating because they bite immediately:

- CUDN names must be **under 16 characters**, so `c{% raw %}{{ .Replica }}{% endraw %}`, not
  `bgp-ra-density-network-{% raw %}{{ .Replica }}{% endraw %}`.
- `PodNetwork` advertisement requires `nodeSelector: {}` — the CRD's CEL rule **rejects** a
  populated one.

---

## Upstream asks

Four, in priority order. Only the second is unambiguously an upstream feature request; the
first should be built out-of-tree first and upstreamed only once it has proven itself.

### 1. A BGP convergence measurement

**What.** Measure RA or CUDN creation through to the prefix being observable in a BGP peer's
table — the `route_advertised_latency` SLI from
[the glossary](glossary.md#sli-naming-convention). Nothing in-cluster observes the peer, so
this cannot be done with a metrics query.

**Precedent.** kube-burner's existing `netpolLatency` measurement already does the structurally
identical thing for NetworkPolicy: it measures actual SDN *enforcement* time by probing from
client pods rather than reading a status field. A `bgpRouteLatency` measurement is the same
pattern with a different observation point.

**Build it out-of-tree first.** kube-burner supports external measurements through the
`measurements` package with `NewMeasurementFactory` and `RunWithAdditionalVars`, so the first
version is a small `kube-burner-ovnk` wrapper binary that registers the measurement and calls
into kube-burner as a library. That avoids an upstream review cycle before the design is
settled. Upstream it when it stops changing. Note that any wrapper must track the **v2.5.0**
pin in `performance-test.yml` and the SHA256 pin in `performance-report.yml`.

**Even lazier first step.** A `beforeJobExecution` / `afterJobExecution` hook pair that
timestamps and diffs `vtysh -c "show bgp vrf all json"` on the peer container gives a coarse
convergence number with no Go at all. Do this in P2 and only build the measurement in P5 if the
coarse number is not good enough.

### 2. Generic CR-condition latency measurement

**What.** Today `customStatusPaths` can *wait* for a condition, but the wait is not turned into
a reported latency distribution. Every CRD that wants `podLatency`-style percentiles needs
bespoke Go.

**Ask.** A config-driven `crLatency` measurement taking a GVK plus a list of condition types,
reporting creation-to-condition percentiles the same way `podLatency` reports
creation-to-`Ready`. This single feature covers RA `Accepted`, `VTEP` conditions and CUDN
`NetworkCreated`, and is useful to every operator-heavy project using kube-burner,
not just this one. **This is the ask most likely to be accepted upstream** because it is
generic.

### 3. Object-size / etcd-footprint reporting

**What.** A measurement or hook-level helper that reports the serialised size distribution of
objects a job creates or watches.

**Why.** [Hypothesis 4](03-bottlenecks.md#4-frrnodestatestatusrunningconfig-size) is an etcd
object-size ceiling, and it is currently only reachable by a `jq` script. Object size is a
general Kubernetes scale concern, so this is plausibly upstreamable. Low priority: the `jq`
script works.

### 4. Finer histogram control

**What.** Configurable bucket boundaries in the latency measurements.

**Why.** Secondary to the same problem on the ovn-kubernetes side (the six-bucket workqueue
histograms in [02](02-metrics.md#known-instrumentation-defects)), but worth raising once the
others land. Lowest priority.

---

## The fabric-side harness gap

**Problem.** `contrib/kind-common.sh` `deploy_frr_external_container()` brings up **exactly one**
FRR container (`FRR_CONTAINER_NAME=frr`), configured as an EVPN route reflector for the whole
cluster, plus one `bgpserver` agnhost container. That is correct and sufficient for functional
e2e. It makes the **"BGP peers per node" ladder dimension unreachable**: there is one peer, and
no amount of workload YAML changes that.

**Two ways out, and the second is much cheaper.**

| Option | Work | Reaches |
|---|---|---|
| Parameterise `deploy_frr_external_container()` for N containers and extend `configure_frr_uplink_peers()` | Non-trivial shell work plus per-container FRR config generation | 1 → 8 external peers, realistic fabric shape |
| Use `[bgp-managed] topology=full-mesh` with `transport=no-overlay` | **No external fabric at all** — `managedbgp` generates the peering | 2(N-1) peers per node, at whatever N the cluster has |

The mesh model is a genuinely different product configuration rather than a simulation of the
fabric one, so it does not substitute for fabric testing. But it reaches high peer counts for
free, and it is the model that exercises the `FRRNodeState` size and BGP session count ceilings
hardest. **Do mesh first.** Build the multi-container fabric only if mesh results suggest the
fabric shape behaves differently.

Route injection for `bgp-route-import` is a third harness need: the fabric side must announce
1k to 500k synthetic prefixes. An FRR container with a generated static-route block plus
`redistribute static` is the simplest source; `bgpserver`-style tooling or a route generator
in the existing container is the alternative.

---

## The kind ceiling

**Kind cannot reach 500 nodes.** The existing lane runs 3 workers on a 32-core box; even
aggressive tuning does not get past roughly 10. Pretending otherwise produces numbers that
mislead. Two tiers, labelled differently, used for different purposes:

| Tier | Where | Nodes | Purpose | Result validity |
|---|---|---|---|---|
| **Regression** | kind, in CI, nightly | 3-10 | Detect that a PR made something measurably worse | **Relative only.** Comparable against the previous nightly, never quotable as a capability |
| **Ceiling** | Real cluster, on demand | 120 / 250 / 500 | Find where things actually break | Absolute. These are the publishable numbers |

The workload YAMLs are **identical across both tiers** — only the ladder values in `jobIterations`
and `replicas` differ. That is the whole point of putting them in `contrib/perf/` rather than in
a scale-lab-only repository, and it means the CI lane continuously validates the same
configuration the ceiling runs use.

### kwok as a middle tier

kwok-style node emulation fakes kubelet, so pods reach `Running` without a container runtime and
node counts in the hundreds are cheap. It is worth evaluating, with a clear limit:

| Half of the system | Does kwok work? |
|---|---|
| Cluster manager — RA reconcile, generated FRRConfiguration fan-out, NAD annotation, apiserver/etcd pressure, `FRRNodeState` and `BGPSessionState` object counts | **Yes.** These are pure Kubernetes-object workloads. Bottlenecks 1-5 all live here |
| Node agent — `routeimport` netlink, EVPN FDB and neighbour programming, OVS port scans, OVN flow installation | **No.** No real netlink, no OVS, no FRR |

Five of the twelve hypotheses — including all three of the highest-ranked ones — are reachable
with kwok at 500 emulated nodes on a single machine. That is a genuinely useful middle tier and
it should be evaluated in [P3](05-roadmap.md) before booking real hardware, not dismissed because
it cannot do everything.

The node-agent half is measurable per-node: hypotheses 7, 8 and 11 are all "what does one node
do with N networks and M pods", which does not require 500 real nodes to observe. Combine kwok
for the control-plane half with a small real cluster carrying a high per-node load for the other
half, and the number of things that genuinely need a 500-node lab shrinks to the fabric-scale
and convergence questions.
