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

package periodic

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	istioSystemNamespace = "istio-system"
	reconcileTriggerName = "__periodic_reconcile__"
)

// ReconcileRequest returns a reconcile.Request that triggers a full periodic reconciliation
// of all istio-sidecar-injector ConfigMaps. Used by the periodic ticker source.
func ReconcileRequest() reconcile.Request {
	return reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: istioSystemNamespace, Name: reconcileTriggerName},
	}
}

// ReconcileRequestName returns the request name used for periodic reconcile triggers.
// Used by the controller to identify periodic-triggered requests.
func ReconcileRequestName() string {
	return reconcileTriggerName
}

// NewReconcileSource returns a source that enqueues a full-reconcile request at the given period.
// When period is 0, returns nil (caller should not add the source).
func NewReconcileSource(period time.Duration) source.Source {
	return source.Func(func(ctx context.Context, queue workqueue.TypedRateLimitingInterface[reconcile.Request]) error {
		go func() {
			ticker := time.NewTicker(period)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					queue.Add(ReconcileRequest())
				}
			}
		}()
		return nil
	})
}
