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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
	placementserver "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/placement-server"
	"github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/internal/utils"
)

// DefaultMoveActionTimeout bounds how long moveEnactor waits, after a
// successful eviction, for a scheduled replacement pod to appear.
const DefaultMoveActionTimeout = 60 * time.Second

// moveActionPollInterval is how often awaitReplacement re-lists pods while
// waiting. Not configurable — short enough not to meaningfully delay
// detection, long enough not to hammer the API server.
const moveActionPollInterval = 2 * time.Second

// moveResult is what moveEnactor decides happened, for the caller
// (dispatchDecision) to translate into a terminal StateWriter transition.
type moveResult struct {
	outcome orchestrationv1alpha1.RebalanceOutcome
	reason  string
}

// Never Ever delete this comments as they are used by kubebuilder to generate RBAC permissions for the controller.
// If you need to change the permissions,
// modify the verbs and resources in the comments below and then run "make generate" to update the generated code.

// +kubebuilder:rbac:groups="",resources=pods/eviction,verbs=create

// moveEnactor evicts podName (a currently Running pod of profile's
// application) via the PDB-aware Eviction API — not a raw delete, unlike
// retryPendingSchedule, since the pod being moved is Ready and eviction is
// what makes that safe — so its owning controller creates a replacement,
// then waits for that replacement to be scheduled.
//
// Records a decision-store entry (keyed by workload identity, see
// decision_store.go) before evicting, so PlacementServer.score biases the
// replacement toward targetNode instead of scoring it fresh. That store
// entry is this function's only way to influence where the replacement
// lands — Kubernetes gives no direct way to pin a specific pod to a specific
// node through the normal scheduling path.
//
// Returns a moveResult describing the terminal outcome to record — never an
// error for anything that happened *after* the store write succeeded, since
// every one of those cases (PDB refusal, wrong node, timeout) is a defined
// outcome the caller writes to status, not a Go error. A non-nil error means
// the store write itself couldn't even be attempted (the target pod no
// longer exists) or the pre-eviction pod list couldn't be read — eviction
// was never attempted either way.
func moveEnactor(
	ctx context.Context,
	c client.Client,
	store *placementserver.DecisionStore,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	podName, targetNode, reason, decisionID string,
	replacementTimeout time.Duration,
) (moveResult, error) {
	if replacementTimeout <= 0 {
		replacementTimeout = DefaultMoveActionTimeout
	}

	before, err := utils.FindPodsForApplication(ctx, c, profile.Spec.ApplicationRef)
	if err != nil {
		return moveResult{}, fmt.Errorf("listing pods before eviction: %w", err)
	}

	pod := findPodByName(before, podName)
	if pod == nil {
		return moveResult{}, fmt.Errorf("pod %s not found among %s/%s's current pods",
			podName, profile.Spec.ApplicationRef.Namespace, profile.Spec.ApplicationRef.Name)
	}

	beforeNames := make(map[string]bool, len(before))
	for _, p := range before {
		beforeNames[p.Name] = true
	}

	key := placementserver.WorkloadKey(profile.Namespace, profile.Name)
	store.Put(key, podName, orchestrationv1alpha1.RebalanceActionMove, targetNode, reason, decisionID)

	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
	}
	if err := c.SubResource("eviction").Create(ctx, pod, eviction); err != nil {
		store.Delete(key)
		if apierrors.IsTooManyRequests(err) {
			return moveResult{
				outcome: OutcomeDeferred,
				reason:  fmt.Sprintf("eviction blocked by PodDisruptionBudget: %v", err),
			}, nil
		}
		return moveResult{}, fmt.Errorf("evicting pod %s: %w", pod.Name, err)
	}

	replacement, err := awaitReplacement(ctx, c, profile, beforeNames, replacementTimeout)
	if err != nil {
		store.Delete(key)
		return moveResult{outcome: OutcomeFailed, reason: err.Error()}, nil
	}

	// No-op if PlacementServer.score already consumed the entry when it
	// scored the replacement — see DecisionStore's doc comment on why
	// self-consuming on lookup, not this delete, is the primary cleanup
	// path. This call is the backstop for whichever case reaches here
	// without a Lookup ever having happened (e.g. the replacement landed
	// through a path that didn't go through score at all).
	store.Delete(key)

	if replacement.Spec.NodeName != targetNode {
		return moveResult{
			outcome: OutcomeFailed,
			reason: fmt.Sprintf("intent not honoured: replacement %s landed on %s, wanted %s",
				replacement.Name, replacement.Spec.NodeName, targetNode),
		}, nil
	}
	return moveResult{outcome: OutcomeEnacted, reason: fmt.Sprintf("moved to %s: %s", targetNode, reason)}, nil
}

func findPodByName(pods []corev1.Pod, name string) *corev1.Pod {
	for i := range pods {
		if pods[i].Name == name {
			return &pods[i]
		}
	}
	return nil
}

// awaitReplacement polls for a pod of profile's application that wasn't in
// beforeNames (i.e. created after eviction) and has been scheduled to a
// node, up to timeout.
func awaitReplacement(
	ctx context.Context,
	c client.Client,
	profile *orchestrationv1alpha1.OrchestrationProfile,
	beforeNames map[string]bool,
	timeout time.Duration,
) (*corev1.Pod, error) {
	var found *corev1.Pod
	pollErr := wait.PollUntilContextTimeout(ctx, moveActionPollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := utils.FindPodsForApplication(ctx, c, profile.Spec.ApplicationRef)
			if err != nil {
				return false, nil // transient list error — keep polling until timeout
			}
			for i := range pods {
				p := &pods[i]
				if beforeNames[p.Name] || p.Spec.NodeName == "" {
					continue // not new, or new but not scheduled yet
				}
				found = p
				return true, nil
			}
			return false, nil
		})
	if pollErr != nil {
		return nil, fmt.Errorf("no scheduled replacement observed within %s", timeout)
	}
	return found, nil
}
