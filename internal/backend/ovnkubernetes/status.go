package ovnkubernetes

import (
	"context"
	"fmt"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
	"github.com/ovn-kubernetes/plexus/internal/backend"
	"github.com/ovn-kubernetes/plexus/internal/multicluster"
)

// checkResourceStatus inspects the status conditions of all child resources
// (VTEP, CUDNs, RouteAdvertisements) across all clusters and returns a Result
// indicating whether the AND should be marked as not-ready.
func (b *OVNKubernetesBackend) checkResourceStatus(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain, clusters []multicluster.ClusterInfo) (backend.Result, error) {
	for _, ci := range clusters {
		if result, err := b.checkClusterResourceStatus(ctx, and, ci.Client, ci.Name); err != nil || result.Requeue {
			return result, err
		}
	}
	return backend.Result{}, nil
}

func (b *OVNKubernetesBackend) checkClusterResourceStatus(ctx context.Context, and *v1beta1.AdministrativeNetworkDomain, cl client.Client, clusterName string) (backend.Result, error) {
	vtep := &vtepv1.VTEP{}
	if err := cl.Get(ctx, client.ObjectKey{Name: vtepName}, vtep); err != nil {
		return backend.Result{}, fmt.Errorf("getting VTEP %q status on cluster %q: %w", vtepName, clusterName, err)
	}
	cond := apimeta.FindStatusCondition(vtep.Status.Conditions, "Accepted")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		msg := "VTEP not yet accepted"
		if cond != nil {
			msg = cond.Message
		}
		return backend.Result{
			Requeue:       true,
			StatusReason:  "VTEPNotReady",
			StatusMessage: fmt.Sprintf("cluster %q: %s", clusterName, msg),
		}, nil
	}

	subnets := and.Subnets()
	for i := range subnets {
		name := cudnName(and, &subnets[i])
		cudn := &udnv1.ClusterUserDefinedNetwork{}
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, cudn); err != nil {
			return backend.Result{}, fmt.Errorf("getting CUDN %q status on cluster %q: %w", name, clusterName, err)
		}
		cond := apimeta.FindStatusCondition(cudn.Status.Conditions, "NetworkCreated")
		if cond == nil || cond.Status != metav1.ConditionTrue {
			msg := "network not yet created"
			if cond != nil {
				msg = cond.Message
			}
			return backend.Result{
				Requeue:       true,
				StatusReason:  "SubnetsNotReady",
				StatusMessage: fmt.Sprintf("cluster %q: CUDN %q not ready: %s", clusterName, name, msg),
			}, nil
		}
	}

	hasEVPN := false
	for i := range subnets {
		if subnets[i].TypeValue() != v1beta1.SubnetTypeIsolated {
			hasEVPN = true
			break
		}
	}
	if hasEVPN {
		name := raName(and)
		ra := &rav1.RouteAdvertisements{}
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, ra); err != nil {
			return backend.Result{}, fmt.Errorf("getting RouteAdvertisements %q status on cluster %q: %w", name, clusterName, err)
		}
		cond := apimeta.FindStatusCondition(ra.Status.Conditions, "Accepted")
		if cond == nil || cond.Status != metav1.ConditionTrue {
			msg := "RouteAdvertisements not yet accepted"
			if cond != nil {
				msg = cond.Message
			}
			return backend.Result{
				Requeue:       true,
				StatusReason:  "SubnetsNotReady",
				StatusMessage: fmt.Sprintf("cluster %q: RouteAdvertisements %q not accepted: %s", clusterName, name, msg),
			}, nil
		}
	}

	return backend.Result{}, nil
}
