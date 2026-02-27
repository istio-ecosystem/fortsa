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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/istio-ecosystem/fortsa/internal/annotator"
	"github.com/istio-ecosystem/fortsa/internal/cache"
	"github.com/istio-ecosystem/fortsa/internal/configmap"
	"github.com/istio-ecosystem/fortsa/internal/mwc"
	"github.com/istio-ecosystem/fortsa/internal/periodic"
	"github.com/istio-ecosystem/fortsa/internal/podscanner"
	"github.com/istio-ecosystem/fortsa/internal/webhook"
)

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
	logger.Info("reconciling namespace (Istio label change)", "namespace", namespace)
	if err := r.awaitIstiodConfigReadDelay(ctx); err != nil {
		return ctrl.Result{}, err
	}
	return r.fetchTagMappingAndScan(ctx, []string{namespace})
}

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
