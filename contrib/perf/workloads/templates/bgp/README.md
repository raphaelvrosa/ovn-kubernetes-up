# BGP / EVPN workload templates

Skeletons for the benchmarks described in
[`docs/perf-scale/bgp-evpn/`](../../../../docs/perf-scale/bgp-evpn/index.md).

**These have not been executed against a cluster.** They parse and render, but first-run
validation is phase P2 of the roadmap, not something already done.

## Conventions

- **CUDN names must be under 16 characters** (VRF name predictability), hence `c{{ .Replica }}`
  rather than descriptive names.
- **`PodNetwork` advertisement requires `nodeSelector: {}`** — the RouteAdvertisements CRD's
  CEL rule rejects a populated selector when `PodNetwork` is advertised.
- Pod, service and network-policy objects are **not duplicated here**; the workloads reference
  `../udn-density/` directly.

## Subnet allocation

Network index `i = .Replica - 1`, allocated out of `10.128.0.0/10` as a `/20` per network:

```
10.{{ add 128 (div i 16) }}.{{ mul (mod i 16) 16 }}.0/20
```

1024 networks fit. A `/20` with `hostSubnet: 26` gives 64 nodes x 64 addresses. **That caps
pods per node at ~60**, so the pod-density workloads override `cidrPrefix` and `hostSubnet`
via `inputVars` and run fewer networks. Density in networks and density in pods are different
runs; do not try to get both from one configuration.

VNI and VLAN arithmetic mirrors `test/e2e/allocators/bgp.go` (`vidMin = 2`, `vidMax = 4094`).
Do not invent a second allocation scheme — two schemes that disagree about VNI ranges is an
afternoon of debugging.
