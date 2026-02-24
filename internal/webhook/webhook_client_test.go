package webhook

import (
	"context"
	"testing"

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

func ptrStr(s string) *string { return &s }
func ptrInt32(n int32) *int32 { return &n }
