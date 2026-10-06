package controller

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"time"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	gitwriter "github.com/inelson/git-state-controller/internal/git"
	"github.com/inelson/git-state-controller/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
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
}

// +kubebuilder:rbac:groups=gitops.example.io,resources=gitresources,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=gitops.example.io,resources=gitresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gitops.example.io,resources=gitresources/finalizers,verbs=update
// +kubebuilder:rbac:groups=gitops.example.io,resources=clustergitconfigs,verbs=get;list;watch
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
		Delete: !cr.DeletionTimestamp.IsZero(), PreviousRevision: cr.Status.LastPublishedRevision}
	if (cr.Spec.GitConfigRef.Kind != "" && cr.Spec.GitConfigRef.Kind != "ClusterGitConfig") || (cr.Spec.DeletionPolicy != "" && cr.Spec.DeletionPolicy != "Delete") {
		return op, "InvalidSpec", errors.New("unsupported configuration kind or deletion policy")
	}
	if err := gitwriter.ValidateDestination(op.URL, op.Branch, op.Path, r.AllowHTTP); err != nil {
		return op, "InvalidSpec", err
	}
	if !op.Delete {
		var err error
		op.Content, _, err = manifest.Render(cr.Spec.Manifest.Raw)
		if err != nil {
			return op, "InvalidSpec", err
		}
	}
	config := &api.ClusterGitConfig{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: ConfigName(cr)}, config); err != nil {
		return op, "ConfigNotFound", errors.New("cannot read ClusterGitConfig")
	}
	creds := config.Spec.Credentials
	if creds.Source != "Secret" || creds.SecretRef.Namespace != r.Namespace {
		return op, "CredentialsInvalid", errors.New("credentials must reference a Secret in the controller namespace")
	}
	secret := &corev1.Secret{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: creds.SecretRef.Name}, secret); err != nil {
		return op, "CredentialsInvalid", errors.New("cannot read credentials Secret")
	}
	op.Credentials = gitwriter.Credentials{Username: string(secret.Data["username"]), Password: string(secret.Data["password"])}
	if op.Credentials.Username == "" || op.Credentials.Password == "" {
		return op, "CredentialsInvalid", errors.New("credentials require nonempty username and password")
	}
	op.AuthorName, op.AuthorEmail = config.Spec.CommitAuthor.Name, config.Spec.CommitAuthor.Email
	if op.AuthorName == "" {
		op.AuthorName = "git-state-controller"
	}
	if op.AuthorEmail == "" {
		op.AuthorEmail = "git-state-controller@example.invalid"
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
		controllerutil.AddFinalizer(cr, Finalizer)
		if err := r.Update(ctx, cr); err != nil {
			return ctrl.Result{}, err
		}
		// Start from an API read after persisting the finalizer, before making Git state.
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
	var result gitwriter.Result
	var op gitwriter.Operation
	var reason string
	var err error
attempts:
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(30+rand.IntN(100)) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				err = ctx.Err()
				break attempts
			case <-timer.C:
			}
			if err = r.Reader.Get(ctx, req.NamespacedName, cr); err != nil {
				if ctx.Err() != nil {
					break
				}
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			if cr.UID != uid {
				return ctrl.Result{}, nil
			}
		}
		op, reason, err = r.resolve(ctx, cr)
		if err != nil {
			break
		}
		result, err = r.Publisher.Attempt(ctx, op)
		if !errors.Is(err, gitwriter.ErrRetry) {
			break
		}
	}
	// The Git deadline must not prevent reporting a timeout through Kubernetes.
	// Keep this API work bounded and tied to manager shutdown independently.
	ctx, statusCancel := context.WithTimeout(requestCtx, 30*time.Second)
	defer statusCancel()
	if err != nil {
		if reason == "" {
			reason = "PublishFailed"
			if errors.Is(err, gitwriter.ErrCredentialsInvalid) {
				reason = "CredentialsInvalid"
			}
		}
		if !cr.DeletionTimestamp.IsZero() {
			reason = "DeleteFailed"
		}
		// Transport diagnostics are intentionally generic and contain no credentials.
		if r.Recorder != nil {
			r.Recorder.Eventf(cr, nil, corev1.EventTypeWarning, reason, "Publish", "%s", err.Error())
		}
		if statusErr := r.setStatus(ctx, cr, false, reason, err.Error(), gitwriter.Result{}, ""); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	if op.Delete {
		err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			current := &api.GitResource{}
			if err := r.Reader.Get(ctx, req.NamespacedName, current); err != nil {
				return client.IgnoreNotFound(err)
			}
			if current.UID != uid {
				return nil
			}
			controllerutil.RemoveFinalizer(current, Finalizer)
			return r.Update(ctx, current)
		})
		if err == nil {
			ctrl.LoggerFrom(ctx).Info("Removed Git file and released finalizer", "resource", req.NamespacedName, "generation", cr.Generation)
		}
		return ctrl.Result{}, err
	}
	_, hash, err := manifest.Render(cr.Spec.Manifest.Raw)
	if err != nil {
		return ctrl.Result{}, err
	}
	reason = "Unchanged"
	if result.Changed {
		reason = "Pushed"
	}
	if err = r.setStatus(ctx, cr, true, reason, "Desired manifest is present in the remote repository.", result, hash); err != nil {
		return ctrl.Result{}, err
	}
	if result.Changed {
		ctrl.LoggerFrom(ctx).Info("Published GitResource", "resource", req.NamespacedName, "generation", cr.Generation, "revision", result.Revision)
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// A status conflict never repeats Git work. The UID and processed generation travel together.
func (r *GitResourceReconciler) setStatus(ctx context.Context, processed *api.GitResource, published bool, reason, message string, result gitwriter.Result, hash string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &api.GitResource{}
		if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(processed), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != processed.UID || current.Status.ObservedGeneration > processed.Generation {
			return nil
		}
		before := current.Status.DeepCopy()
		current.Status.ObservedGeneration = processed.Generation
		status := metav1.ConditionFalse
		if published {
			status = metav1.ConditionTrue
			current.Status.LastPublishedGeneration = processed.Generation
			current.Status.LastPublishedRevision = result.Revision
			current.Status.LastPublishedContentHash = hash
		}
		// Preserve a successful reason on periodic no-ops so status does not churn Pushed -> Unchanged.
		previous := meta.FindStatusCondition(current.Status.Conditions, "Published")
		if published && !result.Changed && previous != nil && previous.Status == metav1.ConditionTrue && previous.ObservedGeneration == processed.Generation {
			reason = previous.Reason
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "Published", Status: status, ObservedGeneration: processed.Generation, Reason: reason, Message: message})
		if reflect.DeepEqual(before, &current.Status) {
			return nil
		}
		return r.Status().Update(ctx, current)
	})
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
	return ctrl.NewControllerManagedBy(mgr).For(&api.GitResource{}).
		Watches(&api.ClusterGitConfig{}, handler.EnqueueRequestsFromMapFunc(r.affected)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.affected)).
		WithOptions(runtimecontroller.Options{MaxConcurrentReconciles: r.Workers}).Complete(r)
}
