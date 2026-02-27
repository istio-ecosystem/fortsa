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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/istio-ecosystem/fortsa/internal/podscanner"
)

// countingAnnotator counts Annotate calls for testing.
type countingAnnotator struct {
	mu    sync.Mutex
	count int
}

func (c *countingAnnotator) Annotate(_ context.Context, _ podscanner.WorkloadRef) error {
	c.mu.Lock()
	c.count++
	c.mu.Unlock()
	return nil
}

func (c *countingAnnotator) getCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func TestFetchTagToRevision(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	mwc := &admissionregv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: "istio-revision-tag-canary",
			Labels: map[string]string{
				"istio.io/tag": "canary",
				"istio.io/rev": "1-20",
			},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mwc).Build()

	got, err := fetchTagToRevision(context.Background(), fakeClient)
	if err != nil {
		t.Fatalf("fetchTagToRevision: %v", err)
	}
	if got["canary"] != "1-20" {
		t.Errorf("fetchTagToRevision: want canary->1-20, got %v", got)
	}
}

func TestPeriodicReconcileRequest(t *testing.T) {
	req := PeriodicReconcileRequest()
	if req.Namespace != "istio-system" {
		t.Errorf("PeriodicReconcileRequest namespace = %q, want istio-system", req.Namespace)
	}
	if req.Name != "__periodic_reconcile__" {
		t.Errorf("PeriodicReconcileRequest name = %q, want __periodic_reconcile__", req.Name)
	}
}

func TestMWCReconcileRequest(t *testing.T) {
	req := MWCReconcileRequest()
	if req.Namespace != "istio-system" {
		t.Errorf("MWCReconcileRequest namespace = %q, want istio-system", req.Namespace)
	}
	if req.Name != "__mwc_reconcile__" {
		t.Errorf("MWCReconcileRequest name = %q, want __mwc_reconcile__", req.Name)
	}
}

func TestConfigMapReconciler_Reconcile_PeriodicTrigger(t *testing.T) {
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
	r := NewConfigMapReconciler(fakeClient, scheme, false, true, 0, 0, 0, nil, nil)
	req := PeriodicReconcileRequest()
	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile(periodic): %v", err)
	}
	// Periodic reconcile runs scan; with nil webhook, scanner returns no workloads
	// Verifies reconcileAll runs without error
}

func TestConfigMapReconciler_Reconcile_MWCTrigger(t *testing.T) {
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
	r := NewConfigMapReconciler(fakeClient, scheme, false, true, 0, 0, 0, nil, nil)
	req := MWCReconcileRequest()
	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("Reconcile(MWC): %v", err)
	}
	// MWC reconcile runs fetchTagMappingAndScan; with nil webhook, scanner returns no workloads
	// Verifies reconcileMWCChange runs without error
}

func TestMutatingWebhookFilter(t *testing.T) {
	filter := MutatingWebhookFilter()
	tests := []struct {
		name     string
		obj      *admissionregv1.MutatingWebhookConfiguration
		wantPass bool
	}{
		{
			name: "istio-revision-tag-default",
			obj: &admissionregv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "istio-revision-tag-default"},
			},
			wantPass: true,
		},
		{
			name: "istio-revision-tag-canary",
			obj: &admissionregv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "istio-revision-tag-canary"},
			},
			wantPass: true,
		},
		{
			name: "istio-sidecar-injector",
			obj: &admissionregv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "istio-sidecar-injector"},
			},
			wantPass: false,
		},
		{
			name: "other-webhook",
			obj: &admissionregv1.MutatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "other-webhook"},
			},
			wantPass: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := filter(tt.obj); got != tt.wantPass {
				t.Errorf("MutatingWebhookFilter() = %v, want %v", got, tt.wantPass)
			}
		})
	}
}

func TestConfigMapFilter(t *testing.T) {
	filter := ConfigMapFilter()
	tests := []struct {
		name     string
		obj      *corev1.ConfigMap
		wantPass bool
	}{
		{
			name: "istio-system and istio-sidecar-injector",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
			},
			wantPass: true,
		},
		{
			name: "istio-system and istio-sidecar-injector-canary",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector-canary"},
			},
			wantPass: true,
		},
		{
			name: "wrong namespace",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "istio-sidecar-injector"},
			},
			wantPass: false,
		},
		{
			name: "wrong name prefix",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "other-config"},
			},
			wantPass: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := filter(tt.obj); got != tt.wantPass {
				t.Errorf("ConfigMapFilter() = %v, want %v", got, tt.wantPass)
			}
		})
	}
}

func TestConfigMapReconciler_lastModifiedChanged(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := NewConfigMapReconciler(fakeClient, scheme, false, true, 0, 0, 0, nil, nil)

	t1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)

	// First call: no cache, should report changed
	if !r.lastModifiedChanged("default", t1) {
		t.Error("lastModifiedChanged: want true (no cache)")
	}

	// Set cache
	r.setCache("istio-system/cm", "default", t1)

	// Same timestamp: should report not changed
	if r.lastModifiedChanged("default", t1) {
		t.Error("lastModifiedChanged: want false (same timestamp)")
	}

	// Different timestamp: should report changed
	if !r.lastModifiedChanged("default", t2) {
		t.Error("lastModifiedChanged: want true (timestamp changed)")
	}
}

func TestConfigMapReconciler_getCacheCopy(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	t1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	r := NewConfigMapReconciler(fakeClient, scheme, false, true, 0, 0, 0, nil, nil)
	r.setCache("istio-system/cm", "default", t1)

	copy := r.getCacheCopy()
	if len(copy) != 1 {
		t.Fatalf("getCacheCopy: want 1 entry, got %d", len(copy))
	}
	if !copy["default"].Equal(t1) {
		t.Errorf("getCacheCopy: want %v, got %v", t1, copy["default"])
	}
}

func TestConfigMapReconciler_clearCacheByConfigMap(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := NewConfigMapReconciler(fakeClient, scheme, false, true, 0, 0, 0, nil, nil)
	r.setCache("istio-system/cm", "default", time.Now())

	r.clearCacheByConfigMap("istio-system/cm")
	copy := r.getCacheCopy()
	if len(copy) != 0 {
		t.Errorf("clearCacheByConfigMap: want empty cache, got %d entries", len(copy))
	}
}

func TestConfigMapReconciler_annotateWorkloadsWithDelay(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	t.Run("no delay annotates all workloads", func(t *testing.T) {
		ann := &countingAnnotator{}
		r := &ConfigMapReconciler{
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
		r := &ConfigMapReconciler{
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
			t.Errorf("annotateWorkloadsWithDelay: elapsed %v, want >= 5ms (delay between workloads)", elapsed)
		}
	})

	t.Run("context cancel during delay exits early", func(t *testing.T) {
		ann := &countingAnnotator{}
		r := &ConfigMapReconciler{
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
		r := &ConfigMapReconciler{
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
