package controller

import (
	"context"
	"errors"
	"fmt"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	writer "github.com/inelson/git-state-controller/internal/git"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestPublicationCombinesStatusAndBuffersEvidence(t *testing.T) {
	_, r, cr := fixture(t)
	publications, configs := 0, 0
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
		switch current := obj.(type) {
		case *api.GitResource:
			if current.Status.LastPublishedRevision != "" {
				publications++
				if !meta.IsStatusConditionTrue(current.Status.Conditions, "Published") || !meta.IsStatusConditionFalse(current.Status.Conditions, "GitDrift") {
					t.Fatal("publication and drift were not written together")
				}
			}
		case *api.ClusterGitConfig:
			configs++
		}
		return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
	}})
	reconcileCR(t, r, cr)
	if publications != 1 || configs != 2 {
		t.Fatalf("first publication patches: resource=%d config=%d", publications, configs)
	}
	reconcileCR(t, r, cr) // A read-only comparison replaces Push evidence with Fetch.
	reconcileCR(t, r, cr) // Identical Fetch evidence and resource status are no-ops.
	if publications != 1 || configs != 3 {
		t.Fatalf("no-op patches: resource=%d config=%d", publications, configs)
	}
}

func TestBranchContentionIsPendingAndTransportFailureIsFailed(t *testing.T) {
	for _, contention := range []bool{true, false} {
		t.Run(fmt.Sprint(contention), func(t *testing.T) {
			s, r, cr := fixture(t)
			attempt := 0
			if contention {
				r.Publisher.BeforePush = func() {
					attempt++
					op := writer.Operation{URL: s.URL, Branch: "main", Path: "other.yaml", Content: []byte(fmt.Sprintf("value: %d\n", attempt)), Credentials: writer.Credentials{Username: "bot", Password: "password"}, AuthorName: "test", AuthorEmail: "test@example.invalid", Message: "advance branch"}
					if _, err := (&writer.Publisher{}).Attempt(context.Background(), op); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				r.Publisher.Push = func(context.Context, *gogit.Repository, *gogit.PushOptions) error {
					return errors.New("connection interrupted")
				}
			}
			result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)})
			current := fresh(t, r, cr)
			published := meta.FindStatusCondition(current.Status.Conditions, "Published")
			if contention {
				if err != nil || result.RequeueAfter <= 0 || published == nil || published.Reason != "PublishPending" {
					t.Fatalf("contention treated as failure: %+v %v %+v", result, err, published)
				}
				r.Publisher.BeforePush = nil
				reconcileCR(t, r, cr)
				if !publicationCurrent(fresh(t, r, cr)) {
					t.Fatal("pending publication did not recover")
				}
			} else if !errors.Is(err, writer.ErrRetry) || errors.Is(err, writer.ErrBranchChanged) || published == nil || published.Reason != "PublishFailed" {
				t.Fatalf("transport failure hidden: %v %+v", err, published)
			}
		})
	}
}

func TestApplicationPredicatesIgnoreEnvelopeAndPreserveOpaqueChanges(t *testing.T) {
	original := ApplicationObject()
	original.SetUID("app")
	original.SetGeneration(1)
	original.Object["spec"] = map[string]interface{}{"source": map[string]interface{}{"targetRevision": "old"}, "destination": map[string]interface{}{"namespace": "demo"}}
	original.Object["status"] = map[string]interface{}{"future": map[string]interface{}{"value": "old"}}
	tests := []struct {
		name   string
		change func(*unstructured.Unstructured)
		want   bool
	}{
		{"envelope", func(o *unstructured.Unstructured) { o.SetGeneration(2); o.SetResourceVersion("new") }, false},
		{"operation", func(o *unstructured.Unstructured) {
			o.Object["operation"] = map[string]interface{}{"sync": map[string]interface{}{}}
		}, false},
		{"unknown status", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "new", "status", "future", "value")
		}, true},
		{"removed status", func(o *unstructured.Unstructured) { delete(o.Object, "status") }, true},
		{"source", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "new", "spec", "source", "targetRevision")
		}, true},
		{"multiple sources", func(o *unstructured.Unstructured) {
			o.Object["spec"].(map[string]interface{})["sources"] = []interface{}{map[string]interface{}{}}
		}, true},
		{"destination", func(o *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(o.Object, "other", "spec", "destination", "namespace")
		}, true},
		{"replacement", func(o *unstructured.Unstructured) { o.SetUID("new") }, true},
		{"tracking", func(o *unstructured.Unstructured) { o.SetAnnotations(map[string]string{UIDAnnotation: "new"}) }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := original.DeepCopy()
			tt.change(next)
			if got := applicationPredicate.Update(event.UpdateEvent{ObjectOld: original, ObjectNew: next}); got != tt.want {
				t.Fatalf("enqueue=%v want=%v", got, tt.want)
			}
		})
	}
	next := original.DeepCopy()
	next.SetGeneration(2)
	next.Object["status"] = map[string]interface{}{"timestamp": "new"}
	if applicationSetPredicate.Update(event.UpdateEvent{ObjectOld: original, ObjectNew: next}) {
		t.Fatal("ApplicationSet status enqueued inventory")
	}
	_ = unstructured.SetNestedField(next.Object, "new", "spec", "source", "targetRevision")
	if !applicationSetPredicate.Update(event.UpdateEvent{ObjectOld: original, ObjectNew: next}) {
		t.Fatal("ApplicationSet spec change was ignored")
	}
}
