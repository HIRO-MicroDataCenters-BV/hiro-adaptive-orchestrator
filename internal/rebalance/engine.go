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

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// Engine is the rebalance engine's runtime loop. It is registered with the
// controller-runtime Manager as a manager.Runnable so its lifecycle (start
// after the manager's cache syncs, stop when the manager's context is
// cancelled) is managed the same way as every other component in this
// operator.
//
// Detection (hybrid trigger evaluation), Decision (AI call + guardrail), and
// Enaction (enactors) are added in later stories; this skeleton only proves
// out registration, startup, and shutdown.
type Engine struct {
	client client.Client
	writer *StateWriter
}

var (
	_ manager.Runnable               = (*Engine)(nil)
	_ manager.LeaderElectionRunnable = (*Engine)(nil)
)

// NewEngine creates a rebalance Engine.
func NewEngine(c client.Client, writer *StateWriter) *Engine {
	return &Engine{client: c, writer: writer}
}

// Start blocks until ctx is cancelled. Called by the Manager once the
// informer cache has synced.
func (e *Engine) Start(ctx context.Context) error {
	logger := logf.FromContext(ctx).WithName("rebalance-engine")
	logger.Info("rebalance engine starting")

	<-ctx.Done()

	logger.Info("rebalance engine stopping")
	return nil
}

// NeedLeaderElection reports that only the leader replica should run the
// engine. If the operator runs with leader election disabled (single
// replica, the current default), the Manager runs all runnables regardless
// of this value — it only matters once/if the operator moves to HA, at
// which point a shared decision store would also need revisiting (see the
// rebalance engine design notes).
func (e *Engine) NeedLeaderElection() bool { return true }
