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

package webhook

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestURLFromClientConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     admissionregv1.WebhookClientConfig
		wantURL string
		wantErr bool
	}{
		{
			name: "URL set",
			cfg: admissionregv1.WebhookClientConfig{
				URL: ptrStr("https://istiod.example.com:443/inject"),
			},
			wantURL: "https://istiod.example.com:443/inject",
			wantErr: false,
		},
		{
			name: "Service reference default port and path",
			cfg: admissionregv1.WebhookClientConfig{
				Service: &admissionregv1.ServiceReference{
					Name:      "istiod",
					Namespace: "istio-system",
				},
			},
			wantURL: "https://istiod.istio-system.svc:443/inject",
			wantErr: false,
		},
		{
			name: "Service reference with custom path and port",
			cfg: admissionregv1.WebhookClientConfig{
				Service: &admissionregv1.ServiceReference{
					Name:      "istiod-canary",
					Namespace: "istio-system",
					Path:      ptrStr("/inject"),
					Port:      ptrInt32(8443),
				},
			},
			wantURL: "https://istiod-canary.istio-system.svc:8443/inject",
			wantErr: false,
		},
		{
			name: "Service reference path without leading slash",
			cfg: admissionregv1.WebhookClientConfig{
				Service: &admissionregv1.ServiceReference{
					Name:      "istiod",
					Namespace: "istio-system",
					Path:      ptrStr("inject"),
				},
			},
			wantURL: "https://istiod.istio-system.svc:443/inject",
			wantErr: false,
		},
		{
			name:    "neither URL nor Service",
			cfg:     admissionregv1.WebhookClientConfig{},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := urlFromClientConfig(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("urlFromClientConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.wantURL {
				t.Errorf("urlFromClientConfig() = %v, want %v", got, tt.wantURL)
			}
		})
	}
}

func TestGetWebhookURLAndCABundle_PreferDefaultTag(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	caBundle := []byte("-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----")
	defaultTagMWC := &admissionregv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: defaultTagWebhookConfigName},
		Webhooks: []admissionregv1.MutatingWebhook{
			{
				ClientConfig: admissionregv1.WebhookClientConfig{
					Service: &admissionregv1.ServiceReference{
						Name:      "istiod-canary",
						Namespace: "istio-system",
						Path:      ptrStr("/inject"),
					},
					CABundle: caBundle,
				},
			},
		},
	}

	t.Run("preferDefaultTagWebhook true and istio-revision-tag-default exists", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(defaultTagMWC).Build()
		wc := NewWebhookClient(fakeClient)
		url, cb, err := wc.getWebhookURLAndCABundle(context.Background(), "default", true)
		if err != nil {
			t.Fatalf("getWebhookURLAndCABundle: %v", err)
		}
		if url != "https://istiod-canary.istio-system.svc:443/inject" {
			t.Errorf("url = %v, want https://istiod-canary.istio-system.svc:443/inject", url)
		}
		if len(cb) != len(caBundle) {
			t.Errorf("caBundle length = %d, want %d", len(cb), len(caBundle))
		}
	})

	t.Run("preferDefaultTagWebhook true but istio-revision-tag-default missing", func(t *testing.T) {
		// No default tag config; need istio-sidecar-injector for fallback
		legacyMWC := &admissionregv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: webhookConfigPrefix},
			Webhooks: []admissionregv1.MutatingWebhook{
				{
					ClientConfig: admissionregv1.WebhookClientConfig{
						CABundle: caBundle,
					},
				},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacyMWC).Build()
		wc := NewWebhookClient(fakeClient)
		url, cb, err := wc.getWebhookURLAndCABundle(context.Background(), "default", true)
		if err != nil {
			t.Fatalf("getWebhookURLAndCABundle: %v", err)
		}
		if url != "https://istiod.istio-system.svc:443/inject" {
			t.Errorf("url = %v, want https://istiod.istio-system.svc:443/inject (fallback)", url)
		}
		if len(cb) == 0 {
			t.Error("caBundle should not be empty")
		}
	})

	t.Run("preferDefaultTagWebhook false uses revision-based", func(t *testing.T) {
		legacyMWC := &admissionregv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: webhookConfigPrefix},
			Webhooks: []admissionregv1.MutatingWebhook{
				{
					ClientConfig: admissionregv1.WebhookClientConfig{
						CABundle: caBundle,
					},
				},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacyMWC, defaultTagMWC).Build()
		wc := NewWebhookClient(fakeClient)
		url, _, err := wc.getWebhookURLAndCABundle(context.Background(), "default", false)
		if err != nil {
			t.Fatalf("getWebhookURLAndCABundle: %v", err)
		}
		// preferDefaultTagWebhook false -> should use istio-sidecar-injector, not istio-revision-tag-default
		if url != "https://istiod.istio-system.svc:443/inject" {
			t.Errorf("url = %v, want https://istiod.istio-system.svc:443/inject", url)
		}
	})
}

func TestTransportForCABundle(t *testing.T) {
	t.Run("invalid PEM returns error", func(t *testing.T) {
		_, err := transportForCABundle([]byte("not-valid-pem"))
		if err == nil {
			t.Error("transportForCABundle with invalid PEM should return error")
		}
	})

	t.Run("valid PEM returns transport", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer server.Close()
		cert := server.Certificate()
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
		transport, err := transportForCABundle(certPEM)
		if err != nil {
			t.Fatalf("transportForCABundle: %v", err)
		}
		if transport == nil {
			t.Error("transport should not be nil")
		}
	})
}

func TestApplyJSONPatch(t *testing.T) {
	original := []byte(`{"spec":{"containers":[{"name":"app","image":"nginx"}]}}`)
	patch := []byte(`[{"op":"add","path":"/spec/containers/-","value":{"name":"istio-proxy","image":"istio/proxyv2:1.20.1"}}]`)
	patched, err := applyJSONPatch(original, patch)
	if err != nil {
		t.Fatalf("applyJSONPatch: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(patched, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	containers, _ := result["spec"].(map[string]interface{})["containers"].([]interface{})
	if len(containers) != 2 {
		t.Errorf("want 2 containers after patch, got %d", len(containers))
	}

	t.Run("invalid patch returns error", func(t *testing.T) {
		_, err := applyJSONPatch(original, []byte(`[{"op":"invalid"}]`))
		if err == nil {
			t.Error("applyJSONPatch with invalid patch should return error")
		}
	})
}

func TestCallWebhook(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = admissionregv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	t.Run("success with JSON patch", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var review admissionv1.AdmissionReview
			if err := json.NewDecoder(r.Body).Decode(&review); err != nil {
				t.Errorf("decode request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			// Return a patch that adds istio-proxy container
			patch := []byte(`[{"op":"add","path":"/spec/containers/-","value":{"name":"istio-proxy","image":"docker.io/istio/proxyv2:1.20.1"}}]`)
			resp := &admissionv1.AdmissionReview{
				TypeMeta: review.TypeMeta,
				Response: &admissionv1.AdmissionResponse{
					UID:     review.Request.UID,
					Allowed: true,
					Patch:   patch,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		urlStr := server.URL
		mwc := &admissionregv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: defaultTagWebhookConfigName},
			Webhooks: []admissionregv1.MutatingWebhook{
				{
					ClientConfig: admissionregv1.WebhookClientConfig{
						URL:      &urlStr,
						CABundle: certPEM,
					},
				},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mwc).Build()
		wc := NewWebhookClient(fakeClient)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "app", Image: "nginx"}},
			},
		}

		mutated, err := wc.CallWebhook(context.Background(), pod, "default", true)
		if err != nil {
			t.Fatalf("CallWebhook: %v", err)
		}
		if mutated == nil {
			t.Fatal("mutated pod should not be nil")
		}
		found := false
		for _, c := range mutated.Spec.Containers {
			if c.Name == "istio-proxy" && c.Image == "docker.io/istio/proxyv2:1.20.1" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected istio-proxy container in mutated pod")
		}
	})

	t.Run("success with no patch returns original pod", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var review admissionv1.AdmissionReview
			_ = json.NewDecoder(r.Body).Decode(&review)
			resp := &admissionv1.AdmissionReview{
				TypeMeta: review.TypeMeta,
				Response: &admissionv1.AdmissionResponse{
					UID:     review.Request.UID,
					Allowed: true,
					Patch:   nil,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		urlStr := server.URL
		mwc := &admissionregv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: defaultTagWebhookConfigName},
			Webhooks: []admissionregv1.MutatingWebhook{
				{
					ClientConfig: admissionregv1.WebhookClientConfig{
						URL:      &urlStr,
						CABundle: certPEM,
					},
				},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mwc).Build()
		wc := NewWebhookClient(fakeClient)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		}

		mutated, err := wc.CallWebhook(context.Background(), pod, "default", true)
		if err != nil {
			t.Fatalf("CallWebhook: %v", err)
		}
		if mutated.Name != pod.Name || mutated.Namespace != pod.Namespace {
			t.Errorf("no patch should return original pod, got %s/%s", mutated.Namespace, mutated.Name)
		}
	})

	t.Run("denied response returns error", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var review admissionv1.AdmissionReview
			_ = json.NewDecoder(r.Body).Decode(&review)
			resp := &admissionv1.AdmissionReview{
				TypeMeta: review.TypeMeta,
				Response: &admissionv1.AdmissionResponse{
					UID:     review.Request.UID,
					Allowed: false,
					Result:  &metav1.Status{Message: "denied for testing"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		}))
		defer server.Close()

		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		urlStr := server.URL
		mwc := &admissionregv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: defaultTagWebhookConfigName},
			Webhooks: []admissionregv1.MutatingWebhook{
				{
					ClientConfig: admissionregv1.WebhookClientConfig{
						URL:      &urlStr,
						CABundle: certPEM,
					},
				},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mwc).Build()
		wc := NewWebhookClient(fakeClient)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		}

		_, err := wc.CallWebhook(context.Background(), pod, "default", true)
		if err == nil {
			t.Error("CallWebhook with denied response should return error")
		}
	})

	t.Run("non-200 status returns error", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
		urlStr := server.URL
		mwc := &admissionregv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: defaultTagWebhookConfigName},
			Webhooks: []admissionregv1.MutatingWebhook{
				{
					ClientConfig: admissionregv1.WebhookClientConfig{
						URL:      &urlStr,
						CABundle: certPEM,
					},
				},
			},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mwc).Build()
		wc := NewWebhookClient(fakeClient)

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "nginx"}}},
		}

		_, err := wc.CallWebhook(context.Background(), pod, "default", true)
		if err == nil {
			t.Error("CallWebhook with 500 status should return error")
		}
	})
}

func TestGetWebhookURL(t *testing.T) {
	tests := []struct {
		revision string
		want     string
	}{
		{"default", "https://istiod.istio-system.svc:443/inject"},
		{"", "https://istiod.istio-system.svc:443/inject"},
		{"canary", "https://istiod-canary.istio-system.svc:443/inject"},
	}
	for _, tt := range tests {
		t.Run(tt.revision, func(t *testing.T) {
			got := getWebhookURL(tt.revision)
			if got != tt.want {
				t.Errorf("getWebhookURL(%q) = %q, want %q", tt.revision, got, tt.want)
			}
		})
	}
}

func ptrStr(s string) *string { return &s }
func ptrInt32(n int32) *int32 { return &n }
