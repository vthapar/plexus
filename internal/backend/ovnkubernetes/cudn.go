package ovnkubernetes

// TODO: switch from ClusterUserDefinedNetwork to namespace-scoped
// UserDefinedNetwork once UDN EVPN support lands in OVN-Kubernetes.
// See https://github.com/ovn-kubernetes/ovn-kubernetes/issues/6604
//
// Currently using CUDN with a 1:1 namespace selector because the
// namespace-scoped UDN API does not support EVPN transport. When
// UDN gains EVPN support, this file should be replaced with udn.go
// creating namespace-scoped UserDefinedNetworks instead.

import (
	"context"
	"fmt"
	"hash/fnv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

const vtepName = "nd-vtep"

// domainRouteTarget returns a shared route target string for all subnets
// within an AND. All non-Isolated subnets in the same AND share this RT,
// enabling intra-domain route leaking via FRR. The wildcard AS (`*`) means
// "match any AS" for import; OVN-K handles the export RT separately using
// the cluster's actual ASN. The local admin is a deterministic hash of the
// AND name (range 1-65535).
func domainRouteTarget(andName string) string {
	h := fnv.New32a()
	h.Write([]byte(andName))
	local := (h.Sum32() % 65535) + 1
	return fmt.Sprintf("*:%d", local)
}

// cudnName returns the deterministic CUDN name for a subnet.
func cudnName(and *v1beta1.AdministrativeNetworkDomain, subnet *v1beta1.Subnet) string {
	return fmt.Sprintf("%s-%s", and.Name, subnet.Name)
}

// buildCUDN constructs the desired ClusterUserDefinedNetwork for a subnet.
// Non-Isolated subnets get an ipVRF with a shared route target to enable
// intra-domain route leaking (all subnets in the same AND can reach each
// other at L3). The shared route target causes FRR to import Type 2 host
// routes across ipVRFs via symmetric IRB.
func (b *OVNKubernetesBackend) buildCUDN(
	and *v1beta1.AdministrativeNetworkDomain,
	subnet *v1beta1.Subnet,
	vnis SubnetVNIs,
) *udnv1.ClusterUserDefinedNetwork {
	cidrs := make(udnv1.DualStackCIDRs, len(subnet.CIDRs))
	for i, c := range subnet.CIDRs {
		cidrs[i] = udnv1.CIDR(c)
	}

	evpnConfig := &udnv1.EVPNConfig{
		VTEP: vtepName,
		MACVRF: &udnv1.VRFConfig{
			VNI: int32(vnis.MACVRF),
		},
	}
	subnetType := subnet.TypeValue()
	if subnetType != v1beta1.SubnetTypeIsolated && vnis.IPVRF != 0 {
		evpnConfig.IPVRF = &udnv1.VRFConfig{
			VNI:         int32(vnis.IPVRF),
			RouteTarget: udnv1.RouteTargetString(domainRouteTarget(and.Name)),
		}
	}

	return &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: cudnName(and, subnet),
			Labels: map[string]string{
				labelNetworkDomain: and.Name,
				labelSubnet:        subnet.Name,
				labelSubnetType:    string(subnetType),
			},
		},
		Spec: udnv1.ClusterUserDefinedNetworkSpec{
			NamespaceSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					labelNetworkDomain: and.Name,
					labelSubnet:        subnet.Name,
				},
			},
			Network: udnv1.NetworkSpec{
				Topology:  udnv1.NetworkTopologyLayer2,
				Transport: udnv1.TransportOptionEVPN,
				Layer2: &udnv1.Layer2Config{
					Role:    udnv1.NetworkRolePrimary,
					Subnets: cidrs,
				},
				EVPN: evpnConfig,
			},
		},
	}
}

// reconcileCUDN ensures the CUDN for the given subnet exists and matches
// the desired state. On controller restart, existing VNIs are reserved
// in the allocator to rebuild state.
func (b *OVNKubernetesBackend) reconcileCUDN(
	ctx context.Context,
	and *v1beta1.AdministrativeNetworkDomain,
	subnet *v1beta1.Subnet,
	cl client.Client,
) error {
	name := cudnName(and, subnet)

	existing := &udnv1.ClusterUserDefinedNetwork{}
	err := cl.Get(ctx, client.ObjectKey{Name: name}, existing)
	if apierrors.IsNotFound(err) {
		vnis, allocErr := b.vniAllocator.AllocateSubnetVNIs(and.Name, subnet.Name, subnet.TypeValue())
		if allocErr != nil {
			return fmt.Errorf("allocating VNIs for subnet %q: %w", subnet.Name, allocErr)
		}

		desired := b.buildCUDN(and, subnet, vnis)
		b.log.Info("creating CUDN for subnet", "cudn", name, "macvrf-vni", vnis.MACVRF, "ipvrf-vni", vnis.IPVRF)
		if err := cl.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating CUDN %q: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting CUDN %q: %w", name, err)
	}

	if err := b.reserveVNIsFromExistingCUDN(and.Name, subnet.Name, existing); err != nil {
		return fmt.Errorf("reserving VNIs from existing CUDN %q: %w", name, err)
	}

	if err := b.updateCUDNLabels(ctx, existing, and, subnet, cl); err != nil {
		return err
	}

	return nil
}

// reserveVNIsFromExistingCUDN reads VNIs from an existing CUDN's EVPN
// config and reserves them in the allocator. This rebuilds allocator
// state on controller restart.
func (b *OVNKubernetesBackend) reserveVNIsFromExistingCUDN(andName, subnetName string, cudn *udnv1.ClusterUserDefinedNetwork) error {
	evpn := cudn.Spec.Network.EVPN
	if evpn == nil {
		return nil
	}

	if evpn.MACVRF != nil {
		if err := b.vniAllocator.allocator.ReserveID(
			subnetKey(andName, subnetName, "macvrf"),
			int(evpn.MACVRF.VNI),
		); err != nil {
			return fmt.Errorf("reserving MACVRF VNI %d: %w", evpn.MACVRF.VNI, err)
		}
	}

	if evpn.IPVRF != nil {
		if err := b.vniAllocator.allocator.ReserveID(
			subnetKey(andName, subnetName, "ipvrf"),
			int(evpn.IPVRF.VNI),
		); err != nil {
			return fmt.Errorf("reserving IPVRF VNI %d: %w", evpn.IPVRF.VNI, err)
		}
	}

	return nil
}

// updateCUDNLabels ensures the CUDN labels match the desired state.
func (b *OVNKubernetesBackend) updateCUDNLabels(
	ctx context.Context,
	existing *udnv1.ClusterUserDefinedNetwork,
	and *v1beta1.AdministrativeNetworkDomain,
	subnet *v1beta1.Subnet,
	cl client.Client,
) error {
	desired := map[string]string{
		labelNetworkDomain: and.Name,
		labelSubnet:        subnet.Name,
		labelSubnetType:    string(subnet.TypeValue()),
	}

	needsUpdate := false
	if existing.Labels == nil {
		existing.Labels = make(map[string]string)
	}
	for k, v := range desired {
		if existing.Labels[k] != v {
			existing.Labels[k] = v
			needsUpdate = true
		}
	}

	if needsUpdate {
		b.log.Info("updating CUDN labels", "cudn", existing.Name)
		if err := cl.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating CUDN %q labels: %w", existing.Name, err)
		}
	}

	return nil
}

// deleteCUDN removes the CUDN for the given subnet and releases its VNIs.
func (b *OVNKubernetesBackend) deleteCUDN(
	ctx context.Context,
	and *v1beta1.AdministrativeNetworkDomain,
	subnet *v1beta1.Subnet,
	cl client.Client,
) error {
	name := cudnName(and, subnet)

	cudn := &udnv1.ClusterUserDefinedNetwork{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	err := cl.Delete(ctx, cudn)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting CUDN %q: %w", name, err)
	}

	b.vniAllocator.ReleaseSubnetVNIs(and.Name, subnet.Name)
	return nil
}
