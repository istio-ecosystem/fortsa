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

package cache

import (
	"testing"
	"time"
)

func TestRevisionCache_LastModifiedChanged(t *testing.T) {
	c := NewRevisionCache()

	t1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 1, 15, 11, 0, 0, 0, time.UTC)

	// First call: no cache, should report changed
	if !c.LastModifiedChanged("default", t1) {
		t.Error("LastModifiedChanged: want true (no cache)")
	}

	// Set cache
	c.Set("istio-system/cm", "default", t1)

	// Same timestamp: should report not changed
	if c.LastModifiedChanged("default", t1) {
		t.Error("LastModifiedChanged: want false (same timestamp)")
	}

	// Different timestamp: should report changed
	if !c.LastModifiedChanged("default", t2) {
		t.Error("LastModifiedChanged: want true (timestamp changed)")
	}
}

func TestRevisionCache_GetCopy(t *testing.T) {
	c := NewRevisionCache()
	t1 := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	c.Set("istio-system/cm", "default", t1)

	copy := c.GetCopy()
	if len(copy) != 1 {
		t.Fatalf("GetCopy: want 1 entry, got %d", len(copy))
	}
	if !copy["default"].Equal(t1) {
		t.Errorf("GetCopy: want %v, got %v", t1, copy["default"])
	}
}

func TestRevisionCache_ClearByConfigMap(t *testing.T) {
	c := NewRevisionCache()
	c.Set("istio-system/cm", "default", time.Now())

	c.ClearByConfigMap("istio-system/cm")
	copy := c.GetCopy()
	if len(copy) != 0 {
		t.Errorf("ClearByConfigMap: want empty cache, got %d entries", len(copy))
	}
}
