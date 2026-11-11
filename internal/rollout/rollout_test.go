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

package rollout

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	scheme.AddKnownTypeWithName(GVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(GVK.GroupVersion().WithKind("RolloutList"), &unstructured.UnstructuredList{})
	return scheme
}

func testRollout(name string, spec map[string]interface{}) *unstructured.Unstructured {
	ro := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": GVK.GroupVersion().String(),
		"kind":       Kind,
		"metadata": map[string]interface{}{
			"name":      name,
			"namespace": "default",
		},
		"spec": spec,
	}}
	ro.SetGroupVersionKind(GVK)
	return ro
}

func testDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"istio.io/rev": "stable"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
			},
		},
	}
}

func TestExtractPodTemplateSpec(t *testing.T) {
	deployment := testDeployment("workload")
	tests := []struct {
		name      string
		rollout   *unstructured.Unstructured
		objects   []client.Object
		wantLabel string
		wantErr   bool
	}{
		{
			name: "inline template",
			rollout: testRollout("inline", map[string]interface{}{
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{"labels": map[string]interface{}{"istio.io/rev": "canary"}},
					"spec":     map[string]interface{}{"containers": []interface{}{map[string]interface{}{"name": "app", "image": "nginx"}}},
				},
			}),
			wantLabel: "canary",
		},
		{
			name: "workloadRef resolves Deployment and ignores empty template",
			rollout: testRollout("ref", map[string]interface{}{
				"template": map[string]interface{}{},
				"workloadRef": map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"name":       "workload",
				},
			}),
			objects:   []client.Object{deployment},
			wantLabel: "stable",
		},
		{
			name: "unsupported workloadRef kind",
			rollout: testRollout("unsupported", map[string]interface{}{
				"workloadRef": map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "StatefulSet",
					"name":       "workload",
				},
			}),
			wantErr: true,
		},
		{
			name: "missing Deployment",
			rollout: testRollout("missing", map[string]interface{}{
				"workloadRef": map[string]interface{}{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"name":       "missing",
				},
			}),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := append([]client.Object{tt.rollout}, tt.objects...)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
			template, err := ExtractPodTemplateSpec(context.Background(), c, tt.rollout)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ExtractPodTemplateSpec error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if template.Labels["istio.io/rev"] != tt.wantLabel {
				t.Errorf("resolved revision = %q, want %q", template.Labels["istio.io/rev"], tt.wantLabel)
			}
		})
	}
}

func TestGetRestartAt(t *testing.T) {
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name string
		ro   *unstructured.Unstructured
		want time.Time
	}{
		{
			name: "present",
			ro:   testRollout("present", map[string]interface{}{"restartAt": want.Format(time.RFC3339)}),
			want: want,
		},
		{
			name: "absent",
			ro:   testRollout("absent", map[string]interface{}{}),
		},
		{
			name: "unparseable",
			ro:   testRollout("invalid", map[string]interface{}{"restartAt": "not-a-timestamp"}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GetRestartAt(tt.ro)
			if err != nil {
				t.Fatalf("GetRestartAt: %v", err)
			}
			if !got.Equal(tt.want) {
				t.Errorf("GetRestartAt = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildRestartAtPatch(t *testing.T) {
	got, err := BuildRestartAtPatch(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildRestartAtPatch: %v", err)
	}
	want := `{"spec":{"restartAt":"2026-01-02T03:04:05Z"}}`
	if string(got) != want {
		t.Errorf("BuildRestartAtPatch = %s, want %s", got, want)
	}
}

type noMatchClient struct {
	client.Client
}

func (c *noMatchClient) Get(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	return &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Group: Group, Resource: "rollouts"}}
}

func TestGet(t *testing.T) {
	rollout := testRollout("found", map[string]interface{}{})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(rollout).Build()

	found, err := Get(context.Background(), c, types.NamespacedName{Namespace: "default", Name: "found"})
	if err != nil {
		t.Fatalf("Get found: %v", err)
	}
	if found == nil || found.GetName() != "found" {
		t.Fatalf("Get found = %#v, want Rollout found", found)
	}

	notFound, err := Get(context.Background(), c, types.NamespacedName{Namespace: "default", Name: "missing"})
	if err != nil {
		t.Fatalf("Get not found: %v", err)
	}
	if notFound != nil {
		t.Fatalf("Get not found = %#v, want nil", notFound)
	}

	noMatch, err := Get(context.Background(), &noMatchClient{Client: c}, types.NamespacedName{Namespace: "default", Name: "missing"})
	if err != nil {
		t.Fatalf("Get no kind match: %v", err)
	}
	if noMatch != nil {
		t.Fatalf("Get no kind match = %#v, want nil", noMatch)
	}
}
