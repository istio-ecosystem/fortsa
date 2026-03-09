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

package podscanner

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/istio-ecosystem/fortsa/internal/constants"
	"github.com/istio-ecosystem/fortsa/internal/webhook"
)

const (
	istioProxyContainerName = "istio-proxy"
	dnsSubdomainMaxLen      = 63
	podNameSuffix           = "-fortsa-check"
)

// ParsedImage holds the components of a container image reference.
type ParsedImage struct {
	Registry  string // path before last /
	ImageName string // last path segment before : or @
	Tag       string // after :, or "latest" if absent
}

// ParseImage splits a container image string into registry, image_name, and tag.
// Handles: docker.io/istio/proxyv2:1.20.1, registry:5000/repo/image:tag, image@sha256:...
func ParseImage(image string) ParsedImage {
	p := ParsedImage{Tag: "latest"}

	// Handle digest: image@sha256:abc123 -> tag is the digest part for comparison
	atIdx := strings.Index(image, "@")
	if atIdx >= 0 {
		image = image[:atIdx]
	}

	colonIdx := strings.LastIndex(image, ":")
	if colonIdx >= 0 {
		p.Tag = image[colonIdx+1:]
		image = image[:colonIdx]
	}

	lastSlash := strings.LastIndex(image, "/")
	if lastSlash >= 0 {
		p.Registry = image[:lastSlash]
		p.ImageName = image[lastSlash+1:]
	} else {
		p.ImageName = image
	}

	return p
}

// imagesMatch returns true if current and expected images match.
// When compareHub is true, full string comparison (including registry) is used.
// When compareHub is false, only image name and tag are compared (registry is ignored).
func imagesMatch(current, expected string, compareHub bool) bool {
	if compareHub {
		return current == expected
	}
	cp := ParseImage(current)
	ep := ParseImage(expected)
	return cp.ImageName == ep.ImageName && cp.Tag == ep.Tag
}

// PodScanner lists Pods with Istio sidecars and finds their parent workloads.
type PodScanner struct {
	client        client.Client
	webhookCaller webhook.WebhookCaller
}

// newPodScanner creates a PodScanner with the given client and webhook caller.
// Used by tests to inject fake webhook callers; production code uses NewPodScanner.
func newPodScanner(c client.Client, webhookCaller webhook.WebhookCaller) *PodScanner {
	return &PodScanner{client: c, webhookCaller: webhookCaller}
}

// NewPodScanner creates a new PodScanner that uses the Istio injection webhook.
func NewPodScanner(c client.Client) *PodScanner {
	return newPodScanner(c, webhook.NewWebhookClient(c))
}

// WorkloadRef identifies a Deployment, StatefulSet, or DaemonSet.
type WorkloadRef struct {
	types.NamespacedName
	Kind string // "Deployment", "StatefulSet", or "DaemonSet"
}

// ScanOptions configures pod scanning behavior.
type ScanOptions struct {
	// CompareHub, when true, requires registry to match when comparing images.
	// When false, only image name and tag are compared (registry is ignored).
	CompareHub bool
	// IstiodConfigReadDelay is added to the effective lastModified (max of ConfigMap and MWC) when deciding whether to skip pods.
	// Pods created within this window after a config update may have been injected before
	// Istiod loaded the new config, so we still scan them.
	IstiodConfigReadDelay time.Duration
	// SkipNamespaces lists namespaces to skip when scanning pods.
	SkipNamespaces []string
	// LimitToNamespaces, when non-empty, restricts scanning to pods in these namespaces only.
	LimitToNamespaces []string
}

// IstioConfig holds Istio revision/tag metadata used when scanning pods for outdated sidecars.
type IstioConfig struct {
	// LastModifiedByRevision maps revision (e.g. "1-20") to ConfigMap lastModified time.
	// Used to skip pods created after the config change.
	LastModifiedByRevision map[string]time.Time
	// TagToRevision maps istio revision tags to revisions (from istio-revision-tag-* MWCs).
	TagToRevision map[string]string
	// LastModifiedByTag maps tag (e.g. "canary") to tag MWC lastModified time.
	// Used when workload uses a tag; skip pods created after the tag MWC changed.
	LastModifiedByTag map[string]time.Time
}

// listPods lists pods according to LimitToNamespaces: single ns, multiple nss, or all.
func listPods(ctx context.Context, c client.Client, opts ScanOptions) (corev1.PodList, error) {
	var podList corev1.PodList
	if len(opts.LimitToNamespaces) == 1 {
		if err := c.List(ctx, &podList, client.InNamespace(opts.LimitToNamespaces[0])); err != nil {
			return podList, fmt.Errorf("list pods in %s: %w", opts.LimitToNamespaces[0], err)
		}
	} else if len(opts.LimitToNamespaces) > 1 {
		limitSet := make(map[string]struct{})
		for _, ns := range opts.LimitToNamespaces {
			if ns != "" {
				limitSet[ns] = struct{}{}
			}
		}
		for ns := range limitSet {
			var nsList corev1.PodList
			if err := c.List(ctx, &nsList, client.InNamespace(ns)); err != nil {
				return podList, fmt.Errorf("list pods in %s: %w", ns, err)
			}
			podList.Items = append(podList.Items, nsList.Items...)
		}
	} else {
		if err := c.List(ctx, &podList); err != nil {
			return podList, fmt.Errorf("list pods: %w", err)
		}
	}
	return podList, nil
}

// shouldSkipPodForLastModified returns true if the pod was created at or after the effective lastModified + IstiodConfigReadDelay.
// Effective lastModified is the most recent of: ConfigMap lastModified for the revision, MWC lastModified for the tag (when using a tag).
// Only pods older than that threshold are scanned.
func shouldSkipPodForLastModified(pod *corev1.Pod, revision, revOrTag string, cfg IstioConfig, opts ScanOptions) bool {
	effectiveLastModified := time.Time{}
	if lastModified, ok := cfg.LastModifiedByRevision[revision]; ok && !lastModified.IsZero() {
		effectiveLastModified = lastModified
	}
	if tagLastModified, ok := cfg.LastModifiedByTag[revOrTag]; ok && !tagLastModified.IsZero() && tagLastModified.After(effectiveLastModified) {
		effectiveLastModified = tagLastModified
	}
	if effectiveLastModified.IsZero() {
		return false
	}
	effectiveWithDelay := effectiveLastModified.Add(opts.IstiodConfigReadDelay)
	return !pod.CreationTimestamp.Time.Before(effectiveWithDelay)
}

// get the istio-proxy image from the pod
func getIstioProxyImage(pod *corev1.Pod) string {
	// Check regular containers (traditional Istio sidecar injection)
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == istioProxyContainerName {
			return pod.Spec.Containers[i].Image
		}
	}
	// Check init containers (Kubernetes native sidecars)
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == istioProxyContainerName {
			return pod.Spec.InitContainers[i].Image
		}
	}
	return ""
}

// getWorkloadRef fetches the object at nn, returns WorkloadRef on success, nil on NotFound, or error.
func (s *PodScanner) getWorkloadRef(ctx context.Context, nn types.NamespacedName, kind string, obj client.Object) (*WorkloadRef, error) {
	if err := s.client.Get(ctx, nn, obj); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return &WorkloadRef{NamespacedName: nn, Kind: kind}, nil
}

// getIntermediateObject fetches a ReplicaSet or ControllerRevision; returns nil on NotFound.
func (s *PodScanner) getIntermediateObject(ctx context.Context, nn types.NamespacedName, kind string) (metav1.Object, error) {
	switch kind {
	case "ReplicaSet":
		var rs appsv1.ReplicaSet
		if err := s.client.Get(ctx, nn, &rs); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &rs, nil
	case "ControllerRevision":
		var cr appsv1.ControllerRevision
		if err := s.client.Get(ctx, nn, &cr); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &cr, nil
	default:
		return nil, nil
	}
}

// fetchIntermediateOwner fetches a ReplicaSet or ControllerRevision and recurses to find the workload.
func (s *PodScanner) fetchIntermediateOwner(ctx context.Context, nn types.NamespacedName, kind string) (*WorkloadRef, error) {
	nextObj, err := s.getIntermediateObject(ctx, nn, kind)
	if err != nil || nextObj == nil {
		return nil, err
	}
	return s.findWorkloadOwner(ctx, nextObj)
}

// determine the owner of the passed in object and owner reference
func (s *PodScanner) resolveOwner(ctx context.Context, obj metav1.Object, owner *metav1.OwnerReference) (*WorkloadRef, error) {
	nn := types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      owner.Name,
	}

	switch owner.Kind {
	case "Deployment": //nolint:goconst
		return s.getWorkloadRef(ctx, nn, "Deployment", &appsv1.Deployment{})
	case "StatefulSet":
		return s.getWorkloadRef(ctx, nn, "StatefulSet", &appsv1.StatefulSet{})
	case "DaemonSet":
		return s.getWorkloadRef(ctx, nn, "DaemonSet", &appsv1.DaemonSet{})
	case "ReplicaSet", "ControllerRevision":
		return s.fetchIntermediateOwner(ctx, nn, owner.Kind)
	}

	return nil, nil
}

// findWorkloadOwner recursively follows ownerReferences to find a Deployment,
// StatefulSet, or DaemonSet. Returns nil if none found.
func (s *PodScanner) findWorkloadOwner(ctx context.Context, obj metav1.Object) (*WorkloadRef, error) {
	owners := obj.GetOwnerReferences()
	if len(owners) == 0 {
		return nil, nil
	}

	// Use the first controller owner (Kubernetes typically has one)
	for _, owner := range owners {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}

		ref, err := s.resolveOwner(ctx, obj, &owner)
		if err != nil {
			return nil, err
		}
		if ref != nil {
			return ref, nil
		}
	}

	return nil, nil
}

// getIstioRevFromWorkloadOrNamespace returns istio.io/rev from the workload's pod template;
// if missing, from the namespace (istio.io/rev or istio-injection=enabled). Returns "default"
// when namespace has istio-injection=enabled. Returns "" when neither workload nor namespace
// has istio.io/rev and namespace lacks istio-injection=enabled (caller should skip the pod).
func (s *PodScanner) getIstioRevFromWorkloadOrNamespace(ctx context.Context, ref *WorkloadRef) (string, error) {
	switch ref.Kind {
	case "Deployment": //nolint:goconst
		var dep appsv1.Deployment
		if err := s.client.Get(ctx, ref.NamespacedName, &dep); err != nil {
			return "", err
		}
		if v, ok := dep.Spec.Template.Labels[constants.LabelIstioRev]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	case "StatefulSet": //nolint:goconst
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, ref.NamespacedName, &sts); err != nil {
			return "", err
		}
		if v, ok := sts.Spec.Template.Labels[constants.LabelIstioRev]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	case "DaemonSet": //nolint:goconst
		var ds appsv1.DaemonSet
		if err := s.client.Get(ctx, ref.NamespacedName, &ds); err != nil {
			return "", err
		}
		if v, ok := ds.Spec.Template.Labels[constants.LabelIstioRev]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	default:
		//nolint:goconst
		return "default", nil
	}

	var ns corev1.Namespace
	if err := s.client.Get(ctx, types.NamespacedName{Name: ref.Namespace}, &ns); err != nil {
		return "", err
	}
	if v, ok := ns.Labels[constants.LabelIstioRev]; ok && v != "" {
		return v, nil
	}
	if v, ok := ns.Labels[constants.LabelIstioInjection]; ok && v == "enabled" {
		//nolint:goconst
		return "default", nil
	}
	return "", nil
}

// buildPodFromWorkload fetches the workload and builds a Pod from its template.
func (s *PodScanner) buildPodFromWorkload(ctx context.Context, ref *WorkloadRef) (*corev1.Pod, error) {
	nn := ref.NamespacedName
	// Default to a placeholder name; the webhook doesn't require a real pod name.
	name := "fortsa-check"
	if nn.Name != "" {
		// ensure the generated name is not too long
		if len(nn.Name) > dnsSubdomainMaxLen-len(podNameSuffix) {
			name = nn.Name[:dnsSubdomainMaxLen-len(podNameSuffix)]
		}
		name = name + podNameSuffix
	}

	switch ref.Kind {
	case "Deployment":
		var dep appsv1.Deployment
		if err := s.client.Get(ctx, nn, &dep); err != nil {
			return nil, err
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   nn.Namespace,
				Name:        name,
				Labels:      dep.Spec.Template.Labels,
				Annotations: dep.Spec.Template.Annotations,
			},
			Spec: dep.Spec.Template.Spec,
		}, nil
	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, nn, &sts); err != nil {
			return nil, err
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   nn.Namespace,
				Name:        name,
				Labels:      sts.Spec.Template.Labels,
				Annotations: sts.Spec.Template.Annotations,
			},
			Spec: sts.Spec.Template.Spec,
		}, nil
	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := s.client.Get(ctx, nn, &ds); err != nil {
			return nil, err
		}
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   nn.Namespace,
				Name:        name,
				Labels:      ds.Spec.Template.Labels,
				Annotations: ds.Spec.Template.Annotations,
			},
			Spec: ds.Spec.Template.Spec,
		}, nil
	default:
		return nil, nil
	}
}

// processPodForOutdatedSidecar checks a single pod for an outdated Istio sidecar. Returns the
// WorkloadRef when the pod has an outdated sidecar and should be added to results; nil when skipped.
func (s *PodScanner) processPodForOutdatedSidecar(ctx context.Context, pod *corev1.Pod, cfg IstioConfig, opts ScanOptions) *WorkloadRef {
	logger := log.FromContext(ctx)

	ref, err := s.findWorkloadOwner(ctx, pod)
	if err != nil {
		logger.Error(err, "failed to find workload owner", "namespace", pod.Namespace, "name", pod.Name)
		return nil
	}
	if ref == nil {
		logger.V(1).Info("no restartable workload owner found", "namespace", pod.Namespace, "name", pod.Name)
		return nil
	}

	// try to determine the istio revision so we know which webhook to call
	revOrTag, err := s.getIstioRevFromWorkloadOrNamespace(ctx, ref)
	if err != nil {
		logger.Error(err, "failed to get istio revision from workload or namespace", "namespace", ref.Namespace, "name", ref.Name, "kind", ref.Kind)
		return nil
	}
	if revOrTag == "" {
		logger.V(1).Info("no istio.io/rev or istio-injection=enabled on workload or namespace, skipping", "namespace",
			ref.Namespace, "name", ref.Name, "kind", ref.Kind)
		return nil
	}
	revision := revOrTag
	if r, ok := cfg.TagToRevision[revOrTag]; ok {
		revision = r
	}

	// skip pods that were created after the config change + delay - they likely have the correct sidecar
	if shouldSkipPodForLastModified(pod, revision, revOrTag, cfg, opts) {
		return nil
	}

	logger.V(1).Info("found workload", "namespace", ref.Namespace, "name", ref.Name, "kind", ref.Kind)

	// build a Pod object from the workload template that we can send to the webhook to see what the expected sidecar image is
	templatePod, err := s.buildPodFromWorkload(ctx, ref)
	if err != nil {
		logger.Error(err, "failed to build check pod from workload", "namespace", ref.Namespace, "name", ref.Name, "kind", ref.Kind)
		return nil
	}

	// call Istio's pod-injector webhook to determine the expected sidecar image
	mutated, err := s.webhookCaller.CallWebhook(ctx, templatePod, revision, revOrTag == "default") //nolint:goconst
	if err != nil {
		logger.Error(err, "webhook call failed", "namespace", pod.Namespace, "name", pod.Name)
		return nil
	}

	expectedImage := getIstioProxyImage(mutated)
	currentImage := getIstioProxyImage(pod)
	logger.V(1).Info("expected image", "expected", expectedImage, "current", currentImage, "namespace", pod.Namespace, "name", pod.Name)
	if expectedImage == "" {
		return nil
	}
	if imagesMatch(currentImage, expectedImage, opts.CompareHub) {
		return nil
	}

	logger.Info("outdated sidecar image found",
		"namespace", pod.Namespace, "name", pod.Name,
		"revision", revision,
		"current", currentImage,
		"expected", expectedImage)

	return ref
}

// ScanOutdatedPods lists all Pods, finds each pod's controller (Deployment/StatefulSet/DaemonSet),
// builds a pod from the workload template, submits it to the Istio injection webhook, and compares
// the mutated response's istio-proxy image with the current pod. Returns WorkloadRefs for pods
// with outdated sidecars. cfg.LastModifiedByRevision is used for the ConfigMap LastModified skip.
// cfg.LastModifiedByTag is used for the tag MWC LastModified skip when workload uses a tag.
// cfg.TagToRevision maps istio revision tags to revisions (from istio-revision-tag-* MutatingWebhookConfigurations).
func (s *PodScanner) ScanOutdatedPods(ctx context.Context, cfg IstioConfig, opts ScanOptions) ([]WorkloadRef, error) {
	logger := log.FromContext(ctx)

	if s.webhookCaller == nil {
		return nil, nil
	}
	if cfg.TagToRevision == nil {
		cfg.TagToRevision = map[string]string{}
	}

	// get a list of pods we will scan
	podList, err := listPods(ctx, s.client, opts)
	if err != nil {
		return nil, err
	}

	seen := make(map[types.NamespacedName]struct{})
	var workloads []WorkloadRef //nolint:prealloc

	skipSet := make(map[string]struct{})
	for _, ns := range opts.SkipNamespaces {
		if ns != "" {
			skipSet[ns] = struct{}{}
		}
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		// skip pods in the namespaces we are skipping
		if _, skip := skipSet[pod.Namespace]; skip {
			continue
		}

		logger.V(1).Info("scanning pod", "namespace", pod.Namespace, "name", pod.Name)

		// returns WorkloadRef when the pod has an outdated sidecar and should be added to results; nil when skipped.
		ref := s.processPodForOutdatedSidecar(ctx, pod, cfg, opts)
		if ref == nil {
			continue
		}
		// avoid adding the same workload multiple times
		if _, ok := seen[ref.NamespacedName]; ok {
			continue
		}
		seen[ref.NamespacedName] = struct{}{}
		workloads = append(workloads, *ref)
	}

	return workloads, nil
}
