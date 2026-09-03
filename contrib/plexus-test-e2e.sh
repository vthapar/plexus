#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# E2E validation and datapath tests for a Plexus multi-cluster setup.
#
# Phases:
#   Cluster health  — controller, spoke Secret, node IPs, VTEP CIDRs
#   Phase 0         — create AND with default subnets if it does not exist
#   Phase 1         — AND Ready condition and subnet inventory
#   Phase 2         — Namespaces and CUDNs on hub and spoke
#   Phase 3         — VTEPs and RouteAdvertisements
#   Phase 4         — Deploy test pods, wait for UDN IPs
#   Phase 5         — Intra-domain connectivity (hub, same cluster)
#   Phase 6         — Cross-cluster EVPN connectivity (hub → spoke)
#
# Usage:
#   contrib/plexus-test-e2e.sh [OPTIONS]
#
# Options:
#   --hub-kubeconfig PATH       Hub cluster kubeconfig (default: ~/plexus-hub.conf)
#   --spoke-kubeconfig PATH     Spoke cluster kubeconfig (default: ~/plexus-spoke-1.conf)
#   --spoke-cluster-name NAME   Spoke cluster name used for Secret lookup (default: plexus-spoke-1)
#   --and NAME                  AND name to test (default: production)
#   --cleanup                   Delete test pods on exit
#   --timeout N                 Pod-ready / ping timeout in seconds (default: 120)
#
# Default subnets created when the AND does not exist:
#   web  10.0.1.0/24   Public
#   app  10.0.10.0/24  Private
#   db   10.0.20.0/24  Isolated

set -uo pipefail

HUB_KUBECONFIG="${HOME}/plexus-hub.conf"
SPOKE_KUBECONFIG="${HOME}/plexus-spoke-1.conf"
SPOKE_CLUSTER_NAME="plexus-spoke-1"
AND_NAME="production"
CLEANUP=false
TIMEOUT=120
TEST_IMAGE="nicolaka/netshoot"
POD_PREFIX="plexus-e2e"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --hub-kubeconfig)    HUB_KUBECONFIG="$2";    shift 2 ;;
    --spoke-kubeconfig)  SPOKE_KUBECONFIG="$2";  shift 2 ;;
    --spoke-cluster-name) SPOKE_CLUSTER_NAME="$2"; shift 2 ;;
    --and)               AND_NAME="$2";           shift 2 ;;
    --cleanup)           CLEANUP=true;            shift ;;
    --timeout)           TIMEOUT="$2";            shift 2 ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

# ── Output helpers ─────────────────────────────────────────────────────────────

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; NC='\033[0m'

PASS_COUNT=0; FAIL_COUNT=0; SKIP_COUNT=0; KNOWN_FAIL_COUNT=0

pass()       { echo -e "  ${GREEN}✓ PASS${NC}  $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail()       { echo -e "  ${RED}✗ FAIL${NC}  $1"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
skip()       { echo -e "  ${YELLOW}⊘ SKIP${NC}  $1"; SKIP_COUNT=$((SKIP_COUNT + 1)); }
known_fail() { echo -e "  ${YELLOW}⊘ KNOWN${NC} $1"; KNOWN_FAIL_COUNT=$((KNOWN_FAIL_COUNT + 1)); }
section()    { echo -e "\n${BLUE}=== $1 ===${NC}"; }

hub()   { kubectl --kubeconfig "$HUB_KUBECONFIG"   "$@"; }
spoke() { kubectl --kubeconfig "$SPOKE_KUBECONFIG" "$@"; }

# udn_pod_ip KUBECONFIG POD NAMESPACE
# Returns the pod's UDN primary IP from the k8s.ovn.org/pod-networks annotation.
# With UDN as primary network, status.podIP holds the infrastructure-locked
# default network IP; the routable UDN IP is only in the annotation.
udn_pod_ip() {
  local kc=$1 pod=$2 ns=$3
  kubectl --kubeconfig "$kc" get pod "$pod" -n "$ns" \
    -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/pod-networks}' 2>/dev/null | \
  python3 -c "
import sys, json
try:
    nets = json.loads(sys.stdin.read())
    for info in nets.values():
        if info.get('role') == 'primary':
            # ip_addresses is canonical; ip_address is deprecated and absent
            # when multiple addresses exist (dual-stack).
            addrs = info.get('ip_addresses') or [info.get('ip_address', '')]
            print(addrs[0].split('/')[0])
            break
except Exception:
    pass
"
}

# check_cudn LABEL KUBECONFIG NAMESPACE
# Checks that the CUDN for NAMESPACE exists and has NetworkCreated=True.
check_cudn() {
  local label=$1 kc=$2 ns=$3
  if kubectl --kubeconfig "$kc" get clusteruserdefinednetwork "$ns" &>/dev/null; then
    local ready
    ready=$(kubectl --kubeconfig "$kc" get clusteruserdefinednetwork "$ns" \
      -o jsonpath='{.status.conditions[?(@.type=="NetworkCreated")].status}' 2>/dev/null || echo "Unknown")
    if [ "$ready" = "True" ]; then
      pass "${label}: CUDN '$ns' is NetworkCreated"
    else
      fail "${label}: CUDN '$ns' not NetworkCreated (status: ${ready})"
    fi
  else
    fail "${label}: CUDN '$ns' missing"
  fi
}

# ── Cleanup ────────────────────────────────────────────────────────────────────

cleanup_pods() {
  echo -e "\nCleaning up test pods..."
  hub   delete pod -l plexus-e2e=true --all-namespaces --ignore-not-found 2>/dev/null || true
  spoke delete pod -l plexus-e2e=true --all-namespaces --ignore-not-found 2>/dev/null || true
}

[ "$CLEANUP" = true ] && trap cleanup_pods EXIT

# ── Cluster health ─────────────────────────────────────────────────────────────

section "Cluster health"

# Plexus controller running and Ready on hub
ctrl=$(hub get pods -n plexus-system -l app.kubernetes.io/name=plexus-controller \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
if [ -z "$ctrl" ]; then
  fail "Plexus controller not found in plexus-system"
elif hub wait pod "$ctrl" -n plexus-system \
    --for=condition=Ready --timeout=10s &>/dev/null 2>&1; then
  pass "Plexus controller '$ctrl' is running and Ready"
else
  fail "Plexus controller '$ctrl' exists but is not Ready"
fi

# Spoke cluster Secret present
if hub get secret "$SPOKE_CLUSTER_NAME" -n plexus-system &>/dev/null; then
  pass "Spoke Secret '$SPOKE_CLUSTER_NAME' exists in plexus-system"
else
  fail "Spoke Secret '$SPOKE_CLUSTER_NAME' missing in plexus-system"
fi

# No connectivity errors in recent controller logs
if [ -n "$ctrl" ]; then
  err_count=$(hub logs -n plexus-system "$ctrl" --tail=50 2>/dev/null | \
    grep -cE "i/o timeout|connection refused|certificate|TLS" || true)
  if [ "$err_count" -eq 0 ]; then
    pass "Controller logs: no spoke connectivity errors"
  else
    fail "Controller logs: ${err_count} error(s) in last 50 lines (timeout/TLS/cert)"
  fi
fi

# ── Phase 0: AND setup ─────────────────────────────────────────────────────────

section "Phase 0: AND setup"

if hub get and "$AND_NAME" &>/dev/null; then
  echo "  AND '$AND_NAME' already exists — skipping creation"
else
  echo "  AND '$AND_NAME' not found — creating with default subnets..."
  hub apply -f - <<EOF
apiVersion: plexus.io/v1beta1
kind: AdministrativeNetworkDomain
metadata:
  name: ${AND_NAME}
spec:
  subnets:
  - name: web
    cidrs: ["10.0.1.0/24"]
    type: Public
  - name: app
    cidrs: ["10.0.10.0/24"]
    type: Private
  - name: db
    cidrs: ["10.0.20.0/24"]
    type: Isolated
EOF
  echo "  Waiting for AND '$AND_NAME' to become Ready (timeout: ${TIMEOUT}s)..."
  if hub wait and "$AND_NAME" --for=condition=Ready \
      --timeout="${TIMEOUT}s" 2>/dev/null; then
    echo "  AND '$AND_NAME' is Ready"
  else
    echo "  Warning: AND '$AND_NAME' not Ready within ${TIMEOUT}s — continuing"
  fi
fi

# ── Phase 1: AND resource validation ──────────────────────────────────────────

section "Phase 1: AND resource validation"

and_ready=$(hub get and "$AND_NAME" \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "NotFound")
if [ "$and_ready" = "True" ]; then
  pass "AND '$AND_NAME' is Ready"
else
  fail "AND '$AND_NAME' not Ready (status: ${and_ready})"
fi

SUBNET_NAMES=()
SUBNET_TYPES=()
while IFS=: read -r name type; do
  [ -z "$name" ] && continue
  SUBNET_NAMES+=("$name")
  SUBNET_TYPES+=("${type:-Private}")
done < <(hub get and "$AND_NAME" \
  -o jsonpath='{range .spec.subnets[*]}{.name}:{.type}{"\n"}{end}' 2>/dev/null)

if [ "${#SUBNET_NAMES[@]}" -eq 0 ]; then
  fail "No subnets found in AND '$AND_NAME' — cannot continue"
  exit 1
fi

echo "  Subnets:"
for i in "${!SUBNET_NAMES[@]}"; do
  echo "    ${SUBNET_NAMES[$i]} (${SUBNET_TYPES[$i]})"
done

# ── Phase 2: Namespaces and CUDNs ─────────────────────────────────────────────

section "Phase 2: Namespaces and CUDNs"

for name in "${SUBNET_NAMES[@]}"; do
  ns="${AND_NAME}-${name}"

  if hub get ns "$ns" &>/dev/null; then
    pass "Hub: namespace '$ns' exists"
  else
    fail "Hub: namespace '$ns' missing"
  fi

  check_cudn "Hub" "$HUB_KUBECONFIG" "$ns"

  if spoke get ns "$ns" &>/dev/null; then
    pass "Spoke: namespace '$ns' exists"
    check_cudn "Spoke" "$SPOKE_KUBECONFIG" "$ns"
  else
    skip "Spoke: namespace '$ns' not present (subnet not scheduled to spoke)"
  fi
done

# ── Phase 3: VTEPs and RouteAdvertisements ────────────────────────────────────

section "Phase 3: VTEPs and RouteAdvertisements"

for entry in "hub:$HUB_KUBECONFIG" "spoke:$SPOKE_KUBECONFIG"; do
  cluster="${entry%%:*}"; kc="${entry##*:}"
  vtep=$(kubectl --kubeconfig "$kc" get vtep nd-vtep \
    -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null || echo "NotFound")
  if [ "$vtep" = "True" ]; then
    pass "${cluster}: VTEP 'nd-vtep' is Accepted"
  else
    fail "${cluster}: VTEP 'nd-vtep' not Accepted (status: ${vtep})"
  fi
done

has_public=false
for i in "${!SUBNET_NAMES[@]}"; do
  if [ "${SUBNET_TYPES[$i]}" = "Public" ]; then
    has_public=true; break
  fi
done
if [ "$has_public" = "true" ]; then
  for entry in "hub:$HUB_KUBECONFIG" "spoke:$SPOKE_KUBECONFIG"; do
    cluster="${entry%%:*}"; kc="${entry##*:}"
    if kubectl --kubeconfig "$kc" get routeadvertisements "$AND_NAME" &>/dev/null; then
      pass "${cluster}: RouteAdvertisements '$AND_NAME' exists (Public subnets present)"
    else
      fail "${cluster}: RouteAdvertisements '$AND_NAME' missing (AND has Public subnets)"
    fi
  done
fi

# ── Phase 4: Deploy test pods ──────────────────────────────────────────────────

section "Phase 4: Deploying test pods"

declare -A HUB_IPS SPOKE_IPS

for name in "${SUBNET_NAMES[@]}"; do
  ns="${AND_NAME}-${name}"; pod="${POD_PREFIX}-${name}"
  hub get pod "$pod" -n "$ns" &>/dev/null && continue
  hub run "$pod" -n "$ns" --image="$TEST_IMAGE" --restart=Never \
    --labels="plexus-e2e=true" --command -- sleep 3600 &>/dev/null
  echo "  Launched hub pod '$pod' in '$ns'"
done

for i in "${!SUBNET_NAMES[@]}"; do
  [ "${SUBNET_TYPES[$i]}" = "Isolated" ] && continue
  name="${SUBNET_NAMES[$i]}"; ns="${AND_NAME}-${name}"; pod="${POD_PREFIX}-${name}"
  spoke get ns "$ns" &>/dev/null || continue
  spoke get pod "$pod" -n "$ns" &>/dev/null && continue
  spoke run "$pod" -n "$ns" --image="$TEST_IMAGE" --restart=Never \
    --labels="plexus-e2e=true" --command -- sleep 3600 &>/dev/null
  echo "  Launched spoke pod '$pod' in '$ns'"
done

echo "  Waiting for pods (timeout: ${TIMEOUT}s)..."

for name in "${SUBNET_NAMES[@]}"; do
  ns="${AND_NAME}-${name}"; pod="${POD_PREFIX}-${name}"
  if hub wait pod "$pod" -n "$ns" --for=condition=Ready \
      --timeout="${TIMEOUT}s" &>/dev/null 2>&1; then
    ip=$(udn_pod_ip "$HUB_KUBECONFIG" "$pod" "$ns")
    HUB_IPS[$name]="$ip"
    pass "Hub pod '${ns}/${pod}' ready (UDN IP: ${ip})"
  else
    fail "Hub pod '${ns}/${pod}' not ready within ${TIMEOUT}s"
  fi
done

for i in "${!SUBNET_NAMES[@]}"; do
  [ "${SUBNET_TYPES[$i]}" = "Isolated" ] && continue
  name="${SUBNET_NAMES[$i]}"; ns="${AND_NAME}-${name}"; pod="${POD_PREFIX}-${name}"
  spoke get ns "$ns" &>/dev/null || continue
  if spoke wait pod "$pod" -n "$ns" --for=condition=Ready \
      --timeout="${TIMEOUT}s" &>/dev/null 2>&1; then
    ip=$(udn_pod_ip "$SPOKE_KUBECONFIG" "$pod" "$ns")
    SPOKE_IPS[$name]="$ip"
    pass "Spoke pod '${ns}/${pod}' ready (UDN IP: ${ip})"
  else
    fail "Spoke pod '${ns}/${pod}' not ready within ${TIMEOUT}s"
  fi
done

# EVPN type-2/3 route propagation is asynchronous; pod Ready ≠ EVPN converged.
echo "  Waiting 30s for EVPN route convergence..."
sleep 30

# ── Phase 5: Intra-domain connectivity (hub) ──────────────────────────────────

section "Phase 5: Intra-domain connectivity (hub)"

SRC=""
for i in "${!SUBNET_NAMES[@]}"; do
  n="${SUBNET_NAMES[$i]}"
  if [ "${SUBNET_TYPES[$i]}" != "Isolated" ] && [ -n "${HUB_IPS[$n]:-}" ]; then
    SRC="$n"; break
  fi
done

# src_pod/src_ns are used by both Phase 5 and Phase 6.
src_pod="${POD_PREFIX}-${SRC:-}"
src_ns="${AND_NAME}-${SRC:-}"

if [ -z "$SRC" ]; then
  skip "No non-Isolated hub pod available; skipping intra-domain tests"
else
  for i in "${!SUBNET_NAMES[@]}"; do
    dst="${SUBNET_NAMES[$i]}"; dst_type="${SUBNET_TYPES[$i]}"
    [ "$dst" = "$SRC" ] && continue
    [ -z "${HUB_IPS[$dst]:-}" ] && continue
    dst_ip="${HUB_IPS[$dst]}"

    if [ "$dst_type" = "Isolated" ]; then
      if hub exec "$src_pod" -n "$src_ns" -- ping -c1 -W2 "$dst_ip" >/dev/null 2>&1; then
        fail "hub/$SRC → hub/$dst ($dst_ip): reachable, but '$dst' is Isolated"
      else
        pass "hub/$SRC → hub/$dst ($dst_ip): correctly blocked (Isolated)"
      fi
    else
      if hub exec "$src_pod" -n "$src_ns" -- ping -c3 -W3 "$dst_ip" >/dev/null 2>&1; then
        pass "hub/$SRC → hub/$dst ($dst_ip): reachable (intra-domain)"
      else
        known_fail "hub/$SRC → hub/$dst ($dst_ip): unreachable (intra-domain, requires OKEP-6607)"
      fi
    fi
  done
fi

# ── Phase 6: Cross-cluster EVPN connectivity ───────────────────────────────────

section "Phase 6: Cross-cluster EVPN connectivity"

if [ "${#SPOKE_IPS[@]}" -eq 0 ]; then
  skip "No spoke pods ready; skipping cross-cluster EVPN tests"
elif [ -z "$SRC" ]; then
  skip "No non-Isolated hub source pod; skipping cross-cluster EVPN tests"
else
  for dst in "${!SPOKE_IPS[@]}"; do
    dst_ip="${SPOKE_IPS[$dst]}"
    if hub exec "$src_pod" -n "$src_ns" -- ping -c3 -W5 "$dst_ip" >/dev/null 2>&1; then
      pass "hub/$SRC → spoke/$dst ($dst_ip): reachable (EVPN)"
    elif [ "$dst" != "$SRC" ]; then
      # Cross-subnet EVPN requires intra-domain IP-VRF routing (OKEP-6607).
      known_fail "hub/$SRC → spoke/$dst ($dst_ip): unreachable (cross-subnet EVPN, requires OKEP-6607)"
    else
      fail "hub/$SRC → spoke/$dst ($dst_ip): unreachable (EVPN tunnel)"
    fi
  done
fi

# ── Summary ────────────────────────────────────────────────────────────────────

echo ""
echo "========================================"
printf "  Results: %d passed, %d failed, %d skipped\n" \
  "$PASS_COUNT" "$FAIL_COUNT" "$SKIP_COUNT"
if [ "$KNOWN_FAIL_COUNT" -gt 0 ]; then
  printf "  Known:   %d test(s) require OKEP-6607 (ClusterNetworkConnect)\n" \
    "$KNOWN_FAIL_COUNT"
fi
echo "========================================"
echo ""

[ "$FAIL_COUNT" -eq 0 ]
