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

// Package controller contains integration tests for OrchestrationProfileReconciler.
// These tests use controller-runtime/envtest which starts a real etcd + kube-apiserver
// in-process — no external cluster is required.
//
// Each Context creates its own uniquely-named resources so tests never interfere
// with each other, even when run in parallel.
package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// makeDeployment creates a bare Deployment with a label selector of {"app": name}.
// envtest does not run the Deployment controller, so no ReplicaSet or Pods are
// created automatically — tests control pod creation explicitly.
func makeDeployment(name, namespace string) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
				},
			},
		},
	}
}

// makePod creates a pod with the given labels and sets its Phase via the status
// subresource after creation. ready=true additionally sets the PodReady condition.
func makePod(ctx context.Context, name, namespace string, labels map[string]string, phase corev1.PodPhase, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "nginx:latest"}},
		},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())

	pod.Status.Phase = phase
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	return pod
}

// reconcileRequest builds the reconcile.Request for a cluster-scoped profile.
func reconcileRequest(name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: name}}
}

var _ = Describe("OrchestrationProfile Reconciler", func() {
	var testCtx context.Context

	BeforeEach(func() {
		testCtx = context.Background()
	})

	// -------------------------------------------------------------------------
	// Profile not found
	// -------------------------------------------------------------------------

	Context("when the profile has already been deleted", func() {
		It("returns no error", func() {
			_, err := newReconciler().Reconcile(testCtx, reconcileRequest("does-not-exist"))
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// -------------------------------------------------------------------------
	// Spec validation failure
	// -------------------------------------------------------------------------

	Context("when the profile spec contains an unsupported applicationRef kind", func() {
		const profileName = "invalid-kind-profile"

		BeforeEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{
				ObjectMeta: metav1.ObjectMeta{Name: profileName},
				Spec: orchestrationv1alpha1.OrchestrationProfileSpec{
					ApplicationRef: orchestrationv1alpha1.ApplicationReference{
						APIVersion: "apps/v1",
						Kind:       "DaemonSet", // not in supportedAppKinds
						Name:       "my-app",
						Namespace:  "default",
					},
					Placement: orchestrationv1alpha1.PlacementSpec{Strategy: "Balanced"},
				},
			}
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())
		})

		AfterEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile); err == nil {
				Expect(k8sClient.Delete(testCtx, profile)).To(Succeed())
			}
		})

		It("sets status to Error with a non-empty reason", func() {
			_, err := newReconciler().Reconcile(testCtx, reconcileRequest(profileName))
			Expect(err).NotTo(HaveOccurred())

			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile)).To(Succeed())
			Expect(profile.Status.Status).To(Equal(StatusError))
			Expect(profile.Status.Reason).NotTo(BeEmpty())
		})
	})

	// -------------------------------------------------------------------------
	// Referenced application does not exist
	// -------------------------------------------------------------------------

	Context("when the referenced Deployment does not exist", func() {
		const profileName = "app-missing-profile"

		BeforeEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{
				ObjectMeta: metav1.ObjectMeta{Name: profileName},
				Spec:       baseSpec("deployment-that-does-not-exist"),
			}
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())
		})

		AfterEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile); err == nil {
				Expect(k8sClient.Delete(testCtx, profile)).To(Succeed())
			}
		})

		It("sets status to NoPods", func() {
			_, err := newReconciler().Reconcile(testCtx, reconcileRequest(profileName))
			Expect(err).NotTo(HaveOccurred())

			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile)).To(Succeed())
			Expect(profile.Status.Status).To(Equal(StatusNoPods))
		})
	})

	// -------------------------------------------------------------------------
	// Application exists but has no pods yet
	// -------------------------------------------------------------------------

	Context("when the Deployment exists but no pods have been scheduled", func() {
		const (
			profileName = "no-pods-profile"
			deployName  = "no-pods-deploy"
		)

		BeforeEach(func() {
			Expect(k8sClient.Create(testCtx, makeDeployment(deployName, "default"))).To(Succeed())
			profile := &orchestrationv1alpha1.OrchestrationProfile{
				ObjectMeta: metav1.ObjectMeta{Name: profileName},
				Spec:       baseSpec(deployName),
			}
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())
		})

		AfterEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile); err == nil {
				Expect(k8sClient.Delete(testCtx, profile)).To(Succeed())
			}
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: deployName, Namespace: "default"}, deploy); err == nil {
				Expect(k8sClient.Delete(testCtx, deploy)).To(Succeed())
			}
		})

		It("sets status to NoPods", func() {
			_, err := newReconciler().Reconcile(testCtx, reconcileRequest(profileName))
			Expect(err).NotTo(HaveOccurred())

			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile)).To(Succeed())
			Expect(profile.Status.Status).To(Equal(StatusNoPods))
			Expect(profile.Status.PlacementStatus.ObservedPods).To(Equal(0))
		})
	})

	// -------------------------------------------------------------------------
	// All pods ready → Active
	// -------------------------------------------------------------------------

	Context("when all pods belonging to the Deployment are Running and Ready", func() {
		const (
			profileName = "all-ready-profile"
			deployName  = "all-ready-deploy"
		)

		BeforeEach(func() {
			labels := map[string]string{"app": deployName}
			Expect(k8sClient.Create(testCtx, makeDeployment(deployName, "default"))).To(Succeed())
			makePod(testCtx, deployName+"-pod-1", "default", labels, corev1.PodRunning, true)
			makePod(testCtx, deployName+"-pod-2", "default", labels, corev1.PodRunning, true)
			profile := &orchestrationv1alpha1.OrchestrationProfile{
				ObjectMeta: metav1.ObjectMeta{Name: profileName},
				Spec:       baseSpec(deployName),
			}
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())
		})

		AfterEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile); err == nil {
				Expect(k8sClient.Delete(testCtx, profile)).To(Succeed())
			}
			for _, podName := range []string{deployName + "-pod-1", deployName + "-pod-2"} {
				pod := &corev1.Pod{}
				if err := k8sClient.Get(testCtx, types.NamespacedName{Name: podName, Namespace: "default"}, pod); err == nil {
					Expect(k8sClient.Delete(testCtx, pod)).To(Succeed())
				}
			}
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: deployName, Namespace: "default"}, deploy); err == nil {
				Expect(k8sClient.Delete(testCtx, deploy)).To(Succeed())
			}
		})

		It("sets status to Active with correct pod counts", func() {
			_, err := newReconciler().Reconcile(testCtx, reconcileRequest(profileName))
			Expect(err).NotTo(HaveOccurred())

			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile)).To(Succeed())
			Expect(profile.Status.Status).To(Equal(StatusActive))
			Expect(profile.Status.PlacementStatus.ObservedPods).To(Equal(2))
			Expect(profile.Status.PlacementStatus.ReadyPods).To(Equal(2))
			Expect(profile.Status.PlacementStatus.PendingPods).To(Equal(0))
			Expect(profile.Status.PlacementStatus.FailedPods).To(Equal(0))
			Expect(profile.Status.PlacementStatus.Strategy).To(Equal("Balanced"))
		})
	})

	// -------------------------------------------------------------------------
	// All pods pending → Pending
	// -------------------------------------------------------------------------

	Context("when all pods are in Pending phase", func() {
		const (
			profileName = "all-pending-profile"
			deployName  = "all-pending-deploy"
		)

		BeforeEach(func() {
			labels := map[string]string{"app": deployName}
			Expect(k8sClient.Create(testCtx, makeDeployment(deployName, "default"))).To(Succeed())
			makePod(testCtx, deployName+"-pod-1", "default", labels, corev1.PodPending, false)
			profile := &orchestrationv1alpha1.OrchestrationProfile{
				ObjectMeta: metav1.ObjectMeta{Name: profileName},
				Spec:       baseSpec(deployName),
			}
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())
		})

		AfterEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile); err == nil {
				Expect(k8sClient.Delete(testCtx, profile)).To(Succeed())
			}
			pod := &corev1.Pod{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: deployName + "-pod-1", Namespace: "default"}, pod); err == nil {
				Expect(k8sClient.Delete(testCtx, pod)).To(Succeed())
			}
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: deployName, Namespace: "default"}, deploy); err == nil {
				Expect(k8sClient.Delete(testCtx, deploy)).To(Succeed())
			}
		})

		It("sets status to Pending", func() {
			_, err := newReconciler().Reconcile(testCtx, reconcileRequest(profileName))
			Expect(err).NotTo(HaveOccurred())

			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile)).To(Succeed())
			Expect(profile.Status.Status).To(Equal(StatusPending))
			Expect(profile.Status.PlacementStatus.PendingPods).To(Equal(1))
		})
	})

	// -------------------------------------------------------------------------
	// All pods failed → Degraded
	// -------------------------------------------------------------------------

	Context("when all pods have Failed", func() {
		const (
			profileName = "all-failed-profile"
			deployName  = "all-failed-deploy"
		)

		BeforeEach(func() {
			labels := map[string]string{"app": deployName}
			Expect(k8sClient.Create(testCtx, makeDeployment(deployName, "default"))).To(Succeed())
			makePod(testCtx, deployName+"-pod-1", "default", labels, corev1.PodFailed, false)
			profile := &orchestrationv1alpha1.OrchestrationProfile{
				ObjectMeta: metav1.ObjectMeta{Name: profileName},
				Spec:       baseSpec(deployName),
			}
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())
		})

		AfterEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile); err == nil {
				Expect(k8sClient.Delete(testCtx, profile)).To(Succeed())
			}
			pod := &corev1.Pod{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: deployName + "-pod-1", Namespace: "default"}, pod); err == nil {
				Expect(k8sClient.Delete(testCtx, pod)).To(Succeed())
			}
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: deployName, Namespace: "default"}, deploy); err == nil {
				Expect(k8sClient.Delete(testCtx, deploy)).To(Succeed())
			}
		})

		It("sets status to Degraded", func() {
			_, err := newReconciler().Reconcile(testCtx, reconcileRequest(profileName))
			Expect(err).NotTo(HaveOccurred())

			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile)).To(Succeed())
			Expect(profile.Status.Status).To(Equal(StatusDegraded))
			Expect(profile.Status.PlacementStatus.FailedPods).To(Equal(1))
		})
	})

	// -------------------------------------------------------------------------
	// Mixed ready + pending → Partial
	// -------------------------------------------------------------------------

	Context("when some pods are ready and others are pending", func() {
		const (
			profileName = "partial-profile"
			deployName  = "partial-deploy"
		)

		BeforeEach(func() {
			labels := map[string]string{"app": deployName}
			Expect(k8sClient.Create(testCtx, makeDeployment(deployName, "default"))).To(Succeed())
			makePod(testCtx, deployName+"-pod-ready", "default", labels, corev1.PodRunning, true)
			makePod(testCtx, deployName+"-pod-pending", "default", labels, corev1.PodPending, false)
			profile := &orchestrationv1alpha1.OrchestrationProfile{
				ObjectMeta: metav1.ObjectMeta{Name: profileName},
				Spec:       baseSpec(deployName),
			}
			Expect(k8sClient.Create(testCtx, profile)).To(Succeed())
		})

		AfterEach(func() {
			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile); err == nil {
				Expect(k8sClient.Delete(testCtx, profile)).To(Succeed())
			}
			for _, podName := range []string{deployName + "-pod-ready", deployName + "-pod-pending"} {
				pod := &corev1.Pod{}
				if err := k8sClient.Get(testCtx, types.NamespacedName{Name: podName, Namespace: "default"}, pod); err == nil {
					Expect(k8sClient.Delete(testCtx, pod)).To(Succeed())
				}
			}
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: deployName, Namespace: "default"}, deploy); err == nil {
				Expect(k8sClient.Delete(testCtx, deploy)).To(Succeed())
			}
		})

		It("sets status to Partial", func() {
			_, err := newReconciler().Reconcile(testCtx, reconcileRequest(profileName))
			Expect(err).NotTo(HaveOccurred())

			profile := &orchestrationv1alpha1.OrchestrationProfile{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: profileName}, profile)).To(Succeed())
			Expect(profile.Status.Status).To(Equal(StatusPartial))
			Expect(profile.Status.PlacementStatus.ObservedPods).To(Equal(2))
			Expect(profile.Status.PlacementStatus.ReadyPods).To(Equal(1))
			Expect(profile.Status.PlacementStatus.PendingPods).To(Equal(1))
		})
	})
})
