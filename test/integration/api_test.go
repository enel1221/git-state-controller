//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestAPIContracts(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("envtest assets required: run make test-api")
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
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "test"}}); err != nil {
		t.Fatal(err)
	}
	cr := &api.GitResource{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "test"}, Spec: api.GitResourceSpec{Repository: api.Repository{URL: "https://example.invalid/repo.git", Branch: "main", Path: "one.yaml"}, Manifest: runtime.RawExtension{Raw: []byte(`{"apiVersion":"future.example/v1","kind":"Unknown","metadata":{"name":"object"},"spec":{"unknown":{"nested":[{"key":"value"}]}}}`)}}}
	if err = c.Create(ctx, cr); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Name}
	if err = c.Get(ctx, key, cr); err != nil {
		t.Fatal(err)
	}
	if cr.Spec.GitConfigRef.Name != "default" || cr.Spec.GitConfigRef.Kind != "ClusterGitConfig" || cr.Spec.DeletionPolicy != "Delete" {
		t.Fatal("defaults missing", cr.Spec)
	}
	var obj map[string]interface{}
	_ = json.Unmarshal(cr.Spec.Manifest.Raw, &obj)
	if obj["spec"] == nil {
		t.Fatal("unknown fields pruned")
	}
	generation := cr.Generation
	cr.Status.LastPublishedGeneration = generation
	cr.Status.LastPublishedRevision = "0123456789012345678901234567890123456789"
	if err = c.Status().Update(ctx, cr); err != nil {
		t.Fatal(err)
	}
	if err = c.Get(ctx, key, cr); err != nil {
		t.Fatal(err)
	}
	if cr.Generation != generation {
		t.Fatal("status changed generation")
	}
	cr.Labels = map[string]string{"metadata": "only"}
	if err = c.Update(ctx, cr); err != nil {
		t.Fatal(err)
	}
	if cr.Generation != generation {
		t.Fatal("metadata changed generation")
	}
	for _, field := range []string{"url", "branch", "path"} {
		fresh := &api.GitResource{}
		_ = c.Get(ctx, key, fresh)
		switch field {
		case "url":
			fresh.Spec.Repository.URL = "https://another.invalid/repo.git"
		case "branch":
			fresh.Spec.Repository.Branch = "other"
		case "path":
			fresh.Spec.Repository.Path = "other.yaml"
		}
		if err = c.Update(ctx, fresh); !apierrors.IsInvalid(err) {
			t.Fatalf("mutable %s: %v", field, err)
		}
	}
	for i, bad := range []string{`[]`, `"yaml"`, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{}}`, `{"metadata":{"name":"x"}}`} {
		invalid := cr.DeepCopy()
		invalid.Name = "bad" + string(rune('a'+i))
		invalid.ResourceVersion = ""
		invalid.UID = ""
		invalid.Spec.Manifest.Raw = []byte(bad)
		if err = c.Create(ctx, invalid); err == nil {
			t.Fatal("accepted invalid manifest", bad)
		}
	}
	cr.Finalizers = []string{"gitops.example.io/git-cleanup"}
	if err = c.Update(ctx, cr); err != nil {
		t.Fatal(err)
	}
	if err = c.Delete(ctx, cr); err != nil {
		t.Fatal(err)
	}
	if err = c.Get(ctx, key, cr); err != nil || cr.DeletionTimestamp.IsZero() {
		t.Fatal("finalizer did not retain CR", err)
	}
	cr.Finalizers = nil
	if err = c.Update(ctx, cr); err != nil {
		t.Fatal(err)
	}
	if err = c.Get(ctx, key, cr); !apierrors.IsNotFound(err) {
		t.Fatal("finalizer removal did not delete", err)
	}
}
