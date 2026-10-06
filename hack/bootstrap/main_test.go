package main

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStaticTemplateRefreshPreservesGeneratorElements(t *testing.T) {
	c := fake.NewClientBuilder().Build()
	ctx := context.Background()
	if err := ensureApplicationSet(ctx, c, "../../dev/bootstrap/applicationset.yaml"); err != nil {
		t.Fatal(err)
	}
	set := &unstructured.Unstructured{}
	set.SetAPIVersion("argoproj.io/v1alpha1")
	set.SetKind("ApplicationSet")
	key := client.ObjectKey{Namespace: "argocd", Name: "git-resources"}
	if err := c.Get(ctx, key, set); err != nil {
		t.Fatal(err)
	}
	elements := []interface{}{map[string]interface{}{"name": "published", "revision": "0123456789012345678901234567890123456789"}}
	generators := []interface{}{map[string]interface{}{"list": map[string]interface{}{"elements": elements}}}
	if err := unstructured.SetNestedSlice(set.Object, generators, "spec", "generators"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(set.Object, "old-template", "spec", "template", "metadata", "name"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, set); err != nil {
		t.Fatal(err)
	}
	if err := ensureApplicationSet(ctx, c, "../../dev/bootstrap/applicationset.yaml"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, set); err != nil {
		t.Fatal(err)
	}
	got, _, err := unstructured.NestedSlice(set.Object, "spec", "generators")
	if err != nil {
		t.Fatal(err)
	}
	retained := got[0].(map[string]interface{})["list"].(map[string]interface{})["elements"].([]interface{})
	if len(retained) != 1 || retained[0].(map[string]interface{})["name"] != "published" {
		t.Fatal("bootstrap erased managed inventory", retained)
	}
	name, _, err := unstructured.NestedString(set.Object, "spec", "template", "metadata", "name")
	if err != nil || name != "{{.applicationName}}" {
		t.Fatal("static template did not update", name, err)
	}
}
