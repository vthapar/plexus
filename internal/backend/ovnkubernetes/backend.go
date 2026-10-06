package ovnkubernetes

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
	configv1beta1 "github.com/ovn-kubernetes/plexus/api/plexuscontrollerconfig/v1beta1"
	"github.com/ovn-kubernetes/plexus/internal/backend"
	"github.com/ovn-kubernetes/plexus/internal/multicluster"
)

// OVNKubernetesBackend translates AND intent into OVN-Kubernetes resources
// (namespaces, CUDNs, RouteAdvertisements, VTEPs, etc.).
//
// In multi-cluster mode, the backend uses the ClusterInventory to push
// resources to spoke clusters matching each subnet's clusterSelector.
// In single-cluster mode, the inventory contains only the hub.
type OVNKubernetesBackend struct {
	client       client.Client
	log          logr.Logger
	vniAllocator *VNIAllocator
	config       *configv1beta1.OVNKubernetesConfig
	inventory    multicluster.ClusterInventory
}

func New(c client.Client, log logr.Logger, config *configv1beta1.OVNKubernetesConfig, inventory multicluster.ClusterInventory) *OVNKubernetesBackend {
	return &OVNKubernetesBackend{
		client:       c,
		log:          log.WithName("ovn-kubernetes-backend"),
		vniAllocator: NewVNIAllocator(),
		config:       config,
		inventory:    inventory,
	}
}

func (b *OVNKubernetesBackend) Name() string {
	return "ovn-kubernetes"
}

func (b *OVNKubernetesBackend) Reconcile(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain) (backend.Result, error) {
	subnets := and.Subnets()
	b.log.Info("reconciling AND", "name", and.Name, "subnets", len(subnets))

	for i := range subnets {
		subnet := &subnets[i]
		clusters, err := b.targetClusters(subnet)
		if err != nil {
			return backend.Result{}, fmt.Errorf("resolving clusters for subnet %q: %w", subnet.Name, err)
		}
		for _, ci := range clusters {
			if err := b.reconcileSubnet(ctx, and, subnet, ci.Client); err != nil {
				return backend.Result{}, fmt.Errorf("reconciling subnet %q on cluster %q: %w", subnet.Name, ci.Name, err)
			}
		}
	}

	allClusters, err := b.inventory.AllClusters()
	if err != nil {
		return backend.Result{}, fmt.Errorf("listing clusters for garbage collection: %w", err)
	}
	for _, ci := range allClusters {
		if err := b.garbageCollectSubnets(ctx, and, ci.Client); err != nil {
			return backend.Result{}, fmt.Errorf("garbage collecting subnets on cluster %q: %w", ci.Name, err)
		}
	}

	for _, ci := range allClusters {
		if err := b.reconcileVTEP(ctx, ci.Client); err != nil {
			return backend.Result{}, fmt.Errorf("reconciling VTEP on cluster %q: %w", ci.Name, err)
		}
		if err := b.reconcileRouteAdvertisements(ctx, and, ci.Client); err != nil {
			return backend.Result{}, fmt.Errorf("reconciling RouteAdvertisements on cluster %q: %w", ci.Name, err)
		}
	}

	return b.checkResourceStatus(ctx, and, allClusters)
}

func (b *OVNKubernetesBackend) Delete(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain) error {
	b.log.Info("deleting AND resources", "name", and.Name)

	allClusters, err := b.inventory.AllClusters()
	if err != nil {
		return fmt.Errorf("listing clusters for deletion: %w", err)
	}

	for _, ci := range allClusters {
		namespaces, err := b.listSubnetNamespaces(ctx, and, ci.Client)
		if err != nil {
			return fmt.Errorf("listing namespaces on cluster %q for deletion: %w", ci.Name, err)
		}
		for _, ns := range namespaces {
			subnetName := ns.Labels[labelSubnet]
			if err := b.deleteSubnet(ctx, and, &v1beta1.Subnet{Name: subnetName}, ci.Client); err != nil {
				return fmt.Errorf("deleting subnet %q on cluster %q: %w", subnetName, ci.Name, err)
			}
		}

		if err := b.deleteRouteAdvertisements(ctx, and, ci.Client); err != nil {
			return fmt.Errorf("deleting RouteAdvertisements on cluster %q: %w", ci.Name, err)
		}

		if err := b.deleteVTEP(ctx, and.Name, ci.Client); err != nil {
			return fmt.Errorf("deleting VTEP on cluster %q: %w", ci.Name, err)
		}
	}

	return nil
}

// targetClusters resolves which clusters a subnet should be rendered to,
// based on the subnet's availabilityZone.clusterSelector. If no AZ is
// set, all clusters in the inventory are returned.
func (b *OVNKubernetesBackend) targetClusters(subnet *v1beta1.Subnet) ([]multicluster.ClusterInfo, error) {
	var selector *metav1.LabelSelector
	if subnet.AvailabilityZone != nil {
		selector = subnet.AvailabilityZone.ClusterSelector
	}
	return b.inventory.MatchClusters(selector)
}

func (b *OVNKubernetesBackend) listSubnetNamespaces(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain, cl client.Client) ([]corev1.Namespace, error) {
	var nsList corev1.NamespaceList
	if err := cl.List(ctx, &nsList, client.MatchingLabels{labelNetworkDomain: and.Name}); err != nil {
		return nil, fmt.Errorf("listing namespaces for AND %q: %w", and.Name, err)
	}
	return nsList.Items, nil
}

func (b *OVNKubernetesBackend) garbageCollectSubnets(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain, cl client.Client) error {
	namespaces, err := b.listSubnetNamespaces(ctx, and, cl)
	if err != nil {
		return err
	}

	subnets := and.Subnets()
	desired := make(map[string]struct{}, len(subnets))
	for _, s := range subnets {
		desired[s.Name] = struct{}{}
	}

	for _, ns := range namespaces {
		subnetName := ns.Labels[labelSubnet]
		if _, ok := desired[subnetName]; ok {
			continue
		}
		b.log.Info("garbage collecting orphaned subnet", "subnet", subnetName, "namespace", ns.Name)
		if err := b.deleteSubnet(ctx, and, &v1beta1.Subnet{Name: subnetName}, cl); err != nil {
			return fmt.Errorf("garbage collecting subnet %q: %w", subnetName, err)
		}
	}

	return nil
}
