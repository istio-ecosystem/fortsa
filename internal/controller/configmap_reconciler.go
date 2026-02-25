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

package controller

import (
	"context"
	"strings"
	"sync"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/istio-ecosystem/fortsa/internal/annotator"
	"github.com/istio-ecosystem/fortsa/internal/configmap"
	"github.com/istio-ecosystem/fortsa/internal/podscanner"
	"github.com/istio-ecosystem/fortsa/internal/webhook"
)

const istioRevisionTagPrefix = "istio-revision-tag-"

// PeriodicReconcileRequest returns a reconcile.Request that triggers a full periodic reconciliation
// of all istio-sidecar-injector ConfigMaps. Used by the periodic source.
func PeriodicReconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: istioSystemNamespace, Name: periodicReconcileTriggerName},
	}
}

// NewPeriodicReconcileSource returns a source that enqueues a full-reconcile request at the given period.
// When period is 0, returns nil (caller should not add the source).
func NewPeriodicReconcileSource(period time.Duration) source.Source {
	return source.Func(func(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		go func() {
			ticker := time.NewTicker(period)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					queue.Add(PeriodicReconcileRequest())
				}
			}
		}()
		return nil
	})
}

const (
	istioSystemNamespace         = "istio-system"
	configMapNamePrefix          = "istio-sidecar-injector"
	periodicReconcileTriggerName = "__periodic_reconcile__"
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
func (r *ConfigMapReconciler) awaitIstiodConfigReadDelay(ctx context.Context) error {
	if r.istiodConfigReadDelay <= 0 {
		return nil
	}
	return waitOrContextDone(ctx, r.istiodConfigReadDelay)
}

// fetchTagToRevision lists istio-revision-tag-* MutatingWebhookConfigurations and builds
// a tag-to-revision map from istio.io/tag and istio.io/rev labels.
func fetchTagToRevision(ctx context.Context, c client.Client) (map[string]string, error) {
	tagToRevision, _, err := fetchTagToRevisionAndLastModified(ctx, c)
	return tagToRevision, err
}

// fetchTagToRevisionAndLastModified lists istio-revision-tag-* MutatingWebhookConfigurations,
// builds tag-to-revision map and tag-to-lastModified map (for pod skip logic).
func fetchTagToRevisionAndLastModified(ctx context.Context, c client.Client) (map[string]string, map[string]time.Time, error) {
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
			lastModifiedByTag[tag] = getMWCLastModified(mwc)
		}
	}
	return tagToRevision, lastModifiedByTag, nil
}

// getMWCLastModified returns the latest modification time of the MutatingWebhookConfiguration.
func getMWCLastModified(mwc *admissionregv1.MutatingWebhookConfiguration) time.Time {
	latest := mwc.CreationTimestamp.Time
	for _, mf := range mwc.ManagedFields {
		if mf.Time != nil && mf.Time.After(latest) {
			latest = mf.Time.Time
		}
	}
	return latest
}

// ConfigMapReconciler reconciles Istio sidecar injector ConfigMaps.
type ConfigMapReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	scanner               *podscanner.PodScanner
	annotator             annotator.WorkloadAnnotator
	dryRun                bool
	compareHub            bool
	restartDelay          time.Duration
	istiodConfigReadDelay time.Duration
	skipNamespaces        []string

	// cache stores ConfigMap LastModified by revision for change detection and pod skip logic.
	cache map[string]time.Time
	// nameToRevision maps ConfigMap namespace/name to revision for delete cleanup.
	nameToRevision map[string]string
	cacheMu        sync.RWMutex
}

// NewConfigMapReconciler creates a new ConfigMapReconciler.
// restartDelay is the delay between restarting each workload; 0 means no delay.
// istiodConfigReadDelay is how long to wait for Istiod to read the updated ConfigMap before scanning; 0 skips the wait.
// webhookCaller is used to call the Istio injection webhook for outdated pod detection; if nil, scanning is skipped.
// skipNamespaces lists namespaces to skip when scanning pods.
func NewConfigMapReconciler(c client.Client, scheme *runtime.Scheme, dryRun bool, compareHub bool, restartDelay time.Duration, istiodConfigReadDelay time.Duration, skipNamespaces []string, webhookCaller webhook.WebhookCaller) *ConfigMapReconciler {
	return &ConfigMapReconciler{
		Client:                c,
		Scheme:                scheme,
		scanner:               podscanner.NewPodScanner(c, webhookCaller),
		annotator:             annotator.NewWorkloadAnnotator(c),
		dryRun:                dryRun,
		compareHub:            compareHub,
		restartDelay:          restartDelay,
		istiodConfigReadDelay: istiodConfigReadDelay,
		skipNamespaces:        skipNamespaces,
		cache:                 make(map[string]time.Time),
		nameToRevision:        make(map[string]string),
	}
}

// Reconcile implements the reconcile loop for ConfigMap events.
func (r *ConfigMapReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Periodic reconciliation: reconcile all istio-sidecar-injector ConfigMaps regardless of change detection
	if req.Name == periodicReconcileTriggerName {
		return r.reconcileAll(ctx)
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
			r.clearCacheByConfigMap(req.String())
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

	if !r.lastModifiedChanged(revision, lastModified) {
		return ctrl.Result{}, nil
	}
	r.setCache(req.String(), revision, lastModified)

	// Wait for Istiod to read the updated ConfigMap before scanning (webhook uses Istiod's config)
	// TODO: This is a hack... We should find a better mechanism.
	if err := r.awaitIstiodConfigReadDelay(ctx); err != nil {
		return ctrl.Result{}, err
	}

	return r.fetchTagMappingAndScan(ctx, nil)
}

// reconcileNamespace performs a namespace-scoped reconciliation when Istio labels change on a namespace.
func (r *ConfigMapReconciler) reconcileNamespace(ctx context.Context, namespace string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	logger.Info("reconciling namespace (Istio label change)", "namespace", namespace)
	if err := r.awaitIstiodConfigReadDelay(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return r.fetchTagMappingAndScan(ctx, []string{namespace})
}

// reconcileAll performs a full reconciliation of all istio-sidecar-injector ConfigMaps,
// bypassing change detection. Used for periodic reconciliation and MWC changes.
func (r *ConfigMapReconciler) reconcileAll(ctx context.Context) (ctrl.Result, error) {
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
		r.setCache(client.ObjectKeyFromObject(cm).String(), vals.Revision, configmap.GetConfigMapLastModified(cm))
	}

	if err := r.awaitIstiodConfigReadDelay(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return r.fetchTagMappingAndScan(ctx, nil)
}

// fetchTagMappingAndScan fetches tag-to-revision and lastModifiedByTag from MWCs, then scans and annotates.
// limitToNamespaces, when non-empty, restricts scanning to those namespaces only (e.g. for namespace label changes).
func (r *ConfigMapReconciler) fetchTagMappingAndScan(ctx context.Context, limitToNamespaces []string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	tagToRevision, lastModifiedByTag, err := fetchTagToRevisionAndLastModified(ctx, r.Client)
	if err != nil {
		logger.Error(err, "failed to fetch tag-to-revision mapping")
		return ctrl.Result{}, err
	}
	return r.scanAndAnnotate(ctx, tagToRevision, lastModifiedByTag, limitToNamespaces)
}

// scanAndAnnotate scans for outdated pods using the current cache and annotates workloads to trigger restarts.
// limitToNamespaces, when non-empty, restricts scanning to those namespaces only.
func (r *ConfigMapReconciler) scanAndAnnotate(ctx context.Context, tagToRevision map[string]string, lastModifiedByTag map[string]time.Time, limitToNamespaces []string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if tagToRevision == nil {
		tagToRevision = map[string]string{}
	}
	lastModifiedByRevision := r.getCacheCopy()
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
func (r *ConfigMapReconciler) annotateWorkloadsWithDelay(ctx context.Context, workloads []podscanner.WorkloadRef) {
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
				"annotation", "fortsa.scaffidi.net/restartedAt")
			continue
		}
		if err := r.annotator.Annotate(ctx, ref); err != nil {
			logger.Error(err, "failed to annotate workload", "workload", ref.NamespacedName, "kind", ref.Kind)
			continue
		}
		logger.Info("annotated workload for restart", "workload", ref.NamespacedName, "kind", ref.Kind)
	}
}

func (r *ConfigMapReconciler) lastModifiedChanged(revision string, lastModified time.Time) bool {
	r.cacheMu.RLock()
	prev := r.cache[revision]
	r.cacheMu.RUnlock()

	if prev.IsZero() {
		return true
	}
	return !prev.Equal(lastModified)
}

func (r *ConfigMapReconciler) setCache(configMapKey, revision string, lastModified time.Time) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	r.cache[revision] = lastModified
	r.nameToRevision[configMapKey] = revision
}

// getCacheCopy returns a shallow copy of the revision -> LastModified map for safe concurrent use.
func (r *ConfigMapReconciler) getCacheCopy() map[string]time.Time {
	r.cacheMu.RLock()
	defer r.cacheMu.RUnlock()
	copy := make(map[string]time.Time, len(r.cache))
	for k, v := range r.cache {
		copy[k] = v
	}
	return copy
}

func (r *ConfigMapReconciler) clearCacheByConfigMap(configMapKey string) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if revision, ok := r.nameToRevision[configMapKey]; ok {
		delete(r.cache, revision)
		delete(r.nameToRevision, configMapKey)
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

// MutatingWebhookFilter returns a predicate that filters MutatingWebhookConfigurations
// to only those named istio-revision-tag-* (tag-to-revision mapping).
func MutatingWebhookFilter() func(client.Object) bool {
	return func(obj client.Object) bool {
		return strings.HasPrefix(obj.GetName(), istioRevisionTagPrefix)
	}
}

// namespaceHasIstioLabels returns true if the namespace has istio.io/rev or istio-injection in its labels.
func namespaceHasIstioLabels(obj client.Object) bool {
	labels := obj.GetLabels()
	if _, ok := labels["istio.io/rev"]; ok {
		return true
	}
	if v, ok := labels["istio-injection"]; ok && v != "" {
		return true
	}
	return false
}

// NamespaceFilter returns a predicate that filters Namespace events to only those
// with Istio-related labels (istio.io/rev or istio-injection). For updates, triggers
// when either old or new has the labels to catch add, remove, or change.
func NamespaceFilter() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return namespaceHasIstioLabels(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return namespaceHasIstioLabels(e.ObjectOld) || namespaceHasIstioLabels(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return namespaceHasIstioLabels(e.Object)
		},
	}
}

// NamespaceReconcileRequest returns a reconcile.Request for a namespace-scoped reconciliation.
// Used when a Namespace's Istio labels change.
func NamespaceReconcileRequest(namespace string) ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Name: namespace},
	}
}
