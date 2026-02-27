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

package podscanner

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/istio-ecosystem/fortsa/internal/configmap"
)

// fakeWebhookCaller returns a mutated pod with the given istio-proxy image.
type fakeWebhookCaller struct {
	expectedProxyImage string
}

func (f *fakeWebhookCaller) CallWebhook(_ context.Context, pod *corev1.Pod, _ string, _ bool) (*corev1.Pod, error) {
	mutated := pod.DeepCopy()
	// Add or replace istio-proxy container with expected image
	found := false
	for i := range mutated.Spec.Containers {
		if mutated.Spec.Containers[i].Name == istioProxyContainerName {
			mutated.Spec.Containers[i].Image = f.expectedProxyImage
			found = true
			break
		}
	}
	if !found && f.expectedProxyImage != "" {
		mutated.Spec.Containers = append(mutated.Spec.Containers, corev1.Container{
			Name:  istioProxyContainerName,
			Image: f.expectedProxyImage,
		})
	}
	return mutated, nil
}

func TestParseImage(t *testing.T) {
	tests := []struct {
		image    string
		wantReg  string
		wantName string
		wantTag  string
	}{
		{"docker.io/istio/proxyv2:1.20.1", "docker.io/istio", "proxyv2", "1.20.1"},
		{"gcr.io/istio/proxyv2:latest", "gcr.io/istio", "proxyv2", "latest"},
		{"registry:5000/repo/image:tag", "registry:5000/repo", "image", "tag"},
		{"proxyv2", "", "proxyv2", "latest"},
		{"istio/proxyv2", "istio", "proxyv2", "latest"},
		{"image@sha256:abc123", "", "image", "latest"},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			p := ParseImage(tt.image)
			if p.Registry != tt.wantReg || p.ImageName != tt.wantName || p.Tag != tt.wantTag {
				t.Errorf("ParseImage(%q) = %+v, want Registry=%q ImageName=%q Tag=%q",
					tt.image, p, tt.wantReg, tt.wantName, tt.wantTag)
			}
		})
	}
}

func TestParsedImage_Matches(t *testing.T) {
	vals := &configmap.IstioValues{Hub: "docker.io/istio", Tag: "1.20.1", Image: "proxyv2"}
	tests := []struct {
		image      string
		compareHub bool
		want       bool
	}{
		{"docker.io/istio/proxyv2:1.20.1", true, true},
		{"docker.io/istio/proxyv2:1.20.0", true, false},
		{"gcr.io/istio/proxyv2:1.20.1", true, false},
		{"docker.io/istio/other:1.20.1", true, false},
		// compareHub=false: registry is ignored
		{"gcr.io/istio/proxyv2:1.20.1", false, true},
		{"registry.example.com/istio/proxyv2:1.20.1", false, true},
		{"docker.io/istio/proxyv2:1.20.0", false, false},
	}
	for _, tt := range tests {
		name := tt.image
		if !tt.compareHub {
			name += "_no_hub"
		}
		t.Run(name, func(t *testing.T) {
			p := ParseImage(tt.image)
			if got := p.Matches(vals, tt.compareHub); got != tt.want {
				t.Errorf("ParseImage(%q).Matches(compareHub=%v) = %v, want %v", tt.image, tt.compareHub, got, tt.want)
			}
		})
	}
}

func TestGetPodRevision(t *testing.T) {
	// getPodRevision is tested indirectly via ScanOutdatedPods; we test the behavior here
	// by creating pods with different status annotations and verifying ScanOutdatedPods
	// uses the correct revision for lookup.
	t.Run("revision from sidecar status", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"istio-injection": "enabled"}}},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-canary",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-1", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-1",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-1", Controller: ptr(true)},
						},
					},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-1", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
			).
			Build()

		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		lastModifiedByRevision := map[string]time.Time{}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), lastModifiedByRevision, map[string]string{}, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload, got %d", len(workloads))
		}
		if len(workloads) > 0 && workloads[0].Name != "dep-1" {
			t.Errorf("want dep-1, got %s", workloads[0].Name)
		}
	})

	t.Run("skip pod with no workload owner", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-standalone",
						Namespace: "default",
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		// Pod has no owner refs (standalone pod); we scan all pods but skip when no restartable workload
		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		lastModifiedByRevision := map[string]time.Time{}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), lastModifiedByRevision, map[string]string{}, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 0 {
			t.Errorf("want 0 workloads (pod has no workload owner), got %d", len(workloads))
		}
	})

	t.Run("skip pod with up-to-date image", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "dep-ok", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "ok"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "ok"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
				},
			},
		}
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"istio-injection": "enabled"}}},
				dep,
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-ok",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-ok", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-ok",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-ok", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.20.1"},
						},
					},
				},
			).
			Build()

		// Webhook returns same image as current pod -> up to date
		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		lastModifiedByRevision := map[string]time.Time{}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), lastModifiedByRevision, map[string]string{}, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 0 {
			t.Errorf("want 0 workloads (image matches webhook), got %d", len(workloads))
		}
	})

	t.Run("skip pod created after ConfigMap last update", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		configMapTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
		podTime := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC) // pod created after ConfigMap

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"istio-injection": "enabled"}}},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-1", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-1",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-1", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "pod-new",
						Namespace:         "default",
						CreationTimestamp: metav1.NewTime(podTime),
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-1", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		// Pod has old image but was created after ConfigMap update -> skip (would have been injected with current config)
		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		lastModifiedByRevision := map[string]time.Time{"default": configMapTime}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), lastModifiedByRevision, map[string]string{}, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 0 {
			t.Errorf("want 0 workloads (pod created after ConfigMap), got %d", len(workloads))
		}
	})

	t.Run("scan pod created within IstiodConfigReadDelay window", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		configMapTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
		// Pod created 5s after ConfigMap; with 10s delay, effectiveLastModified is 10:00:10, so we still scan
		podTime := time.Date(2024, 1, 15, 10, 0, 5, 0, time.UTC)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"istio-injection": "enabled"}}},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-1", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-1",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-1", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "pod-in-window",
						Namespace:         "default",
						CreationTimestamp: metav1.NewTime(podTime),
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-1", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		// Pod has old image, created 5s after ConfigMap; with 10s IstiodConfigReadDelay we still scan it
		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		lastModifiedByRevision := map[string]time.Time{"default": configMapTime}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), lastModifiedByRevision, map[string]string{}, map[string]time.Time{}, ScanOptions{
			IstiodConfigReadDelay: 10 * time.Second,
		})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload (pod in window should be scanned), got %d", len(workloads))
		}
	})

	t.Run("do not skip when tag MWC was modified after pod was created", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		configMapTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
		podTime := time.Date(2024, 1, 15, 10, 0, 5, 0, time.UTC)
		tagMWCTime := time.Date(2024, 1, 15, 10, 0, 15, 0, time.UTC) // Tag MWC modified after pod

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "default",
						Labels: map[string]string{"istio.io/rev": "stable"},
					},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-stable", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-stable",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-stable", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "pod-stable",
						Namespace:         "default",
						CreationTimestamp: metav1.NewTime(podTime),
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-stable", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		// Pod created after ConfigMap (1-29-0) -> ConfigMap skip would apply. But tag "stable" MWC was
		// modified after pod was created, so tag-to-revision mapping may have changed - must scan.
		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		lastModifiedByRevision := map[string]time.Time{"1-29-0": configMapTime}
		tagToRevision := map[string]string{"stable": "1-29-0"}
		lastModifiedByTag := map[string]time.Time{"stable": tagMWCTime}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), lastModifiedByRevision, tagToRevision, lastModifiedByTag, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload (tag MWC modified after pod - must scan), got %d", len(workloads))
		}
	})

	t.Run("flag workload when pod has no sidecar but webhook would inject", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"istio-injection": "enabled"}}},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-no-sidecar", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-no-sidecar",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-no-sidecar", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-no-sidecar",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-no-sidecar", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
					},
				},
			).
			Build()

		// Pod has no istio-proxy; webhook would inject
		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		lastModifiedByRevision := map[string]time.Time{}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), lastModifiedByRevision, map[string]string{}, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload (webhook would inject sidecar), got %d", len(workloads))
		}
		if len(workloads) > 0 && workloads[0].Name != "dep-no-sidecar" {
			t.Errorf("want dep-no-sidecar, got %s", workloads[0].Name)
		}
	})
}

func TestScanOutdatedPods_skipNamespaces(t *testing.T) {
	t.Run("pods in skipped namespaces are not scanned", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"istio-injection": "enabled"}}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
				// Pod in default with outdated image
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-default",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-default", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-default",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-default", Controller: ptr(true)},
						},
					},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-default", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "default"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "default"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				// Pod in kube-system with outdated image (should be skipped)
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-kube-system",
						Namespace: "kube-system",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-kube-system", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-kube-system",
						Namespace: "kube-system",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-kube-system", Controller: ptr(true)},
						},
					},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-kube-system", Namespace: "kube-system"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "kube"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "kube"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
			).
			Build()

		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		lastModifiedByRevision := map[string]time.Time{}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), lastModifiedByRevision, map[string]string{}, map[string]time.Time{}, ScanOptions{
			SkipNamespaces: []string{"kube-system"},
		})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload (only default, kube-system skipped), got %d", len(workloads))
		}
		if len(workloads) > 0 && workloads[0].Name != "dep-default" {
			t.Errorf("want dep-default, got %s", workloads[0].Name)
		}
	})
}

func TestScanOutdatedPods_limitToNamespaces(t *testing.T) {
	t.Run("LimitToNamespaces with multiple namespaces scans both", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns1", Labels: map[string]string{"istio-injection": "enabled"}}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns2", Labels: map[string]string{"istio-injection": "enabled"}}},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-ns1", Namespace: "ns1"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "a"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "a"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-ns1",
						Namespace: "ns1",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-ns1", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-ns1",
						Namespace: "ns1",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-ns1", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-ns2", Namespace: "ns2"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "b"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "b"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-ns2",
						Namespace: "ns2",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-ns2", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-ns2",
						Namespace: "ns2",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-ns2", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		workloads, err := scanner.ScanOutdatedPods(context.Background(), map[string]time.Time{}, map[string]string{}, map[string]time.Time{}, ScanOptions{
			LimitToNamespaces: []string{"ns1", "ns2"},
		})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 2 {
			t.Errorf("want 2 workloads (ns1 and ns2), got %d", len(workloads))
		}
		names := make(map[string]bool)
		for _, w := range workloads {
			names[w.Name] = true
		}
		if !names["dep-ns1"] || !names["dep-ns2"] {
			t.Errorf("want dep-ns1 and dep-ns2, got %v", workloads)
		}
	})
}

func TestScanOutdatedPods_StatefulSetWorkload(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sts-1", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test", "istio.io/rev": "default"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"istio-injection": "enabled"}}},
			sts,
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sts-1-0",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "StatefulSet", Name: "sts-1", Controller: ptr(true)},
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						{Name: "app", Image: "nginx"},
					},
				},
			},
		).
		Build()

	webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
	scanner := NewPodScanner(fakeClient, webhook)
	workloads, err := scanner.ScanOutdatedPods(context.Background(), map[string]time.Time{}, map[string]string{}, map[string]time.Time{}, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanOutdatedPods: %v", err)
	}
	if len(workloads) != 1 {
		t.Errorf("want 1 workload (StatefulSet), got %d", len(workloads))
	}
	if len(workloads) > 0 && (workloads[0].Name != "sts-1" || workloads[0].Kind != "StatefulSet") {
		t.Errorf("want sts-1 StatefulSet, got %s %s", workloads[0].Name, workloads[0].Kind)
	}
}

func TestScanOutdatedPods_DaemonSetWorkload(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds-1", Namespace: "default"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test", "istio.io/rev": "default"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"istio-injection": "enabled"}}},
			ds,
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ds-1-xyz",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "DaemonSet", Name: "ds-1", Controller: ptr(true)},
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						{Name: "app", Image: "nginx"},
					},
				},
			},
		).
		Build()

	webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
	scanner := NewPodScanner(fakeClient, webhook)
	workloads, err := scanner.ScanOutdatedPods(context.Background(), map[string]time.Time{}, map[string]string{}, map[string]time.Time{}, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanOutdatedPods: %v", err)
	}
	if len(workloads) != 1 {
		t.Errorf("want 1 workload (DaemonSet), got %d", len(workloads))
	}
	if len(workloads) > 0 && (workloads[0].Name != "ds-1" || workloads[0].Kind != "DaemonSet") {
		t.Errorf("want ds-1 DaemonSet, got %s %s", workloads[0].Name, workloads[0].Kind)
	}
}

func ptr(b bool) *bool { return &b }

// recordingWebhookCaller records the revision passed to CallWebhook.
type recordingWebhookCaller struct {
	expectedProxyImage string
	calledWithRevision string
}

func (r *recordingWebhookCaller) CallWebhook(_ context.Context, pod *corev1.Pod, revision string, _ bool) (*corev1.Pod, error) {
	r.calledWithRevision = revision
	mutated := pod.DeepCopy()
	found := false
	for i := range mutated.Spec.Containers {
		if mutated.Spec.Containers[i].Name == istioProxyContainerName {
			mutated.Spec.Containers[i].Image = r.expectedProxyImage
			found = true
			break
		}
	}
	if !found && r.expectedProxyImage != "" {
		mutated.Spec.Containers = append(mutated.Spec.Containers, corev1.Container{
			Name:  istioProxyContainerName,
			Image: r.expectedProxyImage,
		})
	}
	return mutated, nil
}

func TestScanOutdatedPods_tagResolution(t *testing.T) {
	t.Run("workload template has istio.io/rev tag, tagToRevision resolves to revision", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		webhook := &recordingWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-canary", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{"app": "test", "istio.io/rev": "canary"},
							},
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-canary",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-canary", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-canary",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-canary", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		scanner := NewPodScanner(fakeClient, webhook)
		tagToRevision := map[string]string{"canary": "1-20"}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), map[string]time.Time{}, tagToRevision, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload, got %d", len(workloads))
		}
		if webhook.calledWithRevision != "1-20" { //nolint:goconst
			t.Errorf("webhook called with revision %q, want 1-20 (resolved from tag canary)", webhook.calledWithRevision)
		}
	})

	t.Run("workload template missing istio.io/rev, namespace has canary, tagToRevision resolves", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		webhook := &recordingWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "default",
						Labels: map[string]string{"istio.io/rev": "canary"},
					},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-ns", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-ns",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-ns", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-ns",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-ns", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		scanner := NewPodScanner(fakeClient, webhook)
		tagToRevision := map[string]string{"canary": "1-20"}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), map[string]time.Time{}, tagToRevision, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload, got %d", len(workloads))
		}
		if webhook.calledWithRevision != "1-20" {
			t.Errorf("webhook called with revision %q, want 1-20 (namespace fallback, resolved from tag canary)", webhook.calledWithRevision)
		}
	})
}

func TestScanOutdatedPods_istioInjectionLabel(t *testing.T) {
	t.Run("namespace has istio-injection=enabled only, pod is scanned with revision default", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		webhook := &recordingWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "default",
						Labels: map[string]string{"istio-injection": "enabled"},
					},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-injection", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-injection",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-injection", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-injection",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-injection", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		scanner := NewPodScanner(fakeClient, webhook)
		workloads, err := scanner.ScanOutdatedPods(context.Background(), map[string]time.Time{}, map[string]string{}, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload (istio-injection=enabled), got %d", len(workloads))
		}
		if webhook.calledWithRevision != "default" { //nolint:goconst
			t.Errorf("webhook called with revision %q, want default (from istio-injection=enabled)", webhook.calledWithRevision)
		}
	})

	t.Run("namespace has neither istio.io/rev nor istio-injection=enabled, pod is skipped", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "no-injection"}},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-skip", Namespace: "no-injection"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-skip",
						Namespace: "no-injection",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-skip", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-skip",
						Namespace: "no-injection",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-skip", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		webhook := &fakeWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		scanner := NewPodScanner(fakeClient, webhook)
		workloads, err := scanner.ScanOutdatedPods(context.Background(), map[string]time.Time{}, map[string]string{}, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 0 {
			t.Errorf("want 0 workloads (namespace has no injection labels), got %d", len(workloads))
		}
	})

	t.Run("istio.io/rev preferred over istio-injection when both present on namespace", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = corev1.AddToScheme(scheme)
		_ = appsv1.AddToScheme(scheme)

		webhook := &recordingWebhookCaller{expectedProxyImage: "docker.io/istio/proxyv2:1.20.1"}
		fakeClient := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(
				&corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "default",
						Labels: map[string]string{"istio.io/rev": "canary", "istio-injection": "enabled"},
					},
				},
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "dep-prefer", Namespace: "default"},
					Spec: appsv1.DeploymentSpec{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
							Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
						},
					},
				},
				&appsv1.ReplicaSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "rs-prefer",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "Deployment", Name: "dep-prefer", Controller: ptr(true)},
						},
					},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-prefer",
						Namespace: "default",
						OwnerReferences: []metav1.OwnerReference{
							{Kind: "ReplicaSet", Name: "rs-prefer", Controller: ptr(true)},
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: istioProxyContainerName, Image: "docker.io/istio/proxyv2:1.19.0"},
						},
					},
				},
			).
			Build()

		scanner := NewPodScanner(fakeClient, webhook)
		tagToRevision := map[string]string{"canary": "1-20"}
		workloads, err := scanner.ScanOutdatedPods(context.Background(), map[string]time.Time{}, tagToRevision, map[string]time.Time{}, ScanOptions{})
		if err != nil {
			t.Fatalf("ScanOutdatedPods: %v", err)
		}
		if len(workloads) != 1 {
			t.Errorf("want 1 workload, got %d", len(workloads))
		}
		if webhook.calledWithRevision != "1-20" {
			t.Errorf("webhook called with revision %q, want 1-20 (istio.io/rev preferred over istio-injection)", webhook.calledWithRevision)
		}
	})
}
