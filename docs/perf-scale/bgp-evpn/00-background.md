# 00 — Background: CUDNs, VRFs, BGP and EVPN

What these objects actually are, what they create on a node, which process creates them, and
how to see all of it on a running cluster.

Every command and every output below was **run against a live three-node kind cluster**. Nothing
here is illustrative pseudo-output.

## Contents

1. [How to follow along](#how-to-follow-along)
2. [The four layers](#the-four-layers)
3. [ClusterUserDefinedNetwork](#clusteruserdefinednetwork)
4. [VRFs — where, when, why and by whom](#vrfs-where-when-why-and-by-whom)
5. [VRFs in shared gateway mode](#vrfs-in-shared-gateway-mode)
6. [RouteAdvertisements — getting a network into BGP](#routeadvertisements-getting-a-network-into-bgp)
7. [VRF-Lite: one BGP session per VRF](#vrf-lite-one-bgp-session-per-vrf)
8. [EVPN — what it is](#evpn-what-it-is)
9. [How OVN-Kubernetes implements EVPN](#how-ovn-kubernetes-implements-evpn)
10. [Seeing EVPN in BGP](#seeing-evpn-in-bgp)
11. [Per-pod: the Type-2 chain](#per-pod-the-type-2-chain)
12. [Side by side](#side-by-side)
13. [Who does what](#who-does-what)
14. [Command cookbook](#command-cookbook)
15. [References](#references)

---

## How to follow along

The cluster used throughout was built with
[the local lab guide](06-local-lab.md#bring-up-the-cluster):

```bash
cd contrib
./kind.sh -rae -mne -nse -evpn -adv -gm local -wk 2
kind export kubeconfig --name ovn
```

```console
$ kubectl get nodes -o wide
NAME                STATUS   ROLES           AGE     VERSION   INTERNAL-IP
ovn-control-plane   Ready    control-plane   4m39s   v1.36.4   172.18.0.4
ovn-worker          Ready    <none>          4m25s   v1.36.4   172.18.0.3
ovn-worker2         Ready    <none>          4m25s   v1.36.4   172.18.0.2
```

Relevant addresses, referenced throughout:

| Thing | Value |
|---|---|
| kind node network | `172.18.0.0/16` |
| External FRR router | `172.18.0.5`, ASN 64512, route reflector |
| External server network | `172.26.0.0/16` |
| Default pod network | `10.244.0.0/16`, `ovn-worker` holds `10.244.1.0/24` |
| `blue` CUDN (Layer3, no EVPN) | `10.200.0.0/16`, `ovn-worker` holds `10.200.1.0/24` |
| `red` CUDN (Layer2, EVPN) | `10.210.0.0/16` |

Confirm the feature gates are on:

```console
$ kubectl -n ovn-kubernetes get deploy ovnkube-control-plane \
    -o jsonpath='{.spec.template.spec.containers[0].env}' \
    | jq -r '.[]|select(.name|test("EVPN|ROUTE_ADV|SEGMENT|GATEWAY"))|"\(.name)=\(.value)"'
OVN_NETWORK_SEGMENTATION_ENABLE=true
OVN_ROUTE_ADVERTISEMENTS_ENABLE=true
OVN_EVPN_ENABLE=true
OVN_GATEWAY_MODE=local
```

---

## The four layers

Keeping these separate is most of the battle. People conflate layers 2 and 3 constantly.

| Layer | Object | Lives in | Question it answers |
|---|---|---|---|
| **1. Intent** | `ClusterUserDefinedNetwork`, `RouteAdvertisements`, `VTEP` | Kubernetes API | "I want an isolated network, advertised, over EVPN" |
| **2. Overlay** | OVN logical switches and routers | nbdb / sbdb | How pods on this network reach each other inside the cluster |
| **3. Host** | Linux **VRF**, management port, VXLAN device, bridge, SVIs | Node kernel | How traffic for this network leaves the node and stays separated from other networks |
| **4. Fabric** | BGP sessions, `FRRConfiguration`, EVPN routes | FRR / frr-k8s | How the outside world learns about this network |

A **VRF is a layer-3 construct**: a separate Linux routing table plus a master device. It is
not an OVN concept and not a BGP concept. BGP *references* VRFs; OVN is unaware of them.

---

## ClusterUserDefinedNetwork

A CUDN is a cluster-scoped request for an isolated network, bound to namespaces by a selector.
Its two axes:

| Axis | Values | Meaning |
|---|---|---|
| `topology` | `Layer2`, `Layer3`, `Localnet` | `Layer3` gives each node a slice of the subnet; `Layer2` is one flat subnet stretched across all nodes |
| `role` | `Primary`, `Secondary` | `Primary` replaces the default pod network for the namespace |
| `transport` | unset, `NoOverlay`, `EVPN` | How traffic leaves: Geneve (default), native routing, or VXLAN/EVPN |

!!! warning "The namespace label must exist at creation time"
    A `Primary` CUDN only attaches to namespaces carrying
    `k8s.ovn.org/primary-user-defined-network`, and a ValidatingAdmissionPolicy **forbids
    adding that label later**:

    ```console
    $ kubectl label namespace lab-blue k8s.ovn.org/primary-user-defined-network=""
    Error ... ValidatingAdmissionPolicy 'user-defined-networks-namespace-label' denied request:
    The 'k8s.ovn.org/primary-user-defined-network' label cannot be added/removed after the
    namespace was created
    ```

    Create the namespace with the label, or delete and recreate it.

### Create one

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: lab-blue
  labels:
    k8s.ovn.org/primary-user-defined-network: ""
---
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: blue
  labels:
    lab: advertised
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: lab-blue
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.200.0.0/16
        hostSubnet: 24
```

```console
$ kubectl get clusteruserdefinednetwork blue -o jsonpath='{.status.conditions}' \
    | jq -r '.[] | "\(.type)=\(.status) \(.reason)"'
NetworkCreated=True NetworkAttachmentDefinitionCreated
NetworkAllocationSucceeded=True NetworkAllocationSucceeded
```

### What it produced

**A NetworkAttachmentDefinition per selected namespace** — the CUDN is cluster-scoped, the NAD
is what CNI actually reads:

```console
$ kubectl -n lab-blue get net-attach-def
NAME   AGE
blue   20s
```

**A per-node subnet allocation**, visible as a node annotation. Note both networks coexist:

```console
$ kubectl get node ovn-worker -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/node-subnets}' | jq
{
  "cluster_udn_blue": [ "10.200.1.0/24" ],
  "default":          [ "10.244.1.0/24" ]
}
```

**A parallel OVN logical topology.** Before the CUDN, `ovn-worker` had one router and one
switch set; afterwards there is a complete second copy prefixed `cluster_udn_blue_`:

```console
$ kubectl -n ovn-kubernetes exec ovnkube-node-9rfgm -c nb-ovsdb -- ovn-nbctl lr-list
8d284cc8-... (GR_cluster_udn_blue_ovn-worker)
5a2fa5d9-... (GR_ovn-worker)
71b62372-... (cluster_udn_blue_ovn_cluster_router)
e57f166b-... (ovn_cluster_router)

$ kubectl -n ovn-kubernetes exec ovnkube-node-9rfgm -c nb-ovsdb -- ovn-nbctl ls-list
f7e7410e-... (cluster_udn_blue_join)
4b175faf-... (cluster_udn_blue_ovn-worker)
da1be70a-... (cluster_udn_blue_transit_switch)
ba111b11-... (ext_cluster_udn_blue_ovn-worker)
b35248bc-... (ext_ovn-worker)
83370da8-... (join)
d22ef062-... (ovn-worker)
1ec949de-... (transit_switch)
```

**This duplication per network is the core scale property of UDNs.** Every network multiplies
the OVN logical topology, which multiplies logical flows, which multiplies work for northd and
ovn-controller. It is the reason the
[scale ladder](01-methodology.md#scale-ladder) climbs CUDN count as a first-class dimension.

---

## VRFs — where, when, why and by whom

### Why a VRF exists at all

Two networks can use the same addresses. The default network is `10.244.0.0/16` and `blue` is
`10.200.0.0/16` here, but nothing stops a second tenant from also using `10.244.0.0/16`. The
node's **main routing table can hold only one route for a given prefix**. A VRF is the Linux
answer: a separate routing table plus a master device, with interfaces enslaved to it, so a
lookup for traffic belonging to `blue` consults table 1009 and never sees the default
network's routes.

So the VRF exists to answer one question: **when a packet for this network reaches the host
networking stack, which routing table decides where it goes?**

### When is one created — the surprising part

A VRF is created **as soon as a primary CUDN is rendered on the node**. It has nothing to do
with BGP. At this point in the walkthrough there is no `RouteAdvertisements` for `blue` at all:

```console
$ docker exec ovn-worker ip -d link show type vrf
10: blue: <NOARP,MASTER,UP,LOWER_UP> mtu 65575 qdisc noqueue state UP mode DEFAULT
    link/ether f2:83:ff:1c:93:30 brd ff:ff:ff:ff:ff:ff
    vrf table 1009 addrgenmode eui64 ...
```

Compare against the baseline taken before the CUDN existed, when the same command printed
nothing: **the default pod network does not get a VRF.** It owns the main table.

| Condition | VRF created? |
|---|---|
| Default pod network | No — uses the main table |
| Primary CUDN | **Yes**, named after the CUDN, in **both** gateway modes |
| Secondary network | No |
| `targetVRF: auto` on a RouteAdvertisements | No *additional* VRF — it reuses the one the CUDN already made |
| EVPN CUDN | Yes, plus VXLAN/bridge/SVI devices |

The second row is worth being precise about, because it is easy to assume otherwise. The call
is `udng.vrfManager.AddVRF(...)` in `pkg/node/gateway_udn.go`, inside
`addNetworkWithResolvedUplink`. The branch guarding it tests **DPU mode**, not gateway mode:

```go
} else if config.IsModeDPUHost() || config.IsModeFull() {
    ...
    if err = udng.vrfManager.AddVRF(vrfDeviceName, mplink.Attrs().Name, uint32(udng.vrfTableId), nil); err != nil {
```

So a primary CUDN gets a VRF in shared gateway mode too. What changes between the modes is not
whether the VRF exists but **whether anything important routes through it** — see
[VRFs in shared gateway mode](#vrfs-in-shared-gateway-mode).

### Naming — and the 15-character limit

The VRF is named **exactly** after the CUDN: `blue` → VRF `blue`. Linux caps interface names at
15 characters, which is the real reason
[advertised CUDN names must be short](01-methodology.md#constraints-that-bound-the-matrix). It
is not an arbitrary API rule.

### What is inside it

Exactly one interface — the network's **management port**:

```console
$ docker exec ovn-worker ip link show master blue
9: ovn-k8s-mp1: <BROADCAST,MULTICAST,PROMISC,UP,LOWER_UP> mtu 1400 master blue state UNKNOWN
    link/ether 0a:58:0a:c8:01:02 brd ff:ff:ff:ff:ff:ff

$ docker exec ovn-worker ip -br addr show | grep mp
ovn-k8s-mp0      UNKNOWN        10.244.1.2/24    # default network
ovn-k8s-mp1      UNKNOWN        10.200.1.2/24    # blue
```

`ovn-k8s-mpN` is the veth between the OVN integration bridge and the host for network N. It is
the door through which pod traffic enters the host stack in local gateway mode, and putting it
in the VRF is what binds "traffic from this network" to "this routing table".

The table itself:

```console
$ docker exec ovn-worker ip route show vrf blue
default via 172.18.0.1 dev breth0 proto 85 mtu 1400
unreachable default proto 85 metric 4278198272
10.96.0.0/16 via 169.254.0.4 dev breth0 proto 85 src 169.254.0.2 mtu 1400
10.200.0.0/16 via 10.200.1.1 dev ovn-k8s-mp1 proto 85
10.200.1.0/24 dev ovn-k8s-mp1 proto kernel scope link src 10.200.1.2
169.254.0.3 via 10.200.1.1 dev ovn-k8s-mp1 proto 85
169.254.0.12 dev ovn-k8s-mp1 proto 85 mtu 1400
```

Reading it: the whole network subnet `10.200.0.0/16` points back into OVN via the management
port; the local node slice is a connected route; the service CIDR and the default route exit
via `breth0`; `169.254.x` are the masquerade-subnet helpers. The `unreachable default` with a
huge metric is the VRF safety net that stops a lookup miss from leaking into the main table.

### How traffic gets steered into it

```console
$ docker exec ovn-worker ip rule show
0:      from all lookup local
30:     from all fwmark 0x1745ec lookup 7
1000:   from all lookup [l3mdev-table]
2000:   from all fwmark 0x1001 lookup 1009
2000:   from all to 169.254.0.12 lookup 1009
4999:   from all fwmark 0x3f0 lookup main
32766:  from all lookup main
32767:  from all lookup default
```

Two mechanisms, both visible:

- **`l3mdev` at priority 1000** — the generic kernel rule. Any packet arriving on an interface
  enslaved to a VRF automatically uses that VRF's table. This is what handles pod traffic
  coming out of `ovn-k8s-mp1`.
- **An nftables firewall mark at priority 2000** — `fwmark 0x1001 → table 1009`. Reply traffic
  and service traffic are marked so they land in the right table even when they are not
  arriving on the VRF's own interface.

### Who creates it

| Component | Where | Does what |
|---|---|---|
| `vrfmanager` | `go-controller/pkg/node/vrfmanager` | Creates and repairs the VRF device, enslaves the management port, owns the routes and rules |
| UDN gateway | `pkg/node` gateway code | Decides what belongs in the table |
| `netlinkdevicemanager` | `pkg/node/netlinkdevicemanager` | Creates the EVPN devices (bridge, VXLAN, SVIs) and the VLAN/VNI mappings |

Both `vrfmanager` and `netlinkdevicemanager` run **periodic full resyncs** on top of
event-driven reconciliation, visible in the node log:

```console
$ kubectl -n ovn-kubernetes logs ovnkube-node-9rfgm -c ovnkube-controller | grep NetlinkDeviceManager
NetlinkDeviceManager: created device evbr-lab-vtep
NetlinkDeviceManager: created device evx4-lab-vtep
NetlinkDeviceManager: created device svl2-red
NetlinkDeviceManager: created device svl3-red
NetlinkDeviceManager: reconciling vxlan-sync
NetlinkDeviceManager: periodic sync enqueued 1 VXLAN device(s) (max jitter: 5s)
NetlinkDeviceManager: found 2 existing VID/VNI mappings on evx4-lab-vtep
```

That periodic sync is correctness insurance against anything else touching the netlink state —
and it is also [bottleneck 11](03-bottlenecks.md#11-periodic-full-resyncs-on-the-node), because
its cost grows with network count whether or not anything changed.

---

## VRFs in shared gateway mode

Everything above was captured on a `--gateway-mode local` cluster. The natural follow-up is
what changes under `--gateway-mode shared`, which is the **default** and the mode most
clusters run.

!!! note "Not measured"
    Unlike the rest of this document, this section is derived from the source and the feature
    documentation rather than from a live shared-gateway cluster. The code references are
    exact; the claims have not been reproduced by hand. Rebuilding the lab under shared
    gateway and re-capturing is a worthwhile hour.

### The one-line answer

**The VRF is still created, and is still populated, but pod egress no longer traverses it.**
In shared gateway mode, pod egress stays inside the OVN/OVS datapath — pod → logical switch →
gateway router → `breth0` — and never enters the host networking stack. The Linux VRF only
ever contained the network's management port, so once pod egress stops using the management
port, the VRF stops being on that path.

### What the VRF is still for

It does not become dead weight. In either mode the VRF continues to serve:

| Traffic | Why it still needs the VRF |
|---|---|
| Host-networked pods reaching UDN services | They originate in the host stack and need the per-network table to pick the right next hop |
| Reply traffic into UDN pods | The `169.254.x` masquerade route pointing at `ovn-k8s-mpN` lives in the VRF |
| Service CIDR reachability from the host | The `10.96.0.0/16` route is a VRF route |
| Address-space separation | Two CUDNs with overlapping subnets still need separate tables for anything host-originated |

The routes and rules themselves are not gateway-mode-conditional either.
`constructUDNVRFIPRules` and `updateUDNVRFIPRoute` in `pkg/node/gateway_udn.go` branch on
`isNetworkAdvertised` and `isNetworkAdvertisedToDefaultVRF` — **not** on `config.Gateway.Mode`.

### What changes for route advertisement

The important difference is on the **import** side — what happens to routes the fabric sends
*to* the cluster.

| | Local gateway | Shared gateway |
|---|---|---|
| Pod egress path | pod → OVN → `mpN` → **host stack** → `breth0` | pod → OVN → **GR** → `breth0` |
| Who makes the egress routing decision | the Linux VRF table | the OVN gateway router |
| Learned BGP routes land in | the node routing table (default VRF, or the network's VRF) | the node routing table **and** are synced into OVN |
| Consumer of learned routes | host routing | **nbdb logical router static routes** on the GR |
| Ingress of advertised prefixes | OVS flow sends to `LOCAL`, host routes to `mpN` | OVS flow sends straight to the network's patch port |
| Hardware offloadable | No | Yes |

The feature documentation states the import behaviour directly
([route-advertisements.md](../../features/bgp-integration/route-advertisements.md)):

> This will result in the routes being installed in the main (default VRF) routing table on the
> nodes and used by the pod egress traffic in local gateway mode. As long as the
> `route-advertisements` feature is enabled, OVN-Kubernetes will synchronize the BGP routes
> from the default VRF to the default OVN pod network gateway router and hence used for the
> egress traffic of the pods on that network in shared gateway mode.

That synchronisation is `pkg/ovn/routeimport` — the component described in
[bottleneck 7](03-bottlenecks.md#7-routeimport-full-dump-and-diff-per-network). **In shared
gateway mode `routeimport` is not an optimisation, it is the only way a learned route reaches
the dataplane.** In local gateway mode the host table would route correctly even if
`routeimport` were doing nothing. That makes its latency and correctness materially more
important in shared gateway mode, and worth stating in any shared-gateway benchmark.

The ingress-side difference is visible as OVS flows on `breth0`: shared gateway sends an
advertised prefix to the network's patch port (`actions=output:3`), local gateway sends it to
the host (`actions=LOCAL`) and lets the VRF route it onward.

### What is not supported in shared gateway mode

Two things, both documented limitations rather than bugs:

| Feature | Status | Source |
|---|---|---|
| Advertising a network **to the default VRF** (`targetVRF` unset) | **Supported** in both modes | — |
| **VRF-Lite** (`targetVRF: auto`, no EVPN) | **Local gateway only** | `route-advertisements.md` Known Limitations: "VRF-Lite configurations are only supported in local gateway mode." |
| **EVPN** | **Local gateway only** | `evpn.md` Known Limitations: "Only supported in local gateway mode. Supporting shared gateway mode is a future goal." |

The reason is the same in both cases and follows from the table above. VRF-Lite and EVPN both
require egress to be routed *by the host, inside the network's VRF* — VRF-Lite out a
VRF-enslaved uplink sub-interface, EVPN into the VXLAN bridge via the SVIs. In shared gateway
mode the packet never reaches the host stack, so neither mechanism is on the path. The OVN
gateway router has no notion of a Linux VRF and no way to hand a packet to an SVI.

The VRF-Lite section of the feature documentation is explicit that the per-VRF uplink is the
administrator's job, not ovn-kubernetes':

> At least one interface with proper IP configuration needs to be attached to the network's
> VRF. The CUDN egress traffic matching the learned routes will be routed through that
> interface. **OVN-Kubernetes does not manage this interface nor its attachment to the
> network's VRF.**

That is exactly the gap observed in [the VRF-Lite walkthrough](#the-catch-observed) — the
session stayed in `Connect` because no such interface exists in the kind harness. Documented
behaviour, not a defect.

### Double-checked: EVPN and shared gateway

Asked directly — *does EVPN work in shared gateway mode?* — the answer is **no, and the
enforcement is weaker than you would want**:

| Layer | Enforces local gateway for EVPN? |
|---|---|
| Feature documentation | **Yes** — `evpn.md` Known Limitations says so plainly |
| `contrib/kind-common.sh` | **Yes** — `"EVPN requires local gateway mode (-gm local)"`, exits 1 |
| `go-controller/pkg/config/config.go` | **No** |
| CUDN CEL rules / transport validation webhook | **No** |

`config.go` validates exactly one thing about EVPN:

```go
if OVNKubernetesFeature.EnableEVPN && !OVNKubernetesFeature.EnableRouteAdvertisements {
    return fmt.Errorf("invalid feature configuration: EVPN requires route advertisements but route advertisements are disabled")
}
```

A grep for any gateway-mode condition near EVPN in the config package returns nothing. So
**`--enable-evpn` together with `--gateway-mode shared` starts cleanly**: ovnkube comes up,
the VTEP is accepted, the CUDN is admitted, and the control plane generates EVPN
`FRRConfiguration`s as usual. What you would not get is a working dataplane, because pod
traffic never reaches the SVIs. The failure would be silent and would look like a
connectivity bug rather than a configuration error.

Two consequences worth carrying forward:

- **For the lab and the benchmark**: always pass `-gm local`. The kind harness enforces it for
  you, which is the only reason this is hard to trip over in practice.
- **As a finding**: a `config.go` validation rejecting `EnableEVPN` with
  `Gateway.Mode == GatewayModeShared` — mirroring the existing route-advertisements check — is
  a small, self-contained PR that converts a silent dataplane failure into a startup error. It
  is listed as a candidate chore in [roadmap P0](05-roadmap.md). The same argument applies to
  VRF-Lite, though that one is per-RouteAdvertisements rather than per-process, so it belongs
  in the RA controller's validation and should surface as a `Not Accepted` condition.

---

## RouteAdvertisements — getting a network into BGP

A VRF makes a network routable **on the node**. Nothing outside the node knows it exists. A
`RouteAdvertisements` object is the request to publish it over BGP.

The cluster was built with `-adv`, so one already exists for the default network:

```console
$ kubectl get routeadvertisements default -o yaml | sed -n '/^spec:/,$p'
spec:
  advertisements:
  - PodNetwork
  frrConfigurationSelector:
    matchLabels:
      name: receive-all
  networkSelectors:
  - networkSelectionType: DefaultNetwork
  nodeSelector: {}
status:
  conditions:
  - message: ovn-kubernetes cluster-manager validated the resource and requested the
      necessary configuration changes
    reason: Accepted
    status: "True"
    type: Accepted
```

Three fields carry all the meaning:

| Field | Meaning |
|---|---|
| `networkSelectors` | Which networks. Only `DefaultNetwork` or `ClusterUserDefinedNetworks` |
| `frrConfigurationSelector` | Which **existing** `FRRConfiguration` to use as the template. Get this wrong and the RA is Accepted but generates nothing |
| `targetVRF` | Unset or `default` → advertise in the default VRF. `auto` → advertise in the network's own VRF |

`nodeSelector: {}` is mandatory for `PodNetwork`: a CRD CEL rule rejects a populated one,
because the pod network has to be advertised from every node.

### What it generates

One `FRRConfiguration` per **(RA × node × matching source config)**:

```console
$ kubectl -n frr-k8s-system get frrconfigurations
NAME                   AGE
ovnk-generated-dmjtp   2m47s
ovnk-generated-thdhr   2m47s
ovnk-generated-tlvlm   2m47s
receive-all            2m47s
```

Three generated objects for three nodes. The annotation records the triple:

```console
$ kubectl -n frr-k8s-system get frrconfiguration ovnk-generated-dmjtp -o yaml
metadata:
  annotations:
    k8s.ovn.org/route-advertisements: default/receive-all/ovn-worker2
  labels:
    k8s.ovn.org/route-advertisements: default
  generateName: ovnk-generated-
spec:
  bgp:
    routers:
    - asn: 64512
      neighbors:
      - address: 172.18.0.5
        asn: 64512
        toAdvertise:
          allowed:
            mode: filtered
            prefixes:
            - 10.244.2.0/24
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - prefix: 172.26.0.0/16
      prefixes:
      - 10.244.2.0/24
  nodeSelector:
    matchLabels:
      kubernetes.io/hostname: ovn-worker2
  raw:
    priority: 10
    rawConfig: |
      router bgp 64512
       address-family ipv4 unicast
        neighbor 172.18.0.5 allowas-in origin
       exit-address-family
      exit
```

Note `10.244.2.0/24` — **this node's own slice**, not the whole `10.244.0.0/16`. A Layer3
network advertises per-node host subnets, so the fabric learns which node owns which slice.
A Layer2 network would advertise the whole subnet identically from every node (anycast).

Also note the prefix list appears **twice**: once under `prefixes` and once per neighbour under
`toAdvertise`. With more peers it is copied again per peer. That duplication is the mechanism
behind [bottleneck 3](03-bottlenecks.md#3-generated-frrconfiguration-object-explosion).

The session is up and the prefix is being sent:

```console
$ kubectl -n frr-k8s-system exec <frr-k8s-pod> -c frr -- vtysh -c "show bgp summary"
BGP router identifier 172.19.0.4, local AS number 64512 VRF default vrf-id 0
Neighbor        V         AS   MsgRcvd MsgSent  Up/Down State/PfxRcd  PfxSnt
172.18.0.5      4      64512        24      35 00:05:57            1       1
```

---

## VRF-Lite: one BGP session per VRF

`targetVRF: auto` means "advertise this network from inside its own VRF". The BGP session for
those routes lives **in the VRF**, peering with the fabric over a link that is also in the VRF.
No VXLAN, no EVPN — just a separate session per tenant. This is **VRF-Lite**.

### It needs a VRF-scoped base config

First attempt, reusing the default `receive-all`:

```console
$ kubectl get routeadvertisements -o wide
NAME      STATUS
blue      Not Accepted: configuration error: FRRConfiguration "receive-all" selected for node
          "ovn-control-plane" has no VRF matching the RouteAdvertisements target VRF or any
          selected network
default   Accepted
```

The controller refuses because the source config has only a default-VRF router
(`controller.go:913`). With `targetVRF: auto` the template must contain a router **in the
target VRF**:

```yaml
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: blue-vrf
  namespace: frr-k8s-system
  labels:
    lab: blue-vrf
spec:
  nodeSelector: {}
  bgp:
    routers:
    - asn: 64512
      vrf: blue          # <-- this is what was missing
      neighbors:
      - address: 172.18.0.5
        asn: 64512
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - prefix: 172.26.0.0/16
```

```console
$ kubectl get routeadvertisements -o wide
NAME      STATUS
blue      Accepted
default   Accepted

$ kubectl -n frr-k8s-system get frrconfigurations -l k8s.ovn.org/route-advertisements=blue \
    -o custom-columns='NAME:.metadata.name,FOR:.metadata.annotations.k8s\.ovn\.org/route-advertisements'
NAME                   FOR
ovnk-generated-2br76   blue/blue-vrf/ovn-control-plane
ovnk-generated-cr6r5   blue/blue-vrf/ovn-worker
ovnk-generated-rvdgh   blue/blue-vrf/ovn-worker2
```

The generated object — **a router with a `vrf` field and its own neighbour**:

```yaml
spec:
  bgp:
    routers:
    - asn: 64512
      vrf: blue
      neighbors:
      - address: 172.18.0.5
        asn: 64512
        toAdvertise:
          allowed:
            mode: filtered
            prefixes:
            - 10.200.1.0/24
      prefixes:
      - 10.200.1.0/24
  raw:
    rawConfig: |
      router bgp 64512 vrf blue
       address-family ipv4 unicast
        neighbor 172.18.0.5 allowas-in origin
       exit-address-family
```

### The catch, observed

FRR now has a second router, and the session does **not** come up:

```console
$ kubectl -n frr-k8s-system exec <pod> -c frr -- vtysh -c "show bgp vrf all summary"
...
BGP router identifier 10.200.1.2, local AS number 64512 VRF blue vrf-id 10
Neighbor        V         AS   MsgRcvd MsgSent  Up/Down State/PfxRcd  PfxSnt
172.18.0.5      4      64512        0       0     never      Connect       0
```

Why:

```console
$ docker exec ovn-worker ip route get 172.18.0.5 vrf blue
RTNETLINK answers: No route to host

$ docker exec ovn-worker ip route get 172.18.0.5
172.18.0.5 dev breth0 src 172.18.0.3 uid 0
```

The peer is reachable in the **main** table but not inside VRF `blue`. `breth0` is not enslaved
to the VRF, so a socket bound to `blue` has nowhere to go. In a real VRF-Lite deployment you
give each VRF its own L3 link to the fabric — typically a VLAN subinterface of the uplink,
enslaved to the VRF, with the ToR holding a matching VRF and sub-interface. The kind harness
peers everything in the default VRF and does not build that, so this session stays in
`Connect`.

**This is the honest limitation of VRF-Lite and the reason EVPN exists:**

> VRF-Lite needs **one BGP session per VRF per node**, and a dedicated L3 link per VRF to carry
> it. At 1000 tenants that is 1000 sessions per node and 1000 sub-interfaces. It does not
> scale, and the fabric has to be configured in lockstep with the cluster.

---

## EVPN — what it is

**EVPN (Ethernet VPN, RFC 7432 / RFC 8365)** is a BGP address family — `l2vpn evpn` — that
carries layer-2 and layer-3 tenant information over a **single** BGP session, instead of one
session per tenant.

Three ideas make it work:

### 1. One session, many tenants, separated by Route Targets

All tenant routes travel over one `l2vpn evpn` session in the **default** VRF. Each route
carries extended communities:

| Attribute | Purpose |
|---|---|
| **RD** (Route Distinguisher) | Makes otherwise identical prefixes from different tenants unique on the wire. Usually `<router-id>:<index>` |
| **RT** (Route Target) | Says which VRF a route belongs to. A receiver imports a route into VRF X if the route's RT matches X's import RT |

The RT is the actual VPN membership marker. OVN-Kubernetes auto-derives it as `<ASN>:<VNI>`
unless you set `routeTarget` explicitly.

### 2. A VNI identifies the tenant in the dataplane

Traffic is VXLAN-encapsulated, and the 24-bit **VNI** in the VXLAN header says which tenant the
inner frame belongs to. Two flavours:

| Term | What it is | Maps to |
|---|---|---|
| **MAC-VRF** | An L2 broadcast domain — a bridge table | A `Layer2` CUDN |
| **IP-VRF** | An L3 routing table | A Linux VRF, i.e. the CUDN's VRF |

A `Layer2` CUDN can have both: a MAC-VRF so pods share a broadcast domain with external hosts,
and an IP-VRF so the same network can also be routed.

### 3. Route types

| Type | Name | Carries | Scales with |
|---|---|---|---|
| **Type-2** | MAC/IP advertisement | One endpoint's MAC, optionally MAC+IP | **Endpoints — i.e. pods** |
| **Type-3** | Inclusive Multicast | "VTEP X participates in VNI Y", used for BUM flooding | VTEPs × VNIs |
| **Type-5** | IP Prefix | A whole subnet, for routing | Subnets |

**Type-2 is the one that matters for scale.** It is per-endpoint, so an EVPN fabric carrying a
Kubernetes cluster carries a route per pod. See
[bottleneck 8](03-bottlenecks.md#8-evpn-scales-with-pods-not-nodes).

### 4. Single VXLAN Device

Rather than one VXLAN netdev per VNI, Linux SVD mode uses **one** netdev carrying all VNIs,
distinguished by VLAN ID on an attached VLAN-aware bridge. Cheaper, but VLAN IDs are 12 bits —
hence the **4094 MAC-VRF + IP-VRF ceiling per VTEP**.

---

## How OVN-Kubernetes implements EVPN

There is **no EVPN CRD**. EVPN is configured in two places: a cluster-scoped `VTEP` object, and
an `evpn` block inside the CUDN.

### Step 1 — the VTEP

```yaml
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: lab-vtep
spec:
  cidrs:
  - 172.18.0.0/16   # the kind node network: node IPs serve as VTEP IPs
  mode: Unmanaged
```

```console
$ kubectl get vtep lab-vtep -o yaml | sed -n '/^status:/,$p'
status:
  conditions:
  - message: VTEP allocation succeeded
    reason: Allocated
    status: "True"
    type: Accepted

$ kubectl get nodes -o json \
    | jq -r '.items[] | "\(.metadata.name): \(.metadata.annotations["k8s.ovn.org/vteps"])"'
ovn-control-plane: {"lab-vtep":{"ips":["172.18.0.4"]}}
ovn-worker:        {"lab-vtep":{"ips":["172.18.0.3"]}}
ovn-worker2:       {"lab-vtep":{"ips":["172.18.0.2"]}}
```

The VTEP CR declares the address range VTEP IPs come from. `Unmanaged` means the addresses
already exist on the nodes; the controller discovers them and writes the
`k8s.ovn.org/vteps` annotation. Using the node subnet is the simplest case — the node IP *is*
the VTEP IP. (Production guidance is a carrier-less dummy interface so the VTEP IP survives a
link going down, which is what enables multihoming.)

### Step 2 — the CUDN

```yaml
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: red
  labels:
    lab: evpn
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: lab-red
  network:
    topology: Layer2
    layer2:
      role: Primary
      subnets:
      - 10.210.0.0/16
    transport: EVPN
    evpn:
      vtep: lab-vtep
      macVRF:
        vni: 10100     # L2 domain
      ipVRF:
        vni: 20100     # L3 domain
```

### Step 3 — EVPN requires a RouteAdvertisements

On its own the CUDN is incomplete:

```console
$ kubectl get clusteruserdefinednetwork red -o jsonpath='{.status.conditions}' | jq -r '.[]|"\(.type)=\(.status) \(.reason)"'
TransportAccepted=False EVPNRouteAdvertisementsIsMissing
NetworkCreated=True NetworkAttachmentDefinitionCreated
```

Unlike VRF-Lite, the RA for EVPN selects the **default-VRF** `receive-all` config — the
controller extracts its neighbours and enables the `l2vpn evpn` address family on them:

```yaml
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: red
spec:
  targetVRF: auto
  advertisements: [PodNetwork]
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      name: receive-all     # the DEFAULT-VRF config, unlike blue
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          lab: evpn
```

```console
$ kubectl get routeadvertisements -o wide
NAME      STATUS
blue      Accepted
default   Accepted
red       Accepted

$ kubectl get clusteruserdefinednetwork red -o jsonpath='{.status.conditions}' | jq -r '.[]|"\(.type)=\(.status)"'
TransportAccepted=True
NetworkCreated=True
NetworkAllocationSucceeded=True
```

### The generated config — the key contrast

This single object explains the whole architecture. Compare it with the VRF-Lite one above:

```yaml
spec:
  bgp:
    routers:
    - asn: 64512
      prefixes:
      - 10.210.0.0/16
      vrf: red                      # (1) a VRF router with NO neighbours
    - asn: 64512
      neighbors:
      - address: 172.18.0.5
        asn: 64512
        toAdvertise:
          allowed:
            mode: filtered
            prefixes:
            - 172.18.0.3/32         # (2) only the VTEP host route
        toReceive:
          allowed:
            mode: filtered
            prefixes:
            - prefix: 172.26.0.0/16
            - ge: 32                # (3) other nodes' VTEP /32s
              le: 32
              prefix: 172.18.0.0/16
      prefixes:
      - 172.18.0.3/32
  raw:
    priority: 10
    rawConfig: |
      router bgp 64512
       address-family l2vpn evpn    # (4) the EVPN family on the underlay session
        neighbor 172.18.0.5 activate
        advertise-all-vni
       exit-address-family
      exit
      !
      vrf red                       # (5) kernel VRF -> L3 VNI binding
       vni 20100
      exit-vrf
      !
      router bgp 64512 vrf red
       address-family l2vpn evpn
        advertise ipv4 unicast      # (6) export the VRF's routes as Type-5
       exit-address-family
      exit
```

Six things to read off it:

1. **The `vrf: red` router has no neighbours.** There is no per-VRF BGP session. This is the
   whole difference from VRF-Lite.
2. **The underlay session advertises only `172.18.0.3/32`** — the node's VTEP address. The
   tenant subnet `10.210.0.0/16` is *not* in the IPv4 unicast family.
3. **It receives other nodes' VTEP /32s** so VXLAN tunnels can be built node-to-node.
4. **`advertise-all-vni`** on the default-VRF router turns the one existing session into the
   EVPN carrier for every VNI.
5. **`vrf red / vni 20100`** binds the Linux VRF to the L3 VNI — this is what makes zebra
   program VXLAN for it.
6. **`advertise ipv4 unicast` inside `l2vpn evpn`** is the export: the VRF's IPv4 routes are
   re-originated as EVPN Type-5 routes.

> **VRF-Lite**: tenant prefixes in IPv4 unicast, one session per VRF.
> **EVPN**: tenant prefixes in `l2vpn evpn`, one session total, VRF membership by Route Target.

### The devices it created

```console
$ docker exec ovn-worker ip -br link show | grep -E "evbr|evx4|ovl2|svl|red|mp2"
evbr-lab-vtep          UP     56:fc:a4:55:93:2d    # VLAN-aware bridge, one per VTEP
evx4-lab-vtep          UNKNOWN e2:13:cf:f3:f5:08   # SVD VXLAN device
ovl2-red               UNKNOWN 56:fc:a4:55:93:2d   # OVN <-> bridge port for the L2 domain
ovn-k8s-mp2            UNKNOWN 0a:58:0a:d2:00:02   # management port for red
red                    UP     fe:ce:f7:46:9e:68    # the VRF
svl3-red@evbr-lab-vtep UP     56:fc:a4:55:93:2d    # SVI for the IP-VRF  (vid 3)
svl2-red@evbr-lab-vtep UP     56:fc:a4:55:93:2d    # SVI for the MAC-VRF (vid 2)
```

The VXLAN device in detail — note `external vnifilter nolearning`, which is SVD mode:

```console
$ docker exec ovn-worker ip -d link show type vxlan
12: evx4-lab-vtep: ... master evbr-lab-vtep
    vxlan id 0 local 172.18.0.3 srcport 0 0 dstport 4789 ttl auto ageing 300
      external vnifilter nolearning
    bridge_slave ... learning off ... neigh_suppress on ... vlan_tunnel on
```

| Setting | Why |
|---|---|
| `external vnifilter` | SVD: one device, per-VLAN VNI mapping |
| `nolearning` / `learning off` | MAC learning is done by BGP, not by flooding |
| `neigh_suppress on` | ARP/ND answered locally from EVPN state instead of flooded |
| `vlan_tunnel on` | Enables the VLAN↔VNI tunnel mapping |
| `dstport 4789` | Standard VXLAN port, not configurable |

The VLAN↔VNI map — **this is where the 4094 ceiling is spent**:

```console
$ docker exec ovn-worker bridge vlan tunnelshow
port              vlan-id    tunnel-id
evx4-lab-vtep     2          10100      # MAC-VRF
                  3          20100      # IP-VRF

$ docker exec ovn-worker bridge vni show
dev               vni                group/remote
evx4-lab-vtep     10100
                  20100
```

One CUDN with both a MAC-VRF and an IP-VRF consumes **two** VLANs. 1000 such CUDNs consume
2000 of the 4094 available per VTEP.

Both SVIs are enslaved to the VRF (table 1014), and the management port joins them:

```console
$ docker exec ovn-worker ip link show master red
14: ovn-k8s-mp2: ... master red
16: svl3-red@evbr-lab-vtep: ... master red
17: svl2-red@evbr-lab-vtep: ... master red

$ docker exec ovn-worker ip -d link show svl2-red | grep -E "vlan protocol|vrf_slave"
    vlan protocol 802.1Q id 2 <REORDER_HDR>
    vrf_slave table 1014
```

---

## Seeing EVPN in BGP

### VNIs as FRR sees them

```console
$ kubectl -n frr-k8s-system exec <pod> -c frr -- vtysh -c "show evpn vni"
VNI    Type VxLAN IF       # MACs # ARPs # Remote VTEPs  Tenant VRF  VLAN  BRIDGE
10100  L2   evx4-lab-vtep  0      0      2               red         2     evbr-lab-vtep
20100  L3   evx4-lab-vtep  0      0      n/a             red         3     evbr-lab-vtep
```

The L2 VNI already knows about the other two nodes as remote VTEPs — learned purely from
Type-3 routes:

```console
$ ... vtysh -c "show evpn vni 10100"
VNI: 10100
 Type: L2
 Vlan: 2
 Tenant VRF: red
 Local VTEP IP: 172.18.0.3
 Remote VTEPs for this VNI:
  172.18.0.2 flood: HER
  172.18.0.4 flood: HER
```

`HER` = Head-End Replication: BUM traffic is unicast-replicated to each remote VTEP rather than
multicast. The L3 VNI ties the two together:

```console
$ ... vtysh -c "show evpn vni 20100"
VNI: 20100
  Type: L3
  Tenant VRF: red
  Vlan: 3
  SVI-If: svl3-red
  Router MAC: 56:fc:a4:55:93:2d
  L2 VNIs: 10100
```

### The EVPN route table, before any pods

```console
$ ... vtysh -c "show bgp l2vpn evpn"
   Network          Next Hop            Metric LocPrf Weight Path
Route Distinguisher: 10.210.0.2:3
 *>  [5]:[0]:[16]:[10.210.0.0]
                    172.18.0.3               0         32768 i
                    ET:8 RT:64512:20100 Rmac:56:fc:a4:55:93:2d
 * i                  172.18.0.2               0    100      0 i
                    RT:64512:20100 ET:8 Rmac:4e:bc:31:c4:ff:34
Route Distinguisher: 172.19.0.2:4
 *>i [3]:[0]:[32]:[172.18.0.2]
                    172.18.0.2                    100      0 i
                    RT:64512:10100 ET:8
Route Distinguisher: 172.19.0.3:4
 *>i [3]:[0]:[32]:[172.18.0.4]
                    172.18.0.4                    100      0 i
                    RT:64512:10100 ET:8
Route Distinguisher: 172.19.0.4:4
 *>  [3]:[0]:[32]:[172.18.0.3]
                    172.18.0.3                         32768 i
                    ET:8 RT:64512:10100
```

Everything from the theory section is visible:

- **`[5]:[0]:[16]:[10.210.0.0]`** — a Type-5 prefix route for the whole tenant subnet, tagged
  `RT:64512:20100`. That RT is `<ASN>:<IP-VRF VNI>`, auto-derived because the CUDN did not set
  `routeTarget`. `Rmac` is the router MAC used for symmetric IRB.
- **Three `[3]` Type-3 routes**, one per node, tagged `RT:64512:10100` (`<ASN>:<MAC-VRF VNI>`).
  These are what populated the remote-VTEP list above.
- **Distinct Route Distinguishers** per originator, keeping otherwise identical prefixes
  unique.
- **No Type-2 routes yet** — there are no pods.

### At the fabric

The external router sees all three nodes on the EVPN family:

```console
$ docker exec frr vtysh -c "show bgp l2vpn evpn summary"
Neighbor        V         AS   MsgRcvd MsgSent  Up/Down State/PfxRcd  PfxSnt
172.18.0.2      4      64512        50      51 00:01:45            2       6
172.18.0.3      4      64512        52      50 00:01:45            4       6
172.18.0.4      4      64512        50      49 00:01:45            2       6
```

---

## Per-pod: the Type-2 chain

Create one pod on the EVPN network:

```console
$ kubectl -n lab-red run p1 --image=registry.k8s.io/e2e-test-images/agnhost:2.45 \
    --overrides='{"spec":{"nodeName":"ovn-worker"}}' --command -- sleep infinity
$ kubectl -n lab-red get pod p1 -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/pod-networks}' \
    | jq '.["lab-red/red"] | {ip_addresses, mac_address, role}'
{
  "ip_addresses": [ "10.210.0.4/16" ],
  "mac_address": "0a:58:0a:d2:00:04",
  "role": "primary"
}
```

Now follow the same MAC through all four layers.

**1. ovnkube-node programs two kernel entries.** This is `ensurePodNeighbors` in
`pkg/node/controllers/evpn/evpn_pod_controller.go`:

```console
$ docker exec ovn-worker bridge fdb show | grep 0a:58:0a:d2
0a:58:0a:d2:00:04 dev ovl2-red vlan 2 master evbr-lab-vtep static

$ docker exec ovn-worker ip neigh show | grep 10.210.0.4
10.210.0.4 dev svl2-red lladdr 0a:58:0a:d2:00:04 PERMANENT
```

**2. zebra learns it as a local MAC in the VNI:**

```console
$ ... vtysh -c "show evpn mac vni 10100"
Number of MACs (local and remote) known for this VNI: 1
MAC               Type   Flags Intf/Remote ES/VTEP   VLAN  Seq #'s
0a:58:0a:d2:00:04 local        ovl2-red              2     0/0
```

**3. bgpd originates Type-2 routes:**

```console
$ ... vtysh -c "show bgp l2vpn evpn route type macip"
Route Distinguisher: 172.19.0.4:4
 *>  [2]:[0]:[48]:[0a:58:0a:d2:00:04] RD 172.19.0.4:4
                    172.18.0.3                         32768 i
                    ET:8 RT:64512:10100
 *>  [2]:[0]:[48]:[0a:58:0a:d2:00:04]:[32]:[10.210.0.4] RD 172.19.0.4:4
                    172.18.0.3                         32768 i
                    ET:8 RT:64512:10100 RT:64512:20100 Rmac:56:fc:a4:55:93:2d

Displayed 2 prefixes (2 paths) (of requested type)
```

**Two Type-2 routes per pod, not one:**

| Route | RTs | Purpose |
|---|---|---|
| `[2]:[0]:[48]:[MAC]` | MAC-VRF only | Pure L2 reachability — bridging |
| `[2]:[0]:[48]:[MAC]:[32]:[IP]` | MAC-VRF **and** IP-VRF | L2 plus L3, enabling symmetric IRB routing |

This is the number to carry into capacity planning. At 200 pods per node on 500 nodes with both
VRF types, that is **200,000 Type-2 routes** in the fabric, on top of Type-3 and Type-5. The
fabric has to be sized for it, and that is a conversation with whoever owns the ToRs — see
[bottleneck 8](03-bottlenecks.md#8-evpn-scales-with-pods-not-nodes).

### OVN's side

For completeness, the EVPN network has a Layer2 topology in OVN, structurally different from
`blue`'s Layer3:

```console
$ kubectl -n ovn-kubernetes exec ovnkube-node-9rfgm -c nb-ovsdb -- ovn-nbctl ls-list
...
ab7cd1b1-... (cluster_udn_red_ovn_layer2_switch)      # one switch, cluster-wide
9eec27bf-... (ext_cluster_udn_red_ovn-worker)
4b175faf-... (cluster_udn_blue_ovn-worker)            # blue: one switch PER NODE
da1be70a-... (cluster_udn_blue_transit_switch)

$ ... ovn-nbctl lr-list
e25592d3-... (GR_cluster_udn_red_ovn-worker)
8dc3c62e-... (cluster_udn_red_transit_router)
8d284cc8-... (GR_cluster_udn_blue_ovn-worker)
71b62372-... (cluster_udn_blue_ovn_cluster_router)
```

---

## Side by side

The same question — "how does this network's traffic leave the cluster?" — answered three ways.

| | **Plain CUDN** | **VRF-Lite** (`blue`) | **EVPN** (`red`) |
|---|---|---|---|
| Linux VRF created | Yes | Yes (same one) | Yes (same one) |
| Advertised over BGP | No | Yes | Yes |
| `targetVRF` | n/a | `auto` | `auto` |
| Source FRRConfiguration must have | n/a | a `vrf: <name>` router | a default-VRF router |
| BGP sessions per node | 0 extra | **1 per VRF** | **0 extra** — reuses the underlay |
| Fabric link per tenant | n/a | **Required** (VLAN subif in the VRF) | Not required |
| What the underlay advertises | n/a | tenant prefixes | the node's VTEP `/32` only |
| Address family | n/a | `ipv4/ipv6 unicast` | `l2vpn evpn` |
| Tenant separation on the wire | separate session | separate session | **Route Target** |
| Encapsulation | Geneve (or none) | Geneve (or none) | **VXLAN**, port 4789 |
| Routes scale with | — | nodes × networks | **pods** (Type-2) |
| Hard ceiling | — | sessions per node | **4094** VLANs per VTEP |
| Devices on the node | VRF + `mpN` | same | + bridge, SVD VXLAN, 2 SVIs per network |
| **Gateway modes** | both | **local only** | **local only** |

The trade is clear: VRF-Lite is simpler to reason about and needs no VXLAN, but costs a session
and a fabric link per tenant. EVPN costs a VXLAN datapath and an encap header, and gives you
arbitrary tenant counts over one session — up to the VLAN ceiling, and up to whatever Type-2
volume the fabric tolerates.

Advertising a network **to the default VRF** — `targetVRF` left unset, no EVPN — is the only
one of the three BGP options that works in shared gateway mode, and it is also the one the
`-adv` flag sets up for the default pod network. See
[VRFs in shared gateway mode](#vrfs-in-shared-gateway-mode).

---

## Who does what

### Control plane

| Component | Package | Responsibility |
|---|---|---|
| CUDN controller | `pkg/clustermanager/userdefinednetwork` | CUDN → NAD per namespace; `NetworkCreated` condition |
| Transport validation | `pkg/clustermanager/userdefinednetwork/transport_validation.go` | `TransportAccepted` — checks an RA exists for an EVPN CUDN |
| Subnet allocator | `pkg/clustermanager` | Per-node host subnets; the `node-subnets` annotation |
| RouteAdvertisements controller | `pkg/clustermanager/routeadvertisements` | Generates `FRRConfiguration` per (RA × node × source); annotates NADs; `Accepted` condition |
| VTEP controller | `pkg/clustermanager/vtep` | VTEP IP allocation/discovery; the `k8s.ovn.org/vteps` annotation |
| frr-k8s controller | upstream | Merges all `FRRConfiguration`s for a node into one FRR config and reloads |

Visible in the log, including the transport handshake between the two controllers:

```console
$ kubectl -n ovn-kubernetes logs deploy/ovnkube-control-plane | grep -iE "routeadvertisements|TransportAccepted"
transport_validation.go:245] Set TransportAccepted condition for ClusterUserDefinedNetwork "red":
  False (reason: EVPNRouteAdvertisementsNotAccepted)
controller.go:379] Finished syncing routeadvertisements "red", took 220.66082ms
controller.go:1426] "Re-queueing CUDN selected by RouteAdvertisements" cudn="red" ra="red" transport="EVPN"
transport_validation.go:134] Found valid RouteAdvertisements "red" for ClusterUserDefinedNetwork "red" with EVPN transport
```

Note `Finished syncing routeadvertisements` — that line is the RA reconcile timing, logged at
V(4). It is the only reconcile-duration signal that exists today, which is why
[02 table 2](02-metrics.md#table-2-missing-with-code-sites) proposes promoting it to a
histogram.

### Data plane

| Component | Package | Responsibility |
|---|---|---|
| `vrfmanager` | `pkg/node/vrfmanager` | VRF device, management-port enslavement, routes, rules |
| `netlinkdevicemanager` | `pkg/node/netlinkdevicemanager` | EVPN bridge, SVD VXLAN device, SVIs, VLAN/VNI mappings |
| EVPN node controller | `pkg/node/controllers/evpn/evpn_node_controller.go` | Per-VTEP reconcile, OVS port wiring |
| EVPN pod controller | `pkg/node/controllers/evpn/evpn_pod_controller.go` | **Per-pod** FDB and permanent-neighbour entries |
| `routeimport` | `pkg/ovn/routeimport` | Kernel `RTPROT_BGP` routes → OVN logical router static routes |
| frr-k8s daemon + FRR | `frr-k8s-system/frr-k8s-daemon` | Renders config, reloads FRR; zebra programs VXLAN/FDB, bgpd speaks EVPN |
| ovn-controller | — | Compiles southbound flows into OpenFlow on the node |

### Feature state as metrics

```console
$ curl -s http://172.18.0.4:9411/metrics | grep -E "cluster_user_defined_networks|route_advertisement_condition|vtep_condition"
ovnkube_clustermanager_cluster_user_defined_networks{role="Primary",topology="Layer2",transport="EVPN"} 1
ovnkube_clustermanager_cluster_user_defined_networks{role="Primary",topology="Layer3",transport="Default"} 1
ovnkube_clustermanager_route_advertisement_condition{condition="Accepted",name="blue",status="true"} 1
ovnkube_clustermanager_route_advertisement_condition{condition="Accepted",name="default",status="true"} 1
ovnkube_clustermanager_route_advertisement_condition{condition="Accepted",name="red",status="true"} 1
ovnkube_clustermanager_vtep_condition{condition="Accepted",name="lab-vtep",status="true"} 1
```

!!! warning "The metrics endpoint does not bind to localhost"
    `ovnkube_cluster_manager_metrics_bind_address: 172.18.0.4:9411` — it binds to the **node
    IP**, so `kubectl port-forward` plus `curl localhost` returns nothing. Curl the node IP
    directly, as above, or pass `-mip 0.0.0.0` to `kind.sh`.

These three gauges are the **entire** BGP/EVPN metric surface today. There is no timing, no
object count, no prefix count — which is what [02](02-metrics.md) is about.

---

## Command cookbook

```bash
kind export kubeconfig --name ovn

# --- Intent layer -------------------------------------------------------
kubectl get clusteruserdefinednetworks,routeadvertisements,vteps -A
kubectl get clusteruserdefinednetwork <n> -o jsonpath='{.status.conditions}' | jq
kubectl get routeadvertisements -o wide
kubectl get nodes -o json | jq -r '.items[]|"\(.metadata.name): \(.metadata.annotations["k8s.ovn.org/vteps"])"'
kubectl get node <node> -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/node-subnets}' | jq

# --- Generated BGP intent ----------------------------------------------
kubectl -n frr-k8s-system get frrconfigurations
kubectl -n frr-k8s-system get frrconfigurations -l k8s.ovn.org/route-advertisements=<ra> \
  -o custom-columns='NAME:.metadata.name,FOR:.metadata.annotations.k8s\.ovn\.org/route-advertisements'
kubectl get frrnodestates -o json \
  | jq '[.items[]|{node:.metadata.name, bytes:(.status.runningConfig|length)}]'
kubectl get bgpsessionstates -A

# --- Host layer ---------------------------------------------------------
docker exec <node> ip -d link show type vrf
docker exec <node> ip link show master <vrf>
docker exec <node> ip route show vrf <vrf>
docker exec <node> ip rule show
docker exec <node> ip -d link show type vxlan
docker exec <node> bridge vlan tunnelshow      # VLAN -> VNI
docker exec <node> bridge vni show
docker exec <node> bridge fdb show | grep -v permanent
docker exec <node> ip neigh show | grep PERMANENT
docker exec <node> ip route show proto bgp     # learned from the fabric

# --- FRR on the node ----------------------------------------------------
P=$(kubectl -n frr-k8s-system get pod -o name --field-selector spec.nodeName=<node> | head -1)
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show bgp vrf all summary"
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show evpn vni"
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show evpn vni <vni>"
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show evpn mac vni <vni>"
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show bgp l2vpn evpn"
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show bgp l2vpn evpn route type macip"
kubectl -n frr-k8s-system exec $P -c frr -- vtysh -c "show running-config"

# --- External fabric ----------------------------------------------------
docker exec frr vtysh -c "show bgp summary"
docker exec frr vtysh -c "show bgp l2vpn evpn summary"
docker exec frr vtysh -c "show ip route bgp"

# --- OVN ----------------------------------------------------------------
POD=$(kubectl -n ovn-kubernetes get pod -l app=ovnkube-node \
  --field-selector spec.nodeName=<node> -o jsonpath='{.items[0].metadata.name}')
kubectl -n ovn-kubernetes exec $POD -c nb-ovsdb -- ovn-nbctl lr-list
kubectl -n ovn-kubernetes exec $POD -c nb-ovsdb -- ovn-nbctl ls-list
kubectl -n ovn-kubernetes exec $POD -c nb-ovsdb -- \
  ovn-nbctl --format=table find logical_router_static_route

# --- Metrics (bind to the NODE IP, not localhost) -----------------------
curl -s http://<node-ip>:9411/metrics | grep ^ovnkube_clustermanager   # control plane
curl -s http://<node-ip>:9410/metrics | grep ^ovnkube_                 # node
curl -s http://<node-ip>:9476/metrics | grep -E '^(ovn|ovs)_'          # OVN/OVS
```

---

## References

### OVN-Kubernetes

| Resource | Covers |
|---|---|
| [Route Advertisements](../../features/bgp-integration/route-advertisements.md) | The RA CRD, supported combinations, Known Limitations |
| [EVPN](../../features/bgp-integration/evpn.md) | The VTEP CRD, CUDN `evpn` block, FRR configuration examples |
| [No-Overlay Mode](../../features/bgp-integration/no-overlay.md) | Running without encapsulation |
| [User Defined Networks](../../features/user-defined-networks/user-defined-networks.md) | CUDN topologies and roles |
| `docs/okeps/okep-5296-bgp.md` | BGP design rationale and alternatives considered |
| `docs/okeps/okep-5088-evpn.md` | EVPN design, SVD choice, known scale concerns |
| [Architecture](../../design/architecture.md) | Which component runs where |

### Standards

| RFC | Title | Why it matters |
|---|---|---|
| [RFC 7432](https://datatracker.ietf.org/doc/html/rfc7432) | BGP MPLS-Based Ethernet VPN | The base EVPN spec. Defines route types 1-4, RD/RT semantics |
| [RFC 8365](https://datatracker.ietf.org/doc/html/rfc8365) | A Network Virtualization Overlay Solution Using EVPN | EVPN over VXLAN — what is actually deployed here |
| [RFC 9136](https://datatracker.ietf.org/doc/html/rfc9136) | IP Prefix Advertisement in EVPN | **Type-5** routes, i.e. the IP-VRF subnet advertisement |
| [RFC 7348](https://datatracker.ietf.org/doc/html/rfc7348) | VXLAN | The encapsulation, VNI semantics, UDP port 4789 |
| [RFC 4364](https://datatracker.ietf.org/doc/html/rfc4364) | BGP/MPLS IP VPNs | Where RD and RT come from. Still the clearest explanation |
| [RFC 4271](https://datatracker.ietf.org/doc/html/rfc4271) | BGP-4 | The base protocol |

### FRR and Linux

| Resource | Covers |
|---|---|
| [FRR EVPN documentation](https://docs.frrouting.org/en/latest/evpn.html) | `advertise-all-vni`, `vni` under `vrf`, symmetric vs asymmetric IRB |
| [FRR BGP documentation](https://docs.frrouting.org/en/latest/bgp.html) | Address families, route maps, `allowas-in` |
| [Linux VRF](https://docs.kernel.org/networking/vrf.html) | `l3mdev`, enslavement, socket binding. Section 4 explains the IPv6 address-loss gotcha |
| [Linux VXLAN](https://docs.kernel.org/networking/vxlan.html) | VXLAN netdev, external mode |
| [frr-k8s](https://github.com/metallb/frr-k8s) | `FRRConfiguration` merge semantics, `FRRNodeState`, `BGPSessionState` |

### Background reading

| Resource | Why |
|---|---|
| [NVIDIA Cumulus: EVPN](https://docs.nvidia.com/networking-ethernet-software/cumulus-linux/Network-Virtualization/Ethernet-Virtual-Private-Network-EVPN/) | The most readable operational EVPN guide; Cumulus and FRR share lineage, so the CLI matches |
| [Cumulus: single VXLAN device](https://docs.nvidia.com/networking-ethernet-software/cumulus-linux/Network-Virtualization/Single-VXLAN-Device/) | SVD mode and where the 4094 ceiling comes from |
| *EVPN in the Data Center* (Dinesh Dutt, O'Reilly) | Book-length treatment of symmetric vs asymmetric IRB |
| [Linux Foundation: l3mdev](https://legacy.netdevconf.info/1.2/papers/ahern-what-is-l3mdev-paper.pdf) | The design paper for the VRF mechanism |

### Where to go next in this set

| Document | For |
|---|---|
| [06 — Local Lab](06-local-lab.md) | Reproducing all of the above from scratch, plus troubleshooting |
| [01 — Methodology](01-methodology.md) | How these objects turn into a scale benchmark |
| [03 — Bottlenecks](03-bottlenecks.md) | Where the implementation is expected to hurt |
