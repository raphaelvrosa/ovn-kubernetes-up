# 16 — ovnkube-node: the EVPN datapath (devices, VTEP IPs, pod entries)

Package `go-controller/pkg/node/controllers/evpn`, with the device
abstraction in `go-controller/pkg/node/netlinkdevicemanager`. The node agent
creates the Linux devices FRR needs for EVPN, discovers and publishes the
VTEP IP, and programs the per-pod FDB and neighbor entries that make FRR
originate Type-2 routes. Line numbers refer to `evpn_node_controller.go`,
`evpn_pod_controller.go`, `utils.go` and `netlinkdevicemanager/controller.go`,
`netlink_ops.go`.

## Contents

1. [Startup wiring and ordering](#startup-wiring-and-ordering)
2. [The device model](#the-device-model)
3. [Device naming](#device-naming)
4. [The VTEP controller](#the-vtep-controller)
5. [VTEP IP discovery](#vtep-ip-discovery)
6. [The netlink device manager](#the-netlink-device-manager)
7. [The pod controller: FDB and neighbor entries](#the-pod-controller-fdb-and-neighbor-entries)
8. [KubeVirt live migration](#kubevirt-live-migration)
9. [Deletion and cleanup paths](#deletion-and-cleanup-paths)
10. [Complexity](#complexity)
11. [Tests](#tests)

---

## Startup wiring and ordering

`controllermanager/node_controller_manager.go:525-543`:

1. `netlinkdevicemanager.NewController()` creates the device manager with an
   empty desired state.
2. `evpn.NewController(...)` is built with the node address manager of the
   default node network controller (so it must come after that controller is
   initialised), the device manager, the network manager and the OVS client.
3. `evpnController.Start()` runs `initialSync` (`:191-237`), which
   **pre-populates** the device manager's desired state from existing VTEPs
   and cleans stale OVS ports and pod entries.
4. Only then `ndm.Run(...)` starts, whose first action is to delete every
   device carrying the manager's alias that is not in the desired state
   (`cleanupOrphanedDevices`). The ordering is what prevents a restart from
   tearing down and recreating the EVPN bridge, which would drop FRR's
   state.

The controller is stopped before the stop channel closes so the device
manager does not process EVPN events during shutdown (`:594-598`).

---

## The device model

Per VTEP (shared by all networks that reference it):

| Device | Type | Settings | Code |
|---|---|---|---|
| `evbr-<vtep>` | bridge | `vlan_filtering 1`, `vlan_default_pvid 0` | `ensureDevices`, `:398-407` |
| `evx4-<vtep>` (and `evx6-<vtep>` when the VTEP has an IPv6 address) | vxlan | `dstport 4789` (`config.DefaultVXLANPort`), `local <vtep ip>`, `external` (`FlowBased`), `vnifilter`, MTU `config.Default.MTU`; bridge port: `vlan_tunnel on`, `neigh_suppress on`, `learning off`, `isolated on` | `ensureVXLAN`, `:720-755` |
| VID to VNI mappings on the VXLAN port | bridge vlan + vni + tunnel_info | one entry per MAC-VRF and per IP-VRF of every network on the VTEP | `getVIDVNIMappings`, `:521-536`; programmed by `addVIDVNIMapping`, `netlink_ops.go:611-650` |

Per network on the VTEP:

| Device | Type | Settings | Code |
|---|---|---|---|
| `svl3-<net>` | vlan on `evbr-<vtep>`, id = IP-VRF VID, master = network VRF | only when the network has an IP-VRF | `reconcileSVIs`, `:538-583` |
| `svl2-<net>` | vlan on `evbr-<vtep>`, id = MAC-VRF VID, master = network VRF | only with a MAC-VRF; anchor for neighbor entries, no IP | same |
| `ovl2-<net>` | OVS internal port on `br-int`, `external_ids: iface-id=macvrf-<switch>, evpn-vtep=<vtep>`; enslaved to `evbr-<vtep>` as access port `pvid untagged` on the MAC-VRF VID | only with a MAC-VRF | `ensureOVSPort`, `:618-645` |

The `isolated on` setting on the VXLAN port exists because in dual-stack
setups two VXLAN devices share one bridge; without isolation, BUM traffic
arriving from one would be flooded out the other and loop between VTEPs
(`:741-747`).

The mapping programming order matters (`netlink_ops.go:605-610`): VXLAN-side
VID, VNI filter entry and tunnel info first, bridge `self` VID last, so that
the existence of a bridge self VLAN implies a complete mapping, which is what
orphan cleanup relies on.

The feature doc lists the equivalent `ip`, `bridge` and `ovs-vsctl` commands in
[evpn.md, "Devices"](../../features/bgp-integration/evpn.md#devices).

---

## Device naming

`utils.go:45-100`. Names must fit the 15-byte interface name limit:

| Device | Primary form | Fallback |
|---|---|---|
| bridge, VXLAN | `evbr-<vtep>`, `evx4-<vtep>`, `evx6-<vtep>` | `evbr.<8 hex of sha256(vtep)>` |
| SVIs, OVS port | `svl3-<cudn>`, `svl2-<cudn>`, `ovl2-<cudn>` | `svl3.<networkID>` etc. |

The separators (`-` for names, `.` for hashes and IDs) guarantee the two
forms never collide, as the comments at `:58-61` and `:86-90` explain.

---

## The VTEP controller

`NewController` (`:115-176`) wires:

| Source | Trigger | Handler |
|---|---|---|
| VTEP informer | add/delete, spec change (`vtepNeedsUpdate`, `:257-263`) | `reconcile(vtepName)` |
| Node informer | own node's `k8s.ovn.org/vteps` annotation changed, regardless of who changed it (`onNodeUpdate`, `:266-288`) | synthetic key `//vtep-annotation-change`: drop the cached annotation, re-validate every unmanaged VTEP |
| Node address manager | addresses changed | synthetic key `//node-address-change`: reconcile unmanaged VTEPs whose annotated IP is no longer on the node |
| Network manager NAD reconciler (`RegisterNADReconciler`) | any NAD change | `reconcileNAD` (`:447-477`): track NAD key to VTEP name, re-queue the VTEP rate-limited on first sight or removal |
| Pod informer | see pod controller | |

`reconcile(key)` (`:330-391`):

1. Synthetic keys go to `reconcileNodeAddressChange` (`:290-328`).
2. VTEP not found: `deleteVTEPDevices` and remove the annotation entry.
3. `Managed` mode: same cleanup, with a warning.
4. Hybrid overlay on the default VXLAN port is a hard error (`:371-374`).
5. `discoverUnmanagedVTEPIPs`; no IP of any family yet means devices are
   deleted and the controller waits for an address event.
6. `ensureDevices` (bridge, VXLANs with mappings, SVIs) through the device
   manager; `reconcileOVSPorts` directly through libovsdb and netlink. A
   missing link for the OVS port is retried rate-limited (`:382-388`), since
   ovs-vswitchd creates it asynchronously.

`collectEVPNNetworks` (`:492-519`) walks all networks under the network
manager lock on every reconcile to find those referencing the VTEP; the VID
and VNI values come from the NAD netconf through `NetInfo`.

---

## VTEP IP discovery

`discoverUnmanagedVTEPIPs` (`:757-798`), per address family present in the
VTEP CIDRs:

1. Read the controller's local copy of the annotation (`getVTEPsAnnotation`,
   `:824-845`; bootstrapped from the informer, then kept locally to avoid
   stale reads across consecutive reconciles).
2. `resolveVTEPIP` (`:800-822`): keep the annotated IP if it is still one of
   the node's addresses and inside the CIDRs; otherwise pick among the node
   addresses that fall in the CIDRs.
3. `pickVTEPIP` (`:896-931`): one candidate wins directly; several are
   filtered through netlink to drop keepalived VIPs and `IFA_F_SECONDARY`
   addresses, then the lowest address wins for determinism.
4. `setVTEPAnnotation` (`:847-894`) writes `{"<vtep>": {"ips": [...]}}` with
   field manager `node-vtep-controller`, skipping the API call when the entry
   is unchanged, and restoring the previous local value on error.

The address manager is the default node network controller's node IP
manager, so any address on the node (not only the primary interface) can
serve as VTEP IP, which is how a dummy interface works.

---

## The netlink device manager

A generic desired-state reconciler for Linux links
(`netlinkdevicemanager/controller.go`):

| Property | Detail |
|---|---|
| Ownership | every created device carries alias `ovn-k8s-ndm:<type>:<name>` (`link.go:23`); devices without it are never touched (`notOwnedError`) |
| API | `EnsureLink(DeviceConfig)` stores desired config and enqueues; `DeleteLink(name)` removes from the store and enqueues; both asynchronous (`manager.go:78-127`) |
| Worker | single worker, infinite retries, keys `device/<name>`, `fullsync`, `vxlan-sync` (`:40-45`, `:103-121`) |
| Events | link and address netlink subscriptions with 100-entry buffers; a closed channel triggers resubscribe plus `fullsync` (`:167-262`) |
| Periodic | every 60 s with up to 5 s jitter per device: `vxlan-sync` re-enqueues VXLAN devices because VNI filter changes have no subscribable netlink group (`:510-523`); after a resubscribe, `fullsync` |
| Apply | `applyDeviceConfig` (`netlink_ops.go:48`): create or patch mutable fields, delete and recreate on immutable differences, set master and VLAN parent, bridge port settings, addresses, and `syncVIDVNIMappings` (`:462-522`) by diffing desired mappings against bridge VLAN, VNI and tunnel info state |
| Dependencies | a missing master or parent is a `dependencyError`: no rate-limited retry, re-triggered by the link event of the dependency (`:439-446`) |

Only the fields listed in `DeviceConfig` (`manager.go:129-160`) are managed;
anything else must stay zero or `EnsureLink` returns an error.

---

## The pod controller: FDB and neighbor entries

For each **local** pod on a MAC-VRF network, FRR must see the pod's MAC on
the bridge and its IPs in the SVI neighbor table in order to originate
Type-2 MAC and MAC/IP routes, and the bridge must not age the MAC out.
`reconcilePod` (`evpn_pod_controller.go:101-179`):

1. Pod gone: `deletePodNeighbors`.
2. `shouldDeleteNeighbors` (completed pod, or migration source whose target
   is ready): delete.
3. Cached entries for the same pod UID: `ensurePodNeighbors` (idempotent
   re-add, used after migration status changes).
4. Otherwise resolve the namespace's primary NAD, skip if default network or
   no MAC-VRF, parse the pod annotation for MAC and IPs, build
   `neighEntries{svi, ovsPort, vid, ips, mac}`, program if
   `shouldEnsureNeighbors`, and cache.

`ensurePodNeighbors` (`:184-204`):

```text
bridge fdb add <mac> dev ovl2-<net> vlan <macvrf vid> master static       # util.LinkFDBSet
ip neigh add <ip> lladdr <mac> dev svl2-<net> nud permanent               # util.LinkNeighSet, per IP
```

`podNeedsUpdate` (`:43-65`) admits local pods whose OVN annotation changed
or that completed, and any pod whose live migration status changed.
`cleanStalePodEntries` (`:262-322`) runs at startup: for every EVPN network it
lists the pods of the served namespaces, collects desired MACs and IPs, and
deletes permanent neighbors and static master FDB entries that are not
desired.

---

## KubeVirt live migration

During a live migration the same MAC and IP exist briefly on two nodes.
The controller uses `kubevirt.DiscoverLiveMigrationStatus`:

| Node | Condition | Action |
|---|---|---|
| source | target domain ready | `shouldDeleteNeighbors` true: delete entries, FRR withdraws the Type-2 routes from this node |
| target | target domain ready | `shouldEnsureNeighbors` true: program entries, FRR advertises with a higher MAC mobility sequence number |
| target | target domain not ready | do nothing yet |
| either | status changed | `podNeedsUpdate` true even for non-local pods, so the source re-evaluates |

The reason for re-programming on the target only after readiness is given at
`:83-87`: zebra's `extern_learn` entries from the remote Type-2 route would
otherwise overwrite the permanent entries.

---

## Deletion and cleanup paths

| Trigger | Effect |
|---|---|
| VTEP deleted (finalizer released) | `deleteVTEPDevices` (`:647-699`): OVS ports by `evpn-vtep` external ID, SVIs tracked in `svisByBridge`, both VXLAN devices, bridge; annotation entry removed |
| Last NAD of a network removed | `reconcileNAD` re-queues the VTEP; `reconcileSVIs` and `reconcileOVSPorts` delete the network's SVIs and port as no longer desired |
| VTEP loses an address family | the corresponding VXLAN device is deleted (`:420-437`) |
| Controller restart | `initialSync`: pre-populate desired devices, `cleanupStaleOVSPorts` for VTEPs that disappeared while down, `cleanStalePodEntries` |
| Pod deleted | FDB and neighbor removed; tolerant of the OVS port or SVI already being gone (`:206-259`) |

---

## Complexity

- VTEP reconcile: O(networks) under the network manager lock, O(networks)
  device ensures (cheap no-ops when unchanged), one OVS port list per
  reconcile. Every NAD event on the node re-queues the VTEP (rate limited).
- Pod controller: O(1) netlink calls per pod event, single worker; startup
  `cleanStalePodEntries` is O(pods in EVPN namespaces on this node) plus
  two table dumps per network.
- Device manager: 60 s periodic re-enqueue of every VXLAN device, each
  costing a bridge VLAN, VNI and tunnel-info dump: O(mappings) per VTEP.
- Fabric-side: one Type-2 MAC route and one Type-2 MAC/IP route per pod IP,
  so EVPN route count scales with pods, not nodes. See hypothesis 8 and 8b
  in [03-bottlenecks.md](../../perf-scale/bgp-evpn/03-bottlenecks.md#8-evpn-scales-with-pods-not-nodes).

---

## Tests

| File | Covers |
|---|---|
| `evpn_node_controller_test.go` (1,662 lines) | device ensure and delete sequences through a mocked device manager, VTEP IP discovery and annotation writes, address change and annotation repair, NAD tracking, OVS port lifecycle |
| `evpn_pod_controller_test.go` (522 lines) | FDB and neighbor programming, deletion tolerance, migration source and target behaviour, stale entry cleanup |
| `utils_test.go` | naming and hash fallbacks |
| `netlinkdevicemanager/*_test.go` | the generic device reconciler, mapping diffs, ownership |
| `test/e2e/evpn.go`, `test/e2e/route_advertisements.go:3012-3017` | end-to-end EVPN connectivity for L3 IP-VRF, L2 MAC-VRF and L2 MAC-VRF plus IP-VRF, with shared and per-network VTEPs; `test/e2e/vtep_api_validations.go` for the VTEP API |
