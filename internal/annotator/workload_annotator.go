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
	restartedAtAnnotation = "fortsa.scaffidi.net/restartedAt"
)

// WorkloadAnnotator annotates workloads to trigger restarts.
type WorkloadAnnotator interface {
	Annotate(ctx context.Context, ref podscanner.WorkloadRef) error
}

// WorkloadAnnotatorImpl adds the restartedAt annotation to workload pod templates.
type WorkloadAnnotatorImpl struct {
	client client.Client
}

// NewWorkloadAnnotator creates a new WorkloadAnnotatorImpl.
func NewWorkloadAnnotator(c client.Client) *WorkloadAnnotatorImpl {
	return &WorkloadAnnotatorImpl{client: c}
}

// Annotate adds fortsa.scaffidi.net/restartedAt to the workload's pod template
// spec.template.metadata.annotations, triggering a rolling restart.
func (a *WorkloadAnnotatorImpl) Annotate(ctx context.Context, ref podscanner.WorkloadRef) error {
	value := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{
					"annotations": map[string]string{
						restartedAtAnnotation: value,
					},
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	switch ref.Kind {
	case "Deployment":
		return a.client.Patch(ctx, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
	case "StatefulSet":
		return a.client.Patch(ctx, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
	case "DaemonSet":
		return a.client.Patch(ctx, &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name},
		}, client.RawPatch(types.MergePatchType, patchBytes))
	default:
		return nil
	}
}
