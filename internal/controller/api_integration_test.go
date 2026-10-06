//go:build integration

package controller

import (
	"context"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
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
}
