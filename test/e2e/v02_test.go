//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"github.com/inelson/git-state-controller/internal/controller"
	"github.com/inelson/git-state-controller/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (s *stack) ready(t *testing.T, cr *api.GitResource) *api.GitResource {
	t.Helper()
	var out *api.GitResource
	poll(t, "current Ready "+cr.Name, func(ctx context.Context) bool {
		out = &api.GitResource{}
		if s.Get(ctx, client.ObjectKeyFromObject(cr), out) != nil {
			return false
		}
		c := meta.FindStatusCondition(out.Status.Conditions, "Ready")
		return c != nil && c.Status == "True" && c.ObservedGeneration == out.Generation
	})
	return out
}
func (s *stack) appRef(cr *api.GitResource) types.NamespacedName {
	if cr.Status.ApplicationRef != nil {
		return types.NamespacedName{Namespace: cr.Status.ApplicationRef.Namespace, Name: cr.Status.ApplicationRef.Name}
	}
	return types.NamespacedName{Namespace: "argocd", Name: controller.ApplicationName(cr)}
}
func (s *stack) externalEdit(t *testing.T, path string, content []byte, remove bool) {
	t.Helper()
	repo := s.clone(t)
	work, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if remove {
		_, err = work.Remove(path)
	} else {
		err = os.WriteFile(filepath.Join(work.Filesystem.Root(), path), content, 0600)
		if err == nil {
			_, err = work.Add(path)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err = work.Commit("E2E external file edit", &gogit.CommitOptions{Author: &object.Signature{Name: "E2E", Email: "e2e@example.invalid", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if err = repo.Push(&gogit.PushOptions{Auth: s.auth}); err != nil {
		t.Fatal(err)
	}
}
func TestV02(t *testing.T) {
	s := newStack(t)
	t.Run("statusless-ready-pause-and-drift", func(t *testing.T) {
		cr := s.ready(t, s.published(t, s.create(t, "read-model")))
		if cr.Status.Resource.Status != nil || cr.Status.Resource.Exists == nil || !*cr.Status.Resource.Exists {
			t.Fatal("ConfigMap fabricated status or missing existence")
		}
		sha := cr.Status.LastPublishedRevision
		app := appObject()
		_ = s.Get(context.Background(), s.appRef(cr), app)
		appUID := app.GetUID()
		s.edit(t, cr, func(c *api.GitResource) {
			c.Annotations = map[string]string{controller.PausedAnnotation: "true"}
			changeValue(c, "paused value")
		})
		poll(t, "paused summary and finalizer", func(ctx context.Context) bool {
			c := &api.GitResource{}
			if s.Get(ctx, client.ObjectKeyFromObject(cr), c) != nil {
				return false
			}
			ready := meta.FindStatusCondition(c.Status.Conditions, "Ready")
			return ready != nil && ready.Reason == "ReconcilePaused" && len(c.Finalizers) > 0
		})
		time.Sleep(2 * time.Second)
		current := s.get(t, cr)
		if current.Status.LastPublishedRevision != sha {
			t.Fatal("paused publication advanced")
		}
		s.edit(t, cr, func(c *api.GitResource) { delete(c.Annotations, controller.PausedAnnotation) })
		cr = s.ready(t, s.published(t, cr))
		sha = cr.Status.LastPublishedRevision
		content, _, _ := manifest.RenderManaged(cr.Spec.Manifest.Raw, cr.Namespace, cr.Name, string(cr.UID), "managed")
		external := bytes.ReplaceAll(content, []byte("paused value"), []byte("external value"))
		s.externalEdit(t, cr.Spec.Repository.Path, external, false)
		poll(t, "informational Git drift", func(ctx context.Context) bool {
			c := &api.GitResource{}
			if s.Get(ctx, client.ObjectKeyFromObject(cr), c) != nil {
				return false
			}
			return meta.IsStatusConditionTrue(c.Status.Conditions, "GitDrift") && meta.IsStatusConditionTrue(c.Status.Conditions, "Ready") && c.Status.LastPublishedRevision == sha
		})
		repo := s.clone(t)
		head, _ := repo.Head()
		still, err := fileAt(repo, head.Hash().String(), cr.Spec.Repository.Path)
		if err != nil || !bytes.Equal(still, external) {
			t.Fatal("drift was repaired")
		}
		s.edit(t, cr, func(c *api.GitResource) { c.Spec.Change.Message = "message only" })
		current = s.published(t, cr)
		if current.Status.LastPublishedRevision != sha || !meta.IsStatusConditionTrue(current.Status.Conditions, "GitDrift") {
			t.Fatal("message-only event advanced pin")
		}
		s.edit(t, cr, func(c *api.GitResource) { changeValue(c, "desired next") })
		cr = s.ready(t, s.published(t, cr))
		if meta.IsStatusConditionTrue(cr.Status.Conditions, "GitDrift") {
			t.Fatal("desired update did not clear warning")
		}
		_ = s.Get(context.Background(), s.appRef(cr), app)
		if app.GetUID() != appUID {
			t.Fatal("Application replaced")
		}
		s.edit(t, cr, func(c *api.GitResource) { c.Annotations = map[string]string{controller.PausedAnnotation: "true"} })
		_ = s.Delete(context.Background(), cr)
		poll(t, "paused deletion", func(ctx context.Context) bool {
			c := &api.GitResource{}
			return s.Get(ctx, client.ObjectKeyFromObject(cr), c) == nil && !c.DeletionTimestamp.IsZero() && len(c.Finalizers) > 0
		})
		s.edit(t, cr, func(c *api.GitResource) { delete(c.Annotations, controller.PausedAnnotation) })
		s.deleted(t, cr)
		s.downstreamDeleted(t, cr)
	})
	t.Run("status-bearing-target-poll-and-size-recovery", func(t *testing.T) {
		cr := s.create(t, "status-target")
		s.edit(t, cr, func(c *api.GitResource) {
			c.Spec.Manifest.Raw = []byte(fmt.Sprintf(`{"apiVersion":"testing.gitops.example.io/v1alpha1","kind":"StatusObject","metadata":{"name":"status-target","namespace":%q},"spec":{"input":"value"}}`, s.namespace))
		})
		cr = s.published(t, cr)
		target := &unstructured.Unstructured{}
		target.SetAPIVersion("testing.gitops.example.io/v1alpha1")
		target.SetKind("StatusObject")
		key := types.NamespacedName{Namespace: s.namespace, Name: cr.Name}
		poll(t, "status target exists", func(ctx context.Context) bool { return s.Get(ctx, key, target) == nil })
		setStatus := func(state, reason string, generation int64, output string) {
			t.Helper()
			if err := s.Get(context.Background(), key, target); err != nil {
				t.Fatal(err)
			}
			if generation < 0 {
				generation = target.GetGeneration()
			}
			target.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{"type": "Ready", "status": state, "reason": reason, "observedGeneration": generation}}, "outputs": map[string]interface{}{"arbitrary": output}}
			if err := s.Status().Update(context.Background(), target); err != nil {
				t.Fatal(err)
			}
		}
		setStatus("True", "Available", target.GetGeneration(), "first")
		cr = s.ready(t, cr)
		s.scale(t, "argocd", "statefulset", "argocd-application-controller", 0)
		restored := false
		t.Cleanup(func() {
			if !restored {
				s.scale(t, "argocd", "statefulset", "argocd-application-controller", 1)
			}
		})
		poll(t, "Argo controller stopped", func(ctx context.Context) bool {
			pods := &corev1.PodList{}
			return s.List(ctx, pods, client.InNamespace("argocd"), client.MatchingLabels{"app.kubernetes.io/name": "argocd-application-controller"}) == nil && len(pods.Items) == 0
		})
		application := appObject()
		if err := s.Get(context.Background(), s.appRef(cr), application); err != nil {
			t.Fatal(err)
		}
		nativeBefore, _ := json.Marshal(application.Object["status"])
		setStatus("True", "Available", target.GetGeneration(), "polled without Argo events")
		poll(t, "target-only poll while Argo stopped", func(ctx context.Context) bool {
			c := &api.GitResource{}
			return s.Get(ctx, client.ObjectKeyFromObject(cr), c) == nil && c.Status.Resource != nil && c.Status.Resource.Status != nil && bytes.Contains(c.Status.Resource.Status.Raw, []byte("polled without Argo events"))
		})
		if err := s.Get(context.Background(), s.appRef(cr), application); err != nil {
			t.Fatal(err)
		}
		nativeAfter, _ := json.Marshal(application.Object["status"])
		if !bytes.Equal(nativeBefore, nativeAfter) {
			t.Fatal("target-only proof had an Argo status update")
		}
		s.scale(t, "argocd", "statefulset", "argocd-application-controller", 1)
		restored = true
		setStatus("False", "ProviderFailed", target.GetGeneration(), "second")
		poll(t, "target-only status poll", func(ctx context.Context) bool {
			c := &api.GitResource{}
			if s.Get(ctx, client.ObjectKeyFromObject(cr), c) != nil {
				return false
			}
			ready := meta.FindStatusCondition(c.Status.Conditions, "Ready")
			return ready != nil && ready.Status == "False" && ready.Reason == "ProviderFailed" && c.Status.Resource.Status != nil && bytes.Contains(c.Status.Resource.Status.Raw, []byte("second"))
		})
		setStatus("True", "Available", target.GetGeneration()-1, "stale")
		poll(t, "stale Ready generation", func(ctx context.Context) bool {
			c := &api.GitResource{}
			if s.Get(ctx, client.ObjectKeyFromObject(cr), c) != nil {
				return false
			}
			ready := meta.FindStatusCondition(c.Status.Conditions, "Ready")
			return ready != nil && ready.Status == "Unknown" && ready.Reason == "StaleResourceStatus"
		})
		setStatus("True", "Available", target.GetGeneration(), strings.Repeat("x", controller.MirrorLimit))
		poll(t, "oversized live status", func(ctx context.Context) bool {
			c := &api.GitResource{}
			return s.Get(ctx, client.ObjectKeyFromObject(cr), c) == nil && c.Status.Resource != nil && c.Status.Resource.Status == nil && c.Status.Resource.Observation.Reason == "SnapshotTooLarge"
		})
		s.edit(t, cr, func(c *api.GitResource) {
			var obj map[string]interface{}
			_ = json.Unmarshal(c.Spec.Manifest.Raw, &obj)
			obj["spec"] = map[string]interface{}{"input": "published despite large status"}
			c.Spec.Manifest.Raw, _ = json.Marshal(obj)
		})
		cr = s.published(t, cr)
		poll(t, "new target spec applied despite mirror size", func(ctx context.Context) bool {
			if s.Get(ctx, key, target) != nil {
				return false
			}
			input, _, _ := unstructured.NestedString(target.Object, "spec", "input")
			return input == "published despite large status"
		})
		setStatus("True", "Available", -1, "small again")
		_ = s.ready(t, cr)
		_ = s.Delete(context.Background(), cr)
		s.deleted(t, cr)
		poll(t, "status target cascade", func(ctx context.Context) bool { return apierrors.IsNotFound(s.Get(ctx, key, target)) })
	})
	t.Run("orphan-and-readopt-identity", func(t *testing.T) {
		cr := s.ready(t, s.published(t, s.create(t, "retained")))
		app := appObject()
		_ = s.Get(context.Background(), s.appRef(cr), app)
		appKey := s.appRef(cr)
		appUID := app.GetUID()
		cm := &corev1.ConfigMap{}
		cmKey := types.NamespacedName{Namespace: s.namespace, Name: cr.Name}
		_ = s.Get(context.Background(), cmKey, cm)
		cmUID := cm.UID
		s.edit(t, cr, func(c *api.GitResource) { c.Spec.DeletionPolicy = "Orphan"; changeValue(c, "last approved") })
		cr = s.ready(t, s.published(t, cr))
		s.edit(t, cr, func(c *api.GitResource) {
			c.Annotations = map[string]string{controller.PausedAnnotation: "true"}
			changeValue(c, "pending unpublished")
		})
		_ = s.Delete(context.Background(), cr)
		s.scale(t, "argocd", "statefulset", "argocd-application-controller", 0)
		restored := false
		t.Cleanup(func() {
			if !restored {
				s.scale(t, "argocd", "statefulset", "argocd-application-controller", 1)
			}
		})
		s.edit(t, cr, func(c *api.GitResource) { delete(c.Annotations, controller.PausedAnnotation) })
		s.deleted(t, cr)
		s.command(t, "kubectl", "--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev", "rollout", "restart", "-n", "git-state-system", "deployment/controller-manager")
		s.command(t, "kubectl", "--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev", "rollout", "status", "-n", "git-state-system", "deployment/controller-manager", "--timeout=180s")
		s.scale(t, "argocd", "statefulset", "argocd-application-controller", 1)
		restored = true
		set := controller.ApplicationSetObject()
		poll(t, "retained Application identity", func(ctx context.Context) bool {
			if s.Get(ctx, types.NamespacedName{Namespace: "argocd", Name: "git-resources"}, set) != nil || s.Get(ctx, appKey, app) != nil || s.Get(ctx, cmKey, cm) != nil {
				return false
			}
			return app.GetUID() == appUID && app.GetDeletionTimestamp().IsZero() && cm.UID == cmUID && cm.Data["value"] == "last approved"
		})
		repo := s.clone(t)
		head, _ := repo.Head()
		content, err := fileAt(repo, head.Hash().String(), cr.Spec.Repository.Path)
		if err != nil {
			t.Fatal(err)
		}
		_, _, uid, state := manifest.Ownership(content)
		if uid != string(cr.UID) || state != "orphaned" || bytes.Contains(content, []byte("pending unpublished")) {
			t.Fatal("orphan wrong content")
		}
		replacement := cr.DeepCopy()
		replacement.ResourceVersion = ""
		replacement.UID = ""
		replacement.Generation = 0
		replacement.DeletionTimestamp = nil
		replacement.CreationTimestamp = metav1.Time{}
		replacement.Finalizers = nil
		replacement.Status = api.GitResourceStatus{}
		replacement.Annotations = map[string]string{controller.AdoptAnnotation: "true"}
		replacement.Spec.DeletionPolicy = "Delete"
		changeValue(replacement, "adopted desired")
		if err := s.Create(context.Background(), replacement); err != nil {
			t.Fatal(err)
		}
		replacement = s.published(t, replacement)
		poll(t, "one-time adoption consumed", func(ctx context.Context) bool {
			c := &api.GitResource{}
			return s.Get(ctx, client.ObjectKeyFromObject(replacement), c) == nil && c.Annotations[controller.AdoptAnnotation] == ""
		})
		replacement = s.ready(t, replacement)
		if s.appRef(replacement) != appKey {
			t.Fatal("adoption renamed Application")
		}
		_ = s.Get(context.Background(), appKey, app)
		_ = s.Get(context.Background(), cmKey, cm)
		if app.GetUID() != appUID || cm.UID != cmUID || cm.Data["value"] != "adopted desired" {
			t.Fatal("adoption replaced workload or Application")
		}
		_ = s.Delete(context.Background(), replacement)
		s.deleted(t, replacement)
		poll(t, "adopted target cleanup", func(ctx context.Context) bool {
			return apierrors.IsNotFound(s.Get(ctx, appKey, app)) && apierrors.IsNotFound(s.Get(ctx, cmKey, cm))
		})
	})
}
