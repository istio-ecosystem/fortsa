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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestBuildLastModifiedByRevision(t *testing.T) {
	baseTime := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "istio-system",
			Name:              "istio-sidecar-injector",
			CreationTimestamp: metav1.NewTime(baseTime),
		},
		Data: map[string]string{
			"values": `{"revision":"default","global":{"hub":"docker.io/istio","tag":"1.20.1","proxy":{"image":"proxyv2"}}}`,
		},
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()

	got, err := BuildLastModifiedByRevision(context.Background(), fakeClient)
	if err != nil {
		t.Fatalf("BuildLastModifiedByRevision() error = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("BuildLastModifiedByRevision() want 1 entry, got %d", len(got))
	}
	if lm, ok := got["default"]; !ok || !lm.Equal(baseTime) {
		t.Errorf("BuildLastModifiedByRevision()[\"default\"] = %v, want %v", got["default"], baseTime)
	}
}
