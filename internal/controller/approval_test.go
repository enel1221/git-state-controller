package controller

import (
	"context"
	"encoding/json"
	"errors"

	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"github.com/inelson/git-state-controller/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func setPolicy(t *testing.T, r *GitResourceReconciler, value string) {
	t.Helper()
	ns := &corev1.Namespace{}
	if err := r.Get(context.Background(), client.ObjectKey{Name: "demo"}, ns); err != nil {
		t.Fatal(err)
	}
	ns.Annotations = map[string]string{ApprovalPolicyAnnotation: value}
	if err := r.Update(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
}
func approve(t *testing.T, r *GitResourceReconciler, cr *api.GitResource) *api.GitResource {
	t.Helper()
	cr = fresh(t, r, cr)
	if cr.Status.Approval == nil || cr.Status.Approval.Request == "" {
		t.Fatal("no advertised request")
	}
	before := cr.DeepCopy()
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[ApprovedRequestAnnotation] = cr.Status.Approval.Request
	if err := r.Patch(context.Background(), cr, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatal(err)
	}
	return cr
}
func TestApprovalRequestsPolicyAndEvents(t *testing.T) {
	_, r, cr := fixture(t)
	first := approvalRequest(cr)
	for _, change := range []func(*api.GitResource){func(c *api.GitResource) { c.Generation++ }, func(c *api.GitResource) { c.UID = "replacement" }, func(c *api.GitResource) { c.Annotations = map[string]string{AdoptAnnotation: "true"} }, func(c *api.GitResource) { c.Spec.Change = &api.Change{Action: "Delete"} }, func(c *api.GitResource) {
		c.Spec.Change = &api.Change{Action: "Delete"}
		c.Spec.DeletionPolicy = "Orphan"
	}} {
		next := cr.DeepCopy()
		change(next)
		if approvalRequest(next) == first {
			t.Fatal("stale request reused")
		}
	}
	next := cr.DeepCopy()
	next.Annotations = map[string]string{PausedAnnotation: "true"}
	next.ResourceVersion = "new"
	next.Status.LastPublishedRevision = strings.Repeat("a", 40)
	if approvalRequest(next) != first {
		t.Fatal("metadata/status changed intent")
	}
	for _, tc := range []struct {
		policy, reason string
		allowed        bool
	}{{"", "NotRequired", true}, {"false", "NotRequired", true}, {"true", "ApprovalPending", false}, {"yes", "InvalidApprovalPolicy", false}} {
		setPolicy(t, r, tc.policy)
		a := r.approval(context.Background(), cr)
		if a.allowed != tc.allowed || a.reason != tc.reason {
			t.Fatal(tc, a)
		}
	}
	if err := r.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}); err != nil {
		t.Fatal(err)
	}
	if a := r.approval(context.Background(), cr); a.allowed || a.envelope.Required != nil {
		t.Fatal("lookup failure allowed operation", a)
	}
	before := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}
	after := before.DeepCopy()
	after.Annotations = map[string]string{ApprovalPolicyAnnotation: "true"}
	if !namespacePredicate.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) || len(r.namespaceRequests(context.Background(), after)) != 1 {
		t.Fatal("namespace policy event lost")
	}
	after = before.DeepCopy()
	after.Status.Phase = corev1.NamespaceActive
	if namespacePredicate.Update(event.UpdateEvent{ObjectOld: before, ObjectNew: after}) {
		t.Fatal("namespace status wakes publisher")
	}
	next = cr.DeepCopy()
	next.Annotations = map[string]string{ApprovedRequestAnnotation: first}
	if !publisherPredicate.Update(event.UpdateEvent{ObjectOld: cr, ObjectNew: next}) {
		t.Fatal("approval event lost")
	}
}
func TestApprovalAttributionConsumptionAndNoop(t *testing.T) {
	s, r, cr := fixture(t)
	setPolicy(t, r, "true")
	cr.Spec.Change = &api.Change{Message: "Reviewed change", Author: &api.ChangeAuthor{Name: "Alex", Email: "alex@example.com"}}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if s.Count(t) != 1 || cr.Spec.Change == nil || cr.Status.Approval.Operation != "Apply" || meta.FindStatusCondition(cr.Status.Conditions, "Ready").Reason != "ApprovalPending" {
		t.Fatal("unapproved side effect", cr.Status)
	}
	cr = approve(t, r, cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Spec.Change != nil || cr.Annotations[ApprovedRequestAnnotation] != "" || s.Count(t) != 2 {
		t.Fatal("completed input not consumed")
	}
	repo, err := gogit.PlainOpen(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Head()
	commit, _ := repo.CommitObject(head.Hash())
	if commit.Author.Name != "Alex" || commit.Committer.Name != "git-state-controller" || commit.Committer.Email == commit.Author.Email {
		t.Fatal("author leaked into committer", commit)
	}
	sha := cr.Status.LastPublishedRevision
	cr.Generation++
	cr.Spec.Change = &api.Change{Message: "no content change", Author: &api.ChangeAuthor{Name: "Other", Email: "other@example.com"}}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Spec.Change != nil || cr.Status.LastPublishedRevision != sha || s.Count(t) != 2 || meta.FindStatusCondition(cr.Status.Conditions, "Approved").Reason != "NoChanges" {
		t.Fatal("no-op requested approval or committed")
	}
	cr.Generation++
	cr.Spec.Manifest.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"new":"value"}}`)
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[ApprovedRequestAnnotation] = "v1:original-uid:1:Apply"
	_ = r.Update(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.LastPublishedRevision != sha || s.Count(t) != 2 || meta.FindStatusCondition(cr.Status.Conditions, "Approved").Reason != "ApprovalMismatch" {
		t.Fatal("stale approval used")
	}
	setPolicy(t, r, "false")
	reconcileCR(t, r, cr)
	head, _ = repo.Head()
	commit, _ = repo.CommitObject(head.Hash())
	if commit.Author.Name != "git-state-controller" {
		t.Fatal("attribution became a default")
	}
}
func TestGateRechecksBeforePushAndRecoveryDoesNotRequireAnotherToken(t *testing.T) {
	s, r, cr := fixture(t)
	r.Publisher.BeforePush = func() { setPolicy(t, r, "true") }
	reconcileCR(t, r, cr)
	if s.Count(t) != 1 {
		t.Fatal("policy change before push ignored")
	}
	r.Publisher.BeforePush = nil
	cr = fresh(t, r, cr)
	cr.Spec.Change = &api.Change{Message: "User body\nGitResource-Generation: 0"}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	cr = approve(t, r, cr)
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if gr, ok := obj.(*api.GitResource); ok && gr.Status.LastPublishedRevision != "" {
			return errors.New("lost publication status")
		}
		return c.SubResource(sub).Patch(ctx, obj, p, opts...)
	}})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err == nil {
		t.Fatal("status failure hidden")
	}
	if s.Count(t) != 2 {
		t.Fatal("expected accepted commit")
	}
	r.Client = base
	cr = fresh(t, r, cr)
	delete(cr.Annotations, ApprovedRequestAnnotation)
	_ = r.Update(context.Background(), cr)
	external(t, r, cr, []byte("external content\n"), false)
	reconcileCR(t, r, cr)
	if !meta.IsStatusConditionTrue(fresh(t, r, cr).Status.Conditions, "GitDrift") {
		t.Fatal("publication recovery hid current branch drift")
	}
	reconcileCR(t, r, cr)
	if s.Count(t) != 3 || !publicationCurrent(fresh(t, r, cr)) {
		t.Fatal("accepted commit did not recover read-only")
	}
}
func TestConsumptionLeavesNewerGenerationAndUnrelatedAnnotations(t *testing.T) {
	_, r, cr := fixture(t)
	cr.Spec.Change = &api.Change{Message: "first"}
	_ = r.Update(context.Background(), cr)
	// A writer arrives after Git verification but before the publication status patch.
	base := r.Client
	changed := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if gr, ok := obj.(*api.GitResource); ok && gr.Status.LastPublishedRevision != "" && !changed {
			changed = true
			next := fresh(t, r, cr)
			next.Generation++
			next.Spec.Change = &api.Change{Message: "first"}
			next.Spec.Manifest.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"new":"value"}}`)
			next.Annotations = map[string]string{"unrelated": "keep"}
			if err := base.Update(ctx, next); err != nil {
				t.Fatal(err)
			}
		}
		return c.SubResource(sub).Patch(ctx, obj, p, opts...)
	}})
	reconcileCR(t, r, cr)
	next := fresh(t, r, cr)
	if next.Spec.Change == nil || next.Annotations["unrelated"] != "keep" || next.Status.LastPublishedGeneration != 1 {
		t.Fatal("new request consumed", next)
	}
}
func TestPreparedAndRawDeleteApprovalAndFrozenCleanup(t *testing.T) {
	for _, raw := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "raw"}[raw], func(t *testing.T) {
			s, r, cr := fixture(t)
			a := inventoryFixture(t, r)
			reconcileCR(t, r, cr)
			handoff(t, a)
			cr = fresh(t, r, cr)
			setPolicy(t, r, "true")
			author := &api.ChangeAuthor{Name: "Jamie", Email: "jamie@example.com"}
			if raw {
				cr.Spec.Change = &api.Change{Message: "old apply message", Author: author}
				_ = r.Update(context.Background(), cr)
				_ = r.Delete(context.Background(), cr)
			} else {
				cr.Generation++
				cr.Spec.Change = &api.Change{Action: "Delete", Message: "Retire", Author: author}
				_ = r.Update(context.Background(), cr)
			}
			reconcileCR(t, r, cr)
			cr = fresh(t, r, cr)
			if s.Count(t) != 2 || cr.Status.Cleanup != nil || cr.DeletionTimestamp.IsZero() == raw || cr.Status.Approval.Operation != "Delete" {
				t.Fatal("delete escaped gate", cr)
			}
			cr = approve(t, r, cr)
			cr = setApprover(t, r, cr, approvalRequest(cr), "Cleanup Reviewer", "cleanup@example.com")
			reconcileCR(t, r, cr)
			if !raw {
				cr = fresh(t, r, cr)
				if cr.DeletionTimestamp.IsZero() {
					t.Fatal("prepared delete did not issue DELETE")
				}
				reconcileCR(t, r, cr)
			}
			cr = fresh(t, r, cr)
			if cr.Status.Cleanup.Revision == "" || len(s.Read(t, cr.Spec.Repository.Path)) != 0 {
				t.Fatal("Git cleanup missing")
			}
			if cr.Status.Cleanup.Author.Name != map[bool]string{false: "Jamie", true: "git-state-controller"}[raw] {
				t.Fatal("raw delete reused apply author")
			}
			if !raw && cr.Spec.Change.Action != "Delete" {
				t.Fatal("deletion reverted to Apply")
			}
			checkpoint := cr.Status.Cleanup.DeepCopy()
			if !strings.Contains(checkpoint.Message, "Approved-by: Cleanup Reviewer <cleanup@example.com>\n") {
				t.Fatal("cleanup approval not frozen", checkpoint.Message)
			}
			cr = setApprover(t, r, cr, checkpoint.Request, "Later Reviewer", "later@example.com")
			op, _, resolveErr := r.resolve(context.Background(), cr)
			if resolveErr != nil || op.Message != checkpoint.Message {
				t.Fatal("accepted cleanup attribution changed", resolveErr)
			}
			cr.Spec.DeletionPolicy = "Orphan"
			cr.Spec.Change = &api.Change{Action: "Delete", Message: "changed", Author: &api.ChangeAuthor{Name: "Other", Email: "other@example.com"}}
			cr.Generation++
			_ = r.Update(context.Background(), cr)
			setPolicy(t, r, "yes")
			reconcileCR(t, r, cr)
			cr = fresh(t, r, cr)
			if cr.Status.Cleanup.Request != checkpoint.Request || cr.Status.Cleanup.Author.Name != checkpoint.Author.Name || cr.Status.Cleanup.Policy != "Delete" {
				t.Fatal("accepted cleanup replanned")
			}
			handoff(t, a)
			reconcileCR(t, r, cr)
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), &api.GitResource{}); !apierrors.IsNotFound(err) {
				t.Fatal("cleanup did not finish", err)
			}
		})
	}
}
func TestDeletionWaitsForExactTargetAndRejectsReplacement(t *testing.T) {
	_, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	reconcileCR(t, r, cr)
	handoff(t, a)
	cr = fresh(t, r, cr)
	target := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "demo", UID: "original-target", Finalizers: []string{"testing/hold"}}}
	_ = r.Create(context.Background(), target)
	_ = r.Delete(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	handoff(t, a)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if meta.FindStatusCondition(cr.Status.Conditions, "Ready").Reason != "WaitingForResourceDeletion" || cr.Status.Cleanup.ResourceRef.UID != "original-target" {
		t.Fatal("target wait not visible", cr.Status)
	}
	base := r.Reader
	r.Reader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "ConfigMap" {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, errors.New("blocked"))
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	reconcileCR(t, r, cr)
	if fresh(t, r, cr).Status.Cleanup.Revision == "" {
		t.Fatal("read failure lost checkpoint")
	}
	r.Reader = base
	current := &corev1.ConfigMap{}
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(target), current)
	current.Finalizers = nil
	_ = r.Update(context.Background(), current)
	_ = r.Delete(context.Background(), current)
	replacement := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "demo", UID: "replacement"}}
	_ = r.Create(context.Background(), replacement)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if meta.FindStatusCondition(cr.Status.Conditions, "Ready").Reason != "IdentityConflict" {
		t.Fatal("replacement treated as old object")
	}
	_ = r.Delete(context.Background(), replacement)
	reconcileCR(t, r, cr)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), &api.GitResource{}); !apierrors.IsNotFound(err) {
		t.Fatal("authoritative absence did not release finalizer", err)
	}
}

func TestApprovalGatesOrphanAndAdoption(t *testing.T) {
	s, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	reconcileCR(t, r, cr)
	handoff(t, a)
	cr = fresh(t, r, cr)
	setPolicy(t, r, "true")
	cr.Spec.DeletionPolicy = "Orphan"
	cr.Generation++
	_ = r.Update(context.Background(), cr)
	_ = r.Delete(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.Approval.Operation != "Orphan" || s.Count(t) != 2 || cr.Status.Cleanup != nil {
		t.Fatal("orphan marker escaped approval")
	}
	cr = approve(t, r, cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	handoff(t, a)
	reconcileCR(t, r, cr)
	if s.Count(t) != 3 {
		t.Fatal("orphan publication missing")
	}
	replacement := cr.DeepCopy()
	replacement.UID = "new-owner"
	replacement.Generation = 1
	replacement.ResourceVersion = ""
	replacement.DeletionTimestamp = nil
	replacement.Finalizers = nil
	replacement.Status = api.GitResourceStatus{}
	replacement.Annotations = map[string]string{AdoptAnnotation: "true"}
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, replacement)
	replacement = fresh(t, r, replacement)
	if replacement.Status.Approval.Operation != "Adopt" || s.Count(t) != 3 {
		t.Fatal("adoption escaped approval")
	}
	replacement = approve(t, r, replacement)
	reconcileCR(t, r, replacement)
	handoff(t, a)
	reconcileCR(t, r, replacement)
	replacement = fresh(t, r, replacement)
	if adopting(replacement) || replacement.Annotations[ApprovedRequestAnnotation] != "" || s.Count(t) != 4 {
		t.Fatal("adoption did not finish once")
	}
}

func TestConcurrentAuthorsStayScopedToTheirPublication(t *testing.T) {
	server, r, template := fixture(t)
	authors := []string{"Alex", "Jamie"}
	failures := make(chan error, 2)
	for _, name := range authors {
		cr := template.DeepCopy()
		cr.Name = strings.ToLower(name)
		cr.UID = types.UID(name)
		cr.ResourceVersion = ""
		cr.Spec.Repository.Path = cr.Name + ".yaml"
		cr.Spec.Change = &api.Change{Author: &api.ChangeAuthor{Name: name, Email: cr.Name + "@example.com"}}
		if err := r.Create(context.Background(), cr); err != nil {
			t.Fatal(err)
		}
		go func() {
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
			failures <- err
		}()
	}
	for range authors {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	repo, err := gogit.PlainOpen(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range authors {
		cr := &api.GitResource{}
		if err := r.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: strings.ToLower(name)}, cr); err != nil {
			t.Fatal(err)
		}
		history, err := repo.Log(&gogit.LogOptions{FileName: &cr.Spec.Repository.Path})
		if err != nil {
			t.Fatal(err)
		}
		commit, err := history.Next()
		history.Close()
		if err != nil {
			t.Fatal(err)
		}
		if commit.Author.Name != name || commit.Committer.Name != "git-state-controller" {
			t.Fatal("concurrent identity leaked", commit)
		}
	}
}

func TestNeverOwnedOrphanDoesNotRequireRetainedHandoff(t *testing.T) {
	server, r, cr := fixture(t)
	op, _, err := r.resolve(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	op.Content, _, err = manifest.RenderManaged(cr.Spec.Manifest.Raw, "demo", "other", "other-uid", "managed")
	if err != nil {
		t.Fatal(err)
	}
	op.OwnerUID = "other-uid"
	op.SourceName = "other"
	if _, err := r.Publisher.Attempt(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	_, _ = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
	cr = fresh(t, r, cr)
	cr.Spec.DeletionPolicy = "Orphan"
	cr.Generation++
	_ = r.Update(context.Background(), cr)
	_ = r.Delete(context.Background(), cr)
	reconcileCR(t, r, cr)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), &api.GitResource{}); !apierrors.IsNotFound(err) {
		t.Fatal("never-owned orphan stranded", err)
	}
	if server.Count(t) != 2 || len(server.Read(t, cr.Spec.Repository.Path)) == 0 {
		t.Fatal("foreign file changed")
	}
}
func TestCancelPreparedDeletionBeforeDeleteWins(t *testing.T) {
	_, r, cr := fixture(t)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	cr.Spec.Change = &api.Change{Action: "Delete"}
	cr.Generation++
	_ = r.Update(context.Background(), cr)
	base := r.Client
	cancelled := false
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if gr, ok := obj.(*api.GitResource); ok && gr.Status.Cleanup != nil && !cancelled {
			cancelled = true
			current := fresh(t, r, cr)
			current.Generation++
			current.Spec.Change = nil
			if err := base.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
		}
		return c.SubResource(sub).Patch(ctx, obj, p, opts...)
	}})
	reconcileCR(t, r, cr)
	r.Client = base
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if !cr.DeletionTimestamp.IsZero() || cr.Status.Cleanup != nil {
		t.Fatal("cancelled request deleted resource")
	}
}

func TestRecoveredPublicationPreservesUnknownApprovalPolicy(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid", true: "unreadable"}[unavailable], func(t *testing.T) {
			server, r, cr := fixture(t)
			reconcileCR(t, r, cr)
			cr = fresh(t, r, cr)
			sha := cr.Status.LastPublishedRevision
			cr.Generation++
			cr.Spec.Change = &api.Change{Message: "metadata only"}
			if err := r.Update(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
			setPolicy(t, r, "invalid")
			if unavailable {
				r.Reader = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if _, ok := obj.(*corev1.Namespace); ok {
						return apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, key.Name, errors.New("test read failure"))
					}
					return c.Get(ctx, key, obj, opts...)
				}})
			}
			reconcileCR(t, r, cr)
			cr = fresh(t, r, cr)
			approved := meta.FindStatusCondition(cr.Status.Conditions, "Approved")
			ready := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
			if approved.Status != metav1.ConditionUnknown || ready.Status != metav1.ConditionUnknown || cr.Status.Approval.Required != nil || cr.Status.LastPublishedRevision != sha || server.Count(t) != 2 || cr.Spec.Change != nil {
				t.Fatal("recovery hid unknown policy or repeated publication", cr.Status)
			}
		})
	}
}

func TestNoopAttributionValidationAndConsumptionDuringAccessFailure(t *testing.T) {
	server, r, cr := fixture(t)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	sha := cr.Status.LastPublishedRevision
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: r.Namespace, Name: "writer"}
	if err := r.Get(context.Background(), key, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["password"] = []byte("wrong")
	if err := r.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	cr.Generation++
	cr.Spec.Change = &api.Change{Message: "no new bytes", Author: &api.ChangeAuthor{Name: "Alex", Email: "alex@example.com"}}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Spec.Change != nil || cr.Status.LastPublishedRevision != sha || server.Count(t) != 2 || meta.FindStatusCondition(cr.Status.Conditions, "GitDrift").Status != metav1.ConditionUnknown {
		t.Fatal("acknowledged no-op retained metadata or lost drift failure", cr)
	}
	cr.Generation++
	cr.Spec.Change = &api.Change{Author: &api.ChangeAuthor{Name: "invalid\nheader", Email: "alex@example.com"}}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err == nil {
		t.Fatal("invalid identity accepted as a no-op")
	}
	cr = fresh(t, r, cr)
	if cr.Spec.Change == nil || meta.FindStatusCondition(cr.Status.Conditions, "Published").Reason != "InvalidSpec" || cr.Status.LastPublishedRevision != sha || server.Count(t) != 2 {
		t.Fatal("invalid no-op consumed or lost last publication", cr)
	}
}

func TestConsumptionPreservesApprovalAddedAfterObservation(t *testing.T) {
	_, r, cr := fixture(t)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	cr.Spec.Change = &api.Change{Message: "no-op"}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	processed := fresh(t, r, cr)
	current := processed.DeepCopy()
	current.Annotations = map[string]string{ApprovedRequestAnnotation: approvalRequest(processed), "unrelated": "keep"}
	if err := r.Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	if err := r.consumeChange(context.Background(), processed, approvalRequest(processed), false); err != nil {
		t.Fatal(err)
	}
	current = fresh(t, r, cr)
	if current.Spec.Change != nil || current.Annotations[ApprovedRequestAnnotation] != approvalRequest(processed) || current.Annotations["unrelated"] != "keep" {
		t.Fatal("consumption removed newly added evidence", current)
	}
}

func TestConsumptionPreservesChangedApprover(t *testing.T) {
	_, r, cr := fixture(t)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	cr.Spec.Change = &api.Change{Message: "no-op"}
	cr.Annotations = map[string]string{ApprovedRequestAnnotation: approvalRequest(cr)}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	processed := setApprover(t, r, cr, approvalRequest(cr), "Original Reviewer", "old@example.com")
	setApprover(t, r, cr, approvalRequest(cr), "New Reviewer", "new@example.com")
	if err := r.consumeChange(context.Background(), processed, approvalRequest(processed), false); err != nil {
		t.Fatal(err)
	}
	current := fresh(t, r, cr)
	who, err := approvalAttributor(current, approvalRequest(processed))
	if err != nil || who == nil || who.Name != "New Reviewer" || current.Spec.Change != nil || current.Annotations[ApprovedRequestAnnotation] != approvalRequest(processed) {
		t.Fatal("consumption removed changed approver evidence", current)
	}
}

func TestPendingRevertCannotRecoverASupersededPublication(t *testing.T) {
	server, r, cr := fixture(t)
	original := append([]byte(nil), cr.Spec.Manifest.Raw...)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	cr.Generation++
	cr.Spec.Manifest.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"greeting":"new"}}`)
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	sha := cr.Status.LastPublishedRevision
	setPolicy(t, r, "true")
	cr.Generation++
	cr.Spec.Manifest.Raw = original
	cr.Spec.Change = &api.Change{Message: "revert requires review"}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.LastPublishedRevision != sha || cr.Spec.Change == nil || meta.FindStatusCondition(cr.Status.Conditions, "Approved").Status != metav1.ConditionFalse || server.Count(t) != 3 {
		t.Fatal("superseded commit bypassed approval", cr.Status)
	}
}

func TestNoopRecoveryPreservesVerifiedPinIncludingUnrelatedCommit(t *testing.T) {
	server, r, cr := fixture(t)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	other := cr.DeepCopy()
	other.Name, other.UID, other.Spec.Repository.Path = "other", "other-uid", "other.yaml"
	op, _, err := r.resolve(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Publisher.Attempt(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	// A verified pin can include unrelated files pushed before remote verification.
	cr.Status.LastPublishedRevision = result.Revision
	if err := r.Status().Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	cr = fresh(t, r, cr)
	cr.Generation++
	cr.Spec.Change = &api.Change{Message: "no-op"}
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	setPolicy(t, r, "invalid")
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.LastPublishedRevision != result.Revision || cr.Spec.Change != nil || server.Count(t) != 3 {
		t.Fatal("no-op recovery changed verified pin", cr.Status)
	}
}

func setApprover(t *testing.T, r *GitResourceReconciler, cr *api.GitResource, request, name, email string) *api.GitResource {
	t.Helper()
	cr = fresh(t, r, cr)
	data, err := json.Marshal(approvalAttribution{Request: request, Name: name, Email: email})
	if err != nil {
		t.Fatal(err)
	}
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[ApprovedByAnnotation] = string(data)
	if err := r.Update(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	return cr
}

func TestApprovalCommitFooters(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		required, named, stale bool
	}{
		{"named", true, true, false}, {"anonymous", true, false, false}, {"stale-identity", true, true, true}, {"automatic", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, r, cr := fixture(t)
			if tc.required {
				setPolicy(t, r, "true")
				reconcileCR(t, r, cr)
				cr = approve(t, r, cr)
			}
			request := approvalRequest(cr)
			if tc.named {
				identityRequest := request
				if tc.stale {
					identityRequest = "v1:older:1:Apply"
				}
				cr = setApprover(t, r, cr, identityRequest, "Jamie Reviewer", "reviewer@example.com")
			}
			reconcileCR(t, r, cr)
			repo, err := gogit.PlainOpen(server.Root)
			if err != nil {
				t.Fatal(err)
			}
			head, _ := repo.Head()
			commit, err := repo.CommitObject(head.Hash())
			if err != nil {
				t.Fatal(err)
			}
			if tc.required && !strings.Contains(commit.Message, "GitResource-Approval: Approved\nGitResource-Approval-Request: "+request+"\n") {
				t.Fatal("missing request-bound approval footer", commit.Message)
			}
			if !tc.required && !strings.Contains(commit.Message, "GitResource-Approval: NotRequired\n") {
				t.Fatal("automatic commit claims approval", commit.Message)
			}
			if strings.Contains(commit.Message, "Approved-by: Jamie Reviewer <reviewer@example.com>\n") != (tc.required && tc.named && !tc.stale) {
				t.Fatal("approver attribution leaked or is missing", commit.Message)
			}
			current := fresh(t, r, cr)
			if tc.required && tc.named && !tc.stale && current.Annotations[ApprovedByAnnotation] != "" {
				t.Fatal("completed approver was not consumed")
			}
			if tc.stale && current.Annotations[ApprovedByAnnotation] == "" {
				t.Fatal("unrelated attribution removed")
			}
			if commit.Author.Name != "git-state-controller" || commit.Committer.Name != "git-state-controller" {
				t.Fatal("approver replaced Git identity")
			}
			reconcileCR(t, r, current)
			if server.Count(t) != 2 {
				t.Fatal("attribution housekeeping recommitted")
			}
		})
	}
}

func TestInvalidApproverBlocksNewWrites(t *testing.T) {
	for _, raw := range []string{`not-json`, `{"request":"v1:original-uid:1:Apply","name":"Jamie"}`, `{"request":"v1:original-uid:1:Apply","name":"Jamie\nInjected","email":"reviewer@example.com"}`, `{"request":"v1:original-uid:1:Apply","name":"Jamie\u0000Injected","email":"reviewer@example.com"}`} {
		t.Run(raw, func(t *testing.T) {
			server, r, cr := fixture(t)
			setPolicy(t, r, "true")
			reconcileCR(t, r, cr)
			cr = approve(t, r, cr)
			cr.Annotations[ApprovedByAnnotation] = raw
			if err := r.Update(context.Background(), cr); err != nil {
				t.Fatal(err)
			}
			reconcileCR(t, r, cr)
			current := fresh(t, r, cr)
			if server.Count(t) != 1 || meta.FindStatusCondition(current.Status.Conditions, "Approved").Reason != "InvalidApprover" {
				t.Fatal("invalid approver was accepted", current.Status)
			}
		})
	}
}

func TestApproverChangeBeforePushRestagesFooter(t *testing.T) {
	server, r, cr := fixture(t)
	setPolicy(t, r, "true")
	reconcileCR(t, r, cr)
	cr = approve(t, r, cr)
	cr = setApprover(t, r, cr, approvalRequest(cr), "Original Reviewer", "old@example.com")
	changed := false
	r.Publisher.BeforePush = func() {
		if !changed {
			changed = true
			setApprover(t, r, cr, approvalRequest(cr), "New Reviewer", "new@example.com")
		}
	}
	reconcileCR(t, r, cr)
	repo, err := gogit.PlainOpen(server.Root)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Head()
	commit, _ := repo.CommitObject(head.Hash())
	if server.Count(t) != 2 || !strings.Contains(commit.Message, "Approved-by: New Reviewer <new@example.com>\n") || strings.Contains(commit.Message, "Original Reviewer") {
		t.Fatal("stale approver reached Git", commit.Message)
	}
	old := cr.DeepCopy()
	next := cr.DeepCopy()
	next.Annotations = map[string]string{ApprovedByAnnotation: "changed"}
	if !publisherPredicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: next}) {
		t.Fatal("approver edit did not wake publisher")
	}
}
