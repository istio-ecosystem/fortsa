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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/istio-ecosystem/fortsa/internal/constants"
)

// BuildLastModifiedByRevision lists istio-sidecar-injector ConfigMaps in istio-system,
// parses each, and returns revision -> LastModified for pod skip logic.
func BuildLastModifiedByRevision(ctx context.Context, c client.Client) (map[string]time.Time, error) {
	var cmList corev1.ConfigMapList
	if err := c.List(ctx, &cmList, client.InNamespace(constants.IstioSystemNamespace)); err != nil {
		return nil, fmt.Errorf("list ConfigMaps in %s: %w", constants.IstioSystemNamespace, err)
	}
	result := make(map[string]time.Time)
	for i := range cmList.Items {
		cm := &cmList.Items[i]
		if !strings.HasPrefix(cm.Name, constants.ConfigMapNamePrefix) {
			continue
		}
		vals, err := ParseConfigMapValues(cm)
		if err != nil {
			continue
		}
		lastModified := GetConfigMapLastModified(cm)
		if existing, ok := result[vals.Revision]; !ok || lastModified.After(existing) {
			result[vals.Revision] = lastModified
		}
	}
	return result, nil
}
