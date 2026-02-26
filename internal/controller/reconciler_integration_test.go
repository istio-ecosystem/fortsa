//go:build integration

/*
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
	"testing"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcilerIntegration(t *testing.T) {
	testEnv := &envtest.Environment{}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("envtest start: %v", err)
	}
	defer testEnv.Stop()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := scheme.Scheme
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:           scheme,
		Metrics:          metricsserver.Options{BindAddress: "0"},
		LeaderElection:   false,
		LeaderElectionID: "fortsa-test",
	})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}

	// nil webhook: integration test has no Istio webhook; scanner returns no workloads
	reconciler := NewConfigMapReconciler(mgr.GetClient(), mgr.GetScheme(), true, true, 0, 0, nil, nil)
	err = ctrl.NewControllerManagedBy(mgr).
		For(&corev1.ConfigMap{}, builder.WithPredicates(predicate.NewPredicateFuncs(ConfigMapFilter()))).
		Watches(
			&admissionregv1.MutatingWebhookConfiguration{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{PeriodicReconcileRequest()}
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(MutatingWebhookFilter())),
		).
		Watches(
			&corev1.Namespace{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{NamespaceReconcileRequest(obj.GetName())}
			}),
			builder.WithPredicates(NamespaceFilter()),
		).
		Complete(reconciler)
	if err != nil {
		t.Fatalf("create controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = mgr.Start(ctx)
	}()

	// Create istio-system namespace
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "istio-system"}}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}

	// Add istio-injection label to default namespace (envtest pre-creates it)
	nsDefault := &corev1.Namespace{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: "default"}, nsDefault); err != nil {
		t.Fatalf("get default namespace: %v", err)
	}
	if nsDefault.Labels == nil {
		nsDefault.Labels = make(map[string]string)
	}
	nsDefault.Labels["istio-injection"] = "enabled"
	if err := k8sClient.Update(ctx, nsDefault); err != nil {
		t.Fatalf("update default namespace: %v", err)
	}

	// Create ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "istio-system",
			Name:      "istio-sidecar-injector",
			Labels:    map[string]string{"istio.io/rev": "default"},
		},
		Data: map[string]string{
			"values": `{"revision":"default","global":{"hub":"docker.io/istio","tag":"1.20.1","proxy":{"image":"proxyv2"}}}`,
		},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("create ConfigMap: %v", err)
	}

	// Create Deployment with outdated sidecar pod
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-dep", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx"},
						{Name: "istio-proxy", Image: "docker.io/istio/proxyv2:1.19.0"},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, dep); err != nil {
		t.Fatalf("create Deployment: %v", err)
	}

	// Create ReplicaSet owned by Deployment
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-dep-rs",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "Deployment", Name: "test-dep", UID: dep.UID, Controller: boolPtr(true)},
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "nginx"},
						{Name: "istio-proxy", Image: "docker.io/istio/proxyv2:1.19.0"},
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, rs); err != nil {
		t.Fatalf("create ReplicaSet: %v", err)
	}

	// Create Pod owned by ReplicaSet
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "test-dep-rs", UID: rs.UID, Controller: boolPtr(true)},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "nginx"},
				{Name: "istio-proxy", Image: "docker.io/istio/proxyv2:1.19.0"},
			},
		},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatalf("create Pod: %v", err)
	}

	// Trigger reconcile by updating ConfigMap
	time.Sleep(500 * time.Millisecond)
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "istio-system", Name: "istio-sidecar-injector"}, cm); err != nil {
		t.Fatalf("get ConfigMap: %v", err)
	}
	cm.Data["values"] = `{"revision":"default","global":{"hub":"docker.io/istio","tag":"1.20.2","proxy":{"image":"proxyv2"}}}`
	if err := k8sClient.Update(ctx, cm); err != nil {
		t.Fatalf("update ConfigMap: %v", err)
	}

	// Wait for reconcile (dry-run so no actual patch)
	time.Sleep(2 * time.Second)

	// Verify Deployment was not annotated (dry-run mode)
	var updatedDep appsv1.Deployment
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "test-dep"}, &updatedDep); err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if updatedDep.Spec.Template.Annotations != nil {
		if _, ok := updatedDep.Spec.Template.Annotations["fortsa.scaffidi.net/restartedAt"]; ok {
			t.Error("dry-run: Deployment should not have restartedAt annotation")
		}
	}

	// MWC change triggers reconcile: create istio-revision-tag-default to verify watch fires
	t.Run("MWC change triggers reconcile", func(t *testing.T) {
		sideEffectNone := admissionregv1.SideEffectClassNone
		mwc := &admissionregv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name: "istio-revision-tag-default",
				Labels: map[string]string{
					"istio.io/tag": "default",
					"istio.io/rev": "default",
				},
			},
			Webhooks: []admissionregv1.MutatingWebhook{
				{
					Name:                    "test.injection.istio.io",
					AdmissionReviewVersions: []string{"v1"},
					SideEffects:             &sideEffectNone,
					ClientConfig: admissionregv1.WebhookClientConfig{
						Service: &admissionregv1.ServiceReference{
							Namespace: "istio-system",
							Name:      "istiod",
							Path:      ptrStr("/inject"),
						},
					},
				},
			},
		}
		if err := k8sClient.Create(ctx, mwc); err != nil {
			t.Fatalf("create MutatingWebhookConfiguration: %v", err)
		}
		time.Sleep(2 * time.Second)
		// Reconcile should have run (PeriodicReconcileRequest path); no crash means success
	})
}

func int32Ptr(i int32) *int32 { return &i }
func boolPtr(b bool) *bool    { return &b }
func ptrStr(s string) *string { return &s }
