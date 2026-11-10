# 07 — Running kube-burner

How to run the BGP performance lane on your own machine, what each input file
does, what comes out, and how to read it.

Everything below was run against a three-node kind cluster with kube-burner
v2.5.0, the version the workflow pins. Outputs are real.

## Contents

1. [What kube-burner does here](#what-kube-burner-does-here)
2. [Quickstart](#quickstart)
3. [Input artifacts](#input-artifacts)
4. [How a run executes](#how-a-run-executes)
5. [Configuration reference](#configuration-reference)
6. [Customising a run](#customising-a-run)
7. [Outputs](#outputs)
8. [Interpreting the output](#interpreting-the-output)
9. [FAQ: what runs in CI, and how](#faq-what-runs-in-ci-and-how)

---

## What kube-burner does here

kube-burner is a Kubernetes load generator. It is **not** a test framework: it
creates objects, waits for them to reach a state you define, measures what
happened, and writes the measurements out. Nothing asserts; you read the
numbers.

The model is four nested things:

```
config            one YAML file = one workload
└── job           ordered, sequential; each has its own cleanup and wait policy
    └── object    a Go template rendered `replicas` times
        └── waitOptions   what "ready" means for that object
measurements      run for the whole config, not per job (pprof, podLatency, ...)
metrics profile   PromQL scraped from Prometheus at the end and indexed
```

For `bgp-ra-density` the shape is: create 20 namespaces and 20 CUDNs, wait for
each CUDN to report `NetworkCreated`, then create 20 RouteAdvertisements and
wait for each to report `Accepted`. While that happens, capture CPU and heap
profiles. At the end, query Prometheus for the metric profile and run a hook
that records the things Prometheus cannot see.

!!! warning "This lane is not a pass/fail gate"
    It has no alert profile, so it only fails if kube-burner itself errors — an
    object that never becomes ready, a template that will not render. A slow
    reconcile produces a large number in the output, not a red run. Turning
    numbers into gates is a later step once a baseline exists.

---

## Quickstart

### 1. A cluster with BGP enabled

```bash
cd contrib
./kind.sh -rae -mne -nse -adv -sm -gm local -wk 2
```

See [06 — Local Lab](06-local-lab.md#bring-up-the-cluster) for what each flag
does. The two that matter here:

- `-rae` installs frr-k8s, the external `frr` container, and the **`receive-all`
  FRRConfiguration that the workload's RouteAdvertisements selects**. Without it
  every RA is Accepted-but-generates-nothing, and the lane measures an empty
  path.
- `-sm` sets `--metrics-enable-scale`, which registers the workqueue and
  libovsdb metric families. Without it those queries return nothing.

### 2. kube-burner

```bash
curl -L https://github.com/kube-burner/kube-burner/releases/download/v2.5.0/kube-burner-V2.5.0-linux-x86_64.tar.gz \
  | tar xz kube-burner
sudo mv kube-burner /usr/local/bin/
kube-burner version
```

Match the version. The workflow pins **v2.5.0** and `performance-report.yml`
additionally SHA256-pins the tarball.

### 3. Run

```bash
cd contrib/perf              # cwd matters: all paths are relative to here
kube-burner init --config workloads/bgp-ra-density.yml
```

That is the minimum. It creates objects and captures pprof, but collects no
Prometheus metrics. For those, see [with metrics](#running-with-metrics).

### 4. Read the results

```bash
cat bgp-state-bgp-ra-density.json | jq
ls pprof-data/
go tool pprof -top -nodecount=20 pprof-data/ovnkube-control-plane-*.pprof
```

### Running with metrics

The metric profile needs a Prometheus that scrapes ovn-kubernetes, reachable on
`localhost:9090`:

```bash
# install the stack and the ovn-kubernetes PodMonitors
./../install-prometheus-infra.sh
kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090 &

cd contrib/perf
kube-burner init --config workloads/bgp-ra-density.yml -e metric-endpoint-local.yml
```

!!! note "Bind address"
    `install-prometheus-infra.sh` installs PodMonitors that scrape the **pod
    IP**. If your cluster was built with `-mip 127.0.0.1`, the metrics endpoints
    bind to loopback only and every scrape fails. Build without `-mip`, or with
    `-mip 0.0.0.0`. The CI lane sets `METRICS_IP: "0.0.0.0"` for exactly this
    reason.

---

## Input artifacts

Everything lives under `contrib/perf/`. Paths inside the config files are
relative to that directory, which is why you must `cd` there first.

| Artifact | Role |
|---|---|
| `workloads/bgp-ra-density.yml` | The workload: jobs, objects, waits, measurements |
| `workloads/templates/bgp/ns.yml` | Namespace, labelled for UDN and indexed |
| `workloads/templates/bgp/cudn_l3.yml` | One Layer3 ClusterUserDefinedNetwork per iteration |
| `workloads/templates/bgp/ra.yml` | One RouteAdvertisements per CUDN |
| `metrics.yml` | Platform PromQL: apiserver, etcd, cAdvisor, kubelet, CRI-O |
| `metrics-ovnk.yml` | ovn-kubernetes, OVN and OVS PromQL |
| `metric-endpoint-local.yml` | Prometheus URL + which profiles + write results to `metrics/` |
| `metric-endpoint.yml` | Same, plus a second indexer writing to OpenSearch |
| `performance-meta.yml` | Env-var template for run metadata; `envsubst`-ed into `perf-meta.yml` in CI |
| `collect-bgp-state.sh` | `beforeCleanup` hook; records what cannot be a Prometheus series |
| `../prometheus-ovnk-podmonitors.yaml` | Makes Prometheus scrape ovnkube at all |

### Object templates

Plain Go templates with [sprig](https://masterminds.github.io/sprig/) functions.
The variables kube-burner provides:

| Variable | Meaning |
|---|---|
| `.Replica` | 1-based index within this object's `replicas` |
| `.Iteration` | 0-based index of the job iteration |
| `.JobName` | Name of the enclosing job |
| `.UUID` | Run UUID |
| anything under `inputVars` | Passed per object from the workload file |

Subnets are derived arithmetically so each network gets a distinct range:

{% raw %}
```yaml
- cidr: 10.{{ add 128 (div (sub .Replica 1) 16) }}.{{ mul (mod (sub .Replica 1) 16) 16 }}.0/20
  hostSubnet: 26
```
{% endraw %}

That carves `10.128.0.0/10` into a `/20` per network, so 1024 networks fit
without colliding with the default cluster subnet.

!!! warning "Two naming rules that bite"
    **CUDN names must be under 16 characters** when advertised — the VRF is
    named after the CUDN and Linux caps interface names at 15. Hence
    `c{% raw %}{{ sub .Replica 1 }}{% endraw %}`, not a descriptive name.

    **`nodeSelector` must be empty** on a RouteAdvertisements that advertises
    `PodNetwork`. A CEL rule on the CRD rejects a populated one, because the pod
    network has to be advertised from every node.

---

## How a run executes

The phase order, taken from an actual run log:

```
Cleaning up previous runs for job: bgp-ra-density-networks
Cleaning up previous runs for job: bgp-ra-density
Triggering job: bgp-ra-density-networks
Waiting up to 4h0m0s for actions to be completed
Actions completed
Verifying created objects
Job bgp-ra-density-networks took 4s
Triggering job: bgp-ra-density
...
Waiting for beforeCleanup command ./collect-bgp-state.sh bgp-ra-density to finish
BeforeCleanup out: wrote ./bgp-state-bgp-ra-density.json
Job bgp-ra-density took 6s
Stopping measurement: pprof
Finished execution with UUID: f567435c-...
```

Points worth knowing:

1. **Jobs run in order, not in parallel.** Networks are created and confirmed
   before any RouteAdvertisements exists, so the RA reconciles do not race a
   growing set of NADs.
2. **`cleanup: true` means "clean up *previous* runs, at the start".** It is not
   a teardown. Objects from your run are still there when kube-burner exits.
   Harmless in CI, where the cluster is destroyed, surprising locally.
3. **`beforeCleanup` runs at the end of its job**, after the objects exist and
   before any later cleanup. That is why the hook sees the full object count.
4. **A failing hook fails the run.** kube-burner sets `rc=1` if the command
   errors, so `collect-bgp-state.sh` is written to always `exit 0`.
5. **Measurements are config-scoped**, not job-scoped. `pprof` starts before the
   first job and stops after the last.

---

## Configuration reference

Only the fields this lane uses. The full set is in the
[kube-burner docs](https://kube-burner.github.io/kube-burner/).

### Job fields

| Field | What it does | Here |
|---|---|---|
| `name` | Job name, and the namespace prefix when `namespacedIterations` is set | must equal the workload filename for the job carrying `podLatency` — see the FAQ |
| `jobIterations` | How many times to run the whole object set | `1`; scale comes from `replicas` instead |
| `qps` / `burst` | Client-side rate limit toward the apiserver | `10` / `10`, matching the other lanes |
| `namespacedIterations` | Create one namespace per iteration | `false`; the namespaces are explicit objects |
| `waitWhenFinished` | Block until the job's objects are ready | `true` |
| `cleanup` | Delete leftovers from a **previous** run before starting | `true` |
| `beforeCleanup` | Shell command run at the end of the job | the state-capture hook |
| `objects[]` | Templates to render, in order | ns, cudn, ra |

### Object fields

| Field | What it does |
|---|---|
| `objectTemplate` | Path to the template, relative to `contrib/perf` |
| `replicas` | How many to render |
| `inputVars` | Extra values exposed to the template |
| `waitOptions.customStatusPaths` | jq expression over `.status` plus an expected value |

Waiting on a CRD condition is what `customStatusPaths` is for:

```yaml
waitOptions:
  customStatusPaths:
  - key: '(.conditions.[] | select(.type == "Accepted")).status'
    value: "True"
```

No Go code is needed for this — a common misconception. It works for any CRD
that reports standard conditions, so `NetworkCreated` on a CUDN and `Accepted`
on a RouteAdvertisements or VTEP are all covered.

### Measurements

```yaml
global:
  measurements:
    - name: pprof
      pprofInterval: 1m
      pprofDirectory: pprof-data
      pprofTargets:
      - name: ovnkube-control-plane
        namespace: "ovn-kubernetes"
        labelSelector: {name: ovnkube-control-plane}
        url: http://localhost:9411/debug/pprof/profile?seconds=30
```

kube-burner `exec`s into the matched pods and curls the URL from inside, which
is why the URL says `localhost`. `--metrics-enable-pprof` is unconditional in
the ovnkube image, so no flag is needed.

`podLatency` is deliberately **not** enabled in this lane: it creates no pods.

---

## Customising a run

### Change the scale

Both `replicas` values must move together — one CUDN per namespace, one RA per
CUDN:

```yaml
- objectTemplate: workloads/templates/bgp/ns.yml
  replicas: 100
- objectTemplate: workloads/templates/bgp/cudn_l3.yml
  replicas: 100
...
- objectTemplate: workloads/templates/bgp/ra.yml
  replicas: 100
```

The derived quantity to predict before you run is in
[01](01-methodology.md#generated-frrconfiguration-objects):

```
generated FRRConfigurations = RAs x nodes x matching source FRRConfigurations
```

With one source config, 100 RAs on 3 nodes should produce 300.

### Advertise one network from one RA instead of many

Swap the per-CUDN selector in `ra.yml` for the shared label and set
`replicas: 1`. That gives few objects each carrying many prefixes, rather than
many objects each carrying few — the Dense versus Sparse contrast from
[01](01-methodology.md#topology-models).

### Add or remove metrics

Append to `metrics-ovnk.yml`:

```yaml
- query: <promql>
  metricName: <camelCaseName>
```

`metricName` becomes the filename in `metrics/` and the document type in
OpenSearch. Keep queries filtered with `> 0` where an empty result is normal,
so the output does not fill with zero series.

### Run only part of a workload

There is no job filter. Comment out the jobs you do not want, or copy the
workload file. Job ordering is the file order.

---

## Outputs

After a local run with metrics enabled, `contrib/perf/` contains:

| Path | Contents |
|---|---|
| `kube-burner-<uuid>.log` | Full run log. The UUID ties everything together |
| `bgp-state-bgp-ra-density.json` | Hook output: object counts and sizes, session state, peer prefix totals |
| `pprof-data/*.pprof` | CPU and heap profiles per target per interval |
| `metrics/<metricName>.json` | One file per entry in the metric profiles |
| `metrics/jobSummary.json` | Per-job start, end, and object operation counts |

All of these are `.gitignore`d, so a local run leaves the tree clean.

### What the hook captures

```json
{
  "label": "bgp-ra-density",
  "timestamp": "2026-10-09T09:49:46Z",
  "routeAdvertisements":        { "count": 21, "accepted": 21 },
  "generatedFRRConfigurations": { "count": 63, "total_bytes": 71898, "max_bytes": 1157 },
  "frrNodeStates": [
    { "node": "ovn-control-plane", "bytes": 1352, "reload": "success" },
    { "node": "ovn-worker",        "bytes": 1345, "reload": "success" },
    { "node": "ovn-worker2",       "bytes": 1346, "reload": "success" }
  ],
  "bgpSessionStates": { "count": 3, "established": 3 },
  "peerBGPSummary": {
    "ribCount": 133, "peerCount": 3, "failedPeers": 0,
    "prefixesReceivedFromNodes": 63, "prefixesSentToNodes": 201
  }
}
```

These are deliberately outside Prometheus. Object *sizes* and a BGP peer's view
are not metrics any in-cluster exporter produces, and
[`FRRNodeState` size against the 1.5 MB etcd object limit](03-bottlenecks.md#4-frrnodestatestatusrunningconfig-size)
is one of the hard ceilings the whole exercise exists to find.

---

## Interpreting the output

### Start with the derivation, not the graph

Every run should be checked against what you predicted. From the run above, on
3 nodes with 21 RouteAdvertisements and one source FRRConfiguration:

| Quantity | Predicted | Observed |
|---|---|---|
| Generated FRRConfigurations | 21 × 3 × 1 = 63 | **63** |
| Prefixes received by the peer | 21 networks × 3 nodes = 63 | **63** |
| Sessions | 3 nodes × 1 peer × 1 VRF = 3 | **3** |

A mismatch is itself the finding. Under-count usually means some nodes were
skipped — dynamic UDN allocation, or a source FRRConfiguration whose own
`nodeSelector` is narrower than the RA's.

### The signal this lane exists to produce

```promql
route_advertisements_frr_configuration_writes_total{op="unchanged"}
```

On the validation run this read **393** for the single pre-existing `default`
RouteAdvertisements, while the 20 new ones read 3 each for `create` and nothing
else. Creating unrelated resources caused `default` to be reconciled ~131 times,
each time re-deriving all three of its generated objects and comparing them with
`reflect.DeepEqual` to conclude nothing had changed.

That is [bottleneck 1](03-bottlenecks.md#1-reconcileall-fan-out), measured. The
ratio of `unchanged` to `create + update + delete` is the headline number:

- **high unchanged, low everything else** → the controller is re-deriving state,
  not doing work. Cost scales with unrelated cluster churn.
- **both low** → healthy.

### Reading the rest

| Look at | To answer |
|---|---|
| `ovnkubeWorkqueueQueueDuration99th` high while `WorkDuration99th` is moderate | Is the single worker starved, or is the reconcile genuinely expensive? |
| `raReconcileDuration99th` by `result` | Is time going into successful work or into `config_error`/`pending` churn? |
| `libovsdbTxnDuration99th` by `db` | Is the bottleneck in OVN writes rather than in the controller? |
| `raGeneratedFRRConfigurations` vs `bgpCRObjectCount` | Is apiserver/etcd pressure tracking the object count as expected? |
| `routeImportRoutes` with `source=bgp` ≠ `source=ovn` | A sync is pending or failing; they converge after a successful one |
| `frrNodeStates[].bytes` across scales | How far from the 1.5 MB etcd object limit? |

### Profiles

```bash
go tool pprof -top -nodecount=30 pprof-data/ovnkube-control-plane-<pod>-<ts>.pprof
```

If [bottleneck 2](03-bottlenecks.md#2-generatefrrconfigurations-recomputes-everything)
is real, `generateFRRConfigurations`, the node lister and annotation parsing
dominate. The profile is the fastest way to confirm or kill that, and it needs
no Prometheus at all.

### What a number from kind does and does not mean

Kind runs 3 workers on one machine. Treat every absolute value as **relative
only** — comparable against another run on the same shape, never quotable as a
capability. Ratios and counts (generated objects, unchanged writes, prefixes)
are trustworthy; latencies and throughput are not. See
[01 reporting rules](01-methodology.md#reporting-rules).

---

## FAQ: what runs in CI, and how

**When does the lane run?**
Nightly, on the `schedule` trigger in `.github/workflows/performance-test.yml`.
Per-PR runs exist for the other lanes but BGP is nightly-only — four extra lanes
against a 240-minute timeout is not a per-PR cost anyone wants.

**On what?**
A single `oracle-vm-32cpu-128gb-x86-64` runner: one kind cluster, 3 workers plus
2 infra nodes, Prometheus on the infra nodes, image built from the PR head.

**How does CI differ from a local run?**

| | Local | CI |
|---|---|---|
| Cluster | whatever you built | kind, 3 workers + 2 infra, `-gm local` |
| Metrics endpoint | you port-forward | `kubectl port-forward` in the run step |
| Indexer | `metric-endpoint-local.yml` | `metric-endpoint.yml` when `ES_SERVER` is set |
| Metadata | none | `envsubst` over `performance-meta.yml` |
| Image | whatever is loaded | built from the PR head by the `build-pr` job |

**Does it gate merges?**
No. There is no alert profile, so it fails only on kube-burner errors. It is an
observation lane.

**Where do the artifacts go?**
Uploaded per run: the report, the pprof bundle, and the whole of `contrib/perf`
including `metrics/` and the hook JSON. Then
`performance-report.yml` compares against a baseline and comments on the PR.

**Why did my new lane produce no artifacts?**
The four artifact steps — report generation, report upload, pprof upload, data
upload — are each gated on an `if:` listing the workload names explicitly. A new
`perf-test` value runs the benchmark and then **silently discards everything**
unless it is added to all four. This is the single easiest way to waste a
two-hour run.

**Why is the pod latency section of my report empty?**
`generate_perf_report.py` loads `podLatencyMeasurement-<workload>.json`, keyed on
the `perf-test` matrix value. The kube-burner **job** carrying the `podLatency`
measurement must therefore be named exactly after the workload file. A mismatch
is not fatal — the loader returns an empty list — it just silently empties that
section. `bgp-ra-density` creates no pods, so its report has CPU and memory
sections only.

**Can I run the CI workload unmodified against a real cluster?**
Yes, that is the point of keeping it in `contrib/perf` rather than in a
scale-lab repository. Only the `replicas` values change. See
[04 — the kind ceiling](04-ci-kube-burner.md#the-kind-ceiling) for the two-tier
strategy.

**Why is there no EVPN lane?**
EVPN needs a VTEP, per-node VTEP IPs and VXLAN devices on the fabric side, none
of which the kind harness builds. [06](06-local-lab.md#scenario-c-evpn-layer2-mac-vrf)
shows how to do it by hand; automating it is roadmap P5.

**Why does the lane use `targetVRF` unset rather than `auto`?**
`auto` needs a router for each network's VRF in the source FRRConfiguration, and
a path from that VRF to the fabric. The kind harness peers everything in the
default VRF, so a VRF-Lite session never establishes — demonstrated in
[00](00-background.md#the-catch-observed). Unset matches what the `bgp-l3-ra`
e2e case already exercises.

---

## Divergence from this branch's skeletons

This branch still carries the original skeleton set — five workload files, eight
templates, an alert profile — written before any of it ran. What actually
shipped is smaller: one workload, three templates, no alert profile, plus the
PodMonitors and `metrics-ovnk.yml` that turned out to be the real prerequisite.

**The shipped lane is the reference.** The remaining skeletons are still
unexecuted and should be treated as drafts for later phases, or deleted once
their phase lands.
