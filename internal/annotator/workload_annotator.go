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

package annotator

import (
	"context"
	"encoding/json"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/istio-ecosystem/fortsa/internal/podscanner"
)

const (
	// RestartedAtAnnotation is the annotation key used to trigger workload restarts.
	RestartedAtAnnotation = "fortsa.scaffidi.net/restartedAt"
)

// WorkloadAnnotator annotates workloads to trigger restarts.
type WorkloadAnnotator interface {
	Annotate(ctx context.Context, ref podscanner.WorkloadRef) (annotated bool, err error)
}

// WorkloadAnnotatorImpl adds the restartedAt annotation to workload pod templates.
type WorkloadAnnotatorImpl struct {
	client             client.Client
	annotationCooldown time.Duration
}

// NewWorkloadAnnotator creates a new WorkloadAnnotatorImpl.
// annotationCooldown: skip re-annotating if the workload was annotated within this duration; 0 disables the check.
func NewWorkloadAnnotator(c client.Client, annotationCooldown time.Duration) *WorkloadAnnotatorImpl {
	return &WorkloadAnnotatorImpl{client: c, annotationCooldown: annotationCooldown}
}

// Annotate adds fortsa.scaffidi.net/restartedAt to the workload's pod template
// spec.template.metadata.annotations, triggering a rolling restart.
// When annotationCooldown is set, skips re-annotating if the workload was annotated within that duration.
// Returns (true, nil) when a patch was applied, (false, nil) when skipped or unsupported kind, (false, err) on error.
func (a *WorkloadAnnotatorImpl) Annotate(ctx context.Context, ref podscanner.WorkloadRef) (bool, error) {
	if a.annotationCooldown > 0 {
		annotatedAt, err := a.getRestartedAt(ctx, ref)
		if err != nil {
			return false, err
		}
		if !annotatedAt.IsZero() && time.Since(annotatedAt) < a.annotationCooldown {
			return false, nil // skip: already annotated recently
		}
	}

	value := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]string{
						RestartedAtAnnotation: value,
					},
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return false, err
	}

	switch ref.Kind {
	case "Deployment":
		err := a.client.Patch(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
		return err == nil, err
	case "StatefulSet":
		err := a.client.Patch(ctx, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
		return err == nil, err
	case "DaemonSet":
		err := a.client.Patch(ctx, &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
		return err == nil, err
	default:
		return false, nil
	}
}

// getRestartedAt returns the time from the workload's restartedAt annotation, or zero time if absent/unparseable.
func (a *WorkloadAnnotatorImpl) getRestartedAt(ctx context.Context, ref podscanner.WorkloadRef) (time.Time, error) {
	var annotations map[string]string
	switch ref.Kind {
	case "Deployment":
		var dep appsv1.Deployment
		if err := a.client.Get(ctx, ref.NamespacedName, &dep); err != nil {
			return time.Time{}, err
		}
		annotations = dep.Spec.Template.Annotations
	case "StatefulSet":
		var sts appsv1.StatefulSet
		if err := a.client.Get(ctx, ref.NamespacedName, &sts); err != nil {
			return time.Time{}, err
		}
		annotations = sts.Spec.Template.Annotations
	case "DaemonSet":
		var ds appsv1.DaemonSet
		if err := a.client.Get(ctx, ref.NamespacedName, &ds); err != nil {
			return time.Time{}, err
		}
		annotations = ds.Spec.Template.Annotations
	default:
		return time.Time{}, nil
	}
	if annotations == nil {
		return time.Time{}, nil
	}
	s, ok := annotations[RestartedAtAnnotation]
	if !ok || s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, nil // unparseable: treat as no annotation
	}
	return t, nil
}
