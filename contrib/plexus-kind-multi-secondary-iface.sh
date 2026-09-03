#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Create a multi-cluster (hub + N spokes) KIND environment for Plexus.
#
# Each cluster has its own Docker network (192.168.x.x) for FRR BGP peering.
# Nodes are also connected to the shared "kind" bridge (172.18.0.0/16) before
# OVN-K deploys, causing OVN-K to bridge the kind interface (eth1) as breth1
# and use it as its uplink for all outgoing traffic. The VTEP CIDR is therefore
# the kind subnet so ovn-encap-ip matches the actual outgoing interface, enabling
# correct EVPN Geneve tunnels. The plexus-controller reaches spoke APIs via the
# kind bridge IP (insecure-skip-tls-verify, since the kind IP is not in the cert SAN).
#
# Usage:
#   contrib/plexus-kind-multi-secondary-iface.sh [OPTIONS]
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
# ║  DOCKER HOST                                                                                   ║
# ║                                                                                                ║
# ║  Networks:  plexus-hub     192.168.11.0/24 ──gw 192.168.11.1                                  ║
# ║             plexus-spoke-1 192.168.12.0/24 ──gw 192.168.12.1                                  ║
# ║             plexus-spoke-2 192.168.13.0/24 ──gw 192.168.13.1                                  ║
# ║             kind (bridge)  172.18.0.0/16   ──gw 172.18.0.1                                    ║
# ║                                                                                                ║
# ║  iptables DOCKER-USER — all pairs, all clusters (pre-flight block):                            ║
# ║    • cluster ↔ cluster  e.g. 192.168.11.0/24 ↔ 192.168.12.0/24  (FRR BGP peering)            ║
# ║    • kind    ↔ cluster  e.g. 172.18.0.0/16   ↔ 192.168.12.0/24  (EVPN Geneve via breth1)     ║
# ╚══════════════╤═══════════════════════════╤══════════════════════════╤══════════════════════════╝
#                │                           │                          │
#   ┌────────────▼──────────────┐ ┌──────────▼──────────────┐ ┌─────────▼───────────────┐
#   │ plexus-hub                │ │ plexus-spoke-1          │ │ plexus-spoke-2          │
#   │ 192.168.11.0/24           │ │ 192.168.12.0/24         │ │ 192.168.13.0/24         │
#   │ Pod CIDR: 10.245.0.0/16   │ │ Pod CIDR: 10.246.0.0/16 │ │ Pod CIDR: 10.247.0.0/16 │
#   │ Svc CIDR: 10.97.0.0/16    │ │ Svc CIDR: 10.98.0.0/16  │ │ Svc CIDR: 10.99.0.0/16  │
#   │                           │ │                         │ │                         │
#   │ Per node (2 interfaces):  │ │ Per node (2 interfaces):│ │ Per node (2 interfaces):│
#   │  eth0   192.168.11.x/24   │ │  eth0  192.168.12.x/24  │ │  eth0  192.168.13.x/24  │
#   │  eth1 ─► breth1 (OVS)    │ │  eth1 ─► breth1 (OVS)  │ │  eth1 ─► breth1 (OVS)  │
#   │   breth1: 172.18.0.x/16  │ │   breth1: 172.18.0.x/16 │ │   breth1: 172.18.0.x/16 │
#   │   breth1: 169.254.0.2/17 │ │   breth1: 169.254.0.2   │ │   breth1: 169.254.0.2   │
#   │  mp0:   10.245.{0,1}.2   │ │  mp0:   10.246.{0,1}.2  │ │  mp0:   10.247.{0,1}.2  │
#   │                           │ │                         │ │                         │
#   │ Routes (via breth1):      │ │ Routes (via breth1):    │ │ Routes (via breth1):    │
#   │  default → 172.18.0.1    │ │  default → 172.18.0.1   │ │  default → 172.18.0.1   │
#   │  .12.0/24 → 172.18.0.1   │ │  .11.0/24 → 172.18.0.1  │ │  .11.0/24 → 172.18.0.1  │
#   │  .13.0/24 → 172.18.0.1   │ │  .13.0/24 → 172.18.0.1  │ │  .12.0/24 → 172.18.0.1  │
#   └──────────┬────────────────┘ └──────────┬──────────────┘ └─────────┬───────────────┘
#   FRR BGP via eth0            FRR BGP via eth0             FRR BGP via eth0
#   (192.168.11.4)              (192.168.12.4)               (192.168.13.4)
#              │                             │                            │
#              └─────────────┬──────────────┘                            │
#                            │◄──────────────────────────────────────────┘
#                  ┌─────────▼───────────────────────────────┐
#                  │  plexus-frr  (iBGP route reflector)     │
#                  │  AS 64512                               │
#                  │  192.168.11.4 on plexus-hub net         │
#                  │  192.168.12.4 on plexus-spoke-1 net     │
#                  │  192.168.13.4 on plexus-spoke-2 net     │
#                  └─────────────────────────────────────────┘
#
# EVPN overlay — AND "production" UDN subnets (across all clusters via EVPN):
#   web  10.0.1.0/24   Public   — intra-domain routing, BGP export via RouteAdvertisements
#   app  10.0.10.0/24  Private  — intra-domain routing, no BGP export
#   db   10.0.20.0/24  Isolated — no routing outside subnet
#
# Key design choices:
#   VTEP IPs        172.18.0.x (kind bridge) — nodes connect to kind BEFORE OVN-K deploys
#                   so OVN-K bridges eth1 as breth1 (kind interface) and allocates
#                   ovn-encap-ip from 172.18.0.0/16; Geneve src matches outgoing interface
#   FRR BGP         frr-k8s on each cluster peers with FRR at FRR's per-cluster IP (eth0),
#                   not breth1 — so BGP sessions use the correct cluster Docker network
#   Spoke Secrets   kind bridge IP (172.18.0.x) + insecure-skip-tls-verify — OVN-K pod
#                   traffic exits via breth1 which has OVS flows for 172.18.0.0/16;
#                   kind IP is not in the spoke API cert SAN so TLS check is skipped
#   OVN-K API URL   per-cluster IP (cert-valid) — used by helm_install_ovnk only

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/plexus-kind-common.sh"

HUB_NAME="plexus-hub"
SPOKE_COUNT=1
SPOKE_PREFIX="plexus-spoke"
WORKERS=1
SKIP_BUILD=false
FORCE_BUILD=false
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

# collect_cluster_subnets RESULT_VAR
# Fills RESULT_VAR (nameref) with the Docker network subnet for hub + all spokes.
collect_cluster_subnets() {
  local -n _out=$1
  _out=()
  compute_cidrs "$CIDR_BASE"
  _out+=("$DOCKER_NETWORK_SUBNET")
  for ((i = 1; i <= SPOKE_COUNT; i++)); do
    compute_cidrs "$((CIDR_BASE + i))"
    _out+=("$DOCKER_NETWORK_SUBNET")
  done
}

# ── Cross-cluster routing ─────────────────────────────────────────────────────
#
# Adds static routes on every cluster's nodes so they can reach all other
# clusters' Docker subnets via the kind bridge gateway. iptables DOCKER-USER
# rules are handled by the pre-flight block before any cluster is created.
#
# NOTE: per-cluster Docker gateways (eth0) are NOT used for routing.
# OVN-K bridges the kind interface as breth1 and uses it as its uplink for
# all outgoing traffic (including EVPN Geneve tunnels). Routes must go via
# the kind bridge gateway so they land on breth1; routes on eth0 are
# invisible to OVN-K's outgoing pipeline.
setup_inter_cluster_node_routes() {
  echo "Configuring cross-cluster node routes via kind bridge..."

  declare -a CLUSTER_SUBNETS
  collect_cluster_subnets CLUSTER_SUBNETS
  mapfile -t CLUSTER_NAMES < <(all_cluster_names)

  local kind_gw
  kind_gw=$($OCI_BIN network inspect kind --format '{{(index .IPAM.Config 0).Gateway}}')

  for ((c = 0; c < ${#CLUSTER_NAMES[@]}; c++)); do
    local cname="${CLUSTER_NAMES[$c]}"
    for ((o = 0; o < ${#CLUSTER_NAMES[@]}; o++)); do
      [ "$o" -eq "$c" ] && continue
      local other_subnet="${CLUSTER_SUBNETS[$o]}"
      echo "  ${cname}: route ${other_subnet} via ${kind_gw} (kind bridge)"
      for node in $(kind get nodes --name "$cname"); do
        $OCI_BIN exec "$node" ip route replace "$other_subnet" via "$kind_gw" 2>/dev/null || true
      done
    done
  done
}

# ─────────────────────────────────────────────────────────────────────────────

if [ "$DELETE" = true ]; then
  echo "=== Deleting all Plexus clusters ==="
  cleanup_all_plexus_clusters
  echo "Done."
  exit 0
fi

resolve_ovn_kubernetes_path
echo "Using OVN-Kubernetes from: ${OVN_KUBERNETES_PATH}"
echo "Hub: ${HUB_NAME}, Spokes: ${SPOKE_COUNT}, Workers/cluster: ${WORKERS}"
echo ""

CIDR_BASE="${CIDR_BASE:-1}"

echo "=== Pre-flight: iptables inter-cluster routing ==="
# Compute every subnet pair that requires a DOCKER-USER ACCEPT rule:
#   - cluster ↔ cluster  (FRR BGP peering over per-cluster Docker networks)
#   - kind    ↔ cluster  (OVN-K Geneve tunnels sourced from breth1/172.18.x.x)
# Rules must exist before cross-cluster traffic flows, even if the Docker
# networks haven't been created yet.
declare -a _cluster_subnets
collect_cluster_subnets _cluster_subnets

# Read the kind subnet dynamically; fall back to the default if the network
# doesn't exist yet (it will be created in Phase 2 with this subnet).
_kind_subnet="172.18.0.0/16"
if $OCI_BIN network inspect kind &>/dev/null 2>&1; then
  _kind_subnet=$($OCI_BIN network inspect kind \
    --format '{{(index .IPAM.Config 0).Subnet}}' 2>/dev/null || echo "172.18.0.0/16")
fi

_preflight_subnets=("$_kind_subnet" "${_cluster_subnets[@]}")

# Build the list of rules that must exist (one per ordered subnet pair).
_missing_rules=()
for ((i = 0; i < ${#_preflight_subnets[@]}; i++)); do
  for ((j = i + 1; j < ${#_preflight_subnets[@]}; j++)); do
    _src="${_preflight_subnets[$i]}"; _dst="${_preflight_subnets[$j]}"
    sudo -n iptables -C DOCKER-USER -s "$_src" -d "$_dst" -j ACCEPT 2>/dev/null || \
      _missing_rules+=("sudo iptables -I DOCKER-USER -s $_src -d $_dst -j ACCEPT")
    sudo -n iptables -C DOCKER-USER -s "$_dst" -d "$_src" -j ACCEPT 2>/dev/null || \
      _missing_rules+=("sudo iptables -I DOCKER-USER -s $_dst -d $_src -j ACCEPT")
  done
done

if [ "${#_missing_rules[@]}" -eq 0 ]; then
  echo "  iptables DOCKER-USER rules already present — OK"
elif sudo -n iptables -L DOCKER-USER &>/dev/null 2>&1; then
  echo "  Rules will be added automatically (sudo access confirmed)"
else
  echo ""
  echo "ERROR: iptables DOCKER-USER rules for inter-cluster routing are missing"
  echo "       and passwordless sudo for iptables is not configured."
  echo ""
  echo "Fix option 1 — grant passwordless sudo for iptables (recommended):"
  printf '  sudo tee /etc/sudoers.d/plexus-iptables <<EOF\n'
  printf '  %s ALL=(ALL) NOPASSWD: /usr/sbin/iptables\n' "${USER}"
  printf '  EOF\n'
  echo ""
  echo "Fix option 2 — add the rules manually before running this script:"
  for rule in "${_missing_rules[@]}"; do
    echo "  ${rule}"
  done
  echo ""
  exit 1
fi
unset _cluster_subnets _kind_subnet _preflight_subnets _missing_rules
echo ""

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
# The kind bridge network must exist before nodes connect to it (Phase 4).
create_docker_network kind 172.18.0.0/16

create_cluster() {
  local name=$1 index=$2 kubeconfig=$3 network=$4

  compute_cidrs "$index"
  create_kind_cluster "$name" "$kubeconfig" "$network" "$WORKERS"

  # Connect every node to the kind bridge before OVN-K is deployed so that
  # OVN-K bridges the kind interface (breth1) as its uplink. This makes the
  # kind bridge IPs the ovn-encap-ip, which keeps Geneve tunnel source and
  # outgoing interface IPs consistent. The VTEP CIDR is set to the kind
  # bridge subnet accordingly (see Phase 5).
  echo "Connecting ${name} nodes to kind bridge..."
  for node in $(kind get nodes --name "$name"); do
    if $OCI_BIN inspect "$node" \
        --format "{{range \$k,\$v := .NetworkSettings.Networks}}{{\$k}} {{end}}" \
        2>/dev/null | grep -qw "kind"; then
      echo "  ${node}: already connected to kind bridge"
    else
      $OCI_BIN network connect kind "$node"
      echo "  ${node}: connected to kind bridge"
    fi
  done
}

HUB_NETWORK="$HUB_NAME"
create_cluster "$HUB_NAME" "$CIDR_BASE" "$HUB_KUBECONFIG" "$HUB_NETWORK"
for ((i = 1; i <= SPOKE_COUNT; i++)); do
  create_cluster "$(spoke_name "$i")" "$((CIDR_BASE + i))" \
    "$(spoke_kubeconfig "$i")" "$(spoke_name "$i")"
done
echo ""

echo "=== Phase 2b: Cross-cluster routing ==="
setup_inter_cluster_node_routes
echo ""

echo "=== Phase 3: External FRR route reflector ==="

# Deploy FRR on the hub's Docker network, then connect it to each spoke
# network so it can peer with all cluster nodes across their L2 domains.
deploy_external_frr "$HUB_KUBECONFIG"

for ((i = 1; i <= SPOKE_COUNT; i++)); do
  spoke_net=$(spoke_name "$i")
  connect_frr_to_network "$spoke_net"
  spoke_ips=()
  while IFS= read -r ip; do
    spoke_ips+=("$ip")
  done < <(get_node_ips "$(spoke_name "$i")" "$spoke_net")
  add_frr_neighbors "${spoke_ips[@]}"
done
echo ""

echo "=== Phase 4: OVN-Kubernetes + FRR-K8s ==="

deploy_ovnk_to_cluster "$HUB_NAME" "$CIDR_BASE" "$HUB_KUBECONFIG" "$HUB_NETWORK" "$HUB_NETWORK"
for ((i = 1; i <= SPOKE_COUNT; i++)); do
  deploy_ovnk_to_cluster "$(spoke_name "$i")" "$((CIDR_BASE + i))" \
    "$(spoke_kubeconfig "$i")" "$(spoke_name "$i")" "$(spoke_name "$i")"
done
echo ""

echo "=== Phase 5: Plexus controller ==="
# VTEP CIDR is the kind bridge subnet. OVN-K bridges the kind interface
# (breth1) as its uplink — nodes are connected to kind before OVN-K deploys
# so OVN-K selects it. The ovn-encap-ip is therefore from 172.18.0.0/16,
# which matches the actual outgoing interface, enabling correct EVPN tunnels.
KIND_SUBNET=$(docker_network_cidr kind)
deploy_plexus_controller "$HUB_NAME" "$HUB_KUBECONFIG" "$KIND_SUBNET"
echo ""

echo "=== Phase 6: Spoke cluster Secrets ==="
# Use the kind bridge IP for the spoke API URL. OVN-K uses breth1 (kind bridge)
# as its uplink, so pod traffic to the spoke API exits via the kind bridge.
# insecure-skip-tls-verify is set in create_spoke_secret because the kind
# bridge IP is not in the spoke API server's TLS cert SAN.
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
