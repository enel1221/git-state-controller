package controller

import (
	"reflect"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"github.com/inelson/git-state-controller/internal/manifest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func mutationChanged(old, new client.Object) bool {
	if old == nil || new == nil {
		return true
	}
	return old.GetUID() != new.GetUID() || old.GetGeneration() != new.GetGeneration() || !old.GetDeletionTimestamp().Equal(new.GetDeletionTimestamp()) || old.GetAnnotations()[PausedAnnotation] != new.GetAnnotations()[PausedAnnotation] || old.GetAnnotations()[AdoptAnnotation] != new.GetAnnotations()[AdoptAnnotation] || old.GetAnnotations()[ApprovedRequestAnnotation] != new.GetAnnotations()[ApprovedRequestAnnotation] || old.GetAnnotations()[ApprovedByAnnotation] != new.GetAnnotations()[ApprovedByAnnotation]
}

var publisherPredicate = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool { return mutationChanged(e.ObjectOld, e.ObjectNew) }}
var configPredicate = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool { return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() }}
var inventoryPredicate = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	old, ok := e.ObjectOld.(*api.GitResource)
	if !ok {
		return false
	}
	new, ok := e.ObjectNew.(*api.GitResource)
	if !ok {
		return false
	}
	return mutationChanged(old, new) || old.Status.LastPublishedRevision != new.Status.LastPublishedRevision || !reflect.DeepEqual(old.Status.ApplicationRef, new.Status.ApplicationRef) || !reflect.DeepEqual(old.Status.Cleanup, new.Status.Cleanup)
}}
var observerPredicate = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	old, ok := e.ObjectOld.(*api.GitResource)
	if !ok {
		return false
	}
	new, ok := e.ObjectNew.(*api.GitResource)
	if !ok {
		return false
	}
	return inventoryPredicate.Update(e) || old.Status.LastPublishedGeneration != new.Status.LastPublishedGeneration || !reflect.DeepEqual(old.Status.PublishedResourceRef, new.Status.PublishedResourceRef) || publicationCurrent(old) != publicationCurrent(new)
}}

func objectFieldChanged(e event.UpdateEvent, fields ...string) bool {
	old, oldOK := e.ObjectOld.(*unstructured.Unstructured)
	new, newOK := e.ObjectNew.(*unstructured.Unstructured)
	if !oldOK || !newOK {
		return true
	}
	a, foundA, errA := unstructured.NestedFieldNoCopy(old.Object, fields...)
	b, foundB, errB := unstructured.NestedFieldNoCopy(new.Object, fields...)
	return foundA != foundB || errA != nil || errB != nil || !reflect.DeepEqual(a, b)
}

func identityChanged(e event.UpdateEvent) bool {
	return e.ObjectOld.GetUID() != e.ObjectNew.GetUID() || !e.ObjectOld.GetDeletionTimestamp().Equal(e.ObjectNew.GetDeletionTimestamp())
}

var applicationSetPredicate = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	return identityChanged(e) || objectFieldChanged(e, "spec")
}}

var applicationPredicate = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	if identityChanged(e) || !reflect.DeepEqual(e.ObjectOld.GetOwnerReferences(), e.ObjectNew.GetOwnerReferences()) {
		return true
	}
	for _, key := range []string{SourceAnnotation, UIDAnnotation, SetAnnotation, manifest.StateAnnotation} {
		if e.ObjectOld.GetAnnotations()[key] != e.ObjectNew.GetAnnotations()[key] {
			return true
		}
	}
	return e.ObjectOld.GetLabels()["gitops.example.io/managed"] != e.ObjectNew.GetLabels()["gitops.example.io/managed"] || objectFieldChanged(e, "spec", "source") || objectFieldChanged(e, "spec", "sources") || objectFieldChanged(e, "spec", "destination") || objectFieldChanged(e, "status")
}}

var namespacePredicate = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	return identityChanged(e) || e.ObjectOld.GetAnnotations()[ApprovalPolicyAnnotation] != e.ObjectNew.GetAnnotations()[ApprovalPolicyAnnotation]
}}
