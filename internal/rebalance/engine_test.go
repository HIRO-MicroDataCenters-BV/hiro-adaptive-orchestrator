/*
Copyright 2026 HIRO Adaptive Orchestrator.

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

package rebalance

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

func TestEngine_ImplementsManagerRunnable(t *testing.T) {
	var _ manager.Runnable = (*Engine)(nil)
	var _ manager.LeaderElectionRunnable = (*Engine)(nil)
}

func TestEngine_StartStopsCleanlyOnContextCancel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := orchestrationv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	writer := NewStateWriter(c, nil)
	engine := NewEngine(c, writer)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- engine.Start(ctx) }()

	// Give Start a moment to enter its blocking wait before cancelling.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not stop within 2s of context cancellation")
	}
}

func TestEngine_NeedLeaderElection(t *testing.T) {
	engine := NewEngine(nil, nil)
	if !engine.NeedLeaderElection() {
		t.Error("NeedLeaderElection() = false, want true")
	}
}
