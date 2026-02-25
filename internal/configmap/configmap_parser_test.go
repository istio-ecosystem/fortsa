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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseConfigMapValues(t *testing.T) {
	tests := []struct {
		name    string
		cm      *corev1.ConfigMap
		want    *IstioValues
		wantErr bool
	}{
		{
			name: "full values from JSON",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
				Data: map[string]string{
					"values": `{
						"revision": "canary",
						"global": {
							"hub": "docker.io/istio",
							"tag": "1.20.1",
							"proxy": {"image": "proxyv2"}
						}
					}`,
				},
			},
			want: &IstioValues{Revision: "canary", Hub: "docker.io/istio", Tag: "1.20.1", Image: "proxyv2"},
		},
		{
			name: "revision from ConfigMap label when missing in JSON",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "istio-system",
					Name:      "istio-sidecar-injector",
					Labels:    map[string]string{"istio.io/rev": "1-18"},
				},
				Data: map[string]string{
					"values": `{"global": {"hub": "gcr.io/istio", "tag": "1.18.0", "proxy": {"image": "proxyv2"}}}`,
				},
			},
			want: &IstioValues{Revision: "1-18", Hub: "gcr.io/istio", Tag: "1.18.0", Image: "proxyv2"},
		},
		{
			name: "default revision when not specified",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
				Data: map[string]string{
					"values": `{"global": {"hub": "docker.io/istio", "tag": "1.20.0", "proxy": {"image": "proxyv2"}}}`,
				},
			},
			want: &IstioValues{Revision: "default", Hub: "docker.io/istio", Tag: "1.20.0", Image: "proxyv2"},
		},
		{
			name: "revision from global when not at top level",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
				Data: map[string]string{
					"values": `{"global": {"revision": "staging", "hub": "registry.example.com", "tag": "v2", "proxy": {"image": "proxyv2"}}}`,
				},
			},
			want: &IstioValues{Revision: "staging", Hub: "registry.example.com", Tag: "v2", Image: "proxyv2"},
		},
		{
			name: "missing values key",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
				Data:       map[string]string{},
			},
			wantErr: true,
		},
		{
			name: "invalid JSON",
			cm: &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Namespace: "istio-system", Name: "istio-sidecar-injector"},
				Data:       map[string]string{"values": `{invalid json}`},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfigMapValues(tt.cm)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseConfigMapValues() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			if got.Revision != tt.want.Revision || got.Hub != tt.want.Hub || got.Tag != tt.want.Tag || got.Image != tt.want.Image {
				t.Errorf("ParseConfigMapValues() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestGetConfigMapLastModified(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	laterTime := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)

	t.Run("uses creationTimestamp when no managedFields", func(t *testing.T) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.NewTime(baseTime),
			},
		}
		got := GetConfigMapLastModified(cm)
		if !got.Equal(baseTime) {
			t.Errorf("GetConfigMapLastModified() = %v, want %v", got, baseTime)
		}
	})

	t.Run("uses latest managedFields time when present", func(t *testing.T) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				CreationTimestamp: metav1.NewTime(baseTime),
				ManagedFields: []metav1.ManagedFieldsEntry{
					{Time: &metav1.Time{Time: baseTime}},
					{Time: &metav1.Time{Time: laterTime}},
				},
			},
		}
		got := GetConfigMapLastModified(cm)
		if !got.Equal(laterTime) {
			t.Errorf("GetConfigMapLastModified() = %v, want %v", got, laterTime)
		}
	})
}
