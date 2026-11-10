# Glossary

Terms as used in this document set and in the code. Benchmarking-specific
terms are in [perf-scale/bgp-evpn/glossary.md](../../perf-scale/bgp-evpn/glossary.md).

## OVN-Kubernetes objects and names

| Term | Meaning |
|---|---|
| **RA** | `RouteAdvertisements`, cluster-scoped CRD declaring which networks are advertised, from which nodes, using which templates. [03](03-api-routeadvertisements.md) |
| **VTEP (CRD)** | `VTEP`, cluster-scoped CRD declaring the CIDRs that VXLAN tunnel endpoint addresses come from. [04](04-api-vtep.md) |
| **CUDN** | `ClusterUserDefinedNetwork`; the only user-defined network type an RA can select. [05](05-api-cudn-transport.md) |
| **NAD** | `NetworkAttachmentDefinition`; one per namespace per network, rendered by the CUDN controller, annotated by the RA controller |
| **Template / source FRRConfiguration** | A user-authored `FRRConfiguration` selected by `spec.frrConfigurationSelector` |
| **Generated FRRConfiguration** | `ovnk-generated-*`, written by the RA controller, one per RA x node x template |
| **Managed BGP** | `[bgp-managed]` config and the `ovnk-managed-bgp*` objects created for no-overlay managed routing. [08](08-cm-managed-bgp-controller.md) |
| **`k8s.ovn.org/route-advertisements`** | Label on generated objects (RA name), annotation on generated objects (`ra/source/node`), annotation on NADs (JSON list of RA names) |
| **`k8s.ovn.org/route-advertisements-validated`** | Annotation marking generated objects built from validated input |
| **`k8s.ovn.org/vteps`** | Node annotation `{"<vtep>": {"ips": [...]}}` written by ovnkube-node |
| **`k8s.ovn.org/node-subnets`** | Node annotation with per-network host subnets, the source of Layer3 prefixes |
| **TransportAccepted** | CUDN condition tying a no-overlay or EVPN CUDN to an accepted RA. [10](10-cm-cudn-transport-and-no-overlay.md) |
| **Accepted** | Condition on RA and VTEP |
| **`targetVRF`** | RA field: `""`/`default`, `auto`, or a VRF name |
| **Advertised network** | A network for which `IsPodNetworkAdvertisedAtNode` is true on a node: selected by an accepted RA with `PodNetwork`, or any no-overlay or EVPN network |
| **Advertised UDN isolation** | `strict` or `loose`; OVN ACLs plus nftables `udn-bgp-drop` |
| **Route import** | Copying `proto bgp` kernel routes into gateway router static routes. [13](13-ovnkube-controller-route-import.md) |
| **LRSR** | `Logical_Router_Static_Route` in nbdb |
| **GR** | Gateway router, `GR_<network>_<node>` |
| **Management port** | `ovn-k8s-mp<id>`, the host interface into a network's OVN switch; the VRF's slave |
| **Masquerade IP** | Per-network `169.254.0.x` address used by OVN SNAT toward the host |
| **VRF name** | CUDN name if at most 15 bytes, else `mp<id>-udn-vrf`; published in `status.vrfName` |
| **MAC-VRF port** | `macvrf-<switch>` LSP with `addresses: [unknown]` on an EVPN Layer2 switch. [14](14-ovnkube-controller-evpn-topology.md) |
| **`evbr-`, `evx4-`, `evx6-`, `svl2-`, `svl3-`, `ovl2-`** | EVPN bridge, VXLAN devices, L2 SVI, L3 SVI, OVS internal port. [16](16-ovnkube-node-evpn-datapath.md) |
| **VID** | VLAN ID allocated cluster-wide per MAC-VRF and IP-VRF, carried in the NAD netconf |
| **NDM** | Netlink device manager, `pkg/node/netlinkdevicemanager` |
| **Network manager** | `pkg/networkmanager`, the component that turns NADs and RAs into `NetInfo` for controllers |
| **NetInfo** | `util.NetInfo`, the in-memory description of a network including advertised VRFs per node |
| **Dynamic UDN allocation** | Networks rendered only on nodes with workloads; affects which nodes advertise |
| **Uplink** | Per-network external bridge feature; affects next hops and VRF enslavement |
| **Interconnect (IC) mode** | One OVN zone per node; the only supported mode for these features |

## Networking terms

| Term | Meaning |
|---|---|
| **ASN** | Autonomous System Number |
| **iBGP / eBGP** | BGP within one AS / between ASes |
| **Route reflector** | An iBGP speaker that re-advertises routes between clients, avoiding a full mesh |
| **`allowas-in origin`** | Accept routes whose AS path starts with our own ASN |
| **AFI/SAFI, address family** | `ipv4 unicast`, `ipv6 unicast`, `l2vpn evpn` |
| **Unnumbered neighbor** | Neighbor defined by interface; session over a /30, /31 or link-local address; carries both families |
| **VRF** | Virtual routing and forwarding instance: a Linux routing table plus master device |
| **VRF-Lite** | One BGP session per VRF, no EVPN, a per-tenant link to the fabric |
| **Route leaking / imports** | `import vrf` in FRR; the `imports` field in `FRRConfiguration` routers |
| **RTPROT_BGP, `proto bgp`** | Kernel route protocol value for routes installed by bgpd through zebra |
| **EVPN** | BGP address family carrying MAC and IP reachability for VXLAN overlays, RFC 7432 / 8365 |
| **VNI** | 24-bit VXLAN network identifier |
| **MAC-VRF / IP-VRF** | EVPN L2 domain / L3 domain |
| **L2VNI / L3VNI** | VNI of a MAC-VRF / IP-VRF |
| **RD / RT** | Route distinguisher / route target |
| **Type-2 / Type-3 / Type-5** | EVPN MAC/IP route / inclusive multicast route / IP prefix route |
| **SVD** | Single VXLAN Device: one VXLAN netdev for all VNIs, VLAN selects the VNI |
| **SVI** | Switched virtual interface: a VLAN interface on the bridge |
| **IRB, symmetric IRB** | Routing between L2 domains over an L3VNI |
| **BUM** | Broadcast, unknown unicast and multicast |
| **Head-end replication** | Unicasting BUM traffic to each remote VTEP |
| **MAC mobility** | EVPN sequence number mechanism letting a MAC move between VTEPs |
| **Neighbor suppression** | Bridge answering ARP/ND from its neighbor table instead of flooding |
| **FRR, bgpd, zebra** | FRRouting suite; its BGP daemon; its kernel interface daemon |
| **frr-k8s** | MetalLB's Kubernetes operator for FRR; owns `FRRConfiguration`, `FRRNodeState`, `BGPSessionState` |
| **Provider network** | The external network the cluster peers with, as used in the feature docs |
