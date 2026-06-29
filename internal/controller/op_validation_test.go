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

// Pure unit tests for the profile validation logic in op_validation.go.
// These tests do not start envtest and have no external dependencies.
package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// validProfileFixture returns a fully-valid OrchestrationProfile that passes all
// validation rules. Individual tests mutate a copy of this to isolate each rule.
func validProfileFixture() *orchestrationv1alpha1.OrchestrationProfile {
	return &orchestrationv1alpha1.OrchestrationProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "fixture"},
		Spec: orchestrationv1alpha1.OrchestrationProfileSpec{
			ApplicationRef: orchestrationv1alpha1.ApplicationReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "my-app",
				Namespace:  "default",
			},
			Placement: orchestrationv1alpha1.PlacementSpec{
				Strategy: "Balanced",
			},
		},
	}
}

var _ = Describe("Profile Validation", func() {
	// validateProfile only uses the Scheme from OrchestrationProfileReconciler,
	// which it never actually accesses during validation — a zero-value struct is enough.
	var r *OrchestrationProfileReconciler

	BeforeEach(func() {
		r = &OrchestrationProfileReconciler{}
	})

	Describe("valid profile", func() {
		It("produces no errors for a fully valid spec", func() {
			Expect(r.validateProfile(validProfileFixture())).To(BeEmpty())
		})
	})

	// -------------------------------------------------------------------------
	// applicationRef
	// -------------------------------------------------------------------------

	Describe("applicationRef validation", func() {
		It("rejects missing name", func() {
			p := validProfileFixture()
			p.Spec.ApplicationRef.Name = ""
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.applicationRef.name"))
		})

		It("rejects missing namespace", func() {
			p := validProfileFixture()
			p.Spec.ApplicationRef.Namespace = ""
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.applicationRef.namespace"))
		})

		It("rejects missing kind", func() {
			p := validProfileFixture()
			p.Spec.ApplicationRef.Kind = ""
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.applicationRef.kind"))
		})

		It("rejects an unsupported kind", func() {
			p := validProfileFixture()
			p.Spec.ApplicationRef.Kind = "DaemonSet"
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.applicationRef.kind"))
		})

		It("rejects missing apiVersion", func() {
			p := validProfileFixture()
			p.Spec.ApplicationRef.APIVersion = ""
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.applicationRef.apiVersion"))
		})

		It("rejects a malformed apiVersion", func() {
			p := validProfileFixture()
			p.Spec.ApplicationRef.APIVersion = "not/a/valid/group/version"
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.applicationRef.apiVersion"))
		})

		DescribeTable("accepts all supported applicationRef kinds",
			func(kind string) {
				p := validProfileFixture()
				p.Spec.ApplicationRef.Kind = kind
				Expect(r.validateProfile(p)).To(BeEmpty())
			},
			Entry("Deployment", "Deployment"),
			Entry("StatefulSet", "StatefulSet"),
			Entry("ReplicaSet", "ReplicaSet"),
			Entry("Job", "Job"),
		)
	})

	// -------------------------------------------------------------------------
	// placement
	// -------------------------------------------------------------------------

	Describe("placement validation", func() {
		It("rejects missing strategy", func() {
			p := validProfileFixture()
			p.Spec.Placement.Strategy = ""
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.placement.strategy"))
		})

		It("rejects an unsupported strategy", func() {
			p := validProfileFixture()
			p.Spec.Placement.Strategy = "Random"
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.placement.strategy"))
		})

		DescribeTable("accepts all valid strategies",
			func(strategy string) {
				p := validProfileFixture()
				p.Spec.Placement.Strategy = strategy
				Expect(r.validateProfile(p)).To(BeEmpty())
			},
			Entry("Balanced", "Balanced"),
			Entry("Packed", "Packed"),
			Entry("Spread", "Spread"),
		)
	})

	// -------------------------------------------------------------------------
	// rebalancing
	// -------------------------------------------------------------------------

	Describe("rebalancing validation", func() {
		It("skips all rebalancing checks when rebalancing is disabled", func() {
			p := validProfileFixture()
			p.Spec.Rebalancing.Enabled = false
			p.Spec.Rebalancing.CooldownSeconds = -99                        // would be invalid if enabled
			p.Spec.Rebalancing.TriggerConditions = []string{"BadCondition"} // would be invalid if enabled
			Expect(r.validateProfile(p)).To(BeEmpty())
		})

		It("rejects negative cooldownSeconds when rebalancing is enabled", func() {
			p := validProfileFixture()
			p.Spec.Rebalancing.Enabled = true
			p.Spec.Rebalancing.CooldownSeconds = -1
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			Expect(errs[0].Field).To(Equal("spec.rebalancing.cooldownSeconds"))
		})

		It("accepts zero cooldownSeconds when rebalancing is enabled", func() {
			p := validProfileFixture()
			p.Spec.Rebalancing.Enabled = true
			p.Spec.Rebalancing.CooldownSeconds = 0
			Expect(r.validateProfile(p)).To(BeEmpty())
		})

		It("rejects an unsupported trigger condition when rebalancing is enabled", func() {
			p := validProfileFixture()
			p.Spec.Rebalancing.Enabled = true
			p.Spec.Rebalancing.TriggerConditions = []string{"CPUThreshold", "InvalidCondition"}
			errs := r.validateProfile(p)
			Expect(errs).NotTo(BeEmpty())
			// The invalid entry is at index 1
			Expect(errs[0].Field).To(ContainSubstring("triggerConditions[1]"))
		})

		DescribeTable("accepts all valid trigger conditions",
			func(condition string) {
				p := validProfileFixture()
				p.Spec.Rebalancing.Enabled = true
				p.Spec.Rebalancing.TriggerConditions = []string{condition}
				Expect(r.validateProfile(p)).To(BeEmpty())
			},
			Entry("CPUThreshold", "CPUThreshold"),
			Entry("MemoryThreshold", "MemoryThreshold"),
			Entry("EnergyThreshold", "EnergyThreshold"),
			Entry("NodeFailure", "NodeFailure"),
			Entry("Scheduled", "Scheduled"),
		)
	})
})
