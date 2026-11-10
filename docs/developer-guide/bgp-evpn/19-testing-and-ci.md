# 19 — Testing and CI

Where the tests for these features live, how CI selects them, and how to
run them locally. Read this before claiming coverage for a change.

## Contents

1. [Unit tests](#unit-tests)
2. [CRD integration tests](#crd-integration-tests)
3. [E2E suites](#e2e-suites)
4. [How CI lanes select BGP and EVPN specs](#how-ci-lanes-select-bgp-and-evpn-specs)
5. [Running locally](#running-locally)
6. [Coverage gaps worth knowing](#coverage-gaps-worth-knowing)
7. [Performance lanes](#performance-lanes)

---

## Unit tests

| Package | Files | Framework | Notes |
|---|---|---|---|
| `pkg/clustermanager/routeadvertisements` | `controller_test.go` (4,881 lines), `rawconfig_test.go`, `validation_test.go` | `testing` with table tests and fake clients | the main table `TestController_reconcile` asserts generated objects field by field; see [07](07-cm-routeadvertisements-controller.md#unit-tests-to-know) |
| `pkg/clustermanager/managedbgp` | `controller_test.go` (1,591 lines) | | base template and RA lifecycle, filters |
| `pkg/clustermanager/vtep` | `controller_test.go` (1,680 lines), suite | Ginkgo | status reasons, finalizer, overlap symmetry |
| `pkg/clustermanager/userdefinednetwork` | `controller_test.go`, `transport_validation_test.go` | Ginkgo | `TransportAccepted`, VID allocation and recovery |
| `pkg/clustermanager/nooverlay` | `controller_test.go` | | events |
| `pkg/networkmanager` | `nad_controller_test.go`, `network_controller_test.go` | | `setAdvertisements`, `preserveAdvertisements`, activity notifications |
| `pkg/ovn/routeimport` | `route_import_test.go` (989 lines) | | see [13](13-ovnkube-controller-route-import.md#tests) |
| `pkg/ovn` | `udn_isolation_test.go`, gateway and layer2/layer3 controller tests | Ginkgo with libovsdb test harness | SNAT matches, isolation ACLs, EVPN switch |
| `pkg/node` | `gateway_udn_test.go`, `gateway_nftables_test.go`, `bridgeconfig/*_test.go`, `vrfmanager/*_test.go` | Ginkgo, fake netlink and nftables | routes, rules, flows, masquerade |
| `pkg/node/controllers/evpn` | `evpn_node_controller_test.go`, `evpn_pod_controller_test.go`, `utils_test.go` | Ginkgo, mocked device manager | see [16](16-ovnkube-node-evpn-datapath.md#tests) |
| `pkg/node/netlinkdevicemanager` | `controller_test.go`, `link_test.go` | Ginkgo | reconciler and mapping diffs |

Run with:

```bash
cd go-controller
go test -race -cover ./pkg/clustermanager/routeadvertisements/... ./pkg/clustermanager/vtep/... \
  ./pkg/clustermanager/managedbgp/... ./pkg/ovn/routeimport/... ./pkg/node/controllers/evpn/...
```

`make test` runs everything; `make lint` is required before a PR.

---

## CRD integration tests

`test/crd-integration/` runs CEL and defaulting rules against a real
apiserver through envtest, no cluster needed (`make -C test test-crd`).
Scenarios relevant here:

| Scenario file | Covers |
|---|---|
| `testscenario/cudn/valid-scenarios-evpn.go`, `invalid-scenarios-evpn.go` | the EVPN rules in [05](05-api-cudn-transport.md#cel-rules-that-tie-topology-role-and-transport-together) |
| `testscenario/cudn/valid-scenarios-no-overlay.go`, `invalid-scenarios-no-overlay.go` | no-overlay rules |
| `test/e2e/vtep_api_validations.go` (`EVPN: VTEP API validations`) | VTEP CEL rules, currently in the e2e tree with the `RouteAdvertisements` and `EVPN` feature labels |

A change to a CEL marker in `routeadvertisements/v1/types.go`,
`vtep/v1/types.go` or `userdefinednetwork/v1/{cudn,evpn}.go` requires a
scenario here, per `go-controller/pkg/crd/AGENTS.md`.

---

## E2E suites

All in `test/e2e/`, Ginkgo. Feature labels are in `test/e2e/feature/features.go`
(`RouteAdvertisements`, `RouteAdvertisementsDynamicUDN`, `NoOverlay`, `EVPN`).

| File | Describe | What it exercises |
|---|---|---|
| `route_advertisements.go:86` | `BGP: When default podNetwork is advertised` | external server reachability, toggling the default RA off and on (Serial) |
| `route_advertisements.go:771` | `BGP: Pod to external server when CUDN network is advertised` | Layer3, Layer3 no-overlay (SNAT enabled and disabled, unmanaged), Layer2 CUDNs advertised in the default VRF |
| `route_advertisements.go:1274` | `BGP: When an advertised CUDN network is dynamically allocated` | acceptance when the network is active on a subset of nodes |
| `route_advertisements.go:1433` | `BGP: isolation` | the connectivity matrix between default network, advertised UDNs, hosts, services and NodePorts, for Layer3, Layer3 no-overlay and Layer2 |
| `route_advertisements.go:2570` | `BGP: For BGP configured networks` | VRF-Lite (L3, L2, L3 no-overlay variants) and EVPN (L3 IP-VRF, L2 MAC-VRF, L2 MAC-VRF plus IP-VRF, shared and per-network VTEPs): reach and be reached by an external server in the same VRF or VPN, KAPI access |
| `route_advertisements_managed_peers.go` | `BGP: managed self-peer filtering` (Serial) | managed template self-peer removal and InternalIP changes |
| `route_advertisements_service_marks.go` | `BGP: UDN service marks after advertisement updates` | service mark refresh without Service events |
| `no_overlay.go` | `No-Overlay: Default network is enabled with no-overlay` | connectivity across ovnkube-node restarts, managed RA and FRRConfiguration repair |
| `evpn.go` | EVPN suite (`WHAT="EVPN"`) | EVPN-specific scenarios, run with `ginkgo -procs=3` |
| `allocators/bgp.go` | | allocates VIDs 2 to 4094 and VNIs for tests; a scale test that exhausts VLANs collides with it |
| `kubevirt.go` (kv-live-migration lane with `evpn: enable-evpn`) | | live migration on EVPN networks |

The external topology the tests assume (FRR router container, `bgpnet`
agnhost server) is built by `contrib/kind-common.sh` (see
[02](02-configuration-and-running-modes.md#deployment-plumbing-ovnkubesh-helm-kind)).

---

## How CI lanes select BGP and EVPN specs

`.github/workflows/test.yml` matrix rows with a `routeadvertisements`
value get `ENABLE_ROUTE_ADVERTISEMENTS=true` (`:718`); `evpn: enable-evpn`
or target `evpn` gets `ENABLE_EVPN=true` (`:719`); `advertise-default`
sets `ADVERTISE_DEFAULT_NETWORK` (`:720`); `no-overlay` values set the three
`ENABLE_NO_OVERLAY*` variables (`:734-736`).

| Lane (`target`) | Gateway | Family | Flags | Runs |
|---|---|---|---|---|
| `bgp` (local dualstack, shared dualstack, local ipv6) | as listed | | advertise-default, network segmentation | `make -C test control-plane` with no `WHAT`: every `Feature:RouteAdvertisements` spec except skips below |
| `bgp-no-overlay` | local | dualstack | plus `no-overlay: true` | same plus `Feature:NoOverlay` |
| `bgp-no-overlay-dynamic` | shared | dualstack | plus dynamic UDN allocation, `no-overlay: snat-enabled` | same; `BGP: For BGP configured networks` skipped on dynamic clusters |
| `bgp-loose-isolation` | shared | dualstack | `advertised-udn-isolation-mode: loose` | same |
| `bgp-managed` | local | dualstack | `routeadvertisements: managed`, `no-overlay: managed` | Serial suite only, no external router: `BGP: When default podNetwork is advertised` skipped |
| `evpn` | local | dualstack | advertise-default, network segmentation, `enable-evpn` | `WHAT="EVPN"` only |
| `kv-live-migration` (local dualstack) | local | dualstack | route advertisements, `enable-evpn` | KubeVirt suites |
| `shard-conformance` rows with `routeadvertisements` | | | | Kubernetes conformance only, no `test/e2e` specs |

Selection logic is in `test/scripts/e2e-cp.sh:140-240` and `:318-335`:
`Feature:EVPN` is skipped unless `ENABLE_EVPN`, `Feature:NoOverlay` unless
`ENABLE_NO_OVERLAY`, `Feature:RouteAdvertisementsDynamicUDN` unless both
route advertisements and dynamic allocation are on, and
`Feature:RouteAdvertisements` unless route advertisements are on. The
`BGP: For BGP configured networks` suite (VRF-Lite and EVPN CUDNs) only runs
in local gateway lanes and never on dynamic clusters.

Consequences for a PR:

- A new spec labelled `feature.EVPN` runs only in the `evpn` lane, which
  passes `WHAT="EVPN"`; the describe text must match that focus or the spec
  is silently not selected.
- A new `feature.RouteAdvertisements` spec with `ginkgo.Serial` runs only in
  `bgp-managed` (the `serial` lane does not enable route advertisements);
  check its preconditions against that lane's flags.
- Shared gateway EVPN is not covered anywhere, by design.
- The lanes starting with `bgp` and `evpn` have a 190 minute timeout
  (`test.yml:878`).

---

## Running locally

```bash
# cluster with BGP, EVPN and a default-network RA
cd contrib && ./kind.sh -rae -mne -nse -evpn -adv -gm local -wk 2
kind export kubeconfig --name ovn

# one suite
cd test && make control-plane WHAT="BGP: For BGP configured networks"
# EVPN suite
make control-plane WHAT="EVPN"
```

Environment variables honoured by `e2e-cp.sh` include
`ENABLE_ROUTE_ADVERTISEMENTS`, `ENABLE_EVPN`, `ENABLE_NO_OVERLAY`,
`ADVERTISE_DEFAULT_NETWORK`, `DYNAMIC_UDN_ALLOCATION`, `OVN_GATEWAY_MODE`,
`FOCUS` and `FLAKE_ATTEMPTS`. `docs/developer-guide/local_testing_guide.md`
has the generic instructions; the perf-scale local lab
([06-local-lab.md](../../perf-scale/bgp-evpn/06-local-lab.md)) has the
BGP-specific troubleshooting.

---

## Coverage gaps worth knowing

- VRF-Lite in shared gateway mode with Uplinks has its own OKEP and Uplink
  lanes; it is not exercised by the `bgp` lanes.
- `EgressIP` advertisement is covered in unit tests and in the egress IP
  e2e suites when route advertisements are enabled, not in a dedicated lane.
- DPU next hops are unit tested (`TestGetDPUHostGatewayNextHopsUsesDPUHostAddressAnnotation`)
  and exercised by the dpu-simulator lanes.
- Nothing in CI measures scale; see below.

---

## Performance lanes

`.github/workflows/performance-test.yml` runs kube-burner workloads from
`contrib/perf/`. On this branch it does not include BGP or EVPN workloads;
the `perf-scale/bgp-evpn-benchmarking` branch carries the `bgp-ra-density`,
`bgp-ra-churn`, `bgp-route-import`, `evpn-density` and `evpn-pod-density`
workloads and the matching docs copied into `docs/perf-scale/bgp-evpn/`
([04-ci-kube-burner.md](../../perf-scale/bgp-evpn/04-ci-kube-burner.md),
[07-running-kube-burner.md](../../perf-scale/bgp-evpn/07-running-kube-burner.md)).
