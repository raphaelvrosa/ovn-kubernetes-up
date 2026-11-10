# 14 — ovnkube-controller: the OVN logical topology of EVPN and no-overlay networks

An EVPN or no-overlay network keeps the per-node OVN topology of its
topology type (Layer2 or Layer3) but drops everything that exists only to
carry east/west traffic over the Geneve interconnect. For EVPN Layer2
networks a dedicated logical switch port hands unknown-destination traffic
to the Linux EVPN bridge. This page lists those differences and the code
that implements them.

## Contents

1. [The single switch: `hasInterconnectTransport`](#the-single-switch-hasinterconnecttransport)
2. [What is not created](#what-is-not-created)
3. [Layer2 EVPN: the MAC-VRF port](#layer2-evpn-the-mac-vrf-port)
4. [Layer2 EVPN: gateway ARP and NS suppression ACLs](#layer2-evpn-gateway-arp-and-ns-suppression-acls)
5. [Layer2 EVPN: multicast settings](#layer2-evpn-multicast-settings)
6. [Layer3 EVPN and no-overlay](#layer3-evpn-and-no-overlay)
7. [How the port gets bound: the node side](#how-the-port-gets-bound-the-node-side)
8. [Packet walks](#packet-walks)
9. [Review notes](#review-notes)

---

## The single switch: `hasInterconnectTransport`

```go
// go-controller/pkg/ovn/base_network_controller.go:991-998
func (bnc *BaseNetworkController) hasInterconnectTransport() bool {
	switch bnc.Transport() {
	case types.NetworkTransportEVPN, types.NetworkTransportNoOverlay:
		return false
	default:
		return true
	}
}
```

and its Layer2 companion `isLayer2WithInterconnectTransport()`. Every site
that creates interconnect state checks one of the two.

---

## What is not created

| Resource | Overlay | EVPN / no-overlay | Code |
|---|---|---|---|
| Transit switch configuration on the Layer2 switch (`AddTransitSwitchConfig`) | yes | no | `base_secondary_layer2_network_controller.go:191-202` |
| Transit router for Layer2 (`newClusterRouter` with `Layer2UsesTransitRouter`) | transit router | plain cluster router | `layer2_user_defined_network_controller.go:1136-1145` |
| Remote node interconnect setup (transit switch ports, remote chassis routes) | yes | no | `layer2_user_defined_network_controller.go:1012-1016`, `:790`; `layer3_user_defined_network_controller.go:943`, `:978`, `:1083`, `:1158` |
| Remote pod logical switch ports on Layer2 | yes (`requiresLogicalPort`) | no | `base_network_controller_user_defined.go:346-348`, `:600-606`, `:1091` |
| Tunnel key reconciliation for Layer2 | yes | skipped | `layer2_user_defined_network_controller.go:534-540` |
| Gateway router route for the whole cluster subnet to the distributed router (Layer3) | yes | only this node's host subnet (no-overlay) | `gateway.go:545-615`, see [12](12-ovnkube-controller-advertised-networks.md#no-overlay-gateway-router-routes) |

The gateway router per node, the join switch, the external switch and the
management port are unchanged: north/south still goes through the gateway
router, and in local gateway mode through the management port into the host.

---

## Layer2 EVPN: the MAC-VRF port

`initializeLogicalSwitch` (`base_secondary_layer2_network_controller.go:160-266`)
adds, for `Transport() == evpn`, a logical switch port on the network's single
Layer2 switch:

```go
// base_secondary_layer2_network_controller.go:209-220
macvrfportName := util.GetMACVRFPortName(switchName)          // "macvrf-" + switch name (types.MACVRFPortPrefix)
macvrfport := &nbdb.LogicalSwitchPort{
	Name:      macvrfportName,
	Addresses: []string{"unknown"},
	ExternalIDs: map[string]string{
		types.NetworkExternalID:  oc.GetNetworkName(),
		types.TopologyExternalID: oc.TopologyType(),
	},
}
```

`addresses: ["unknown"]` makes OVN forward every frame whose destination MAC
is not known on the switch to this port. Since remote pods have no logical
switch port, their MACs are unknown to OVN, and the frame leaves through the
MAC-VRF port into the EVPN bridge, where the Linux FDB (populated by FRR from
Type-2 routes) picks the VXLAN tunnel. External hosts in the same MAC-VRF are
reached the same way.

The port is bound on the node by setting `iface-id` of the OVS internal
port `ovl2-<network>` to this name
(`node/controllers/evpn/evpn_node_controller.go:618-623`).

---

## Layer2 EVPN: gateway ARP and NS suppression ACLs

`getDenyARPAndNSOnMACVRF` (`:274-321`) adds two drop ACLs on the switch,
tier default, priority `types.DefaultDenyPriority`, direction `to-lport`:

| Name | Match |
|---|---|
| `DenyOnMACVRF-GatewayARP` | `outport=="<macvrf port>" && eth.dst==<gateway MAC> && arp && arp.op==1 && arp.tpa==<gateway IPv4>` |
| `DenyOnMACVRF-GatewayNS` | `outport=="<macvrf port>" && eth.dst==<gateway MAC> && nd && icmp.type==135 && nd.target==<gateway IPv6>` |

OVN floods unicast ARP requests for historic reasons; without these ACLs a
pod's ARP for its gateway would be flooded into the fabric. The gateway is
only reachable locally, so answering from the fabric would be wrong.

---

## Layer2 EVPN: multicast settings

Set on the logical switch for EVPN transport
(`base_secondary_layer2_network_controller.go:204-207`):

```text
other_config: mcast_snoop=true  mcast_flood_unregistered=true  mcast_querier=false
```

and, when multicast is enabled, the MAC-VRF port is added to the cluster
router port group (`:241-258`) so the `AllowInterNode` multicast ACL permits
traffic to and from the fabric. The feature doc explains the resulting
behaviour in [evpn.md, "Multicast"](../../features/bgp-integration/evpn.md#multicast).

---

## Layer3 EVPN and no-overlay

For Layer3 there is no MAC-VRF: each node's switch is a separate broadcast
domain. East/west traffic between nodes therefore leaves the pod's node
switch through the cluster router and the gateway router like north/south
traffic, exits to the host (local gateway mode) and is routed by the kernel
inside the network's VRF:

- **EVPN**: the IP-VRF SVI (`svl3-<network>`) is enslaved to the VRF, FRR
  binds the VRF to the L3VNI (`vrf X / vni N`) and originates Type-5 routes
  for the VRF's prefixes; remote host subnets arrive as Type-5 routes and are
  installed as kernel routes in the VRF pointing at the SVI
  ([16](16-ovnkube-node-evpn-datapath.md)).
- **no-overlay**: remote host subnets arrive as plain BGP unicast routes in
  the default VRF (or the network's VRF with VRF-Lite); in shared gateway
  mode they are imported into the gateway router by
  [13](13-ovnkube-controller-route-import.md) (pod subnets are **not**
  ignored for no-overlay), in local gateway mode the kernel routes them.

The OVN side of both is therefore "Layer3 topology without interconnect".

---

## How the port gets bound: the node side

```mermaid
sequenceDiagram
    participant ZC as ovnkube-controller
    participant NB as nbdb
    participant EN as EVPN node controller
    participant OVS as ovs-vswitchd
    participant OC as ovn-controller
    ZC->>NB: LSP macvrf-<switch> addresses=[unknown]
    EN->>OVS: port ovl2-<net> on br-int, external_ids:iface-id=macvrf-<switch>, evpn-vtep=<vtep>
    EN->>EN: ip link set ovl2-<net> master evbr-<vtep>; bridge vlan add vid <macvrf vid> pvid untagged
    OC->>OVS: bind LSP to the port (iface-id match)
```

The zone and node controllers never coordinate directly; the LSP name is a
deterministic function of the switch name on both sides.

---

## Packet walks

**Pod A (node 1) to pod B (node 2), Layer2 EVPN MAC-VRF:**

1. OVN on node 1 has no LSP for B, so the frame is output on the MAC-VRF
   port.
2. The frame enters `ovl2-<net>` on `evbr-<vtep>` with the MAC-VRF VLAN.
3. The bridge FDB has B's MAC (from a Type-2 route, installed by zebra on
   the VXLAN port with the remote VTEP as destination), so the frame goes to
   `evx4-<vtep>`, is encapsulated with the MAC-VRF VNI and sent to node 2's
   VTEP IP.
4. On node 2 the VXLAN device maps the VNI to the VLAN, the bridge forwards
   to `ovl2-<net>` (B's MAC is static there, programmed by the EVPN pod
   controller) and OVN delivers to B's LSP.

ARP from A for B is answered by the bridge's neighbor suppression
(`neigh_suppress on`) from the Type-2 MAC/IP route, without flooding.

**Pod A (node 1) to external 10.1.0.5 via IP-VRF, Layer3 EVPN:**

1. OVN routes to the gateway router, which (local gateway mode) sends to the
   management port; the kernel receives in VRF `<net>`.
2. The VRF table has a Type-5 derived route `10.1.0.0/24 via <remote VTEP>
   dev svl3-<net>`; the packet is encapsulated with the IP-VRF VNI and the
   remote router MAC.
3. Return traffic arrives on the L3 VNI, is routed in VRF `<net>` to the
   management port and OVN delivers it to A. No SNAT is applied because the
   network is advertised.

---

## Review notes

- Any new interconnect resource must be gated with `hasInterconnectTransport`
  or it will be created for EVPN and no-overlay networks and never used (and
  may leak Geneve tunnel keys).
- The `unknown` address on the MAC-VRF port means **every** unknown unicast
  leaves the switch; a test that expects isolation between a pod and an
  unknown MAC must account for the fabric path.
- The ACL names `DenyOnMACVRF-GatewayARP/NS` are keyed per controller name;
  deleting a network must remove them (they are attached to the switch, so
  they go with it).
