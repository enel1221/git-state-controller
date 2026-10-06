package controller

import (
	"context"
	"errors"
	"reflect"
	"time"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	gitwriter "github.com/inelson/git-state-controller/internal/git"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Capture accepted cleanup before DELETE or Git mutation. Store identities, never credentials.
func (r *GitResourceReconciler) prepareCleanup(ctx context.Context, cr *api.GitResource, op gitwriter.Operation, captured approvalDecision) error {
	decision := r.approval(ctx, cr)
	if !decision.allowed {
		return errApprovalPending
	}
	if !decision.sameEvidence(captured) {
		return gitwriter.ErrRetry
	}
	checkpoint := &api.CleanupCheckpoint{Policy: cr.Spec.DeletionPolicy, Request: approvalRequest(cr), GitConfigRef: &api.GitConfigReference{Kind: "ClusterGitConfig", Name: ConfigName(cr)}, Author: &api.ChangeAuthor{Name: op.AuthorName, Email: op.AuthorEmail}, Message: op.Message}
	if checkpoint.Policy == "" {
		checkpoint.Policy = "Delete"
	}
	if cr.Status.PublishedResourceRef != nil {
		ref := *cr.Status.PublishedResourceRef
		if snapshot := cr.Status.Resource; snapshot != nil && snapshot.Ref.Name == ref.Name && snapshot.Ref.Kind == ref.Kind && (snapshot.Ref.Namespace == ref.Namespace || ref.Namespace == "" && snapshot.Ref.Namespace == cr.Namespace) {
			publishedGV, e1 := schema.ParseGroupVersion(ref.APIVersion)
			observedGV, e2 := schema.ParseGroupVersion(snapshot.Ref.APIVersion)
			if e1 == nil && e2 == nil && publishedGV.Group == observedGV.Group {
				ref = snapshot.Ref
			}
		}
		if ref.UID == "" {
			observed := observeExact(ctx, r.Reader, r.Mapper, ref, cr.Namespace)
			if observed.Exists != nil && *observed.Exists {
				ref = observed.Ref
			}
		}
		checkpoint.ResourceRef = &ref
	}
	ref := applicationRef(cr, r.setKey().Namespace)
	app := ApplicationObject()
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, app); err == nil {
		if app.GetAnnotations()[UIDAnnotation] != "" && app.GetAnnotations()[UIDAnnotation] != string(cr.UID) {
			return errors.New("application identity differs from cleanup owner")
		}
		checkpoint.ApplicationUID = string(app.GetUID())
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return patchStatus(ctx, r.Client, r.Reader, cr, "publisher", func(current *api.GitResource) {
		if decision.envelope.Required != nil && *decision.envelope.Required && (current.Annotations[ApprovedRequestAnnotation] != approvalRequest(current) || current.Annotations[ApprovedByAnnotation] != cr.Annotations[ApprovedByAnnotation]) {
			return
		}
		if current.Generation == cr.Generation && current.UID == cr.UID && !paused(current) && deletionRequested(current) && (current.Status.Cleanup == nil || current.DeletionTimestamp.IsZero()) {
			current.Status.Cleanup = checkpoint
			current.Status.ApplicationRef = &ref
		}
	})
}

func (r *GitResourceReconciler) finishCleanup(ctx context.Context, cr *api.GitResource) (ctrl.Result, error) {
	cleanup := cr.Status.Cleanup
	if err := r.recordApproval(ctx, cr, r.approval(ctx, cr)); err != nil {
		return ctrl.Result{}, err
	}
	if cleanup.Policy == "Orphan" && !cleanup.Unowned {
		if !r.handoffVerified(ctx, cr, cleanup.Revision, "orphaned") {
			return r.waitCleanup(ctx, cr, "Orphaning", "Waiting for the verified retained ApplicationSet entry.")
		}
		return ctrl.Result{}, r.releaseFinalizer(ctx, cr)
	}
	set := ApplicationSetObject()
	err := r.Reader.Get(ctx, r.setKey(), set)
	if err != nil && !apierrors.IsNotFound(err) {
		return r.waitCleanup(ctx, cr, "Deleting", "Cannot verify ApplicationSet entry removal.")
	}
	if err == nil {
		entries, err := listElements(set)
		if err != nil {
			return r.waitCleanup(ctx, cr, "Deleting", "Cannot read ApplicationSet inventory.")
		}
		for _, entry := range entries {
			if stringField(entry, "uid") == string(cr.UID) {
				return r.waitCleanup(ctx, cr, "Deleting", "Waiting for ApplicationSet entry removal.")
			}
		}
	}
	ref := cr.Status.ApplicationRef
	if ref == nil {
		return r.waitCleanup(ctx, cr, "Deleting", "Application identity cannot be recovered.")
	}
	app := ApplicationObject()
	err = r.Reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, app)
	if err == nil {
		if cleanup.ApplicationUID != "" && cleanup.ApplicationUID != string(app.GetUID()) || app.GetAnnotations()[UIDAnnotation] != "" && app.GetAnnotations()[UIDAnnotation] != string(cr.UID) {
			return r.waitCleanup(ctx, cr, "IdentityConflict", "A replacement Application occupies the cleanup identity.")
		}
		return r.waitCleanup(ctx, cr, "WaitingForApplicationDeletion", "Waiting for Argo to delete the Application and its resource.")
	}
	if !apierrors.IsNotFound(err) {
		return r.waitCleanup(ctx, cr, "WaitingForApplicationDeletion", "Cannot verify Application absence.")
	}
	if !cleanup.Unowned {
		resourceRef := cleanup.ResourceRef
		if resourceRef == nil {
			resourceRef = cr.Status.PublishedResourceRef
		}
		if resourceRef == nil || resourceRef.Name == "" {
			return r.waitCleanup(ctx, cr, "WaitingForResourceDeletion", "Published resource identity cannot be recovered.")
		}
		resource := observeExact(ctx, r.Reader, r.Mapper, *resourceRef, cr.Namespace)
		if resource.Exists == nil {
			return r.waitCleanup(ctx, cr, "WaitingForResourceDeletion", resource.Observation.Reason+": "+resource.Observation.Message)
		}
		if *resource.Exists {
			if resourceRef.UID != "" && resource.Ref.UID != resourceRef.UID {
				return r.waitCleanup(ctx, cr, "IdentityConflict", "A replacement resource occupies the cleanup identity.")
			}
			return r.waitCleanup(ctx, cr, "WaitingForResourceDeletion", "Waiting for the direct resource to disappear; its finalizers remain authoritative.")
		}
	}
	return ctrl.Result{}, r.releaseFinalizer(ctx, cr)
}
func (r *GitResourceReconciler) waitCleanup(ctx context.Context, cr *api.GitResource, reason, message string) (ctrl.Result, error) {
	return ctrl.Result{RequeueAfter: 2 * time.Second}, r.setStatus(ctx, cr, false, reason, message, gitwriter.Result{}, "")
}

// An exact GET is independent of vanished Application inventory. Only NotFound means absent.
func observeExact(ctx context.Context, reader client.Reader, mapper meta.RESTMapper, ref api.ResourceReference, defaultNamespace string) *api.ResourceSnapshot {
	now := metav1.Now()
	result := &api.ResourceSnapshot{Ref: ref, LastUpdatedAt: &now}
	if mapper == nil {
		result.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "API discovery is unavailable."}
		return result
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		result.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Invalid published API version."}
		return result
	}
	if gv.Group == api.GroupVersion.Group {
		result.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Recursive controller observation is unsupported."}
		return result
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, gv.Version)
	if meta.IsNoMatchError(err) {
		meta.MaybeResetRESTMapper(mapper)
		mapping, err = mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, gv.Version)
	}
	if err != nil {
		result.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Published version cannot be discovered."}
		return result
	}
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if result.Ref.Namespace == "" {
			result.Ref.Namespace = defaultNamespace
		}
	} else {
		result.Ref.Namespace = ""
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(mapping.GroupVersionKind)
	if err = reader.Get(ctx, client.ObjectKey{Namespace: result.Ref.Namespace, Name: ref.Name}, live); err != nil {
		result.Observation = observationError(err)
		if apierrors.IsNotFound(err) {
			exists := false
			result.Exists = &exists
			result.Ref.UID = ""
		}
		return result
	}
	exists := true
	result.Exists = &exists
	result.Ref.UID, result.ResourceVersion, result.DeletionTimestamp = string(live.GetUID()), live.GetResourceVersion(), live.GetDeletionTimestamp()
	if generation, found, _ := unstructured.NestedInt64(live.Object, "metadata", "generation"); found {
		result.Generation = &generation
	}
	result.Observation = api.Observation{Reason: "Observed"}
	result.Status, err = copyObjectField(live.Object, "status")
	if err != nil {
		result.Observation = api.Observation{Reason: "InvalidStatus", Message: err.Error()}
	}
	return result
}

func (r *GitResourceReconciler) discardUnusedCleanup(ctx context.Context, cr *api.GitResource) error {
	if !cr.DeletionTimestamp.IsZero() || cr.Status.Cleanup == nil || deletionRequested(cr) && cr.Status.Cleanup.Request == approvalRequest(cr) {
		return nil
	}
	return patchStatus(ctx, r.Client, r.Reader, cr, "publisher", func(current *api.GitResource) {
		if current.DeletionTimestamp.IsZero() && current.Generation == cr.Generation && reflect.DeepEqual(current.Status.Cleanup, cr.Status.Cleanup) {
			current.Status.Cleanup = nil
		}
	})
}
