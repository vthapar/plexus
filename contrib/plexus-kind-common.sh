#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Shared helpers for Plexus KIND cluster scripts.
# Sourced by plexus-kind.sh and plexus-kind-multi-secondary-iface.sh.

set -euo pipefail

PLEXUS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OVN_KUBERNETES_PATH="${OVN_KUBERNETES_PATH:-}"
OCI_BIN="${OCI_BIN:-docker}"
KIND_IMAGE="${KIND_IMAGE:-kindest/node}"
K8S_VERSION="${K8S_VERSION:-v1.36.1}"

# Keep in sync with ovn-kubernetes/contrib/kind-common.sh FRR constants.
readonly FRR_K8S_GIT_REF="b43efcb206be"
readonly FRR_K8S_UPSTREAM_FRR_IMAGE="quay.io/frrouting/frr:10.4.1"
readonly FRR_K8S_ALL_IN_ONE_UPSTREAM_FRR_IMAGE="quay.io/frrouting/frr:10.4.3"
readonly FRR_DEPLOYED_IMAGE="quay.io/frrouting/frr:10.6.0"
readonly FRR_EXTERNAL_DEMO_IMAGE="${FRR_DEPLOYED_IMAGE}"
FRR_K8S_FRR_IMAGE="${FRR_K8S_FRR_IMAGE:-${FRR_DEPLOYED_IMAGE}}"
FRR_TMP_DIR=""

resolve_ovn_kubernetes_path() {
  if [ -n "$OVN_KUBERNETES_PATH" ] && [ -d "$OVN_KUBERNETES_PATH" ]; then
    return
  fi

  local candidates=(
    "${PLEXUS_DIR}/../ovn-kubernetes"
  )
  local gopath="${GOPATH:-}"
  if [ -z "$gopath" ] && command -v go >/dev/null 2>&1; then
    gopath=$(go env GOPATH 2>/dev/null || true)
  fi
  if [ -n "$gopath" ]; then
    IFS=: read -ra entries <<< "$gopath"
    for entry in "${entries[@]}"; do
      candidates+=("${entry}/src/github.com/ovn-kubernetes/ovn-kubernetes")
    done
  fi

  for path in "${candidates[@]}"; do
    if [ -d "$path/helm/ovn-kubernetes" ]; then
      OVN_KUBERNETES_PATH="$(cd "$path" && pwd)"
      return
    fi
  done

  echo "error: cannot locate ovn-kubernetes checkout (set OVN_KUBERNETES_PATH)" >&2
  exit 1
}

# compute_cidrs INDEX
# Pod and Service CIDRs must be unique per cluster (routed across clusters via EVPN).
# Join, transit, and masquerade are cluster-internal and can safely overlap.
# Each cluster gets its own Docker network subnet for L2 isolation.
compute_cidrs() {
  local idx=$1
  POD_CIDR="10.$((244 + idx)).0.0/16"
  POD_NETWORK="10.$((244 + idx)).0.0/16/24"
  SVC_CIDR="10.$((96 + idx)).0.0/16"
  JOIN_SUBNET="100.64.0.0/16"
  TRANSIT_SUBNET="100.88.0.0/16"
  MASQ_SUBNET="169.254.0.0/17"
  DOCKER_NETWORK_SUBNET="192.168.$((10 + idx)).0/24"
}

docker_network_cidr() {
  local name=$1
  $OCI_BIN network inspect "$name" --format '{{(index .IPAM.Config 0).Subnet}}'
}

create_docker_network() {
  local name=$1 subnet=$2
  if ! $OCI_BIN network inspect "$name" >/dev/null 2>&1; then
    echo "Creating Docker network ${name} (${subnet})..."
    $OCI_BIN network create "$name" --subnet "$subnet"
  else
    echo "Docker network ${name} already exists"
  fi
}

generate_kind_config() {
  local workers=$1 pod_cidr=$2 svc_cidr=$3
  cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  kubeProxyMode: "none"
  podSubnet: "${pod_cidr}"
  serviceSubnet: "${svc_cidr}"
nodes:
  - role: control-plane
    image: ${KIND_IMAGE}:${K8S_VERSION}
EOF
  for ((i = 0; i < workers; i++)); do
    echo "  - role: worker"
    echo "    image: ${KIND_IMAGE}:${K8S_VERSION}"
  done
}

# detect_api_url CLUSTER_NAME DOCKER_NETWORK -> https://<container-ip>:6443
detect_api_url() {
  local name=$1 network=${2:-kind}
  local ip
  ip=$($OCI_BIN inspect -f "{{(index .NetworkSettings.Networks \"${network}\").IPAddress}}" "${name}-control-plane" 2>/dev/null)
  if [ -z "$ip" ]; then
    echo "error: could not get IP for ${name}-control-plane on network '${network}'." \
         "Is the container running and connected to that network?" >&2
    return 1
  fi
  echo "https://${ip}:6443"
}

# get_node_ips CLUSTER_NAME DOCKER_NETWORK (one per line)
get_node_ips() {
  local name=$1 network=${2:-kind}
  for node in $(kind get nodes --name "$name"); do
    $OCI_BIN inspect -f "{{(index .NetworkSettings.Networks \"${network}\").IPAddress}}" "$node"
  done
}

# TODO: OVN-K should set rp_filter=2 on SVL interfaces it creates.
# See https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6631
# Until that's fixed, we disable strict rp_filter on all nodes so that
# EVPN inter-VRF traffic (arriving on svl3.X with a source IP from a
# different subnet) is not dropped by reverse-path filtering.
configure_node_sysctl() {
  local name=$1
  echo "Configuring sysctl rp_filter=2 (loose) on cluster ${name} nodes..."
  for node in $(kind get nodes --name "$name"); do
    $OCI_BIN exec "$node" sysctl -w net.ipv4.conf.all.rp_filter=2
    $OCI_BIN exec "$node" sysctl -w net.ipv4.conf.default.rp_filter=2
  done
}

# create_kind_cluster NAME KUBECONFIG NETWORK [WORKERS]
# Creates a KIND cluster on NETWORK (idempotent), loads the OVN image, and
# configures sysctl. Caller must call compute_cidrs beforehand to set POD_CIDR,
# SVC_CIDR, and DOCKER_NETWORK_SUBNET.
create_kind_cluster() {
  local name=$1 kubeconfig=$2 network=$3 workers=${4:-1}

  create_docker_network "$network" "$DOCKER_NETWORK_SUBNET"
  echo "--- Cluster ${name}: pods=${POD_CIDR} svcs=${SVC_CIDR}" \
       "net=${network} (${DOCKER_NETWORK_SUBNET}) ---"

  if kind get clusters 2>/dev/null | grep -qx "$name"; then
    echo "Cluster ${name} already exists, skipping creation"
  else
    generate_kind_config "$workers" "$POD_CIDR" "$SVC_CIDR" | \
      KIND_EXPERIMENTAL_DOCKER_NETWORK="$network" \
      kind create cluster \
        --name "$name" \
        --kubeconfig "$kubeconfig" \
        --config /dev/stdin
  fi

  load_ovn_image "$name"
  configure_node_sysctl "$name"
}

build_ovn_image() {
  local image_name="localhost/ovn-daemonset-fedora:dev"
  if [ "${FORCE_BUILD:-false}" != true ] && $OCI_BIN image inspect "$image_name" >/dev/null 2>&1; then
    echo "OVN image ${image_name} already exists, skipping build (use --force-build to rebuild)"
    OVN_IMAGE="$image_name"
    return
  fi
  echo "Building OVN-Kubernetes image..."
  make -C "${OVN_KUBERNETES_PATH}/dist/images" \
    IMAGE="$image_name" \
    OCI_BIN="$OCI_BIN" \
    fedora-image
  OVN_IMAGE="$image_name"
}

load_ovn_image() {
  local name=$1
  echo "Loading OVN image into cluster ${name}..."
  kind load docker-image "$OVN_IMAGE" --name "$name"
}

# Adapted from ovn-kubernetes/contrib/kind-common.sh: enable_multi_net(),
# install_online_ovn_kubernetes_crds().
install_ovnk_crds() {
  local kubeconfig=$1
  echo "Installing OVN-Kubernetes prerequisite CRDs..."

  local multus_version="v4.1.3"
  echo "  Multus CNI ${multus_version} (provides NetworkAttachmentDefinition CRD)..."
  wget -qO- "https://raw.githubusercontent.com/k8snetworkplumbingwg/multus-cni/${multus_version}/deployments/multus-daemonset.yml" | \
    sed -e "s|multus-cni:snapshot|multus-cni:${multus_version}|g" | \
    KUBECONFIG="$kubeconfig" kubectl apply -f -

  echo "  Multi-network-policy CRD..."
  KUBECONFIG="$kubeconfig" kubectl apply -f \
    "https://raw.githubusercontent.com/k8snetworkplumbingwg/multi-networkpolicy/refs/tags/v1.0.1/scheme.yml"

  echo "  IPAMClaim CRD..."
  KUBECONFIG="$kubeconfig" kubectl apply -f \
    "https://raw.githubusercontent.com/k8snetworkplumbingwg/ipamclaims/v0.5.1-alpha/artifacts/k8s.cni.cncf.io_ipamclaims.yaml"

  echo "  AdminNetworkPolicy + BaselineAdminNetworkPolicy CRDs..."
  KUBECONFIG="$kubeconfig" kubectl apply -f \
    "https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_adminnetworkpolicies.yaml"
  KUBECONFIG="$kubeconfig" kubectl apply -f \
    "https://raw.githubusercontent.com/kubernetes-sigs/network-policy-api/v0.1.5/config/crd/experimental/policy.networking.k8s.io_baselineadminnetworkpolicies.yaml"
}

helm_install_ovnk() {
  local name=$1 kubeconfig=$2 network=${3:-kind}
  local api_url
  api_url=$(detect_api_url "$name" "$network")

  install_ovnk_crds "$kubeconfig"

  echo "Installing OVN-Kubernetes via Helm into ${name}..."
  local helm_chart="${OVN_KUBERNETES_PATH}/helm/ovn-kubernetes"
  local values_file="${helm_chart}/values-single-node-zone.yaml"

  KUBECONFIG="$kubeconfig" helm upgrade --install ovn-kubernetes "$helm_chart" \
    -f "$values_file" \
    --set k8sAPIServer="${api_url}" \
    --set podNetwork="${POD_NETWORK}" \
    --set serviceNetwork="${SVC_CIDR}" \
    --set mtu=1400 \
    --set global.image.repository="${OVN_IMAGE%:*}" \
    --set global.image.tag="${OVN_IMAGE##*:}" \
    --set global.gatewayMode=local \
    --set global.enableMultiNetwork=true \
    --set global.enableNetworkSegmentation=true \
    --set global.enableNetworkConnect=true \
    --set global.enableRouteAdvertisements=true \
    --set global.enableEVPN=true \
    --set global.enableAdminNetworkPolicy=true \
    --set global.enablePersistentIPs=true \
    --set global.enableDynamicUDNAllocation=true \
    --set global.advertisedUDNIsolationMode=loose \
    --set-string global.v4JoinSubnet="${JOIN_SUBNET}" \
    --set-string global.v4TransitSubnet="${TRANSIT_SUBNET}" \
    --set-string global.v4MasqueradeSubnet="${MASQ_SUBNET}"
}

# Adapted from ovn-kubernetes/contrib/kind-common.sh: clone_frr().
# Clones metallb/frr-k8s, applies ovn-kubernetes patches, bumps the FRR image,
# and renames the container to plexus-frr.
# Sets FRR_TMP_DIR to the temp directory containing the clone.
clone_frr_k8s() {
  [ -n "$FRR_TMP_DIR" ] && [ -d "$FRR_TMP_DIR" ] && return

  FRR_TMP_DIR=$(mktemp -d)
  # Compose with any existing EXIT handler so we don't overwrite caller cleanup.
  local _prev_exit
  _prev_exit=$(trap -p EXIT | sed "s/trap -- \('.*'\) EXIT/\1/")
  if [ -n "$_prev_exit" ]; then
    # shellcheck disable=SC2064
    trap "${_prev_exit}; rm -rf \$FRR_TMP_DIR" EXIT
  else
    trap 'rm -rf $FRR_TMP_DIR' EXIT
  fi

  pushd "$FRR_TMP_DIR" >/dev/null
  git clone --quiet --no-tags --single-branch --branch main https://github.com/metallb/frr-k8s
  pushd frr-k8s >/dev/null
  git checkout --quiet --detach "$FRR_K8S_GIT_REF"
  popd >/dev/null

  # Download OVN-K patches (route-reflector-client support, etc.)
  curl -Ls https://github.com/jcaamano/frr-k8s/archive/refs/heads/ovnk-bgp-v0.0.21.tar.gz \
    | tar xzf - frr-k8s-ovnk-bgp-v0.0.21/patches --strip-components 1

  pushd frr-k8s >/dev/null
  # Normalize the image tag so the OVN-K patch context applies cleanly
  sed -i 's|quay.io/frrouting/frr:10.4.3|quay.io/frrouting/frr:9.1.0|g' hack/demo/demo.sh
  git apply ../patches/*

  # Bump the external demo container to FRR 10.6.0 (fixes EVPN + coredump issues)
  sed -i "s|${FRR_K8S_UPSTREAM_FRR_IMAGE}|${FRR_EXTERNAL_DEMO_IMAGE}|g" hack/demo/demo.sh

  # Rename the container from 'frr' to 'plexus-frr' so we don't collide
  # with an existing ovn-kubernetes kind cluster's FRR container.
  sed -i 's/--name frr\b/--name plexus-frr/g' hack/demo/demo.sh
  sed -i 's/docker rm -f frr$/docker rm -f plexus-frr/' hack/demo/demo.sh
  sed -i 's/" frr)/" plexus-frr)/' hack/demo/demo.sh

  popd >/dev/null
  popd >/dev/null
}

# Adapted from ovn-kubernetes/contrib/kind-common.sh: deploy_frr_external_container().
# Uses frr-k8s's demo.sh to create the external FRR container, then configures
# EVPN on the running instance.
deploy_external_frr() {
  local kubeconfig=$1; shift
  local node_ips=("$@")

  if $OCI_BIN ps --format '{{.Names}}' | grep -Eq '^plexus-frr$'; then
    echo "External FRR (plexus-frr) already running, reusing"
    if [ ${#node_ips[@]} -gt 0 ]; then
      add_frr_neighbors "${node_ips[@]}"
    fi
    return
  fi

  echo "Deploying external FRR route reflector via frr-k8s demo..."
  clone_frr_k8s

  pushd "${FRR_TMP_DIR}/frr-k8s/hack/demo" >/dev/null

  # demo.sh uses 'kubectl get nodes' to discover IPs; point it at our cluster
  KUBECONFIG="$kubeconfig" ./demo.sh
  popd >/dev/null

  # Wait for FRR daemons inside the container
  echo "Waiting for FRR daemons to start..."
  local attempts=0
  while ! $OCI_BIN exec plexus-frr vtysh -c "show daemons" >/dev/null 2>&1; do
    if (( ++attempts > 30 )); then
      echo "error: FRR daemons did not start after 30s"
      exit 1
    fi
    sleep 1
  done

  # Configure EVPN on the external FRR, same as ovn-kubernetes does when
  # ENABLE_EVPN=true: activate all neighbors in l2vpn evpn address-family
  # and set them as route-reflector-clients.
  echo "Configuring EVPN on external FRR..."
  local bgp_neighbors vtysh_cmds
  bgp_neighbors=$($OCI_BIN exec plexus-frr vtysh -c "show running-config" \
    | grep "^ neighbor.*remote-as" | awk '{print $2}')
  vtysh_cmds=(-c "configure terminal" -c "router bgp 64512" -c "address-family l2vpn evpn")
  for neighbor in $bgp_neighbors; do
    vtysh_cmds+=(-c "neighbor $neighbor activate")
    vtysh_cmds+=(-c "neighbor $neighbor route-reflector-client")
  done
  vtysh_cmds+=(-c "advertise-all-vni" -c "exit-address-family" -c "end" -c "write memory")
  $OCI_BIN exec plexus-frr vtysh "${vtysh_cmds[@]}"
  echo "FRR route reflector is running with EVPN"
}

# add_frr_neighbors IPS...
# Adds new BGP neighbors to the already-running plexus-frr container
# (used by multi-cluster to expand the neighbor set after initial creation).
add_frr_neighbors() {
  local ips=("$@")
  echo "Adding ${#ips[@]} BGP neighbor(s) to external FRR..."

  local vtysh_cmds=(-c "configure terminal" -c "router bgp 64512")
  for ip in "${ips[@]}"; do
    vtysh_cmds+=(-c "neighbor ${ip} remote-as 64512")
  done

  vtysh_cmds+=(-c "address-family l2vpn evpn")
  for ip in "${ips[@]}"; do
    vtysh_cmds+=(-c "neighbor ${ip} activate")
    vtysh_cmds+=(-c "neighbor ${ip} route-reflector-client")
  done
  vtysh_cmds+=(-c "exit-address-family")

  vtysh_cmds+=(-c "address-family ipv4 unicast")
  for ip in "${ips[@]}"; do
    vtysh_cmds+=(-c "neighbor ${ip} activate")
    vtysh_cmds+=(-c "neighbor ${ip} route-reflector-client")
  done
  vtysh_cmds+=(-c "exit-address-family")

  vtysh_cmds+=(-c "end" -c "write memory")

  $OCI_BIN exec plexus-frr vtysh "${vtysh_cmds[@]}"
  echo "BGP neighbors configured"
}

# Adapted from ovn-kubernetes/contrib/kind-common.sh: install_frr_k8s().
install_frr_k8s() {
  local kubeconfig=$1
  echo "Installing FRR-K8s in-cluster..."
  clone_frr_k8s

  KUBECONFIG="$kubeconfig" kubectl apply -f "${FRR_TMP_DIR}/frr-k8s/charts/frr-k8s/charts/crds/templates/"

  # Replace the upstream FRR image reference in the all-in-one manifest with
  # the version we actually deploy (10.6.0), matching ovn-kubernetes.
  sed -i "s|${FRR_K8S_ALL_IN_ONE_UPSTREAM_FRR_IMAGE}|${FRR_K8S_FRR_IMAGE}|g" \
    "${FRR_TMP_DIR}/frr-k8s/config/all-in-one/frr-k8s.yaml"
  # gcr.io/kubebuilder/kube-rbac-proxy is gone; use the k8s.io mirror.
  sed -i 's|gcr.io/kubebuilder/kube-rbac-proxy|registry.k8s.io/kubebuilder/kube-rbac-proxy|g' \
    "${FRR_TMP_DIR}/frr-k8s/config/all-in-one/frr-k8s.yaml"

  KUBECONFIG="$kubeconfig" kubectl apply -f "${FRR_TMP_DIR}/frr-k8s/config/all-in-one/frr-k8s.yaml"

  echo "Waiting for FRR-K8s to be ready..."
  if ! KUBECONFIG="$kubeconfig" kubectl rollout status \
      -n frr-k8s-system daemonset frr-k8s-daemon --timeout 5m; then
    echo "Warning: frr-k8s-daemon did not roll out within 5m; current pod state:"
    KUBECONFIG="$kubeconfig" kubectl get pods -n frr-k8s-system
  fi
}

# configure_frr_k8s_peering KUBECONFIG CLUSTER_NAME FRR_NETWORK
# Waits for the frr-k8s webhook, then applies a FRRConfiguration that tells
# frr-k8s nodes to accept all BGP routes from the external FRR route reflector.
#
# FRR_NETWORK is the Docker network on which FRR is connected to this cluster's
# nodes (e.g. "plexus-hub" or "plexus-spoke-1"). Each cluster's frr-k8s nodes
# must peer with FRR using FRR's IP on *their* Docker network; using FRR's hub
# IP for spoke clusters prevents BGP sessions from establishing.
configure_frr_k8s_peering() {
  local kubeconfig=$1 cluster_name=$2 frr_network=${3:-kind}

  echo "Waiting for FRR-K8s webhook to become ready..."
  # The webhook declares readiness before its endpoint is actually serving,
  # so curl from inside the control-plane node to verify.
  local r=0
  timeout 300s bash -x <<PROBE || r=$?
while true; do
  CLUSTER_IP=\$(KUBECONFIG="$kubeconfig" kubectl get svc -n frr-k8s-system frr-k8s-webhook-service -o jsonpath='{.spec.clusterIP}')
  $OCI_BIN exec "${cluster_name}-control-plane" curl -ksS --connect-timeout 30 "https://\${CLUSTER_IP}" && exit 0
  echo "Waiting for frr-k8s webhook..."
  sleep 1
done
PROBE
  if [ "$r" -ne 0 ]; then
    KUBECONFIG="$kubeconfig" kubectl describe pod -n frr-k8s-system -l app=frr-k8s-webhook-server
    KUBECONFIG="$kubeconfig" kubectl logs -n frr-k8s-system -l app=frr-k8s-webhook-server
  fi

  # Resolve FRR's IP on this cluster's Docker network so frr-k8s nodes peer
  # with the correct address (each cluster reaches FRR via its own subnet).
  local frr_ip
  frr_ip=$($OCI_BIN inspect plexus-frr \
    --format "{{(index .NetworkSettings.Networks \"${frr_network}\").IPAddress}}" 2>/dev/null)
  if [ -z "$frr_ip" ]; then
    echo "Warning: could not get FRR IP on network '${frr_network}'; falling back to kind bridge"
    frr_ip=$($OCI_BIN inspect plexus-frr \
      --format "{{(index .NetworkSettings.Networks \"kind\").IPAddress}}" 2>/dev/null)
  fi

  echo "Applying FRR-K8s peering config (FRR neighbor: ${frr_ip})..."
  KUBECONFIG="$kubeconfig" kubectl apply -n frr-k8s-system -f - <<EOF
apiVersion: frrk8s.metallb.io/v1beta1
kind: FRRConfiguration
metadata:
  name: receive-all
spec:
  bgp:
    routers:
    - asn: 64512
      neighbors:
      - address: ${frr_ip}
        asn: 64512
        toReceive:
          allowed:
            mode: all
EOF
}

build_plexus_image() {
  local image_name="localhost/plexus-controller:dev"
  if [ "${FORCE_BUILD:-false}" != true ] && $OCI_BIN image inspect "$image_name" >/dev/null 2>&1; then
    echo "Plexus image ${image_name} already exists, skipping build (use --force-build to rebuild)"
    PLEXUS_IMAGE="$image_name"
    return
  fi
  echo "Building Plexus controller image..."
  $OCI_BIN build -t "$image_name" "$PLEXUS_DIR"
  PLEXUS_IMAGE="$image_name"
}

deploy_plexus_controller() {
  local cluster_name=$1 kubeconfig=$2
  shift 2
  local vtep_cidrs=("$@")

  echo "Loading Plexus image into cluster ${cluster_name}..."
  kind load docker-image "$PLEXUS_IMAGE" --name "$cluster_name"

  local helm_args=(
    --set image.repository="${PLEXUS_IMAGE%:*}"
    --set image.tag="${PLEXUS_IMAGE##*:}"
    --set image.pullPolicy=Never
  )
  if [ ${#vtep_cidrs[@]} -gt 0 ]; then
    local vtep_json
    vtep_json=$(printf ',"%s"' "${vtep_cidrs[@]}")
    helm_args+=(--set-json "ovnKubernetes.vtepCIDRs=[${vtep_json:1}]")
  fi

  echo "Installing Plexus via Helm into ${cluster_name}..."
  KUBECONFIG="$kubeconfig" helm upgrade --install plexus "${PLEXUS_DIR}/helm/plexus" "${helm_args[@]}"

  echo "Waiting for Plexus controller..."
  if ! KUBECONFIG="$kubeconfig" kubectl rollout status \
      deployment -n plexus-system plexus-controller --timeout 2m; then
    echo "Warning: plexus-controller did not roll out within 2m; current pod state:"
    KUBECONFIG="$kubeconfig" kubectl get pods -n plexus-system
  fi
}

# deploy_ovnk_to_cluster NAME INDEX KUBECONFIG NETWORK FRR_NETWORK
# Installs frr-k8s, OVN-Kubernetes, and configures BGP peering on one cluster.
deploy_ovnk_to_cluster() {
  local name=$1 index=$2 kubeconfig=$3 network=$4 frr_network=$5

  compute_cidrs "$index"
  install_frr_k8s "$kubeconfig"
  helm_install_ovnk "$name" "$kubeconfig" "$network"
  wait_for_ovnk "$kubeconfig"
  configure_frr_k8s_peering "$kubeconfig" "$name" "$frr_network"
}

wait_for_ovnk() {
  local kubeconfig=$1
  echo "Waiting for OVN-Kubernetes pods..."
  local failed=false
  for resource in \
      "daemonset/ovs-node" \
      "deployment/ovnkube-control-plane" \
      "daemonset/ovnkube-node"; do
    if ! KUBECONFIG="$kubeconfig" kubectl rollout status \
        -n ovn-kubernetes "$resource" --timeout 5m; then
      echo "Warning: ${resource} did not roll out within 5m"
      failed=true
    fi
  done
  if [ "$failed" = true ]; then
    echo "Current OVN-Kubernetes pod state:"
    KUBECONFIG="$kubeconfig" kubectl get pods -n ovn-kubernetes
  fi
}

connect_frr_to_network() {
  local network=$1
  if ! $OCI_BIN network inspect "$network" >/dev/null 2>&1; then
    echo "error: Docker network ${network} does not exist" >&2
    return 1
  fi
  if $OCI_BIN inspect plexus-frr -f "{{(index .NetworkSettings.Networks \"${network}\").IPAddress}}" 2>/dev/null | grep -q .; then
    echo "FRR already connected to ${network}"
    return
  fi
  echo "Connecting FRR to Docker network ${network}..."
  $OCI_BIN network connect "$network" plexus-frr
}

# cleanup_all_plexus_clusters
# Detects and tears down every kind cluster whose name starts with "plexus",
# removes their Docker networks and kubeconfigs, and stops the FRR container.
# Works regardless of whether a single-cluster or multi-cluster deployment was used.
cleanup_all_plexus_clusters() {
  local clusters=()
  mapfile -t clusters < <(kind get clusters 2>/dev/null | grep '^plexus' || true)

  if [ "${#clusters[@]}" -eq 0 ]; then
    echo "No Plexus KIND clusters found."
  else
    echo "Deleting ${#clusters[@]} cluster(s): ${clusters[*]}"
    for name in "${clusters[@]}"; do
      kind delete cluster --name "$name" 2>/dev/null || true
      rm -f "${HOME}/${name}.conf"
      # Multi-cluster script names network == cluster (e.g. plexus-hub).
      # Single-cluster script names network plexus-<cluster> (e.g. plexus-plexus).
      # Try both; delete_docker_network is a no-op for networks that don't exist.
      delete_docker_network "$name"
      delete_docker_network "plexus-${name}"
    done
  fi

  cleanup_external_frr
}

cleanup_external_frr() {
  if $OCI_BIN ps -a --format '{{.Names}}' | grep -Eq '^plexus-frr$'; then
    echo "Removing external FRR container..."
    $OCI_BIN rm -f plexus-frr
  fi
}

delete_docker_network() {
  local name=$1
  if $OCI_BIN network inspect "$name" >/dev/null 2>&1; then
    echo "Removing Docker network ${name}..."
    $OCI_BIN network rm "$name" 2>/dev/null || true
  fi
}

create_spoke_secret() {
  local hub_kubeconfig=$1 spoke_name=$2 spoke_index=$3 spoke_network=${4:-kind}

  local spoke_api_url
  spoke_api_url=$(detect_api_url "$spoke_name" "$spoke_network")

  echo "Creating spoke Secret for ${spoke_name} on hub..."

  local raw_kubeconfig
  raw_kubeconfig=$(kind get kubeconfig --name "$spoke_name")

  local patched_kubeconfig
  patched_kubeconfig=$(echo "$raw_kubeconfig" | \
    sed "s|server: https://127.0.0.1:[0-9]*|server: ${spoke_api_url}|" | \
    sed 's|    certificate-authority-data:.*|    insecure-skip-tls-verify: true|')

  KUBECONFIG="$hub_kubeconfig" kubectl create namespace plexus-system 2>/dev/null || true

  echo "$patched_kubeconfig" | \
    KUBECONFIG="$hub_kubeconfig" kubectl create secret generic "${spoke_name}" \
      --namespace=plexus-system \
      --from-file=kubeconfig=/dev/stdin \
      --dry-run=client -o yaml | \
    KUBECONFIG="$hub_kubeconfig" kubectl apply -f -

  KUBECONFIG="$hub_kubeconfig" kubectl label secret "${spoke_name}" \
    --namespace=plexus-system \
    --overwrite \
    plexus.io/cluster=true \
    topology.kubernetes.io/zone="spoke-${spoke_index}"
}
