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

package annotator

import (
	"context"
	"testing"

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

	annotator := NewWorkloadAnnotator(fakeClient)
	ref := podscanner.WorkloadRef{
		NamespacedName: types.NamespacedName{Namespace: dep.Namespace, Name: dep.Name},
		Kind:           "Deployment",
	}

	if err := annotator.Annotate(context.Background(), ref); err != nil {
		t.Fatalf("Annotate: %v", err)
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
