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

package mwc

import (
	"context"
	"strings"
	"time"

	admissionregv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrl "sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	istioRevisionTagPrefix  = "istio-revision-tag-"
	istioSystemNamespace    = "istio-system"
	mwcReconcileTriggerName = "__mwc_reconcile__"
)

// ReconcileRequest returns a reconcile.Request that triggers a tag-mapping reconciliation
// when istio-revision-tag-* MutatingWebhookConfigurations change. Used by the MWC watch.
func ReconcileRequest() ctrl.Request {
	return ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: istioSystemNamespace, Name: mwcReconcileTriggerName},
	}
}

// ReconcileRequestName returns the request name used for MWC reconcile triggers.
// Used by the controller to identify MWC-triggered requests.
func ReconcileRequestName() string {
	return mwcReconcileTriggerName
}

// Filter returns a predicate function that filters MutatingWebhookConfigurations
// to only those named istio-revision-tag-* (tag-to-revision mapping).
func Filter() func(client.Object) bool {
	return func(obj client.Object) bool {
		return strings.HasPrefix(obj.GetName(), istioRevisionTagPrefix)
	}
}

// getLastModified returns the latest modification time of the MutatingWebhookConfiguration.
func getLastModified(mwc *admissionregv1.MutatingWebhookConfiguration) time.Time {
	latest := mwc.CreationTimestamp.Time
	for _, mf := range mwc.ManagedFields {
		if mf.Time != nil && mf.Time.After(latest) {
			latest = mf.Time.Time
		}
	}
	return latest
}

// FetchTagToRevision lists istio-revision-tag-* MutatingWebhookConfigurations and builds
// a tag-to-revision map from istio.io/tag and istio.io/rev labels.
func FetchTagToRevision(ctx context.Context, c client.Client) (map[string]string, error) {
	tagToRevision, _, err := FetchTagToRevisionAndLastModified(ctx, c)
	return tagToRevision, err
}

// FetchTagToRevisionAndLastModified lists istio-revision-tag-* MutatingWebhookConfigurations,
// builds tag-to-revision map and tag-to-lastModified map (for pod skip logic).
func FetchTagToRevisionAndLastModified(ctx context.Context, c client.Client) (map[string]string, map[string]time.Time, error) {
	var mwcList admissionregv1.MutatingWebhookConfigurationList
	if err := c.List(ctx, &mwcList); err != nil {
		return nil, nil, err
	}
	tagToRevision := make(map[string]string)
	lastModifiedByTag := make(map[string]time.Time)
	for i := range mwcList.Items {
		mwc := &mwcList.Items[i]
		if !strings.HasPrefix(mwc.Name, istioRevisionTagPrefix) {
			continue
		}
		tag := mwc.Labels["istio.io/tag"]
		revision := mwc.Labels["istio.io/rev"]
		if tag != "" && revision != "" {
			tagToRevision[tag] = revision
			lastModifiedByTag[tag] = getLastModified(mwc)
		}
	}
	return tagToRevision, lastModifiedByTag, nil
}
