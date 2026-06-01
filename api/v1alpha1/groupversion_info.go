// Package v1alpha1 contains the onp.io/v1alpha1 API types.
//
// The group holds the CRDs ONP uses to model on-prem nodes declaratively:
// Machine (an individual physical node and its power metadata) and, in later
// milestones, NodePool (pool-level policy).
//
// +kubebuilder:object:generate=true
// +groupName=onp.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the group/version for all types in this package.
var GroupVersion = schema.GroupVersion{Group: "onp.io", Version: "v1alpha1"}

// SchemeBuilder collects the type registrations for GroupVersion. Each type's
// init() adds itself here so callers only need AddToScheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme registers every type in GroupVersion with a runtime.Scheme.
var AddToScheme = SchemeBuilder.AddToScheme
