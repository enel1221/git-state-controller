package controller

import (
	"reflect"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func mutationChanged(old, new client.Object) bool {
	if old == nil || new == nil {
		return true
	}
	return old.GetGeneration() != new.GetGeneration() || !old.GetDeletionTimestamp().Equal(new.GetDeletionTimestamp()) || old.GetAnnotations()[PausedAnnotation] != new.GetAnnotations()[PausedAnnotation] || old.GetAnnotations()[AdoptAnnotation] != new.GetAnnotations()[AdoptAnnotation]
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
