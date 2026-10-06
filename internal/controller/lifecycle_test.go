package controller

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	writer "github.com/inelson/git-state-controller/internal/git"
	"github.com/inelson/git-state-controller/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func inventoryFixture(t *testing.T, r *GitResourceReconciler) *ApplicationSetReconciler {
	t.Helper()
	set := ApplicationSetObject()
	set.SetName("git-resources")
	set.SetNamespace("argocd")
	set.SetUID("set-uid")
	set.Object["spec"] = map[string]interface{}{"generators": []interface{}{map[string]interface{}{"list": map[string]interface{}{"elements": []interface{}{}}}}}
	if err := r.Create(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	return &ApplicationSetReconciler{Client: r.Client, Reader: r.Reader, Key: types.NamespacedName{Namespace: "argocd", Name: "git-resources"}}
}
func handoff(t *testing.T, a *ApplicationSetReconciler) {
	t.Helper()
	if _, err := a.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
}
func fresh(t *testing.T, r *GitResourceReconciler, cr *api.GitResource) *api.GitResource {
	t.Helper()
	out := &api.GitResource{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), out); err != nil {
		t.Fatal(err)
	}
	return out
}
func external(t *testing.T, r *GitResourceReconciler, cr *api.GitResource, content []byte, remove bool) {
	t.Helper()
	op, _, err := r.resolve(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	op.Content = content
	op.Delete = remove
	op.OwnerUID = ""
	op.PreviousRevision = ""
	op.Message = "External change"
	op.ReadOnly = false
	op.Orphan = false
	if _, err = r.Publisher.Attempt(context.Background(), op); err != nil {
		t.Fatal(err)
	}
}
func TestPauseDriftAndCollision(t *testing.T) {
	s, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	cr = fresh(t, r, cr)
	cr.Annotations = map[string]string{PausedAnnotation: "true"}
	_ = r.Update(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if len(cr.Finalizers) != 1 || s.Count(t) != 1 {
		t.Fatal("paused initial publication had effects")
	}
	delete(cr.Annotations, PausedAnnotation)
	_ = r.Update(context.Background(), cr)
	reconcileCR(t, r, cr)
	handoff(t, a)
	cr = fresh(t, r, cr)
	sha := cr.Status.LastPublishedRevision
	external(t, r, cr, []byte("external content\n"), false)
	count := s.Count(t)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if s.Count(t) != count || cr.Status.LastPublishedRevision != sha || !meta.IsStatusConditionTrue(cr.Status.Conditions, "GitDrift") {
		t.Fatal("drift repaired or pin advanced")
	}
	cr.Spec.Change = &api.Change{Message: "message only"}
	cr.Generation++
	_ = r.Update(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.LastPublishedGeneration != cr.Generation || s.Count(t) != count || !meta.IsStatusConditionTrue(cr.Status.Conditions, "GitDrift") {
		t.Fatal("message-only update repaired drift")
	}
	cr.Spec.Manifest.Raw = bytes.ReplaceAll(cr.Spec.Manifest.Raw, []byte("hello"), []byte("changed"))
	cr.Generation++
	_ = r.Update(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if s.Count(t) != count+1 || meta.IsStatusConditionTrue(cr.Status.Conditions, "GitDrift") {
		t.Fatal("real desired change did not overwrite drift")
	}
	other := cr.DeepCopy()
	other.Name = "collision"
	other.UID = "different"
	other.ResourceVersion = ""
	other.Finalizers = nil
	other.Status = api.GitResourceStatus{}
	_ = r.Create(context.Background(), other)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(other)}); !errors.Is(err, writer.ErrPathAlreadyExists) {
		t.Fatal("collision allowed", err)
	}
	other = fresh(t, r, other)
	_ = r.Delete(context.Background(), other)
	reconcileCR(t, r, other)
	if len(s.Read(t, cr.Spec.Repository.Path)) == 0 {
		t.Fatal("rejected CR deleted owner's file")
	}
	cr.Annotations = map[string]string{PausedAnnotation: "true"}
	_ = r.Update(context.Background(), cr)
	_ = r.Delete(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.DeletionTimestamp.IsZero() || len(s.Read(t, cr.Spec.Repository.Path)) == 0 {
		t.Fatal("paused deletion mutated Git")
	}
	handoff(t, a)
	delete(cr.Annotations, PausedAnnotation)
	_ = r.Update(context.Background(), cr)
	reconcileCR(t, r, cr)
	if len(s.Read(t, cr.Spec.Repository.Path)) != 0 {
		t.Fatal("resume failed cleanup")
	}
}
func TestOrphanRestartAndAdoption(t *testing.T) {
	s, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	reconcileCR(t, r, cr)
	handoff(t, a)
	cr = fresh(t, r, cr)
	identity := *cr.Status.ApplicationRef
	// A pending unpublished spec and an external branch edit must not become orphan content.
	cr.Spec.Manifest.Raw = bytes.ReplaceAll(cr.Spec.Manifest.Raw, []byte("hello"), []byte("pending"))
	cr.Spec.DeletionPolicy = "Orphan"
	cr.Generation++
	_ = r.Update(context.Background(), cr)
	external(t, r, cr, []byte("external drift"), false)
	_ = r.Delete(context.Background(), cr)
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if gr, ok := obj.(*api.GitResource); ok && gr.Status.Cleanup != nil && gr.Status.Cleanup.Revision != "" {
			return errors.New("checkpoint interrupted")
		}
		return c.Status().Patch(ctx, obj, p, opts...)
	}})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err == nil {
		t.Fatal("checkpoint failure swallowed")
	}
	count := s.Count(t)
	r.Client = base
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.Cleanup.Revision == "" || s.Count(t) != count {
		t.Fatal("orphan recovery duplicated commit")
	}
	content := s.Read(t, cr.Spec.Repository.Path)
	_, _, uid, state := manifest.Ownership(content)
	if uid != string(cr.UID) || state != "orphaned" || !bytes.Contains(content, []byte("hello")) || bytes.Contains(content, []byte("pending")) {
		t.Fatal("wrong orphan content", string(content))
	}
	// Missing ApplicationSet holds the finalizer; restored API handoff is enough without Argo.
	set := ApplicationSetObject()
	_ = r.Get(context.Background(), a.Key, set)
	_ = r.Delete(context.Background(), set)
	reconcileCR(t, r, cr)
	_ = fresh(t, r, cr)
	set.SetResourceVersion("")
	_ = r.Create(context.Background(), set)
	handoff(t, a)
	reconcileCR(t, r, cr)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), &api.GitResource{}); !apierrors.IsNotFound(err) {
		t.Fatal("orphan finalizer not released", err)
	}
	handoff(t, a)
	_ = r.Get(context.Background(), a.Key, set)
	entries, _ := listElements(set)
	if len(entries) != 1 || stringField(entries[0], "managementState") != "orphaned" {
		t.Fatal("missing CR removed retained entry")
	}
	// Stale old publication cannot reset the retained marker or SHA.
	stale := *cr.DeepCopy()
	stale.Status.Cleanup = nil
	desired := retainedElements([]api.GitResource{stale}, []interface{}{entries[0]})
	if stringField(desired[0].(map[string]interface{}), "managementState") != "orphaned" {
		t.Fatal("sticky retention reset")
	}
	replacement := cr.DeepCopy()
	replacement.UID = "new-uid"
	replacement.ResourceVersion = ""
	replacement.DeletionTimestamp = nil
	replacement.Finalizers = nil
	replacement.Generation = 1
	replacement.Status = api.GitResourceStatus{}
	replacement.Annotations = map[string]string{AdoptAnnotation: "true"}
	replacement.Spec.DeletionPolicy = "Delete"
	if err := r.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, replacement)
	replacement = fresh(t, r, replacement)
	if *replacement.Status.ApplicationRef != identity {
		t.Fatal("adoption changed Application identity")
	}
	handoff(t, a)
	reconcileCR(t, r, replacement)
	replacement = fresh(t, r, replacement)
	if adopting(replacement) {
		t.Fatal("adoption annotation not consumed")
	}
	_, _, uid, state = manifest.Ownership(s.Read(t, replacement.Spec.Repository.Path))
	if uid != "new-uid" || state != "managed" {
		t.Fatal("adoption marker wrong")
	}
	before := s.Count(t)
	reconcileCR(t, r, replacement)
	if s.Count(t) != before {
		t.Fatal("adoption retry duplicated commit")
	}
}
func TestAdoptionLiveOwnerBlocked(t *testing.T) {
	s, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	reconcileCR(t, r, cr)
	handoff(t, a)
	other := fresh(t, r, cr)
	other.Name = "other"
	other.UID = "new"
	other.ResourceVersion = ""
	other.Status = api.GitResourceStatus{}
	other.Finalizers = nil
	other.Annotations = map[string]string{AdoptAnnotation: "true"}
	_ = r.Create(context.Background(), other)
	count := s.Count(t)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(other)}); err == nil || s.Count(t) != count {
		t.Fatal("live old owner overwritten", err)
	}
}
func TestLegacyUpgradeContinuity(t *testing.T) {
	s, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	bytesOld, hash, _ := manifest.Render(cr.Spec.Manifest.Raw)
	op, _, _ := r.resolve(context.Background(), cr)
	op.OwnerUID = ""
	op.Content = bytesOld
	result, err := r.Publisher.Attempt(context.Background(), op)
	if err != nil {
		t.Fatal(err)
	}
	cr = fresh(t, r, cr)
	cr.Status.LastPublishedRevision = result.Revision
	cr.Status.LastPublishedContentHash = hash
	cr.Status.LastPublishedGeneration = cr.Generation
	condition(cr, "Published", "True", "Pushed", "")
	_ = r.Status().Update(context.Background(), cr)
	set := ApplicationSetObject()
	_ = r.Get(context.Background(), a.Key, set)
	entry := PublicationElements([]api.GitResource{*cr})[0].(map[string]interface{})
	entry["applicationName"] = "legacy-identity"
	delete(entry, "branch")
	delete(entry, "path")
	delete(entry, "managementState")
	_ = unstructured.SetNestedSlice(set.Object, []interface{}{map[string]interface{}{"list": map[string]interface{}{"elements": []interface{}{entry}}}}, "spec", "generators")
	_ = r.Update(context.Background(), set)
	count := s.Count(t)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.ApplicationRef.Name != "legacy-identity" || s.Count(t) != count {
		t.Fatal("upgrade renamed or incidentally published")
	}
	cr.Generation++
	cr.Spec.Manifest.Raw = bytes.ReplaceAll(cr.Spec.Manifest.Raw, []byte("hello"), []byte("next"))
	_ = r.Update(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	_, _, uid, state := manifest.Ownership(s.Read(t, cr.Spec.Repository.Path))
	if uid != string(cr.UID) || state != "managed" || cr.Status.ApplicationRef.Name != "legacy-identity" {
		t.Fatal("upgrade publication failed continuity")
	}
}

func TestOrphanRecoveryWithoutPublicationStatus(t *testing.T) {
	s, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	op, _, err := r.resolve(context.Background(), cr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Publisher.Attempt(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	// Process died after verified publication, before saving any publication status.
	cr = fresh(t, r, cr)
	cr.Spec.DeletionPolicy = "Orphan"
	cr.Finalizers = []string{Finalizer}
	_ = r.Update(context.Background(), cr)
	_ = r.Delete(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.LastPublishedRevision == "" || cr.Status.PublishedResourceRef == nil || cr.Status.Cleanup.Revision == "" {
		t.Fatal("narrow UID/path recovery missing", cr.Status)
	}
	count := s.Count(t)
	handoff(t, a)
	reconcileCR(t, r, cr)
	if s.Count(t) != count {
		t.Fatal("restart duplicated orphan content")
	}
	handoff(t, a)
}
func TestCredentialsEvidenceRotationAndPushDenial(t *testing.T) {
	_, r, cr := fixture(t)
	ctx := context.Background()
	reconcileCR(t, r, cr)
	config := &api.ClusterGitConfig{}
	_ = r.Get(ctx, types.NamespacedName{Name: "default"}, config)
	if config.Status.LastAccess == nil || config.Status.LastAccess.Operation != "Push" || !config.Status.LastAccess.Success || !meta.IsStatusConditionTrue(config.Status.Conditions, "Ready") {
		t.Fatal("real push not evidenced", config.Status)
	}
	op, _, err := r.resolve(ctx, fresh(t, r, cr))
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: "writer"}
	_ = r.Get(ctx, key, secret)
	oldVersion := secret.ResourceVersion
	secret.Data["password"] = []byte("rotated")
	_ = r.Update(ctx, secret)
	op.Access("Push", true)
	op.FlushAccess()
	_ = r.Get(ctx, types.NamespacedName{Name: "default"}, config)
	if config.Status.LastAccess.SecretResourceVersion != oldVersion {
		t.Fatal("old operation certified rotated credentials")
	}
	secret.Data["password"] = nil
	_ = r.Update(ctx, secret)
	_, _, _, err = r.loadConfig(ctx, "default")
	if err == nil {
		t.Fatal("empty credentials accepted")
	}
	_ = r.Get(ctx, types.NamespacedName{Name: "default"}, config)
	if !meta.IsStatusConditionFalse(config.Status.Conditions, "Ready") {
		t.Fatal("invalid keys reported loaded")
	}
	secret.Data["password"] = []byte("password")
	_ = r.Update(ctx, secret)
	cr = fresh(t, r, cr)
	cr.Generation++
	cr.Spec.Manifest.Raw = bytes.ReplaceAll(cr.Spec.Manifest.Raw, []byte("hello"), []byte("denied"))
	_ = r.Update(ctx, cr)
	r.Publisher.Push = func(context.Context, *gogit.Repository, *gogit.PushOptions) error {
		return transport.ErrAuthorizationFailed
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); !errors.Is(err, writer.ErrCredentialsInvalid) {
		t.Fatal("denied push not distinguished", err)
	}
	_ = r.Get(ctx, types.NamespacedName{Name: "default"}, config)
	if config.Status.LastAccess.Operation != "Push" || config.Status.LastAccess.Success {
		t.Fatal("successful fetch certified push access", config.Status)
	}
	r.Publisher.Push = nil
	reconcileCR(t, r, cr)
	_ = r.Get(ctx, types.NamespacedName{Name: "default"}, config)
	if !config.Status.LastAccess.Success {
		t.Fatal("access recovery not recorded")
	}
}

func TestSimultaneousFirstWritesHaveOneOwner(t *testing.T) {
	s, r, cr := fixture(t)
	other := cr.DeepCopy()
	other.Name = "collision"
	other.UID = "collision-uid"
	other.ResourceVersion = ""
	if err := r.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	barrier := make(chan struct{})
	r.Publisher.BeforePush = func() {
		n := count.Add(1)
		if n == 2 {
			close(barrier)
		}
		if n <= 2 {
			<-barrier
		}
	}
	outcomes := make(chan error, 2)
	for _, obj := range []*api.GitResource{cr, other} {
		go func(obj *api.GitResource) {
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
			outcomes <- err
		}(obj)
	}
	successes, collisions := 0, 0
	for i := 0; i < 2; i++ {
		err := <-outcomes
		if err == nil {
			successes++
		} else if errors.Is(err, writer.ErrPathAlreadyExists) {
			collisions++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || collisions != 1 || s.Count(t) != 2 {
		t.Fatal("first writer ownership race", successes, collisions, s.Count(t))
	}
	r.Publisher.BeforePush = nil
	cr = fresh(t, r, cr)
	other = fresh(t, r, other)
	winner, loser := cr, other
	if cr.Status.LastPublishedRevision == "" {
		winner, loser = other, cr
	}
	_ = r.Delete(context.Background(), loser)
	reconcileCR(t, r, loser)
	_, _, uid, _ := manifest.Ownership(s.Read(t, winner.Spec.Repository.Path))
	if uid != string(winner.UID) {
		t.Fatal("loser deletion changed ownership")
	}
}

func TestSavedOrphanCheckpointCompletesWithoutGitRepair(t *testing.T) {
	s, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	reconcileCR(t, r, cr)
	handoff(t, a)
	cr = fresh(t, r, cr)
	cr.Spec.DeletionPolicy = "Orphan"
	_ = r.Update(context.Background(), cr)
	_ = r.Delete(context.Background(), cr)
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	checkpoint := cr.Status.Cleanup.Revision
	handoff(t, a)
	// A subsequent external branch edit does not replace the verified orphan SHA.
	external(t, r, cr, []byte("external after checkpoint"), false)
	count := s.Count(t)
	s.Server.Close()
	reconcileCR(t, r, cr)
	if s.Count(t) != count {
		t.Fatal("handoff retried Git cleanup")
	}
	set := ApplicationSetObject()
	_ = r.Get(context.Background(), a.Key, set)
	entries, _ := listElements(set)
	if stringField(entries[0], "revision") != checkpoint {
		t.Fatal("durable pin changed")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), &api.GitResource{}); !apierrors.IsNotFound(err) {
		t.Fatal("saved cleanup depended on Git", err)
	}
}

func TestRetainedPathNeedsAdoptionEvenWhenBranchFileMissing(t *testing.T) {
	_, r, cr := fixture(t)
	a := inventoryFixture(t, r)
	reconcileCR(t, r, cr)
	handoff(t, a)
	cr = fresh(t, r, cr)
	cr.Spec.DeletionPolicy = "Orphan"
	_ = r.Update(context.Background(), cr)
	_ = r.Delete(context.Background(), cr)
	reconcileCR(t, r, cr)
	handoff(t, a)
	reconcileCR(t, r, cr)
	external(t, r, cr, nil, true)
	next := cr.DeepCopy()
	next.UID = "new-owner"
	next.ResourceVersion = ""
	next.DeletionTimestamp = nil
	next.Finalizers = nil
	next.Status = api.GitResourceStatus{}
	if err := r.Create(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(next)}); !errors.Is(err, writer.ErrPathAlreadyExists) {
		t.Fatal("retained path created a parallel Application", err)
	}
}
