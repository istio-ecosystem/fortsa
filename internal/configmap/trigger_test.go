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

package configmap

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReconcileRequest(t *testing.T) {
	req := ReconcileRequest()
	if req.Namespace != "istio-system" {
		t.Errorf("ReconcileRequest namespace = %q, want istio-system", req.Namespace)
	}
	if req.Name != "__configmap_change__" {
		t.Errorf("ReconcileRequest name = %q, want __configmap_change__", req.Name)
	}
}

func TestReconcileRequestName(t *testing.T) {
	name := ReconcileRequestName()
	if name != "__configmap_change__" {
		t.Errorf("ReconcileRequestName = %q, want __configmap_change__", name)
	}
}

func TestFilter(t *testing.T) {
	filter := Filter()
	tests := []struct {
		name     string
		obj      *corev1.ConfigMap
		wantPass bool
	}{
		{
			name: "istio-sidecar-injector in istio-system",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "istio-system",
					Name:      "istio-sidecar-injector",
				},
			},
			wantPass: true,
		},
		{
			name: "istio-sidecar-injector-canary in istio-system",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "istio-system",
					Name:      "istio-sidecar-injector-canary",
				},
			},
			wantPass: true,
		},
		{
			name: "istio-sidecar-injector in default namespace",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      "istio-sidecar-injector",
				},
			},
			wantPass: false,
		},
		{
			name: "other-configmap in istio-system",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "istio-system",
					Name:      "other-configmap",
				},
			},
			wantPass: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := filter(tt.obj); got != tt.wantPass {
				t.Errorf("Filter() = %v, want %v", got, tt.wantPass)
			}
		})
	}
}
