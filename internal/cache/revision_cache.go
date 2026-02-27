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
	"sync"
	"time"
)

// RevisionCache stores ConfigMap revision-to-LastModified mappings for change detection
// and pod skip logic. It also tracks configMapKey-to-revision for delete cleanup.
type RevisionCache struct {
	revisionToLastModified map[string]time.Time
	nameToRevision         map[string]string
	mu                     sync.RWMutex
}

// NewRevisionCache creates a new RevisionCache.
func NewRevisionCache() *RevisionCache {
	return &RevisionCache{
		revisionToLastModified: make(map[string]time.Time),
		nameToRevision:         make(map[string]string),
	}
}

// LastModifiedChanged returns true if the revision's lastModified differs from the cached value.
// Returns true when the revision is not yet cached (first time seen).
func (c *RevisionCache) LastModifiedChanged(revision string, lastModified time.Time) bool {
	c.mu.RLock()
	prev := c.revisionToLastModified[revision]
	c.mu.RUnlock()

	if prev.IsZero() {
		return true
	}
	return !prev.Equal(lastModified)
}

// Set stores the configMapKey-to-revision and revision-to-lastModified mappings.
func (c *RevisionCache) Set(configMapKey, revision string, lastModified time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revisionToLastModified[revision] = lastModified
	c.nameToRevision[configMapKey] = revision
}

// GetCopy returns a shallow copy of the revision-to-LastModified map for safe concurrent use.
func (c *RevisionCache) GetCopy() map[string]time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	copy := make(map[string]time.Time, len(c.revisionToLastModified))
	for k, v := range c.revisionToLastModified {
		copy[k] = v
	}
	return copy
}

// ClearByConfigMap removes cache entries for the given configMapKey.
func (c *RevisionCache) ClearByConfigMap(configMapKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if revision, ok := c.nameToRevision[configMapKey]; ok {
		delete(c.revisionToLastModified, revision)
		delete(c.nameToRevision, configMapKey)
	}
}
