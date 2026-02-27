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

package mwc

import (
	"context"
	"testing"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestFetchTagToRevision(t *testing.T) {
	scheme := newTestScheme()
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

	got, err := FetchTagToRevision(context.Background(), fakeClient)
	if err != nil {
		t.Fatalf("FetchTagToRevision: %v", err)
	}
	if got["canary"] != "1-20" {
		t.Errorf("FetchTagToRevision: want canary->1-20, got %v", got)
	}
}

func TestReconcileRequest(t *testing.T) {
	req := ReconcileRequest()
	if req.Namespace != "istio-system" {
		t.Errorf("ReconcileRequest namespace = %q, want istio-system", req.Namespace)
	}
	if req.Name != "__mwc_reconcile__" {
		t.Errorf("ReconcileRequest name = %q, want __mwc_reconcile__", req.Name)
	}
}

func TestFilter(t *testing.T) {
	filter := Filter()
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
				t.Errorf("Filter() = %v, want %v", got, tt.wantPass)
			}
		})
	}
}

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return scheme
}
