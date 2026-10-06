//go:build integration

package controller

import (
	"context"
	"fmt"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"os"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"testing"
)

func TestRealStatusRoundTripAndConflict(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("run make test-api")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}); err != nil {
		t.Fatal(err)
	}
	cr := readyFixture()
	cr.UID = ""
	cr.Generation = 0
	cr.Status = api.GitResourceStatus{}
	cr.Spec.Manifest = runtime.RawExtension{Raw: []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"target"}}`)}
	if err := c.Create(ctx, cr); err != nil {
		t.Fatal(err)
	}
	original := cr.DeepCopy()
	snapshot := rawJSON(map[string]interface{}{"future": map[string]interface{}{"nested": []interface{}{map[string]interface{}{"output": "raw value", "number": 9007199254740991}}}, "conditions": []interface{}{map[string]interface{}{"type": "Future", "status": "arbitrary native format", "upstreamExtra": true}}, "operationState": map[string]interface{}{"phase": "Failed", "syncResult": map[string]interface{}{"resources": []interface{}{map[string]interface{}{"message": "per-resource apply failure"}}}}})
	first := true
	wrapped := interceptor.NewClient(c, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, base client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if first {
			first = false
			if err := patchStatus(ctx, base, c, original, "publisher", func(current *api.GitResource) {
				current.Status.LastPublishedRevision = "0123456789012345678901234567890123456789"
				current.Status.LastPublishedGeneration = current.Generation
				current.Status.LastPublishedContentHash = "sha256:0123456789012345678901234567890123456789012345678901234567890123"
				condition(current, "Published", "True", "Pushed", "")
			}); err != nil {
				t.Fatal(err)
			}
		}
		return base.Status().Patch(ctx, obj, p, opts...)
	}})
	if err := patchStatus(ctx, wrapped, c, original, "publisher", func(current *api.GitResource) {
		current.Status.ArgoCD = &api.ApplicationSnapshot{Observation: api.Observation{Reason: "Observed"}, Status: snapshot}
		current.Status.Resource = &api.ResourceSnapshot{Ref: api.ResourceReference{APIVersion: "v1", Kind: "ConfigMap", Name: "target"}, Observation: api.Observation{Reason: "Observed"}, Status: rawJSON(map[string]interface{}{})}
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rawObject(cr.Status.ArgoCD.Status), rawObject(snapshot)) || cr.Status.Resource.Status == nil || !meta.IsStatusConditionTrue(cr.Status.Conditions, "Published") {
		t.Fatal("unknown JSON or concurrent publication lost", cr.Status)
	}
	if publisherPredicate.Update(event.UpdateEvent{ObjectOld: original, ObjectNew: cr}) {
		t.Fatal("real status-only update enqueued publisher")
	}
	version := cr.ResourceVersion
	if err := patchStatus(ctx, c, c, cr, "publisher", func(*api.GitResource) {}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(cr), cr)
	if version != cr.ResourceVersion {
		t.Fatal("real semantic no-op patched")
	}
	if err := patchStatus(ctx, c, c, cr, "publisher", func(current *api.GitResource) {
		current.Status.ArgoCD.Status = rawJSON(map[string]interface{}{"replacement": "only"})
		current.Status.Resource.Status = nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(cr), cr)
	if nested(rawObject(cr.Status.ArgoCD.Status), "future") != nil || cr.Status.Resource.Status != nil {
		t.Fatal("subtree replacement merged removed keys")
	}
	t.Run("approved-consumption-generation-and-validation", func(t *testing.T) { testRealConsumption(t, c) })
}

func testRealConsumption(t *testing.T, c client.WithWatch) {
	ctx := context.Background()
	server, r, cr := fixture(t)
	config := &api.ClusterGitConfig{}
	if err := r.Get(ctx, client.ObjectKey{Name: "default"}, config); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: "writer"}, secret); err != nil {
		t.Fatal(err)
	}
	config.ResourceVersion = ""
	secret.ResourceVersion = ""
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.Namespace}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, config); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	r.Client = c
	r.Reader = c
	setPolicy(t, r, "true")
	cr.UID = ""
	cr.Generation = 0
	cr.ResourceVersion = ""
	cr.Name = "approved-consumption"
	cr.Spec.Change = &api.Change{Message: "One time", Author: &api.ChangeAuthor{Name: "Alex", Email: "alex@example.com"}}
	if err := c.Create(ctx, cr); err != nil {
		t.Fatal(err)
	}
	reconcileCR(t, r, cr)
	cr = approve(t, r, cr)
	generation := cr.Generation
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Spec.Change != nil || cr.Generation != generation+1 || cr.Status.LastPublishedGeneration != generation || cr.Annotations[ApprovedRequestAnnotation] != "" {
		t.Fatal("real consumption did not advance generation safely", cr)
	}
	reconcileCR(t, r, cr)
	cr = fresh(t, r, cr)
	if cr.Status.LastPublishedGeneration != cr.Generation || meta.FindStatusCondition(cr.Status.Conditions, "Approved").Reason != "NoChanges" || server.Count(t) != 2 {
		t.Fatal("housekeeping recommitted or sought approval", cr.Status)
	}
	for i, author := range []*api.ChangeAuthor{{Name: "Partial"}, {Name: "bad\nheader", Email: "user@example.com"}, {Name: "Name", Email: "invalid"}, {Name: "   ", Email: "user@example.com"}} {
		invalid := cr.DeepCopy()
		invalid.Name = fmt.Sprintf("invalid-author-%d", i)
		invalid.ResourceVersion = ""
		invalid.UID = ""
		invalid.Status = api.GitResourceStatus{}
		invalid.Spec.Change = &api.Change{Author: author}
		if err := c.Create(ctx, invalid); !apierrors.IsInvalid(err) {
			t.Fatalf("invalid author accepted: %+v %v", author, err)
		}
	}
	plain := cr.DeepCopy()
	plain.Name = "omitted-change"
	plain.UID = ""
	plain.ResourceVersion = ""
	plain.Spec.Change = nil
	plain.Status = api.GitResourceStatus{}
	if err := c.Create(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(plain), plain); err != nil {
		t.Fatal(err)
	}
	if plain.Spec.Change != nil {
		t.Fatal("omitted change was defaulted")
	}
}
