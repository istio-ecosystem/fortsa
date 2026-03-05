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
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/istio-ecosystem/fortsa/internal/constants"
)

// ReconcileRequest returns a reconcile.Request that triggers a ConfigMap change reconciliation
// when istio-sidecar-injector* ConfigMaps in istio-system change. Used by the ConfigMap watch.
func ReconcileRequest() ctrl.Request {
	log.FromContext(context.TODO()).V(1).Info("configmap change detected")
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: constants.IstioSystemNamespace, Name: constants.ReconcileTriggerNameIstioChange},
	}
}

// ReconcileRequestName returns the request name used for ConfigMap reconcile triggers.
// Used by the controller to identify ConfigMap-triggered requests.
func ReconcileRequestName() string {
	return constants.ReconcileTriggerNameIstioChange
}

// Filter returns a predicate function that filters ConfigMaps to only those
// in istio-system with name equal to or prefixed with istio-sidecar-injector.
func Filter() func(client.Object) bool {
	return func(obj client.Object) bool {
		if obj.GetNamespace() != constants.IstioSystemNamespace {
			return false
		}
		return strings.HasPrefix(obj.GetName(), constants.ConfigMapNamePrefix)
	}
}
