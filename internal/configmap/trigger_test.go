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

	"github.com/istio-ecosystem/fortsa/internal/constants"
)

func TestReconcileRequest(t *testing.T) {
	req := ReconcileRequest()
	if req.Namespace != constants.IstioSystemNamespace {
		t.Errorf("ReconcileRequest namespace = %q, want %q", req.Namespace, constants.IstioSystemNamespace)
	}
	if req.Name != constants.ReconcileTriggerNameIstioChange {
		t.Errorf("ReconcileRequest name = %q, want %q", req.Name, constants.ReconcileTriggerNameIstioChange)
	}
}

func TestReconcileRequestName(t *testing.T) {
	name := ReconcileRequestName()
	if name != constants.ReconcileTriggerNameIstioChange {
		t.Errorf("ReconcileRequestName = %q, want %q", name, constants.ReconcileTriggerNameIstioChange)
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
					Namespace: constants.IstioSystemNamespace,
					Name:      constants.ConfigMapNamePrefix,
				},
			},
			wantPass: true,
		},
		{
			name: "istio-sidecar-injector-canary in istio-system",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: constants.IstioSystemNamespace,
					Name:      constants.ConfigMapNamePrefix + "-canary",
				},
			},
			wantPass: true,
		},
		{
			name: "istio-sidecar-injector in default namespace",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
					Name:      constants.ConfigMapNamePrefix,
				},
			},
			wantPass: false,
		},
		{
			name: "other-configmap in istio-system",
			obj: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: constants.IstioSystemNamespace,
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
