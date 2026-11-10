# 13 — ovnkube-controller: the route import controller

Package `go-controller/pkg/ovn/routeimport`. It copies BGP-learned routes
from a node's kernel routing tables into the OVN gateway router of each
network as `Logical_Router_Static_Route` rows, so that pod egress in shared
gateway mode (which never touches the host stack) follows the routes FRR
learned. In local gateway mode the kernel routes are used directly and this
controller is only registered for the default network and Layer3 UDNs; Layer2
UDNs register only in shared gateway mode.

Line numbers refer to `route_import.go` at upstream `master` `654e2915f`.

## Contents

1. [Interface and lifecycle](#interface-and-lifecycle)
2. [Who registers networks](#who-registers-networks)
3. [Event sources](#event-sources)
4. [`syncNetwork`: the reconcile](#syncnetwork-the-reconcile)
5. [Route selection rules](#route-selection-rules)
6. [Table discovery and the `tables` hint](#table-discovery-and-the-tables-hint)
7. [OVN side](#ovn-side)
8. [Uplink filtering](#uplink-filtering)
9. [Failure modes and resubscription](#failure-modes-and-resubscription)
10. [Complexity and timing](#complexity-and-timing)
11. [Tests](#tests)

---

## Interface and lifecycle

```go
// route_import.go:48-72
type Manager interface {
	AddNetwork(network util.NetInfo) error
	NeedsReconciliation(network util.NetInfo) bool
	ReconcileNetwork(name string) error
	ForgetNetwork(name string)
}
type Controller interface { Manager; Start() error; Stop() }
```

`New(node, nbClient, uplinkStateLister)` (`:74-96`) builds one controller
per ovnkube-controller process with a single-worker reconciler keyed by
network name. `Start` subscribes to netlink and starts the worker
(`:187-191`); `Stop` cancels the subscriptions (`:193-197`).

Internal state (`:98-113`): `networks` (name to `NetInfo`), `networkIDs` (ID
to name) and `tables` (kernel table to network ID, a hint only). All guarded
by one `RWMutex`.

---

## Who registers networks

| Caller | When | Code |
|---|---|---|
| `DefaultNetworkController.Start` | always (route advertisements enabled) | `ovn/default_network_controller.go:648-652`; `ForgetNetwork` at `:370-372` |
| `Layer3UserDefinedNetworkController` start | always | `ovn/layer3_user_defined_network_controller.go:560-564`; forget at `:385-387` |
| `BaseLayer2UserDefinedNetworkController` start | **shared gateway mode only** | `ovn/base_secondary_layer2_network_controller.go:151-156`; forget at `:55-57` |
| `BaseNetworkController.doReconcile` | when `NeedsReconciliation` (Uplink changed) | `ovn/base_network_controller.go:250-255` |

`AddNetwork` rejects duplicates by ID and by name and immediately schedules a
reconcile (`:115-145`). For the default network it pins the main table in the
hint map.

---

## Event sources

Two goroutines started by `subscribe` (`:199-254`):

| Subscription | Filter | Action |
|---|---|---|
| `RouteSubscribeWithOptions` with `ListExisting: true` (`:624-636`) | `update.Protocol == unix.RTPROT_BGP` (`:256-259`) | find the network for `update.Table` in the hint map; if known, schedule it (`:260-265`) |
| `LinkSubscribeWithOptions` with `ListExisting: true` (`:638-649`) | link is a `*netlink.Vrf` (`:268-272`) | map VRF name to network (CUDN name form or ID-derived form), on `RTM_NEWLINK` update the table hint and schedule (`:274-308`) |

Both channels are buffered (100) and recreated every second if they are nil
or closed (`subscribePeriod`). A closed link channel also clears the `tables`
hint so it is rebuilt from `ListExisting` events (`:244-247`).

Scheduling always goes through `reconcile(network)`, which is
`ReconcileAfter(network, 500ms)` (`:310-312`, `reconcileDelay`). The
workqueue coalesces keys, so a burst of route updates for one network results
in one sync 500 ms after the first.

---

## `syncNetwork`: the reconcile

`:327-433`.

```text
info := networks[network]; return if unknown
table := getRoutingTableForNetwork(network)      -- netlink is the source of truth, not the hint
return if no VRF yet (noTable)
tables[table] = network ID                        -- refresh hint (handles link-before-AddNetwork ordering)
ignoreSubnets := network subnets, unless transport is no-overlay
routeLinkIndex := Uplink bridge ifindex, or 0, or noRouteLinkIndex
expected := getBGPRoutes(table, ignoreSubnets, routeLinkIndex)   -- kernel dump, filtered
router := GR name for this node; outport := rtoe-<router>
actual, uuids := getOVNRoutes(router)             -- LRSRs owned by this controller
adds := expected - actual; deletes := actual - expected
if none: return
ops := CreateLogicalRouterStaticRoutesOps(adds with ExternalIDs owner=RouteImport, OutputPort=outport)
ops += DeleteLogicalRouterStaticRoutesOps(deletes by UUID)
TransactAndCheck(ops)
```

It is a full set reconciliation: there is no incremental application of the
netlink event itself. The event only decides **which** network to re-sync.

---

## Route selection rules

`getBGPRoutes` (`:435-460`) and `routesFromNetlinkRoute` (`:584-622`):

| Rule | Code |
|---|---|
| Only `proto bgp` routes in the network's table, both families | filter `Protocol: RTPROT_BGP, Table: table` |
| Skip routes whose destination is inside any of the network's own subnets, **except** for no-overlay networks | `ignoreSubnets` (`:358-365`, `:445-448`) |
| Multipath routes become one `(dst, gw)` pair per next hop | `:602-612` |
| Routes without a gateway (`<nil>` or empty) are skipped, so directly connected BGP routes are not imported | `validIP` (`:588-593`, `:617-621`) |
| With an Uplink, only next hops on the Uplink bridge's ifindex are kept; `noRouteLinkIndex` imports nothing | `:604-606`, `:614-616`, `:585-587` |
| An interrupted dump aborts the sync rather than applying a partial set | `:452-456` |

Why pod subnets are skipped for overlay networks: a BGP peer that
re-advertises the cluster's own subnets (a route reflector does) would
otherwise install a route to the node's own pods via the fabric. For
no-overlay networks that is precisely the desired east/west route.

---

## Table discovery and the `tables` hint

`getRoutingTableForNetwork` (`:537-564`): the default network is
`RT_TABLE_MAIN`; a UDN's table is read from its VRF link
(`util.GetNetworkVRFName`), returning `noTable` if the link does not exist yet
or is not a VRF. The VRF is created by ovnkube-node, which runs in the same
pod as ovnkube-controller in interconnect mode, so there is a startup window
where the network is registered but the table is unknown; the link event
closes it (`syncLinkUpdate`).

The `tables` map is only a hint for mapping route events to networks; the
comment at `:336-339` explains the trade-off: the reconcile re-reads netlink
rather than maintaining a precise cache.

---

## OVN side

Imported routes are `nbdb.LogicalRouterStaticRoute` rows on the network's
gateway router (`GR_<network>_<node>`) with:

- `ip_prefix` = destination, `nexthop` = gateway from the kernel route,
- `output_port` = `rtoe-GR_...` (`types.GWRouterToExtSwitchPrefix + router`),
  set so IPv6 link-local next hops resolve (`:377-379`),
- `external_ids["k8s.ovn.org/owner-controller"] = "RouteImport"`
  (`controllerExternalIDKey`, `controllerName`), which is also the predicate
  used to list "our" routes (`:513-515`).

Routes not owned by this controller on the same router (the default route,
the join subnet, Uplink routes) are never touched.

The feature doc shows the result with `ovn-nbctl lr-route-list` in
[route-advertisements.md](../../features/bgp-integration/route-advertisements.md#route-import).

---

## Uplink filtering

`getRouteLinkIndexFilter` (`:466-506`): for a network with an Uplink, the
gateway router is attached only to the selected Uplink bridge, so only routes
whose next hop goes out through that bridge are valid. The bridge name comes
from the resolved `UplinkState` (`status.ovsBridge.name`); until it is
resolved, `noRouteLinkIndex` makes the import empty. `NeedsReconciliation`
(`:163-174`) reports a change of Uplink name so the base controller re-syncs.

---

## Failure modes and resubscription

- Netlink subscription errors are logged and retried every second.
- A failed NB transaction is returned; the reconciler retries with
  rate-limited backoff (`DefaultTypedControllerRateLimiter`), and the next
  sync recomputes the full set.
- A VRF deletion is ignored on purpose: the network is expected to be
  forgotten through `ForgetNetwork` (`:293-297`).
- If zebra withdraws and re-adds routes during a reload, the 500 ms
  coalescing window absorbs the flap only if both events arrive within it.

---

## Complexity and timing

Per sync of one network: one filtered netlink dump of the table (O(routes in
table)), one NB lookup of the router's static routes (O(routes on router)),
two set differences, and one transaction of size O(delta). Every BGP route
event in a tracked table costs one such sync, so with R learned routes per
VRF and a fabric that flaps, the dump cost is O(R) per 500 ms per network.
With N networks sharing one fabric event (a peer reset re-sending all
routes), that is N dumps. The perf-scale set lists this as hypothesis 7 in
[03-bottlenecks.md](../../perf-scale/bgp-evpn/03-bottlenecks.md#7-routeimport-full-dump-and-diff-per-network)
and proposes a histogram for the sync duration; today the only signal is the
V(5) log line `Reconciled network ... took`.

---

## Tests

`route_import_test.go` (989 lines) uses a fake netlink implementation and a
libovsdb test harness:

| Test | Covers |
|---|---|
| `TestBGPRoutesDiscardsInterruptedDump` (`:56`) | an interrupted dump is not applied |
| `TestBGPRoutesStreamsBothFamilies` (`:69`) | IPv4 and IPv6 routes from one dump |
| `Test_controller_syncNetwork` (`:108`) | table discovery, pod subnet filtering for overlay versus no-overlay, multipath expansion, Uplink ifindex filtering, delete of stale LRSRs and ownership predicates |
| `Test_controller_syncRouteUpdate` (`:665`), `Test_controller_syncLinkUpdate` (`:743`) | event to network mapping and the `tables` hint |
| `Test_controller_subscribe` (`:885`) | resubscription behaviour | End to end, the import path is
exercised by every `BGP:` suite that reaches an external server through a
learned route (`test/e2e/route_advertisements.go`).
