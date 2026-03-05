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

package configmap

import (
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	istioSystemNamespace       = "istio-system"
	configMapNamePrefix        = "istio-sidecar-injector"
	configMapReconcileTrigName = "__istio_change__"
)

// ReconcileRequest returns a reconcile.Request that triggers a ConfigMap change reconciliation
// when istio-sidecar-injector* ConfigMaps in istio-system change. Used by the ConfigMap watch.
func ReconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: istioSystemNamespace, Name: configMapReconcileTrigName},
	}
}

// ReconcileRequestName returns the request name used for ConfigMap reconcile triggers.
// Used by the controller to identify ConfigMap-triggered requests.
func ReconcileRequestName() string {
	return configMapReconcileTrigName
}

// Filter returns a predicate function that filters ConfigMaps to only those
// in istio-system with name equal to or prefixed with istio-sidecar-injector.
func Filter() func(client.Object) bool {
	return func(obj client.Object) bool {
		if obj.GetNamespace() != istioSystemNamespace {
			return false
		}
		return strings.HasPrefix(obj.GetName(), configMapNamePrefix)
	}
}
