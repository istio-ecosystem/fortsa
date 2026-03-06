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
	"sync"
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/istio-ecosystem/fortsa/internal/configmap"
	"github.com/istio-ecosystem/fortsa/internal/mwc"
	"github.com/istio-ecosystem/fortsa/internal/periodic"
	"github.com/istio-ecosystem/fortsa/internal/podscanner"
)

// countingAnnotator counts Annotate calls for testing.
type countingAnnotator struct {
	mu    sync.Mutex
	count int
}

func (c *countingAnnotator) Annotate(_ context.Context, _ podscanner.WorkloadRef) (bool, error) {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	return true, nil
}

func (c *countingAnnotator) getCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func TestIstioChangeReconciler_Reconcile_PeriodicTrigger(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
		Data: map[string]string{
			"values": `{"revision":"default","global":{"hub":"docker.io/istio","tag":"1.20.1","proxy":{"image":"proxyv2"}}}`,
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	r := NewIstioChangeReconciler(ReconcilerOptions{
		Client:     fakeClient,
		Scheme:     scheme,
		DryRun:     false,
		CompareHub: true,
	})
	req := periodic.ReconcileRequest()
	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile(periodic): %v", err)
	}
	// Periodic reconcile runs scan; with nil webhook, scanner returns no workloads
	// Verifies reconcileAll runs without error
}

func TestIstioChangeReconciler_Reconcile_MWCTrigger(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
		Data: map[string]string{
			"values": `{"revision":"default","global":{"hub":"docker.io/istio","tag":"1.20.1","proxy":{"image":"proxyv2"}}}`,
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	r := NewIstioChangeReconciler(ReconcilerOptions{
		Client:     fakeClient,
		Scheme:     scheme,
		DryRun:     false,
		CompareHub: true,
	})
	req := mwc.ReconcileRequest()
	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile(MWC): %v", err)
	}
	// MWC reconcile runs fetchTagMappingAndScan; with nil webhook, scanner returns no workloads
	// Verifies reconcileMWCChange runs without error
}

func TestIstioChangeReconciler_Reconcile_NamespaceTrigger(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
		Data: map[string]string{
			"values": `{"revision":"default","global":{"hub":"docker.io/istio","tag":"1.20.1","proxy":{"image":"proxyv2"}}}`,
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	r := NewIstioChangeReconciler(ReconcilerOptions{
		Client:     fakeClient,
		Scheme:     scheme,
		DryRun:     false,
		CompareHub: true,
	})
	// Namespace trigger: Namespace empty, Name is the namespace to reconcile
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "", Name: "default"}}
	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile(namespace): %v", err)
	}
}

func TestIstioChangeReconciler_Reconcile_ConfigMapTrigger(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	validCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
		Data: map[string]string{
			"values": `{"revision":"default","global":{"hub":"docker.io/istio","tag":"1.20.1","proxy":{"image":"proxyv2"}}}`,
		},
	}

	t.Run("ConfigMap trigger runs reconcileConfigMapChange", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(validCM).Build()
		r := NewIstioChangeReconciler(ReconcilerOptions{Client: fakeClient, Scheme: scheme})
		req := configmap.ReconcileRequest()
		_, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("Reconcile(configmap trigger): %v", err)
		}
	})

	t.Run("ConfigMap trigger with no ConfigMaps succeeds", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		r := NewIstioChangeReconciler(ReconcilerOptions{Client: fakeClient, Scheme: scheme})
		req := configmap.ReconcileRequest()
		_, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("Reconcile(configmap trigger, no ConfigMaps): %v", err)
		}
	})

	t.Run("ConfigMap trigger skips invalid ConfigMap and continues", func(t *testing.T) {
		invalidCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
			Data:       map[string]string{"values": `{invalid json}`},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(invalidCM).Build()
		r := NewIstioChangeReconciler(ReconcilerOptions{Client: fakeClient, Scheme: scheme})
		req := configmap.ReconcileRequest()
		_, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("Reconcile(configmap trigger, invalid ConfigMap): %v", err)
		}
	})

	t.Run("unknown request logs error and returns no-op", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(validCM).Build()
		r := NewIstioChangeReconciler(ReconcilerOptions{Client: fakeClient, Scheme: scheme})
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "unknown"}}
		result, err := r.Reconcile(context.Background(), req)
		if err != nil {
			t.Fatalf("Reconcile(unknown): expected no error (request ignored): %v", err)
		}
		if result.RequeueAfter > 0 {
			t.Error("unknown request should not requeue")
		}
	})
}

func TestIstioChangeReconciler_annotateWorkloadsWithDelay(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	t.Run("no delay annotates all workloads", func(t *testing.T) {
		ann := &countingAnnotator{}
		r := &IstioChangeReconciler{
			Client:       fakeClient,
			annotator:    ann,
			dryRun:       false,
			restartDelay: 0,
		}
		workloads := []podscanner.WorkloadRef{
			{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "dep1"}, Kind: "Deployment"},
			{NamespacedName: types.NamespacedName{Namespace: "ns2", Name: "dep2"}, Kind: "Deployment"},
		}
		r.annotateWorkloadsWithDelay(context.Background(), workloads)
		if got := ann.getCount(); got != 2 {
			t.Errorf("annotateWorkloadsWithDelay(restartDelay=0): annotator called %d times, want 2", got)
		}
	})

	t.Run("delay between workloads", func(t *testing.T) {
		ann := &countingAnnotator{}
		r := &IstioChangeReconciler{
			Client:       fakeClient,
			annotator:    ann,
			dryRun:       false,
			restartDelay: 1 * time.Second,
		}
		workloads := []podscanner.WorkloadRef{
			{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "dep1"}, Kind: "Deployment"},
			{NamespacedName: types.NamespacedName{Namespace: "ns2", Name: "dep2"}, Kind: "Deployment"},
		}
		start := time.Now()
		r.annotateWorkloadsWithDelay(context.Background(), workloads)
		elapsed := time.Since(start)
		if got := ann.getCount(); got != 2 {
			t.Errorf("annotateWorkloadsWithDelay: annotator called %d times, want 2", got)
		}
		if elapsed < 1*time.Second {
			t.Errorf("annotateWorkloadsWithDelay: elapsed %v, want >= 1s (delay between workloads)", elapsed)
		}
	})

	t.Run("context cancel during delay exits early", func(t *testing.T) {
		ann := &countingAnnotator{}
		r := &IstioChangeReconciler{
			Client:       fakeClient,
			annotator:    ann,
			dryRun:       false,
			restartDelay: 1 * time.Second,
		}
		workloads := []podscanner.WorkloadRef{
			{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "dep1"}, Kind: "Deployment"},
			{NamespacedName: types.NamespacedName{Namespace: "ns2", Name: "dep2"}, Kind: "Deployment"},
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			r.annotateWorkloadsWithDelay(ctx, workloads)
			close(done)
		}()
		time.Sleep(10 * time.Millisecond)
		cancel()
		<-done
		if got := ann.getCount(); got != 1 {
			t.Errorf("annotateWorkloadsWithDelay (cancelled during delay): annotator called %d times, want 1 (second workload skipped)", got)
		}
	})

	t.Run("dry-run does not annotate", func(t *testing.T) {
		ann := &countingAnnotator{}
		r := &IstioChangeReconciler{
			Client:       fakeClient,
			annotator:    ann,
			dryRun:       true,
			restartDelay: 0,
		}
		workloads := []podscanner.WorkloadRef{
			{NamespacedName: types.NamespacedName{Namespace: "ns1", Name: "dep1"}, Kind: "Deployment"},
		}
		r.annotateWorkloadsWithDelay(context.Background(), workloads)
		if got := ann.getCount(); got != 0 {
			t.Errorf("annotateWorkloadsWithDelay (dry-run): annotator called %d times, want 0", got)
		}
	})
}
