# Glossary

Terms used across the [BGP and EVPN Performance and Scale](index.md) document set.

## Contents

1. [OVN-Kubernetes concepts](#ovn-kubernetes-concepts)
2. [BGP and EVPN concepts](#bgp-and-evpn-concepts)
3. [frr-k8s objects](#frr-k8s-objects)
4. [Benchmarking terms](#benchmarking-terms)
5. [SLI naming convention](#sli-naming-convention)

---

## OVN-Kubernetes concepts

| Term | Meaning |
|---|---|
| **RA** | `RouteAdvertisements`, the cluster-scoped CRD that declares which networks are advertised, from which nodes, derived from which base `FRRConfiguration`s. `go-controller/pkg/crd/routeadvertisements/v1` |
| **CUDN** | `ClusterUserDefinedNetwork`, a cluster-scoped user-defined network. The only network type selectable by an RA besides the default network |
| **NAD** | `NetworkAttachmentDefinition`. Each UDN/CUDN renders to one NAD per namespace; the RA controller annotates every NAD with the RAs that select its network |
| **`ReconcileAll()` fan-out** | The pattern where a watched object's event re-enqueues *every* RouteAdvertisements rather than the affected ones. See [bottleneck 1](03-bottlenecks.md#1-reconcileall-fan-out) |
| **Generated FRRConfiguration** | An `FRRConfiguration` produced by ovn-kubernetes, named `ovnk-generated-*`, labelled `k8s.ovn.org/route-advertisements: <ra>` and annotated `<ra>/<sourceFRRConfig>/<node>`. One per (RA x node x source config) |
| **Source / base FRRConfiguration** | A user-authored `FRRConfiguration` selected by `spec.frrConfigurationSelector`, used as the template for generated ones |
| **No-overlay** | `transport: no-overlay`. Pod traffic is routed natively using BGP-learned routes instead of Geneve encapsulation |
| **Managed BGP** | `[bgp-managed] topology=full-mesh`. ovn-kubernetes auto-generates the base `FRRConfiguration` peering every node with every other node, and the RAs to go with it. No external fabric required |
| **Route import** | The node-side path that reads `RTPROT_BGP` routes from the network's VRF routing table and programs them as OVN logical router static routes. `go-controller/pkg/ovn/routeimport` |
| **LRSR** | Logical Router Static Route, the nbdb table that imported routes land in |
| **Advertised-UDN isolation** | nftables chain `udn-bgp-drop` plus OVN ACLs preventing advertised UDNs from reaching each other. `strict` (default) or `loose` via `--advertised-udn-isolation-mode` |
| **Dynamic UDN allocation** | `EnableDynamicUDNAllocation`. A network is only instantiated on nodes that need it, which lets the RA controller skip nodes where the network is inactive |
| **VRF-Lite** | Advertising a network in its own VRF (`targetVRF: auto`) without EVPN. Local gateway mode only |

---

## BGP and EVPN concepts

| Term | Meaning |
|---|---|
| **ASN** | Autonomous System Number |
| **eBGP / iBGP** | External / internal BGP. The fabric model uses eBGP to the ToR; managed full-mesh uses iBGP between nodes |
| **ToR** | Top-of-Rack switch, the usual BGP peer for a node in a fabric deployment |
| **RIB / FIB** | Routing Information Base (BGP's view) / Forwarding Information Base (the kernel's) |
| **VTEP** | VXLAN Tunnel End Point. Also the cluster-scoped `VTEP` CRD declaring which node CIDRs are VTEP sources |
| **VNI** | VXLAN Network Identifier, the 24-bit fabric-wide tenant identifier |
| **VID** | VLAN ID on the local EVPN bridge, mapped 1:1 to a VNI. Bounded at 4094 |
| **MAC-VRF** | An EVPN L2 domain (a bridge domain). Maps to a Layer2 CUDN |
| **IP-VRF** | An EVPN L3 domain (a routing table). Maps to a Layer3 CUDN or the L3 side of a Layer2 one |
| **RT** | Route Target, the BGP extended community that controls import/export between VRFs |
| **Type-2 route** | EVPN MAC/IP advertisement. **One per pod IP** — the reason EVPN route count scales with pods |
| **Type-3 route** | EVPN Inclusive Multicast Ethernet Tag, one per VNI per VTEP, used for BUM handling |
| **Type-5 route** | EVPN IP Prefix route, the subnet-level L3 advertisement |
| **SVD** | Single VXLAN Device. One VXLAN netdev carries all VNIs, distinguished by VLAN. The source of the 4094 limit |
| **SVI** | Switched Virtual Interface, the L3 interface on a bridge VLAN |
| **BUM** | Broadcast, Unknown-unicast and Multicast traffic |
| **MAC mobility** | The EVPN mechanism that lets a MAC move between VTEPs, used for KubeVirt live migration |

---

## frr-k8s objects

| Object | Meaning and scale relevance |
|---|---|
| **`FRRConfiguration`** | The input CR. frr-k8s merges all of them that select a node into one FRR config |
| **`FRRNodeState`** | One per node. `status.runningConfig` holds the **entire rendered FRR running configuration as a string**, plus conversion and reload results. Grows with VRF count; see [bottleneck 4](03-bottlenecks.md#4-frrnodestatestatusrunningconfig-size) |
| **`BGPSessionState`** | One per (node, peer, VRF) with `bgpStatus` and `bfdStatus`. Cardinality is multiplicative; see [bottleneck 5](03-bottlenecks.md#5-bgpsessionstate-cardinality) |
| **frr-k8s daemon** | The per-node DaemonSet that renders config and reloads FRR |
| **frr-k8s metrics exporter** | Sidecar that shells out to `vtysh ... json` to produce `frrk8s_bgp_*` metrics. Scrape cost grows with peers x VRFs |

---

## Benchmarking terms

| Term | Meaning |
|---|---|
| **Topology model** | Dense, Sparse or Mesh. Determines how CUDNs map onto nodes; see [methodology](01-methodology.md#topology-models) |
| **Scale ladder** | The ordered sequence of values for each dimension, climbed until something fails |
| **Derived quantity** | A number not set directly but implied by the targets, such as generated-object count |
| **Cold start** | Measuring a component booting into pre-existing cluster state |
| **Incremental** | Measuring the cost of a change applied to a cluster already at scale. The interesting case for RA |
| **Churn** | Repeated create/delete cycles to expose leaks, unbounded growth and stale-state handling |
| **Convergence** | The interval from a Kubernetes intent change to the corresponding route being present at the BGP peer and usable in the dataplane |
| **Ceiling** | A hard limit that produces failure rather than slowdown. Reported as a number, not a curve |
| **Blind spot** | A hop in the end-to-end path with no metric covering it |

---

## SLI naming convention

Service Level Indicators in this set are named `<subject>_<verb>_latency` and always state
their two endpoints explicitly, because the interesting disagreements are about where the
measurement starts and stops.

| SLI | From | To |
|---|---|---|
| `ra_accepted_latency` | RA `metadata.creationTimestamp` | RA `status.conditions[Accepted] == True` |
| `frrconfig_rendered_latency` | RA accepted | All selected nodes' `FRRNodeState.status.lastReloadResult` reflects the new config |
| `route_advertised_latency` | RA created | Prefix observable in the peer's BGP table |
| `route_imported_latency` | Route present in the node's VRF table | Corresponding LRSR present in nbdb |
| `evpn_pod_reachable_latency` | Pod `Ready` | Type-2 route for the pod IP observable at the peer |
| `netpol_style_enforcement_latency` | Intent applied | Traffic actually follows the new path, measured from a client pod |

The last one is deliberately shaped like kube-burner's existing `netpolLatency` measurement,
which is the model for the custom convergence measurement proposed in
[04](04-ci-kube-burner.md#upstream-asks).
