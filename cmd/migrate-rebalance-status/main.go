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

// Command migrate-rebalance-status is a one-time, operator-run tool that
// rewrites every OrchestrationProfile whose status.rebalancingStatus.state
// still holds a legacy terminal value (Enacted, NoOp, Rejected, Deferred,
// Failed — valid under the old 9-value state enum) into the current model:
// state=Watching plus a RecentDecisions entry carrying that value as an
// Outcome instead.
//
// Run order matters:
//  1. Apply the updated CRD (the narrowed 5-value state enum) first —
//     Kubernetes does not re-validate objects already stored in etcd, so
//     existing legacy-state profiles remain readable after this.
//  2. Run this command. Every write it makes sets state to Watching, which
//     is valid under both the old and new enum, so it succeeds regardless
//     of whether the new CRD has landed yet — but should still run after
//     step 1 so nothing else observes a profile still on the old model.
//  3. Roll out the operator build with the 5-state reconciler/writer code.
//
// Safe to re-run: profiles already on the new model (state != one of the
// legacy values) are left untouched.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// legacyOutcomes maps every state value that only existed under the old
// 9-value enum to the RebalanceOutcome it becomes under the new model.
// Triggered/Evaluating/Decided/Enacting are excluded on purpose: a profile
// caught mid-cycle when this runs has no terminal outcome to record yet, so
// it's left as-is and will run to completion under the new reconciler.
var legacyOutcomes = map[orchestrationv1alpha1.RebalancingStateType]orchestrationv1alpha1.RebalanceOutcome{
	"Enacted":  orchestrationv1alpha1.RebalanceOutcomeEnacted,
	"NoOp":     orchestrationv1alpha1.RebalanceOutcomeNoOp,
	"Rejected": orchestrationv1alpha1.RebalanceOutcomeRejected,
	"Deferred": orchestrationv1alpha1.RebalanceOutcomeDeferred,
	"Failed":   orchestrationv1alpha1.RebalanceOutcomeFailed,
}

func main() {
	var dryRun bool
	flag.BoolVar(&dryRun, "dry-run", true,
		"List what would change without writing. Pass -dry-run=false to actually migrate.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	logf.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := logf.Log.WithName("migrate-rebalance-status")

	scheme := runtime.NewScheme()
	utilruntime.Must(orchestrationv1alpha1.AddToScheme(scheme))

	restConfig := ctrl.GetConfigOrDie()
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "building client")
		os.Exit(1)
	}

	ctx := context.Background()
	list := &orchestrationv1alpha1.OrchestrationProfileList{}
	if err := c.List(ctx, list); err != nil {
		log.Error(err, "listing OrchestrationProfiles")
		os.Exit(1)
	}

	var migrated, skipped, failed int
	for i := range list.Items {
		profile := &list.Items[i]
		outcome, legacy := legacyOutcomes[profile.Status.RebalancingStatus.State]
		if !legacy {
			skipped++
			continue
		}

		log.Info("migrating profile",
			"profile", profile.Name, "legacyState", profile.Status.RebalancingStatus.State,
			"outcome", outcome, "dryRun", dryRun,
		)
		if dryRun {
			migrated++
			continue
		}

		if err := migrateProfile(ctx, c, profile.Name, outcome); err != nil {
			log.Error(err, "migrating profile failed", "profile", profile.Name)
			failed++
			continue
		}
		migrated++
	}

	log.Info("migration complete",
		"totalProfiles", len(list.Items), "migrated", migrated, "skipped", skipped, "failed", failed, "dryRun", dryRun,
	)
	if failed > 0 {
		os.Exit(1)
	}
}

// migrateProfile re-fetches the profile by name and retries on write
// conflicts, mirroring internal/rebalance/writer.go's StateWriter — this
// tool runs once, outside the reconciler, but the same retry discipline
// applies to any status write.
func migrateProfile(
	ctx context.Context,
	c client.Client,
	name string,
	outcome orchestrationv1alpha1.RebalanceOutcome,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		profile := &orchestrationv1alpha1.OrchestrationProfile{}
		if err := c.Get(ctx, client.ObjectKey{Name: name}, profile); err != nil {
			return fmt.Errorf("fetching profile %s: %w", name, err)
		}

		rs := &profile.Status.RebalancingStatus
		if _, legacy := legacyOutcomes[rs.State]; !legacy {
			return nil // already migrated by a concurrent run, or by the new reconciler
		}

		rs.RecentDecisions = append([]orchestrationv1alpha1.RebalanceDecision{{
			DecisionID:       rs.DecisionID,
			Outcome:          outcome,
			Action:           rs.Action,
			Reason:           rs.Reason,
			Details:          rs.Details,
			StartedAt:        rs.StartedAt,
			LastTransitionAt: rs.LastTransitionAt,
		}}, rs.RecentDecisions...)
		rs.State = orchestrationv1alpha1.RebalancingStateWatching
		rs.LastTransitionAt = metav1.Now()

		return c.Status().Update(ctx, profile)
	})
}
