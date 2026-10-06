package controller

import (
	"context"
	"errors"
	"net/mail"
	"reflect"
	"strings"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *GitResourceReconciler) loadConfig(ctx context.Context, name string) (*api.ClusterGitConfig, *corev1.Secret, string, error) {
	config := &api.ClusterGitConfig{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Name: name}, config); err != nil {
		return nil, nil, "ConfigNotFound", errors.New("cannot read ClusterGitConfig")
	}
	invalid := func(secret *corev1.Secret, message string) (*api.ClusterGitConfig, *corev1.Secret, string, error) {
		r.recordConfig(ctx, config, secret, false, "CredentialsInvalid", nil)
		return config, secret, "CredentialsInvalid", errors.New(message)
	}
	credentials := config.Spec.Credentials
	if credentials.Source != "Secret" || credentials.SecretRef.Namespace != r.Namespace || credentials.SecretRef.Name == "" {
		return invalid(nil, "credentials must reference a Secret in the controller namespace")
	}
	secret := &corev1.Secret{}
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: credentials.SecretRef.Name}, secret); err != nil {
		return invalid(nil, "cannot read credentials Secret")
	}
	if len(secret.Data["username"]) == 0 || len(secret.Data["password"]) == 0 {
		return invalid(secret, "credentials require nonempty username and password")
	}
	nameAuthor, email := config.Spec.CommitAuthor.Name, config.Spec.CommitAuthor.Email
	if nameAuthor == "" {
		nameAuthor = "git-state-controller"
	}
	if email == "" {
		email = "git-state-controller@example.invalid"
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || strings.TrimSpace(nameAuthor) == "" || strings.ContainsAny(nameAuthor, "\r\n<>") {
		return invalid(secret, "commit identity must have a usable name and email")
	}
	ready := meta.FindStatusCondition(config.Status.Conditions, "Ready")
	if config.Status.ObservedGeneration != config.Generation || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != config.Generation || ready.Reason != "CredentialsLoaded" || !reflect.DeepEqual(config.Status.CredentialsRef, credentialReference(config, secret)) {
		r.recordConfig(ctx, config, secret, true, "CredentialsLoaded", nil)
	}
	return config, secret, "", nil
}

func credentialReference(config *api.ClusterGitConfig, secret *corev1.Secret) *api.CredentialReference {
	ref := &api.CredentialReference{Namespace: config.Spec.Credentials.SecretRef.Namespace, Name: config.Spec.Credentials.SecretRef.Name}
	if secret != nil {
		ref.UID = string(secret.UID)
		ref.ResourceVersion = secret.ResourceVersion
	}
	return ref
}

// Evidence is tied to the configuration and Secret actually used, never a later rotation.
func (r *GitResourceReconciler) recordConfig(ctx context.Context, used *api.ClusterGitConfig, secret *corev1.Secret, loaded bool, reason string, evidence *api.GitAccessObservation) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &api.ClusterGitConfig{}
		if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(used), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != used.UID || current.Generation != used.Generation || !reflect.DeepEqual(current.Spec, used.Spec) {
			return nil
		}
		if secret != nil {
			latest := &corev1.Secret{}
			if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(secret), latest); err != nil {
				return nil
			}
			if latest.UID != secret.UID || latest.ResourceVersion != secret.ResourceVersion {
				return nil
			}
		}
		before := current.DeepCopy()
		current.Status.ObservedGeneration = used.Generation
		current.Status.CredentialsRef = credentialReference(used, secret)
		value, message := metav1.ConditionFalse, "Credentials could not be loaded; check Secret reference, keys and commit identity."
		if loaded {
			value, message = metav1.ConditionTrue, "Secret loaded; repository-specific access is reported separately."
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "Ready", Status: value, ObservedGeneration: used.Generation, Reason: reason, Message: message})
		if evidence != nil {
			next := *evidence
			if previous := current.Status.LastAccess; previous != nil {
				a, b := *previous, next
				a.LastUpdatedAt = metav1.Time{}
				b.LastUpdatedAt = metav1.Time{}
				if reflect.DeepEqual(a, b) {
					next.LastUpdatedAt = previous.LastUpdatedAt
				}
			}
			current.Status.LastAccess = &next
		}
		if reflect.DeepEqual(before.Status, current.Status) {
			return nil
		}
		err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		statusPatchCounter.WithLabelValues("config", outcome(err)).Inc()
		return err
	})
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cannot record configuration diagnostics")
	}
}
