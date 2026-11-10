# 17 — End-to-end workflows

Each workflow lists, in order, every controller that acts, the object or
kernel state it changes, and the code that does it. Use them to trace a bug
report or to predict the blast radius of a change. Cross-references point to
the detailed pages.

## Contents

1. [W1: Advertise the default network](#w1-advertise-the-default-network)
2. [W2: Advertise a Layer3 CUDN in the default VRF](#w2-advertise-a-layer3-cudn-in-the-default-vrf)
3. [W3: VRF-Lite CUDN](#w3-vrf-lite-cudn)
4. [W4: EVPN CUDN from scratch](#w4-evpn-cudn-from-scratch)
5. [W5: A pod is created on an EVPN MAC-VRF network](#w5-a-pod-is-created-on-an-evpn-mac-vrf-network)
6. [W6: A route is learned from the fabric](#w6-a-route-is-learned-from-the-fabric)
7. [W7: A node joins or leaves](#w7-a-node-joins-or-leaves)
8. [W8: A RouteAdvertisements is deleted](#w8-a-routeadvertisements-is-deleted)
9. [W9: No-overlay with managed routing, cluster bring-up](#w9-no-overlay-with-managed-routing-cluster-bring-up)
10. [W10: Controller restarts](#w10-controller-restarts)
11. [W11: Validation failure and recovery](#w11-validation-failure-and-recovery)

---

## W1: Advertise the default network

Precondition: frr-k8s deployed; an `FRRConfiguration` `receive-all` with a
default-VRF router peering with the fabric, labelled `name: receive-all`.

```mermaid
sequenceDiagram
    autonumber
    participant U as user
    participant API as apiserver
    participant RC as RA controller (cluster manager)
    participant NM as network manager (zone + node)
    participant ZC as ovnkube-controller
    participant ND as ovnkube-node
    participant FK as frr-k8s
    U->>API: RouteAdvertisements default {DefaultNetwork, PodNetwork, nodeSelector {}, selector name=receive-all}
    API-->>RC: RA add (raNeedsUpdate: generation)
    RC->>RC: getSelectedNADs -> EnsureDefaultNetworkNAD; nodes; templates
    RC->>RC: per node: prefixes = node-subnets annotation for "default"
    RC->>API: N x FRRConfiguration ovnk-generated-* (router asn, neighbors with toAdvertise=host subnet)
    RC->>API: NAD ovn-kubernetes/default annotation k8s.ovn.org/route-advertisements=["default"]
    RC->>API: RA status Accepted=True
    API-->>FK: merge receive-all + generated, reload FRR; node advertises its /24
    API-->>NM: NAD annotation change -> syncNetwork(default) -> setAdvertisements
    NM->>ZC: Reconcile: advertised flipped for local node -> node gateway re-sync
    ZC->>ZC: cluster subnet SNAT gets match ip4.dst == $node-ips (gateway.go:1115-1140)
    NM->>ND: Reconcile: SetDefaultPodNetworkAdvertised(true); Gateway.Reconcile
    ND->>ND: bridge flows priority 300/104 (bridgeflows.go:1143-1170); nftables masquerade only to @remote-node-ips (gateway_nftables.go:873-877)
```

Result: external hosts reach pod IPs directly; pod egress keeps its pod IP.
Checks: `kubectl get ra`, `kubectl -n frr-k8s-system get frrconfigurations`,
`vtysh -c "show bgp ipv4"` on a node, `ovs-ofctl dump-flows breth0 | grep priority=300`.

---

## W2: Advertise a Layer3 CUDN in the default VRF

Same as W1 with these differences:

| Step | Difference | Code |
|---|---|---|
| selection | NADs owned by the CUDN and matching the RA's label selector (`controller.go:2178-2193`) | |
| generation | the matched default-VRF router gets `imports: [{vrf: <cudn>}]` and an extra router `vrf: <cudn>, imports: [{vrf: default}]` (`controller.go:1494-1531`) | routes learned in the default VRF leak into the CUDN VRF and back |
| zone | SNAT to the masquerade IP with the node-IP conditional, isolation pass ACL on the node switch, subnet in the global advertised set, stor port in the drop port group (`udn_isolation.go:482-565`) | `strict` only |
| node | VRF keeps its default route; ip rule `to <subnet> lookup <table>` added (`gateway_udn.go:1532-1535`); subnets added to `advertised-udn-subnets-v4/v6` (`:1945-1969`); bridge flows for the UDN patch port | |
| CUDN status | no `TransportAccepted` (transport is overlay) | |

---

## W3: VRF-Lite CUDN

Precondition: an `FRRConfiguration` with a router `vrf: <cudn>` and a
neighbor reachable from inside the VRF; an interface enslaved to the VRF by
the administrator (or an Uplink).

| Step | What happens | Code |
|---|---|---|
| RA with `targetVRF: auto` | only the `vrf: <cudn>` router is matched; `matchedNetworks` must cover every active network per node (`controller.go:1016-1019`) | |
| generated object | one router with the CUDN VRF, prefixes = host subnets; raw config adds `allowas-in origin` under `router bgp <asn> vrf <cudn>` (`rawconfig.go:138-162`) | no imports |
| node | `isNetworkAdvertised && !isNetworkAdvertisedToDefaultVRF`: managed default routes removed from the VRF (`gateway_udn.go:1844-1849`), subnet ip rules deleted, service marks disabled, bridge flows send ingress to `LOCAL` | traffic can only follow BGP routes in the VRF |
| Uplink variant | the Uplink gateway interface is enslaved to the VRF (`gateway_udn.go:1753-1817`) | shared gateway mode support |

Failure signature when the template has no VRF router: RA `Not Accepted:
... has no VRF matching the RouteAdvertisements target VRF or any selected network`.

---

## W4: EVPN CUDN from scratch

Preconditions: `-evpn`, local gateway mode, frr-k8s with FRR 10+, a fabric
peer with `l2vpn evpn` activated.

```mermaid
sequenceDiagram
    autonumber
    participant U as user
    participant API as apiserver
    participant VN as VTEP controller (CM)
    participant EN as EVPN node controller
    participant CC as CUDN controller (CM)
    participant RC as RA controller (CM)
    participant ZC as ovnkube-controller
    participant FK as frr-k8s / FRR
    U->>API: VTEP evpn-vtep {cidrs: [100.64.0.0/24], mode: Unmanaged}
    VN->>API: finalizer; Accepted=False AllocationFailed
    EN->>EN: discoverUnmanagedVTEPIPs: node address in CIDR
    EN->>API: node annotation k8s.ovn.org/vteps={"evpn-vtep":{"ips":["100.64.0.1"]}}
    EN->>EN: ensure evbr-evpn-vtep, evx4-evpn-vtep (no mappings yet)
    API-->>VN: node annotation changed -> ReconcileAll
    VN->>API: Accepted=True Allocated
    U->>API: RouteAdvertisements evpn {targetVRF: auto, selector evpn=enabled, template with default-VRF router}
    RC->>API: Accepted=False ConfigurationPending: no networks selected
    U->>API: CUDN blue {Layer2 primary, transport EVPN, evpn{vtep, macVRF vni 100, ipVRF vni 101}, label evpn=enabled}
    CC->>CC: validateEVPN (flag, local gw, VTEP accepted); allocate VIDs 2,3; reserve VNIs
    CC->>API: NAD with transport=evpn, evpn{...vid}; TransportAccepted=False ...IsMissing
    API-->>RC: NAD add -> ReconcileAll
    RC->>API: per node FRRConfiguration: default router + VTEP /32, vrf blue router (ASN inherited), raw l2vpn evpn + vrf blue vni 101
    RC->>API: NAD annotation ["evpn"]; RA Accepted=True
    API-->>CC: RA notifier -> TransportAccepted=True EVPNTransportAccepted
    API-->>FK: reload: advertise-all-vni, Type-3 for VNI 100, Type-5 for 10.0.10.0/24 in RT 65000:101
    API-->>EN: NAD reconciler -> reconcile VTEP: VID/VNI mappings 2->100, 3->101; svl2-blue, svl3-blue in VRF blue; ovl2-blue on br-int iface-id macvrf-<switch>
    API-->>ZC: NAD -> Layer2 controller: switch with macvrf port (addresses unknown), ARP/NS deny ACLs, mcast settings; no transit switch
    ZC->>ZC: node sync: SNAT conditional, isolation ACLs
```

Checks: `kubectl get vtep`, CUDN `TransportAccepted`,
`bridge vlan tunnelshow`, `vtysh -c "show evpn vni"`,
`vtysh -c "show bgp l2vpn evpn"`.

---

## W5: A pod is created on an EVPN MAC-VRF network

| Step | Component | Code |
|---|---|---|
| 1 | CNI / ovnkube-controller allocate IP and MAC, write `k8s.ovn.org/pod-networks`, create the LSP on the Layer2 switch | standard pod add |
| 2 | EVPN pod controller sees the annotation (`podNeedsUpdate`), resolves the primary NAD, builds `neighEntries` | `evpn_pod_controller.go:101-179` |
| 3 | `bridge fdb add <mac> dev ovl2-blue vlan 2 master static`; `ip neigh add <ip> lladdr <mac> dev svl2-blue nud permanent` | `:184-204` |
| 4 | zebra sees a local MAC on VNI 100 and a neighbor on the SVI; bgpd originates Type-2 `[2]:[0]:[48]:[mac]` with RT 65000:100 and `[2]:[0]:[48]:[mac]:[32]:[ip]` with RTs 65000:100 and 65000:101 | FRR |
| 5 | remote nodes install the MAC in their FDB pointing at this VTEP and the IP in their SVI neighbor table (`extern_learn`) | FRR / zebra |
| 6 | pod delete: FDB and neighbor removed, routes withdrawn | `:206-259` |

Live migration adds the source/target hand-off described in
[16](16-ovnkube-node-evpn-datapath.md#kubevirt-live-migration).

---

## W6: A route is learned from the fabric

| Step | Component | Code |
|---|---|---|
| 1 | the peer sends `172.26.0.0/16` allowed by `toReceive` on the user template | FRR |
| 2 | zebra installs `172.26.0.0/16 via <peer> proto bgp` in the main table (default VRF) and, with `imports`, in the CUDN VRF tables | FRR |
| 3 | local gateway mode: the kernel routes pod egress (coming from the management port, in the VRF) using it; nothing else to do | |
| 4 | shared gateway mode: route import gets the netlink event (`RTPROT_BGP`, table known), waits 500 ms, dumps the table, diffs against `RouteImport`-owned LRSRs on `GR_<net>_<node>`, creates the static route with `output_port rtoe-GR_...` | `route_import.go:256-266`, `:327-433` |
| 5 | ovn-northd and ovn-controller turn it into flows; pod egress to `172.26.0.0/16` uses the learned next hop | OVN |

Routes inside the network's own subnets are ignored unless the network is
no-overlay (`:358-365`).

---

## W7: A node joins or leaves

| Event | Cluster manager | Zone | Node |
|---|---|---|---|
| node add | RA controller: `nodeNeedsUpdate` (labels, addresses, subnets) -> `ReconcileAll`; once the subnet annotation exists, a generated object for the node appears (`controller.go:848-868`). VTEP controller: nothing until the annotation. Managed BGP: base template gains a neighbor | new node's own zone programs its topology | VRFs, EVPN devices, VTEP annotation |
| node delete | generated objects for the node disappear from the desired state and are deleted (`updateFRRConfigurations`); managed template loses the neighbor; VTEP claims drop | other zones remove nothing specific to BGP | other nodes' FRR withdraws the routes learned from it |
| node subnet change | `NodeSubnetAnnotationChanged` -> reconcile; prefixes updated | SNAT and isolation re-synced if advertised | |
| node address change | managed template rebuilt; self-peer filter re-evaluated; VTEP IP re-resolved on the node (`reconcileNodeAddressChange`) | | |

---

## W8: A RouteAdvertisements is deleted

1. `onRADelete` under EVPN: `ReconcileAll` so other RAs re-evaluate VTEP
   claims (`controller.go:2286-2290`).
2. `reconcile(name)` with a nil RA: `generateFRRConfigurations` returns
   empty, `updateFRRConfigurations` deletes every object labelled with the
   name, `updateNADs` removes the name from every NAD annotation, the
   metric and the network cache entry are dropped (`:400-408`, `:424-480`).
3. frr-k8s withdraws the prefixes on reload.
4. Network manager: `raNeedsUpdate` ignores the delete; the NAD annotation
   change drives `setAdvertisements`, which now finds no RA and clears the
   maps (`network_controller.go:571-660`).
5. Zone: advertised flipped -> SNAT unconditional again, isolation ACL
   removed, subnets removed from the global set.
6. Node: default route restored in the VRF (if VRF-Lite), subnet rules
   removed or added, nftables set elements removed, bridge flows rebuilt,
   services resynced.
7. CUDN controller: RA notifier (delete) re-queues every no-overlay and EVPN
   CUDN; `TransportAccepted` goes `False/...IsMissing` for those that lost
   their only RA. The networks keep running; EVPN and no-overlay networks are
   treated as advertised by the predicates regardless, so their datapath
   does not flip.

---

## W9: No-overlay with managed routing, cluster bring-up

1. Config: `[default] transport = no-overlay`, `[no-overlay] outbound-snat,
   routing = managed`, `[bgp-managed] frr-namespace`.
2. Cluster manager starts the managed BGP controller; `Start` triggers an
   initial reconcile (`managedbgp/controller.go:155-159`).
3. `ensureBaseFRRConfiguration` writes `ovnk-managed-bgp` with every node's
   InternalIPs as neighbors; `ensureManagedRouteAdvertisement(true)` writes
   `ovnk-managed-bgp-default`.
4. RA controller generates per-node objects from the managed template:
   self-peer removed, `toReceive` for the cluster subnet with
   `ge = le = hostSubnetLength`, prefixes = the node's host subnet.
5. frr-k8s establishes N-1 iBGP sessions per node; each node learns every
   other node's `/24`.
6. Zone: gateway router routes only the local host subnet to the
   distributed router (`gateway.go:555-559`); SNAT per `outbound-snat`.
7. Node: masquerade rules per `outbound-snat`
   (`gateway_nftables.go:838-866`); management port IPs without a prefix
   route for local gateway Layer3 no-overlay (`gateway_udn.go:1231-1233`).
8. Route import (shared gateway) copies the learned host subnets into the
   gateway router; pod subnets are not ignored for no-overlay.
9. The no-overlay validation controller emits `NoOverlayConfigurationReady`
   on the default NAD once the RA is accepted.

---

## W10: Controller restarts

| Process | What is rebuilt | What is preserved |
|---|---|---|
| cluster manager | every RA is reconciled from informer state; generated objects are diffed, not recreated (`updateFRRConfigurations` compares specs); VIDs recovered from NADs; VTEP finalizers re-ensured; condition metrics re-recorded from stored conditions | generated `FRRConfiguration`s, NAD annotations, statuses |
| ovnkube-controller | `ConfigureAdvertisedNetworkIsolation` and `CleanupStaleAdvertisedNetworkSubnets`; route import resubscribes with `ListExisting` and resyncs; per-node SNAT and ACLs rebuilt on node sync | nbdb rows, updated in place |
| ovnkube-node | VRF manager `Repair`; rule manager owns priority 2000 and removes unknown rules; nftables chains recreated and flushed; EVPN `initialSync` pre-populates device desired state **before** the device manager's orphan cleanup; stale OVS ports and pod entries cleaned | VRFs, EVPN devices and FRR sessions survive |

---

## W11: Validation failure and recovery

Example: a node's `k8s.ovn.org/vteps` annotation is edited to an address
outside the VTEP CIDRs.

1. RA controller: `nodeNeedsUpdate` (annotation changed) -> `ReconcileAll`.
2. `getNodeVTEPPrefixes` fails for that node; it is added to
   `invalidVTEPNodes`, generation continues for the others
   (`controller.go:808-817`).
3. The node is excluded from the desired state; `reconcileRouteAdvertisements`
   applies the sanitized state: the node's generated objects are deleted,
   the others updated (`:467-480`).
4. Status: `Accepted=False / ConfigurationError` with the node named; the
   metric flips.
5. CUDN controller: RA notifier (accepted changed) -> `TransportAccepted=False
   / EVPNRouteAdvertisementsNotAccepted` on the selected CUDNs.
6. Network manager: `raNeedsUpdate` sees not-accepted -> `preserveAdvertisements`;
   datapath state is kept.
7. EVPN node controller on that node: annotation change -> re-validate and
   rewrite the correct value (`evpn_node_controller.go:266-288`).
8. The corrected annotation triggers step 1 again; the node's object is
   recreated; statuses return to `True`.
