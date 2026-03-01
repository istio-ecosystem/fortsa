# Fortsa: linear code walkthrough

*2026-03-01T05:50:22Z by Showboat 0.6.1*
<!-- showboat-id: 0e944a91-1c59-402e-ac1b-c26912199434 -->

This document is a linear, source-backed walkthrough of how Fortsa works.

It is written as an executable demo: every code snippet is produced by running commands against this repository and capturing the output. You can re-run and diff all embedded snippets with:

  uvx showboat verify walkthrough.md

Linear walkthrough plan (followed below):

1. Entry point: flags, manager, controller wiring (cmd/main.go)
2. Watches and how events are turned into reconcile requests
3. Reconcile routing: periodic vs MWC vs namespace vs ConfigMap (internal/controller)
4. ConfigMap parsing: extract revision/hub/tag/image and compute last-modified (internal/configmap)
5. Change detection: revision cache for last-modified and delete cleanup (internal/cache)
6. Tag mapping: build tag -> revision and tag last-modified from istio-revision-tag-* MWCs (internal/mwc)
7. Pod scanning: list pods, skip logic, dedupe workloads, and per-pod evaluation (internal/podscanner)
8. Owner chain traversal: Pod -> ReplicaSet/ControllerRevision -> Deployment/StatefulSet/DaemonSet
9. Expected state: build a pod from the workload template and call the Istio injection webhook (internal/webhook)
10. Detect outdated sidecars: compare istio-proxy image (with optional hub comparison)
11. Actuation: patch workload pod templates with restartedAt (cooldown + optional delay) (internal/annotator)
12. Periodic and namespace triggers: how they feed into the same scan+annotate pipeline

## 1) Entry point and controller wiring (cmd/main.go)

Fortsa runs as a controller-runtime manager. The entry point is responsible for:

- Parsing flags that control safety behavior (dry-run, compare-hub, delays, cooldowns, skip namespaces)
- Configuring logging and TLS options (notably: HTTP/2 is disabled by default)
- Creating the controller-runtime Manager
- Constructing the Webhook client and the main reconciler
- Defining which objects are watched and how events enqueue reconcile requests

Start by looking at flag parsing and safety-related defaults.

```bash
sed -n '72,175p' cmd/main.go
```

```output
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
	var annotationCooldown time.Duration
	var skipNamespaces string
	var probeAddr string
	var secureMetrics bool
	var webhookCertPath, webhookCertName, webhookCertKey string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var enableHTTP2 bool
	var showVersion bool
	var tlsOpts []func(*tls.Config)
	pflag.BoolVar(&showVersion, "version", false, "Print version information and exit.")
	pflag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	pflag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	pflag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable leader election for controller manager.")
	pflag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	pflag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	pflag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	pflag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	pflag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	pflag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	pflag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	pflag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	pflag.BoolVar(&dryRun, "dry-run", false, "If true, log what would be done without annotating workloads.")
	pflag.BoolVar(&compareHub, "compare-hub", false,
		"If true, require container image registry to match ConfigMap hub when detecting outdated pods.")
	pflag.DurationVar(&restartDelay, "restart-delay", 0,
		"Delay between restarting each workload (e.g. 5s). Use 0 for no delay.")
	pflag.DurationVar(&istiodConfigReadDelay, "istiod-config-read-delay", 10*time.Second,
		"Wait for Istiod to read the updated ConfigMap before scanning (e.g. 10s). Use 0 to skip.")
	pflag.DurationVar(&reconcilePeriod, "reconcile-period", 0,
		"Period between full reconciliations of all istio-sidecar-injector ConfigMaps. "+
			"Use 0 to disable periodic reconciliation.")
	pflag.DurationVar(&annotationCooldown, "annotation-cooldown", 5*time.Minute,
		"Skip re-annotating a workload if it was annotated within this duration. Use 0 to disable.")
	pflag.StringVar(&skipNamespaces, "skip-namespaces", "kube-system,istio-system",
		"Comma-separated list of namespaces to skip when scanning pods for outdated sidecars.")

	zapOpts := zap.Options{
		Development: true,
	}
	zapOpts.BindFlags(flag.CommandLine)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()

	if showVersion {
		v := Version
		if v == "" {
			v = "dev"
		}
		_, _ = fmt.Fprintf(os.Stdout, "fortsa version %s\n", v)
		if Commit != "" {
			_, _ = fmt.Fprintf(os.Stdout, "  commit: %s\n", Commit)
		}
		if BuildTime != "" {
			_, _ = fmt.Fprintf(os.Stdout, "  build time: %s\n", BuildTime)
		}
		os.Exit(0)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}
```

Next is manager construction and the controller wiring. The important detail is that Fortsa's controller is `For(ConfigMap)` and additionally `Watches` MWCs and Namespaces. A periodic source can also enqueue a synthetic reconcile request.

```bash
sed -n '240,336p' cmd/main.go
```

```output
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "71f32f9d.fortsa.scaffidi.net",
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
	reconciler := controller.NewIstioChangeReconciler(controller.ReconcilerOptions{
		Client:                mgr.GetClient(),
		Scheme:                mgr.GetScheme(),
		DryRun:                dryRun,
		CompareHub:            compareHub,
		RestartDelay:          restartDelay,
		IstiodConfigReadDelay: istiodConfigReadDelay,
		AnnotationCooldown:    annotationCooldown,
		SkipNamespaces:        parseSkipNamespaces(skipNamespaces),
		WebhookCaller:         webhookClient,
	})

	fortsaController := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(predicate.NewPredicateFuncs(controller.ConfigMapFilter()))).
		Watches(
			&admissionregv1.MutatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{mwc.ReconcileRequest()}
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(mwc.Filter())),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{namespace.ReconcileRequest(obj.GetName())}
			}),
			builder.WithPredicates(namespace.Filter()),
		)
	if reconcilePeriod > 0 {
		fortsaController = fortsaController.WatchesRawSource(periodic.NewReconcileSource(reconcilePeriod))
	}
	err = fortsaController.Complete(reconciler)
	if err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
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
```

## 2) Watches and reconcile requests (internal/mwc, internal/namespace, internal/periodic)

The controller watches three kinds of inputs:

- ConfigMaps: `istio-sidecar-injector*` in `istio-system` (the primary signal)
- MutatingWebhookConfigurations: `istio-revision-tag-*` (updates tag to revision mapping)
- Namespaces: changes to `istio.io/rev` or `istio-injection` labels (namespace-scoped scan)

Two of those watches do not reconcile the watched object directly. Instead, they enqueue a synthetic reconcile request that the reconciler interprets by name/shape:

- MWC watch enqueues a request with name `__mwc_reconcile__`
- Periodic source enqueues a request with name `__periodic_reconcile__`

Namespace watch uses a request with a name but no namespace to encode: "scan only this namespace".

```bash
sed -n '1,130p' internal/mwc/trigger.go
```

```output
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mwc

import (
	"context"
	"strings"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	istioRevisionTagPrefix  = "istio-revision-tag-"
	istioSystemNamespace    = "istio-system"
	mwcReconcileTriggerName = "__mwc_reconcile__"
)

// ReconcileRequest returns a reconcile.Request that triggers a tag-mapping reconciliation
// when istio-revision-tag-* MutatingWebhookConfigurations change. Used by the MWC watch.
func ReconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: istioSystemNamespace, Name: mwcReconcileTriggerName},
	}
}

// ReconcileRequestName returns the request name used for MWC reconcile triggers.
// Used by the controller to identify MWC-triggered requests.
func ReconcileRequestName() string {
	return mwcReconcileTriggerName
}

// Filter returns a predicate function that filters MutatingWebhookConfigurations
// to only those named istio-revision-tag-* (tag-to-revision mapping).
func Filter() func(client.Object) bool {
	return func(obj client.Object) bool {
		return strings.HasPrefix(obj.GetName(), istioRevisionTagPrefix)
	}
}

// getLastModified returns the latest modification time of the MutatingWebhookConfiguration.
func getLastModified(mwc *admissionregv1.MutatingWebhookConfiguration) time.Time {
	latest := mwc.CreationTimestamp.Time
	for _, mf := range mwc.ManagedFields {
		if mf.Time != nil && mf.Time.After(latest) {
			latest = mf.Time.Time
		}
	}
	return latest
}

// FetchTagToRevision lists istio-revision-tag-* MutatingWebhookConfigurations and builds
// a tag-to-revision map from istio.io/tag and istio.io/rev labels.
func FetchTagToRevision(ctx context.Context, c client.Client) (map[string]string, error) {
	tagToRevision, _, err := FetchTagToRevisionAndLastModified(ctx, c)
	return tagToRevision, err
}

// FetchTagToRevisionAndLastModified lists istio-revision-tag-* MutatingWebhookConfigurations,
// builds tag-to-revision map and tag-to-lastModified map (for pod skip logic).
func FetchTagToRevisionAndLastModified(ctx context.Context, c client.Client) (map[string]string, map[string]time.Time, error) {
	var mwcList admissionregv1.MutatingWebhookConfigurationList
	if err := c.List(ctx, &mwcList); err != nil {
		return nil, nil, err
	}
	tagToRevision := make(map[string]string)
	lastModifiedByTag := make(map[string]time.Time)
	for i := range mwcList.Items {
		mwc := &mwcList.Items[i]
		if !strings.HasPrefix(mwc.Name, istioRevisionTagPrefix) {
			continue
		}
		tag := mwc.Labels["istio.io/tag"]
		revision := mwc.Labels["istio.io/rev"]
		if tag != "" && revision != "" {
			tagToRevision[tag] = revision
			lastModifiedByTag[tag] = getLastModified(mwc)
		}
	}
	return tagToRevision, lastModifiedByTag, nil
}
```

```bash
sed -n '1,120p' internal/namespace/trigger.go
```

```output
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package namespace

import (
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// hasIstioLabels returns true if the object has istio.io/rev or istio-injection in its labels.
func hasIstioLabels(obj client.Object) bool {
	labels := obj.GetLabels()
	if _, ok := labels["istio.io/rev"]; ok {
		return true
	}
	if v, ok := labels["istio-injection"]; ok && v != "" {
		return true
	}
	return false
}

// Filter returns a predicate that filters Namespace events to only those
// with Istio-related labels (istio.io/rev or istio-injection). For updates, triggers
// when either old or new has the labels to catch add, remove, or change.
func Filter() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return hasIstioLabels(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasIstioLabels(e.ObjectOld) || hasIstioLabels(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return hasIstioLabels(e.Object)
		},
	}
}

// ReconcileRequest returns a reconcile.Request for a namespace-scoped reconciliation.
// Used when a Namespace's Istio labels change.
func ReconcileRequest(namespace string) ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Name: namespace},
	}
}
```

```bash
sed -n '1,120p' internal/periodic/trigger.go
```

```output
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package periodic

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	istioSystemNamespace = "istio-system"
	reconcileTriggerName = "__periodic_reconcile__"
)

// ReconcileRequest returns a reconcile.Request that triggers a full periodic reconciliation
// of all istio-sidecar-injector ConfigMaps. Used by the periodic ticker source.
func ReconcileRequest() reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: istioSystemNamespace, Name: reconcileTriggerName},
	}
}

// ReconcileRequestName returns the request name used for periodic reconcile triggers.
// Used by the controller to identify periodic-triggered requests.
func ReconcileRequestName() string {
	return reconcileTriggerName
}

// NewReconcileSource returns a source that enqueues a full-reconcile request at the given period.
// When period is 0, returns nil (caller should not add the source).
func NewReconcileSource(period time.Duration) source.Source {
	return source.Func(func(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		go func() {
			ticker := time.NewTicker(period)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					queue.Add(ReconcileRequest())
				}
			}
		}()
		return nil
	})
}
```

## 3) The main reconciler: routing and convergence (internal/controller)

`IstioChangeReconciler` is the single reconciler implementation used by the controller.

The key idea: different watches enqueue different shapes of `ctrl.Request`, and the reconciler routes them by inspecting the request:

- `__periodic_reconcile__` name triggers a full refresh of the ConfigMap cache and then a scan.
- `__mwc_reconcile__` name triggers a tag mapping refresh and then a scan.
- Namespace watch uses `req.Namespace == "" && req.Name != ""` to mean: scan only that namespace.
- ConfigMap reconcile is the normal path: parse values, compute last-modified, check cache, then scan.

All paths converge into the same "fetch tag mapping -> scan pods -> annotate workloads" pipeline.

```bash
sed -n '40,175p' internal/controller/istio_change_reconciler.go
```

```output
const (
	istioSystemNamespace = "istio-system"
	configMapNamePrefix  = "istio-sidecar-injector"
)

// waitOrContextDone waits for d or until ctx is cancelled.
// Returns ctx.Err() if cancelled, nil if the duration elapsed.
func waitOrContextDone(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// awaitIstiodConfigReadDelay waits for Istiod to read updated config before scanning.
// No-op if istiodConfigReadDelay is 0.
func (r *IstioChangeReconciler) awaitIstiodConfigReadDelay(ctx context.Context) error {
	if r.istiodConfigReadDelay <= 0 {
		return nil
	}
	return waitOrContextDone(ctx, r.istiodConfigReadDelay)
}

// ReconcilerOptions holds configuration for NewIstioChangeReconciler.
type ReconcilerOptions struct {
	Client                client.Client
	Scheme                *runtime.Scheme
	DryRun                bool
	CompareHub            bool
	RestartDelay          time.Duration
	IstiodConfigReadDelay time.Duration
	AnnotationCooldown    time.Duration
	SkipNamespaces        []string
	WebhookCaller         webhook.WebhookCaller
}

// IstioChangeReconciler reconciles Istio sidecar injector ConfigMaps and related changes
// (MWC tag mapping, namespace labels).
type IstioChangeReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	scanner               *podscanner.PodScanner
	annotator             annotator.WorkloadAnnotator
	dryRun                bool
	compareHub            bool
	restartDelay          time.Duration
	istiodConfigReadDelay time.Duration
	skipNamespaces        []string

	revisionCache *cache.RevisionCache
}

// NewIstioChangeReconciler creates a new IstioChangeReconciler from the given options.
func NewIstioChangeReconciler(opts ReconcilerOptions) *IstioChangeReconciler {
	return &IstioChangeReconciler{
		Client:                opts.Client,
		Scheme:                opts.Scheme,
		scanner:               podscanner.NewPodScanner(opts.Client, opts.WebhookCaller),
		annotator:             annotator.NewWorkloadAnnotator(opts.Client, opts.AnnotationCooldown),
		dryRun:                opts.DryRun,
		compareHub:            opts.CompareHub,
		restartDelay:          opts.RestartDelay,
		istiodConfigReadDelay: opts.IstiodConfigReadDelay,
		skipNamespaces:        opts.SkipNamespaces,
		revisionCache:         cache.NewRevisionCache(),
	}
}

// Reconcile implements the reconcile loop for ConfigMap events.
func (r *IstioChangeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Periodic reconciliation: full reconcile of all ConfigMaps (ticker-triggered only)
	if req.Name == periodic.ReconcileRequestName() {
		return r.reconcileAll(ctx)
	}

	// MWC tag mapping change: re-fetch tag-to-revision and scan (no ConfigMap cache refresh)
	if req.Name == mwc.ReconcileRequestName() {
		return r.reconcileMWCChange(ctx)
	}

	// Namespace label change: scan only pods in that namespace
	if req.Namespace == "" && req.Name != "" {
		return r.reconcileNamespace(ctx, req.Name)
	}

	// Only reconcile ConfigMaps in istio-system
	if req.Namespace != istioSystemNamespace {
		return ctrl.Result{}, nil
	}

	// Only reconcile ConfigMaps named or prefixed with istio-sidecar-injector
	if !strings.HasPrefix(req.Name, configMapNamePrefix) {
		return ctrl.Result{}, nil
	}

	log.FromContext(ctx).Info("reconciling ConfigMap", "namespace", req.Namespace, "name", req.Name)

	var cm corev1.ConfigMap
	if err := r.Get(ctx, req.NamespacedName, &cm); err != nil {
		if errors.IsNotFound(err) {
			r.revisionCache.ClearByConfigMap(req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	vals, err := configmap.ParseConfigMapValues(&cm)
	if err != nil {
		logger.Error(err, "failed to parse ConfigMap values")
		return ctrl.Result{}, err
	}
	lastModified := configmap.GetConfigMapLastModified(&cm)
	revision := vals.Revision

	if !r.revisionCache.LastModifiedChanged(revision, lastModified) {
		return ctrl.Result{}, nil
	}
	r.revisionCache.Set(req.String(), revision, lastModified)

	// Wait for Istiod to read the updated ConfigMap before scanning (webhook uses Istiod's config)
	// TODO: This is a hack... We should find a better mechanism.
	if err := r.awaitIstiodConfigReadDelay(ctx); err != nil {
		return ctrl.Result{}, err
	}

	return r.fetchTagMappingAndScan(ctx, nil)
}

// reconcileNamespace performs a namespace-scoped reconciliation when Istio labels change on a namespace.
func (r *IstioChangeReconciler) reconcileNamespace(ctx context.Context, namespace string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
```

The reconcile methods all flow into `fetchTagMappingAndScan()` and then into `scanAndAnnotate()`. Note the separation of concerns:

- controller layer: orchestration + caching + optional delays
- podscanner: detection (read-heavy)
- annotator: actuation (write-heavy)

```bash
sed -n '183,305p' internal/controller/istio_change_reconciler.go
```

```output
// reconcileMWCChange re-fetches the tag-to-revision mapping and scans. Used when MWCs change.
// Does not refresh the ConfigMap cache (ConfigMaps are unchanged).
func (r *IstioChangeReconciler) reconcileMWCChange(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("MWC tag mapping changed, reconciling")
	if err := r.awaitIstiodConfigReadDelay(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return r.fetchTagMappingAndScan(ctx, nil)
}

// reconcileAll performs a full reconciliation of all istio-sidecar-injector ConfigMaps,
// bypassing change detection. Used for periodic reconciliation only.
func (r *IstioChangeReconciler) reconcileAll(ctx context.Context) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("periodic reconciliation of all istio-sidecar-injector ConfigMaps")

	var cmList corev1.ConfigMapList
	if err := r.List(ctx, &cmList, client.InNamespace(istioSystemNamespace)); err != nil {
		logger.Error(err, "failed to list ConfigMaps")
		return ctrl.Result{}, err
	}

	// Update cache from all matching ConfigMaps
	for i := range cmList.Items {
		cm := &cmList.Items[i]
		if !strings.HasPrefix(cm.Name, configMapNamePrefix) {
			continue
		}
		vals, err := configmap.ParseConfigMapValues(cm)
		if err != nil {
			logger.Error(err, "failed to parse ConfigMap values", "configmap", cm.Name)
			continue
		}
		r.revisionCache.Set(client.ObjectKeyFromObject(cm).String(), vals.Revision, configmap.GetConfigMapLastModified(cm))
	}

	if err := r.awaitIstiodConfigReadDelay(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return r.fetchTagMappingAndScan(ctx, nil)
}

// fetchTagMappingAndScan fetches tag-to-revision and lastModifiedByTag from MWCs, then scans and annotates.
// limitToNamespaces, when non-empty, restricts scanning to those namespaces only (e.g. for namespace label changes).
func (r *IstioChangeReconciler) fetchTagMappingAndScan(ctx context.Context, limitToNamespaces []string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	tagToRevision, lastModifiedByTag, err := mwc.FetchTagToRevisionAndLastModified(ctx, r.Client)
	if err != nil {
		logger.Error(err, "failed to fetch tag-to-revision mapping")
		return ctrl.Result{}, err
	}
	return r.scanAndAnnotate(ctx, tagToRevision, lastModifiedByTag, limitToNamespaces)
}

// scanAndAnnotate scans for outdated pods using the current cache and annotates workloads to trigger restarts.
// limitToNamespaces, when non-empty, restricts scanning to those namespaces only.
func (r *IstioChangeReconciler) scanAndAnnotate(ctx context.Context, tagToRevision map[string]string, lastModifiedByTag map[string]time.Time, limitToNamespaces []string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if tagToRevision == nil {
		tagToRevision = map[string]string{}
	}
	lastModifiedByRevision := r.revisionCache.GetCopy()
	opts := podscanner.ScanOptions{
		CompareHub:            r.compareHub,
		IstiodConfigReadDelay: r.istiodConfigReadDelay,
		SkipNamespaces:        r.skipNamespaces,
		LimitToNamespaces:     limitToNamespaces,
	}
	workloads, err := r.scanner.ScanOutdatedPods(ctx, lastModifiedByRevision, tagToRevision, lastModifiedByTag, opts)
	if err != nil {
		logger.Error(err, "failed to scan pods")
		return ctrl.Result{}, err
	}
	r.annotateWorkloadsWithDelay(ctx, workloads)
	return ctrl.Result{}, nil
}

// annotateWorkloadsWithDelay annotates each workload, with an optional delay between each.
// Exported for testing.
func (r *IstioChangeReconciler) annotateWorkloadsWithDelay(ctx context.Context, workloads []podscanner.WorkloadRef) {
	logger := log.FromContext(ctx)
	for i, ref := range workloads {
		if i > 0 && r.restartDelay > 0 {
			if err := waitOrContextDone(ctx, r.restartDelay); err != nil {
				return
			}
		}
		if r.dryRun {
			logger.Info("[dry-run] would annotate workload for restart",
				"workload", ref.NamespacedName,
				"kind", ref.Kind,
				"annotation", annotator.RestartedAtAnnotation)
			continue
		}
		annotated, err := r.annotator.Annotate(ctx, ref)
		if err != nil {
			logger.Error(err, "failed to annotate workload", "workload", ref.NamespacedName, "kind", ref.Kind)
			continue
		}
		if annotated {
			logger.Info("annotated workload for restart", "workload", ref.NamespacedName, "kind", ref.Kind)
		} else {
			logger.Info("skipped annotating workload for restart", "workload", ref.NamespacedName, "kind", ref.Kind)
		}
	}
}

// ConfigMapFilter returns a predicate that filters ConfigMaps to only those
// in istio-system with name equal to or prefixed with istio-sidecar-injector.
func ConfigMapFilter() func(client.Object) bool {
	return func(obj client.Object) bool {
		if obj.GetNamespace() != istioSystemNamespace {
			return false
		}
		name := obj.GetName()
		return strings.HasPrefix(name, configMapNamePrefix)
	}
}
```

## 4) ConfigMap parsing and last-modified computation (internal/configmap)

Fortsa watches Istio sidecar injector ConfigMaps (names starting with `istio-sidecar-injector`). The reconciler parses the ConfigMap's `data["values"]` JSON to extract:

- revision (from `revision`, `global.revision`, or ConfigMap label `istio.io/rev`; defaulting to `default`)
- hub, tag, image (from `global.hub`, `global.tag`, `global.proxy.image`)

It also computes a `LastModified` timestamp used to skip pods that were created after the config update (with an additional "istiod config read" delay window).

```bash
sed -n '1,140p' internal/configmap/configmap_parser.go
```

```output
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package configmap

import (
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// IstioValues represents the extracted values from the Istio sidecar injector ConfigMap.
type IstioValues struct {
	Revision     string
	Hub          string
	Tag          string
	Image        string    // global.proxy.image, e.g. "proxyv2"
	LastModified time.Time // when the ConfigMap was last updated; used to skip pods created after this time
}

// GetConfigMapLastModified returns the latest modification time of the ConfigMap.
// Uses metadata.managedFields timestamps when available; falls back to creationTimestamp.
func GetConfigMapLastModified(cm *corev1.ConfigMap) time.Time {
	latest := cm.CreationTimestamp.Time
	for _, mf := range cm.ManagedFields {
		if mf.Time != nil && mf.Time.After(latest) {
			latest = mf.Time.Time
		}
	}
	return latest
}

// ParseConfigMapValues extracts hub, tag, global.proxy.image, and revision from the ConfigMap's values JSON.
// Revision is taken from the values JSON (top-level or global) or the ConfigMap label istio.io/rev.
// Returns an error if the values key is missing or the JSON cannot be parsed.
func ParseConfigMapValues(cm *corev1.ConfigMap) (*IstioValues, error) {
	raw, ok := cm.Data["values"]
	if !ok || raw == "" {
		return nil, fmt.Errorf("ConfigMap %s/%s has no 'values' key", cm.Namespace, cm.Name)
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("failed to parse values JSON: %w", err)
	}

	vals := &IstioValues{}

	// Revision: prefer values JSON, then ConfigMap label istio.io/rev
	if v, ok := data["revision"].(string); ok && v != "" {
		vals.Revision = v
	}
	if vals.Revision == "" {
		if global, ok := data["global"].(map[string]interface{}); ok {
			if v, ok := global["revision"].(string); ok && v != "" {
				vals.Revision = v
			}
		}
	}
	if vals.Revision == "" && cm.Labels != nil {
		if v, ok := cm.Labels["istio.io/rev"]; ok && v != "" {
			vals.Revision = v
		}
	}
	if vals.Revision == "" {
		vals.Revision = "default"
	}

	if global, ok := data["global"].(map[string]interface{}); ok {
		if v, ok := global["hub"].(string); ok {
			vals.Hub = v
		}
		if v, ok := global["tag"].(string); ok {
			vals.Tag = v
		}
		if proxy, ok := global["proxy"].(map[string]interface{}); ok {
			if v, ok := proxy["image"].(string); ok {
				vals.Image = v
			}
		}
	}

	return vals, nil
}
```

## 5) Change detection state: RevisionCache (internal/cache)

The controller needs a lightweight memory of "what revision last changed when".

`RevisionCache` stores:

- `revisionToLastModified`: used to detect ConfigMap changes and to feed skip logic in pod scanning
- `nameToRevision`: used to clean up cache when a watched ConfigMap is deleted

This cache is process-local and protected by an RWMutex because reconciles can run concurrently.

```bash
sed -n '1,140p' internal/cache/revision_cache.go
```

```output
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cache

import (
	"sync"
	"time"
)

// RevisionCache stores ConfigMap revision-to-LastModified mappings for change detection
// and pod skip logic. It also tracks configMapKey-to-revision for delete cleanup.
type RevisionCache struct {
	revisionToLastModified map[string]time.Time
	nameToRevision         map[string]string
	mu                     sync.RWMutex
}

// NewRevisionCache creates a new RevisionCache.
func NewRevisionCache() *RevisionCache {
	return &RevisionCache{
		revisionToLastModified: make(map[string]time.Time),
		nameToRevision:         make(map[string]string),
	}
}

// LastModifiedChanged returns true if the revision's lastModified differs from the cached value.
// Returns true when the revision is not yet cached (first time seen).
func (c *RevisionCache) LastModifiedChanged(revision string, lastModified time.Time) bool {
	c.mu.RLock()
	prev := c.revisionToLastModified[revision]
	c.mu.RUnlock()

	if prev.IsZero() {
		return true
	}
	return !prev.Equal(lastModified)
}

// Set stores the configMapKey-to-revision and revision-to-lastModified mappings.
func (c *RevisionCache) Set(configMapKey, revision string, lastModified time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revisionToLastModified[revision] = lastModified
	c.nameToRevision[configMapKey] = revision
}

// GetCopy returns a shallow copy of the revision-to-LastModified map for safe concurrent use.
func (c *RevisionCache) GetCopy() map[string]time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	copy := make(map[string]time.Time, len(c.revisionToLastModified))
	for k, v := range c.revisionToLastModified {
		copy[k] = v
	}
	return copy
}

// ClearByConfigMap removes cache entries for the given configMapKey.
func (c *RevisionCache) ClearByConfigMap(configMapKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if revision, ok := c.nameToRevision[configMapKey]; ok {
		delete(c.revisionToLastModified, revision)
		delete(c.nameToRevision, configMapKey)
	}
}
```

## 6) Pod scanning: detect workloads with outdated sidecars (internal/podscanner)

The pod scanner is the read-heavy engine. It answers: "Which restartable workloads currently have at least one pod whose Istio sidecar image does not match the image that would be injected right now?"

Core ideas:

- It lists pods (optionally limited to specific namespaces).
- It skips namespaces configured by flag.
- For each pod, it finds the owning workload by traversing ownerReferences.
- It determines the Istio revision or tag from workload template labels, then namespace labels.
- It uses last-modified timestamps (ConfigMap by revision, and MWC by tag) plus a configurable delay window to skip pods that were definitely created after the relevant config change.
- It builds a pod from the workload template and calls the Istio injection webhook to compute the expected mutated pod.
- It compares the `istio-proxy` image in the live pod vs the expected mutated pod.
- It deduplicates results to a list of unique workloads (Deployment/StatefulSet/DaemonSet).

Start with image parsing/comparison helpers.

```bash
sed -n '37,110p' internal/podscanner/pod_scanner.go
```

```output
const (
	istioProxyContainerName = "istio-proxy"
)

// ParsedImage holds the components of a container image reference.
type ParsedImage struct {
	Registry  string // path before last /
	ImageName string // last path segment before : or @
	Tag       string // after :, or "latest" if absent
}

// ParseImage splits a container image string into registry, image_name, and tag.
// Handles: docker.io/istio/proxyv2:1.20.1, registry:5000/repo/image:tag, image@sha256:...
func ParseImage(image string) ParsedImage {
	p := ParsedImage{Tag: "latest"}

	// Handle digest: image@sha256:abc123 -> tag is the digest part for comparison
	atIdx := strings.Index(image, "@")
	if atIdx >= 0 {
		image = image[:atIdx]
	}

	colonIdx := strings.LastIndex(image, ":")
	if colonIdx >= 0 {
		p.Tag = image[colonIdx+1:]
		image = image[:colonIdx]
	}

	lastSlash := strings.LastIndex(image, "/")
	if lastSlash >= 0 {
		p.Registry = image[:lastSlash]
		p.ImageName = image[lastSlash+1:]
	} else {
		p.ImageName = image
	}

	return p
}

// Matches returns true if the parsed container image matches the expected Istio values.
// When compareHub is true, registry must match hub; when false, only image name and tag are compared.
func (p ParsedImage) Matches(vals *configmap.IstioValues, compareHub bool) bool {
	if compareHub && p.Registry != vals.Hub {
		return false
	}
	return p.ImageName == vals.Image && p.Tag == vals.Tag
}

// imagesMatch returns true if current and expected images match.
// When compareHub is true, full string comparison (including registry) is used.
// When compareHub is false, only image name and tag are compared (registry is ignored).
func imagesMatch(current, expected string, compareHub bool) bool {
	if compareHub {
		return current == expected
	}
	cp := ParseImage(current)
	ep := ParseImage(expected)
	return cp.ImageName == ep.ImageName && cp.Tag == ep.Tag
}

// PodScanner lists Pods with Istio sidecars and finds their parent workloads.
type PodScanner struct {
	client        client.Client
	webhookCaller webhook.WebhookCaller
}

// NewPodScanner creates a new PodScanner.
func NewPodScanner(c client.Client, webhookCaller webhook.WebhookCaller) *PodScanner {
	return &PodScanner{client: c, webhookCaller: webhookCaller}
}

// WorkloadRef identifies a Deployment, StatefulSet, or DaemonSet.
type WorkloadRef struct {
	types.NamespacedName
```

Next are scan options, pod listing behavior, and the key skip function that uses last-modified plus the istiod config-read delay window. This is one of the main safety levers to avoid scanning (and restarting) pods that are already up to date.

```bash
sed -n '114,205p' internal/podscanner/pod_scanner.go
```

```output
// ScanOptions configures pod scanning behavior.
type ScanOptions struct {
	// CompareHub, when true, requires registry to match when comparing images.
	// When false, only image name and tag are compared (registry is ignored).
	CompareHub bool
	// IstiodConfigReadDelay is added to ConfigMap LastModified when deciding whether to skip pods.
	// Pods created within this window after a ConfigMap update may have been injected before
	// Istiod loaded the new config, so we still scan them.
	IstiodConfigReadDelay time.Duration
	// SkipNamespaces lists namespaces to skip when scanning pods.
	SkipNamespaces []string
	// LimitToNamespaces, when non-empty, restricts scanning to pods in these namespaces only.
	LimitToNamespaces []string
}

// listPods lists pods according to LimitToNamespaces: single ns, multiple nss, or all.
func listPods(ctx context.Context, c client.Client, opts ScanOptions) (corev1.PodList, error) {
	var podList corev1.PodList
	if len(opts.LimitToNamespaces) == 1 {
		if err := c.List(ctx, &podList, client.InNamespace(opts.LimitToNamespaces[0])); err != nil {
			return podList, err
		}
	} else if len(opts.LimitToNamespaces) > 1 {
		limitSet := make(map[string]struct{})
		for _, ns := range opts.LimitToNamespaces {
			if ns != "" {
				limitSet[ns] = struct{}{}
			}
		}
		for ns := range limitSet {
			var nsList corev1.PodList
			if err := c.List(ctx, &nsList, client.InNamespace(ns)); err != nil {
				return podList, err
			}
			podList.Items = append(podList.Items, nsList.Items...)
		}
	} else {
		if err := c.List(ctx, &podList); err != nil {
			return podList, err
		}
	}
	return podList, nil
}

// shouldSkipPodForConfigMap returns true if the pod was created after the ConfigMap was last
// updated (and thus was injected with current config). When using a tag, also considers the tag's
// MWC LastModified: if the tag was modified after the pod was created, we must scan.
func shouldSkipPodForConfigMap(pod *corev1.Pod, revision, revOrTag string, lastModifiedByRevision, lastModifiedByTag map[string]time.Time, opts ScanOptions) bool {
	skip := false
	if lastModified, ok := lastModifiedByRevision[revision]; ok && !lastModified.IsZero() {
		effectiveLastModified := lastModified.Add(opts.IstiodConfigReadDelay)
		if !pod.CreationTimestamp.Time.Before(effectiveLastModified) {
			skip = true
		}
	}
	if skip && revOrTag != revision {
		// Using a tag - override skip if tag's MWC was modified after pod was created
		if tagLastModified, ok := lastModifiedByTag[revOrTag]; ok && !tagLastModified.IsZero() {
			tagEffective := tagLastModified.Add(opts.IstiodConfigReadDelay)
			if pod.CreationTimestamp.Time.Before(tagEffective) {
				skip = false // Tag was modified after pod - must scan
			}
		} else {
			skip = false // No tag lastModified info - be conservative, scan
		}
	}
	return skip
}

// processPodForOutdatedSidecar checks a single pod for an outdated Istio sidecar. Returns the
// WorkloadRef when the pod has an outdated sidecar and should be added to results; nil when skipped.
func (s *PodScanner) processPodForOutdatedSidecar(ctx context.Context, pod *corev1.Pod, tagToRevision map[string]string, lastModifiedByRevision, lastModifiedByTag map[string]time.Time, opts ScanOptions) *WorkloadRef {
	logger := log.FromContext(ctx)

	ref, err := s.findWorkloadOwner(ctx, pod)
	if err != nil {
		logger.Error(err, "failed to find workload owner", "pod", pod.Namespace+"/"+pod.Name)
		return nil
	}
	if ref == nil {
		logger.Info("no restartableworkload owner found", "pod", pod.Namespace+"/"+pod.Name)
		return nil
	}

	revOrTag, err := s.getIstioRevFromWorkloadOrNamespace(ctx, ref)
	if err != nil {
		logger.Error(err, "failed to get istio.io/rev from workload or namespace", "workload", ref.NamespacedName)
		return nil
	}
	if revOrTag == "" {
		logger.V(1).Info("namespace has no istio.io/rev or istio-injection=enabled, skipping", "workload", ref.NamespacedName)
		return nil
```

```bash
sed -n '158,255p' internal/podscanner/pod_scanner.go
```

```output
// shouldSkipPodForConfigMap returns true if the pod was created after the ConfigMap was last
// updated (and thus was injected with current config). When using a tag, also considers the tag's
// MWC LastModified: if the tag was modified after the pod was created, we must scan.
func shouldSkipPodForConfigMap(pod *corev1.Pod, revision, revOrTag string, lastModifiedByRevision, lastModifiedByTag map[string]time.Time, opts ScanOptions) bool {
	skip := false
	if lastModified, ok := lastModifiedByRevision[revision]; ok && !lastModified.IsZero() {
		effectiveLastModified := lastModified.Add(opts.IstiodConfigReadDelay)
		if !pod.CreationTimestamp.Time.Before(effectiveLastModified) {
			skip = true
		}
	}
	if skip && revOrTag != revision {
		// Using a tag - override skip if tag's MWC was modified after pod was created
		if tagLastModified, ok := lastModifiedByTag[revOrTag]; ok && !tagLastModified.IsZero() {
			tagEffective := tagLastModified.Add(opts.IstiodConfigReadDelay)
			if pod.CreationTimestamp.Time.Before(tagEffective) {
				skip = false // Tag was modified after pod - must scan
			}
		} else {
			skip = false // No tag lastModified info - be conservative, scan
		}
	}
	return skip
}

// processPodForOutdatedSidecar checks a single pod for an outdated Istio sidecar. Returns the
// WorkloadRef when the pod has an outdated sidecar and should be added to results; nil when skipped.
func (s *PodScanner) processPodForOutdatedSidecar(ctx context.Context, pod *corev1.Pod, tagToRevision map[string]string, lastModifiedByRevision, lastModifiedByTag map[string]time.Time, opts ScanOptions) *WorkloadRef {
	logger := log.FromContext(ctx)

	ref, err := s.findWorkloadOwner(ctx, pod)
	if err != nil {
		logger.Error(err, "failed to find workload owner", "pod", pod.Namespace+"/"+pod.Name)
		return nil
	}
	if ref == nil {
		logger.Info("no restartableworkload owner found", "pod", pod.Namespace+"/"+pod.Name)
		return nil
	}

	revOrTag, err := s.getIstioRevFromWorkloadOrNamespace(ctx, ref)
	if err != nil {
		logger.Error(err, "failed to get istio.io/rev from workload or namespace", "workload", ref.NamespacedName)
		return nil
	}
	if revOrTag == "" {
		logger.V(1).Info("namespace has no istio.io/rev or istio-injection=enabled, skipping", "workload", ref.NamespacedName)
		return nil
	}
	revision := revOrTag
	if r, ok := tagToRevision[revOrTag]; ok {
		revision = r
	}

	if shouldSkipPodForConfigMap(pod, revision, revOrTag, lastModifiedByRevision, lastModifiedByTag, opts) {
		return nil
	}

	logger.Info("found workload", "workload", ref.Namespace+"/"+ref.Name)

	templatePod, err := s.buildPodFromWorkload(ctx, ref)
	if err != nil {
		logger.Error(err, "failed to build pod from workload", "workload", ref.NamespacedName)
		return nil
	}

	//nolint:goconst // "default" is the Istio default revision name
	mutated, err := s.webhookCaller.CallWebhook(ctx, templatePod, revision, revOrTag == "default")
	if err != nil {
		logger.Error(err, "webhook call failed", "pod", pod.Namespace+"/"+pod.Name)
		return nil
	}

	expectedImage := getIstioProxyImage(mutated)
	currentImage := getIstioProxyImage(pod)
	logger.V(1).Info("expected image", "expected", expectedImage, "current", currentImage, "pod", pod.Namespace+"/"+pod.Name)
	if expectedImage == "" {
		return nil
	}
	if imagesMatch(currentImage, expectedImage, opts.CompareHub) {
		return nil
	}

	logger.Info("outdated image",
		"pod", pod.Namespace+"/"+pod.Name,
		"revision", revision,
		"current", currentImage,
		"expected", expectedImage)

	return ref
}

// ScanOutdatedPods lists all Pods, finds each pod's controller (Deployment/StatefulSet/DaemonSet),
// builds a pod from the workload template, submits it to the Istio injection webhook, and compares
// the mutated response's istio-proxy image with the current pod. Returns WorkloadRefs for pods
// with outdated sidecars. lastModifiedByRevision is used for the ConfigMap LastModified skip.
// lastModifiedByTag is used for the tag MWC LastModified skip when workload uses a tag.
// tagToRevision maps istio revision tags to revisions (from istio-revision-tag-* MutatingWebhookConfigurations).
```

The heart of scanning is `ScanOutdatedPods()`: list pods, skip configured namespaces, process each pod, and collect unique workloads.

```bash
sed -n '250,315p' internal/podscanner/pod_scanner.go
```

```output
// ScanOutdatedPods lists all Pods, finds each pod's controller (Deployment/StatefulSet/DaemonSet),
// builds a pod from the workload template, submits it to the Istio injection webhook, and compares
// the mutated response's istio-proxy image with the current pod. Returns WorkloadRefs for pods
// with outdated sidecars. lastModifiedByRevision is used for the ConfigMap LastModified skip.
// lastModifiedByTag is used for the tag MWC LastModified skip when workload uses a tag.
// tagToRevision maps istio revision tags to revisions (from istio-revision-tag-* MutatingWebhookConfigurations).
func (s *PodScanner) ScanOutdatedPods(ctx context.Context, lastModifiedByRevision map[string]time.Time, tagToRevision map[string]string, lastModifiedByTag map[string]time.Time, opts ScanOptions) ([]WorkloadRef, error) {
	logger := log.FromContext(ctx)

	if s.webhookCaller == nil {
		return nil, nil
	}
	if tagToRevision == nil {
		tagToRevision = map[string]string{}
	}

	podList, err := listPods(ctx, s.client, opts)
	if err != nil {
		return nil, err
	}

	seen := make(map[types.NamespacedName]struct{})
	var workloads []WorkloadRef //nolint:prealloc

	skipSet := make(map[string]struct{})
	for _, ns := range opts.SkipNamespaces {
		if ns != "" {
			skipSet[ns] = struct{}{}
		}
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if _, skip := skipSet[pod.Namespace]; skip {
			continue
		}

		logger.Info("scanning pod", "pod", pod.Namespace+"/"+pod.Name)

		ref := s.processPodForOutdatedSidecar(ctx, pod, tagToRevision, lastModifiedByRevision, lastModifiedByTag, opts)
		if ref == nil {
			continue
		}
		if _, ok := seen[ref.NamespacedName]; ok {
			continue
		}
		seen[ref.NamespacedName] = struct{}{}
		workloads = append(workloads, *ref)
	}

	return workloads, nil
}

// getIstioRevFromWorkloadOrNamespace returns istio.io/rev from the workload's pod template;
// if missing, from the namespace (istio.io/rev or istio-injection=enabled). Returns "default"
// when namespace has istio-injection=enabled. Returns "" when neither workload nor namespace
// has istio.io/rev and namespace lacks istio-injection=enabled (caller should skip the pod).
func (s *PodScanner) getIstioRevFromWorkloadOrNamespace(ctx context.Context, ref *WorkloadRef) (string, error) {
	switch ref.Kind {
	case "Deployment": //nolint:goconst
		var dep appsv1.Deployment
		if err := s.client.Get(ctx, ref.NamespacedName, &dep); err != nil {
			return "", err
		}
		if v, ok := dep.Spec.Template.Labels["istio.io/rev"]; ok && v != "" {
			return v, nil
```

Revision selection is resolved by looking for `istio.io/rev` on the workload template first, then falling back to namespace labels. A namespace with `istio-injection=enabled` implies the `default` revision.

```bash
sed -n '303,352p' internal/podscanner/pod_scanner.go
```

```output
// getIstioRevFromWorkloadOrNamespace returns istio.io/rev from the workload's pod template;
// if missing, from the namespace (istio.io/rev or istio-injection=enabled). Returns "default"
// when namespace has istio-injection=enabled. Returns "" when neither workload nor namespace
// has istio.io/rev and namespace lacks istio-injection=enabled (caller should skip the pod).
func (s *PodScanner) getIstioRevFromWorkloadOrNamespace(ctx context.Context, ref *WorkloadRef) (string, error) {
	switch ref.Kind {
	case "Deployment": //nolint:goconst
		var dep appsv1.Deployment
		if err := s.client.Get(ctx, ref.NamespacedName, &dep); err != nil {
			return "", err
		}
		if v, ok := dep.Spec.Template.Labels["istio.io/rev"]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	case "StatefulSet": //nolint:goconst
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, ref.NamespacedName, &sts); err != nil {
			return "", err
		}
		if v, ok := sts.Spec.Template.Labels["istio.io/rev"]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	case "DaemonSet": //nolint:goconst
		var ds appsv1.DaemonSet
		if err := s.client.Get(ctx, ref.NamespacedName, &ds); err != nil {
			return "", err
		}
		if v, ok := ds.Spec.Template.Labels["istio.io/rev"]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	default:
		return "default", nil
	}

	var ns corev1.Namespace
	if err := s.client.Get(ctx, types.NamespacedName{Name: ref.Namespace}, &ns); err != nil {
		return "", err
	}
	if v, ok := ns.Labels["istio.io/rev"]; ok && v != "" {
		return v, nil
	}
	if v, ok := ns.Labels["istio-injection"]; ok && v == "enabled" {
		return "default", nil
	}
	return "", nil
}

```

To compute the expected injected sidecar, the scanner builds a synthetic Pod object from the workload's pod template. That pod is then sent to the webhook.

The pod name is a placeholder designed to stay under Kubernetes DNS subdomain limits.

```bash
sed -n '353,432p' internal/podscanner/pod_scanner.go
```

```output
// dnsSubdomainMaxLen is the max length for Kubernetes DNS subdomain names (e.g. pod names).
const dnsSubdomainMaxLen = 63
const podNameSuffix = "-fortsa2-check"

// buildPodFromWorkload fetches the workload and builds a Pod from its template.
func (s *PodScanner) buildPodFromWorkload(ctx context.Context, ref *WorkloadRef) (*corev1.Pod, error) {
	nn := ref.NamespacedName
	// Default to a placeholder name; the webhook doesn't require a real pod name.
	name := "fortsa2-check"
	if nn.Name != "" {
		if len(nn.Name) > dnsSubdomainMaxLen-len(podNameSuffix) {
			name = nn.Name[:dnsSubdomainMaxLen-len(podNameSuffix)]
		}
		name = name + podNameSuffix
	}

	switch ref.Kind {
	case "Deployment":
		var dep appsv1.Deployment
		if err := s.client.Get(ctx, nn, &dep); err != nil {
			return nil, err
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   nn.Namespace,
				Name:        name,
				Labels:      dep.Spec.Template.Labels,
				Annotations: dep.Spec.Template.Annotations,
			},
			Spec: dep.Spec.Template.Spec,
		}, nil
	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, nn, &sts); err != nil {
			return nil, err
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   nn.Namespace,
				Name:        name,
				Labels:      sts.Spec.Template.Labels,
				Annotations: sts.Spec.Template.Annotations,
			},
			Spec: sts.Spec.Template.Spec,
		}, nil
	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := s.client.Get(ctx, nn, &ds); err != nil {
			return nil, err
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   nn.Namespace,
				Name:        name,
				Labels:      ds.Spec.Template.Labels,
				Annotations: ds.Spec.Template.Annotations,
			},
			Spec: ds.Spec.Template.Spec,
		}, nil
	default:
		return nil, nil
	}
}

func getIstioProxyImage(pod *corev1.Pod) string {
	// Check regular containers (traditional Istio sidecar injection)
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == istioProxyContainerName {
			return pod.Spec.Containers[i].Image
		}
	}
	// Check init containers (Kubernetes native sidecars: istio-proxy in initContainers with restartPolicy: Always)
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == istioProxyContainerName {
			return pod.Spec.InitContainers[i].Image
		}
	}
	return ""
}

```

Finally, workload ownership resolution. Pods are often owned by ReplicaSets (Deployments) or ControllerRevisions (StatefulSets/DaemonSets). Fortsa traverses these as intermediate nodes and only returns restartable top-level workloads: Deployment, StatefulSet, DaemonSet.

```bash
sed -n '433,526p' internal/podscanner/pod_scanner.go
```

```output
// findWorkloadOwner recursively follows ownerReferences to find a Deployment,
// StatefulSet, or DaemonSet. Returns nil if none found.
func (s *PodScanner) findWorkloadOwner(ctx context.Context, obj metav1.Object) (*WorkloadRef, error) {
	owners := obj.GetOwnerReferences()
	if len(owners) == 0 {
		return nil, nil
	}

	// Use the first controller owner (Kubernetes typically has one)
	for _, owner := range owners {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}

		ref, err := s.resolveOwner(ctx, obj, &owner)
		if err != nil {
			return nil, err
		}
		if ref != nil {
			return ref, nil
		}
	}

	return nil, nil
}

func (s *PodScanner) resolveOwner(ctx context.Context, obj metav1.Object, owner *metav1.OwnerReference) (*WorkloadRef, error) {
	nn := types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      owner.Name,
	}

	switch owner.Kind {
	case "Deployment":
		var dep appsv1.Deployment
		if err := s.client.Get(ctx, nn, &dep); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &WorkloadRef{NamespacedName: nn, Kind: "Deployment"}, nil

	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, nn, &sts); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &WorkloadRef{NamespacedName: nn, Kind: "StatefulSet"}, nil

	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := s.client.Get(ctx, nn, &ds); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &WorkloadRef{NamespacedName: nn, Kind: "DaemonSet"}, nil

	case "ReplicaSet", "ControllerRevision":
		// Recurse: fetch the owner and follow its ownerReferences
		var nextObj runtime.Object
		switch owner.Kind {
		case "ReplicaSet":
			var rs appsv1.ReplicaSet
			if err := s.client.Get(ctx, nn, &rs); err != nil {
				if errors.IsNotFound(err) {
					return nil, nil
				}
				return nil, err
			}
			nextObj = &rs
		case "ControllerRevision":
			// ControllerRevision is unstructured in apps/v1; we need to get it
			// and check its ownerReferences. ControllerRevision is in apps/v1.
			var cr appsv1.ControllerRevision
			if err := s.client.Get(ctx, nn, &cr); err != nil {
				if errors.IsNotFound(err) {
					return nil, nil
				}
				return nil, err
			}
			nextObj = &cr
		}

		return s.findWorkloadOwner(ctx, nextObj.(metav1.Object))
	}

	return nil, nil
}
```

## 7) Computing the expected injected pod: Istio webhook client (internal/webhook)

Fortsa does not try to re-implement Istio injection logic. Instead it asks Istio itself:

- Build a synthetic Pod from the workload template.
- Send it to Istio's injection webhook as an AdmissionReview Create request.
- Apply the returned JSON patch to get the mutated pod.
- Extract the `istio-proxy` image from the mutated pod and compare it to the live pod.

Key implementation details:

- The webhook URL is derived either from the MutatingWebhookConfiguration client config, or from the in-cluster service DNS name.
- TLS verification uses the webhook's `caBundle` extracted from MutatingWebhookConfiguration.
- The HTTP client has a 30s timeout.
- If the webhook returns no patch, Fortsa treats that as "no mutation" and returns the original pod.

```bash
sed -n '40,170p' internal/webhook/webhook_client.go
```

```output

const (
	istioSystemNamespace        = "istio-system"
	injectPath                  = "/inject"
	injectPort                  = 443
	webhookConfigPrefix         = "istio-sidecar-injector"
	defaultTagWebhookConfigName = "istio-revision-tag-default"
)

// WebhookCaller calls the Istio injection webhook to get the expected mutated pod.
type WebhookCaller interface {
	CallWebhook(ctx context.Context, pod *corev1.Pod, revision string, preferDefaultTagWebhook bool) (*corev1.Pod, error)
}

// WebhookClient calls the Istio sidecar injection webhook to get the expected mutated pod.
type WebhookClient struct {
	k8sClient client.Client
}

// NewWebhookClient creates a WebhookClient. caBundle is read from MutatingWebhookConfiguration.
func NewWebhookClient(k8sClient client.Client) *WebhookClient {
	return &WebhookClient{k8sClient: k8sClient}
}

// urlFromClientConfig extracts the webhook URL from WebhookClientConfig.
// Supports both URL (direct) and Service (in-cluster) references.
func urlFromClientConfig(cfg admissionregv1.WebhookClientConfig) (string, error) {
	if cfg.URL != nil && *cfg.URL != "" {
		return *cfg.URL, nil
	}
	if cfg.Service == nil {
		return "", fmt.Errorf("WebhookClientConfig has neither URL nor Service")
	}
	svc := cfg.Service
	port := int32(injectPort)
	if svc.Port != nil {
		port = *svc.Port
	}
	path := injectPath
	if svc.Path != nil && *svc.Path != "" {
		path = *svc.Path
		if path[0] != '/' {
			path = "/" + path
		}
	}
	ns := istioSystemNamespace
	if svc.Namespace != "" {
		ns = svc.Namespace
	}
	return fmt.Sprintf("https://%s.%s.svc:%d%s", svc.Name, ns, port, path), nil
}

// getWebhookURLAndCABundle returns the webhook URL and caBundle for the given revision.
// When preferDefaultTagWebhook is true, first tries istio-revision-tag-default; if it exists,
// uses its ClientConfig. Otherwise falls back to istio-sidecar-injector (or -revision) and istiod.
func (w *WebhookClient) getWebhookURLAndCABundle(ctx context.Context, revision string, preferDefaultTagWebhook bool) (url string, caBundle []byte, err error) {
	if preferDefaultTagWebhook {
		var mwc admissionregv1.MutatingWebhookConfiguration
		if err := w.k8sClient.Get(ctx, types.NamespacedName{Name: defaultTagWebhookConfigName}, &mwc); err == nil && len(mwc.Webhooks) > 0 {
			cfg := mwc.Webhooks[0].ClientConfig
			url, urlErr := urlFromClientConfig(cfg)
			if urlErr != nil {
				return "", nil, fmt.Errorf("extract URL from %s: %w", defaultTagWebhookConfigName, urlErr)
			}
			caBundle = cfg.CABundle
			if len(caBundle) == 0 {
				return "", nil, fmt.Errorf("MutatingWebhookConfiguration %s has no caBundle", defaultTagWebhookConfigName)
			}
			return url, caBundle, nil
		}
	}

	caBundle, err = w.getCABundleForRevision(ctx, revision)
	if err != nil {
		return "", nil, err
	}
	return getWebhookURL(revision), caBundle, nil
}

// getCABundleForRevision returns the caBundle from the MutatingWebhookConfiguration
// for the given Istio revision. Config name: istio-sidecar-injector (default) or istio-sidecar-injector-<revision>.
func (w *WebhookClient) getCABundleForRevision(ctx context.Context, revision string) ([]byte, error) {
	configName := webhookConfigPrefix
	if revision != "" && revision != "default" {
		configName = webhookConfigPrefix + "-" + revision
	}
	var mwc admissionregv1.MutatingWebhookConfiguration
	if err := w.k8sClient.Get(ctx, types.NamespacedName{Name: configName}, &mwc); err != nil {
		return nil, fmt.Errorf("get MutatingWebhookConfiguration %s: %w", configName, err)
	}
	if len(mwc.Webhooks) == 0 {
		return nil, fmt.Errorf("MutatingWebhookConfiguration %s has no webhooks", configName)
	}
	caBundle := mwc.Webhooks[0].ClientConfig.CABundle
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("MutatingWebhookConfiguration %s has no caBundle", configName)
	}
	return caBundle, nil
}

// transportForCABundle returns an http.Transport configured with the given caBundle for TLS verification.
func transportForCABundle(caBundle []byte) (*http.Transport, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundle) {
		return nil, fmt.Errorf("failed to parse caBundle")
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:            pool,
			InsecureSkipVerify: false,
		},
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}, nil
}

// getWebhookURL returns the Istio inject webhook URL for the given revision.
// For default revision, uses istiod.istio-system.svc; for others, istiod-<revision>.istio-system.svc.
func getWebhookURL(revision string) string {
	svcName := "istiod"
	if revision != "" && revision != "default" {
		svcName = "istiod-" + revision
	}
	// Use full FQDN for in-cluster DNS resolution
	return fmt.Sprintf("https://%s.%s.svc:%d%s", svcName, istioSystemNamespace, injectPort, injectPath)
}

// CallWebhook sends the pod to the Istio injection webhook and returns the mutated pod.
// The pod should be built from the workload's pod template (Deployment/StatefulSet/DaemonSet).
// When preferDefaultTagWebhook is true, uses istio-revision-tag-default if it exists; otherwise uses istiod.svc.
```

```bash
sed -n '168,275p' internal/webhook/webhook_client.go
```

```output
// CallWebhook sends the pod to the Istio injection webhook and returns the mutated pod.
// The pod should be built from the workload's pod template (Deployment/StatefulSet/DaemonSet).
// When preferDefaultTagWebhook is true, uses istio-revision-tag-default if it exists; otherwise uses istiod.svc.
func (w *WebhookClient) CallWebhook(ctx context.Context, pod *corev1.Pod, revision string, preferDefaultTagWebhook bool) (*corev1.Pod, error) {
	url, caBundle, err := w.getWebhookURLAndCABundle(ctx, revision, preferDefaultTagWebhook)
	if err != nil {
		return nil, fmt.Errorf("get webhook URL and caBundle: %w", err)
	}
	transport, err := transportForCABundle(caBundle)
	if err != nil {
		return nil, fmt.Errorf("create transport: %w", err)
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	podRaw, err := json.Marshal(pod)
	if err != nil {
		return nil, fmt.Errorf("marshal pod: %w", err)
	}

	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AdmissionReview",
			APIVersion: "admission.k8s.io/v1",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID("fortsa2-webhook-check"),
			Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Namespace: pod.Namespace,
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: podRaw,
			},
		},
	}

	reviewBytes, err := json.Marshal(review)
	if err != nil {
		return nil, fmt.Errorf("marshal admission review: %w", err)
	}

	log.FromContext(ctx).V(1).Info("webhook call start", "url", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reviewBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webhook request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	var reviewResp admissionv1.AdmissionReview
	if err := json.NewDecoder(resp.Body).Decode(&reviewResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if reviewResp.Response == nil {
		return nil, fmt.Errorf("webhook response has no response body")
	}
	if !reviewResp.Response.Allowed {
		return nil, fmt.Errorf("webhook denied request: %s", reviewResp.Response.Result.Message)
	}

	// Apply JSON patch to get mutated pod
	if len(reviewResp.Response.Patch) == 0 {
		// No patch means no injection (e.g. namespace not enabled)
		return pod, nil
	}

	patchedRaw, err := applyJSONPatch(podRaw, reviewResp.Response.Patch)
	if err != nil {
		return nil, fmt.Errorf("apply patch: %w", err)
	}

	var mutated corev1.Pod
	if err := json.Unmarshal(patchedRaw, &mutated); err != nil {
		return nil, fmt.Errorf("unmarshal mutated pod: %w", err)
	}

	return &mutated, nil
}

func applyJSONPatch(original, patch []byte) ([]byte, error) {
	decoded, err := jsonpatch.DecodePatch(patch)
	if err != nil {
		return nil, fmt.Errorf("decode patch: %w", err)
	}
	patched, err := decoded.Apply(original)
	if err != nil {
		return nil, fmt.Errorf("apply patch: %w", err)
	}
	return patched, nil
}
```

## 8) Actuation: trigger restarts by patching workloads (internal/annotator)

Once the scanner returns a list of workloads that appear to have outdated sidecars, Fortsa triggers restarts using the same mechanism as `kubectl rollout restart`:

- Patch `spec.template.metadata.annotations` on the owning workload.
- Specifically, set `fortsa.scaffidi.net/restartedAt` to a new RFC3339 timestamp.

Because this is a potentially disruptive operation, the annotator includes a cooldown:

- If `--annotation-cooldown` is set, and the workload was annotated recently, the patch is skipped.

The reconciler can also introduce a delay between annotating workloads (`--restart-delay`) and can run in dry-run mode (`--dry-run`).

```bash
sed -n '1,200p' internal/annotator/workload_annotator.go
```

```output
/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package annotator

import (
	"context"
	"encoding/json"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/istio-ecosystem/fortsa/internal/podscanner"
)

const (
	// RestartedAtAnnotation is the annotation key used to trigger workload restarts.
	RestartedAtAnnotation = "fortsa.scaffidi.net/restartedAt"
)

// WorkloadAnnotator annotates workloads to trigger restarts.
type WorkloadAnnotator interface {
	Annotate(ctx context.Context, ref podscanner.WorkloadRef) (annotated bool, err error)
}

// WorkloadAnnotatorImpl adds the restartedAt annotation to workload pod templates.
type WorkloadAnnotatorImpl struct {
	client             client.Client
	annotationCooldown time.Duration
}

// NewWorkloadAnnotator creates a new WorkloadAnnotatorImpl.
// annotationCooldown: skip re-annotating if the workload was annotated within this duration; 0 disables the check.
func NewWorkloadAnnotator(c client.Client, annotationCooldown time.Duration) *WorkloadAnnotatorImpl {
	return &WorkloadAnnotatorImpl{client: c, annotationCooldown: annotationCooldown}
}

// Annotate adds fortsa.scaffidi.net/restartedAt to the workload's pod template
// spec.template.metadata.annotations, triggering a rolling restart.
// When annotationCooldown is set, skips re-annotating if the workload was annotated within that duration.
// Returns (true, nil) when a patch was applied, (false, nil) when skipped or unsupported kind, (false, err) on error.
func (a *WorkloadAnnotatorImpl) Annotate(ctx context.Context, ref podscanner.WorkloadRef) (bool, error) {
	if a.annotationCooldown > 0 {
		annotatedAt, err := a.getRestartedAt(ctx, ref)
		if err != nil {
			return false, err
		}
		if !annotatedAt.IsZero() && time.Since(annotatedAt) < a.annotationCooldown {
			return false, nil // skip: already annotated recently
		}
	}

	value := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]string{
						RestartedAtAnnotation: value,
					},
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return false, err
	}

	switch ref.Kind {
	case "Deployment":
		err := a.client.Patch(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
		return err == nil, err
	case "StatefulSet":
		err := a.client.Patch(ctx, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
		return err == nil, err
	case "DaemonSet":
		err := a.client.Patch(ctx, &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
		return err == nil, err
	default:
		return false, nil
	}
}

// getRestartedAt returns the time from the workload's restartedAt annotation, or zero time if absent/unparseable.
func (a *WorkloadAnnotatorImpl) getRestartedAt(ctx context.Context, ref podscanner.WorkloadRef) (time.Time, error) {
	var annotations map[string]string
	switch ref.Kind {
	case "Deployment":
		var dep appsv1.Deployment
		if err := a.client.Get(ctx, ref.NamespacedName, &dep); err != nil {
			return time.Time{}, err
		}
		annotations = dep.Spec.Template.Annotations
	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := a.client.Get(ctx, ref.NamespacedName, &sts); err != nil {
			return time.Time{}, err
		}
		annotations = sts.Spec.Template.Annotations
	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := a.client.Get(ctx, ref.NamespacedName, &ds); err != nil {
			return time.Time{}, err
		}
		annotations = ds.Spec.Template.Annotations
	default:
		return time.Time{}, nil
	}
	if annotations == nil {
		return time.Time{}, nil
	}
	s, ok := annotations[RestartedAtAnnotation]
	if !ok || s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, nil // unparseable: treat as no annotation
	}
	return t, nil
}
```

## 9) End-to-end execution trace (putting it all together)

This is the linear control flow when something changes in the cluster:

1. A watch event happens (ConfigMap change, MWC tag change, namespace label change, or a periodic tick).
2. The controller enqueues a `ctrl.Request`.
3. `IstioChangeReconciler.Reconcile()` routes based on request shape:
   - periodic trigger: refresh ConfigMap cache from all injector ConfigMaps, then scan
   - MWC trigger: refresh tag mapping, then scan
   - namespace trigger: scan only that namespace
   - ConfigMap trigger: parse values, compute last-modified, check cache, then scan
4. Before scanning, Fortsa optionally waits `--istiod-config-read-delay` so the webhook reflects new config.
5. Fortsa lists pods (optionally limited to namespaces) and skips configured namespaces.
6. For each pod, Fortsa:
   - resolves its owning workload by traversing ownerReferences
   - determines revision or tag from workload template labels, then namespace labels
   - applies skip logic using ConfigMap last-modified by revision and MWC last-modified by tag
   - builds a synthetic pod from the workload template
   - calls the Istio injection webhook and applies the JSON patch to compute the expected mutated pod
   - compares the live vs expected `istio-proxy` image
7. For any workload with at least one outdated pod, Fortsa patches the workload pod template with `fortsa.scaffidi.net/restartedAt`, subject to cooldown.
8. Kubernetes rolls the workload, and newly created pods are injected by Istio with the expected sidecar.

Operationally, the most important "safety valves" are:

- `--dry-run` (no writes)
- `--skip-namespaces` (avoid sensitive namespaces)
- `--annotation-cooldown` (avoid restart churn)
- `--restart-delay` (throttle restarts)
- `--istiod-config-read-delay` plus last-modified based skip logic (reduce unnecessary scanning and false positives)


