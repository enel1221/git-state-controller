package main

import (
	"flag"
	"os"
	"time"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"github.com/inelson/git-state-controller/internal/controller"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	// +kubebuilder:scaffold:imports
)

func main() {
	var namespace, argoNamespace, setName, probe string
	var workers int
	var allowHTTP, leader bool
	var timeout, poll time.Duration
	flag.StringVar(&namespace, "controller-namespace", "git-state-system", "Namespace allowed to contain Git credential Secrets")
	flag.StringVar(&argoNamespace, "argocd-namespace", "argocd", "ApplicationSet namespace (match scoped RBAC)")
	flag.StringVar(&setName, "applicationset-name", "git-resources", "Managed ApplicationSet (match scoped RBAC)")
	flag.StringVar(&probe, "health-probe-bind-address", ":8081", "Probe address")
	flag.IntVar(&workers, "max-concurrent-reconciles", 4, "Concurrent GitResource workers")
	flag.BoolVar(&allowHTTP, "allow-http", false, "Explicit exception for disposable local Git servers")
	flag.BoolVar(&leader, "leader-elect", true, "Enable manager leader election")
	flag.DurationVar(&timeout, "git-operation-timeout", 2*time.Minute, "Bound for all Git attempts in one reconciliation")
	flag.DurationVar(&poll, "resource-poll-interval", 15*time.Second, "Exact target observation interval")
	options := zap.Options{Development: false}
	options.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&options)))
	if workers < 1 || timeout <= 0 || poll <= 0 {
		ctrl.Log.Error(nil, "workers and timeout must be positive")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(api.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: ":8080"}, HealthProbeBindAddress: probe,
		LeaderElection: leader, LeaderElectionNamespace: namespace, LeaderElectionID: "git-state-controller.gitops.example.io", LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}:                  {Namespaces: map[string]cache.Config{namespace: {}}},
			controller.ApplicationObject():    {Namespaces: map[string]cache.Config{argoNamespace: {}}, Label: labels.SelectorFromSet(labels.Set{"gitops.example.io/managed": "true"})},
			controller.ApplicationSetObject(): {Namespaces: map[string]cache.Config{argoNamespace: {}}, Field: fields.OneTermEqualSelector("metadata.name", setName)},
		}},
	})
	if err != nil {
		ctrl.Log.Error(err, "Create manager")
		os.Exit(1)
	}
	publication := &controller.GitResourceReconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Namespace: namespace, AllowHTTP: allowHTTP, Workers: workers, OperationTimeout: timeout, Recorder: mgr.GetEventRecorder("git-publication"), SetKey: types.NamespacedName{Namespace: argoNamespace, Name: setName}}
	if err = publication.SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "Set up publication controller")
		os.Exit(1)
	}
	inventory := &controller.ApplicationSetReconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Key: types.NamespacedName{Namespace: argoNamespace, Name: setName}, Recorder: mgr.GetEventRecorder("applicationset-inventory")}
	if err = inventory.SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "Set up inventory controller")
		os.Exit(1)
	}
	observer := &controller.StatusReconciler{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), SetKey: inventory.Key, PollInterval: poll}
	if err = observer.SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "Set up status observer")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))
	if err = mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "Run manager")
		os.Exit(1)
	}
}
