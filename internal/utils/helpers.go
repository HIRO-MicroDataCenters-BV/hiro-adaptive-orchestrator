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

package utils

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// KeysOf extracts the keys of a string→bool map as a slice.
// Used for generating field.NotSupported error messages with the full list
// of accepted values.
func KeysOf(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ResolveAppFromPod walks the pod's OwnerReferences to find the name, namespace,
// and kind of the top-level workload (Deployment, StatefulSet, Job, or standalone ReplicaSet).
//
// Walk logic:
//
//	Pod.OwnerRef → ReplicaSet → check RS.OwnerRef → Deployment?
//	                                               → standalone RS if not
//	Pod.OwnerRef → StatefulSet  (direct)
//	Pod.OwnerRef → Job          (direct)
//
// Returns ("", "", "") if no recognized workload owner is found.
func ResolveAppFromPod(
	ctx context.Context,
	client client.Client,
	pod *corev1.Pod,
) (appName, appNamespace, appKind string) {
	logger := logf.FromContext(ctx)

	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {

		case "ReplicaSet":
			// Fetch the RS to check if a Deployment owns it above
			rs := &appsv1.ReplicaSet{}
			if err := client.Get(ctx, types.NamespacedName{
				Name: ref.Name, Namespace: pod.Namespace,
			}, rs); err != nil {
				// Transient error — fall back to RS name itself
				logger.V(1).Info("could not fetch owner ReplicaSet, using RS name",
					"rs", ref.Name, "pod", pod.Name, "err", err)
				return ref.Name, pod.Namespace, "ReplicaSet"
			}
			for _, rsOwner := range rs.OwnerReferences {
				if rsOwner.Kind == "Deployment" {
					return rsOwner.Name, pod.Namespace, "Deployment"
				}
			}
			return rs.Name, pod.Namespace, "ReplicaSet" // standalone ReplicaSet

		case "StatefulSet":
			return ref.Name, pod.Namespace, "StatefulSet"

		case "Job":
			return ref.Name, pod.Namespace, "Job"
		}
	}
	return "", "", ""
}

// NodeNames extracts the names of a list of nodes as a slice.
func NodeNames(nodes []*corev1.Node) []string {
	if len(nodes) == 0 {
		return nil
	}

	names := make([]string, len(nodes)) // Pre-allocate exact size
	for i, node := range nodes {
		names[i] = node.Name
	}
	return names
}
