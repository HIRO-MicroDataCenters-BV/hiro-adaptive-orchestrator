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

// Integration tests proving the CRD's OpenAPI schema enforces the
// rebalancing decision-lifecycle enums: the active-state enum
// (status.rebalancingStatus.state) and the separate terminal-outcome enum
// (status.rebalancingStatus.recentDecisions[].outcome).
// Uses the same envtest apiserver as orchestrationprofile_controller_test.go.
package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

var _ = Describe("OrchestrationProfile rebalancingStatus.state CRD validation", func() {
	var testCtx context.Context

	BeforeEach(func() {
		testCtx = context.Background()
	})

	newProfile := func(name string) *orchestrationv1alpha1.OrchestrationProfile {
		return &orchestrationv1alpha1.OrchestrationProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       baseSpec(name),
		}
	}

	DescribeTable("accepts every valid decision-lifecycle state",
		func(state orchestrationv1alpha1.RebalancingStateType) {
			profile := newProfile("rebalance-state-valid-" + strings.ToLower(string(state)))
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())

			profile.Status.RebalancingStatus.State = state
			Expect(k8sClient.Status().Update(testCtx, profile)).To(Succeed())
		},
		Entry("Watching", orchestrationv1alpha1.RebalancingStateWatching),
		Entry("Triggered", orchestrationv1alpha1.RebalancingStateTriggered),
		Entry("Evaluating", orchestrationv1alpha1.RebalancingStateEvaluating),
		Entry("Decided", orchestrationv1alpha1.RebalancingStateDecided),
		Entry("Enacting", orchestrationv1alpha1.RebalancingStateEnacting),
	)

	It("rejects an unrecognized state value", func() {
		profile := newProfile("rebalance-state-invalid")
		Expect(k8sClient.Create(testCtx, profile)).To(Succeed())

		profile.Status.RebalancingStatus.State = "NotARealState"
		err := k8sClient.Status().Update(testCtx, profile)
		Expect(err).To(HaveOccurred())
	})

	It("rejects a legacy terminal state value no longer valid as a state", func() {
		profile := newProfile("rebalance-state-legacy-terminal")
		Expect(k8sClient.Create(testCtx, profile)).To(Succeed())

		profile.Status.RebalancingStatus.State = "Enacted"
		err := k8sClient.Status().Update(testCtx, profile)
		Expect(err).To(HaveOccurred())
	})

	DescribeTable("accepts every valid recentDecisions outcome",
		func(outcome orchestrationv1alpha1.RebalanceOutcome) {
			profile := newProfile("rebalance-outcome-valid-" + strings.ToLower(string(outcome)))
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())

			profile.Status.RebalancingStatus.RecentDecisions = []orchestrationv1alpha1.RebalanceDecision{
				{DecisionID: "d1", Outcome: outcome},
			}
			Expect(k8sClient.Status().Update(testCtx, profile)).To(Succeed())
		},
		Entry("Enacted", orchestrationv1alpha1.RebalanceOutcomeEnacted),
		Entry("NoOp", orchestrationv1alpha1.RebalanceOutcomeNoOp),
		Entry("Rejected", orchestrationv1alpha1.RebalanceOutcomeRejected),
		Entry("Deferred", orchestrationv1alpha1.RebalanceOutcomeDeferred),
		Entry("Failed", orchestrationv1alpha1.RebalanceOutcomeFailed),
	)

	It("rejects an unrecognized outcome inside a recentDecisions entry", func() {
		profile := newProfile("rebalance-outcome-invalid-history")
		Expect(k8sClient.Create(testCtx, profile)).To(Succeed())

		profile.Status.RebalancingStatus.RecentDecisions = []orchestrationv1alpha1.RebalanceDecision{
			{DecisionID: "d1", Outcome: "NotARealOutcome"},
		}
		err := k8sClient.Status().Update(testCtx, profile)
		Expect(err).To(HaveOccurred())
	})
})
