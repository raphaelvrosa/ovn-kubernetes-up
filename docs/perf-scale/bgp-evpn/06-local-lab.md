# 06 — Local Lab: BGP and EVPN in kind

A hands-on minimum viable scenario. Bring up a kind cluster with route advertisements and
EVPN, create RouteAdvertisements by hand, watch what ovn-kubernetes generates, read the FRR
side, and iterate on code with metrics on.

## Contents

1. [What you get](#what-you-get)
2. [Prerequisites](#prerequisites)
3. [Bring up the cluster](#bring-up-the-cluster)
4. [Verify the plumbing](#verify-the-plumbing)
5. [Scenario A — advertise the default network](#scenario-a-advertise-the-default-network)
6. [Scenario B — advertise a CUDN in its own VRF](#scenario-b-advertise-a-cudn-in-its-own-vrf)
7. [Scenario C — EVPN Layer2 MAC-VRF](#scenario-c-evpn-layer2-mac-vrf)
8. [Where to look when it breaks](#where-to-look-when-it-breaks)
9. [Metrics and profiles](#metrics-and-profiles)
10. [The fast iteration loop](#the-fast-iteration-loop)
11. [Teardown](#teardown)

---

## What you get

`contrib/kind.sh` (a symlink to `kind-helm.sh`) builds the entire BGP topology for you when
`-rae` is passed. Nothing in this document asks you to construct a fabric by hand.

```text
  ┌──────────────┐        ┌─────────────────┐        ┌──────────────────────┐
  │  bgpserver   │        │   frr           │        │  kind cluster        │
  │  (agnhost)   │────────│   container     │────────│  ovn-control-plane   │
  │              │        │   ASN 64512     │        │  ovn-worker          │
  │ 172.26.0.0/16│        │  route reflector│        │  ovn-worker2         │
  └──────────────┘        └─────────────────┘        │    frr-k8s-daemon    │
                             172.18.0.0/16           └──────────────────────┘
                              (kind network)
```

| Piece | Name | Notes |
|---|---|---|
| External router | container `frr` | ASN 64512, configured as a BGP **route reflector** for every node |
| External server | container `bgpserver` | On `172.26.0.0/16`, reachable only via the FRR router. The thing you ping to prove advertisement worked |
| In-cluster speaker | DaemonSet `frr-k8s-daemon` in `frr-k8s-system` | One FRR per node |
| Base FRRConfiguration | `receive-all` in `frr-k8s-system`, labelled `name: receive-all` | Peers each node with the external FRR and accepts `172.26.0.0/16`. **This is the object your `frrConfigurationSelector` must match** |

Everything uses **ASN 64512** on both sides — iBGP with the external FRR acting as the
reflector. There is exactly one external peer; see
[the fabric-side harness gap](04-ci-kube-burner.md#the-fabric-side-harness-gap) for why that
matters at scale, but it is irrelevant for a functional lab.

---

## Prerequisites

Generic kind/Go/kubectl setup is in
[Running CI Locally](../../developer-guide/local_testing_guide.md) and
[Launching OVN-Kubernetes on KIND](../../installation/launching-ovn-kubernetes-on-kind.md).
Only the BGP-specific extras are listed here.

```bash
# VRF kernel module — required for targetVRF: auto and for EVPN.
sudo modprobe vrf

# kind-helm.sh raises these itself, but a prior low value will bite you first.
sudo sysctl -w fs.inotify.max_user_watches=524288
sudo sysctl -w fs.inotify.max_user_instances=512

# One-time: install the kind and helm binaries the scripts expect.
make -C test install-kind
```

!!! note "Bridge netfilter"
    `kind-helm.sh` disables bridge netfilter automatically when `-rae` is set
    (`helm_prereqs` → `disable_bridge_netfilter`). You do not need to do it yourself, but if
    you are reusing a cluster built without `-rae`, traffic will behave oddly until you
    rebuild.

FRR versions in play, worth knowing because EVPN behaviour depends on them
(`contrib/kind-common.sh`):

| Component | Image |
|---|---|
| External `frr` container | `quay.io/frrouting/frr:10.6.0` |
| In-cluster frr-k8s daemon | same, overridable with `FRR_K8S_FRR_IMAGE` |
| frr-k8s | pinned git ref `b43efcb206be`, cloned and patched at install time |

---

## Bring up the cluster

### Route advertisements only

```bash
cd contrib
./kind.sh \
  --route-advertisements-enable \
  --multi-network-enable \
  --network-segmentation-enable \
  --advertise-default-network \
  --scale-metrics \
  --gateway-mode local \
  --num-workers 2 \
  --master-loglevel 5 \
  --node-loglevel 5
```

Short form: `./kind.sh -rae -mne -nse -adv -sm -gm local -wk 2 -ml 5 -nl 5`.

| Flag | Why |
|---|---|
| `-rae` | Deploys frr-k8s, the external `frr` container and `bgpserver`; sets `--enable-route-advertisements` |
| `-mne` | **Mandatory.** `kind-common.sh` rejects `-rae` without multi-network |
| `-nse` | Needed for primary CUDNs in [scenario B](#scenario-b-advertise-a-cudn-in-its-own-vrf) |
| `-adv` | Creates a ready-made `RouteAdvertisements/default` for the default pod network, and adds host routes back to the pod subnets so return traffic works |
| `-sm` | Turns on `--metrics-enable-scale` — the workqueue metric family. See [metrics](#metrics-and-profiles) |
| `-gm local` | Default is `shared`. Local gateway is **required** for VRF-Lite and EVPN |
| `-ml 5 -nl 5` | `klog` V(5). The RA controller logs its reconcile duration at V(4) and `routeimport` at V(5), so anything less hides exactly what you came to see |

Useful extras:

| Flag | Effect |
|---|---|
| `-rud` | `--advertised-udn-isolation-mode=loose` — skips the whole `udn_isolation` ACL path |
| `-dudn` | `EnableDynamicUDNAllocation` — networks only instantiated on nodes that need them |
| `-noe` | No-overlay mode (`-noe snat-enabled`, `-noe managed`) |
| `-i6` | Add IPv6 (dual-stack) |

### Adding EVPN

```bash
./kind.sh -rae -mne -nse -evpn -sm -gm local -wk 2 -ml 5 -nl 5
```

`-evpn` is validated against two things in `kind-common.sh`: it **requires `-rae`**, and it
**requires `--gateway-mode local`**. Beyond setting `--enable-evpn`, it configures the
external FRR with `address-family l2vpn evpn`, `advertise-all-vni`, and activates every
neighbour as a route-reflector client.

Note it does **not** create any VTEP device or VTEP CR — that is
[scenario C](#scenario-c-evpn-layer2-mac-vrf).

---

## Verify the plumbing

Before writing any RouteAdvertisements, confirm the substrate is healthy. Each of these has
caught a broken cluster for me faster than reading logs.

```bash
# 1. frr-k8s is up: one daemon pod per node, plus the statuscleaner.
kubectl -n frr-k8s-system get pods -o wide

# 2. The base FRRConfiguration exists and carries the label RAs select on.
kubectl -n frr-k8s-system get frrconfiguration receive-all -o yaml

# 3. Every node has an FRRNodeState. This is also the object whose size is
#    bottleneck 4 — note how big it already is with zero VRFs.
kubectl get frrnodestates
kubectl get frrnodestates -o json | jq '[.items[] | {node: .metadata.name, bytes: (.status.runningConfig | length)}]'

# 4. Sessions are established, one per node.
kubectl get bgpsessionstates -A

# 5. The external router sees every node as a peer.
docker exec frr vtysh -c "show bgp summary"
```

If step 4 or 5 is empty, nothing further in this document will work. Start at
[where to look](#where-to-look-when-it-breaks).

---

## Scenario A — advertise the default network

With `-adv` the chart already created this for you:

```bash
kubectl get routeadvertisements default -o yaml
```

```yaml
spec:
  networkSelectors:
  - networkSelectionType: DefaultNetwork
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      name: receive-all
  advertisements:
  - PodNetwork
```

Three things to internalise from this object:

- `nodeSelector: {}` is **mandatory** when advertising `PodNetwork`. The CRD has a CEL rule
  that rejects a populated one, because the pod network must be advertised from every node.
- `frrConfigurationSelector` matches the `receive-all` base config. Get this label wrong and
  the RA is accepted but generates nothing — a quiet failure mode worth triggering once
  deliberately so you recognise it.
- `targetVRF` is unset, so routes go into the default VRF.

### Watch what it produced

```bash
# Accepted?
kubectl get routeadvertisements default \
  -o jsonpath='{.status.conditions[?(@.type=="Accepted")]}' | jq

# The generated objects: one per (RA x node x matching source config).
kubectl -n frr-k8s-system get frrconfigurations -l k8s.ovn.org/route-advertisements=default

# With 1 RA, 3 nodes and 1 source config you should see exactly 3.
kubectl -n frr-k8s-system get frrconfigurations \
  -l k8s.ovn.org/route-advertisements=default --no-headers | wc -l
```

Open one and find the prefixes:

```bash
kubectl -n frr-k8s-system get frrconfigurations \
  -l k8s.ovn.org/route-advertisements=default -o yaml \
  | yq '.items[0].spec.bgp.routers[].neighbors[].toAdvertise'
```

Note the prefix list is repeated **per neighbour**, not stored once. That is the mechanism
behind [bottleneck 3](03-bottlenecks.md#3-generated-frrconfiguration-object-explosion): peer
count multiplies stored bytes.

### Confirm it reached the fabric

```bash
# Pod subnets should appear in the external router's table.
docker exec frr vtysh -c "show ip route bgp"
docker exec frr vtysh -c "show bgp ipv4 unicast"
```

### Prove the dataplane

```bash
kubectl run testpod --image=registry.k8s.io/e2e-test-images/agnhost:2.45 \
  --command -- sleep infinity
kubectl wait --for=condition=Ready pod/testpod --timeout=60s

BGPSERVER_IP=$(docker inspect bgpserver \
  -f '{% raw %}{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}{% endraw %}' | head -1)
kubectl exec testpod -- curl -s --max-time 5 "http://${BGPSERVER_IP}:8080/clientip"
```

The returned client IP should be the **pod IP**, not a node IP. That is the whole point:
the pod subnet is routed, so no SNAT happens.

### The interesting experiment

[Bottleneck 1](03-bottlenecks.md#1-reconcileall-fan-out) says an unrelated node change
re-reconciles every RA. Trigger it:

```bash
kubectl -n ovn-kubernetes logs -l name=ovnkube-control-plane --tail=0 -f &
kubectl label node ovn-worker scratch=1 --overwrite
```

Semantically this is a no-op for BGP. Watch how much reconcile work it causes. With one RA
it is cheap; the point of the scale work is that the cost is linear in RA count.

---

## Scenario B — advertise a CUDN in its own VRF

VRF-Lite: the network is advertised into a dedicated VRF named after the CUDN. Local gateway
mode only.

!!! warning "CUDN names must be under 16 characters"
    The VRF name is derived from the CUDN name and the kernel caps interface names at 15
    characters. `blue` is fine; `my-advertised-tenant-network` is not, and the failure is
    not obvious.

```bash
kubectl create namespace bgp-lab
kubectl label namespace bgp-lab k8s.ovn.org/primary-user-defined-network=""

cat <<'EOF' | kubectl apply -f -
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: blue
  labels:
    lab: advertised
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: bgp-lab
  network:
    topology: Layer3
    layer3:
      role: Primary
      subnets:
      - cidr: 10.200.0.0/16
        hostSubnet: 24
---
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: blue
spec:
  targetVRF: auto
  advertisements:
  - PodNetwork
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      name: receive-all
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          lab: advertised
EOF
```

`targetVRF: auto` is what makes this VRF-Lite rather than a default-VRF advertisement.

### Inspect

```bash
kubectl get clusteruserdefinednetwork blue -o jsonpath='{.status.conditions}' | jq
kubectl get routeadvertisements blue -o jsonpath='{.status.conditions}' | jq

# Generated objects now carry a VRF-scoped router.
kubectl -n frr-k8s-system get frrconfigurations \
  -l k8s.ovn.org/route-advertisements=blue -o yaml | yq '.items[0].spec.bgp.routers'

# The VRF exists on the node.
docker exec ovn-worker ip link show type vrf
docker exec ovn-worker ip route show vrf blue

# FRR on that node now has a per-VRF router.
kubectl -n frr-k8s-system exec ds/frr-k8s-daemon -c frr -- vtysh -c "show bgp vrf all summary"
```

### Watch FRRNodeState grow

This is [bottleneck 4](03-bottlenecks.md#4-frrnodestatestatusrunningconfig-size) in
miniature. Record the size, add CUDNs, record again:

```bash
kubectl get frrnodestates -o json \
  | jq '[.items[] | {node: .metadata.name, bytes: (.status.runningConfig | length)}]'
```

Each VRF adds a `router bgp 64512 vrf <name>` block. Extrapolate against the **1.5 MB default
etcd object limit** — ten CUDNs here tells you roughly where a thousand lands.

### Isolation on and off

With the default `strict` mode, advertised UDNs cannot reach each other:

```bash
docker exec ovn-worker nft list chain inet ovn-kubernetes udn-bgp-drop
docker exec ovn-worker nft list set inet ovn-kubernetes advertised-udn-subnets-v4
```

Rebuild with `-rud` to compare against `loose`, which skips the entire path. That delta is a
[first-class benchmark axis](01-methodology.md#scale-ladder), and it is easiest to understand
by looking at the nftables and OVN ACL state side by side.

---

## Scenario C — EVPN Layer2 MAC-VRF

Requires the cluster built with `-evpn`.

### Step 1 — the VTEP

The laziest correct option: point the VTEP CIDR at the **kind node network**. Node IPs then
serve as VTEP IPs and there is nothing to configure per node.

```bash
cat <<'EOF' | kubectl apply -f -
apiVersion: k8s.ovn.org/v1
kind: VTEP
metadata:
  name: lab-vtep
spec:
  cidrs:
  - 172.18.0.0/16
  mode: Unmanaged
EOF

kubectl get vtep lab-vtep -o jsonpath='{.status.conditions}' | jq
kubectl get nodes -o json | jq '.items[] | {name: .metadata.name, vteps: .metadata.annotations["k8s.ovn.org/vteps"]}'
```

Every node should pick up a `k8s.ovn.org/vteps` annotation. If they do not, the EVPN node
controller is not seeing the VTEP — check its logs before going further.

!!! note "Why not a dedicated device"
    Production guidance is to put the VTEP IP on a carrier-less device (a dummy interface) so
    it survives a physical link going down, which is what enables EVPN multihoming. For a
    lab, node IPs are fine and save you a per-node setup step. If you want the realistic
    shape, add `ip addr add <ip>/32 dev lo` on each node and give the VTEP a CIDR matching
    those — that is exactly what `ensureVTEPLoopbackIPs` in `test/e2e/evpn.go` does.

### Step 2 — the external EVPN fabric

`-evpn` configured BGP on the external FRR but not the VXLAN datapath. Build it, mirroring
`setupEVPNBridgeOnExternalFRR` in `test/e2e/evpn.go`:

```bash
FRR_IP=$(docker inspect frr -f '{% raw %}{{(index .NetworkSettings.Networks "kind").IPAddress}}{% endraw %}')
VNI=10100
VID=100

# VLAN-aware bridge
docker exec frr ip link add brevpn type bridge vlan_filtering 1 vlan_default_pvid 0
docker exec frr ip link set brevpn addrgenmode none

# Single VXLAN Device: one netdev carrying all VNIs, distinguished by VLAN.
# This is the source of the 4094 ceiling.
docker exec frr ip link add vxevpn type vxlan \
  dstport 4789 local "${FRR_IP}" nolearning external vnifilter
docker exec frr ip link set vxevpn addrgenmode none
docker exec frr ip link set vxevpn master brevpn

docker exec frr ip link set brevpn up
docker exec frr ip link set vxevpn up
docker exec frr bridge link set dev vxevpn vlan_tunnel on neigh_suppress on learning off

# Map VLAN -> VNI
docker exec frr bridge vlan add dev brevpn vid "${VID}" self
docker exec frr bridge vlan add dev vxevpn vid "${VID}"
docker exec frr bridge vni add dev vxevpn vni "${VNI}"
docker exec frr bridge vlan add dev vxevpn vid "${VID}" tunnel_info id "${VNI}"
```

### Step 3 — the EVPN CUDN

```bash
kubectl create namespace evpn-lab
kubectl label namespace evpn-lab k8s.ovn.org/primary-user-defined-network=""

cat <<'EOF' | kubectl apply -f -
apiVersion: k8s.ovn.org/v1
kind: ClusterUserDefinedNetwork
metadata:
  name: red
  labels:
    lab: evpn
spec:
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: evpn-lab
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
        vni: 10100
---
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: red
spec:
  targetVRF: auto
  advertisements:
  - PodNetwork
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      name: receive-all
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          lab: evpn
EOF
```

The `vni: 10100` must match the VNI you mapped on the external bridge. Route targets are
auto-derived to `<ASN>:<VNI>` when omitted.

### Step 4 — observe Type-2 routes appear per pod

This is the single most important EVPN behaviour to see with your own eyes.

```bash
# Baseline: no pods yet.
docker exec frr vtysh -c "show bgp l2vpn evpn route type macip" | tail -5

kubectl -n evpn-lab run p1 --image=registry.k8s.io/e2e-test-images/agnhost:2.45 \
  --command -- sleep infinity
kubectl -n evpn-lab wait --for=condition=Ready pod/p1 --timeout=90s

# One MAC/IP route per pod IP has appeared.
docker exec frr vtysh -c "show bgp l2vpn evpn route type macip"
docker exec frr vtysh -c "show evpn vni detail"
```

Then look at what produced it on the node side:

```bash
NODE=$(kubectl -n evpn-lab get pod p1 -o jsonpath='{.spec.nodeName}')
docker exec "${NODE}" bridge fdb show | grep -i vx
docker exec "${NODE}" ip neigh show | grep PERMANENT
```

One permanent neighbour entry and one FDB entry per pod IP, programmed by
`ensurePodNeighbors` in `pkg/node/controllers/evpn/evpn_pod_controller.go`. Scale pods, not
nodes, and watch both tables grow linearly. **This is why EVPN scales with pods while plain
route advertisements scale with nodes × networks** — see
[bottleneck 8](03-bottlenecks.md#8-evpn-scales-with-pods-not-nodes).

### Step 5 — VLAN consumption

```bash
docker exec "${NODE}" bridge vlan show
```

Each MAC-VRF and each IP-VRF burns one VLAN against the **4094** SVD ceiling. Adding an
`ipVRF` block to the CUDN doubles the consumption per network.

---

## Where to look when it breaks

Ordered by how often each one is the answer.

### RouteAdvertisements never becomes Accepted

```bash
kubectl get routeadvertisements <name> -o jsonpath='{.status.conditions}' | jq
kubectl -n ovn-kubernetes logs -l name=ovnkube-control-plane --tail=200 | grep -i routeadvert
```

The usual cause is `frrConfigurationSelector` not matching `name: receive-all`.

### Accepted, but no generated FRRConfigurations

```bash
kubectl -n frr-k8s-system get frrconfigurations -l k8s.ovn.org/route-advertisements=<name>
kubectl -n ovn-kubernetes logs -l name=ovnkube-control-plane | grep -iE "generat|frrconfig"
```

Generated objects are named `ovnk-generated-*` and annotated `<ra>/<sourceConfig>/<node>`.
If the count is lower than RAs × nodes, some node was skipped — dynamic UDN allocation and
the `NodeHasNetwork` check are the usual reasons, and that is the intended behaviour.

### Generated, but FRR did not take it

```bash
kubectl get frrnodestates -o json \
  | jq '.items[] | {node: .metadata.name, conversion: .status.lastConversionResult, reload: .status.lastReloadResult}'
kubectl -n frr-k8s-system logs ds/frr-k8s-daemon -c frr-k8s --tail=200
kubectl -n frr-k8s-system logs ds/frr-k8s-daemon -c reloader --tail=100
```

`lastConversionResult` and `lastReloadResult` are the two fields that tell you whether
frr-k8s rejected the config or FRR rejected the reload.

### FRR took it, but the peer does not see the route

```bash
kubectl -n frr-k8s-system exec ds/frr-k8s-daemon -c frr -- vtysh -c "show bgp summary"
kubectl -n frr-k8s-system exec ds/frr-k8s-daemon -c frr -- vtysh -c "show bgp ipv4 unicast"
kubectl -n frr-k8s-system exec ds/frr-k8s-daemon -c frr -- vtysh -c "show running-config"
docker exec frr vtysh -c "show bgp neighbors"
```

### Routes learned, but not programmed into OVN

That is the `routeimport` path, and it logs at **V(5)**:

```bash
kubectl -n ovn-kubernetes logs ds/ovnkube-node -c ovnkube-controller | grep -i routeimport

# Kernel side
docker exec ovn-worker ip route show proto bgp
docker exec ovn-worker ip route show vrf blue proto bgp

# OVN side
docker exec ovn-worker ovn-nbctl --format=table find logical_router_static_route
```

### Everything looks right but traffic fails

```bash
# Advertised-UDN isolation is on by default and will drop UDN-to-UDN traffic.
docker exec ovn-worker nft list table inet ovn-kubernetes | grep -A 20 udn-bgp-drop

# Gateway mode — VRF-Lite and EVPN need local.
kubectl get node ovn-worker -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/l3-gateway-config}' | jq
```

### Useful one-liners

```bash
# Every BGP/EVPN object at a glance.
kubectl get routeadvertisements,vteps,clusteruserdefinednetworks -A
kubectl get frrconfigurations,frrnodestates,bgpsessionstates -A

# Follow the control plane while you apply something.
kubectl -n ovn-kubernetes logs -l name=ovnkube-control-plane -f --tail=0

# Full OVN logical topology for one node.
docker exec ovn-worker ovn-nbctl show
```

---

## Metrics and profiles

### pprof needs nothing

`--metrics-enable-pprof` is **unconditional** in the kind image
(`dist/images/ovnkube.sh`), so CPU and heap profiles work on a cluster built with no special
flags at all.

| Component | Port | Serves |
|---|---|---|
| ovnkube-node + ovnkube-controller | `9410` | `ovnkube_node_*`, `ovnkube_controller_*`, `/debug/pprof/*` |
| OVN and OVS exporters (same pod) | `9476` | `ovn_db_*`, `ovn_northd_*`, `ovn_controller_*`, `ovs_vswitchd_*` |
| ovnkube-control-plane | `9411` | `ovnkube_clustermanager_*`, `/debug/pprof/*` |

Ports are echoed at container startup, so if a future change moves them:

```bash
kubectl -n ovn-kubernetes logs ds/ovnkube-node -c ovnkube-controller | grep metrics_bind_address
```

### Scrape by hand

```bash
kubectl -n ovn-kubernetes port-forward ds/ovnkube-node 9410:9410 &
kubectl -n ovn-kubernetes port-forward deploy/ovnkube-control-plane 9411:9411 &

# Did -sm actually take? This is empty without it.
curl -s localhost:9411/metrics | grep ovnkube_clustermanager_workqueue_adds_total

# The BGP/EVPN feature gauges.
curl -s localhost:9411/metrics | grep -E 'route_advertisement_condition|vtep_condition|cluster_user_defined_networks'
```

### Profile the reconcile loop

```bash
# 30-second CPU profile while you apply a batch of RAs.
curl -s "http://localhost:9411/debug/pprof/profile?seconds=30" -o cm-cpu.pprof
go tool pprof -top -nodecount=30 cm-cpu.pprof

curl -s "http://localhost:9411/debug/pprof/heap" -o cm-heap.pprof
go tool pprof -top cm-heap.pprof
```

Create 20 CUDNs and 20 RAs in a loop while the profile runs. If
[bottleneck 2](03-bottlenecks.md#2-generatefrrconfigurations-recomputes-everything) is real,
`generateFRRConfigurations`, the node lister and annotation parsing will dominate the
profile. **That experiment needs nothing from the roadmap** — it works on the cluster you
just built.

### What you still cannot see

The in-cluster Prometheus stack (`./kind.sh -prom`, or the perf lane's
`contrib/install-prometheus-infra.sh`) ships **no ServiceMonitor for ovnkube**, so the series
above never reach Prometheus. Fixing that is [roadmap P0](05-roadmap.md). For a local lab,
`curl` and `pprof` are enough and you can skip the stack entirely.

Transaction latency into OVN is genuinely unmeasurable today —
`pkg/libovsdb/ops/transact.go` has no instrumentation at all. That is
[02 table 2 row 1](02-metrics.md#table-2-missing-with-code-sites) and the highest-leverage
thing to add while you are in here.

---

## The fast iteration loop

Rebuilding the whole cluster for a one-line change wastes twenty minutes. Don't.

### Change Go code, keep the cluster

```bash
# 1. Build the image with your change.
make -C go-controller build
make -C dist/images fedora-image

# 2. Push it into the kind nodes.
kind load docker-image ovn-daemonset-fedora:latest --name ovn

# 3. Restart only what changed.
kubectl -n ovn-kubernetes rollout restart deployment/ovnkube-control-plane   # control plane
kubectl -n ovn-kubernetes rollout restart daemonset/ovnkube-node             # node agents
kubectl -n ovn-kubernetes rollout status  deployment/ovnkube-control-plane
```

Step 1 dominates. For a cluster-manager-only change, restarting just the control-plane
Deployment and leaving the DaemonSet alone is noticeably faster and keeps your pods alive.

### Turn log level up without a rebuild

```bash
kubectl -n ovn-kubernetes set env deployment/ovnkube-control-plane OVN_LOGLEVEL_CONTROLLER=5
```

Or rebuild the cluster with `-ml 5 -nl 5`, which is what the
[bring-up command](#bring-up-the-cluster) already does.

### Exercise a change in a loop

```bash
for i in $(seq 1 20); do
  kubectl apply -f - <<EOF
apiVersion: k8s.ovn.org/v1
kind: RouteAdvertisements
metadata:
  name: ra-$i
spec:
  targetVRF: auto
  advertisements: [PodNetwork]
  nodeSelector: {}
  frrConfigurationSelector:
    matchLabels:
      name: receive-all
  networkSelectors:
  - networkSelectionType: ClusterUserDefinedNetworks
    clusterUserDefinedNetworkSelector:
      networkSelector:
        matchLabels:
          lab: advertised
EOF
done

# Count the blast radius.
kubectl -n frr-k8s-system get frrconfigurations \
  -l 'k8s.ovn.org/route-advertisements' --no-headers | wc -l
```

20 RAs × 3 nodes × 1 source config should give **60** generated objects. If the number
differs, that is a finding — go read
[the derivation](01-methodology.md#generated-frrconfiguration-objects) and work out which
assumption is wrong.

### Benchmarks without a cluster

Two hot paths already have Go benchmarks or are trivially benchmarkable, and no CI job runs
them:

```bash
cd go-controller
go test ./pkg/ovn/routeimport/... -bench=BenchmarkBGPRoutesStreaming -benchmem -run=NONE
```

### Running the real e2e suites

Once something works by hand, the existing suites are the regression net:

```bash
# See .github/workflows/test.yml for the bgp, bgp-no-overlay,
# bgp-loose-isolation and evpn lane definitions.
make -C test control-plane WHAT="Route Advertisements"
```

---

## Teardown

```bash
cd contrib
./kind.sh --delete
```

`--delete` only touches the BGP scaffolding when `-rae` is **also** passed — it reuses the
same flag parsing to know what exists, so plain `--delete` leaves the external containers
running:

```bash
./kind.sh -rae --delete
```

Even then, `destroy_bgp` **stops** the `frr` and `bgpserver` containers rather than removing
them, and removes only the BGP server network. Clean up fully with:

```bash
docker rm -f frr bgpserver
docker network prune -f
```

A stopped-but-present `frr` container is a good way to confuse your next run, because the
install path will try to recreate it. Check with `docker ps -a | grep -E 'frr|bgpserver'`
before rebuilding.

---

## Quick reference

```bash
# Cluster with everything on
cd contrib && ./kind.sh -rae -mne -nse -evpn -adv -sm -gm local -wk 2 -ml 5 -nl 5

# Objects
kubectl get routeadvertisements,vteps,clusteruserdefinednetworks -A
kubectl get frrconfigurations,frrnodestates,bgpsessionstates -A
kubectl -n frr-k8s-system get frrconfigurations -l k8s.ovn.org/route-advertisements

# In-cluster FRR
kubectl -n frr-k8s-system exec ds/frr-k8s-daemon -c frr -- vtysh -c "show bgp vrf all summary"

# External FRR
docker exec frr vtysh -c "show bgp summary"
docker exec frr vtysh -c "show bgp l2vpn evpn route type macip"

# Node datapath
docker exec ovn-worker ip route show proto bgp
docker exec ovn-worker ovn-nbctl --format=table find logical_router_static_route

# Metrics and profiles
kubectl -n ovn-kubernetes port-forward deploy/ovnkube-control-plane 9411:9411 &
curl -s localhost:9411/metrics | grep ovnkube_clustermanager_
curl -s "http://localhost:9411/debug/pprof/profile?seconds=30" -o cm.pprof
```
