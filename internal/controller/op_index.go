package controller

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// ProfileByAppRefIndex is the field index key registered on OrchestrationProfile.
//
// Index value: "<appNamespace>/<appName>" derived from spec.applicationRef.
//
// This index serves TWO mappers:
//  1. appToProfileMapFunc  — workload event → direct lookup by name+namespace
//  2. podToProfileMapFunc  — pod event → resolve app via OwnerRef → lookup here
//
// The index lives entirely in the controller-runtime informer cache (in-memory).
// It is rebuilt automatically on operator restart. its zero maintenance code.
//
// Complexity: O(1) per lookup regardless of number of profiles in the cluster.

// RegisterProfileIndexes registers all field indexes for the OrchestrationProfile
// controller. Call this once in main.go BEFORE mgr.Start():
//
//	if err := controller.RegisterProfileIndexes(mgr); err != nil {
//	    setupLog.Error(err, "unable to register profile indexes")
//	    os.Exit(1)
//	}
func RegisterProfileIndexes(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&orchestrationv1alpha1.OrchestrationProfile{},
		ProfileByAppRefIndex,
		profileByAppRefIndexFunc,
	); err != nil {
		return fmt.Errorf("registering index %q: %w", ProfileByAppRefIndex, err)
	}
	ctrl.Log.Info("Registered ProfileByAppRefIndex", "index", ProfileByAppRefIndex)
	return nil
}

// profileByAppRefIndexFunc is the indexer function for ProfileByAppRefIndex.
// Called once per profile at cache-sync time, and again whenever a profile
// is created or updated.
//
// Returns nil for incomplete profiles (missing name or namespace) so they
// are never matched by index lookups.
func profileByAppRefIndexFunc(obj client.Object) []string {
	profile, ok := obj.(*orchestrationv1alpha1.OrchestrationProfile)
	if !ok {
		return nil
	}
	appRef := profile.Spec.ApplicationRef
	if appRef.Name == "" || appRef.Namespace == "" {
		return nil
	}
	return []string{appRef.Namespace + "/" + appRef.Name}
}
