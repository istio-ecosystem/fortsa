package podscanner

import (
	"context"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/istio-ecosystem/fortsa/internal/configmap"
	"github.com/istio-ecosystem/fortsa/internal/webhook"
)

const (
	istioProxyContainerName = "istio-proxy"
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

// Matches returns true if the parsed container image matches the expected Istio values.
// When compareHub is true, registry must match hub; when false, only image name and tag are compared.
func (p ParsedImage) Matches(vals *configmap.IstioValues, compareHub bool) bool {
	if compareHub && p.Registry != vals.Hub {
		return false
	}
	return p.ImageName == vals.Image && p.Tag == vals.Tag
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

// NewPodScanner creates a new PodScanner.
func NewPodScanner(c client.Client, webhookCaller webhook.WebhookCaller) *PodScanner {
	return &PodScanner{client: c, webhookCaller: webhookCaller}
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
	// IstiodConfigReadDelay is added to ConfigMap LastModified when deciding whether to skip pods.
	// Pods created within this window after a ConfigMap update may have been injected before
	// Istiod loaded the new config, so we still scan them.
	IstiodConfigReadDelay time.Duration
	// SkipNamespaces lists namespaces to skip when scanning pods.
	SkipNamespaces []string
	// LimitToNamespaces, when non-empty, restricts scanning to pods in these namespaces only.
	LimitToNamespaces []string
}

// ScanOutdatedPods lists all Pods, finds each pod's controller (Deployment/StatefulSet/DaemonSet),
// builds a pod from the workload template, submits it to the Istio injection webhook, and compares
// the mutated response's istio-proxy image with the current pod. Returns WorkloadRefs for pods
// with outdated sidecars. lastModifiedByRevision is used for the ConfigMap LastModified skip.
// lastModifiedByTag is used for the tag MWC LastModified skip when workload uses a tag.
// tagToRevision maps istio revision tags to revisions (from istio-revision-tag-* MutatingWebhookConfigurations).
func (s *PodScanner) ScanOutdatedPods(ctx context.Context, lastModifiedByRevision map[string]time.Time, tagToRevision map[string]string, lastModifiedByTag map[string]time.Time, opts ScanOptions) ([]WorkloadRef, error) {
	logger := log.FromContext(ctx)

	if s.webhookCaller == nil {
		return nil, nil
	}
	if tagToRevision == nil {
		tagToRevision = map[string]string{}
	}

	var podList corev1.PodList
	if len(opts.LimitToNamespaces) == 1 {
		if err := s.client.List(ctx, &podList, client.InNamespace(opts.LimitToNamespaces[0])); err != nil {
			return nil, err
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
			if err := s.client.List(ctx, &nsList, client.InNamespace(ns)); err != nil {
				return nil, err
			}
			podList.Items = append(podList.Items, nsList.Items...)
		}
	} else {
		if err := s.client.List(ctx, &podList); err != nil {
			return nil, err
		}
	}

	seen := make(map[types.NamespacedName]struct{})
	var workloads []WorkloadRef

	skipSet := make(map[string]struct{})
	for _, ns := range opts.SkipNamespaces {
		if ns != "" {
			skipSet[ns] = struct{}{}
		}
	}

	for i := range podList.Items {
		pod := &podList.Items[i]
		if _, skip := skipSet[pod.Namespace]; skip {
			continue
		}

		logger.Info("scanning pod", "pod", pod.Namespace+"/"+pod.Name)

		ref, err := s.findWorkloadOwner(ctx, pod)
		if err != nil {
			logger.Error(err, "failed to find workload owner", "pod", pod.Namespace+"/"+pod.Name)
			continue
		}
		if ref == nil {
			logger.Info("no restartableworkload owner found", "pod", pod.Namespace+"/"+pod.Name)
			continue
		}

		revOrTag, err := s.getIstioRevFromWorkloadOrNamespace(ctx, ref)
		if err != nil {
			logger.Error(err, "failed to get istio.io/rev from workload or namespace", "workload", ref.NamespacedName)
			continue
		}
		if revOrTag == "" {
			logger.V(1).Info("namespace has no istio.io/rev or istio-injection=enabled, skipping", "workload", ref.NamespacedName)
			continue
		}
		revision := revOrTag
		if r, ok := tagToRevision[revOrTag]; ok {
			revision = r
		}

		// Skip pods created after the ConfigMap was last updated; they were injected with the current config.
		// Add IstiodConfigReadDelay to LastModified: pods created in that window may have been
		// injected before Istiod loaded the new config, so we still scan them.
		// When using a tag, also require the tag's MWC to not have been modified after the pod was created;
		// otherwise the tag-to-revision mapping may have changed and we must scan.
		skip := false
		if lastModified, ok := lastModifiedByRevision[revision]; ok && !lastModified.IsZero() {
			effectiveLastModified := lastModified.Add(opts.IstiodConfigReadDelay)
			if !pod.CreationTimestamp.Time.Before(effectiveLastModified) {
				skip = true
			}
		}
		if skip && revOrTag != revision {
			// Using a tag - override skip if tag's MWC was modified after pod was created
			if tagLastModified, ok := lastModifiedByTag[revOrTag]; ok && !tagLastModified.IsZero() {
				tagEffective := tagLastModified.Add(opts.IstiodConfigReadDelay)
				if pod.CreationTimestamp.Time.Before(tagEffective) {
					skip = false // Tag was modified after pod - must scan
				}
			} else {
				skip = false // No tag lastModified info - be conservative, scan
			}
		}
		if skip {
			continue
		}

		logger.Info("found workload", "workload", ref.Namespace+"/"+ref.Name)

		templatePod, err := s.buildPodFromWorkload(ctx, ref)
		if err != nil {
			logger.Error(err, "failed to build pod from workload", "workload", ref.NamespacedName)
			continue
		}

		mutated, err := s.webhookCaller.CallWebhook(ctx, templatePod, revision, revOrTag == "default")
		if err != nil {
			logger.Error(err, "webhook call failed", "pod", pod.Namespace+"/"+pod.Name)
			continue
		}

		expectedImage := getIstioProxyImage(mutated)
		currentImage := getIstioProxyImage(pod)
		logger.V(1).Info("expected image", "expected", expectedImage, "current", currentImage, "pod", pod.Namespace+"/"+pod.Name)
		if expectedImage == "" {
			continue
		}
		if imagesMatch(currentImage, expectedImage, opts.CompareHub) {
			continue
		}

		logger.Info("outdated image",
			"pod", pod.Namespace+"/"+pod.Name,
			"revision", revision,
			"current", currentImage,
			"expected", expectedImage)

		if _, ok := seen[ref.NamespacedName]; ok {
			continue
		}
		seen[ref.NamespacedName] = struct{}{}
		workloads = append(workloads, *ref)
	}

	return workloads, nil
}

// getIstioRevFromWorkloadOrNamespace returns istio.io/rev from the workload's pod template;
// if missing, from the namespace (istio.io/rev or istio-injection=enabled). Returns "default"
// when namespace has istio-injection=enabled. Returns "" when neither workload nor namespace
// has istio.io/rev and namespace lacks istio-injection=enabled (caller should skip the pod).
func (s *PodScanner) getIstioRevFromWorkloadOrNamespace(ctx context.Context, ref *WorkloadRef) (string, error) {
	switch ref.Kind {
	case "Deployment":
		var dep appsv1.Deployment
		if err := s.client.Get(ctx, ref.NamespacedName, &dep); err != nil {
			return "", err
		}
		if v, ok := dep.Spec.Template.Labels["istio.io/rev"]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, ref.NamespacedName, &sts); err != nil {
			return "", err
		}
		if v, ok := sts.Spec.Template.Labels["istio.io/rev"]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := s.client.Get(ctx, ref.NamespacedName, &ds); err != nil {
			return "", err
		}
		if v, ok := ds.Spec.Template.Labels["istio.io/rev"]; ok && v != "" {
			return v, nil
		}
		// Template has no istio.io/rev; continue to namespace check below
	default:
		return "default", nil
	}

	var ns corev1.Namespace
	if err := s.client.Get(ctx, types.NamespacedName{Name: ref.Namespace}, &ns); err != nil {
		return "", err
	}
	if v, ok := ns.Labels["istio.io/rev"]; ok && v != "" {
		return v, nil
	}
	if v, ok := ns.Labels["istio-injection"]; ok && v == "enabled" {
		return "default", nil
	}
	return "", nil
}

// dnsSubdomainMaxLen is the max length for Kubernetes DNS subdomain names (e.g. pod names).
const dnsSubdomainMaxLen = 63
const podNameSuffix = "-fortsa2-check"

// buildPodFromWorkload fetches the workload and builds a Pod from its template.
func (s *PodScanner) buildPodFromWorkload(ctx context.Context, ref *WorkloadRef) (*corev1.Pod, error) {
	nn := ref.NamespacedName
	// Default to a placeholder name; the webhook doesn't require a real pod name.
	name := "fortsa2-check"
	if nn.Name != "" {
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

func getIstioProxyImage(pod *corev1.Pod) string {
	// Check regular containers (traditional Istio sidecar injection)
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == istioProxyContainerName {
			return pod.Spec.Containers[i].Image
		}
	}
	// Check init containers (Kubernetes native sidecars: istio-proxy in initContainers with restartPolicy: Always)
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == istioProxyContainerName {
			return pod.Spec.InitContainers[i].Image
		}
	}
	return ""
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

func (s *PodScanner) resolveOwner(ctx context.Context, obj metav1.Object, owner *metav1.OwnerReference) (*WorkloadRef, error) {
	nn := types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      owner.Name,
	}

	switch owner.Kind {
	case "Deployment":
		var dep appsv1.Deployment
		if err := s.client.Get(ctx, nn, &dep); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &WorkloadRef{NamespacedName: nn, Kind: "Deployment"}, nil

	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := s.client.Get(ctx, nn, &sts); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &WorkloadRef{NamespacedName: nn, Kind: "StatefulSet"}, nil

	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := s.client.Get(ctx, nn, &ds); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		return &WorkloadRef{NamespacedName: nn, Kind: "DaemonSet"}, nil

	case "ReplicaSet", "ControllerRevision":
		// Recurse: fetch the owner and follow its ownerReferences
		var nextObj runtime.Object
		switch owner.Kind {
		case "ReplicaSet":
			var rs appsv1.ReplicaSet
			if err := s.client.Get(ctx, nn, &rs); err != nil {
				if errors.IsNotFound(err) {
					return nil, nil
				}
				return nil, err
			}
			nextObj = &rs
		case "ControllerRevision":
			// ControllerRevision is unstructured in apps/v1; we need to get it
			// and check its ownerReferences. ControllerRevision is in apps/v1.
			var cr appsv1.ControllerRevision
			if err := s.client.Get(ctx, nn, &cr); err != nil {
				if errors.IsNotFound(err) {
					return nil, nil
				}
				return nil, err
			}
			nextObj = &cr
		}

		return s.findWorkloadOwner(ctx, nextObj.(metav1.Object))
	}

	return nil, nil
}
