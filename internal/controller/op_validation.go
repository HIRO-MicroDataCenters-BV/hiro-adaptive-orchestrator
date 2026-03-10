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

package controller

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// validateProfile performs deep field-level validation of the OrchestrationProfile spec.
//
// Validation rules:
//   - applicationRef: name, namespace, kind, and apiVersion are all required.
//     kind must be one of the supported workload kinds.
//     apiVersion must be parseable as group/version.
//   - placement.strategy: required and must be one of the supported strategies.
//     EnergyAware strategy requires awareness.energy = true.
//   - rebalancing: only validated when enabled = true.
//     cooldownSeconds must be >= 0.
//     each triggerCondition must be a supported value.
//
// Returns a field.ErrorList so callers get precise, kubectl-compatible error messages.
func (r *OrchestrationProfileReconciler) validateProfile(
	profile *orchestrationv1alpha1.OrchestrationProfile,
) field.ErrorList {
	var errs field.ErrorList
	spec := profile.Spec
	specPath := field.NewPath("spec")

	errs = append(errs, validateApplicationRef(spec, specPath)...)
	errs = append(errs, validatePlacement(spec, specPath)...)
	errs = append(errs, validateRebalancing(spec, specPath)...)

	return errs
}

// validateApplicationRef validates the applicationRef block of the spec.
func validateApplicationRef(
	spec orchestrationv1alpha1.OrchestrationProfileSpec,
	specPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	appRef := spec.ApplicationRef
	appRefPath := specPath.Child("applicationRef")

	if appRef.Name == "" {
		errs = append(errs, field.Required(
			appRefPath.Child("name"),
			"application name must be specified",
		))
	}

	if appRef.Namespace == "" {
		errs = append(errs, field.Required(
			appRefPath.Child("namespace"),
			"application namespace must be specified",
		))
	}

	if appRef.Kind == "" {
		errs = append(errs, field.Required(
			appRefPath.Child("kind"),
			"application kind must be specified",
		))
	} else if !supportedAppKinds[appRef.Kind] {
		errs = append(errs, field.NotSupported(
			appRefPath.Child("kind"),
			appRef.Kind,
			keysOf(supportedAppKinds),
		))
	}

	if appRef.APIVersion == "" {
		errs = append(errs, field.Required(
			appRefPath.Child("apiVersion"),
			"application apiVersion must be specified",
		))
	} else {
		if _, err := schema.ParseGroupVersion(appRef.APIVersion); err != nil {
			errs = append(errs, field.Invalid(
				appRefPath.Child("apiVersion"),
				appRef.APIVersion,
				fmt.Sprintf("invalid apiVersion format: %v", err),
			))
		}
	}

	return errs
}

// validatePlacement validates the placement block of the spec.
func validatePlacement(
	spec orchestrationv1alpha1.OrchestrationProfileSpec,
	specPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList
	placementPath := specPath.Child("placement")

	if spec.Placement.Strategy == "" {
		errs = append(errs, field.Required(
			placementPath.Child("strategy"),
			"placement strategy must be specified",
		))
	} else if !validStrategies[spec.Placement.Strategy] {
		errs = append(errs, field.NotSupported(
			placementPath.Child("strategy"),
			spec.Placement.Strategy,
			keysOf(validStrategies),
		))
	}

	// EnergyAware strategy requires energy awareness explicitly enabled.
	if spec.Placement.Strategy == "EnergyAware" && !spec.Placement.Awareness.Energy {
		errs = append(errs, field.Invalid(
			placementPath.Child("awareness").Child("energy"),
			false,
			"energy awareness must be enabled when strategy is EnergyAware",
		))
	}

	return errs
}

// validateRebalancing validates the rebalancing block of the spec.
// Skipped entirely when rebalancing.enabled = false.
func validateRebalancing(
	spec orchestrationv1alpha1.OrchestrationProfileSpec,
	specPath *field.Path,
) field.ErrorList {
	if !spec.Rebalancing.Enabled {
		return nil
	}

	var errs field.ErrorList
	rebalancingPath := specPath.Child("rebalancing")

	if spec.Rebalancing.CooldownSeconds < 0 {
		errs = append(errs, field.Invalid(
			rebalancingPath.Child("cooldownSeconds"),
			spec.Rebalancing.CooldownSeconds,
			"cooldownSeconds must be >= 0",
		))
	}

	for i, condition := range spec.Rebalancing.TriggerConditions {
		if !validTriggerConditions[condition] {
			errs = append(errs, field.NotSupported(
				rebalancingPath.Child("triggerConditions").Index(i),
				condition,
				keysOf(validTriggerConditions),
			))
		}
	}

	return errs
}
