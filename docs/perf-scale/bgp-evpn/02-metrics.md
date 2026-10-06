# 02 — Metrics and Instrumentation

What can be observed today, what cannot, and exactly where to add the missing instrumentation.

## Contents

1. [Summary of coverage](#summary-of-coverage)
2. [Table 1 — exists and usable today](#table-1-exists-and-usable-today)
3. [Table 2 — missing, with code sites](#table-2-missing-with-code-sites)
4. [Table 3 — measured outside Prometheus](#table-3-measured-outside-prometheus)
5. [Required flags](#required-flags)
6. [Useful PromQL](#useful-promql)
7. [Profiling](#profiling)
8. [Known instrumentation defects](#known-instrumentation-defects)

---

## Summary of coverage

**Problem.** The BGP and EVPN features export exactly three metrics, all of them gauges of a
status condition, and none of them timing or volume:

| Metric | Registered when |
|---|---|
| `ovnkube_clustermanager_route_advertisement_condition{name,condition,status}` | `--enable-route-advertisements` |
| `ovnkube_clustermanager_vtep_condition{name,condition,status}` | `--enable-evpn` |
| `ovnkube_clustermanager_cluster_user_defined_networks{role,topology,transport}` | always; the `transport` label distinguishes `Default`, `EVPN` and `NoOverlay` |

There is no metric for reconcile duration, generated object count, advertised prefix count,
imported route count, transaction size, or per-pod EVPN programming. The RouteAdvertisements
controller times its own reconcile and logs it at `klog.V(4)`; `routeimport` does the same at
`V(5)`. Those log lines are the cheapest existing source of timing if you need a number before
any code lands.

**Impact.** Without table 2, a scale run can show that something is slow but not which of the
twelve hypotheses in [03](03-bottlenecks.md) is responsible. Instrumentation is therefore
phase P1 of the [roadmap](05-roadmap.md), before benchmarking proper.

**Second problem.** The existing perf lane scrapes none of it. `contrib/perf/metrics.yml`
contains apiserver, etcd, cAdvisor, kubelet and CRI-O queries and **not a single `ovnkube_`,
`ovn_`, `ovs_` or `frrk8s_` query**. `KIND_PROMETHEUS_INFRA_ONLY: true` in
`.github/workflows/performance-test.yml` and a 30-line `contrib/prometheus-values.yaml` with no
`ServiceMonitor` mean there is probably nothing to scrape in the first place. Fixing that is
phase P0 and is a precondition for everything else.

---

## Table 1 — exists and usable today

Full reference in [Observability → Metrics](../../observability/metrics.md). Listed here is
the subset relevant to BGP/EVPN scale, with why each matters.

### Control plane, ovn-kubernetes

| Metric | Type | Relevance |
|---|---|---|
| `ovnkube_clustermanager_workqueue_depth{name}` | Gauge | Queue backlog per controller. The RA sub-controllers appear as `name="clustermanager routeadvertisements controller"`, `"... frrconfiguration controller"`, `"... nad controller"`, `"... node controller"`, `"... egressip controller"`, `"... namespace controller"`, `"... uplinkstate controller"` |
| `ovnkube_clustermanager_workqueue_adds_total{name}` | Counter | **The key metric for hypothesis 1.** Adds far exceeding actual intent changes is `ReconcileAll()` amplification |
| `ovnkube_clustermanager_workqueue_queue_duration_seconds{name}` | Histogram | Time spent waiting. Rises when `Threadiness: 1` starves |
| `ovnkube_clustermanager_workqueue_work_duration_seconds{name}` | Histogram | Time spent in `reconcile`. The direct measure of hypothesis 2 |
| `ovnkube_clustermanager_workqueue_unfinished_work_seconds{name}`, `..._longest_running_processor_seconds{name}` | Gauge | Detects a single reconcile that never returns |
| `ovnkube_clustermanager_workqueue_retries_total{name}` | Counter | Error-driven churn |
| `ovnkube_clustermanager_cluster_user_defined_networks{role,topology,transport}` | Gauge | Confirms the workload actually created what it intended |
| `ovnkube_clustermanager_udn_nad_sync_duration_seconds` | Histogram | NAD rendering cost, adjacent to `updateNADs` |
| `ovnkube_clustermanager_route_advertisement_condition{name,condition,status}` | Gauge | Acceptance state; the endpoint of `ra_accepted_latency` |
| `ovnkube_clustermanager_vtep_condition{name,condition,status}` | Gauge | VTEP acceptance state |
| `ovnkube_controller_sync_duration_seconds{resource}` | Gauge | Initial sync per resource type |
| `ovnkube_controller_resource_update_total{name}` | Counter | Event volume reaching the controller |
| `ovnkube_controller_pod_creation_latency_seconds` and the four-stage chain (`pod_first_seen_lsp_created_duration_seconds`, `pod_lsp_created_port_binding_duration_seconds`, `pod_port_binding_port_binding_chassis_duration_seconds`, `pod_port_binding_chassis_port_binding_up_duration_seconds`) | Histogram | Whether advertised networks make pod setup slower |
| `ovnkube_controller_network_programming_duration_seconds`, `..._network_programming_ovn_duration_seconds` | Histogram | End-to-end event-to-applied. Needs `--metrics-enable-config-duration` |
| `ovnkube_node_cni_request_duration_seconds` | Histogram | CNI ADD/DEL, the node-side pod cost |

### OVN and OVS

| Metric | Relevance |
|---|---|
| `ovn_db_db_size_bytes` | nbdb/sbdb growth as advertised networks and imported routes accumulate |
| `ovn_db_jsonrpc_server_sessions` | Client pressure on the databases |
| `ovn_db_txn_success`, `txn_error`, `txn_try_again`, `txn_aborted`, `txn_uncommitted` | Transaction outcome counters. **Counts only, no latency** — see table 2 |
| `ovn_northd_ovn_northd_loop_*` stopwatch family (`_total_samples`, `_maximum`, `_95th_percentile`, `_long_term_avg`) | northd loop duration. Rises with logical-topology size |
| `ovn_controller_flow_installation_*`, `flow_generation_*`, `if_status_mgr_run_*`, `ct_zone_commit_*` stopwatches | Per-node OVN programming cost |
| `ovn_controller_integration_bridge_openflow_total` | Flow count per node; the OVS-side ceiling |
| `ovs_vswitchd_dp_flows_total`, `ovs_vswitchd_bridge_flows_total` | Datapath and bridge flow counts |

### frr-k8s and FRR

| Metric | Relevance |
|---|---|
| `frrk8s_bgp_session_up{peer,vrf}` | Session state. The denominator for convergence and flap measurement |
| `frrk8s_bgp_updates_total{peer,vrf}` | UPDATE message volume. Detects churn amplification on the wire |
| `frrk8s_bgp_announced_prefixes_total{peer,vrf}` | **Confirms the derived prefix count from [01](01-methodology.md#advertised-prefixes)** |
| `frrk8s_bgp_received_prefixes_total{peer,vrf}` | The import-side volume feeding `routeimport` |
| `frrk8s_bfd_*` | BFD state, if BFD profiles are in use |
| frr-k8s controller-runtime `controller_runtime_reconcile_duration_seconds{controller}`, `workqueue_*` | frr-k8s's own reconcile cost, from its manager |
| `scrape_duration_seconds{job="frr-k8s-monitor"}` | **Not a value metric but a cost metric.** The exporter shells out to `vtysh`; see [bottleneck 6](03-bottlenecks.md#6-frr-k8s-metrics-exporter-scrape-cost) |

### Platform

Already present in `contrib/perf/metrics.yml`: apiserver request latency/rate/inflight,
container and pod CPU/RSS, node CPU/memory/network/disk, kubelet and CRI-O resource usage.
etcd metrics are the ones to watch for hypotheses 3, 4 and 5 —
`etcd_server_quota_backend_bytes`, `etcd_mvcc_db_total_size_in_bytes`,
`etcd_request_duration_seconds`, and `apiserver_storage_objects{resource}` broken down by CRD.

---

## Table 2 — missing, with code sites

Each row is one small, independent PR. Ordered by leverage.

| # | Proposed metric | Type | Where | Notes |
|---|---|---|---|---|
| 1 | `ovnkube_libovsdb_txn_duration_seconds{db,result}`, `ovnkube_libovsdb_txn_ops` | Histogram | `go-controller/pkg/libovsdb/ops/transact.go` | **Highest leverage single PR in this document.** There is currently zero instrumentation in this file. Transaction latency and op-count are the shared cost centre for every feature, not just BGP. `ovnkube_master_libovsdb_*` from the vendored client covers update messages and monitors, not transactions |
| 2 | `ovnkube_clustermanager_route_advertisements_reconcile_duration_seconds{ra,result}` | Histogram | `pkg/clustermanager/routeadvertisements/controller.go`, `reconcile` (L375) | The function already measures its own duration and emits it via `klog.V(4)`. Promoting it to a histogram is a few lines |
| 3 | `ovnkube_clustermanager_route_advertisements_frr_configurations{ra}` gauge; `..._frr_configuration_writes_total{ra,op}` counter with `op` in `create,update,delete,unchanged` | Gauge + Counter | `updateFRRConfigurations` (L1521) | Directly measures hypothesis 3. The `unchanged` label is what proves or disproves wasted rewrite churn |
| 4 | `ovnkube_clustermanager_route_advertisements_advertised_prefixes{ra,network,family}` | Gauge | The `getPrefixes` closure inside `generateFRRConfigurations` (L800) | Validates the derived prefix count against reality |
| 5 | `ovnkube_clustermanager_route_advertisements_nad_updates_total{ra}`, `..._nads_listed` | Counter + Gauge | `updateNADs` (L1617) | This function lists every NAD in the cluster on every RA reconcile. The gauge makes that visible |
| 6 | `ovnkube_node_route_import_sync_duration_seconds{network}`; `ovnkube_node_route_import_routes{network,source}` with `source` in `bgp,ovn`; `ovnkube_node_route_import_ops_total{network,op}` | Histogram + Gauge + Counter | `pkg/ovn/routeimport/route_import.go`, `syncNetwork` (L326), `getBGPRoutes` (L434), `getOVNRoutes` (L507) | Timings are already at `klog.V(5)`. The `ops_total` counter exposes transaction size, which is the metric for the ToR-flap scenario |
| 7 | `ovnkube_node_evpn_pod_program_duration_seconds`; `ovnkube_node_evpn_fdb_entries{network}`; `ovnkube_node_evpn_neigh_entries{network}` | Histogram + Gauge | `pkg/node/controllers/evpn/evpn_pod_controller.go`, `ensurePodNeighbors` (L184) | The per-pod EVPN cost, and the direct measure of the pods-not-nodes scaling property |
| 8 | `ovnkube_node_evpn_vtep_reconcile_duration_seconds{vtep}`; `ovnkube_node_evpn_networks{vtep}` | Histogram + Gauge | `pkg/node/controllers/evpn/evpn_node_controller.go` | `collectEVPNNetworks` walks all networks under the network-manager lock per reconcile |
| 9 | `ovnkube_node_evpn_vlans_used{vtep}` | Gauge | EVPN node controller | Distance to the 4094 ceiling. Cheap, and turns a hard wall into a dashboard warning |
| 10 | `ovnkube_clustermanager_route_advertisements_egressip_resolve_duration_seconds` | Histogram | `getEgressIPsByNodesByNetworks` (L1802) | Only meaningful when `EgressIP` advertisement is on |
| 11 | `ovnkube_controller_advertised_network_isolation_duration_seconds{network}` | Histogram | `pkg/ovn/udn_isolation.go`, `addAdvertisedNetworkIsolation` (L482) | Quantifies the `strict` vs `loose` delta from the controller side |

Rows 1, 2, 3 and 6 are the minimum viable set: they cover the control-plane loop, its output
volume, the node-side import loop, and the shared database cost. Rows 4, 5, 7-11 refine.

---

## Table 3 — measured outside Prometheus

These cannot reasonably become metrics, and should be captured by kube-burner hooks
(`beforeJobExecution`, `afterJobExecution`, `beforeCleanup`) writing to the artifact directory.

| Quantity | How |
|---|---|
| `FRRNodeState.status.runningConfig` byte size, min/mean/max/p99 across nodes | `kubectl get frrnodestates -o json \| jq '[.items[].status.runningConfig \| length]'`. **The measurement for the etcd ceiling in hypothesis 4** |
| Generated `FRRConfiguration` count and total bytes | `kubectl get frrconfigurations -A -l k8s.ovn.org/route-advertisements -o json` and size it |
| `BGPSessionState` object count | `kubectl get bgpsessionstates -A --no-headers \| wc -l`, plus `apiserver_storage_objects` |
| `VTEP` object size across node count | Tests the `okep-5088-evpn.md:1001` per-node IP map concern |
| FRR RIB and FIB sizes per VRF | `vtysh -c "show bgp vrf all summary json"` and `show ip route summary` in the frr-k8s pod |
| FRR reload wall time | `FRRNodeState.status.lastReloadResult` plus frr-k8s daemon logs |
| Peer-side route table | `vtysh` on the external FRR container. The far endpoint of `route_advertised_latency` |
| nbdb LRSR row count per logical router | `ovn-nbctl --format=json find logical_router_static_route` |
| OVN logical flow count | `ovn-sbctl --format=json list logical_flow \| wc` , or `ovn_controller_integration_bridge_openflow_total` per node |

---

## Required flags

Without these, most of table 1 does not exist.

| Flag | Effect | In the kind perf lane |
|---|---|---|
| `--metrics-enable-scale` | **Registers all workqueue metrics** and several latency histograms. Gates the single most important family for this work | **Off, but free to turn on**: set `OVN_METRICS_SCALE_ENABLE: "true"` in the workflow env block. `contrib/kind-common.sh:172` reads it, `kind-helm.sh:714` passes it to helm as `global.enableMetricsScale`, and `dist/images/ovnkube.sh:1205` turns it into the flag. No deployment change needed |
| `--metrics-enable-config-duration` | Registers `network_programming_duration_seconds` | Off; gated on `OVNKUBE_CONFIG_DURATION_ENABLE` (`ovnkube.sh:292`) |
| `--metrics-enable-pprof` | Serves `/debug/pprof/*` on the metrics address | **Already on, unconditionally** — `ovnkube.sh:1312` for ovnkube-controller and `:2550` for ovnkube-node. This is why the existing workloads' `pprof` measurement works, and why **profiling needs none of P0** |
| `--collection-interval` | How often OVS/OVN values are refreshed into the registry, default 30 s. Lower it for short runs or the scrape samples stale values | 30s |
| `--db-txn-timeout` | Default 100 s. The config comment notes it "may be useful to increase for high-scale clusters". Record its value with every result | 100s |

The practical consequence: **a CPU profile of ovnkube-cluster-manager under BGP load requires
no instrumentation, no ServiceMonitor and no flags.** That makes a first bottleneck hunt much
cheaper than the [roadmap's](05-roadmap.md) phase ordering suggests — see
[04](04-ci-kube-burner.md#minimum-viable-first-run).

---

## Useful PromQL

Starting points for `contrib/perf/metrics-ovnk.yml` and for interactive investigation.

**Reconcile amplification (hypothesis 1).** Compare queue adds against intent changes:

```promql
sum(rate(ovnkube_clustermanager_workqueue_adds_total{name=~"clustermanager routeadvertisements.*"}[2m])) by (name)
```

Against an idle cluster this should be approximately zero.

**Queue starvation (hypothesis 1, `Threadiness: 1`):**

```promql
histogram_quantile(0.99, sum(rate(ovnkube_clustermanager_workqueue_queue_duration_seconds_bucket[2m])) by (name, le))
```

**Reconcile cost (hypothesis 2):**

```promql
histogram_quantile(0.99, sum(rate(ovnkube_clustermanager_workqueue_work_duration_seconds_bucket{name="clustermanager routeadvertisements controller"}[2m])) by (le))
```

**Advertised prefix count against derivation:**

```promql
sum(frrk8s_bgp_announced_prefixes_total) by (vrf)
```

**Session health and flap:**

```promql
count(frrk8s_bgp_session_up == 0) by (vrf)
changes(frrk8s_bgp_session_up[10m]) > 4
```

**etcd object pressure (hypotheses 3, 4, 5):**

```promql
apiserver_storage_objects{resource=~"frrconfigurations.*|frrnodestates.*|bgpsessionstates.*|routeadvertisements.*|clusteruserdefinednetworks.*"}
```

**OVN database growth:**

```promql
ovn_db_db_size_bytes
rate(ovn_db_txn_try_again[2m])
```

**Exporter scrape cost (hypothesis 6):**

```promql
scrape_duration_seconds{job=~".*frr.*"}
```

**Steady-state control-plane CPU with an idle cluster** — the trap for hypothesis 1:

```promql
sum(irate(container_cpu_usage_seconds_total{container="ovnkube-cluster-manager"}[2m])) by (pod)
```

---

## Profiling

The existing workloads already capture CPU and heap profiles every minute via kube-burner's
`pprof` measurement, targeting `:9410` for ovnkube-controller and `:9411` for
ovnkube-control-plane. The BGP workloads reuse that block verbatim.

For BGP work, add the frr-k8s controller as a fourth target if it exposes pprof, and prefer
a longer `pprofInterval` on multi-hour runs to keep the artifact bundle manageable.

One Go benchmark already exists on this path and should be extended rather than replaced:
`pkg/ovn/routeimport/route_import_test.go`, `BenchmarkBGPRoutesStreaming`. No CI job currently
runs `go test -bench`; adding one for the `routeimport` and `libovsdb/ops` packages is a cheap
early-warning signal that does not need a cluster.

---

## Known instrumentation defects

| Defect | Impact | Fix |
|---|---|---|
| Workqueue histograms use `prometheus.ExponentialBuckets(10e-3, 10, 6)` — 10 ms to 1000 s in **six** buckets | p99 estimates are near-useless; a reconcile taking 200 ms and one taking 900 ms land in the same bucket | Widen to ~12 buckets with a base of 2 or 3. `pkg/metrics/workqueue.go:57,63` |
| ovnkube-node never calls `registerWorkqueueMetrics` | No queue visibility on the node side at all, where `routeimport` and the EVPN controllers live | Register it in the node metrics path, gated on the same `--metrics-enable-scale` |
| `Threadiness: 1` at 30+ call sites with no config knob | Queue-duration metrics will show starvation with no way to respond except a code change | Plumb threadiness through config; see [bottleneck 12](03-bottlenecks.md#12-hardcoded-concurrency-and-rate-limiting) |
| The RA sub-controllers pass `workqueue.DefaultTypedControllerRateLimiter` rather than the framework's `DefaultRateLimiter` | A 10 qps / 100 burst token bucket applies on top of exponential backoff, capping reconcile throughput independently of worker count | Record which limiter is in effect with every result; consider aligning with the framework default |
| Controller `name` labels contain spaces (`"clustermanager routeadvertisements controller"`) | PromQL needs quoting or regex matching; awkward in dashboards | Cosmetic, low priority, but worth knowing before writing queries |
