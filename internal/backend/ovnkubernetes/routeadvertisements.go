package ovnkubernetes

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	crdtypes "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/types"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
)

func raName(and *v1beta1.AdministrativeNetworkDomain) string {
	return and.Name
}

func (b *OVNKubernetesBackend) reconcileRouteAdvertisements(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain, cl client.Client) error {
	hasEVPN := false
	subnets := and.Subnets()
	for i := range subnets {
		if subnets[i].TypeValue() != v1beta1.SubnetTypeIsolated {
			hasEVPN = true
			break
		}
	}

	name := raName(and)

	if !hasEVPN {
		return b.deleteRouteAdvertisementsByName(ctx, name, cl)
	}

	existing := &rav1.RouteAdvertisements{}
	err := cl.Get(ctx, client.ObjectKey{Name: name}, existing)
	if apierrors.IsNotFound(err) {
		ra := b.buildRouteAdvertisements(and)
		b.log.Info("creating RouteAdvertisements", "name", name)
		if err := cl.Create(ctx, ra); err != nil {
			return fmt.Errorf("creating RouteAdvertisements %q: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting RouteAdvertisements %q: %w", name, err)
	}

	desired := b.buildRouteAdvertisements(and)
	existing.Spec = desired.Spec
	if existing.Labels == nil {
		existing.Labels = make(map[string]string)
	}
	for k, v := range desired.Labels {
		existing.Labels[k] = v
	}
	b.log.Info("updating RouteAdvertisements", "name", name)
	if err := cl.Update(ctx, existing); err != nil {
		return fmt.Errorf("updating RouteAdvertisements %q: %w", name, err)
	}

	return nil
}

func (b *OVNKubernetesBackend) buildRouteAdvertisements(and *v1beta1.AdministrativeNetworkDomain) *rav1.RouteAdvertisements {
	frrSelector := metav1.LabelSelector{}
	if b.config != nil && b.config.FRRConfigurationSelector != nil {
		frrSelector = *b.config.FRRConfigurationSelector
	}

	return &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: raName(and),
			Labels: map[string]string{
				labelManagedBy:     "plexus",
				labelNetworkDomain: and.Name,
			},
		},
		Spec: rav1.RouteAdvertisementsSpec{
			TargetVRF: "auto",
			NetworkSelectors: crdtypes.NetworkSelectors{
				{
					NetworkSelectionType: crdtypes.ClusterUserDefinedNetworks,
					ClusterUserDefinedNetworkSelector: &crdtypes.ClusterUserDefinedNetworkSelector{
						NetworkSelector: metav1.LabelSelector{
							MatchLabels: map[string]string{
								labelNetworkDomain: and.Name,
							},
							MatchExpressions: []metav1.LabelSelectorRequirement{
								{
									Key:      labelSubnetType,
									Operator: metav1.LabelSelectorOpIn,
									Values:   []string{string(v1beta1.SubnetTypePublic), string(v1beta1.SubnetTypePrivate)},
								},
							},
						},
					},
				},
			},
			NodeSelector:             metav1.LabelSelector{},
			FRRConfigurationSelector: frrSelector,
			Advertisements:           []rav1.AdvertisementType{rav1.PodNetwork},
		},
	}
}

func (b *OVNKubernetesBackend) deleteRouteAdvertisements(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain, cl client.Client) error {
	return b.deleteRouteAdvertisementsByName(ctx, raName(and), cl)
}

func (b *OVNKubernetesBackend) deleteRouteAdvertisementsByName(ctx context.Context, name string, cl client.Client) error {
	ra := &rav1.RouteAdvertisements{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}
	err := cl.Delete(ctx, ra)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting RouteAdvertisements %q: %w", name, err)
	}
	return nil
}
