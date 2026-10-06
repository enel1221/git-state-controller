package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func rawJSON(value interface{}) *apiext.JSON {
	b, _ := json.Marshal(value)
	return &apiext.JSON{Raw: b}
}
func readyFixture() *api.GitResource {
	cr := &api.GitResource{ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "example", UID: "owner", Generation: 2}, Spec: api.GitResourceSpec{Repository: api.Repository{URL: "https://example.invalid/repo", Branch: "main", Path: "demo/resource.yaml"}}}
	cr.Status.LastPublishedGeneration = 2
	cr.Status.LastPublishedRevision = strings.Repeat("a", 40)
	condition(cr, "Published", metav1.ConditionTrue, "Pushed", "")
	source := map[string]interface{}{"repoURL": cr.Spec.Repository.URL, "targetRevision": cr.Status.LastPublishedRevision, "path": "demo", "directory": map[string]interface{}{"include": "resource.yaml"}}
	dest := map[string]interface{}{"server": "https://kubernetes.default.svc", "namespace": "demo"}
	cr.Status.ArgoCD = &api.ApplicationSnapshot{UID: "app", Observation: api.Observation{Reason: "Observed"}, Source: rawJSON(source), Destination: rawJSON(dest), Status: rawJSON(map[string]interface{}{"sync": map[string]interface{}{"status": "Synced", "revision": cr.Status.LastPublishedRevision, "comparedTo": map[string]interface{}{"source": source, "destination": dest}}, "health": map[string]interface{}{"status": "Healthy"}})}
	exists := true
	gen := int64(4)
	cr.Status.Resource = &api.ResourceSnapshot{Ref: api.ResourceReference{APIVersion: "v1", Kind: "ConfigMap", Namespace: "demo", Name: "target", UID: "target-uid"}, Generation: &gen, Exists: &exists, Observation: api.Observation{Reason: "Observed"}}
	return cr
}
func mutateApp(cr *api.GitResource, fn func(map[string]interface{})) {
	raw := rawObject(cr.Status.ArgoCD.Status)
	fn(raw)
	cr.Status.ArgoCD.Status = rawJSON(raw)
}
func TestSummary(t *testing.T) {
	tests := []struct {
		name          string
		edit          func(*api.GitResource)
		synced, ready metav1.ConditionStatus
		reason        string
	}{
		{"statusless", func(*api.GitResource) {}, "True", "True", "Healthy"},
		{"same generation new SHA", func(cr *api.GitResource) { cr.Status.LastPublishedRevision = strings.Repeat("b", 40) }, "False", "False", "RevisionPending"},
		{"old comparison", func(cr *api.GitResource) {
			mutateApp(cr, func(o map[string]interface{}) {
				o["sync"].(map[string]interface{})["comparedTo"] = map[string]interface{}{"source": map[string]interface{}{"targetRevision": "old"}}
			})
		}, "False", "False", "RevisionPending"},
		{"historical success", func(cr *api.GitResource) {
			mutateApp(cr, func(o map[string]interface{}) {
				o["sync"].(map[string]interface{})["status"] = "OutOfSync"
				o["operationState"] = map[string]interface{}{"phase": "Succeeded"}
			})
		}, "False", "False", "OutOfSync"},
		{"running same pending", func(cr *api.GitResource) {
			mutateApp(cr, func(o map[string]interface{}) {
				o["sync"].(map[string]interface{})["status"] = "OutOfSync"
				o["operationState"] = map[string]interface{}{"phase": "Running", "operation": map[string]interface{}{"sync": map[string]interface{}{"revision": cr.Status.LastPublishedRevision}}}
			})
		}, "False", "False", "Syncing"},
		{"old running", func(cr *api.GitResource) {
			mutateApp(cr, func(o map[string]interface{}) {
				o["sync"].(map[string]interface{})["status"] = "OutOfSync"
				o["operationState"] = map[string]interface{}{"phase": "Running", "operation": map[string]interface{}{"sync": map[string]interface{}{"revision": "old"}}}
			})
		}, "False", "False", "OutOfSync"},
		{"missing comparison", func(cr *api.GitResource) {
			mutateApp(cr, func(o map[string]interface{}) { delete(o["sync"].(map[string]interface{}), "comparedTo") })
		}, "Unknown", "Unknown", "RevisionPending"},
		{"pending generation", func(cr *api.GitResource) { cr.Generation++ }, "Unknown", "False", "PublishPending"},
		{"paused deletion", func(cr *api.GitResource) {
			cr.Annotations = map[string]string{PausedAnnotation: "true"}
			now := metav1.Now()
			cr.DeletionTimestamp = &now
		}, "True", "False", "ReconcilePaused"},
		{"deleting", func(cr *api.GitResource) { now := metav1.Now(); cr.DeletionTimestamp = &now }, "True", "False", "Deleting"},
		{"forbidden", func(cr *api.GitResource) {
			cr.Status.Resource.Observation.Reason = "Forbidden"
			cr.Status.Resource.Status = nil
		}, "True", "Unknown", "Forbidden"},
		{"too large", func(cr *api.GitResource) { cr.Status.ArgoCD.Observation.Reason = "SnapshotTooLarge" }, "Unknown", "Unknown", "SnapshotTooLarge"},
		{"missing", func(cr *api.GitResource) {
			no := false
			cr.Status.Resource.Exists = &no
			cr.Status.Resource.Observation.Reason = "NotFound"
		}, "True", "False", "ResourceMissing"},
		{"target deleting", func(cr *api.GitResource) { now := metav1.Now(); cr.Status.Resource.DeletionTimestamp = &now }, "True", "False", "ResourceDeleting"},
		{"stale ready", func(cr *api.GitResource) {
			cr.Status.Resource.Status = rawJSON(map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "observedGeneration": 3}}})
		}, "True", "Unknown", "StaleResourceStatus"},
		{"false ready", func(cr *api.GitResource) {
			cr.Status.Resource.Status = rawJSON(map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "False", "reason": "ProviderFailed", "message": "full upstream reason"}}})
		}, "True", "False", "ProviderFailed"},
		{"unknown ready", func(cr *api.GitResource) {
			cr.Status.Resource.Status = rawJSON(map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "Unknown", "reason": "Waiting"}}})
		}, "True", "Unknown", "Waiting"},
		{"ambiguous ready", func(cr *api.GitResource) {
			cr.Status.Resource.Status = rawJSON(map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}, map[string]interface{}{"type": "Ready", "status": "True"}}})
		}, "True", "Unknown", "InvalidStatus"},
		{"bad ready", func(cr *api.GitResource) {
			cr.Status.Resource.Status = rawJSON(map[string]interface{}{"conditions": "oops"})
		}, "True", "Unknown", "InvalidStatus"},
		{"degraded", func(cr *api.GitResource) {
			mutateApp(cr, func(o map[string]interface{}) { o["health"] = map[string]interface{}{"status": "Degraded"} })
		}, "True", "False", "Degraded"},
		{"unknown health", func(cr *api.GitResource) { mutateApp(cr, func(o map[string]interface{}) { delete(o, "health") }) }, "True", "Unknown", "HealthUnknown"},
		{"informational drift", func(cr *api.GitResource) { condition(cr, "GitDrift", metav1.ConditionTrue, "ExternalModification", "") }, "True", "True", "Healthy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := readyFixture()
			tt.edit(cr)
			summarize(cr)
			s := meta.FindStatusCondition(cr.Status.Conditions, "Synced")
			r := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
			if s.Status != tt.synced || r.Status != tt.ready || r.Reason != tt.reason {
				t.Fatalf("synced=%+v ready=%+v", s, r)
			}
		})
	}
}
func observerFixture(t *testing.T) (*StatusReconciler, *api.GitResource, *unstructured.Unstructured, client.WithWatch) {
	t.Helper()
	cr := readyFixture()
	cr.Status.PublishedResourceRef = &api.ResourceReference{APIVersion: "v1", Kind: "ConfigMap", Name: "target"}
	cr.Status.ApplicationRef = &api.ApplicationReference{Namespace: "argocd", Name: "app"}
	app := ApplicationObject()
	app.SetName("app")
	app.SetNamespace("argocd")
	app.SetUID("app-uid")
	app.SetAnnotations(map[string]string{SourceAnnotation: "demo/example", UIDAnnotation: string(cr.UID), SetAnnotation: "argocd/git-resources"})
	app.Object["spec"] = map[string]interface{}{"source": rawObject(cr.Status.ArgoCD.Source), "destination": rawObject(cr.Status.ArgoCD.Destination)}
	app.Object["status"] = rawObject(cr.Status.ArgoCD.Status)
	app.Object["status"].(map[string]interface{})["resources"] = []interface{}{map[string]interface{}{"kind": "ConfigMap", "version": "v1", "namespace": "demo", "name": "other"}, map[string]interface{}{"kind": "ConfigMap", "version": "v1", "namespace": "demo", "name": "target"}}
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.GitResource{}).WithObjects(cr, app, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "target", UID: "live-uid"}, Data: map[string]string{"private": "do not mirror"}}).Build()
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{corev1.SchemeGroupVersion})
	mapper.Add(corev1.SchemeGroupVersion.WithKind("ConfigMap"), meta.RESTScopeNamespace)
	return &StatusReconciler{Client: c, Reader: c, Mapper: mapper, SetKey: types.NamespacedName{Namespace: "argocd", Name: "git-resources"}}, cr, app, c
}
func TestObserverSnapshotsNoopAndReplacement(t *testing.T) {
	r, cr, app, c := observerFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}
	app.Object["status"].(map[string]interface{})["future"] = map[string]interface{}{"nested": []interface{}{map[string]interface{}{"error": "upstream error"}}}
	if err := c.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, req.NamespacedName, cr)
	if cr.Status.Resource.Ref.UID != "live-uid" || cr.Status.Resource.Status != nil || !meta.IsStatusConditionTrue(cr.Status.Conditions, "Ready") || nested(rawObject(cr.Status.ArgoCD.Status), "future") == nil {
		t.Fatal(cr.Status)
	}
	before := cr.ResourceVersion
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, req.NamespacedName, cr)
	if before != cr.ResourceVersion {
		t.Fatal("noop observation wrote status")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(app), app)
	app.SetLabels(map[string]string{"metadata": "only"})
	app.SetGeneration(app.GetGeneration() + 1)
	_ = c.Update(ctx, app)
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	if cr.ResourceVersion != before {
		t.Fatal("metadata-only RV persisted")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(app), app)
	delete(app.Object["status"].(map[string]interface{}), "future")
	_ = c.Update(ctx, app)
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	if nested(rawObject(cr.Status.ArgoCD.Status), "future") != nil {
		t.Fatal("removed upstream key retained")
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "demo", Name: "target"}}
	_ = c.Delete(ctx, cm)
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	if cr.Status.Resource.Exists == nil || *cr.Status.Resource.Exists || cr.Status.Resource.Ref.UID != "" {
		t.Fatal("absence not authoritative", cr.Status.Resource)
	}
	cm.UID = "replacement"
	cm.ResourceVersion = ""
	_ = c.Create(ctx, cm)
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	if cr.Status.Resource.Ref.UID != "replacement" {
		t.Fatal("replacement identity lost")
	}
	// Forbidden clears payload, retains last successful identity/time, and does not imply absence.
	r.Reader = interceptor.NewClient(c, interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if obj.GetObjectKind().GroupVersionKind().Kind == "ConfigMap" {
			return apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, key.Name, nil)
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	last := cr.Status.Resource.LastUpdatedAt
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	if cr.Status.Resource.Exists != nil || cr.Status.Resource.Observation.Reason != "Forbidden" || !cr.Status.Resource.LastUpdatedAt.Equal(last) || cr.Status.Resource.Ref.UID != "replacement" {
		t.Fatal(cr.Status.Resource)
	}
}
func TestMirrorLimitsAndEventIsolation(t *testing.T) {
	r, cr, app, c := observerFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}
	app.Object["status"].(map[string]interface{})["large"] = strings.Repeat("x", MirrorLimit)
	_ = c.Update(ctx, app)
	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Get(ctx, req.NamespacedName, cr)
	if cr.Status.ArgoCD.Status != nil || cr.Status.ArgoCD.Observation.Reason != "SnapshotTooLarge" {
		t.Fatal(cr.Status.ArgoCD)
	}
	before := cr.ResourceVersion
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	if before != cr.ResourceVersion {
		t.Fatal("oversize heartbeat")
	}
	old := cr.DeepCopy()
	cr.Status.ArgoCD.Observation.Message = "new status"
	cr.Status.Resource.Status = rawJSON(map[string]interface{}{"ready": false})
	e := event.UpdateEvent{ObjectOld: old, ObjectNew: cr}
	if publisherPredicate.Update(e) || inventoryPredicate.Update(e) || observerPredicate.Update(e) {
		t.Fatal("observation triggered unrelated queue")
	}
	cr.Annotations = map[string]string{PausedAnnotation: "true"}
	if !publisherPredicate.Update(e) {
		t.Fatal("pause not enqueued")
	}
	_ = c.Get(ctx, client.ObjectKeyFromObject(app), app)
	delete(app.Object["status"].(map[string]interface{}), "large")
	_ = c.Update(ctx, app)
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	if cr.Status.ArgoCD.Observation.Reason != "Observed" {
		t.Fatal("size recovery failed")
	}
}
func TestStatusConflictMergesWriters(t *testing.T) {
	r, cr, _, c := observerFixture(t)
	first := true
	r.Client = interceptor.NewClient(c, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, base client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		if first {
			first = false
			current := &api.GitResource{}
			_ = base.Get(ctx, client.ObjectKeyFromObject(cr), current)
			current.Status.LastPublishedContentHash = "new hash"
			condition(current, "GitDrift", metav1.ConditionTrue, "ExternalModification", "")
			if err := base.Status().Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			return apierrors.NewConflict(api.GroupVersion.WithResource("gitresources").GroupResource(), cr.Name, nil)
		}
		return base.Status().Patch(ctx, obj, p, opts...)
	}})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
	if cr.Status.LastPublishedContentHash != "new hash" || !meta.IsStatusConditionTrue(cr.Status.Conditions, "GitDrift") || cr.Status.Resource.Ref.UID != "live-uid" {
		t.Fatal("writer fields lost", cr.Status)
	}
	old := cr.Status.DeepCopy()
	if err := patchStatus(context.Background(), c, c, cr, "publisher", func(*api.GitResource) {}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
	if !reflect.DeepEqual(old, &cr.Status) {
		t.Fatal("pure no-op changed conditions")
	}
}

func TestInventoryScopeAndFailures(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*api.GitResource, *unstructured.Unstructured, *StatusReconciler)
		reason string
		exists bool
	}{
		{"wrong namespace", func(_ *api.GitResource, a *unstructured.Unstructured, _ *StatusReconciler) {
			a.Object["status"].(map[string]interface{})["resources"] = []interface{}{map[string]interface{}{"kind": "ConfigMap", "version": "v1", "namespace": "wrong", "name": "target"}}
		}, "NotTracked", false},
		{"hooks", func(_ *api.GitResource, a *unstructured.Unstructured, _ *StatusReconciler) {
			a.Object["status"].(map[string]interface{})["resources"] = []interface{}{map[string]interface{}{"kind": "ConfigMap", "version": "v1", "namespace": "demo", "name": "target", "hook": true}}
		}, "NotTracked", false},
		{"ambiguous", func(_ *api.GitResource, a *unstructured.Unstructured, _ *StatusReconciler) {
			entry := map[string]interface{}{"kind": "ConfigMap", "version": "v1", "namespace": "demo", "name": "target"}
			a.Object["status"].(map[string]interface{})["resources"] = []interface{}{entry, entry}
		}, "ReadFailed", false},
		{"unsupported served version", func(_ *api.GitResource, a *unstructured.Unstructured, _ *StatusReconciler) {
			a.Object["status"].(map[string]interface{})["resources"] = []interface{}{map[string]interface{}{"kind": "ConfigMap", "version": "v999", "namespace": "demo", "name": "target"}}
		}, "UnsupportedTarget", false},
		{"recursive status", func(cr *api.GitResource, _ *unstructured.Unstructured, _ *StatusReconciler) {
			cr.Status.PublishedResourceRef = &api.ResourceReference{APIVersion: api.GroupVersion.String(), Kind: "GitResource", Name: "example"}
		}, "UnsupportedTarget", false},
		{"cluster scoped", func(cr *api.GitResource, a *unstructured.Unstructured, r *StatusReconciler) {
			cr.Status.PublishedResourceRef = &api.ResourceReference{APIVersion: "v1", Kind: "Namespace", Name: "live", Namespace: "discard"}
			r.Mapper.(*meta.DefaultRESTMapper).Add(corev1.SchemeGroupVersion.WithKind("Namespace"), meta.RESTScopeRoot)
			a.Object["status"].(map[string]interface{})["resources"] = []interface{}{map[string]interface{}{"kind": "Namespace", "version": "v1", "name": "live"}}
			_ = r.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "live", UID: "cluster-uid"}})
		}, "Observed", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, cr, app, c := observerFixture(t)
			tt.edit(cr, app, r)
			_ = c.Update(context.Background(), app)
			_, target := r.Observe(context.Background(), cr)
			if target.Observation.Reason != tt.reason {
				t.Fatal(target.Observation)
			}
			if tt.exists && (target.Exists == nil || !*target.Exists || target.Ref.Namespace != "") {
				t.Fatal("cluster scope incorrectly namespaced", target)
			}
			if !tt.exists && target.Exists != nil {
				t.Fatal("inventory ambiguity asserted existence")
			}
		})
	}
}
func TestServerSizeFallbackAndWholeGuard(t *testing.T) {
	r, cr, _, c := observerFixture(t)
	calls := 0
	r.Client = interceptor.NewClient(c, interceptor.Funcs{SubResourcePatch: func(ctx context.Context, base client.Client, sub string, obj client.Object, p client.Patch, opts ...client.SubResourcePatchOption) error {
		calls++
		if calls == 1 {
			return apierrors.NewRequestEntityTooLargeError("server size limit")
		}
		return base.Status().Patch(ctx, obj, p, opts...)
	}})
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
	if calls != 2 || cr.Status.ArgoCD.Status != nil || cr.Status.ArgoCD.Observation.Reason != "SnapshotTooLarge" || !meta.IsStatusConditionTrue(cr.Status.Conditions, "Published") {
		t.Fatal("small fallback blocked publication", calls, cr.Status)
	}
	cr.Spec.Manifest.Raw = []byte(`{"large":"` + strings.Repeat("x", ObjectLimit) + `"}`)
	cr.Status.Resource.Status = rawJSON(map[string]interface{}{"keep": "small"})
	limitMirrors(cr)
	if cr.Status.Resource.Status != nil || len(cr.Spec.Manifest.Raw) < ObjectLimit {
		t.Fatal("whole guard shrank user spec or retained mirrors")
	}
}

func TestNativeInvalidReasonAndInitialFailureTime(t *testing.T) {
	cr := readyFixture()
	cr.Status.Resource.Status = rawJSON(map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "False", "reason": "provider failed / bad output", "message": "native detail"}}})
	summarize(cr)
	ready := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
	if ready.Status != "False" || ready.Reason != "ResourceNotReady" || !strings.Contains(ready.Message, "native detail") {
		t.Fatal("invalid native reason broke standard condition", ready)
	}
	now := metav1.Now()
	a := &api.ApplicationSnapshot{LastUpdatedAt: &now, Observation: api.Observation{Reason: "NotFound"}}
	appStable(nil, a)
	if a.LastUpdatedAt != nil {
		t.Fatal("first failure invented a successful snapshot time")
	}
	resource := &api.ResourceSnapshot{LastUpdatedAt: &now, Observation: api.Observation{Reason: "Forbidden"}}
	resourceStable(nil, resource)
	if resource.LastUpdatedAt != nil {
		t.Fatal("first failure timestamp fabricated")
	}
}

func TestApplicationFailureKeepsTargetReference(t *testing.T) {
	r, cr, app, c := observerFixture(t)
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cr)}
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	uid := cr.Status.Resource.Ref.UID
	last := cr.Status.Resource.LastUpdatedAt
	_ = c.Delete(ctx, app)
	_, _ = r.Reconcile(ctx, req)
	_ = c.Get(ctx, req.NamespacedName, cr)
	if cr.Status.ArgoCD.UID != "" || cr.Status.Resource.Ref.UID != uid || cr.Status.Resource.Exists != nil || cr.Status.Resource.Status != nil || !cr.Status.Resource.LastUpdatedAt.Equal(last) {
		t.Fatal("Application absence discarded target reference or invented target absence", cr.Status)
	}
}

func TestInventoryVersionCanDifferFromUnservedAuthoredVersion(t *testing.T) {
	r, cr, _, _ := observerFixture(t)
	cr.Status.PublishedResourceRef.APIVersion = "v99"
	_, target := r.Observe(context.Background(), cr)
	if target.Observation.Reason != "Observed" || target.Ref.APIVersion != "v1" || target.Ref.UID != "live-uid" {
		t.Fatal("observer required the authored version instead of the inventory's served version", target)
	}
	cr = readyFixture()
	cr.Status.Resource.Generation = nil
	cr.Status.Resource.Status = rawJSON(map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": "True", "observedGeneration": "bad"}}})
	summarize(cr)
	ready := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
	if ready.Status != "Unknown" || ready.Reason != "InvalidStatus" {
		t.Fatal("malformed observedGeneration accepted without a live generation", ready)
	}
}
