# 20 — Review checklist and change recipes

A checklist for reviewing or authoring changes to the BGP and EVPN code, and
recipes for the common kinds of change. Each item names the page that
explains why.

## Contents

1. [General](#general)
2. [API changes](#api-changes)
3. [RouteAdvertisements controller changes](#routeadvertisements-controller-changes)
4. [FRR configuration output changes](#frr-configuration-output-changes)
5. [Network manager and NetInfo changes](#network-manager-and-netinfo-changes)
6. [OVN datapath changes](#ovn-datapath-changes)
7. [Node datapath changes](#node-datapath-changes)
8. [EVPN node changes](#evpn-node-changes)
9. [Tests and CI](#tests-and-ci)
10. [Recipes](#recipes)

---

## General

- [ ] The change is gated by `util.IsRouteAdvertisementsEnabled()` or
      `util.IsEVPNEnabled()`, not by raw config fields ([02](02-configuration-and-running-modes.md#derived-predicates)).
- [ ] Gateway mode assumptions are explicit: EVPN and VRF-Lite are local
      gateway features; route import matters in shared gateway mode
      ([01](01-concepts.md#gateway-modes-and-why-they-matter-here)).
- [ ] Interconnect-only resources are guarded by `hasInterconnectTransport()`
      ([14](14-ovnkube-controller-evpn-topology.md)).
- [ ] No new `ReconcileAll()` trigger is added without a reason stated in the
      PR; every one multiplies cluster-wide work by the RA count
      ([18](18-performance-and-scale.md#control-plane-cost-model)).
- [ ] Errors are classified: `errConfig` and `errPending` for user-fixable or
      self-resolving states (no retry), operational errors otherwise
      ([07](07-cm-routeadvertisements-controller.md#validation-and-error-classes)).
- [ ] Anything written to FRR passes through a validating constructor in
      `validation.go` ([06](06-frrconfiguration-contract.md#raw-frr-configuration)).

---

## API changes

- [ ] CEL markers updated, CRD regenerated (`make -C go-controller codegen`),
      API reference regenerated (`make -C docs generate-api-reference`).
- [ ] A `test/crd-integration` scenario for each new rule ([19](19-testing-and-ci.md#crd-integration-tests)).
- [ ] New RA spec fields: `raNeedsUpdate` relies on generation; the CUDN
      notifier filter and the network manager filter enumerate fields
      ([03](03-api-routeadvertisements.md#review-notes)).
- [ ] New CUDN EVPN fields: CRD, CNI `NetConf`, NAD template, both `NetInfo`
      implementations and the mock, RA validation ([05](05-api-cudn-transport.md#review-notes)).
- [ ] Route target or VNI grammar: `evpn.go` markers and
      `validateRouteTarget` / `validateVNI` change together.
- [ ] VTEP annotation format: writer (node), `ValidateNodeVTEPs`,
      `ParseNodeVTEPs`, both readers' tests ([04](04-api-vtep.md#review-notes)).

---

## RouteAdvertisements controller changes

- [ ] Output is deterministic: inputs sorted, maps iterated through sorted
      keys, neighbors sorted in raw config. A non-deterministic spec rewrites
      every object on every reconcile.
- [ ] The `<ra>/<source>/<node>` key annotation and the RA label are set on
      every generated object; `updateFRRConfigurations` keys on them.
- [ ] Generated objects are never selectable as templates (name prefix check).
- [ ] `targetVRF` semantics (`""`, `auto`, literal) are handled in the router
      match switch and nowhere else.
- [ ] Dynamic allocation: a per-node skip uses `nm.NodeHasNetwork`, and the
      `auto` completeness check tolerates inactive networks.
- [ ] EVPN: default-VRF router required; IP-VRF router found or synthesised;
      VTEP host routes added; conflicts across RAs considered.
- [ ] Partial failure semantics preserved: `errPending` and `errInvalidVTEP`
      still apply the sanitized desired state; unsafe input deletes
      unvalidated objects.
- [ ] A row added to `TestController_reconcile` with the full expected
      `FRRConfiguration`.

---

## FRR configuration output changes

- [ ] frr-k8s merge semantics still hold: same ASN per VRF across objects,
      neighbors declared consistently, raw priority 10
      ([06](06-frrconfiguration-contract.md#merge-semantics-relied-upon)).
- [ ] The FRR keyword is documented at docs.frrouting.org for the FRR
      version frr-k8s ships; EVPN keywords need FRR 10+.
- [ ] Unnumbered neighbors appear in both unicast families.
- [ ] `rawconfig_test.go` golden strings updated.
- [ ] Verified against a kind cluster: `FRRNodeState.status.lastReloadResult`
      is `success`.

---

## Network manager and NetInfo changes

- [ ] `setAdvertisements` and `preserveAdvertisements` keep the
      "controller must start even if the RA is not accepted" guarantee
      ([11](11-network-manager-propagation.md#deadlock-avoidance-preserveadvertisements)).
- [ ] The two predicates `IsPodNetworkAdvertisedAtNode` and
      `IsPodNetworkAdvertisedAtNodeDefaultVRF` remain the only decision
      points; no-overlay and EVPN are always advertised by transport.
- [ ] `canReconcile` includes any new immutable field; `needsReconcile`
      includes any new dynamic field.

---

## OVN datapath changes

- [ ] SNAT rows use `GetNetworkScopedClusterSubnetSNATMatch` and the shared
      node IP address set with a back-reference
      ([12](12-ovnkube-controller-advertised-networks.md#snat-rules-for-advertised-networks)).
- [ ] Isolation ACL priorities 1050 and 1100 and the isolation tier are
      unchanged, or the precedence against network policy is re-argued.
- [ ] Route import: owner external ID `RouteImport`, output port set, pod
      subnets ignored except for no-overlay ([13](13-ovnkube-controller-route-import.md)).
- [ ] Layer2 EVPN: MAC-VRF port name derived from the switch name on both the
      zone and node sides.

---

## Node datapath changes

- [ ] The four routing-domain decisions stay consistent:
      `getDefaultRouteExceptIfVRFLite`, `constructUDNVRFIPRules`,
      `shouldAddUDNServiceMarkRules`, `shouldEnslaveUplinkGatewayToVRF`
      ([15](15-ovnkube-node-vrf-and-host-datapath.md#review-notes)).
- [ ] Managed routes carry `types.OVNKProtocol`.
- [ ] `netConfig.Advertised` is stored before the flow cache is rebuilt.
- [ ] nftables set elements carry the network name comment used for cleanup.
- [ ] Bridge flow priorities 300, 104, 550, 490 and table 2 priority 200 keep
      their relative order against egress IP flows (103, 105).

---

## EVPN node changes

- [ ] Device desired state is pre-populated in `initialSync` before the
      device manager runs, or a restart will delete the bridge
      ([16](16-ovnkube-node-evpn-datapath.md#startup-wiring-and-ordering)).
- [ ] Only `DeviceConfig`-managed fields are set; others are rejected.
- [ ] VID/VNI mapping order: VXLAN side first, bridge `self` last.
- [ ] VTEP annotation writes use `node-vtep-controller` and are no-ops when
      unchanged; the local cache is restored on error.
- [ ] Pod entries: migration source and target behaviour preserved; cleanup
      tolerates missing devices.
- [ ] New device names fit in 15 bytes with the hash or ID fallback.

---

## Tests and CI

- [ ] Unit tests for the package changed, following the existing table or
      Ginkgo style.
- [ ] E2E: the spec carries the right feature label and its describe text
      matches the lane's `WHAT` focus; the lane's flags satisfy its
      preconditions ([19](19-testing-and-ci.md#how-ci-lanes-select-bgp-and-evpn-specs)).
- [ ] The PR states which lane executes the new or changed spec and links
      the CI log showing it ran, per `AGENTS.md`.
- [ ] Docs under `docs/features/bgp-integration/` updated if user-visible
      behaviour changed; this developer set updated if a controller's inputs,
      outputs or ordering changed.

---

## Recipes

### Add a new validation of RA input

1. Add the check in `generateFRRConfigurations` at the matching phase with
   the right sentinel (`errConfig` for user errors, `errUnsafeFRRInput` if
   the value reaches FRR).
2. If the value comes from a NAD field, make sure the controller reads it
   through `NetInfo` so hand-written NADs are covered.
3. Add the message to the table in
   [07](07-cm-routeadvertisements-controller.md#validation-and-error-classes)
   and to the troubleshooting section of `route-advertisements.md`.
4. Add a `TestController_reconcile` row expecting `Accepted=False` with the
   reason and no generated objects (or the sanitized set).

### Add a field to the generated FRRConfiguration

1. Decide whether it is per router, per neighbor or raw.
2. Set it in `generateFRRConfiguration` after the struct copy; for raw
   config, extend the matching `gen*Section` function and the structure
   comment at the top of `rawconfig.go`.
3. Keep determinism; extend the golden strings in `rawconfig_test.go`.
4. Verify the frr-k8s merge result on a kind cluster.

### Make the node react to a new advertised-state dimension

1. Add the predicate next to `IsPodNetworkAdvertisedAtNode` in `util`.
2. Include it in `UserDefinedNodeNetworkController.shouldReconcileNetworkChange`
   and in `updateAdvertisementStatus`.
3. Apply it in `reconcileAdvertisedState` and in `addNetworkWithResolvedUplink`
   (initial programming).
4. Mirror it in the zone controller if OVN state depends on it.

### Add a new EVPN device per network

1. Extend `evpnNetworkInfo` and `collectEVPNNetworks`.
2. Ensure it in `ensureDevices` (device manager) or `reconcileOVSPorts`
   (OVS), track it for deletion (`svisByBridge` pattern), and delete it in
   `deleteVTEPDevices`.
3. Give it a name through `getEVPNNetworkDeviceName` with a new prefix.
4. Pre-populate it in `initialSync`.
