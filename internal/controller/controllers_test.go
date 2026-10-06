package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	writer "github.com/inelson/git-state-controller/internal/git"
	"github.com/inelson/git-state-controller/internal/manifest"
	"github.com/inelson/git-state-controller/internal/testgit"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func fixture(t *testing.T) (*testgit.Server, *GitResourceReconciler, *api.GitResource) {
	s := testgit.New(t)
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	cr := &api.GitResource{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "demo", UID: "original-uid", Generation: 1}, Spec: api.GitResourceSpec{Repository: api.Repository{URL: s.URL, Branch: "main", Path: "demo/example.yaml"}, Manifest: runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"greeting":"hello"}}`)}}}
	config := &api.ClusterGitConfig{ObjectMeta: metav1.ObjectMeta{Name: "default"}, Spec: api.ClusterGitConfigSpec{Credentials: api.Credentials{Source: "Secret", SecretRef: api.SecretReference{Namespace: "git-state-system", Name: "writer"}}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "git-state-system", Name: "writer"}, Data: map[string][]byte{"username": []byte("bot"), "password": []byte("password")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.GitResource{}).WithObjects(cr, config, secret).Build()
	return s, &GitResourceReconciler{Client: c, Reader: c, Publisher: &writer.Publisher{}, Namespace: "git-state-system", AllowHTTP: true}, cr
}
func reconcileCR(t *testing.T, r *GitResourceReconciler, cr *api.GitResource) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err != nil {
		t.Fatal(err)
	}
}
func TestGenerationAndStatusConflict(t *testing.T) {
	s, r, cr := fixture(t)
	key := client.ObjectKeyFromObject(cr)
	base := r.Client
	conflicts := 0
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
		if sub == "status" && conflicts == 0 {
			conflicts++
			return apierrors.NewConflict(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "gitresources"}, cr.Name, errors.New("test conflict"))
		}
		return c.SubResource(sub).Update(ctx, obj, opts...)
	}})
	r.Publisher.BeforePush = func() {
		current := &api.GitResource{}
		_ = base.Get(context.Background(), key, current)
		current.Generation = 2
		current.Spec.Manifest.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"greeting":"new"}}`)
		if err := base.Update(context.Background(), current); err != nil {
			t.Error(err)
		}
	}
	reconcileCR(t, r, cr)
	current := &api.GitResource{}
	_ = base.Get(context.Background(), key, current)
	if current.Status.LastPublishedGeneration != 1 || current.Generation != 2 {
		t.Fatal("older bytes mislabeled", current)
	}
	if conflicts != 1 || s.Count(t) != 2 {
		t.Fatal("status conflict repeated Git", conflicts, s.Count(t))
	}
	r.Publisher.BeforePush = nil
	reconcileCR(t, r, cr)
	_ = base.Get(context.Background(), key, current)
	if current.Status.LastPublishedGeneration != 2 {
		t.Fatal(current.Status)
	}
	count := s.Count(t)
	reconcileCR(t, r, cr)
	if s.Count(t) != count {
		t.Fatal("duplicate metadata/no-op commit")
	}
	current.Spec.Change.Message = "new message"
	current.Generation = 3
	_ = base.Update(context.Background(), current)
	reconcileCR(t, r, cr)
	_ = base.Get(context.Background(), key, current)
	if current.Status.LastPublishedGeneration != 3 || s.Count(t) != count {
		t.Fatal("message-only commit")
	}
}
func TestFailedPublicationRetainsSuccessAndRecovery(t *testing.T) {
	_, r, cr := fixture(t)
	reconcileCR(t, r, cr)
	current := &api.GitResource{}
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(cr), current)
	sha := current.Status.LastPublishedRevision
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: "writer"}
	_ = r.Get(context.Background(), key, secret)
	secret.Data["password"] = []byte("wrong")
	_ = r.Update(context.Background(), secret)
	current.Generation = 2
	current.Spec.Manifest.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"changed":"yes"}}`)
	_ = r.Update(context.Background(), current)
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err == nil {
		t.Fatal("false success")
	}
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(cr), current)
	if current.Status.LastPublishedRevision != sha || current.Status.LastPublishedGeneration != 1 || meta.IsStatusConditionTrue(current.Status.Conditions, "Published") {
		t.Fatal(current.Status)
	}
	secret.Data["password"] = []byte("password")
	_ = r.Update(context.Background(), secret)
	reconcileCR(t, r, cr)
	if err := r.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, cr)
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(cr), current); !apierrors.IsNotFound(err) {
		t.Fatal("finalizer retained", err)
	}
}

func TestGitDeadlineStillRecordsFailure(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprint("deleting=", deleting), func(t *testing.T) {
			_, r, cr := fixture(t)
			reconcileCR(t, r, cr)
			current := &api.GitResource{}
			key := client.ObjectKeyFromObject(cr)
			_ = r.Get(context.Background(), key, current)
			sha := current.Status.LastPublishedRevision
			if deleting {
				if err := r.Delete(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			} else {
				current.Generation++
				current.Spec.Manifest.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"changed":"yes"}}`)
				if err := r.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			}
			r.OperationTimeout = 200 * time.Millisecond
			r.Publisher.Push = func(ctx context.Context, _ *gogit.Repository, _ *gogit.PushOptions) error {
				<-ctx.Done()
				return ctx.Err()
			}
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}})
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err == nil {
				t.Fatal("timeout reported success")
			}
			_ = r.Get(context.Background(), key, current)
			condition := meta.FindStatusCondition(current.Status.Conditions, "Published")
			reason := "PublishFailed"
			if deleting {
				reason = "DeleteFailed"
			}
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != reason || current.Status.LastPublishedRevision != sha {
				t.Fatalf("timeout lost failure or prior publication: %+v", current.Status)
			}
			if deleting && len(current.Finalizers) == 0 {
				t.Fatal("timeout released finalizer")
			}
		})
	}
}
func TestStaleUIDCannotWriteStatus(t *testing.T) {
	_, r, cr := fixture(t)
	copy := cr.DeepCopy()
	copy.UID = "old"
	if err := r.setStatus(context.Background(), copy, true, "Pushed", "test", writer.Result{Revision: "abc"}, "hash"); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
	if cr.Status.LastPublishedRevision != "" {
		t.Fatal("stale UID wrote status")
	}
}
func TestInventoryPreservesOtherFieldsAndFailure(t *testing.T) {
	_, r, cr := fixture(t)
	reconcileCR(t, r, cr)
	set := ApplicationSetObject()
	set.SetName("git-resources")
	set.SetNamespace("argocd")
	set.Object["spec"] = map[string]interface{}{"generators": []interface{}{map[string]interface{}{"list": map[string]interface{}{"elements": []interface{}{}, "template": map[string]interface{}{"metadata": map[string]interface{}{"labels": map[string]interface{}{"keep": "yes"}}}}}}, "template": map[string]interface{}{"keep": "yes"}}
	if err := r.Create(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	a := &ApplicationSetReconciler{Client: r.Client, Reader: r.Reader, Key: client.ObjectKeyFromObject(set)}
	if _, err := a.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	got := ApplicationSetObject()
	_ = r.Get(context.Background(), a.Key, got)
	b, _ := json.Marshal(got.Object)
	if !json.Valid(b) {
		t.Fatal("invalid inventory")
	}
	failing := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
		return errors.New("inventory unavailable")
	}})
	a.Reader = failing
	if _, err := a.Reconcile(context.Background(), ctrl.Request{}); err == nil {
		t.Fatal("inventory failure swallowed")
	}
	after := ApplicationSetObject()
	_ = r.Get(context.Background(), a.Key, after)
	before, _ := json.Marshal(got.Object)
	now, _ := json.Marshal(after.Object)
	if string(before) != string(now) {
		t.Fatal("failure cleared inventory")
	}
	a.Reader = r.Reader
	current := &api.GitResource{}
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(cr), current)
	_ = r.Delete(context.Background(), current)
	reconcileCR(t, r, cr)
	if _, err := a.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
}

func TestFailedStatusPersistenceRecoversWithoutCommit(t *testing.T) {
	s, r, cr := fixture(t)
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
		return errors.New("status unavailable")
	}})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err == nil {
		t.Fatal("status failure swallowed")
	}
	if s.Count(t) != 2 {
		t.Fatal("push did not complete")
	}
	r.Client = base
	reconcileCR(t, r, cr)
	if s.Count(t) != 2 {
		t.Fatal("status recovery duplicated content commit")
	}
	got := &api.GitResource{}
	_ = base.Get(context.Background(), client.ObjectKeyFromObject(cr), got)
	if got.Status.LastPublishedGeneration != cr.Generation {
		t.Fatal("publication recovery missing", got.Status)
	}
}

func TestRetryRefreshesDesiredGeneration(t *testing.T) {
	s, r, cr := fixture(t)
	first := true
	r.Publisher.BeforePush = func() {
		if !first {
			return
		}
		first = false
		other := writer.Operation{URL: s.URL, Branch: "main", Path: "other.yaml", Content: []byte("kind: ConfigMap\n"), Credentials: writer.Credentials{Username: "bot", Password: "password"}, AuthorName: "test", AuthorEmail: "test@example.invalid", Message: "advance branch"}
		if _, err := (&writer.Publisher{}).Attempt(context.Background(), other); err != nil {
			t.Error(err)
		}
		latest := &api.GitResource{}
		_ = r.Get(context.Background(), client.ObjectKeyFromObject(cr), latest)
		latest.Generation = 2
		latest.Spec.Manifest.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example"},"data":{"greeting":"newest"}}`)
		if err := r.Update(context.Background(), latest); err != nil {
			t.Error(err)
		}
	}
	reconcileCR(t, r, cr)
	got := &api.GitResource{}
	_ = r.Get(context.Background(), client.ObjectKeyFromObject(cr), got)
	if got.Status.LastPublishedGeneration != 2 {
		t.Fatal("retried obsolete generation", got.Status)
	}
	if !bytes.Contains(s.Read(t, cr.Spec.Repository.Path), []byte("newest")) {
		t.Fatal("newest bytes not published")
	}
	if s.Read(t, "other.yaml") == nil {
		t.Fatal("retry lost competing file")
	}
}

func TestInventoryConflictRecomputesPublications(t *testing.T) {
	_, r, cr := fixture(t)
	reconcileCR(t, r, cr)
	set := ApplicationSetObject()
	set.SetName("git-resources")
	set.SetNamespace("argocd")
	set.Object["spec"] = map[string]interface{}{"generators": []interface{}{map[string]interface{}{"list": map[string]interface{}{"elements": []interface{}{}}}}}
	if err := r.Create(context.Background(), set); err != nil {
		t.Fatal(err)
	}
	base := r.Client
	conflict := true
	patched := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if conflict {
			conflict = false
			fresh := cr.DeepCopy()
			fresh.Name = "additional"
			fresh.UID = "additional-uid"
			fresh.ResourceVersion = ""
			fresh.Status = api.GitResourceStatus{LastPublishedGeneration: 1, LastPublishedRevision: "0123456789012345678901234567890123456789"}
			if err := c.Create(ctx, fresh); err != nil {
				return err
			}
			return apierrors.NewConflict(schema.GroupResource{Group: "argoproj.io", Resource: "applicationsets"}, set.GetName(), errors.New("inventory race"))
		}
		return c.Patch(ctx, obj, patch, opts...)
	}})
	inventory := &ApplicationSetReconciler{Client: patched, Reader: base, Key: client.ObjectKeyFromObject(set)}
	if _, err := inventory.Reconcile(context.Background(), ctrl.Request{}); err != nil {
		t.Fatal(err)
	}
	got := ApplicationSetObject()
	if err := base.Get(context.Background(), inventory.Key, got); err != nil {
		t.Fatal(err)
	}
	generators, _, err := unstructured.NestedSlice(got.Object, "spec", "generators")
	if err != nil {
		t.Fatal(err)
	}
	elements := generators[0].(map[string]interface{})["list"].(map[string]interface{})["elements"].([]interface{})
	if len(elements) != 2 {
		t.Fatal("conflict reused stale inventory", elements)
	}
}

func TestCredentialNamespaceAndDependencyMapping(t *testing.T) {
	_, r, cr := fixture(t)
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: r.Namespace, Name: "writer"}, secret); err != nil {
		t.Fatal(err)
	}
	requests := r.affected(context.Background(), secret)
	if len(requests) != 1 || requests[0].NamespacedName != client.ObjectKeyFromObject(cr) {
		t.Fatal("Secret change omitted default reference", requests)
	}
	config := &api.ClusterGitConfig{}
	if err := r.Get(context.Background(), types.NamespacedName{Name: "default"}, config); err != nil {
		t.Fatal(err)
	}
	config.Spec.Credentials.SecretRef.Namespace = "other-namespace"
	if err := r.Update(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if _, reason, err := r.resolve(context.Background(), cr); err == nil || reason != "CredentialsInvalid" {
		t.Fatal("cross-namespace credentials accepted", reason, err)
	}
}

func TestTwentyConcurrentResourcesWithFourWorkers(t *testing.T) {
	s, r, base := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resources := make([]*api.GitResource, 20)
	for i := range resources {
		cr := base.DeepCopy()
		cr.Name = fmt.Sprintf("concurrent-%02d", i)
		cr.UID = types.UID(cr.Name)
		cr.ResourceVersion = ""
		cr.Spec.Repository.Path = fmt.Sprintf("many/%s.yaml", cr.Name)
		if err := r.Create(ctx, cr); err != nil {
			t.Fatal(err)
		}
		resources[i] = cr
	}
	jobs := make(chan *api.GitResource, len(resources))
	for _, cr := range resources {
		jobs <- cr
	}
	close(jobs)
	failures := make(chan error, len(resources))
	var workers sync.WaitGroup
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for cr := range jobs {
				req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}
				for {
					_, err := r.Reconcile(ctx, req)
					if err == nil {
						break
					}
					if ctx.Err() != nil {
						failures <- fmt.Errorf("%s: %w", cr.Name, err)
						break
					}
					timer := time.NewTimer(50 * time.Millisecond)
					select {
					case <-ctx.Done():
						timer.Stop()
					case <-timer.C:
					}
				}
			}
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	for _, cr := range resources {
		got := &api.GitResource{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(cr), got); err != nil {
			t.Fatal(err)
		}
		if got.Status.LastPublishedGeneration != 1 || len(got.Status.LastPublishedRevision) != 40 {
			t.Fatal("missing publication", cr.Name, got.Status)
		}
		want, _, err := manifest.Render(cr.Spec.Manifest.Raw)
		if err != nil {
			t.Fatal(err)
		}
		s.Assert(t, cr.Spec.Repository.Path, want)
	}
	s.Assert(t, "README.md", []byte("unrelated seed\n"))
}
