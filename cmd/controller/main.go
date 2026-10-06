package main

import (
	"context"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	rav1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/routeadvertisements/v1"
	udnv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/userdefinednetwork/v1"
	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"

	andv1beta1 "github.com/ovn-kubernetes/plexus/api/administrativenetworkdomain/v1beta1"
	configv1beta1 "github.com/ovn-kubernetes/plexus/api/plexuscontrollerconfig/v1beta1"
	"github.com/ovn-kubernetes/plexus/internal/backend/ovnkubernetes"
	"github.com/ovn-kubernetes/plexus/internal/controller"
	"github.com/ovn-kubernetes/plexus/internal/multicluster"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(andv1beta1.AddToScheme(scheme))
	utilruntime.Must(configv1beta1.AddToScheme(scheme))
	utilruntime.Must(udnv1.AddToScheme(scheme))
	utilruntime.Must(rav1.AddToScheme(scheme))
	utilruntime.Must(vtepv1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var clusterSecretNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&clusterSecretNamespace, "cluster-secret-namespace", "", "Restrict cluster Secret discovery to this namespace (default: all namespaces).")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	restConfig := ctrl.GetConfigOrDie()

	directClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "unable to create direct client for config lookup")
		os.Exit(1)
	}

	plexusConfig := &configv1beta1.PlexusControllerConfig{}
	if err := directClient.Get(context.Background(), client.ObjectKey{Name: "plexus"}, plexusConfig); err != nil {
		setupLog.Error(err, "unable to fetch PlexusControllerConfig 'plexus'")
		os.Exit(1)
	}
	if plexusConfig.Spec == nil || plexusConfig.Spec.OVNKubernetes == nil {
		setupLog.Error(nil, "PlexusControllerConfig 'plexus' is missing ovnKubernetes configuration")
		os.Exit(1)
	}

	// TODO: enable leader election by default for production deployments.
	// Currently disabled unless --leader-elect is passed. Multi-replica
	// deployments MUST use leader election to prevent split-brain VNI
	// allocation and duplicate resource creation.
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "plexus-controller.plexus.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	inventory := multicluster.NewSecretInventory(multicluster.SecretInventoryOptions{
		HubClient: mgr.GetClient(),
		HubLabels: map[string]string{},
		Scheme:    scheme,
		Namespace: clusterSecretNamespace,
		Log:       ctrl.Log,
	})

	backend := ovnkubernetes.New(mgr.GetClient(), ctrl.Log, plexusConfig.Spec.OVNKubernetes, inventory)

	if err := (&controller.ANDReconciler{
		Client:    mgr.GetClient(),
		Backend:   backend,
		Inventory: inventory,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "AND")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting plexus controller")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
