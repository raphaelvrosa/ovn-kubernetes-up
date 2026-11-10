# 01 — Concepts: VRFs, BGP, EVPN, FRR and frr-k8s

The minimum networking theory needed to read the rest of this set, mapped
onto the OVN-Kubernetes objects that use it. For a hands-on version of the
same material, captured on a live kind cluster, read
[perf-scale/bgp-evpn/00-background.md](../../perf-scale/bgp-evpn/00-background.md).

## Contents

1. [The four layers](#the-four-layers)
2. [Linux VRFs](#linux-vrfs)
3. [BGP in one page](#bgp-in-one-page)
4. [FRR and frr-k8s](#frr-and-frr-k8s)
5. [Route import versus route export](#route-import-versus-route-export)
6. [The three advertisement shapes](#the-three-advertisement-shapes)
7. [EVPN](#evpn)
8. [Single VXLAN Device (SVD)](#single-vxlan-device-svd)
9. [Symmetric IRB](#symmetric-irb)
10. [No-overlay](#no-overlay)
11. [Gateway modes and why they matter here](#gateway-modes-and-why-they-matter-here)
12. [Standards and further reading](#standards-and-further-reading)

---

## The four layers

| Layer | Objects | Lives in | Question answered |
|---|---|---|---|
| Intent | `ClusterUserDefinedNetwork`, `RouteAdvertisements`, `VTEP`, `FRRConfiguration` | Kubernetes API | What should be isolated, advertised, and how |
| Overlay | logical switches, routers, static routes, NAT, ACLs | OVN nbdb | How pods reach each other and the gateway inside the cluster |
| Host | VRF devices, routes, rules, nftables, bridge, VXLAN, SVIs | node kernel | How traffic for a network leaves the node and stays separated |
| Fabric | BGP sessions, routes, EVPN routes | FRR on the node, the provider's routers | How the outside learns about the network |

Keep them apart when reading code: the cluster manager works on the intent
layer and emits more intent (`FRRConfiguration`); ovnkube-controller works on
the overlay; ovnkube-node works on the host; FRR works on the fabric.

---

## Linux VRFs

A VRF (Virtual Routing and Forwarding instance) is a routing table plus a
master device. Interfaces enslaved to the VRF device use that table through
the `l3mdev` policy routing rule; sockets bound to the device do too. Two
networks with the same addresses can coexist because each has its own table.

OVN-Kubernetes creates one VRF per primary UDN or CUDN on every node where
the network is rendered, regardless of BGP
([15](15-ovnkube-node-vrf-and-host-datapath.md#vrf-creation-and-the-vrf-manager)).
The VRF name is the CUDN name when it fits in 15 bytes, otherwise
`mp<id>-udn-vrf` (`util.GetNetworkVRFName`); the CUDN publishes it in
`status.vrfName`. The default network has no VRF and uses the main table.

FRR references VRFs by name in `router bgp <asn> vrf <name>`, binds an L3VNI
to a VRF with `vrf <name> / vni <n>`, and installs learned routes into the
VRF's table.

Reference: <https://docs.kernel.org/networking/vrf.html>.

---

## BGP in one page

BGP (RFC 4271) exchanges prefixes between *speakers*. Each speaker has an
Autonomous System Number; a session between speakers in the same AS is iBGP,
between different ASes eBGP. Each prefix carries attributes; the ones that
matter here:

| Attribute | Used for |
|---|---|
| next hop | the address traffic for the prefix is sent to; in OVN-Kubernetes normally the advertising node's IP, or a DPU host address in DPU mode |
| AS path | loop prevention; `allowas-in origin` tells FRR to accept a route whose path starts with our own AS, needed when a route reflector or eBGP peer shares the AS |
| route target (extended community) | VPN membership for EVPN and L3VPN routes |

Address families (AFI/SAFI) separate what a session carries: `ipv4 unicast`,
`ipv6 unicast`, `l2vpn evpn`. One session can carry several; OVN-Kubernetes
activates `l2vpn evpn` on the same session that carries unicast
([06](06-frrconfiguration-contract.md#raw-frr-configuration)).

What BGP does **not** do: it does not create interfaces, VRFs or tunnels. It
only exchanges routes and, with EVPN, MAC and VTEP information. Everything
else is the job of zebra (FRR's kernel interface) and of OVN-Kubernetes.

---

## FRR and frr-k8s

FRR (FRRouting) is the routing suite; `bgpd` speaks BGP, `zebra` programs
the kernel (routes, and for EVPN also FDB and neighbor entries and VXLAN
tunnel endpoints). frr-k8s (from the MetalLB project) runs FRR as a
DaemonSet and renders its configuration from `FRRConfiguration` custom
resources: every object whose `nodeSelector` matches the node is merged into
one configuration, the result is validated, written and reloaded.

OVN-Kubernetes is a client of frr-k8s: it reads user-authored
`FRRConfiguration`s as templates and writes derived ones. It never runs FRR
itself, never calls `vtysh`, and never reads FRR state back. The whole
contract is in [06](06-frrconfiguration-contract.md).

---

## Route import versus route export

| Direction | Who configures | Mechanism |
|---|---|---|
| **Import** (fabric to cluster) | the administrator, with `toReceive` on `FRRConfiguration` neighbors | FRR installs routes into the node's main table or a VRF table; in shared gateway mode the route import controller copies them into the gateway router ([13](13-ovnkube-controller-route-import.md)); in local gateway mode the kernel uses them directly |
| **Export** (cluster to fabric) | `RouteAdvertisements` | the cluster manager generates `FRRConfiguration`s with `prefixes` and `toAdvertise` ([07](07-cm-routeadvertisements-controller.md)) |

The one exception to "import is manual": when a CUDN is advertised in the
default VRF, the generated objects also leak the default VRF's routes into
the CUDN's VRF and back (`imports`), and for no-overlay networks they add a
`toReceive` filter for the pod subnets of the other nodes.

---

## The three advertisement shapes

| Shape | `targetVRF` | Session used | Tenant separation | Gateway modes |
|---|---|---|---|---|
| **Default VRF** | unset or `default` | the default VRF session(s) of the template | none on the wire: all advertised subnets share the fabric routing domain; isolation is enforced in OVN and nftables | both |
| **VRF-Lite** | `auto` (no EVPN) | one session **per VRF**, configured by the administrator on a router with `vrf: <name>` in the template | a separate session and a separate link per tenant | local; shared only with the Uplink feature |
| **EVPN** | `auto` or the network's VRF | the default VRF session with `l2vpn evpn` activated | route targets and VNIs | local |

Pick the shape by what the fabric offers: a plain IP fabric gets the first,
a VRF-aware provider edge gets VRF-Lite, an EVPN fabric gets EVPN.

---

## EVPN

EVPN (RFC 7432, over VXLAN per RFC 8365) is the BGP address family
`l2vpn evpn`. It carries, over one session, both layer-2 and layer-3
information for many tenants. Key ideas:

| Term | Meaning | In OVN-Kubernetes |
|---|---|---|
| VNI | 24-bit VXLAN network identifier | `spec.network.evpn.macVRF.vni` / `ipVRF.vni` |
| MAC-VRF | an L2 domain (bridge table) identified by an L2 VNI | a Layer2 CUDN with `macVRF` |
| IP-VRF | an L3 domain (routing table) identified by an L3 VNI | a Layer3 CUDN, or the `ipVRF` of a Layer2 CUDN; bound to the Linux VRF |
| Route distinguisher (RD) | makes identical prefixes from different tenants unique | auto-derived by FRR |
| Route target (RT) | which VRF imports a route | `routeTarget`, default `<ASN>:<VNI>` |
| Type-2 route | MAC, or MAC plus IP, of one endpoint | one or two per **pod IP**, originated by zebra from the FDB and neighbor entries the node programs |
| Type-3 route | "this VTEP participates in this VNI", drives BUM replication | one per VNI per node |
| Type-5 route | an IP prefix in an IP-VRF | one per host subnet per node (Layer3), the whole subnet (Layer2 with IP-VRF) |
| VTEP | the VXLAN tunnel endpoint address of a node | the `VTEP` CRD and the `k8s.ovn.org/vteps` node annotation |

The property with the largest consequence: **Type-2 routes scale with pods,
not with nodes**. Every pod IP becomes a route in the fabric.

---

## Single VXLAN Device (SVD)

Linux can run one VXLAN device per VNI (multiple VXLAN devices) or one
device for all VNIs with `external` (flow based) mode and `vnifilter`, where
the VLAN tag on a VLAN-aware bridge selects the VNI. OVN-Kubernetes uses the
second: one bridge `evbr-<vtep>` and one VXLAN device `evx4-<vtep>` per
VTEP, one VLAN ID per MAC-VRF and per IP-VRF
([16](16-ovnkube-node-evpn-datapath.md#the-device-model)). Because VLAN IDs
are 12 bits, at most 4094 VRFs of both kinds together can exist per VTEP; the
cluster manager allocates the IDs cluster-wide
([10](10-cm-cudn-transport-and-no-overlay.md#evpn-vid-allocation-and-vni-reservation)).

FRR must be version 10 or later for SVD (`evpn.md` prerequisites).

Reference: <https://docs.frrouting.org/en/latest/evpn.html>, section
"Single VXLAN device".

---

## Symmetric IRB

Integrated routing and bridging: an L2 domain with its own L3 VNI so that
routed traffic between two MAC-VRFs or to the outside is encapsulated with
the IP-VRF VNI and the remote router MAC, rather than bridged. OVN-Kubernetes
Layer2 networks with both `macVRF` and `ipVRF` work this way: the L2 SVI
`svl2-<net>` anchors neighbor entries, the L3 SVI `svl3-<net>` carries the
L3 VNI, both are enslaved to the network's VRF, and FRR advertises the VRF's
unicast routes as Type-5 (`advertise ipv4 unicast`).

---

## No-overlay

Not a BGP mechanism, but a consumer of it: a Layer3 network whose inter-node
pod traffic is routed natively by the underlay instead of being tunnelled
over Geneve. It requires every node to advertise its host subnet and accept
every other node's, either from an external fabric (unmanaged routing) or
from a full mesh of iBGP sessions the cluster manager generates (managed
routing, [08](08-cm-managed-bgp-controller.md)). OVN drops the interconnect
transit switch for such networks ([14](14-ovnkube-controller-evpn-topology.md)).

---

## Gateway modes and why they matter here

| | Local gateway | Shared gateway |
|---|---|---|
| Pod egress path | OVN -> management port -> host stack -> VRF table -> physical interface | OVN -> gateway router -> external bridge |
| Who applies learned routes | the kernel | OVN gateway router, through route import |
| Ingress for advertised prefixes | bridge flow to `LOCAL`, then ip rule into the VRF | bridge flow to the network's patch port |
| VRF-Lite | yes | only with Uplink enslavement |
| EVPN | yes | no (the SVIs are only on the host path); `IsEVPNEnabled()` requires local |
| Hardware offload | no | yes |

The datapath docs ([12](12-ovnkube-controller-advertised-networks.md),
[15](15-ovnkube-node-vrf-and-host-datapath.md)) call out the mode in every
table.

---

## Standards and further reading

| Reference | Why |
|---|---|
| [RFC 4271](https://datatracker.ietf.org/doc/html/rfc4271) BGP-4 | the base protocol |
| [RFC 4364](https://datatracker.ietf.org/doc/html/rfc4364) BGP/MPLS IP VPNs | where RD and RT come from |
| [RFC 4360](https://datatracker.ietf.org/doc/html/rfc4360) extended communities | the route target encoding validated by `RouteTargetString` |
| [RFC 7432](https://datatracker.ietf.org/doc/html/rfc7432) EVPN | route types 1 to 4, MAC mobility |
| [RFC 8365](https://datatracker.ietf.org/doc/html/rfc8365) EVPN over VXLAN | what is deployed |
| [RFC 9136](https://datatracker.ietf.org/doc/html/rfc9136) IP prefix advertisement in EVPN | Type-5 |
| [RFC 7348](https://datatracker.ietf.org/doc/html/rfc7348) VXLAN | encapsulation, port 4789 |
| [RFC 8950](https://datatracker.ietf.org/doc/html/rfc8950) IPv4 NLRI with IPv6 next hop | why unnumbered neighbors carry both families |
| [FRR BGP](https://docs.frrouting.org/en/latest/bgp.html), [FRR EVPN](https://docs.frrouting.org/en/latest/evpn.html) | every keyword in the raw config |
| [frr-k8s](https://github.com/metallb/frr-k8s) | `FRRConfiguration` semantics, merge rules, `FRRNodeState`, `BGPSessionState` |
| [Linux VRF](https://docs.kernel.org/networking/vrf.html), [Linux VXLAN](https://docs.kernel.org/networking/vxlan.html) | kernel behaviour the node relies on |
| `docs/okeps/okep-5296-bgp.md`, `docs/okeps/okep-5088-evpn.md`, `docs/okeps/okep-5259-no-overlay.md`, `docs/okeps/okep-6019-vrf-lite-shared-gateway-external-bridges.md` | design rationale and alternatives |
