package main

import (
	"context"
	"flag"
	"os"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/spf13/pflag"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	// +kubebuilder:scaffold:imports

	"github.com/istio-ecosystem/fortsa/internal/controller"
	"github.com/istio-ecosystem/fortsa/internal/webhook"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// parseSkipNamespaces splits a comma-separated string into non-empty trimmed namespace names.
func parseSkipNamespaces(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))

	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var dryRun bool
	var compareHub bool
	var restartDelay time.Duration
	var istiodConfigReadDelay time.Duration
	var reconcilePeriod time.Duration
	var skipNamespaces string
	pflag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	pflag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election for controller manager.")
	pflag.BoolVar(&dryRun, "dry-run", false, "If true, log what would be done without annotating workloads.")
	pflag.BoolVar(&compareHub, "compare-hub", false, "If true, require container image registry to match ConfigMap hub when detecting outdated pods.")
	pflag.DurationVar(&restartDelay, "restart-delay", 0, "Delay between restarting each workload (e.g. 5s). Use 0 for no delay.")
	pflag.DurationVar(&istiodConfigReadDelay, "istiod-config-read-delay", 10*time.Second, "Wait for Istiod to read the updated ConfigMap before scanning (e.g. 10s). Use 0 to skip.")
	pflag.DurationVar(&reconcilePeriod, "reconcile-period", 1*time.Hour, "Period between full reconciliations of all istio-sidecar-injector ConfigMaps. Use 0 to disable periodic reconciliation.")
	pflag.StringVar(&skipNamespaces, "skip-namespaces", "kube-system,istio-system", "Comma-separated list of namespaces to skip when scanning pods for outdated sidecars.")

	zapOpts := zap.Options{
		Development: true,
	}
	zapOpts.BindFlags(flag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		Metrics:          metricsserver.Options{BindAddress: metricsAddr},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "71f32f9d.fortsa.scaffidi.net",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	webhookClient := webhook.NewWebhookClient(mgr.GetClient())
	reconciler := controller.NewConfigMapReconciler(mgr.GetClient(), mgr.GetScheme(), dryRun, compareHub, restartDelay, istiodConfigReadDelay, parseSkipNamespaces(skipNamespaces), webhookClient)

	builder := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}).
		WithEventFilter(predicate.NewPredicateFuncs(controller.ConfigMapFilter())).
		Watches(
			&admissionregv1.MutatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{controller.PeriodicReconcileRequest()}
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(controller.MutatingWebhookFilter())),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{controller.NamespaceReconcileRequest(obj.GetName())}
			}),
			builder.WithPredicates(controller.NamespaceFilter()),
		)
	if reconcilePeriod > 0 {
		builder = builder.WatchesRawSource(controller.NewPeriodicReconcileSource(reconcilePeriod))
	}
	err = builder.Complete(reconciler)
	if err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if dryRun {
		setupLog.Info("starting manager in dry-run mode (no workloads will be annotated)")
	} else {
		setupLog.Info("starting manager")
	}
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
