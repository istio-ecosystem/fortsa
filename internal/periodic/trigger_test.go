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

package periodic

import (
	"testing"
)

func TestReconcileRequest(t *testing.T) {
	req := ReconcileRequest()
	if req.Namespace != "istio-system" {
		t.Errorf("ReconcileRequest namespace = %q, want istio-system", req.Namespace)
	}
	if req.Name != "__periodic_reconcile__" {
		t.Errorf("ReconcileRequest name = %q, want __periodic_reconcile__", req.Name)
	}
}

func TestReconcileRequestName(t *testing.T) {
	name := ReconcileRequestName()
	if name != "__periodic_reconcile__" {
		t.Errorf("ReconcileRequestName = %q, want __periodic_reconcile__", name)
	}
}
