package controller

import (
	"context"
	"errors"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	gitwriter "github.com/inelson/git-state-controller/internal/git"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func (r *GitResourceReconciler) setKey() types.NamespacedName {
	if r.SetKey.Name == "" {
		return types.NamespacedName{Namespace: "argocd", Name: "git-resources"}
	}
	return r.SetKey
}
func (r *GitResourceReconciler) publicationIdentity(ctx context.Context, cr *api.GitResource) (api.ApplicationReference, bool, error) {
	key := r.setKey()
	ref := applicationRef(cr, key.Namespace)
	adopt := adopting(cr) && !deletionRequested(cr)
	set := ApplicationSetObject()
	if err := r.Reader.Get(ctx, key, set); err != nil {
		if adopt {
			return ref, false, errors.New("cannot resolve adoption identity from ApplicationSet")
		}
		return ref, false, nil
	}
	entries, err := listElements(set)
	if err != nil {
		return ref, false, err
	}
	matches := []map[string]interface{}{}
	legacy := false
	for _, e := range entries {
		if entryMatches(e, cr) {
			matches = append(matches, e)
			if stringField(e, "uid") == string(cr.UID) {
				ref.Name = stringField(e, "applicationName")
				legacy = cr.Status.LastPublishedRevision != ""
			}
		}
	}
	if !adopt {
		for _, e := range matches {
			if !deletionRequested(cr) && stringField(e, "managementState") == "orphaned" && stringField(e, "uid") != string(cr.UID) {
				return ref, false, gitwriter.ErrPathAlreadyExists
			}
		}
		return ref, legacy, nil
	}
	if len(matches) > 1 {
		return ref, false, errors.New("ambiguous ApplicationSet path; adoption requires one identity")
	}
	if len(matches) == 1 {
		e := matches[0]
		if stringField(e, "namespace") != cr.Namespace {
			return ref, false, errors.New("retained destination namespace differs; adopt in the original namespace")
		}
		oldUID := stringField(e, "uid")
		if oldUID != string(cr.UID) {
			owner := &api.GitResource{}
			err = r.Reader.Get(ctx, types.NamespacedName{Namespace: stringField(e, "namespace"), Name: stringField(e, "name")}, owner)
			if err == nil && string(owner.UID) == oldUID {
				return ref, false, errors.New("old GitResource owner still exists; adoption blocked")
			}
			if err != nil && !apierrors.IsNotFound(err) {
				return ref, false, errors.New("cannot verify old GitResource owner absence")
			}
		}
		ref.Name = stringField(e, "applicationName")
		if ref.Name == "" {
			return ref, false, errors.New("inventory has no recoverable Application identity")
		}
	}
	app := ApplicationObject()
	err = r.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, app)
	if err == nil && !app.GetDeletionTimestamp().IsZero() {
		return ref, false, errors.New("application is terminating; adoption blocked")
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ref, false, errors.New("cannot verify Application identity")
	}
	return ref, legacy, nil
}
func (r *GitResourceReconciler) handoffVerified(ctx context.Context, cr *api.GitResource, revision, state string) bool {
	set := ApplicationSetObject()
	if r.Reader.Get(ctx, r.setKey(), set) != nil {
		return false
	}
	entries, err := listElements(set)
	if err != nil {
		return false
	}
	ref := applicationRef(cr, r.setKey().Namespace)
	count := 0
	for _, e := range entries {
		if entryMatches(e, cr) && stringField(e, "applicationName") == ref.Name && stringField(e, "uid") == string(cr.UID) && stringField(e, "revision") == revision && stringField(e, "managementState") == state {
			count++
		}
	}
	return count == 1
}
func (r *GitResourceReconciler) releaseFinalizer(ctx context.Context, processed *api.GitResource) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &api.GitResource{}
		if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(processed), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != processed.UID || paused(current) {
			return nil
		}
		if cleanup := current.Status.Cleanup; cleanup != nil && cleanup.Policy == "Orphan" && !cleanup.Unowned && cleanup.Revision != "" && !r.handoffVerified(ctx, current, cleanup.Revision, "orphaned") {
			return nil
		}
		before := current.DeepCopy()
		controllerutil.RemoveFinalizer(current, Finalizer)
		return r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}
