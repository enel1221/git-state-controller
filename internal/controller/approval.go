package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"reflect"
	"strings"
	"unicode"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"github.com/inelson/git-state-controller/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ApprovalPolicyAnnotation = "gitops.example.io/approval-required"
const ApprovedRequestAnnotation = "gitops.example.io/approved-request"
const ApprovedByAnnotation = "gitops.example.io/approved-by"
const EnabledAnnotation = "gitops.example.io/enabled"

var errApprovalPending = errors.New("approval is pending")

func deletionRequested(cr *api.GitResource) bool {
	return !cr.DeletionTimestamp.IsZero() || cr.Spec.Change != nil && cr.Spec.Change.Action == "Delete"
}
func effectiveOperation(cr *api.GitResource) string {
	if deletionRequested(cr) {
		policy := cr.Spec.DeletionPolicy
		if !cr.DeletionTimestamp.IsZero() && cr.Status.Cleanup != nil {
			policy = cr.Status.Cleanup.Policy
		}
		if policy == "Orphan" {
			return "Orphan"
		}
		return "Delete"
	}
	if adopting(cr) {
		return "Adopt"
	}
	return "Apply"
}
func approvalRequest(cr *api.GitResource) string {
	return fmt.Sprintf("v1:%s:%d:%s", cr.UID, cr.Generation, effectiveOperation(cr))
}
func desiredUnchanged(cr *api.GitResource) bool {
	if deletionRequested(cr) || adopting(cr) || cr.Status.LastPublishedRevision == "" {
		return false
	}
	content, _, err := manifest.RenderManaged(cr.Spec.Manifest.Raw, cr.Namespace, cr.Name, string(cr.UID), "managed")
	if err != nil {
		return false
	}
	if manifest.Hash(content) == cr.Status.LastPublishedContentHash {
		return true
	}
	// Keep v0.1 continuity without adding ownership markers solely for an upgrade.
	_, hash, err := manifest.Render(cr.Spec.Manifest.Raw)
	return err == nil && hash == cr.Status.LastPublishedContentHash
}
func validateAuthor(author *api.ChangeAuthor) error {
	if author == nil {
		return nil
	}
	address, err := mail.ParseAddress(author.Email)
	if err != nil || address.Address != author.Email || strings.TrimSpace(author.Name) == "" || len(author.Name) > 128 || len(author.Email) > 254 || strings.ContainsAny(author.Name, "<>") || strings.ContainsAny(author.Email, "<>") || strings.IndexFunc(author.Name, unicode.IsControl) >= 0 || strings.IndexFunc(author.Email, unicode.IsControl) >= 0 {
		return errors.New("change author requires a usable name and email without header characters")
	}
	return nil
}

type approvalDecision struct {
	envelope        *api.ApprovalStatus
	approver        *approvalAttribution
	status          metav1.ConditionStatus
	reason, message string
	allowed         bool
}

// Attribution is supplied by the caller and bound to one advertised request.
type approvalAttribution struct {
	Request string `json:"request"`
	Name    string `json:"name"`
	Email   string `json:"email"`
}

func (a approvalDecision) sameEvidence(b approvalDecision) bool {
	return a.envelope.Request == b.envelope.Request && reflect.DeepEqual(a.envelope.Required, b.envelope.Required) && reflect.DeepEqual(a.approver, b.approver)
}

func approvalAttributor(cr *api.GitResource, request string) (*approvalAttribution, error) {
	raw := cr.Annotations[ApprovedByAnnotation]
	if raw == "" {
		return nil, nil
	}
	var who approvalAttribution
	if len(raw) > 1024 || json.Unmarshal([]byte(raw), &who) != nil {
		return nil, errors.New("approved-by must be bounded JSON with request, name and email")
	}
	if who.Request != request {
		return nil, nil // Older attribution cannot become this request's approver.
	}
	if err := validateAuthor(&api.ChangeAuthor{Name: who.Name, Email: who.Email}); err != nil {
		return nil, errors.New("approver requires a usable name and email without header characters")
	}
	return &who, nil
}

func approvalTrailers(a approvalDecision) string {
	if a.envelope.Required == nil || !*a.envelope.Required {
		return "GitResource-Approval: NotRequired\n"
	}
	footer := "GitResource-Approval: Approved\nGitResource-Approval-Request: " + a.envelope.Request + "\n"
	if a.approver != nil {
		footer += fmt.Sprintf("Approved-by: %s <%s>\n", a.approver.Name, a.approver.Email)
	}
	return footer
}

func (r *GitResourceReconciler) approval(ctx context.Context, cr *api.GitResource) approvalDecision {
	a := approvalDecision{envelope: &api.ApprovalStatus{}, status: metav1.ConditionUnknown, reason: "ApprovalPolicyUnknown", message: "Cannot read namespace approval policy."}
	if !cr.DeletionTimestamp.IsZero() && cr.Status.Cleanup != nil {
		a.envelope = cr.Status.Approval.DeepCopy()
		if a.envelope == nil {
			a.envelope = &api.ApprovalStatus{}
		}
		a.envelope.Request = cr.Status.Cleanup.Request
		a.envelope.Operation = effectiveOperation(cr)
		a.status, a.reason, a.message, a.allowed = metav1.ConditionTrue, "Approved", "Completing the accepted cleanup operation.", true
		return a
	}
	namespace := &corev1.Namespace{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: cr.Namespace}, namespace); err != nil {
		a.message = "Cannot read namespace approval policy: " + err.Error()
		return a
	}
	policy := namespace.Annotations[ApprovalPolicyAnnotation]
	if policy != "" && policy != "false" && policy != "true" {
		a.reason, a.message = "InvalidApprovalPolicy", "Namespace approval policy must be true, false or absent."
		return a
	}
	required := policy == "true"
	a.envelope.Required = &required
	if desiredUnchanged(cr) {
		a.status, a.reason, a.message, a.allowed = metav1.ConditionTrue, "NoChanges", "No new external mutation requires approval.", true
		return a
	}
	generation := cr.Generation
	a.envelope.Generation, a.envelope.Operation, a.envelope.Request = &generation, effectiveOperation(cr), approvalRequest(cr)
	if !required {
		a.status, a.reason, a.message, a.allowed = metav1.ConditionTrue, "NotRequired", "Namespace does not require approval.", true
		return a
	}
	if cr.Annotations[ApprovedRequestAnnotation] == a.envelope.Request {
		var err error
		a.approver, err = approvalAttributor(cr, a.envelope.Request)
		if err != nil {
			a.status, a.reason, a.message = metav1.ConditionFalse, "InvalidApprover", err.Error()
			return a
		}
		a.status, a.reason, a.message, a.allowed = metav1.ConditionTrue, "Approved", "Current request has matching approval.", true
		return a
	}
	a.status, a.reason, a.message = metav1.ConditionFalse, "ApprovalPending", "Namespace requires approval for this "+a.envelope.Operation+" request."
	if cr.Annotations[ApprovedRequestAnnotation] != "" {
		a.reason = "ApprovalMismatch"
	}
	return a
}
func (r *GitResourceReconciler) recordApproval(ctx context.Context, cr *api.GitResource, a approvalDecision) error {
	return patchStatus(ctx, r.Client, r.Reader, cr, "publisher", func(current *api.GitResource) {
		if current.Generation != cr.Generation || effectiveOperation(current) != effectiveOperation(cr) || !current.DeletionTimestamp.Equal(cr.DeletionTimestamp) {
			return
		}
		current.Status.Approval = a.envelope.DeepCopy()
		condition(current, "Approved", a.status, a.reason, a.message)
	})
}

// Completion only consumes the exact input whose publication status is durable.
func (r *GitResourceReconciler) consumeChange(ctx context.Context, processed *api.GitResource, approval string, adoption bool) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &api.GitResource{}
		if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(processed), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != processed.UID || current.Generation != processed.Generation || !current.DeletionTimestamp.IsZero() || paused(current) || !reflect.DeepEqual(current.Spec.Change, processed.Spec.Change) || current.Status.LastPublishedGeneration != processed.Generation {
			return nil
		}
		if adoption && !r.handoffVerified(ctx, current, current.Status.LastPublishedRevision, "managed") {
			return nil
		}
		before := current.DeepCopy()
		current.Spec.Change = nil
		if approval != "" && processed.Annotations[ApprovedRequestAnnotation] == approval && current.Annotations[ApprovedRequestAnnotation] == approval && current.Annotations[ApprovedByAnnotation] == processed.Annotations[ApprovedByAnnotation] {
			delete(current.Annotations, ApprovedRequestAnnotation)
			if who, err := approvalAttributor(processed, approval); err == nil && who != nil {
				delete(current.Annotations, ApprovedByAnnotation)
			}
		}
		if adoption {
			delete(current.Annotations, AdoptAnnotation)
		}
		if reflect.DeepEqual(before.Spec, current.Spec) && reflect.DeepEqual(before.Annotations, current.Annotations) {
			return nil
		}
		return r.Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
}
