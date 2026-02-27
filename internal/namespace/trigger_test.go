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

package namespace

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestNamespaceFilter(t *testing.T) {
	filter := Filter()

	tests := []struct {
		name     string
		event    event.CreateEvent
		wantPass bool
	}{
		{
			name: "namespace with istio.io/rev",
			event: event.CreateEvent{
				Object: &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "test",
						Labels: map[string]string{"istio.io/rev": "default"},
					},
				},
			},
			wantPass: true,
		},
		{
			name: "namespace with istio-injection enabled",
			event: event.CreateEvent{
				Object: &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "test",
						Labels: map[string]string{"istio-injection": "enabled"},
					},
				},
			},
			wantPass: true,
		},
		{
			name: "namespace without Istio labels",
			event: event.CreateEvent{
				Object: &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "test",
						Labels: map[string]string{"app": "foo"},
					},
				},
			},
			wantPass: false,
		},
		{
			name: "namespace with empty istio-injection",
			event: event.CreateEvent{
				Object: &corev1.Namespace{
					ObjectMeta: metav1.ObjectMeta{
						Name:   "test",
						Labels: map[string]string{"istio-injection": ""},
					},
				},
			},
			wantPass: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := filter.Create(tt.event); got != tt.wantPass {
				t.Errorf("Filter().Create() = %v, want %v", got, tt.wantPass)
			}
		})
	}
}

func TestNamespaceReconcileRequest(t *testing.T) {
	req := ReconcileRequest("my-namespace")
	if req.Namespace != "" {
		t.Errorf("ReconcileRequest namespace = %q, want empty", req.Namespace)
	}
	if req.Name != "my-namespace" {
		t.Errorf("ReconcileRequest name = %q, want my-namespace", req.Name)
	}
}
