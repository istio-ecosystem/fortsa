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

package rollout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// Group is the Argo Rollouts API group.
	Group = "argoproj.io"
	// Version is the Argo Rollouts API version supported by Fortsa.
	Version = "v1alpha1"
	// Kind is the Argo Rollouts resource kind.
	Kind = "Rollout"
)

// GVK identifies the Argo Rollouts Rollout resource.
var GVK = schema.GroupVersionKind{Group: Group, Version: Version, Kind: Kind}

var (
	errInvalidWorkloadRef         = errors.New("invalid Rollout workloadRef")
	errUnsupportedWorkloadRefAPI  = errors.New("unsupported Rollout workloadRef apiVersion")
	errUnsupportedWorkloadRefKind = errors.New("unsupported Rollout workloadRef kind")
)

// Get fetches an Argo Rollouts Rollout. A missing Rollout or Rollout CRD is
// treated as unsupported and returns a nil object without an error.
func Get(ctx context.Context, c client.Client, nn types.NamespacedName) (*unstructured.Unstructured, error) {
	ro := &unstructured.Unstructured{}
	ro.SetGroupVersionKind(GVK)
	if err := c.Get(ctx, nn, ro); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get Rollout %s: %w", nn, err)
	}
	return ro, nil
}

// ExtractPodTemplateSpec returns the effective pod template for a Rollout.
// Rollouts using workloadRef resolve their Deployment template from the
// referenced Deployment because Argo Rollouts does not persist that template.
func ExtractPodTemplateSpec(ctx context.Context, c client.Client, ro *unstructured.Unstructured) (*corev1.PodTemplateSpec, error) {
	if ro == nil {
		return nil, fmt.Errorf("extract pod template: %w", errInvalidWorkloadRef)
	}

	templateMap, found, err := unstructured.NestedMap(ro.Object, "spec", "template")
	if err != nil {
		return nil, fmt.Errorf("read Rollout %s spec.template: %w", ro.GetName(), err)
	}
	if found && len(templateMap) > 0 {
		var template corev1.PodTemplateSpec
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(templateMap, &template); err != nil {
			return nil, fmt.Errorf("convert Rollout %s spec.template: %w", ro.GetName(), err)
		}
		if !reflect.DeepEqual(template, corev1.PodTemplateSpec{}) {
			return &template, nil
		}
	}

	workloadRef, found, err := unstructured.NestedMap(ro.Object, "spec", "workloadRef")
	if err != nil {
		return nil, fmt.Errorf("read Rollout %s spec.workloadRef: %w", ro.GetName(), err)
	}
	if !found {
		return nil, fmt.Errorf("rollout %s has no effective pod template or workloadRef: %w", ro.GetName(), errInvalidWorkloadRef)
	}

	kind, err := requiredWorkloadRefString(workloadRef, "kind")
	if err != nil {
		return nil, fmt.Errorf("rollout %s: %w", ro.GetName(), err)
	}
	if kind != "Deployment" {
		return nil, fmt.Errorf("rollout %s workloadRef kind %q: %w", ro.GetName(), kind, errUnsupportedWorkloadRefKind)
	}
	apiVersion, err := requiredWorkloadRefString(workloadRef, "apiVersion")
	if err != nil {
		return nil, fmt.Errorf("rollout %s: %w", ro.GetName(), err)
	}
	name, err := requiredWorkloadRefString(workloadRef, "name")
	if err != nil {
		return nil, fmt.Errorf("rollout %s: %w", ro.GetName(), err)
	}
	if apiVersion != appsv1.SchemeGroupVersion.String() {
		return nil, fmt.Errorf("rollout %s workloadRef apiVersion %q: %w", ro.GetName(), apiVersion, errUnsupportedWorkloadRefAPI)
	}

	var dep appsv1.Deployment
	deploymentNN := types.NamespacedName{Namespace: ro.GetNamespace(), Name: name}
	if err := c.Get(ctx, deploymentNN, &dep); err != nil {
		return nil, fmt.Errorf("get Rollout %s workloadRef Deployment %s: %w", ro.GetName(), deploymentNN, err)
	}
	return &dep.Spec.Template, nil
}

func requiredWorkloadRefString(workloadRef map[string]interface{}, field string) (string, error) {
	value, found, err := unstructured.NestedString(workloadRef, field)
	if err != nil {
		return "", fmt.Errorf("read workloadRef.%s: %w", field, err)
	}
	if !found || value == "" {
		return "", fmt.Errorf("workloadRef.%s is required: %w", field, errInvalidWorkloadRef)
	}
	return value, nil
}

// GetRestartAt returns the Rollout's restartAt time. Missing, malformed, or
// non-string values are treated as an unset timestamp.
func GetRestartAt(ro *unstructured.Unstructured) (time.Time, error) {
	if ro == nil {
		return time.Time{}, nil
	}
	value, found, err := unstructured.NestedString(ro.Object, "spec", "restartAt")
	if err != nil || !found || value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// BuildRestartAtPatch builds a JSON merge patch that sets a Rollout restartAt timestamp.
func BuildRestartAtPatch(t time.Time) ([]byte, error) {
	patch := map[string]interface{}{
		"spec": map[string]string{
			"restartAt": t.UTC().Format(time.RFC3339),
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("marshal Rollout restartAt patch: %w", err)
	}
	return patchBytes, nil
}
