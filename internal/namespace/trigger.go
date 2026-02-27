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

package namespace

import (
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// hasIstioLabels returns true if the object has istio.io/rev or istio-injection in its labels.
func hasIstioLabels(obj client.Object) bool {
	labels := obj.GetLabels()
	if _, ok := labels["istio.io/rev"]; ok {
		return true
	}
	if v, ok := labels["istio-injection"]; ok && v != "" {
		return true
	}
	return false
}

// Filter returns a predicate that filters Namespace events to only those
// with Istio-related labels (istio.io/rev or istio-injection). For updates, triggers
// when either old or new has the labels to catch add, remove, or change.
func Filter() predicate.Funcs {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return hasIstioLabels(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return hasIstioLabels(e.ObjectOld) || hasIstioLabels(e.ObjectNew)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return hasIstioLabels(e.Object)
		},
	}
}

// ReconcileRequest returns a reconcile.Request for a namespace-scoped reconciliation.
// Used when a Namespace's Istio labels change.
func ReconcileRequest(namespace string) ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Name: namespace},
	}
}
