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
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	istioSystemNamespace        = "istio-system"
	injectPath                  = "/inject"
	injectPort                  = 443
	webhookConfigPrefix         = "istio-sidecar-injector"
	defaultTagWebhookConfigName = "istio-revision-tag-default"
)

// WebhookCaller calls the Istio injection webhook to get the expected mutated pod.
type WebhookCaller interface {
	CallWebhook(ctx context.Context, pod *corev1.Pod, revision string, preferDefaultTagWebhook bool) (*corev1.Pod, error)
}

// WebhookClient calls the Istio sidecar injection webhook to get the expected mutated pod.
type WebhookClient struct {
	k8sClient client.Client
}

// NewWebhookClient creates a WebhookClient. caBundle is read from MutatingWebhookConfiguration.
func NewWebhookClient(k8sClient client.Client) *WebhookClient {
	return &WebhookClient{k8sClient: k8sClient}
}

// urlFromClientConfig extracts the webhook URL from WebhookClientConfig.
// Supports both URL (direct) and Service (in-cluster) references.
func urlFromClientConfig(cfg admissionregv1.WebhookClientConfig) (string, error) {
	if cfg.URL != nil && *cfg.URL != "" {
		return *cfg.URL, nil
	}
	if cfg.Service == nil {
		return "", fmt.Errorf("WebhookClientConfig has neither URL nor Service")
	}
	svc := cfg.Service
	port := int32(injectPort)
	if svc.Port != nil {
		port = *svc.Port
	}
	path := injectPath
	if svc.Path != nil && *svc.Path != "" {
		path = *svc.Path
		if path[0] != '/' {
			path = "/" + path
		}
	}
	ns := istioSystemNamespace
	if svc.Namespace != "" {
		ns = svc.Namespace
	}
	return fmt.Sprintf("https://%s.%s.svc:%d%s", svc.Name, ns, port, path), nil
}

// getWebhookURLAndCABundle returns the webhook URL and caBundle for the given revision.
// When preferDefaultTagWebhook is true, first tries istio-revision-tag-default; if it exists,
// uses its ClientConfig. Otherwise falls back to istio-sidecar-injector (or -revision) and istiod.
func (w *WebhookClient) getWebhookURLAndCABundle(ctx context.Context, revision string, preferDefaultTagWebhook bool) (url string, caBundle []byte, err error) {
	if preferDefaultTagWebhook {
		var mwc admissionregv1.MutatingWebhookConfiguration
		if err := w.k8sClient.Get(ctx, types.NamespacedName{Name: defaultTagWebhookConfigName}, &mwc); err == nil && len(mwc.Webhooks) > 0 {
			cfg := mwc.Webhooks[0].ClientConfig
			url, urlErr := urlFromClientConfig(cfg)
			if urlErr != nil {
				return "", nil, fmt.Errorf("extract URL from %s: %w", defaultTagWebhookConfigName, urlErr)
			}
			caBundle = cfg.CABundle
			if len(caBundle) == 0 {
				return "", nil, fmt.Errorf("MutatingWebhookConfiguration %s has no caBundle", defaultTagWebhookConfigName)
			}
			return url, caBundle, nil
		}
	}

	caBundle, err = w.getCABundleForRevision(ctx, revision)
	if err != nil {
		return "", nil, err
	}
	return getWebhookURL(revision), caBundle, nil
}

// getCABundleForRevision returns the caBundle from the MutatingWebhookConfiguration
// for the given Istio revision. Config name: istio-sidecar-injector (default) or istio-sidecar-injector-<revision>.
func (w *WebhookClient) getCABundleForRevision(ctx context.Context, revision string) ([]byte, error) {
	configName := webhookConfigPrefix
	if revision != "" && revision != "default" {
		configName = webhookConfigPrefix + "-" + revision
	}
	var mwc admissionregv1.MutatingWebhookConfiguration
	if err := w.k8sClient.Get(ctx, types.NamespacedName{Name: configName}, &mwc); err != nil {
		return nil, fmt.Errorf("get MutatingWebhookConfiguration %s: %w", configName, err)
	}
	if len(mwc.Webhooks) == 0 {
		return nil, fmt.Errorf("MutatingWebhookConfiguration %s has no webhooks", configName)
	}
	caBundle := mwc.Webhooks[0].ClientConfig.CABundle
	if len(caBundle) == 0 {
		return nil, fmt.Errorf("MutatingWebhookConfiguration %s has no caBundle", configName)
	}
	return caBundle, nil
}

// transportForCABundle returns an http.Transport configured with the given caBundle for TLS verification.
func transportForCABundle(caBundle []byte) (*http.Transport, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundle) {
		return nil, fmt.Errorf("failed to parse caBundle")
	}
	return &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:            pool,
			InsecureSkipVerify: false,
		},
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second,
	}, nil
}

// getWebhookURL returns the Istio inject webhook URL for the given revision.
// For default revision, uses istiod.istio-system.svc; for others, istiod-<revision>.istio-system.svc.
func getWebhookURL(revision string) string {
	svcName := "istiod"
	if revision != "" && revision != "default" {
		svcName = "istiod-" + revision
	}
	// Use full FQDN for in-cluster DNS resolution
	return fmt.Sprintf("https://%s.%s.svc:%d%s", svcName, istioSystemNamespace, injectPort, injectPath)
}

// CallWebhook sends the pod to the Istio injection webhook and returns the mutated pod.
// The pod should be built from the workload's pod template (Deployment/StatefulSet/DaemonSet).
// When preferDefaultTagWebhook is true, uses istio-revision-tag-default if it exists; otherwise uses istiod.svc.
func (w *WebhookClient) CallWebhook(ctx context.Context, pod *corev1.Pod, revision string, preferDefaultTagWebhook bool) (*corev1.Pod, error) {
	url, caBundle, err := w.getWebhookURLAndCABundle(ctx, revision, preferDefaultTagWebhook)
	if err != nil {
		return nil, fmt.Errorf("get webhook URL and caBundle: %w", err)
	}
	transport, err := transportForCABundle(caBundle)
	if err != nil {
		return nil, fmt.Errorf("create transport: %w", err)
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	podRaw, err := json.Marshal(pod)
	if err != nil {
		return nil, fmt.Errorf("marshal pod: %w", err)
	}

	review := &admissionv1.AdmissionReview{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AdmissionReview",
			APIVersion: "admission.k8s.io/v1",
		},
		Request: &admissionv1.AdmissionRequest{
			UID:       types.UID("fortsa2-webhook-check"),
			Kind:      metav1.GroupVersionKind{Group: "", Version: "v1", Kind: "Pod"},
			Resource:  metav1.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"},
			Namespace: pod.Namespace,
			Operation: admissionv1.Create,
			Object: runtime.RawExtension{
				Raw: podRaw,
			},
		},
	}

	reviewBytes, err := json.Marshal(review)
	if err != nil {
		return nil, fmt.Errorf("marshal admission review: %w", err)
	}

	log.FromContext(ctx).V(1).Info("webhook call start", "url", url)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reviewBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webhook request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}

	var reviewResp admissionv1.AdmissionReview
	if err := json.NewDecoder(resp.Body).Decode(&reviewResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if reviewResp.Response == nil {
		return nil, fmt.Errorf("webhook response has no response body")
	}
	if !reviewResp.Response.Allowed {
		return nil, fmt.Errorf("webhook denied request: %s", reviewResp.Response.Result.Message)
	}

	// Apply JSON patch to get mutated pod
	if len(reviewResp.Response.Patch) == 0 {
		// No patch means no injection (e.g. namespace not enabled)
		return pod, nil
	}

	patchedRaw, err := applyJSONPatch(podRaw, reviewResp.Response.Patch)
	if err != nil {
		return nil, fmt.Errorf("apply patch: %w", err)
	}

	var mutated corev1.Pod
	if err := json.Unmarshal(patchedRaw, &mutated); err != nil {
		return nil, fmt.Errorf("unmarshal mutated pod: %w", err)
	}

	return &mutated, nil
}

func applyJSONPatch(original, patch []byte) ([]byte, error) {
	decoded, err := jsonpatch.DecodePatch(patch)
	if err != nil {
		return nil, fmt.Errorf("decode patch: %w", err)
	}
	patched, err := decoded.Apply(original)
	if err != nil {
		return nil, fmt.Errorf("apply patch: %w", err)
	}
	return patched, nil
}
