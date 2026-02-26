/*
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
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// IstioValues represents the extracted values from the Istio sidecar injector ConfigMap.
type IstioValues struct {
	Revision     string
	Hub          string
	Tag          string
	Image        string    // global.proxy.image, e.g. "proxyv2"
	LastModified time.Time // when the ConfigMap was last updated; used to skip pods created after this time
}

// GetConfigMapLastModified returns the latest modification time of the ConfigMap.
// Uses metadata.managedFields timestamps when available; falls back to creationTimestamp.
func GetConfigMapLastModified(cm *corev1.ConfigMap) time.Time {
	latest := cm.CreationTimestamp.Time
	for _, mf := range cm.ManagedFields {
		if mf.Time != nil && mf.Time.After(latest) {
			latest = mf.Time.Time
		}
	}
	return latest
}

// ParseConfigMapValues extracts hub, tag, global.proxy.image, and revision from the ConfigMap's values JSON.
// Revision is taken from the values JSON (top-level or global) or the ConfigMap label istio.io/rev.
// Returns an error if the values key is missing or the JSON cannot be parsed.
func ParseConfigMapValues(cm *corev1.ConfigMap) (*IstioValues, error) {
	raw, ok := cm.Data["values"]
	if !ok || raw == "" {
		return nil, fmt.Errorf("ConfigMap %s/%s has no 'values' key", cm.Namespace, cm.Name)
	}

	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, fmt.Errorf("failed to parse values JSON: %w", err)
	}

	vals := &IstioValues{}

	// Revision: prefer values JSON, then ConfigMap label istio.io/rev
	if v, ok := data["revision"].(string); ok && v != "" {
		vals.Revision = v
	}
	if vals.Revision == "" {
		if global, ok := data["global"].(map[string]interface{}); ok {
			if v, ok := global["revision"].(string); ok && v != "" {
				vals.Revision = v
			}
		}
	}
	if vals.Revision == "" && cm.Labels != nil {
		if v, ok := cm.Labels["istio.io/rev"]; ok && v != "" {
			vals.Revision = v
		}
	}
	if vals.Revision == "" {
		vals.Revision = "default"
	}

	if global, ok := data["global"].(map[string]interface{}); ok {
		if v, ok := global["hub"].(string); ok {
			vals.Hub = v
		}
		if v, ok := global["tag"].(string); ok {
			vals.Tag = v
		}
		if proxy, ok := global["proxy"].(map[string]interface{}); ok {
			if v, ok := proxy["image"].(string); ok {
				vals.Image = v
			}
		}
	}

	return vals, nil
}
