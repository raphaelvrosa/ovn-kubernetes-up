# References

## In this repository

| Resource | Covers |
|---|---|
| [Route Advertisements feature doc](../../features/bgp-integration/route-advertisements.md) | user-facing workflows, examples, troubleshooting, known limitations |
| [EVPN feature doc](../../features/bgp-integration/evpn.md) | VTEP and CUDN examples, node devices, FRR config examples, compatibility |
| [No-Overlay feature doc](../../features/bgp-integration/no-overlay.md) | transport, SNAT, managed and unmanaged routing |
| [Multi-VTEP](../../features/multiple-networks/multi-vtep.md) | multiple VTEPs |
| [User Defined Networks](../../features/user-defined-networks/user-defined-networks.md) | CUDN API |
| [Uplinks](../../features/user-defined-networks/uplinks.md) | Uplink feature used by VRF-Lite in shared gateway mode |
| [RouteAdvertisements API reference](../../api-reference/routeadvertisements-api-spec.md), [VTEP API reference](../../api-reference/vtep-api-spec.md), [UserDefinedNetwork API reference](../../api-reference/userdefinednetwork-api-spec.md) | generated field docs |
| [OKEP-5296 BGP](../../okeps/okep-5296-bgp.md) | design and alternatives for route advertisements |
| [OKEP-5088 EVPN](../../okeps/okep-5088-evpn.md) | SVD choice, VTEP design, known scale concerns |
| [OKEP-5259 No-Overlay](../../okeps/okep-5259-no-overlay.md) | no-overlay design |
| [OKEP-6019 VRF-Lite shared gateway with Uplinks](../../okeps/okep-6019-vrf-lite-shared-gateway-external-bridges.md) | Uplink enslavement |
| [OKEP-5552 Dynamic UDN node allocation](../../okeps/okep-5552-dynamic-udn-node-allocation.md) | per-node network activity |
| [Architecture](../../design/architecture.md) | which component runs where |
| [Local testing guide](../local_testing_guide.md) | kind and e2e basics |
| [Perf and scale: BGP and EVPN](../../perf-scale/bgp-evpn/index.md) | benchmarking methodology, metrics, bottleneck hypotheses, results, local lab |

## Code map

| Area | Package or file |
|---|---|
| CRDs | `go-controller/pkg/crd/routeadvertisements/v1`, `go-controller/pkg/crd/vtep/v1`, `go-controller/pkg/crd/userdefinednetwork/v1/{cudn,evpn,spec}.go` |
| Config | `go-controller/pkg/config/config.go` |
| Cluster manager | `go-controller/pkg/clustermanager/{routeadvertisements,managedbgp,vtep,nooverlay}`, `go-controller/pkg/clustermanager/userdefinednetwork/{controller,transport_validation}.go`, `.../notifier` |
| Network manager | `go-controller/pkg/networkmanager/{network_controller,nad_controller}.go`, `go-controller/pkg/util/multi_network.go` |
| Zone controller | `go-controller/pkg/ovn/{gateway,udn_isolation,base_network_controller,base_network_controller_user_defined,base_secondary_layer2_network_controller,layer2_user_defined_network_controller,layer3_user_defined_network_controller,egressip}.go`, `go-controller/pkg/ovn/routeimport` |
| Node | `go-controller/pkg/node/{gateway_udn,gateway_nftables,gateway_shared_intf,gateway,default_node_network_controller,user_defined_node_network_controller}.go`, `go-controller/pkg/node/bridgeconfig/bridgeflows.go`, `go-controller/pkg/node/vrfmanager`, `go-controller/pkg/node/controllers/evpn`, `go-controller/pkg/node/netlinkdevicemanager` |
| Wiring | `go-controller/pkg/clustermanager/clustermanager.go`, `go-controller/pkg/controllermanager/{controller_manager,node_controller_manager}.go` |
| Constants | `go-controller/pkg/types/const.go`, `go-controller/pkg/util/vtep_node_annotations.go` |
| Deployment | `dist/images/ovnkube.sh`, `helm/ovn-kubernetes/values-single-node-zone.yaml`, `contrib/kind.sh`, `contrib/kind-common.sh`, `contrib/frr-k8s/patches` |
| Tests | `go-controller/pkg/**/*_test.go`, `test/crd-integration/testscenario/cudn`, `test/e2e/{route_advertisements*,evpn,no_overlay,vtep_api_validations}.go`, `test/scripts/e2e-cp.sh`, `.github/workflows/test.yml` |

## External

| Resource | Why |
|---|---|
| [frr-k8s](https://github.com/metallb/frr-k8s), [frr-k8s docs](https://metallb.github.io/frr-k8s/) | `FRRConfiguration` API and merge semantics, `FRRNodeState`, `BGPSessionState`, metrics |
| [FRR BGP documentation](https://docs.frrouting.org/en/latest/bgp.html) | address families, VRF instances, `import vrf`, `allowas-in`, route maps |
| [FRR EVPN documentation](https://docs.frrouting.org/en/latest/evpn.html) | `advertise-all-vni`, L3VNI binding, SVD, route targets, IRB |
| [FRR zebra documentation](https://docs.frrouting.org/en/latest/zebra.html) | kernel route installation, VRF handling |
| [Linux VRF](https://docs.kernel.org/networking/vrf.html) | l3mdev rules, enslavement, address loss on master change |
| [Linux VXLAN](https://docs.kernel.org/networking/vxlan.html) | external mode, VNI filtering |
| [RFC 4271](https://datatracker.ietf.org/doc/html/rfc4271), [RFC 4360](https://datatracker.ietf.org/doc/html/rfc4360), [RFC 4364](https://datatracker.ietf.org/doc/html/rfc4364), [RFC 7348](https://datatracker.ietf.org/doc/html/rfc7348), [RFC 7432](https://datatracker.ietf.org/doc/html/rfc7432), [RFC 8365](https://datatracker.ietf.org/doc/html/rfc8365), [RFC 8950](https://datatracker.ietf.org/doc/html/rfc8950), [RFC 9136](https://datatracker.ietf.org/doc/html/rfc9136) | the protocol standards |
| [OVN northbound schema](https://www.ovn.org/support/dist-docs/ovn-nb.5.html) | `NAT` (`match`, `allowed_ext_ips`, `exempted_ext_ips`), `ACL`, `Logical_Router_Static_Route`, `Port_Group`, `Address_Set` |
| [NVIDIA Cumulus EVPN guide](https://docs.nvidia.com/networking-ethernet-software/cumulus-linux/Network-Virtualization/Ethernet-Virtual-Private-Network-EVPN/) | readable operational EVPN reference; CLI matches FRR |
