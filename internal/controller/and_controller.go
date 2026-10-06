package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"

	v1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
	"github.com/ovn-kubernetes/plexus/internal/backend"
	"github.com/ovn-kubernetes/plexus/internal/multicluster"
)

const finalizerName = "plexus.io/and-protection"

// ANDReconciler reconciles AdministrativeNetworkDomain resources.
//
// TODO: emit Kubernetes Events on significant state changes (subnet
// created/deleted, VTEP not ready, reconcile error) for observability
// via kubectl describe. Use record.EventRecorder from the manager.
//
// TODO: add Prometheus metrics for reconciliation latency, error counts,
// VNI pool utilization, and child resource health. Register via the
// controller-runtime metrics registry.
type ANDReconciler struct {
	client.Client
	Backend   backend.Backend
	Inventory *multicluster.SecretInventory
}

// +kubebuilder:rbac:groups=plexus.io,resources=administrativenetworkdomains,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=plexus.io,resources=administrativenetworkdomains/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=plexus.io,resources=administrativenetworkdomains/finalizers,verbs=update
// +kubebuilder:rbac:groups=plexus.io,resources=plexuscontrollerconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=clusteruserdefinednetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=routeadvertisements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=vteps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=vteps/status,verbs=get
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=clusteruserdefinednetworks/status,verbs=get
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=routeadvertisements/status,verbs=get

func (r *ANDReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if err := r.Inventory.Sync(ctx); err != nil {
		logger.Error(err, "failed to sync cluster inventory")
		return ctrl.Result{}, fmt.Errorf("syncing cluster inventory: %w", err)
	}

	and := &v1beta1.AdministrativeNetworkDomain{}
	if err := r.Get(ctx, req.NamespacedName, and); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !and.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(and, finalizerName) {
			logger.Info("deleting AND resources", "name", and.Name)
			if err := r.Backend.Delete(ctx, and); err != nil {
				return ctrl.Result{}, fmt.Errorf("backend delete: %w", err)
			}
			controllerutil.RemoveFinalizer(and, finalizerName)
			if err := r.Update(ctx, and); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(and, finalizerName) {
		controllerutil.AddFinalizer(and, finalizerName)
		if err := r.Update(ctx, and); err != nil {
			return ctrl.Result{}, err
		}
	}

	noSubnets := len(and.Subnets()) == 0
	if and.Status == nil {
		and.Status = &v1beta1.AdministrativeNetworkDomainStatus{}
	}

	result, err := r.Backend.Reconcile(ctx, and)
	if err != nil {
		meta.SetStatusCondition(&and.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "ReconcileError",
			Message:            err.Error(),
			ObservedGeneration: and.Generation,
		})
		_ = r.Status().Update(ctx, and)
		return ctrl.Result{}, fmt.Errorf("backend reconcile: %w", err)
	}

	if noSubnets {
		meta.SetStatusCondition(&and.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             "NoSubnets",
			Message:            "No subnets defined; domain is awaiting subnet configuration",
			ObservedGeneration: and.Generation,
		})
	} else if result.StatusReason != "" {
		meta.SetStatusCondition(&and.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionFalse,
			Reason:             result.StatusReason,
			Message:            result.StatusMessage,
			ObservedGeneration: and.Generation,
		})
	} else {
		meta.SetStatusCondition(&and.Status.Conditions, metav1.Condition{
			Type:               "Ready",
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            fmt.Sprintf("All %d subnets reconciled by %s backend", len(and.Subnets()), r.Backend.Name()),
			ObservedGeneration: and.Generation,
		})
	}
	if err := r.Status().Update(ctx, and); err != nil {
		return ctrl.Result{}, err
	}

	if result.Requeue {
		// Use Requeue (not RequeueAfter) to leverage the workqueue's
		// built-in exponential backoff rate limiter (5ms to ~16min).
		// This avoids custom backoff logic and follows the standard
		// controller-runtime pattern for transient not-ready states.
		return ctrl.Result{Requeue: true}, nil //nolint:staticcheck // intentional: leverages workqueue exponential backoff
	}
	return ctrl.Result{}, nil
}

func andNameFromLabels(_ context.Context, obj client.Object) []reconcile.Request {
	andName := obj.GetLabels()["plexus.io/network-domain"]
	if andName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: andName}}}
}

func (r *ANDReconciler) plexusVTEPToANDs(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()["plexus.io/managed-by"] != "plexus" {
		return nil
	}
	var andList v1beta1.AdministrativeNetworkDomainList
	if err := r.List(ctx, &andList); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, len(andList.Items))
	for i := range andList.Items {
		requests[i] = reconcile.Request{NamespacedName: client.ObjectKey{Name: andList.Items[i].Name}}
	}
	return requests
}

// clusterSecretToANDs maps changes to cluster Secrets (plexus.io/cluster=true)
// to all AND resources, since a cluster inventory change may affect which
// clusters each subnet targets.
func (r *ANDReconciler) clusterSecretToANDs(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[multicluster.LabelCluster] != "true" {
		return nil
	}
	var andList v1beta1.AdministrativeNetworkDomainList
	if err := r.List(ctx, &andList); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, len(andList.Items))
	for i := range andList.Items {
		requests[i] = reconcile.Request{NamespacedName: client.ObjectKey{Name: andList.Items[i].Name}}
	}
	return requests
}

func (r *ANDReconciler) SetupWithManager(mgr ctrl.Manager) error {
	clusterSecretFilter := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[multicluster.LabelCluster] == "true"
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.AdministrativeNetworkDomain{}).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(andNameFromLabels)).
		Watches(&udnv1.ClusterUserDefinedNetwork{}, handler.EnqueueRequestsFromMapFunc(andNameFromLabels)).
		Watches(&rav1.RouteAdvertisements{}, handler.EnqueueRequestsFromMapFunc(andNameFromLabels)).
		Watches(&vtepv1.VTEP{}, handler.EnqueueRequestsFromMapFunc(r.plexusVTEPToANDs)).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.clusterSecretToANDs),
			builder.WithPredicates(clusterSecretFilter),
		).
		Named("and").
		Complete(r)
}
