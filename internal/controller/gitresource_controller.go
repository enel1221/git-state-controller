package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	gitwriter "github.com/inelson/git-state-controller/internal/git"
	"github.com/inelson/git-state-controller/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	runtimecontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const Finalizer = "gitops.example.io/git-cleanup"

type GitResourceReconciler struct {
	client.Client
	Reader           client.Reader
	Publisher        *gitwriter.Publisher
	Recorder         events.EventRecorder
	Namespace        string
	AllowHTTP        bool
	Workers          int
	OperationTimeout time.Duration
	SetKey           types.NamespacedName
}

// +kubebuilder:rbac:groups=gitops.example.io,resources=gitresources,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.example.io,resources=gitresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gitops.example.io,resources=gitresources/finalizers,verbs=update
// +kubebuilder:rbac:groups=gitops.example.io,resources=clustergitconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=gitops.example.io,resources=clustergitconfigs/status,verbs=get;patch;update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch,namespace=git-state-system
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func ConfigName(cr *api.GitResource) string {
	if cr.Spec.GitConfigRef.Name == "" {
		return "default"
	}
	return cr.Spec.GitConfigRef.Name
}

func (r *GitResourceReconciler) resolve(ctx context.Context, cr *api.GitResource) (gitwriter.Operation, string, error) {
	op := gitwriter.Operation{URL: cr.Spec.Repository.URL, Branch: cr.Spec.Repository.Branch, Path: cr.Spec.Repository.Path,
		Delete: !cr.DeletionTimestamp.IsZero(), PreviousRevision: cr.Status.LastPublishedRevision,
		PreviousHash: cr.Status.LastPublishedContentHash, OwnerUID: string(cr.UID), SourceNamespace: cr.Namespace, SourceName: cr.Name, AllowAdopt: adopting(cr)}
	if cr.Status.Cleanup != nil && cr.Status.Cleanup.Policy == "Orphan" && op.Delete {
		op.Orphan = true
		op.Delete = false
		if cr.Status.Cleanup.Revision != "" {
			op.PreviousRevision = cr.Status.Cleanup.Revision
		}
	}
	if (cr.Spec.GitConfigRef.Kind != "" && cr.Spec.GitConfigRef.Kind != "ClusterGitConfig") || (cr.Spec.DeletionPolicy != "" && cr.Spec.DeletionPolicy != "Delete" && cr.Spec.DeletionPolicy != "Orphan") {
		return op, "InvalidSpec", errors.New("unsupported configuration kind or deletion policy")
	}
	if err := gitwriter.ValidateDestination(op.URL, op.Branch, op.Path, r.AllowHTTP); err != nil {
		return op, "InvalidSpec", err
	}
	if !op.Delete && !op.Orphan {
		var err error
		op.Content, _, err = manifest.RenderManaged(cr.Spec.Manifest.Raw, cr.Namespace, cr.Name, string(cr.UID), "managed")
		if err != nil {
			return op, "InvalidSpec", err
		}
	}
	config, secret, reason, err := r.loadConfig(ctx, ConfigName(cr))
	if err != nil {
		return op, reason, err
	}
	op.Credentials = gitwriter.Credentials{Username: string(secret.Data["username"]), Password: string(secret.Data["password"])}
	op.AuthorName, op.AuthorEmail = config.Spec.CommitAuthor.Name, config.Spec.CommitAuthor.Email
	if op.AuthorName == "" {
		op.AuthorName = "git-state-controller"
	}
	if op.AuthorEmail == "" {
		op.AuthorEmail = "git-state-controller@example.invalid"
	}
	var evidence *api.GitAccessObservation
	op.Access = func(operation string, success bool) {
		if evidence != nil && evidence.Operation == "Push" && operation == "Fetch" {
			return
		}
		why, msg := "Succeeded", ""
		if !success {
			why = "AccessFailed"
			msg = "Git access failed; check repository permissions and connectivity."
		}
		evidence = &api.GitAccessObservation{RepositoryURL: op.URL, Operation: operation, Success: success, Reason: why, Message: msg, ConfigGeneration: config.Generation, SecretUID: string(secret.UID), SecretResourceVersion: secret.ResourceVersion, LastUpdatedAt: metav1.Now()}
	}
	op.FlushAccess = func() {
		if evidence == nil {
			return
		}
		evidenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		r.recordConfig(evidenceCtx, config, secret, true, "CredentialsLoaded", evidence)
	}
	action := "Publish"
	if op.Delete {
		action = "Delete"
	}
	op.Message = cr.Spec.Change.Message
	if op.Message == "" {
		op.Message = fmt.Sprintf("%s %s/%s", action, cr.Namespace, cr.Name)
	}
	op.Message += fmt.Sprintf("\n\nGitResource: %s/%s\nGitResource-UID: %s\nGitResource-Generation: %d\n", cr.Namespace, cr.Name, cr.UID, cr.Generation)
	return op, "", nil
}

func (r *GitResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	requestCtx := ctx
	timeout := r.OperationTimeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cr := &api.GitResource{}
	if err := r.Reader.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	uid := cr.UID
	if cr.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(cr, Finalizer) {
		before := cr.DeepCopy()
		controllerutil.AddFinalizer(cr, Finalizer)
		if err := r.Patch(ctx, cr, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Reader.Get(ctx, req.NamespacedName, cr); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if cr.UID != uid {
			return ctrl.Result{}, nil
		}
	}
	if !cr.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(cr, Finalizer) {
		return ctrl.Result{}, nil
	}
	if paused(cr) {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, patchStatus(ctx, r.Client, r.Reader, cr, "publisher", func(*api.GitResource) {})
	}
	if !cr.DeletionTimestamp.IsZero() && cr.Status.Cleanup == nil {

		if err := patchStatus(ctx, r.Client, r.Reader, cr, "publisher", func(current *api.GitResource) {
			if current.Status.Cleanup == nil && !paused(current) {
				policy := current.Spec.DeletionPolicy
				if policy == "" {
					policy = "Delete"
				}
				current.Status.Cleanup = &api.CleanupCheckpoint{Policy: policy}
			}
		}); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Reader.Get(ctx, req.NamespacedName, cr); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	// A durable cleanup checkpoint proves the Git effect already completed.
	// Retrying the API handoff must not repair later branch edits or change its pin.
	if !cr.DeletionTimestamp.IsZero() && cr.Status.Cleanup != nil && cr.Status.Cleanup.Revision != "" {
		if cr.Status.Cleanup.Policy == "Orphan" && !r.handoffVerified(ctx, cr, cr.Status.Cleanup.Revision, "orphaned") {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, r.releaseFinalizer(ctx, cr)
	}
	var result gitwriter.Result
	var op gitwriter.Operation
	defer func() {
		if op.FlushAccess != nil {
			op.FlushAccess()
		}
	}()
	var reason string
	var err error
attempts:
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(publicationRetryDelay(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				err = ctx.Err()
				break attempts
			case <-timer.C:
			}
			if err = r.Reader.Get(ctx, req.NamespacedName, cr); err != nil {
				break
			}
			if cr.UID != uid {
				return ctrl.Result{}, nil
			}
		}
		if paused(cr) {
			err = gitwriter.ErrPaused
			break
		}
		op, reason, err = r.resolve(ctx, cr)
		if err != nil {
			break
		}
		ref, legacy, identityErr := r.publicationIdentity(ctx, cr)
		if identityErr != nil {
			err = identityErr
			reason = "AdoptionBlocked"
			if errors.Is(identityErr, gitwriter.ErrPathAlreadyExists) {
				reason = "PathAlreadyExists"
			}
			break
		}
		op.AllowLegacy = legacy
		op.OwnershipGuard = func(checkCtx context.Context, namespace, name, ownerUID string) error {
			old := &api.GitResource{}
			e := r.Reader.Get(checkCtx, types.NamespacedName{Namespace: namespace, Name: name}, old)
			if e == nil && string(old.UID) == ownerUID {
				return errors.New("old GitResource owner still exists; adoption blocked")
			}
			if e != nil && !apierrors.IsNotFound(e) {
				return errors.New("cannot verify old owner absence")
			}
			return nil
		}
		if cr.Status.ApplicationRef == nil || *cr.Status.ApplicationRef != ref {
			if err = patchStatus(ctx, r.Client, r.Reader, cr, "publisher", func(current *api.GitResource) {
				if current.Generation == cr.Generation && !paused(current) {
					current.Status.ApplicationRef = &ref
				}
			}); err != nil {
				break
			}
			cr.Status.ApplicationRef = &ref
		}
		if !op.Delete && !op.Orphan && !op.AllowAdopt && cr.Status.LastPublishedRevision != "" {
			desired := manifest.Hash(op.Content)
			if desired == cr.Status.LastPublishedContentHash {
				op.ReadOnly = true
			} else if legacy {
				_, oldHash, e := manifest.Render(cr.Spec.Manifest.Raw)
				if e == nil && oldHash == cr.Status.LastPublishedContentHash {
					op.ReadOnly = true
				}
			}
		}
		processed := cr.DeepCopy()
		op.Guard = func(guardCtx context.Context) error {
			current := &api.GitResource{}
			if e := r.Reader.Get(guardCtx, req.NamespacedName, current); e != nil {
				return e
			}
			if current.UID != uid || !current.DeletionTimestamp.Equal(processed.DeletionTimestamp) {
				return gitwriter.ErrRetry
			}
			if paused(current) {
				return gitwriter.ErrPaused
			}
			if adopting(current) {
				_, _, e := r.publicationIdentity(guardCtx, current)
				return e
			}
			return nil
		}
		result, err = r.Publisher.Attempt(ctx, op)
		if !errors.Is(err, gitwriter.ErrRetry) {
			break
		}
	}
	ctx, statusCancel := context.WithTimeout(requestCtx, 30*time.Second)
	defer statusCancel()
	if errors.Is(err, gitwriter.ErrPaused) {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, patchStatus(ctx, r.Client, r.Reader, cr, "publisher", func(*api.GitResource) {})
	}
	if err != nil {
		if errors.Is(err, gitwriter.ErrBranchChanged) {
			return ctrl.Result{RequeueAfter: publicationRetryDelay(3)}, r.setStatus(ctx, cr, false, "PublishPending", "Remote branch advanced; retrying publication.", gitwriter.Result{}, "")
		}
		if cr.DeletionTimestamp.IsZero() && cr.Status.LastPublishedRevision != "" && (op.ReadOnly || len(op.Content) > 0 && manifest.Hash(op.Content) == cr.Status.LastPublishedContentHash) {
			if statusErr := r.setStatus(ctx, cr, true, "Unchanged", "Desired manifest publication verified in Git.", gitwriter.Result{Revision: cr.Status.LastPublishedRevision}, cr.Status.LastPublishedContentHash); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if reason == "" {
			reason = "PublishFailed"
			if errors.Is(err, gitwriter.ErrCredentialsInvalid) {
				reason = "CredentialsInvalid"
			}
			if errors.Is(err, gitwriter.ErrPathAlreadyExists) {
				reason = "PathAlreadyExists"
			}
			if errors.Is(err, gitwriter.ErrRecoveryRequired) {
				reason = "RecoveryRequired"
			}
		}
		if !cr.DeletionTimestamp.IsZero() {
			reason = "DeleteFailed"
		}
		statusErr := r.setStatus(ctx, cr, false, reason, err.Error(), gitwriter.Result{}, "")
		if statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	if !cr.DeletionTimestamp.IsZero() {
		if !result.Unowned {
			if err = patchStatus(ctx, r.Client, r.Reader, cr, "publisher", func(current *api.GitResource) {
				if current.Status.Cleanup != nil {
					current.Status.Cleanup.Revision = result.Revision
					if current.Status.LastPublishedRevision == "" && result.Recovery != nil {
						current.Status.LastPublishedRevision = result.Recovery.Revision
						current.Status.LastPublishedGeneration = result.Recovery.Generation
						current.Status.LastPublishedContentHash = manifest.Hash(result.Recovery.Content)
						object, e := manifest.Decode(result.Recovery.Content)
						if e == nil {
							raw, _ := json.Marshal(object)
							current.Status.PublishedResourceRef = resourceReference(raw)
						}
					}
				}
			}); err != nil {
				return ctrl.Result{}, err
			}
			cr.Status.Cleanup.Revision = result.Revision
			if e := r.Reader.Get(ctx, req.NamespacedName, cr); e != nil {
				return ctrl.Result{}, client.IgnoreNotFound(e)
			}
		}
		if op.Orphan && !result.Unowned {
			if !r.handoffVerified(ctx, cr, result.Revision, "orphaned") {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
		return ctrl.Result{}, r.releaseFinalizer(ctx, cr)
	}
	hash := manifest.Hash(op.Content)
	if op.ReadOnly {
		hash = cr.Status.LastPublishedContentHash
	}
	reason = "Unchanged"
	if result.Changed {
		reason = "Pushed"
	}
	if err = r.setStatus(ctx, cr, true, reason, "Desired manifest publication verified in Git.", result, hash); err != nil {
		return ctrl.Result{}, err
	}
	if adopting(cr) {
		cr.Status.LastPublishedRevision = result.Revision
		if r.handoffVerified(ctx, cr, result.Revision, "managed") {
			if err = r.consumeAdoption(ctx, cr); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}
	if result.Changed {
		ctrl.LoggerFrom(ctx).Info("Published GitResource", "resource", req.NamespacedName, "generation", cr.Generation, "revision", result.Revision)
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// Publication and observer writers merge only their fields against the latest API object.
func (r *GitResourceReconciler) setStatus(ctx context.Context, processed *api.GitResource, published bool, reason, message string, result gitwriter.Result, hash string) error {
	return patchStatus(ctx, r.Client, r.Reader, processed, "publisher", func(current *api.GitResource) {
		if current.Status.ObservedGeneration > processed.Generation || current.Status.LastPublishedGeneration > processed.Generation {
			return
		}
		current.Status.ObservedGeneration = processed.Generation
		status := metav1.ConditionFalse
		if published {
			status = metav1.ConditionTrue
			current.Status.LastPublishedGeneration = processed.Generation
			current.Status.LastPublishedRevision = result.Revision
			current.Status.LastPublishedContentHash = hash
			current.Status.PublishedResourceRef = resourceReference(processed.Spec.Manifest.Raw)
		}
		previous := meta.FindStatusCondition(current.Status.Conditions, "Published")
		why := reason
		if published && !result.Changed && previous != nil && previous.Status == metav1.ConditionTrue && previous.ObservedGeneration == processed.Generation {
			why = previous.Reason
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "Published", Status: status, Reason: why, Message: bounded(message), ObservedGeneration: processed.Generation})
		if current.Generation == processed.Generation && current.DeletionTimestamp.IsZero() {
			if published && result.Hash != "" {
				status, why, message := metav1.ConditionFalse, "InSync", "Managed file matches the last published content."
				if result.Missing {
					status, why, message = metav1.ConditionTrue, "FileMissing", "Managed file is absent at branch HEAD."
				} else if result.Hash != hash {
					status, why, message = metav1.ConditionTrue, "ExternalModification", "Branch HEAD differs from the last published content."
				}
				condition(current, "GitDrift", status, why, message)
			} else if current.Status.LastPublishedRevision != "" && reason != "PublishPending" && (!published || reason == "Unchanged") {
				condition(current, "GitDrift", metav1.ConditionUnknown, "AccessFailed", "Cannot compare the managed file with the published content.")
			}
		}

	})
}
func resourceReference(raw []byte) *api.ResourceReference {
	var obj struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return &api.ResourceReference{APIVersion: obj.APIVersion, Kind: obj.Kind, Name: obj.Metadata.Name, Namespace: obj.Metadata.Namespace}
}

func (r *GitResourceReconciler) affected(ctx context.Context, changed client.Object) []reconcile.Request {
	list := &api.GitResourceList{}
	if err := r.List(ctx, list); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Unable to enqueue configuration dependents")
		return nil
	}
	names := map[string]bool{}
	switch obj := changed.(type) {
	case *api.ClusterGitConfig:
		names[obj.Name] = true
	case *corev1.Secret:
		configs := &api.ClusterGitConfigList{}
		if err := r.List(ctx, configs); err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "Unable to list configurations")
			return nil
		}
		for _, config := range configs.Items {
			if config.Spec.Credentials.SecretRef.Namespace == obj.Namespace && config.Spec.Credentials.SecretRef.Name == obj.Name {
				names[config.Name] = true
			}
		}
	}
	requests := []reconcile.Request{}
	for _, cr := range list.Items {
		if names[ConfigName(&cr)] {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&cr)})
		}
	}
	return requests
}
func (r *GitResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}
	if r.Publisher == nil {
		r.Publisher = &gitwriter.Publisher{}
	}
	if r.Workers == 0 {
		r.Workers = 4
	}
	return ctrl.NewControllerManagedBy(mgr).For(&api.GitResource{}, builder.WithPredicates(publisherPredicate)).
		Watches(&api.ClusterGitConfig{}, handler.EnqueueRequestsFromMapFunc(r.affected), builder.WithPredicates(configPredicate)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.affected)).
		WithOptions(runtimecontroller.Options{MaxConcurrentReconciles: r.Workers}).Complete(r)
}

func publicationRetryDelay(attempt int) time.Duration {
	delay := time.Duration(1<<min(attempt-1, 2)) * 500 * time.Millisecond
	return delay/2 + time.Duration(rand.Int64N(int64(delay)))
}
