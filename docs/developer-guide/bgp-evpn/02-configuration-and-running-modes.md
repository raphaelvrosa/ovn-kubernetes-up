# 02 — Configuration, feature gates and running modes

Everything that decides whether the BGP and EVPN code paths run, and in
which shape. All values are in `go-controller/pkg/config/config.go` unless
stated.

## Contents

1. [Feature flags](#feature-flags)
2. [Config file sections](#config-file-sections)
3. [Validation at startup](#validation-at-startup)
4. [Derived predicates](#derived-predicates)
5. [Which controllers run where](#which-controllers-run-where)
6. [Running modes matrix](#running-modes-matrix)
7. [Deployment plumbing: ovnkube.sh, Helm, kind](#deployment-plumbing-ovnkubesh-helm-kind)
8. [The kind lab in one command](#the-kind-lab-in-one-command)

---

## Feature flags

| Flag (CLI) | Config key (`[ovnkubernetes-feature]`) | Field | Default |
|---|---|---|---|
| `--enable-route-advertisements` | `enable-route-advertisements` | `OVNKubernetesFeature.EnableRouteAdvertisements` (`:581`) | false |
| `--enable-evpn` | `enable-evpn` | `OVNKubernetesFeature.EnableEVPN` (`:582`) | false |
| `--advertised-udn-isolation-mode` | `advertised-udn-isolation-mode` | `OVNKubernetesFeature.AdvertisedUDNIsolationMode` (`:594`), `strict` or `loose` | `strict` (`:180`) |
| `--enable-multi-network` | `enable-multi-network` | required by route advertisements because the default network needs a NAD | false |
| `--enable-network-segmentation` | `enable-network-segmentation` | required for CUDNs, hence for VRF-Lite, EVPN and no-overlay CUDNs | false |
| `--enable-egress-ip` | | required to advertise `EgressIP` | |
| `--enable-dynamic-udn-allocation` | | changes which nodes advertise a CUDN ([07](07-cm-routeadvertisements-controller.md#dynamic-udn-allocation-interactions)) | false |
| `--enable-uplink` | | DPU next hops and VRF-Lite enslavement | false |
| `--transport` | `[default] transport` | `Default.Transport` (`:381`), `""` or `no-overlay` | `""` |
| `--gateway-mode` | `[gateway] mode` | `Gateway.Mode`, `shared` or `local` | shared |

Flag definitions: `:1154-1158` (`transport`), `:1372-1388` (the three feature flags).

---

## Config file sections

```ini
[default]
transport = no-overlay            # only for the default network; CUDNs use spec.network.transport

[no-overlay]                      # NoOverlayConfig, config.go:762-772
outbound-snat = enabled|disabled  # required with transport=no-overlay
routing = managed|unmanaged       # required with transport=no-overlay

[bgp-managed]                     # ManagedBGPConfig, config.go:781-797, file only, no CLI flags
as-number = 64512                 # default 64512 (:270)
topology = full-mesh              # required when routing=managed; only value supported
frr-namespace = frr-k8s-system    # required when routing=managed; enables the managed BGP controller
```

`buildManagedBGPConfig` (`:2693-2700`) reads `[bgp-managed]` from the file
only.

---

## Validation at startup

| Check | Code | Error |
|---|---|---|
| isolation mode is `strict` or `loose` | `:2459-2462` | invalid advertised-udn-isolation-mode |
| EVPN requires route advertisements | `:2463-2465` | EVPN requires route advertisements but route advertisements are disabled |
| `transport` is empty or `no-overlay` | `:2608-2610` | invalid transport |
| no-overlay requires route advertisements, `outbound-snat`, `routing`; managed routing requires `topology = full-mesh` and a valid `frr-namespace` | `:2613-2643` | |
| `[no-overlay]` set without `transport = no-overlay` | `:2644-2647` | warning only |
| managed ASN not 0, 23456, 65535 or 4294967295 | `:2703-2727` | invalid as-number |

Not validated at startup: EVPN with shared gateway mode. `IsEVPNEnabled()`
simply returns false in shared gateway mode, so the EVPN controllers do not
start and the CUDN controller rejects EVPN CUDNs with
`EVPNConfigError: ... only supported in local gateway mode`
(`userdefinednetwork/controller.go:1352-1354`).

---

## Derived predicates

```go
// go-controller/pkg/util/multi_network.go:2110-2118
func IsRouteAdvertisementsEnabled() bool {
	return config.OVNKubernetesFeature.EnableMultiNetwork && config.OVNKubernetesFeature.EnableRouteAdvertisements
}
func IsEVPNEnabled() bool {
	return IsRouteAdvertisementsEnabled() && config.Gateway.Mode == config.GatewayModeLocal && config.OVNKubernetesFeature.EnableEVPN
}
```

Use these, never the raw config fields, when gating code.

---

## Which controllers run where

| Process | Controller | Gate | Doc |
|---|---|---|---|
| ovnkube-cluster-manager | RouteAdvertisements controller | `IsRouteAdvertisementsEnabled()` | [07](07-cm-routeadvertisements-controller.md) |
| ovnkube-cluster-manager | managed BGP controller | route advertisements and `ManagedBGP.FRRNamespace != ""` | [08](08-cm-managed-bgp-controller.md) |
| ovnkube-cluster-manager | no-overlay validation controller | route advertisements and `Default.Transport == no-overlay` | [10](10-cm-cudn-transport-and-no-overlay.md#the-no-overlay-validation-controller) |
| ovnkube-cluster-manager | VTEP controller | `IsEVPNEnabled()` | [09](09-cm-vtep-controller.md) |
| ovnkube-cluster-manager | CUDN controller transport validation, VID allocator, RA and VTEP notifiers | network segmentation; RA informer when route advertisements; VTEP informer when EVPN | [10](10-cm-cudn-transport-and-no-overlay.md) |
| ovnkube-controller | route import manager | `IsRouteAdvertisementsEnabled()` | [13](13-ovnkube-controller-route-import.md) |
| ovnkube-controller | advertised network isolation, SNAT conditionals, EVPN topology | route advertisements; per network transport | [12](12-ovnkube-controller-advertised-networks.md), [14](14-ovnkube-controller-evpn-topology.md) |
| ovnkube-node | node network manager with RA watch | `isNetworkManagerRequiredForNode()` | [11](11-network-manager-propagation.md) |
| ovnkube-node | VRF manager, rule manager, advertised isolation nftables | network segmentation; route advertisements | [15](15-ovnkube-node-vrf-and-host-datapath.md) |
| ovnkube-node | EVPN node and pod controllers, netlink device manager | `IsEVPNEnabled()` | [16](16-ovnkube-node-evpn-datapath.md) |

In interconnect mode (the only supported mode for route advertisements,
`route-advertisements.md` known limitations) ovnkube-controller and
ovnkube-node run in the same `ovnkube-node` pod, one zone per node.

---

## Running modes matrix

| Scenario | Flags | Objects needed | Gateway mode |
|---|---|---|---|
| Advertise the default network | `-mne -rae` and a user `FRRConfiguration` plus RA | FRRConfiguration, RouteAdvertisements | both |
| Advertise CUDNs in the default VRF | add `-nse` | CUDN, RA selecting it | both |
| VRF-Lite CUDN | same | FRRConfiguration with a `vrf:` router, RA with `targetVRF: auto`, an interface in the VRF (manual or Uplink) | local (shared with Uplink) |
| EVPN CUDN | add `-evpn -gm local` | VTEP, CUDN with `transport: EVPN`, RA with `targetVRF: auto`, FRRConfiguration with a default-VRF router | local |
| No-overlay default network, unmanaged | `transport=no-overlay`, `routing=unmanaged`, RA and FRRConfiguration by the admin | | both |
| No-overlay default network, managed | `transport=no-overlay`, `routing=managed`, `[bgp-managed]` | nothing: generated | both |
| No-overlay CUDN | CUDN `transport: NoOverlay` with `noOverlay.routing` | RA (managed: generated) | both |
| Loose isolation | `--advertised-udn-isolation-mode=loose` | | both |
| DPU | `--ovnkube-node-mode dpu` / `dpu-host`, shared gateway | FRR on the DPU, next hops from the host annotations | shared |

---

## Deployment plumbing: ovnkube.sh, Helm, kind

| Layer | Variables | Code |
|---|---|---|
| Container entrypoint | `OVN_ROUTE_ADVERTISEMENTS_ENABLE`, `OVN_EVPN_ENABLE`, `OVN_ADVERTISED_UDN_ISOLATION_MODE` become `--enable-route-advertisements`, `--enable-evpn`, `--advertised-udn-isolation-mode` for cluster manager, controller and node (`dist/images/ovnkube.sh:247-252`, `:1155-1171`, `:1494-1510`, `:1998-2012`) |
| OVN tuning | in local gateway mode with route advertisements, `NB_Global.options:use_ct_inv_match=false` (`dist/images/ovnkube.sh:893-896`) |
| Helm | `global.enableRouteAdvertisements`, `enableEVPN`, `advertiseDefaultNetwork`, `advertisedUDNIsolationMode`, `enableNoOverlay`, `enableNoOverlaySnat`, `enableNoOverlayManagedRouting`, `managedBGPASNumber`, `managedBGPTopology`, `managedBGPFRRNamespace` (`helm/ovn-kubernetes/values-single-node-zone.yaml:84-106`) |
| kind | `-rae`, `-evpn`, `-adv`, `-rud`, `-noe [snat-enabled\|managed]`, `--bgp-*` and `--frr-k8s-*` options (`contrib/kind.sh:49-76`, `:220-260`); validations in `contrib/kind-common.sh:248-292`; deploys frr-k8s at `FRR_K8S_GIT_REF` with the demo patch and an external FRR router container (`:1633-1760`); `-adv` writes a `receive-all` style FRRConfiguration and a default RA |

The external FRR container in kind is a route reflector in AS 64512 peering
with every node; with `-evpn` it activates `l2vpn evpn` with
`advertise-all-vni` and `route-reflector-client` for each node
(`kind-common.sh:1739-1767`). An agnhost server on `172.26.0.0/16` behind
it is the "external server" the e2e tests reach.

---

## The kind lab in one command

```bash
cd contrib
./kind.sh -rae -mne -nse -evpn -adv -gm local -wk 2
kind export kubeconfig --name ovn
```

The perf-scale local lab guide walks through the result step by step:
[06-local-lab.md](../../perf-scale/bgp-evpn/06-local-lab.md).
