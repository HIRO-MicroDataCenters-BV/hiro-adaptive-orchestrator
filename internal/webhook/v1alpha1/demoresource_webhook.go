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

package v1alpha1

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	orchestrationv1alpha1 "github.com/HIRO-MicroDataCenters-BV/hiro-adaptive-orchestrator/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var demoresourcelog = logf.Log.WithName("demoresource-resource")

// SetupDemoResourceWebhookWithManager registers the webhook for DemoResource in the manager.
func SetupDemoResourceWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &orchestrationv1alpha1.DemoResource{}).
		WithValidator(&DemoResourceValidator{}).
		WithDefaulter(&DemoResourceDefaulter{}).
		Complete()
}

// TODO(user): EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!

// +kubebuilder:webhook:path=/mutate-orchestration-hiro-io-v1alpha1-demoresource,mutating=true,failurePolicy=fail,sideEffects=None,groups=orchestration.hiro.io,resources=demoresources,verbs=create;update,versions=v1alpha1,name=mdemoresource-v1alpha1.kb.io,admissionReviewVersions=v1

// DemoResourceDefaulter struct is responsible for setting default values on the custom resource of the
// Kind DemoResource when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type DemoResourceDefaulter struct {
	// TODO(user): Add more fields as needed for defaulting
}

// Default implements admission.Defaulter so a webhook will be registered for the Kind DemoResource.
func (d *DemoResourceDefaulter) Default(_ context.Context, obj *orchestrationv1alpha1.DemoResource) error {
	demoresourcelog.Info("Defaulting for DemoResource", "name", obj.GetName())

	// TODO(user): fill in your defaulting logic.

	return nil
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
// NOTE: If you want to customise the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-orchestration-hiro-io-v1alpha1-demoresource,mutating=false,failurePolicy=fail,sideEffects=None,groups=orchestration.hiro.io,resources=demoresources,verbs=create;update,versions=v1alpha1,name=vdemoresource-v1alpha1.kb.io,admissionReviewVersions=v1

// DemoResourceValidator struct is responsible for validating the DemoResource resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type DemoResourceValidator struct {
	// TODO(user): Add more fields as needed for validation
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type DemoResource.
func (v *DemoResourceValidator) ValidateCreate(_ context.Context, obj *orchestrationv1alpha1.DemoResource) (admission.Warnings, error) {
	demoresourcelog.Info("Validation for DemoResource upon creation", "name", obj.GetName())

	// TODO(user): fill in your validation logic upon object creation.

	return nil, nil
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type DemoResource.
func (v *DemoResourceValidator) ValidateUpdate(_ context.Context, oldObj, newObj *orchestrationv1alpha1.DemoResource) (admission.Warnings, error) {
	demoresourcelog.Info("Validation for DemoResource upon update", "name", newObj.GetName())

	// TODO(user): fill in your validation logic upon object update.

	return nil, nil
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type DemoResource.
func (v *DemoResourceValidator) ValidateDelete(_ context.Context, obj *orchestrationv1alpha1.DemoResource) (admission.Warnings, error) {
	demoresourcelog.Info("Validation for DemoResource upon deletion", "name", obj.GetName())

	// TODO(user): fill in your validation logic upon object deletion.

	return nil, nil
}
