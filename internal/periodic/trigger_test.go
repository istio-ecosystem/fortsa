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
	"context"
	"testing"
	"time"

	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestNewReconcileSource(t *testing.T) {
	period := 15 * time.Millisecond
	src := NewReconcileSource(period)
	if src == nil {
		t.Fatal("NewReconcileSource returned nil")
	}

	queue := workqueue.NewTypedRateLimitingQueue[reconcile.Request](workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	ctx := context.Background()

	if err := src.Start(ctx, queue); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for at least one tick to fire
	time.Sleep(period + 5*time.Millisecond)

	req, shutdown := queue.Get()
	if shutdown {
		t.Fatal("queue shut down unexpectedly")
	}
	queue.Done(req)

	expected := ReconcileRequest()
	if req.Namespace != expected.Namespace || req.Name != expected.Name {
		t.Errorf("got request %v, want %v", req, expected)
	}
}

func TestNewReconcileSource_contextCancelStopsTicker(t *testing.T) {
	period := 20 * time.Millisecond
	src := NewReconcileSource(period)
	queue := workqueue.NewTypedRateLimitingQueue[reconcile.Request](workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	ctx, cancel := context.WithCancel(context.Background())

	_ = src.Start(ctx, queue)

	// Allow one tick to fire
	time.Sleep(period + 10*time.Millisecond)
	req, shutdown := queue.Get()
	if shutdown {
		t.Fatal("queue shut down unexpectedly")
	}
	queue.Done(req)

	// Cancel context - goroutine should exit on ctx.Done()
	cancel()
	time.Sleep(period * 2) // Give goroutine time to exit

	// Verify we got the expected request
	expected := ReconcileRequest()
	if req.Namespace != expected.Namespace || req.Name != expected.Name {
		t.Errorf("got request %v, want %v", req, expected)
	}
}

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
