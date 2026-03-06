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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/istio-ecosystem/fortsa/internal/annotator"
	"github.com/istio-ecosystem/fortsa/internal/configmap"
	"github.com/istio-ecosystem/fortsa/internal/constants"
	"github.com/istio-ecosystem/fortsa/internal/mwc"
	"github.com/istio-ecosystem/fortsa/internal/periodic"
	"github.com/istio-ecosystem/fortsa/internal/podscanner"
	"github.com/istio-ecosystem/fortsa/internal/webhook"
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
	}
}

// Reconcile implements the reconcile loop for ConfigMap, MWC, namespace, and periodic events.
func (r *IstioChangeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	defer log.FromContext(ctx).Info("reconcile completed")

	// Periodic reconciliation: full reconcile of all ConfigMaps (ticker-triggered only)
	if req.Name == periodic.ReconcileRequestName() {
		log.FromContext(ctx).Info("periodic reconciliation")
		return r.reconcileAll(ctx, nil)
	}

	// ConfigMap change: list matching ConfigMaps, refresh cache, and scan
	if req.Name == configmap.ReconcileRequestName() {
		log.FromContext(ctx).Info("Istio change, reconciling")
		return r.reconcileAll(ctx, nil)
	}

	// Namespace label change: scan only pods in that namespace
	if req.Namespace == "" && req.Name != "" {
		log.FromContext(ctx).Info("namespace label change, scanning namespace", "namespace", req.Name)
		return r.reconcileAll(ctx, []string{req.Name})
	}

	return ctrl.Result{}, nil
}

// reconcileAll performs a full reconciliation of istio-sidecar-injector ConfigMaps.
// When limitToNamespaces is nil, scans all namespaces; when non-nil, restricts scanning to those namespaces.
func (r *IstioChangeReconciler) reconcileAll(ctx context.Context, limitToNamespaces []string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if err := r.awaitIstiodConfigReadDelay(ctx); err != nil {
		return ctrl.Result{}, fmt.Errorf("await istiod config read delay: %w", err)
	}

	if limitToNamespaces != nil {
		logger.Info("scanning namespaces", "namespaces", limitToNamespaces)
	} else {
		logger.Info("scanning all pods")
	}

	var cmList corev1.ConfigMapList
	if err := r.List(ctx, &cmList, client.InNamespace(constants.IstioSystemNamespace)); err != nil {
		logger.Error(err, "failed to list ConfigMaps")
		return ctrl.Result{}, fmt.Errorf("list ConfigMaps in %s: %w", constants.IstioSystemNamespace, err)
	}

	lastModifiedByRevision := make(map[string]time.Time)
	for i := range cmList.Items {
		cm := &cmList.Items[i]
		if !strings.HasPrefix(cm.Name, constants.ConfigMapNamePrefix) {
			continue
		}
		vals, err := configmap.ParseConfigMapValues(cm)
		if err != nil {
			logger.Error(err, "failed to parse ConfigMap values", "configmap", cm.Name)
			continue
		}
		lastModified := configmap.GetConfigMapLastModified(cm)
		if existing, ok := lastModifiedByRevision[vals.Revision]; !ok || lastModified.After(existing) {
			lastModifiedByRevision[vals.Revision] = lastModified
		}
	}

	return r.fetchTagMappingAndScan(ctx, lastModifiedByRevision, limitToNamespaces)
}

// fetchTagMappingAndScan fetches tag-to-revision and lastModifiedByTag from MWCs, then scans and annotates.
// limitToNamespaces, when non-empty, restricts scanning to those namespaces only (e.g. for namespace label changes).
func (r *IstioChangeReconciler) fetchTagMappingAndScan(ctx context.Context, lastModifiedByRevision map[string]time.Time, limitToNamespaces []string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	tagToRevision, lastModifiedByTag, err := mwc.FetchTagToRevisionAndLastModified(ctx, r.Client)
	if err != nil {
		logger.Error(err, "failed to fetch tag-to-revision mapping")
		return ctrl.Result{}, fmt.Errorf("fetch tag-to-revision mapping: %w", err)
	}
	return r.scanAndAnnotate(ctx, lastModifiedByRevision, tagToRevision, lastModifiedByTag, limitToNamespaces)
}

// scanAndAnnotate scans for outdated pods and annotates workloads to trigger restarts.
// limitToNamespaces, when non-empty, restricts scanning to those namespaces only.
func (r *IstioChangeReconciler) scanAndAnnotate(ctx context.Context, lastModifiedByRevision map[string]time.Time, tagToRevision map[string]string, lastModifiedByTag map[string]time.Time, limitToNamespaces []string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if tagToRevision == nil {
		tagToRevision = map[string]string{}
	}
	if lastModifiedByRevision == nil {
		lastModifiedByRevision = map[string]time.Time{}
	}
	opts := podscanner.ScanOptions{
		CompareHub:            r.compareHub,
		IstiodConfigReadDelay: r.istiodConfigReadDelay,
		SkipNamespaces:        r.skipNamespaces,
		LimitToNamespaces:     limitToNamespaces,
	}
	workloads, err := r.scanner.ScanOutdatedPods(ctx, lastModifiedByRevision, tagToRevision, lastModifiedByTag, opts)
	if err != nil {
		logger.Error(err, "failed to scan pods")
		return ctrl.Result{}, fmt.Errorf("scan outdated pods: %w", err)
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
				"namespace", ref.Namespace, "name", ref.Name, "kind", ref.Kind)
			continue
		}
		annotated, err := r.annotator.Annotate(ctx, ref)
		if err != nil {
			logger.Error(err, "failed to annotate workload for restart", "namespace", ref.Namespace, "name", ref.Name, "kind", ref.Kind)
			// Partial success: log and continue with remaining workloads.
			continue
		}
		if annotated {
			logger.Info("annotated workload for restart", "namespace", ref.Namespace, "name", ref.Name, "kind", ref.Kind)
		} else {
			// currently only happens due to cooldown period.
			logger.Info("skipped annotating workload for restart", "namespace", ref.Namespace, "name", ref.Name, "kind", ref.Kind)
		}
	}
}
