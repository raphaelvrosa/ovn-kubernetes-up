# BGP Route Advertisements and EVPN: Engineering Guide

A code-level guide to the BGP route advertisement, VRF-Lite, no-overlay and
EVPN features of OVN-Kubernetes, written for engineers who need to review
pull requests, reason about scale, and change these controllers. Every claim
is tied to the code at upstream `master` commit `654e2915f` (2026-11-10);
line numbers are accurate at that commit, symbol names are the durable part.

## Contents

1. [Summary](#summary)
2. [How the feature is put together](#how-the-feature-is-put-together)
3. [Index](#index)
4. [Reading paths](#reading-paths)
5. [Relationship to the other documentation](#relationship-to-the-other-documentation)
6. [Conventions](#conventions)

---

## Summary

OVN-Kubernetes does not speak BGP. It delegates to FRR, run by frr-k8s, and
expresses intent through two of its own CRDs plus fields on a third:

| Object | Owner | Purpose |
|---|---|---|
| `RouteAdvertisements` | user (or the managed BGP controller) | which networks to export, from which nodes, in which VRF, using which `FRRConfiguration` templates |
| `VTEP` | user | where nodes' VXLAN tunnel endpoint addresses come from |
| `ClusterUserDefinedNetwork.spec.network.transport`, `.evpn`, `.noOverlay` | user | per-network transport: Geneve overlay, native routing, or EVPN |

Three processes act on them:

| Process | Does |
|---|---|
| ovnkube-cluster-manager | validates the intent, generates one `FRRConfiguration` per RA, node and template, annotates the networks' NADs with the RAs that select them, allocates EVPN VLAN IDs, validates VTEPs, and for managed no-overlay creates the full-mesh template and RAs |
| ovnkube-controller (zone) | reads the advertised state through the network manager and adapts OVN: conditional SNAT, isolation ACLs, no transit switch for EVPN and no-overlay, MAC-VRF ports for EVPN Layer2, and imports BGP-learned kernel routes into gateway routers |
| ovnkube-node | creates VRFs, routes, policy rules, nftables and bridge flows for advertised networks; discovers VTEP addresses, builds the EVPN bridge, VXLAN, SVIs and OVS ports, and programs per-pod FDB and neighbor entries so FRR originates EVPN routes |

The single hand-off between the cluster manager and the datapath processes
is the NAD annotation `k8s.ovn.org/route-advertisements`; the single hand-off
to FRR is the set of generated `FRRConfiguration` objects.

---

## How the feature is put together

```mermaid
flowchart LR
    subgraph intent
        RA[RouteAdvertisements]
        VT[VTEP]
        CU[CUDN transport/evpn]
        FS[FRRConfiguration templates]
    end
    subgraph cluster manager
        RC[RA controller]
        VC[VTEP controller]
        CC[CUDN controller]
        MB[managed BGP]
    end
    subgraph outputs
        FG[FRRConfiguration ovnk-generated-*]
        NAD[NAD annotation]
        ST[statuses]
    end
    subgraph node pod
        NM[network manager]
        ZC[ovnkube-controller: SNAT, ACLs, routeimport, EVPN topology]
        ND[ovnkube-node: VRF, rules, nftables, flows, EVPN devices, pod entries]
    end
    FK[frr-k8s / FRR]
    RA --> RC
    FS --> RC
    VT --> VC
    CU --> CC
    MB --> RA
    MB --> FS
    RC --> FG
    RC --> NAD
    RC --> ST
    VC --> ST
    CC --> ST
    FG --> FK
    NAD --> NM
    NM --> ZC
    NM --> ND
    ND -. k8s.ovn.org/vteps .-> RC
    ND -. VRF, SVIs, FDB, neighbors .-> FK
    FK -. kernel routes .-> ZC
```

---

## Index

| Document | Contents |
|---|---|
| [Glossary](glossary.md) | terms, object names, annotations |
| [01 — Concepts](01-concepts.md) | VRFs, BGP, FRR and frr-k8s, the three advertisement shapes, EVPN, SVD, IRB, no-overlay, gateway modes |
| [02 — Configuration and running modes](02-configuration-and-running-modes.md) | flags, config sections, validation, which controller runs where, deployment plumbing, kind |
| [03 — API: RouteAdvertisements](03-api-routeadvertisements.md) | fields, CEL, controller-time validation, status, annotations, relationships |
| [04 — API: VTEP](04-api-vtep.md) | fields, CEL, status reasons, the node annotation, finalizer |
| [05 — API: CUDN transport, EVPN and no-overlay](05-api-cudn-transport.md) | transport fields, CEL matrix, `TransportAccepted`, `vrfName`, NAD netconf, `NetInfo` accessors |
| [06 — FRRConfiguration contract](06-frrconfiguration-contract.md) | fields read and written, naming, merge semantics, raw FRR config, FRR features relied upon |
| [07 — Cluster manager: RouteAdvertisements controller](07-cm-routeadvertisements-controller.md) | sub-controllers, the reconcile cycle, generation in detail, apply, error classes, VTEP conflicts, complexity, tests |
| [08 — Cluster manager: managed BGP controller](08-cm-managed-bgp-controller.md) | base template, managed RAs, filters |
| [09 — Cluster manager: VTEP controller](09-cm-vtep-controller.md) | validations, finalizer, status |
| [10 — Cluster manager: CUDN transport validation and no-overlay validation](10-cm-cudn-transport-and-no-overlay.md) | `TransportAccepted` decision tree, notifiers, VID and VNI allocation, event-only validator |
| [11 — Network manager propagation](11-network-manager-propagation.md) | from NAD annotation and RA status to `NetInfo`, deadlock avoidance, downstream triggers |
| [12 — ovnkube-controller: advertised networks](12-ovnkube-controller-advertised-networks.md) | SNAT conditionals, isolation ACLs, no-overlay GR routes, EgressIP |
| [13 — ovnkube-controller: route import](13-ovnkube-controller-route-import.md) | netlink to LRSR reconciliation, filters, Uplinks |
| [14 — ovnkube-controller: EVPN topology](14-ovnkube-controller-evpn-topology.md) | what is not created, MAC-VRF port, ACLs, multicast, packet walks |
| [15 — ovnkube-node: VRFs and host datapath](15-ovnkube-node-vrf-and-host-datapath.md) | VRF manager, routes, rules, nftables, bridge flows, service marks, Uplink enslavement |
| [16 — ovnkube-node: EVPN datapath](16-ovnkube-node-evpn-datapath.md) | devices, naming, VTEP controller, IP discovery, device manager, pod controller, live migration |
| [17 — End-to-end workflows](17-end-to-end-workflows.md) | eleven traced scenarios from object creation to kernel state |
| [18 — Performance and scale](18-performance-and-scale.md) | dimensions, object counts, cost models per process, ceilings, concurrency, observability, measured results |
| [19 — Testing and CI](19-testing-and-ci.md) | unit, CRD integration and e2e suites, lane selection, local runs, gaps |
| [20 — Review checklist and recipes](20-review-checklist.md) | per-area checklists and change recipes |
| [References](references.md) | in-repo docs, code map, external standards and documentation |

---

## Reading paths

| Goal | Read |
|---|---|
| Understand the feature from zero | glossary, 01, 02, then 03 to 06, then 17 |
| Review a cluster manager PR | 07 (or 08, 09, 10), 06, 20 |
| Review a datapath PR | 11, then 12 to 16 as relevant, 20 |
| Debug "the RA is Accepted but nothing is advertised" | 07 validation table, 06, 17 W1, feature doc troubleshooting |
| Debug "pods on an EVPN network cannot reach each other" | 17 W4 and W5, 16, 14 |
| Estimate the cost of a change at scale | 18, then `docs/perf-scale/bgp-evpn/03-bottlenecks.md` |
| Add a test and make sure CI runs it | 19 |

---

## Relationship to the other documentation

- `docs/features/bgp-integration/` is the **user** documentation: how to
  configure the features, examples, troubleshooting commands, known
  limitations. This set does not repeat it; it links to it.
- `docs/okeps/` records the **design** decisions and alternatives.
- `docs/perf-scale/bgp-evpn/` is the **benchmarking** set: methodology,
  metrics, hypotheses and measured results. This set references its
  hypotheses from the code side and defers to it for numbers.
- `docs/api-reference/` holds the generated field reference.

---

## Conventions

- Code references are `path:line` or `path:start-end` relative to the
  repository root, with `go-controller/pkg/...` shortened to `pkg/...` inside
  pages that name their package up front.
- "Zone controller" means ovnkube-controller; "node" means ovnkube-node;
  "cluster manager" means ovnkube-cluster-manager.
- Error messages and status reasons are quoted verbatim from the code.
- Nothing here was measured unless it says "measured" and cites the
  perf-scale page it comes from.
