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
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/istio-ecosystem/fortsa/internal/podscanner"
)

func TestWorkloadAnnotator_Annotate(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-dep", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"existing": "value"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep).
		Build()

	annotator := NewWorkloadAnnotator(fakeClient, 0)
	ref := podscanner.WorkloadRef{
		NamespacedName: types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name},
		Kind:           "Deployment",
	}

	annotated, err := annotator.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if !annotated {
		t.Error("Annotate: want true (patch applied), got false")
	}

	var updated appsv1.Deployment
	if err := fakeClient.Get(context.Background(), ref.NamespacedName, &updated); err != nil {
		t.Fatalf("Get Deployment: %v", err)
	}
	if updated.Spec.Template.Annotations == nil {
		t.Fatal("expected annotations on pod template")
	}
	if _, ok := updated.Spec.Template.Annotations["fortsa.scaffidi.net/restartedAt"]; !ok {
		t.Error("expected fortsa.scaffidi.net/restartedAt annotation")
	}
	if updated.Spec.Template.Annotations["existing"] != "value" {
		t.Error("existing annotation should be preserved")
	}
}

func TestWorkloadAnnotator_Annotate_cooldownSkipsReannotation(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-dep", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep).
		Build()

	// 5m cooldown: second annotate within cooldown should no-op
	annotator := NewWorkloadAnnotator(fakeClient, 5*time.Minute)
	ref := podscanner.WorkloadRef{
		NamespacedName: types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name},
		Kind:           "Deployment",
	}

	// First annotate
	annotated, err := annotator.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Annotate: %v", err)
	}
	if !annotated {
		t.Error("first Annotate: want true (patch applied), got false")
	}

	var afterFirst appsv1.Deployment
	if err := fakeClient.Get(context.Background(), ref.NamespacedName, &afterFirst); err != nil {
		t.Fatalf("Get Deployment: %v", err)
	}
	firstValue := afterFirst.Spec.Template.Annotations[RestartedAtAnnotation]
	if firstValue == "" {
		t.Fatal("expected restartedAt after first annotate")
	}

	// Second annotate immediately (within cooldown) - should no-op, return (false, nil)
	annotated, err = annotator.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("second Annotate: %v", err)
	}
	if annotated {
		t.Error("second Annotate: want false (skipped due to cooldown), got true")
	}

	var afterSecond appsv1.Deployment
	if err := fakeClient.Get(context.Background(), ref.NamespacedName, &afterSecond); err != nil {
		t.Fatalf("Get Deployment: %v", err)
	}
	secondValue := afterSecond.Spec.Template.Annotations[RestartedAtAnnotation]
	if secondValue != firstValue {
		t.Errorf("cooldown should skip re-annotation: restartedAt changed from %q to %q", firstValue, secondValue)
	}
}

func TestWorkloadAnnotator_Annotate_StatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sts", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sts).
		Build()

	a := NewWorkloadAnnotator(fakeClient, 0)
	ref := podscanner.WorkloadRef{
		NamespacedName: types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name},
		Kind:           "StatefulSet",
	}

	annotated, err := a.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if !annotated {
		t.Error("Annotate: want true (patch applied), got false")
	}

	var updated appsv1.StatefulSet
	if err := fakeClient.Get(context.Background(), ref.NamespacedName, &updated); err != nil {
		t.Fatalf("Get StatefulSet: %v", err)
	}
	if _, ok := updated.Spec.Template.Annotations[RestartedAtAnnotation]; !ok {
		t.Error("expected fortsa.scaffidi.net/restartedAt annotation")
	}
}

func TestWorkloadAnnotator_Annotate_DaemonSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ds", Namespace: "default"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ds).
		Build()

	a := NewWorkloadAnnotator(fakeClient, 0)
	ref := podscanner.WorkloadRef{
		NamespacedName: types.NamespacedName{Namespace: ds.Namespace, Name: ds.Name},
		Kind:           "DaemonSet",
	}

	annotated, err := a.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if !annotated {
		t.Error("Annotate: want true (patch applied), got false")
	}

	var updated appsv1.DaemonSet
	if err := fakeClient.Get(context.Background(), ref.NamespacedName, &updated); err != nil {
		t.Fatalf("Get DaemonSet: %v", err)
	}
	if _, ok := updated.Spec.Template.Annotations[RestartedAtAnnotation]; !ok {
		t.Error("expected fortsa.scaffidi.net/restartedAt annotation")
	}
}

func TestWorkloadAnnotator_Annotate_unsupportedKind(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	a := NewWorkloadAnnotator(fakeClient, 0)
	ref := podscanner.WorkloadRef{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "cronjob-1"},
		Kind:           "CronJob",
	}

	annotated, err := a.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if annotated {
		t.Error("Annotate with unsupported kind: want false, got true")
	}
}

func TestWorkloadAnnotator_getRestartedAt_cooldownWithStatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sts", Namespace: "default"},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "test"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "test"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sts).
		Build()

	a := NewWorkloadAnnotator(fakeClient, 5*time.Minute)
	ref := podscanner.WorkloadRef{
		NamespacedName: types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name},
		Kind:           "StatefulSet",
	}

	annotated, err := a.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("first Annotate: %v", err)
	}
	if !annotated {
		t.Error("first Annotate: want true, got false")
	}

	annotated, err = a.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("second Annotate: %v", err)
	}
	if annotated {
		t.Error("second Annotate within cooldown: want false (skipped), got true")
	}
}

func TestWorkloadAnnotator_getRestartedAt_unparseableTimestamp(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "test-dep", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{RestartedAtAnnotation: "not-a-valid-timestamp"},
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep).
		Build()

	a := NewWorkloadAnnotator(fakeClient, 5*time.Minute)
	ref := podscanner.WorkloadRef{
		NamespacedName: types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name},
		Kind:           "Deployment",
	}

	// Unparseable timestamp: getRestartedAt returns zero, so we don't skip - we annotate
	annotated, err := a.Annotate(context.Background(), ref)
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}
	if !annotated {
		t.Error("Annotate with unparseable restartedAt: want true (treat as no annotation), got false")
	}
}
