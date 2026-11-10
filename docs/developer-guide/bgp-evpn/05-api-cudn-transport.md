# 05 — API: ClusterUserDefinedNetwork transport, EVPN and no-overlay fields

BGP and EVPN do not add a network CRD of their own. They extend the existing
`ClusterUserDefinedNetwork` (CUDN) with a `transport` selector and two
transport-specific blocks, and they add a `vrfName` to its status. This page
covers exactly those parts; the rest of the CUDN API is documented in
[user-defined-networks.md](../../features/user-defined-networks/user-defined-networks.md).

## Contents

1. [Where the API lives](#where-the-api-lives)
2. [`spec.network.transport`](#specnetworktransport)
3. [`spec.network.evpn`](#specnetworkevpn)
4. [`spec.network.noOverlay`](#specnetworknooverlay)
5. [CEL rules that tie topology, role and transport together](#cel-rules-that-tie-topology-role-and-transport-together)
6. [Status: `TransportAccepted` and `vrfName`](#status-transportaccepted-and-vrfname)
7. [How the fields reach the rest of the system: the NAD `netconf`](#how-the-fields-reach-the-rest-of-the-system-the-nad-netconf)
8. [NetInfo accessors](#netinfo-accessors)
9. [Namespaced UserDefinedNetwork](#namespaced-userdefinednetwork)
10. [Review notes](#review-notes)

---

## Where the API lives

| Artifact | Path |
|---|---|
| CUDN spec and transport types | `go-controller/pkg/crd/userdefinednetwork/v1/cudn.go` |
| EVPN types and route target validation | `go-controller/pkg/crd/userdefinednetwork/v1/evpn.go` |
| Getters shared by UDN and CUDN specs | `go-controller/pkg/crd/userdefinednetwork/v1/spec.go` |
| CNI-side mirror written into the NAD | `go-controller/pkg/cni/types/types.go:96-109`, `:207-226` |
| NAD rendering | `go-controller/pkg/clustermanager/userdefinednetwork/template/net-attach-def-template.go` |
| CRD integration scenarios | `test/crd-integration/testscenario/cudn/*evpn*.go`, `*no-overlay*.go` |

---

## `spec.network.transport`

```go
// cudn.go:98-107 (doc comment abridged)
// Transport describes the transport technology for pod-to-pod traffic.
// Allowed values are "NoOverlay" and "EVPN".
// When omitted, the network uses the default OVN overlay transport (e.g. Geneve, VXLAN)
// +kubebuilder:validation:Enum=NoOverlay;EVPN
// +kubebuilder:validation:XValidation:rule="self == oldSelf", message="Transport is immutable"
Transport TransportOption `json:"transport,omitempty"`
```

| CRD value (`cudn.go:290-297`) | Canonical value in NAD and `NetInfo` (`types/const.go:336-338`) | East/west path |
|---|---|---|
| unset | `""` | Geneve overlay through the interconnect transit switch |
| `NoOverlay` | `no-overlay` | Routed natively, learned through BGP; no transit switch |
| `EVPN` | `evpn` | VXLAN driven by EVPN; no transit switch |

The conversion is `transportToNetConf` in
`net-attach-def-template.go:331-341`. Code that decides whether a network
uses the interconnect overlay calls `hasInterconnectTransport()`
(`ovn/base_network_controller.go:991-998`), which returns `false` for both
`evpn` and `no-overlay`.

The field is immutable; a transport change means deleting and recreating the
CUDN.

---

## `spec.network.evpn`

```go
// evpn.go:252-268
type EVPNConfig struct {
	VTEP   string     `json:"vtep"`              // name of a VTEP CR, required
	MACVRF *VRFConfig `json:"macVRF,omitempty"`  // required for Layer2, forbidden for Layer3
	IPVRF  *VRFConfig `json:"ipVRF,omitempty"`   // required for Layer3, optional for Layer2
}

// evpn.go:305-330
type VRFConfig struct {
	VNI         int32             `json:"vni"`                   // 1..16777215
	RouteTarget RouteTargetString `json:"routeTarget,omitempty"` // "<ASN>:<VNI>" when omitted, derived by FRR
}
```

### VNI

A 24-bit VXLAN Network Identifier (`Minimum=1`, `Maximum=16777215`,
`evpn.go:310-312`). Uniqueness is enforced per VTEP by the CUDN controller
(`userdefinednetwork/controller.go:528-546`, `reserveVNIs`), not by CEL. The
MAC-VRF and IP-VRF of one network must differ (`evpn.go:251`).

The RA controller re-validates the range before writing it into FRR
configuration (`routeadvertisements/validation.go:83-88`), because a NAD can
be hand-written and bypass the CRD.

### Route target

`RouteTargetString` is a 6-byte BGP extended community value in FRR's accepted
notations (`evpn.go:274-302`). The CEL rules admit:

| Form | Global administrator | Local administrator |
|---|---|---|
| `EF:OPQR` | 2-byte ASN (0..65535) | up to 4 bytes |
| `GHJK:MN` | 4-byte ASN (65536..4294967295) | up to 2 bytes |
| `A.B.C.D:MN` | IPv4 address | up to 2 bytes |
| `*:OPQR` / `*:MN` | wildcard | up to 4 bytes |

The same grammar is implemented in Go by `validateRouteTarget`
(`routeadvertisements/validation.go:178-217`), and the comment at
`evpn.go:270-272` ties the two together: change one, change the other. When
the route target is omitted, FRR auto-derives `<ASN>:<VNI>`; the generated raw
config only emits `route-target import/export` lines when a value is set
(`rawconfig.go:243-249`, `:270-273`).

A side effect of the 6-byte constraint: with a 4-byte ASN the auto-derived
target only works for VNIs up to 65535 (`evpn.go:291`).

### VTEP

A reference by name to a [VTEP](04-api-vtep.md). It is validated by the CUDN
controller at reconcile time (`transport_validation.go:203-222`): missing
gives `TransportAccepted=False/VTEPNotFound`, not accepted gives
`VTEPNotAccepted`.

---

## `spec.network.noOverlay`

```go
// cudn.go:305-315
type NoOverlayConfig struct {
	OutboundSNAT SNATOption    `json:"outboundSNAT"` // Enabled | Disabled
	Routing      RoutingOption `json:"routing"`      // Managed | Unmanaged
}
```

- `outboundSNAT` controls whether pod-to-external traffic is SNATed to the
  node IP. Its effect in OVN and nftables is described in
  [12](12-ovnkube-controller-advertised-networks.md#snat-rules-for-advertised-networks)
  and [15](15-ovnkube-node-vrf-and-host-datapath.md).
- `routing: Managed` opts the CUDN into the managed full-mesh BGP fabric; the
  managed BGP controller labels it and creates the shared RA
  ([08](08-cm-managed-bgp-controller.md)).

The block is immutable (`cudn.go:110`).

---

## CEL rules that tie topology, role and transport together

All on `NetworkSpec` (`cudn.go:54-62`) unless noted.

| Rule | Message |
|---|---|
| `transport != 'NoOverlay' \|\| (topology == 'Layer3' && layer3.role == 'Primary')` | transport 'NoOverlay' is only supported for Layer3 primary networks |
| `transport != 'NoOverlay' \|\| has(noOverlay)` | spec.noOverlay is required when type transport is 'NoOverlay' |
| `transport == 'NoOverlay' \|\| !has(noOverlay)` | spec.noOverlay is forbidden when transport type is not 'NoOverlay' |
| `transport != 'EVPN' \|\| primary Layer2 \|\| primary Layer3` | transport 'EVPN' is only supported for Layer2 or Layer3 primary networks |
| `transport != 'EVPN' \|\| has(evpn)` | spec.evpn field is required when transport is 'EVPN' |
| `transport == 'EVPN' \|\| !has(evpn)` | spec.evpn field is forbidden when transport is not 'EVPN' |
| `transport != 'EVPN' \|\| topology != 'Layer2' \|\| has(evpn.macVRF)` | spec.evpn.macVRF field is required for Layer2 topology when transport is 'EVPN' |
| `transport != 'EVPN' \|\| topology != 'Layer3' \|\| has(evpn.ipVRF)` | spec.evpn.ipVRF field is required for Layer3 topology when transport is 'EVPN' |
| `transport != 'EVPN' \|\| topology != 'Layer3' \|\| !has(evpn.macVRF)` | spec.evpn.macVRF field is forbidden for Layer3 topology when transport is 'EVPN' |
| On `ClusterUserDefinedNetworkSpec` (`cudn.go:29`): `!has(uplinks) \|\| transport != 'EVPN'` | spec.uplinks is not supported with EVPN transport |
| On `EVPNConfig` (`evpn.go:250-251`) | at least one of macVRF or ipVRF must be specified; macVRF and ipVRF must use different VNIs |

Resulting supported matrix:

| Topology | Role | `""` (overlay) | `NoOverlay` | `EVPN` |
|---|---|---|---|---|
| Layer3 | Primary | yes | yes | yes, `ipVRF` only |
| Layer2 | Primary | yes | no | yes, `macVRF` required, `ipVRF` optional |
| Layer3 / Layer2 | Secondary | yes | no | no |
| Localnet | any | yes | no | no |

---

## Status: `TransportAccepted` and `vrfName`

### `TransportAccepted` condition

Set by the CUDN controller for every CUDN whose transport is not the default
overlay (`transport_validation.go:54-91`). Reasons (`transport_validation.go:27-36`):

| Status | Reason | Meaning |
|---|---|---|
| `True` | `EVPNTransportAccepted` / `NoOverlayTransportAccepted` | An RA selects this CUDN, advertises `PodNetwork`, and is `Accepted=True` |
| `False` | `EVPNRouteAdvertisementsIsMissing` / `NoOverlayRouteAdvertisementsIsMissing` | No RA advertises this CUDN's pod network |
| `False` | `EVPNRouteAdvertisementsNotAccepted` / `NoOverlayRouteAdvertisementsNotAccepted` | RAs exist but none is accepted; message lists them |
| `False` | `RouteAdvertisementsNotEnabled` | The RouteAdvertisements feature is off |
| `False` | `VTEPNotFound` / `VTEPNotAccepted` / `EVPNConfigError` | EVPN only; see [10](10-cm-cudn-transport-and-no-overlay.md) |

The condition is set on the in-memory object and applied once with the other
CUDN conditions by `updateClusterUDNStatus`, so a single status update carries
all of them (`transport_validation.go:48-50`).

### `status.vrfName`

```go
// cudn.go:127-134
// VRFName is the name of the Linux VRF device that OVN-Kubernetes creates ...
VRFName *string `json:"vrfName,omitempty"`
```

Computed by `util.GetCUDNVRFName` (`util/multi_network.go:2208-2216`): the
CUDN name verbatim when it is at most 15 bytes (`types.MaxInterfaceNameLength`)
and not `default`, otherwise the ID-derived form `<UDNVRFDevicePrefix><id><UDNVRFDeviceSuffix>`.
Users must read this field rather than assume the name; the RA controller,
the node VRF manager and the route import controller all derive the same name
from `util.GetNetworkVRFName` (`multi_network.go:2179-2190`).

---

## How the fields reach the rest of the system: the NAD `netconf`

The CUDN controller renders one `NetworkAttachmentDefinition` per selected
namespace. The NAD's `spec.config` JSON carries the transport fields in the
ovn-kubernetes CNI `NetConf` (`cni/types/types.go`):

```json
{
  "cniVersion": "1.0.0",
  "name": "cluster_udn_blue",
  "type": "ovn-k8s-cni-overlay",
  "topology": "layer2",
  "role": "primary",
  "subnets": "10.0.10.0/24",
  "transport": "evpn",
  "evpn": {
    "vtep": "evpn-vtep",
    "macVRF": {"vni": 100, "routeTarget": "65000:100", "vid": 12},
    "ipVRF":  {"vni": 101, "vid": 11}
  }
}
```

Two things are added at render time that are not in the CUDN:

- **`vid`**: a cluster-wide VLAN ID per MAC-VRF and per IP-VRF, allocated by
  the CUDN controller from an `id.Allocator` of size 4095 with IDs 0 and 1
  reserved (`userdefinednetwork/controller.go:71-77`, `:306-314`,
  `controller_helper.go:328-378`). VIDs are recovered from existing NADs on
  restart (`recoverEVPNIDs`, `controller.go:490-497`) and released on CUDN
  deletion (`controller.go:518-519`). The node uses the VID for the bridge
  VLAN, the SVI and the VLAN-to-VNI mapping ([16](16-ovnkube-node-evpn-datapath.md)).
- The EVPN block is only rendered when `util.IsEVPNEnabled()`; otherwise the
  render fails with `EVPN transport requested but EVPN feature is not enabled`
  (`net-attach-def-template.go:223-228`).

`transport` and `evpn` are part of the fields compared by
`util.AreNetworksCompatible`; a change to them is not reconcilable in place
and recreates the network controller.

---

## NetInfo accessors

Every consumer reads the fields through `util.NetInfo`
(`util/multi_network.go`):

| Method | Default network (`DefaultNetInfo`) | User defined network |
|---|---|---|
| `Transport()` | `config.Default.Transport` (`:692`) | `netconf.transport` (`:946`) |
| `OutboundSNAT()` | `config.NoOverlay.OutboundSNAT` | `netconf.noOverlay.outboundSNAT` (`:951`) |
| `EVPNVTEPName()`, `EVPNMACVRFVNI()`, `EVPNMACVRFRouteTarget()`, `EVPNIPVRFVNI()`, `EVPNIPVRFRouteTarget()`, `EVPNMACVRFVID()`, `EVPNIPVRFVID()` | empty / `0` (`:702-735`) | from `netconf.evpn` (`:956-1010`) |
| `GetPodNetworkAdvertisedVRFs()`, `GetPodNetworkAdvertisedOnNodeVRFs(node)` | set by the network manager from RAs (`:354-384`) | same |
| `GetEgressIPAdvertisedVRFs()`, `GetEgressIPAdvertisedOnNodeVRFs(node)`, `GetEgressIPAdvertisedNodes()` | same (`:386-422`) | same |

Two derived predicates are used everywhere and deserve memorising
(`multi_network.go:2157-2177`):

```go
func IsPodNetworkAdvertisedAtNode(netInfo NetInfo, node string) bool {
	switch netInfo.Transport() {
	case types.NetworkTransportNoOverlay, types.NetworkTransportEVPN:
		return true
	default:
		return len(netInfo.GetPodNetworkAdvertisedOnNodeVRFs(node)) > 0
	}
}

func IsPodNetworkAdvertisedAtNodeDefaultVRF(netInfo NetInfo, node string) bool {
	switch netInfo.Transport() {
	case types.NetworkTransportEVPN:
		return false
	default:
		return slices.Contains(netInfo.GetPodNetworkAdvertisedOnNodeVRFs(node), types.DefaultNetworkName)
	}
}
```

No-overlay and EVPN networks are therefore treated as advertised **before**
any RA is accepted. That is deliberate: both transports are unusable without
advertisement, and the controllers must program the advertised datapath from
the start rather than flip it later.

---

## Namespaced UserDefinedNetwork

`UserDefinedNetworkSpec` implements the same `GetTransport`, `GetEVPN` and
`GetNoOverlay` getters but returns the zero values (`spec.go:23-36`). A
namespaced UDN is always overlay and can never be advertised, because the RA
network selector only admits CUDNs and the default network.

---

## Review notes

- Adding a field under `evpn` requires touching, in order: `evpn.go` (CRD),
  `cni/types/types.go` (NAD mirror), `net-attach-def-template.go`
  (`renderEVPNConfig`), `util/multi_network.go` (accessor on both NetInfo
  implementations and the fake), `routeadvertisements/validation.go` if the
  value is written to FRR, and `test/crd-integration` scenarios.
- CEL rules reference `oldSelf`; ratcheting means an invalid object that was
  admitted by an older CRD stays admitted on no-op updates. The RA controller's
  re-validation exists for exactly this reason.
- The `vid` must never be user settable: it is an allocator output and the
  node assumes cluster-wide consistency.
