#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Create a multi-cluster (hub + N spokes) KIND environment for Plexus.
#
# All clusters share the default "kind" Docker network (172.18.0.0/16). Nodes
# from every cluster are on the same L2 segment, so cross-cluster traffic
# requires no iptables rules, no secondary interfaces, and no routing hacks.
# OVN-K bridges the kind interface as breth0 and uses 172.18.0.x IPs as VTEP
# endpoints. EVPN Geneve tunnels work directly between nodes across clusters.
#
# Usage:
#   contrib/plexus-kind-multi.sh [OPTIONS]
#
# Options:
#   --hub NAME           Hub cluster name (default: plexus-hub)
#   --spokes N           Number of spoke clusters (default: 1)
#   --spoke-prefix P     Spoke cluster name prefix (default: plexus-spoke)
#   --workers N          Worker nodes per cluster (default: 1)
#   --skip-build         Reuse existing OVN image
#   --force-build        Rebuild OVN image even if it exists
#   --delete             Tear down all clusters and exit
#
# Topology (hub + 2 spokes, CIDR_BASE=1):
#
# ╔════════════════════════════════════════════════════════════════════════════════════════════════╗
# ║  DOCKER HOST — single "kind" network 172.18.0.0/16 (gw: 172.18.0.1)                         ║
# ║  All cluster nodes are on the same L2. No iptables rules or routing hacks needed.             ║
# ╚══════════════╤═══════════════════════════╤══════════════════════════╤══════════════════════════╝
#                │                           │                          │
#   ┌────────────▼──────────────┐ ┌──────────▼──────────────┐ ┌─────────▼───────────────┐
#   │ plexus-hub                │ │ plexus-spoke-1          │ │ plexus-spoke-2          │
#   │ Pod CIDR: 10.245.0.0/16   │ │ Pod CIDR: 10.246.0.0/16 │ │ Pod CIDR: 10.247.0.0/16 │
#   │ Svc CIDR: 10.97.0.0/16    │ │ Svc CIDR: 10.98.0.0/16  │ │ Svc CIDR: 10.99.0.0/16  │
#   │                           │ │                         │ │                         │
#   │ Per node (1 interface):   │ │ Per node (1 interface): │ │ Per node (1 interface): │
#   │  breth0: 172.18.0.x/16   │ │  breth0: 172.18.0.x/16  │ │  breth0: 172.18.0.x/16  │
#   │  breth0: 169.254.0.2/17  │ │  breth0: 169.254.0.2    │ │  breth0: 169.254.0.2    │
#   │  mp0: 10.245.{0,1}.2     │ │  mp0: 10.246.{0,1}.2    │ │  mp0: 10.247.{0,1}.2    │
#   └──────────┬────────────────┘ └──────────┬──────────────┘ └─────────┬───────────────┘
#   FRR BGP via breth0          FRR BGP via breth0           FRR BGP via breth0
#              │                             │                            │
#              └─────────────┬──────────────┘                            │
#                            │◄──────────────────────────────────────────┘
#                  ┌─────────▼───────────────────────────────┐
#                  │  plexus-frr  (iBGP route reflector)     │
#                  │  AS 64512, on the kind network           │
#                  └─────────────────────────────────────────┘
#
# EVPN overlay — AND "production" UDN subnets (across all clusters via EVPN):
#   web  10.0.1.0/24   Public   — intra-domain routing, BGP export via RouteAdvertisements
#   app  10.0.10.0/24  Private  — intra-domain routing, no BGP export
#   db   10.0.20.0/24  Isolated — no routing outside subnet
#
# Key design choices:
#   Single network    All clusters on the default "kind" Docker network — all nodes
#                     share L2, eliminating cross-network routing and iptables rules.
#   VTEP IPs          172.18.0.x — OVN-K bridges eth0 (kind) as breth0 and allocates
#                     ovn-encap-ip from 172.18.0.0/16; Geneve src matches outgoing interface.
#   FRR BGP           FRR container on kind network; all cluster nodes reachable directly;
#                     frr-k8s peers with FRR at its 172.18.x.x IP.
#   Spoke Secrets     kind network IP (172.18.0.x) + insecure-skip-tls-verify — the
#                     kind IP is not in the spoke API cert SAN.
#   Pod CIDRs         Still unique per cluster (no overlap) for routing correctness.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/plexus-kind-common.sh"

HUB_NAME="plexus-hub"
SPOKE_COUNT=1
SPOKE_PREFIX="plexus-spoke"
WORKERS=1
SKIP_BUILD=false
DELETE=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --hub)          HUB_NAME="$2"; shift 2 ;;
    --spokes)       SPOKE_COUNT="$2"; shift 2 ;;
    --spoke-prefix) SPOKE_PREFIX="$2"; shift 2 ;;
    --workers)      WORKERS="$2"; shift 2 ;;
    --skip-build)   SKIP_BUILD=true; shift ;;
    --force-build)  FORCE_BUILD=true; shift ;;
    --delete)       DELETE=true; shift ;;
    *)              echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

HUB_KUBECONFIG="${HOME}/${HUB_NAME}.conf"

spoke_name()      { echo "${SPOKE_PREFIX}-${1}"; }
spoke_kubeconfig(){ echo "${HOME}/$(spoke_name "$1").conf"; }

all_cluster_names() {
  echo "$HUB_NAME"
  for ((i = 1; i <= SPOKE_COUNT; i++)); do spoke_name "$i"; done
}

# ─────────────────────────────────────────────────────────────────────────────

if [ "$DELETE" = true ]; then
  echo "=== Deleting all Plexus clusters ==="
  for name in $(all_cluster_names); do
    kind delete cluster --name "$name" 2>/dev/null || true
    rm -f "${HOME}/${name}.conf"
  done
  cleanup_external_frr
  echo "Done."
  exit 0
fi

resolve_ovn_kubernetes_path
echo "Using OVN-Kubernetes from: ${OVN_KUBERNETES_PATH}"
echo "Hub: ${HUB_NAME}, Spokes: ${SPOKE_COUNT}, Workers/cluster: ${WORKERS}"
echo ""

CIDR_BASE="${CIDR_BASE:-1}"

echo "=== Phase 1: Images ==="
if [ "$SKIP_BUILD" = true ]; then
  OVN_IMAGE="localhost/ovn-daemonset-fedora:dev"
  PLEXUS_IMAGE="localhost/plexus-controller:dev"
  echo "Skipping image builds, using existing images"
else
  build_ovn_image
  build_plexus_image
fi
echo ""

echo "=== Phase 2: Creating KIND clusters ==="
# All clusters use the default "kind" Docker network — no per-cluster networks.
# This places all nodes on the same L2 (172.18.0.0/16), eliminating cross-
# network routing and iptables rules. OVN-K bridges the kind interface as
# breth0 and uses 172.18.0.x IPs as ovn-encap-ip for EVPN Geneve tunnels.

create_cluster() {
  local name=$1 index=$2 kubeconfig=$3

  compute_cidrs "$index"
  echo "--- Cluster ${name} (index=${index}): pods=${POD_CIDR} svcs=${SVC_CIDR} net=kind ---"

  if kind get clusters 2>/dev/null | grep -qx "$name"; then
    echo "Cluster ${name} already exists, skipping creation"
  else
    generate_kind_config "$WORKERS" "$POD_CIDR" "$SVC_CIDR" | \
      kind create cluster \
        --name "$name" \
        --kubeconfig "$kubeconfig" \
        --config /dev/stdin
  fi

  load_ovn_image "$name"
  configure_node_sysctl "$name"
}

create_cluster "$HUB_NAME" "$CIDR_BASE" "$HUB_KUBECONFIG"
for ((i = 1; i <= SPOKE_COUNT; i++)); do
  create_cluster "$(spoke_name "$i")" "$((CIDR_BASE + i))" "$(spoke_kubeconfig "$i")"
done
echo ""

echo "=== Phase 3: External FRR route reflector ==="

# All nodes are on the kind network — FRR deploys once and peers with all
# cluster nodes directly without needing separate per-cluster connections.
deploy_external_frr "$HUB_KUBECONFIG"

for ((i = 1; i <= SPOKE_COUNT; i++)); do
  spoke_ips=()
  while IFS= read -r ip; do
    spoke_ips+=("$ip")
  done < <(get_node_ips "$(spoke_name "$i")" kind)
  add_frr_neighbors "${spoke_ips[@]}"
done
echo ""

echo "=== Phase 4: OVN-Kubernetes + FRR-K8s ==="

deploy_ovnk_to_cluster() {
  local name=$1 index=$2 kubeconfig=$3

  compute_cidrs "$index"
  install_frr_k8s "$kubeconfig"
  helm_install_ovnk "$name" "$kubeconfig" kind
  wait_for_ovnk "$kubeconfig"
  # All nodes on kind network — FRR's kind IP is the BGP neighbor address.
  configure_frr_k8s_peering "$kubeconfig" "$name" kind
}

deploy_ovnk_to_cluster "$HUB_NAME" "$CIDR_BASE" "$HUB_KUBECONFIG"
for ((i = 1; i <= SPOKE_COUNT; i++)); do
  deploy_ovnk_to_cluster "$(spoke_name "$i")" "$((CIDR_BASE + i))" "$(spoke_kubeconfig "$i")"
done
echo ""

echo "=== Phase 5: Plexus controller ==="
# VTEP CIDR is the kind network subnet. All nodes use their 172.18.0.x IP as
# ovn-encap-ip, and Geneve tunnels run directly over the shared kind L2.
KIND_SUBNET=$(docker_network_cidr kind)
deploy_plexus_controller "$HUB_NAME" "$HUB_KUBECONFIG" "$KIND_SUBNET"
echo ""

echo "=== Phase 6: Spoke cluster Secrets ==="
# Use the spoke control-plane's kind network IP. All nodes are on the same
# network so pod traffic reaches the spoke API directly without extra routing.
# insecure-skip-tls-verify is set because the kind IP is not in the cert SAN.
for ((i = 1; i <= SPOKE_COUNT; i++)); do
  create_spoke_secret "$HUB_KUBECONFIG" "$(spoke_name "$i")" "$i" kind
done
echo ""

echo "============================================"
echo "  Multi-cluster Plexus environment is ready"
echo "============================================"
echo ""
echo "Hub cluster:"
echo "  Name:       ${HUB_NAME}"
echo "  KUBECONFIG: ${HUB_KUBECONFIG}"
echo ""
for ((i = 1; i <= SPOKE_COUNT; i++)); do
  echo "Spoke cluster ${i}:"
  echo "  Name:       $(spoke_name "$i")"
  echo "  KUBECONFIG: $(spoke_kubeconfig "$i")"
  echo ""
done
echo "Plexus controller:"
echo "  KUBECONFIG=${HUB_KUBECONFIG} kubectl get pods -n plexus-system"
echo ""
echo "Spoke Secrets:"
echo "  KUBECONFIG=${HUB_KUBECONFIG} kubectl -n plexus-system get secrets -l plexus.io/cluster=true"
