# 06 — The FRRConfiguration contract with frr-k8s

OVN-Kubernetes never talks to FRR. It reads user-authored `FRRConfiguration`
objects as templates and writes generated `FRRConfiguration` objects; frr-k8s
merges all objects that select a node and renders the FRR daemon configuration.
This page documents that contract: which fields are read, which fields are
written, what raw FRR configuration is emitted, and which FRR semantics the
implementation relies on.

## Contents

1. [Versions and sources](#versions-and-sources)
2. [The frr-k8s objects](#the-frr-k8s-objects)
3. [FRRConfiguration fields OVN-Kubernetes reads](#frrconfiguration-fields-ovn-kubernetes-reads)
4. [FRRConfiguration fields OVN-Kubernetes writes](#frrconfiguration-fields-ovn-kubernetes-writes)
5. [Naming, labels and annotations of generated objects](#naming-labels-and-annotations-of-generated-objects)
6. [Merge semantics relied upon](#merge-semantics-relied-upon)
7. [Raw FRR configuration](#raw-frr-configuration)
8. [FRR features relied upon](#frr-features-relied-upon)
9. [Known constraints of the contract](#known-constraints-of-the-contract)
10. [Reading frr-k8s state when debugging](#reading-frr-k8s-state-when-debugging)

---

## Versions and sources

| Component | Where pinned | Value |
|---|---|---|
| frr-k8s Go API | `go-controller/go.mod:33` | `github.com/metallb/frr-k8s v0.0.0-20260603082256-b43efcb206be` |
| frr-k8s deployed in kind | `contrib/kind-common.sh:1633-1634` | `FRR_K8S_GIT_REF=b43efcb206be` |
| Vendored API types | `go-controller/vendor/github.com/metallb/frr-k8s/api/v1beta1/frrconfiguration_types.go` | |
| frr-k8s documentation | <https://github.com/metallb/frr-k8s> and <https://metallb.github.io/frr-k8s/> | |
| FRR documentation | <https://docs.frrouting.org/en/latest/bgp.html>, <https://docs.frrouting.org/en/latest/evpn.html> | |

The kind harness applies `contrib/frr-k8s/patches/0001-Improvements-to-the-demo.patch`
to the frr-k8s demo so that the external FRR router peers with every node and,
with `-evpn`, activates `l2vpn evpn` with `advertise-all-vni`
(`contrib/kind-common.sh:1739-1760`).

---

## The frr-k8s objects

| Object | Scope | Written by | Read by OVN-Kubernetes |
|---|---|---|---|
| `FRRConfiguration` (`frrk8s.metallb.io/v1beta1`) | namespaced | users, MetalLB, OVN-Kubernetes | yes, through `wf.FRRConfigurationsInformer()` (`factory.go:1971`) |
| `FRRNodeState` | cluster | frr-k8s daemon, one per node, carries `status.runningConfig`, `lastConversionResult`, `lastReloadResult` | no |
| `BGPSessionState` | namespaced | frr-k8s daemon, one per (node, neighbor, VRF) | no |

OVN-Kubernetes neither reads `FRRNodeState` nor `BGPSessionState`. There is
no feedback from FRR into the RA status: `Accepted=True` means the generated
objects were written, not that a session is established.

---

## FRRConfiguration fields OVN-Kubernetes reads

From each **source** object selected by `spec.frrConfigurationSelector`
(vendored types at `frrconfiguration_types.go:24-130`):

| Field | Used for | Code |
|---|---|---|
| `metadata.name` | Skip objects named `ovnk-generated-*` | `controller.go:737-740` |
| `metadata.namespace` | Generated objects are created in the **same namespace** as their source | `controller.go:1721` |
| `spec.nodeSelector` | Which selected nodes the template applies to | `controller.go:742-756` |
| `spec.bgp.routers[].vrf` | Match against `targetVRF` and selected network VRFs; collect the set of VRFs any source defines | `controller.go:757-759`, `:1286-1325` |
| `spec.bgp.routers[].asn` | Copied; also inherited by IP-VRF routers created for EVPN networks | `controller.go:1490`, `:1564-1597` |
| `spec.bgp.routers[].id` | Copied into the import routers | `controller.go:1522` |
| `spec.bgp.routers[].neighbors[]` | Every neighbor is copied, then its `toAdvertise` and, for no-overlay networks and EVPN, `toReceive` are rewritten | `controller.go:1377-1481` |
| `neighbors[].address` / `neighbors[].interface` | Identify the neighbor for raw config; interface-only neighbors are "unnumbered" and carry both families | `controller.go:2440-2452`, `validation.go:117-135` |
| `neighbors[].dualStackAddressFamily` | **Rejected** when `true` | `controller.go:1413-1421` |
| `spec.bgp.routers[].neighbors[].asn`, `localASN`, `dynamicASN`, `sourceaddress`, BFD, password, timers and every other neighbor field | Copied verbatim through struct copy | `controller.go:1378` (`neighbor := ...` copies the whole struct) |
| `spec.bgp.bfdProfiles` | Copied verbatim (`new.Spec = source.Spec`) | `controller.go:1736` |
| `spec.raw` | **Replaced** by the generated raw config when any is produced; otherwise the source's raw block is kept through the struct copy | `controller.go:1736-1746` |

Fields never consulted: `neighbors[].toReceive` of the source is preserved
only by copy (the controller does not reason about it), `neighbors[].toAdvertise`
of the source is **discarded** and rebuilt.

---

## FRRConfiguration fields OVN-Kubernetes writes

Each generated object has:

```yaml
metadata:
  generateName: ovnk-generated-
  namespace: <source namespace>
  labels:
    k8s.ovn.org/route-advertisements: <ra name>
  annotations:
    k8s.ovn.org/route-advertisements: <ra>/<source>/<node>
    k8s.ovn.org/route-advertisements-validated: "true"
spec:
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: <node>          # controller.go:1738-1742
  bgp:
    routers: [...]                            # see below
  raw:                                        # only when EVPN or allowas-in lines are needed
    priority: 10                              # rawConfigPriority, controller.go:66
    rawConfig: |
      ...
```

Router contents per case, all built in `generateFRRConfiguration`
(`controller.go:1269-1750`):

| Router | `vrf` | `prefixes` | `neighbors[].toAdvertise` | `neighbors[].toReceive` | `imports` |
|---|---|---|---|---|---|
| Matched source router | as source | the node's prefixes for the matched network(s): host subnets (L3/default), network subnets (L2), egress IPs | `mode: filtered`, same prefixes filtered by the neighbor's family (unnumbered gets both) | untouched, unless no-overlay networks are selected: then `filtered` with each no-overlay pod subnet as `{prefix, ge: hostSubnetLength, le: hostSubnetLength}` | when `targetVRF` is not `auto`: one `{vrf: <other selected network vrf>}` per other selected network |
| Reciprocal import router (`targetVRF` not `auto`) | each other selected network's VRF | none | none | none | `{vrf: <target vrf or "default">}` |
| IP-VRF router for an EVPN network, when the source has a router for that VRF | that VRF | the network's prefixes on this node | none (EVPN uses the underlay session) | none | none |
| IP-VRF router for an EVPN network, when no source has one | that VRF, with the **underlay router's ASN** | the network's prefixes on this node | none | none | none |
| Default-VRF router, EVPN underlay | `""` | union of matched prefixes **and** the node's VTEP host routes (`/32` or `/128`) | prefixes above, family filtered | adds `{prefix: <vtep cidr>, ge: 32|128, le: 32|128}` per VTEP CIDR, `mode: filtered` | as above |
| Cloned default-VRF router carrying only VTEP routes (`targetVRF` is a literal VRF name and the default router was not matched) | `""` | VTEP host routes only | VTEP host routes only | VTEP CIDR selectors | as source |

DPU shared gateway mode additionally sets `neighbors[].toAdvertise.nextHop`
to the DPU host's gateway address or the resolved `UplinkState` address
(`controller.go:1440-1451`, `:1753-1839`).

---

## Naming, labels and annotations of generated objects

- `generateName: ovnk-generated-` (`controller.go:62`). Names are random; the
  identity key is the annotation value `<ra>/<source>/<node>`.
- The label `k8s.ovn.org/route-advertisements: <ra>` is the selector used to
  list "everything this RA owns" (`controller.go:1931-1942`).
- `updateFRRConfigurations` (`controller.go:1924-2019`) diffs desired versus
  existing by annotation key: create when missing, `Update` when
  `reflect.DeepEqual(spec)` or the validated annotation differs, delete the
  rest. If several existing objects share a key, the last one is kept and the
  others deleted.
- `FieldManager: clustermanager-routeadvertisements-controller` on every
  write, so the controller can ignore its own update events
  (`frrConfigurationNeedsUpdate`, `controller.go:2298-2309`).

---

## Merge semantics relied upon

frr-k8s merges every `FRRConfiguration` whose `nodeSelector` matches the node
into one FRR configuration. The implementation depends on these merge
behaviours:

1. **Routers with the same ASN and VRF are merged.** The generated per-node
   object re-declares the same `router bgp <asn>` as the source; frr-k8s
   unions neighbors, prefixes and imports. This is why the source keeps its
   `toReceive` and the generated object adds `toAdvertise`: both end up on
   the same neighbor. The route-advertisements feature doc states the
   consequence at
   [route-advertisements.md](../../features/bgp-integration/route-advertisements.md#frrconfiguration-instances-generated-by-ovn-kubernetes):
   "frr-k8s will merge the prefixes from both the generated and the base
   FRRConfiguration" (`controller.go:1455-1457` comment).
2. **A neighbor must be declared with the same ASN everywhere it appears**, so
   the generated object copies the source neighbor struct instead of
   re-creating it.
3. **Raw configuration blocks are appended in `priority` order.** OVN-Kubernetes
   uses priority 10 so a user can override with a higher value
   (`controller.go:64-67`).
4. **Imports (`router bgp ... vrf X` + `import vrf Y`) are rendered per
   router**, which is why reciprocal leaking needs two routers.

If frr-k8s merge rules change, the contract above is what to re-verify.

---

## Raw FRR configuration

Generated by `generateRawConfig` (`rawconfig.go:60-87`). The structure is
documented at the top of the file (`rawconfig.go:13-58`) and reproduced here:

```text
router bgp <asn>                    <- genDefaultVRFSection, only when the default VRF router has neighbors
 address-family ipv4 unicast        <- genUnicastSection
  neighbor <ip|iface> allowas-in origin
 exit-address-family
 address-family ipv6 unicast
  neighbor <ip|iface> allowas-in origin
 exit-address-family
 address-family l2vpn evpn          <- genDefaultVRFEVPNSection, only when any selected network has a MAC-VRF or IP-VRF
  neighbor <ip|iface> activate
  neighbor <ip|iface> allowas-in origin
  advertise-all-vni
  vni <id>                          <- one per MAC-VRF, only when its routeTarget is set
   route-target import <rt>
   route-target export <rt>
  exit-vni
 exit-address-family
exit
!
vrf <name>                          <- genVRFVNISection, one per IP-VRF
 vni <id>
exit-vrf
!
router bgp <asn> vrf <name>         <- genNonDefaultVRFSection, one per non-default VRF with an ASN
 address-family ipv4 unicast        <- only when the VRF router has neighbors (VRF-Lite)
  neighbor <ip|iface> allowas-in origin
 exit-address-family
 address-family ipv6 unicast
  neighbor <ip|iface> allowas-in origin
 exit-address-family
 address-family l2vpn evpn          <- genNonDefaultVRFEVPNSection, only when the network has an IP-VRF
  advertise ipv4 unicast            <- only when the network has IPv4 subnets
  advertise ipv6 unicast            <- only when the network has IPv6 subnets
  route-target import <rt>          <- only when routeTarget is set
  route-target export <rt>
 exit-address-family
exit
!
```

Properties worth knowing:

- Neighbors are sorted by identifier so the output is deterministic
  (`rawconfig.go:64`, `:90-92`); a non-deterministic string would cause a
  spurious `Update` on every reconcile.
- Every token written was validated by a constructor in `validation.go`
  (`frrNeighbor`, `vrfConfig`, `ipVRFConfig`); the raw config is never built
  from unvalidated strings. This is the mitigation for FRR configuration
  injection through a crafted NAD or FRRConfiguration.
- Unnumbered neighbors (interface only) appear in both the IPv4 and the IPv6
  unicast sections (`rawconfig.go:165-185`).
- `allowas-in origin` is emitted for every neighbor, unconditionally
  (`rawconfig.go:238-241`). It is a no-op for iBGP and required for eBGP
  peers that share our ASN, for example a route reflector re-advertising a
  node's own routes to another node.
- `advertise-all-vni` makes FRR pick up every VNI that zebra sees on the
  VXLAN device, so VNIs do not have to be listed; the explicit `vni` section
  exists only to override the auto-derived route target.

---

## FRR features relied upon

| FRR concept | Where used | Reference |
|---|---|---|
| Per-VRF BGP instances (`router bgp <asn> vrf <name>`) | VRF-Lite and IP-VRF routers | <https://docs.frrouting.org/en/latest/bgp.html#vrf-route-leaking> |
| `import vrf` route leaking | `targetVRF` not `auto` | same section |
| `allowas-in origin` | all neighbors | <https://docs.frrouting.org/en/latest/bgp.html#clicmd-neighbor-PEER-allowas-in-1-10-origin> |
| `address-family l2vpn evpn`, `advertise-all-vni`, `vni` / `route-target` | EVPN underlay | <https://docs.frrouting.org/en/latest/evpn.html> |
| `vrf X` / `vni N` binding (L3VNI) and `advertise ipv4|ipv6 unicast` (Type-5 export) | IP-VRF | <https://docs.frrouting.org/en/latest/evpn.html#l3-vni> |
| Single VXLAN Device with VNI filtering (zebra reads VLAN-to-VNI tunnel mappings from the bridge) | node datapath | <https://docs.frrouting.org/en/latest/evpn.html#single-vxlan-device-svd>, requires FRR 10+ per `evpn.md` prerequisites |
| RFC 8950 next hops for unnumbered sessions | family handling of interface neighbors | <https://datatracker.ietf.org/doc/html/rfc8950> |
| Routes installed with `proto bgp` (`RTPROT_BGP`) | route import | [13 — Route import](13-ovnkube-controller-route-import.md) |

---

## Known constraints of the contract

- **No typed EVPN API in frr-k8s yet.** Everything EVPN is raw config; the
  feature doc marks this as an implementation detail that may change
  (`evpn.md`, "FRR Configuration" note). A typed API would replace
  `rawconfig.go` and the `Raw` assignment at `controller.go:1744-1748`.
- **`dualStackAddressFamily` is unsupported** because FRR may pick the IPv6
  masquerade address as next hop for IPv6 prefixes over an IPv4 session
  (`controller.go:1413-1421`).
- **Generated objects cannot be selected as templates**, enforced by name
  prefix, so a label mistake cannot create a feedback loop.
- **Generated object count** is RA x node x matching source; see
  [18 — Performance and scale](18-performance-and-scale.md).
- The source object's `nodeSelector` is honoured when choosing which nodes
  get a derived object, but the derived object always selects exactly one
  node by `kubernetes.io/hostname`; frr-k8s therefore sees one more object
  per node per RA per source.

---

## Reading frr-k8s state when debugging

```bash
kubectl -n frr-k8s-system get frrconfigurations
kubectl -n frr-k8s-system get frrconfigurations -l k8s.ovn.org/route-advertisements=<ra> \
  -o custom-columns='NAME:.metadata.name,KEY:.metadata.annotations.k8s\.ovn\.org/route-advertisements'
kubectl get frrnodestates <node> -o jsonpath='{.status.lastConversionResult} {.status.lastReloadResult}'
kubectl get frrnodestates <node> -o jsonpath='{.status.runningConfig}'
kubectl get bgpsessionstates -A
P=$(kubectl -n frr-k8s-system get pod -o name --field-selector spec.nodeName=<node> | head -1)
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show bgp vrf all summary"
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show bgp l2vpn evpn"
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show ip route vrf <vrf>"
```

`lastConversionResult` failing means the merged objects are inconsistent
(typically a neighbor declared twice with different ASNs);
`lastReloadResult` failing means FRR rejected the rendered file, which for
OVN-Kubernetes almost always points at the raw block.
