//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"github.com/inelson/git-state-controller/internal/controller"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *stack) approvalPending(t *testing.T, cr *api.GitResource) *api.GitResource {
	t.Helper()
	out := &api.GitResource{}
	poll(t, "current approval request", func(ctx context.Context) bool {
		if s.Get(ctx, client.ObjectKeyFromObject(cr), out) != nil {
			return false
		}
		a := meta.FindStatusCondition(out.Status.Conditions, "Approved")
		return a != nil && a.ObservedGeneration == out.Generation && a.Status == "False" && out.Status.Approval != nil && out.Status.Approval.Generation != nil && *out.Status.Approval.Generation == out.Generation
	})
	return out
}
func (s *stack) approve(t *testing.T, reviewed *api.GitResource) {
	t.Helper()
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &api.GitResource{}
		if err := s.Get(context.Background(), client.ObjectKeyFromObject(reviewed), current); err != nil {
			return err
		}
		if current.UID != reviewed.UID || current.Generation != reviewed.Generation || !reflect.DeepEqual(current.Spec, reviewed.Spec) || current.Status.Approval.Request != reviewed.Status.Approval.Request {
			return fmt.Errorf("stale review")
		}
		before := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[controller.ApprovedRequestAnnotation] = reviewed.Status.Approval.Request
		identity, err := json.Marshal(map[string]string{"request": reviewed.Status.Approval.Request, "name": "Live Reviewer", "email": "reviewer@example.invalid"})
		if err != nil {
			return err
		}
		current.Annotations[controller.ApprovedByAnnotation] = string(identity)
		return s.Patch(context.Background(), current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestV03(t *testing.T) {
	s := newStack(t)
	ns := &corev1.Namespace{}
	if err := s.Get(context.Background(), client.ObjectKey{Name: s.namespace}, ns); err != nil {
		t.Fatal(err)
	}
	before := ns.DeepCopy()
	ns.Annotations = map[string]string{controller.ApprovalPolicyAnnotation: "true", controller.EnabledAnnotation: "true"}
	if err := s.Patch(context.Background(), ns, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	cr := s.create(t, "approved")
	s.edit(t, cr, func(c *api.GitResource) {
		c.Spec.Change = &api.Change{Message: "Live approved create", Author: &api.ChangeAuthor{Name: "Live Editor", Email: "editor@example.invalid"}}
	})
	cr = s.approvalPending(t, cr)
	if cr.Status.LastPublishedRevision != "" {
		t.Fatal("unapproved create published")
	}
	repo := s.clone(t)
	head, _ := repo.Head()
	if _, err := fileAt(repo, head.Hash().String(), cr.Spec.Repository.Path); err == nil {
		t.Fatal("unapproved file created")
	}
	request := cr.Status.Approval.Request
	approvedAt := time.Now()
	s.approve(t, cr)
	cr = s.published(t, cr)
	publishedAt := time.Now()
	cr = s.ready(t, cr)
	if cr.Spec.Change != nil || cr.Annotations[controller.ApprovedRequestAnnotation] != "" || meta.FindStatusCondition(cr.Status.Conditions, "Approved").Reason != "NoChanges" {
		t.Fatal("completed metadata not consumed", cr)
	}
	repo = s.clone(t)
	history, err := repo.Log(&gogit.LogOptions{FileName: &cr.Spec.Repository.Path})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := history.Next()
	history.Close()
	if err != nil || !strings.Contains(commit.Message, "GitResource-Approval-Request: "+request+"\nApproved-by: Live Reviewer <reviewer@example.invalid>\n") {
		t.Fatal("live approval footer missing", err)
	}
	if cr.Annotations[controller.ApprovedByAnnotation] != "" {
		t.Fatal("live approver not consumed")
	}
	previous := cr.Status.LastPublishedRevision
	s.edit(t, cr, func(c *api.GitResource) { changeValue(c, "proposal") })
	cr = s.approvalPending(t, cr)
	if cr.Status.LastPublishedRevision != previous {
		t.Fatal("pending update replaced deployment pin")
	}
	reviewed := cr.DeepCopy()
	s.edit(t, cr, func(c *api.GitResource) { changeValue(c, "newer proposal") })
	cr = s.approvalPending(t, cr)
	s.edit(t, cr, func(current *api.GitResource) {
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[controller.ApprovedRequestAnnotation] = reviewed.Status.Approval.Request
		identity, err := json.Marshal(map[string]string{"request": reviewed.Status.Approval.Request, "name": "Live Reviewer", "email": "reviewer@example.invalid"})
		if err != nil {
			t.Fatal(err)
		}
		current.Annotations[controller.ApprovedByAnnotation] = string(identity)
	})
	poll(t, "stale approval rejected", func(ctx context.Context) bool {
		if s.Get(ctx, client.ObjectKeyFromObject(cr), cr) != nil {
			return false
		}
		a := meta.FindStatusCondition(cr.Status.Conditions, "Approved")
		return a != nil && a.Reason == "ApprovalMismatch"
	})
	if cr.Status.LastPublishedRevision != previous {
		t.Fatal("stale evidence published newer bytes")
	}
	s.approve(t, cr)
	cr = s.ready(t, cr)
	target := &corev1.ConfigMap{}
	if err := s.Get(context.Background(), types.NamespacedName{Namespace: s.namespace, Name: cr.Name}, target); err != nil {
		t.Fatal(err)
	}
	original := target.DeepCopy()
	target.Finalizers = append(target.Finalizers, "testing.gitops.example.io/hold")
	if err := s.Patch(context.Background(), target, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		live := &corev1.ConfigMap{}
		if s.Get(context.Background(), client.ObjectKeyFromObject(target), live) == nil {
			old := live.DeepCopy()
			live.Finalizers = slices.DeleteFunc(live.Finalizers, func(f string) bool { return f == "testing.gitops.example.io/hold" })
			_ = s.Patch(context.Background(), live, client.MergeFrom(old))
		}
	})
	deleteStarted := time.Now()
	if err := s.Delete(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	cr = s.approvalPending(t, cr)
	if cr.Status.Approval.Operation != "Delete" || cr.Status.Cleanup != nil {
		t.Fatal("raw delete bypassed gate")
	}
	s.approve(t, cr)
	poll(t, "Git complete with wrapper retained for downstream deletion", func(ctx context.Context) bool {
		if s.Get(ctx, client.ObjectKeyFromObject(cr), cr) != nil {
			return false
		}
		ready := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
		return cr.Status.Cleanup != nil && cr.Status.Cleanup.Revision != "" && ready != nil && (ready.Reason == "WaitingForApplicationDeletion" || ready.Reason == "WaitingForResourceDeletion")
	})
	gitDeletedAt := time.Now()
	checkpoint := cr.Status.Cleanup.DeepCopy()
	s.command(t, "kubectl", "--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev", "rollout", "restart", "-n", "git-state-system", "deployment/controller-manager")
	s.command(t, "kubectl", "--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev", "rollout", "status", "-n", "git-state-system", "deployment/controller-manager", "--timeout=180s")
	if err := s.Get(context.Background(), client.ObjectKeyFromObject(cr), cr); err != nil {
		t.Fatal("wrapper released while target held", err)
	}
	if cr.Status.Cleanup.Revision != checkpoint.Revision || cr.Status.Cleanup.Request != checkpoint.Request {
		t.Fatal("restart replanned cleanup")
	}
	if err := s.Get(context.Background(), client.ObjectKeyFromObject(target), target); err != nil {
		t.Fatal(err)
	}
	original = target.DeepCopy()
	target.Finalizers = slices.DeleteFunc(target.Finalizers, func(f string) bool { return f == "testing.gitops.example.io/hold" })
	if err := s.Patch(context.Background(), target, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatal(err)
	}
	s.deleted(t, cr)
	s.downstreamDeleted(t, cr)
	if err := s.Get(context.Background(), client.ObjectKeyFromObject(target), &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatal("target still exists", err)
	}
	report := map[string]any{"approval_wait_seconds": approvedAt.Sub(started).Seconds(), "approval_to_publication_seconds": publishedAt.Sub(approvedAt).Seconds(), "delete_to_git_cleanup_seconds": gitDeletedAt.Sub(deleteStarted).Seconds(), "delete_to_wrapper_removal_seconds": time.Since(deleteStarted).Seconds(), "target_finalizer_intentionally_held": true}
	dir := filepath.Join(s.root, "reports", fmt.Sprintf("approval-v03-%d", started.Unix()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "results.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("approval/deletion timing report: %s", dir)
}
