#!/usr/bin/env bash
#
# Capture the BGP/EVPN scale quantities that cannot be Prometheus metrics.
# Invoked from kube-burner job hooks; see
# docs/perf-scale/bgp-evpn/02-metrics.md table 3.
#
# NOT YET EXECUTED AGAINST A CLUSTER.
#
# Usage: collect-bgp-state.sh <label>
# Writes bgp-state-<label>.json into ${ARTIFACT_DIR:-.}.

set -euo pipefail

LABEL="${1:-unlabelled}"
OUT_DIR="${ARTIFACT_DIR:-.}"
OUT="${OUT_DIR}/bgp-state-${LABEL}.json"
FRR_CONTAINER="${FRR_CONTAINER_NAME:-frr}"

mkdir -p "${OUT_DIR}"

# FRRNodeState.status.runningConfig is the entire rendered FRR config as a
# single string, per node. It grows with VRF count and the default etcd object
# size limit is 1.5 MB. This is the measurement for that ceiling.
frrnodestate_sizes() {
  kubectl get frrnodestates -o json 2>/dev/null \
    | jq -c '[.items[] | {node: .metadata.name,
                          bytes: (.status.runningConfig // "" | length),
                          reload: .status.lastReloadResult}]' \
    || echo 'null'
}

# Generated FRRConfigurations: count and total serialised size.
# generated = RAs x nodes x matching source FRRConfigurations.
generated_frrconfigs() {
  kubectl get frrconfigurations -A -l k8s.ovn.org/route-advertisements -o json 2>/dev/null \
    | jq -c '{count: (.items | length),
              total_bytes: ([.items[] | tojson | length] | add // 0),
              max_bytes:   ([.items[] | tojson | length] | max // 0)}' \
    || echo 'null'
}

# One BGPSessionState per (node, peer, VRF). Cardinality is multiplicative.
bgpsessionstates() {
  kubectl get bgpsessionstates -A -o json 2>/dev/null \
    | jq -c '{count: (.items | length),
              down: ([.items[] | select(.status.bgpStatus != "Established")] | length)}' \
    || echo 'null'
}

# VTEP status carries a per-node IP map, flagged as non-scaling at ~5000 nodes.
vtep_sizes() {
  kubectl get vteps -o json 2>/dev/null \
    | jq -c '[.items[] | {name: .metadata.name, bytes: (tojson | length)}]' \
    || echo 'null'
}

# The far endpoint of route_advertised_latency: nothing in-cluster observes the
# peer's table. For EVPN this is where the Type-2 route count comes from.
peer_routes() {
  if command -v docker >/dev/null && docker inspect "${FRR_CONTAINER}" >/dev/null 2>&1; then
    docker exec "${FRR_CONTAINER}" vtysh -c "show bgp vrf all summary json" 2>/dev/null \
      || echo 'null'
  else
    echo 'null'
  fi
}

peer_evpn_routes() {
  if command -v docker >/dev/null && docker inspect "${FRR_CONTAINER}" >/dev/null 2>&1; then
    docker exec "${FRR_CONTAINER}" \
      vtysh -c "show bgp l2vpn evpn route type macip json" 2>/dev/null \
      | jq -c '{type2_count: ([.. | objects | select(has("routeType"))] | length)}' \
      || echo 'null'
  else
    echo 'null'
  fi
}

jq -n \
  --arg label "${LABEL}" \
  --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson frrnodestates "$(frrnodestate_sizes)" \
  --argjson generated "$(generated_frrconfigs)" \
  --argjson sessions "$(bgpsessionstates)" \
  --argjson vteps "$(vtep_sizes)" \
  --argjson peer "$(peer_routes)" \
  --argjson peer_evpn "$(peer_evpn_routes)" \
  '{label: $label, timestamp: $ts, frrNodeStates: $frrnodestates,
    generatedFRRConfigurations: $generated, bgpSessionStates: $sessions,
    vteps: $vteps, peerBGPSummary: $peer, peerEVPN: $peer_evpn}' \
  > "${OUT}"

echo "wrote ${OUT}"
