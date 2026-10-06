//go:build e2e

// These tests deliberately disrupt only the dedicated disposable development stack.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-logr/logr"
	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"github.com/inelson/git-state-controller/internal/controller"
	"github.com/inelson/git-state-controller/internal/manifest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type stack struct {
	client.Client
	root, namespace, repoURL string
	auth                     *http.BasicAuth
	watcher                  client.WithWatch
}

func newStack(t *testing.T) *stack {
	ctrl.SetLogger(logr.Discard())
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"forgejo-port", "argocd-port"} {
		port, err := os.ReadFile(filepath.Join(root, ".dev", endpoint))
		if err != nil {
			t.Fatal(err)
		}
		poll(t, endpoint+" host endpoint", func(ctx context.Context) bool {
			req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, "http://127.0.0.1:"+strings.TrimSpace(string(port))+"/", nil)
			if err != nil {
				return false
			}
			response, err := (&nethttp.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				return false
			}
			defer response.Body.Close()
			return response.StatusCode == nethttp.StatusOK
		})
	}
	kubeconfig := filepath.Join(root, ".dev/kubeconfig")
	if os.Getenv("KUBECONFIG") != kubeconfig {
		t.Fatal("Run make test-e2e: explicit managed kubeconfig required")
	}
	if _, err = os.Stat(filepath.Join(root, ".dev/owned")); err != nil {
		t.Fatal("Managed cluster missing; run make up or make dev")
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}, &clientcmd.ConfigOverrides{CurrentContext: "k3d-git-state-dev"}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Timeout = 15 * time.Second
	scheme := runtime.NewScheme()
	_ = api.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	_ = rbacv1.AddToScheme(scheme)
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	watchConfig := *cfg
	watchConfig.Timeout = 0
	watcher, err := client.NewWithWatch(&watchConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, ".dev/credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var creds struct{ Username, Password string }
	if err = json.Unmarshal(b, &creds); err != nil {
		t.Fatal(err)
	}
	port, err := os.ReadFile(filepath.Join(root, ".dev/forgejo-port"))
	if err != nil {
		t.Fatal(err)
	}
	s := &stack{Client: c, root: root, namespace: fmt.Sprintf("git-e2e-%d", time.Now().UnixNano()), repoURL: "http://127.0.0.1:" + strings.TrimSpace(string(port)) + "/demo/resources.git", auth: &http.BasicAuth{Username: creds.Username, Password: creds.Password}, watcher: watcher}
	if err = c.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.namespace}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		namespace := &corev1.Namespace{}
		if c.Get(ctx, client.ObjectKey{Name: s.namespace}, namespace) == nil && namespace.Annotations[controller.ApprovalPolicyAnnotation] != "" {
			before := namespace.DeepCopy()
			namespace.Annotations[controller.ApprovalPolicyAnnotation] = "false"
			_ = c.Patch(ctx, namespace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		}
		resources := &api.GitResourceList{}
		if err := c.List(ctx, resources, client.InNamespace(s.namespace)); err == nil {
			for i := range resources.Items {
				_ = c.Delete(ctx, &resources.Items[i])
			}
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			list := &api.GitResourceList{}
			if err := c.List(ctx, list, client.InNamespace(s.namespace)); err == nil && len(list.Items) == 0 {
				break
			}
			select {
			case <-ctx.Done():
				t.Error("test GitResource cleanup timed out")
				return
			case <-ticker.C:
			}
		}
		_ = c.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: s.namespace}})
	})
	return s
}
func poll(t *testing.T, what string, fn func(context.Context) bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if fn(ctx) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Timed out waiting for %s", what)
		case <-ticker.C:
		}
	}
}
func (s *stack) create(t *testing.T, name string) *api.GitResource {
	t.Helper()
	raw, _ := json.Marshal(map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{"name": name, "namespace": s.namespace}, "data": map[string]interface{}{"value": "initial"}})
	cr := &api.GitResource{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.namespace}, Spec: api.GitResourceSpec{Repository: api.Repository{URL: "http://forgejo.forgejo.svc.cluster.local:3000/demo/resources.git", Branch: "main", Path: fmt.Sprintf("e2e/%s/shared/%s.yaml", s.namespace, name)}, Manifest: runtime.RawExtension{Raw: raw}}}
	if err := s.Create(context.Background(), cr); err != nil {
		t.Fatal(err)
	}
	return cr
}
func (s *stack) get(t *testing.T, cr *api.GitResource) *api.GitResource {
	t.Helper()
	out := &api.GitResource{}
	if err := s.Get(context.Background(), client.ObjectKeyFromObject(cr), out); err != nil {
		t.Fatal(err)
	}
	return out
}
func (s *stack) published(t *testing.T, cr *api.GitResource) *api.GitResource {
	t.Helper()
	var out *api.GitResource
	poll(t, "publication of "+cr.Name, func(ctx context.Context) bool {
		out = &api.GitResource{}
		return s.Get(ctx, client.ObjectKeyFromObject(cr), out) == nil && out.Status.LastPublishedGeneration == out.Generation && len(out.Status.LastPublishedRevision) == 40 && meta.IsStatusConditionTrue(out.Status.Conditions, "Published")
	})
	return out
}
func (s *stack) edit(t *testing.T, cr *api.GitResource, fn func(*api.GitResource)) {
	t.Helper()
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		fresh := &api.GitResource{}
		if err := s.Get(context.Background(), client.ObjectKeyFromObject(cr), fresh); err != nil {
			return err
		}
		before := fresh.DeepCopy()
		fn(fresh)
		return s.Patch(context.Background(), fresh, client.MergeFrom(before))
	}); err != nil {
		t.Fatal(err)
	}
}
func changeValue(cr *api.GitResource, value string) {
	var obj map[string]interface{}
	_ = json.Unmarshal(cr.Spec.Manifest.Raw, &obj)
	obj["data"] = map[string]interface{}{"value": value}
	cr.Spec.Manifest.Raw, _ = json.Marshal(obj)
}
func (s *stack) clone(t *testing.T) *gogit.Repository {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r *gogit.Repository
	for {
		var err error
		r, err = gogit.PlainCloneContext(ctx, t.TempDir(), false, &gogit.CloneOptions{URL: s.repoURL, ReferenceName: plumbing.NewBranchReferenceName("main"), SingleBranch: true, Auth: s.auth})
		if err == nil {
			break
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatal("Cannot clone live fixture repository within recovery deadline")
		case <-timer.C:
		}
	}
	return r
}
func fileAt(repo *gogit.Repository, revision, path string) ([]byte, error) {
	commit, err := repo.CommitObject(plumbing.NewHash(revision))
	if err != nil {
		return nil, err
	}
	f, err := commit.File(path)
	if err != nil {
		return nil, err
	}
	b, err := f.Contents()
	return []byte(b), err
}
func (s *stack) assertFile(t *testing.T, cr *api.GitResource) {
	t.Helper()
	repo := s.clone(t)
	head, _ := repo.Head()
	want, _, err := manifest.RenderManaged(cr.Spec.Manifest.Raw, cr.Namespace, cr.Name, string(cr.UID), "managed")
	if err != nil {
		t.Fatal(err)
	}
	got, err := fileAt(repo, head.Hash().String(), cr.Spec.Repository.Path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Wrong published content for %s", cr.Name)
	}
	got, err = fileAt(repo, cr.Status.LastPublishedRevision, cr.Spec.Repository.Path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("status SHA does not identify processed bytes")
	}
}
func appObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("argoproj.io/v1alpha1")
	obj.SetKind("Application")
	return obj
}
func (s *stack) application(t *testing.T, cr *api.GitResource) {
	t.Helper()
	poll(t, "SHA-pinned Application and ConfigMap for "+cr.Name, func(ctx context.Context) bool {
		app := appObject()
		if s.Get(ctx, types.NamespacedName{Namespace: "argocd", Name: controller.ApplicationName(cr)}, app) != nil {
			return false
		}
		sha, _, _ := unstructured.NestedString(app.Object, "spec", "source", "targetRevision")
		include, _, _ := unstructured.NestedString(app.Object, "spec", "source", "directory", "include")
		recurse, _, _ := unstructured.NestedBool(app.Object, "spec", "source", "directory", "recurse")
		cm := &corev1.ConfigMap{}
		if s.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: cr.Name}, cm) != nil {
			return false
		}
		var obj map[string]interface{}
		_ = json.Unmarshal(cr.Spec.Manifest.Raw, &obj)
		want := obj["data"].(map[string]interface{})["value"].(string)
		return sha == cr.Status.LastPublishedRevision && include == cr.Name+".yaml" && !recurse && cm.Data["value"] == want
	})
}
func (s *stack) deleted(t *testing.T, cr *api.GitResource) {
	t.Helper()
	poll(t, "GitResource deletion", func(ctx context.Context) bool {
		return apierrors.IsNotFound(s.Get(ctx, client.ObjectKeyFromObject(cr), &api.GitResource{}))
	})
}
func (s *stack) downstreamDeleted(t *testing.T, cr *api.GitResource) {
	t.Helper()
	poll(t, "Application and ConfigMap cascading deletion", func(ctx context.Context) bool {
		return apierrors.IsNotFound(s.Get(ctx, types.NamespacedName{Namespace: "argocd", Name: controller.ApplicationName(cr)}, appObject())) && apierrors.IsNotFound(s.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: cr.Name}, &corev1.ConfigMap{}))
	})
}
func (s *stack) command(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = s.root
	cmd.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(s.root, ".dev/kubeconfig"), "KUBECTL_KUBERC=false")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Command %s failed: %v\n%s", args[0], err, b)
	}
}
func (s *stack) scale(t *testing.T, namespace, kind, name string, n int) {
	t.Helper()
	s.command(t, "kubectl", "--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev", "scale", "-n", namespace, kind+"/"+name, fmt.Sprintf("--replicas=%d", n))
	if n > 0 {
		s.command(t, "kubectl", "--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev", "rollout", "status", "-n", namespace, kind+"/"+name, "--timeout=180s")
	}
}

func TestManagedStack(t *testing.T) {
	s := newStack(t)
	one := s.create(t, "one")
	two := s.create(t, "two")
	one = s.published(t, one)
	two = s.published(t, two)
	t.Run("initial-independent-applications", func(t *testing.T) {
		s.assertFile(t, one)
		s.assertFile(t, two)
		s.application(t, one)
		s.application(t, two)
		if controller.ApplicationName(one) == controller.ApplicationName(two) {
			t.Fatal("application collision")
		}
	})
	t.Run("update-no-op-message-and-rapid-generations", func(t *testing.T) {
		old := one.Status.LastPublishedRevision
		s.edit(t, one, func(cr *api.GitResource) { changeValue(cr, "updated") })
		one = s.published(t, one)
		if one.Status.LastPublishedRevision == old {
			t.Fatal("update did not advance SHA")
		}
		s.assertFile(t, one)
		s.application(t, one)
		sha := one.Status.LastPublishedRevision
		s.edit(t, one, func(cr *api.GitResource) { cr.Labels = map[string]string{"only": "metadata"} })
		s.edit(t, one, func(cr *api.GitResource) { cr.Spec.Change = &api.Change{Message: "message only"} })
		one = s.published(t, one)
		if one.Status.LastPublishedRevision != sha {
			t.Fatal("no-op changed SHA")
		}
		for i := 0; i < 8; i++ {
			value := fmt.Sprint(i)
			s.edit(t, one, func(cr *api.GitResource) { changeValue(cr, value) })
		}
		one = s.published(t, one)
		s.assertFile(t, one)
		s.application(t, one)
	})
	t.Run("unknown-fields-and-immutable-destination", func(t *testing.T) {
		cr := s.create(t, "unknown")
		s.edit(t, cr, func(cr *api.GitResource) {
			cr.Spec.Manifest.Raw = []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"unknown"},"data":{"value":"opaque"},"arbitrary":{"list":[1,true,{"hello":"world"}]},"status":{"drop":"yes"}}`)
		})
		cr = s.published(t, cr)
		s.assertFile(t, cr)
		for _, field := range []string{"url", "branch", "path"} {
			copy := s.get(t, one)
			switch field {
			case "url":
				copy.Spec.Repository.URL = "https://other.invalid/repo.git"
			case "branch":
				copy.Spec.Repository.Branch = "other"
			case "path":
				copy.Spec.Repository.Path = "other.yaml"
			}
			if err := s.Update(context.Background(), copy); !apierrors.IsInvalid(err) {
				t.Fatalf("destination %s accepted: %v", field, err)
			}
		}
		if err := s.Delete(context.Background(), cr); err != nil {
			t.Fatal(err)
		}
		s.deleted(t, cr)
		s.downstreamDeleted(t, cr)
	})
	t.Run("twenty-concurrent-publications", func(t *testing.T) { s.measureTwenty(t) })
	t.Run("alternate-configuration-failure-and-recovery", func(t *testing.T) {
		name := s.namespace
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "git-state-system"}, Data: map[string][]byte{"username": []byte(s.auth.Username), "password": []byte("wrong")}}
		if err := s.Create(context.Background(), secret); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Delete(context.Background(), secret) })
		config := &api.ClusterGitConfig{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: api.ClusterGitConfigSpec{Credentials: api.Credentials{Source: "Secret", SecretRef: api.SecretReference{Namespace: "git-state-system", Name: name}}}}
		if err := s.Create(context.Background(), config); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Delete(context.Background(), config) })
		s.edit(t, two, func(cr *api.GitResource) {
			cr.Spec.GitConfigRef = api.GitConfigReference{Kind: "ClusterGitConfig", Name: name}
			changeValue(cr, "after recovery")
		})
		poll(t, "failed new publication", func(ctx context.Context) bool {
			cr := &api.GitResource{}
			return s.Get(ctx, client.ObjectKeyFromObject(two), cr) == nil && cr.Status.ObservedGeneration == cr.Generation && !meta.IsStatusConditionTrue(cr.Status.Conditions, "Published") && cr.Status.LastPublishedRevision == two.Status.LastPublishedRevision
		})
		app := appObject()
		if err := s.Get(context.Background(), types.NamespacedName{Namespace: "argocd", Name: controller.ApplicationName(two)}, app); err != nil {
			t.Fatal(err)
		}
		sha, _, _ := unstructured.NestedString(app.Object, "spec", "source", "targetRevision")
		if sha != two.Status.LastPublishedRevision {
			t.Fatal("failed update replaced prior Application SHA")
		}
		fresh := &corev1.Secret{}
		_ = s.Get(context.Background(), client.ObjectKeyFromObject(secret), fresh)
		fresh.Data["password"] = []byte(s.auth.Password)
		if err := s.Update(context.Background(), fresh); err != nil {
			t.Fatal(err)
		}
		two = s.published(t, two)
		s.application(t, two)
		s.edit(t, two, func(cr *api.GitResource) {
			cr.Spec.GitConfigRef = api.GitConfigReference{Kind: "ClusterGitConfig", Name: "default"}
		})
		two = s.published(t, two)
	})
	t.Run("handoff-failure-independent-of-publication", func(t *testing.T) {
		role := &rbacv1.Role{}
		key := types.NamespacedName{Namespace: "argocd", Name: "manager-role"}
		if err := s.Get(context.Background(), key, role); err != nil {
			t.Fatal(err)
		}
		original := role.DeepCopy()
		for i := range role.Rules {
			role.Rules[i].Verbs = []string{"get", "list", "watch"}
		}
		if err := s.Update(context.Background(), role); err != nil {
			t.Fatal(err)
		}
		restored := false
		restore := func() {
			if restored {
				return
			}
			fresh := &rbacv1.Role{}
			if s.Get(context.Background(), key, fresh) == nil {
				fresh.Rules = original.Rules
				if err := s.Update(context.Background(), fresh); err != nil {
					t.Error(err)
				}
			}
			restored = true
		}
		t.Cleanup(restore)
		cr := s.create(t, "handoff")
		cr = s.published(t, cr)
		s.assertFile(t, cr)
		sha := cr.Status.LastPublishedRevision
		restore()
		s.application(t, cr)
		if s.get(t, cr).Status.LastPublishedRevision != sha {
			t.Fatal("handoff retry recommitted Git")
		}
	})
	t.Run("redeploy-preserves-history-and-inventory", func(t *testing.T) {
		set := controller.ApplicationSetObject()
		if err := s.Get(context.Background(), types.NamespacedName{Namespace: "argocd", Name: "git-resources"}, set); err != nil {
			t.Fatal(err)
		}
		before, _, _ := unstructured.NestedSlice(set.Object, "spec", "generators")
		repo := s.clone(t)
		head, _ := repo.Head()
		s.command(t, "make", "up")
		after := controller.ApplicationSetObject()
		if err := s.Get(context.Background(), client.ObjectKeyFromObject(set), after); err != nil {
			t.Fatal(err)
		}
		generators, _, _ := unstructured.NestedSlice(after.Object, "spec", "generators")
		a, _ := json.Marshal(before)
		b, _ := json.Marshal(generators)
		if !bytes.Equal(a, b) {
			t.Fatal("redeployment erased inventory")
		}
		newRepo := s.clone(t)
		newHead, _ := newRepo.Head()
		if head.Hash() != newHead.Hash() {
			t.Fatal("redeploy changed Git history")
		}
		s.application(t, one)
	})
	t.Run("development-watch-loads-changed-code", func(t *testing.T) {
		logPath := filepath.Join(s.root, "reports/dev-watch.log")
		if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
			t.Fatal(err)
		}
		log, err := os.Create(logPath)
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command("make", "dev")
		command.Dir = s.root
		command.Env = append(os.Environ(), "KUBECONFIG="+filepath.Join(s.root, ".dev/kubeconfig"))
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Stdout = log
		command.Stderr = log
		if err = command.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGINT)
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
				<-done
			}
			_ = log.Close()
		}
		t.Cleanup(stop)
		poll(t, "Skaffold watch loop", func(context.Context) bool {
			b, _ := os.ReadFile(logPath)
			return bytes.Contains(b, []byte("Watching for changes"))
		})
		deployment := &appsv1.Deployment{}
		key := types.NamespacedName{Namespace: "git-state-system", Name: "controller-manager"}
		if err = s.Get(context.Background(), key, deployment); err != nil {
			t.Fatal(err)
		}
		image := deployment.Spec.Template.Spec.Containers[0].Image
		probe := filepath.Join(s.root, "cmd/e2e_watch_probe.go")
		if _, err = os.Stat(probe); !os.IsNotExist(err) {
			t.Fatal("E2E watch probe already exists; refusing to overwrite it")
		}
		if err = os.WriteFile(probe, []byte("package main\nimport \"os\"\nfunc init(){os.Setenv(\"GIT_STATE_E2E_WATCH\", \"temporary\")}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(probe) })
		poll(t, "changed Go code built, loaded and deployed", func(ctx context.Context) bool {
			d := &appsv1.Deployment{}
			return s.Get(ctx, key, d) == nil && d.Spec.Template.Spec.Containers[0].Image != image && d.Status.ObservedGeneration == d.Generation && d.Status.Replicas == 1 && d.Status.UpdatedReplicas == 1 && d.Status.ReadyReplicas == 1 && d.Status.AvailableReplicas == 1
		})
		stop()
		if err = os.Remove(probe); err != nil {
			t.Fatal(err)
		}
		s.application(t, two)
	})
	t.Run("git-outage-during-delete", func(t *testing.T) {
		s.scale(t, "forgejo", "deployment", "forgejo", 0)
		restored := false
		t.Cleanup(func() {
			if !restored {
				s.scale(t, "forgejo", "deployment", "forgejo", 1)
			}
		})
		t.Cleanup(func() {
			if t.Failed() {
				s.command(t, "hack/diagnostics.sh")
			}
		})
		// Wait for endpoint removal rather than assuming scale has already stopped the Pod.
		poll(t, "Forgejo outage", func(ctx context.Context) bool {
			pods := &corev1.PodList{}
			return s.List(ctx, pods, client.InNamespace("forgejo"), client.MatchingLabels{"app": "forgejo"}) == nil && len(pods.Items) == 0
		})
		if err := s.Delete(context.Background(), one); err != nil {
			t.Fatal(err)
		}
		poll(t, "DeleteFailed with retained finalizer", func(ctx context.Context) bool {
			cr := &api.GitResource{}
			if s.Get(ctx, client.ObjectKeyFromObject(one), cr) != nil {
				return false
			}
			condition := meta.FindStatusCondition(cr.Status.Conditions, "Published")
			return !cr.DeletionTimestamp.IsZero() && condition != nil && condition.Reason == "DeleteFailed" && len(cr.Finalizers) > 0
		})
		s.scale(t, "forgejo", "deployment", "forgejo", 1)
		restored = true
		s.deleted(t, one)
		s.downstreamDeleted(t, one)
		repo := s.clone(t)
		head, _ := repo.Head()
		if _, err := fileAt(repo, head.Hash().String(), one.Spec.Repository.Path); err == nil {
			t.Fatal("deleted file retained")
		}
		if _, err := fileAt(repo, head.Hash().String(), two.Spec.Repository.Path); err != nil {
			t.Fatal("delete removed unrelated file")
		}
	})
	t.Run("argo-unavailable-delete-and-restart-recovery", func(t *testing.T) {
		s.scale(t, "argocd", "deployment", "argocd-applicationset-controller", 0)
		s.scale(t, "argocd", "statefulset", "argocd-application-controller", 0)
		restored := false
		t.Cleanup(func() {
			if !restored {
				s.scale(t, "argocd", "deployment", "argocd-applicationset-controller", 1)
				s.scale(t, "argocd", "statefulset", "argocd-application-controller", 1)
			}
		})
		if err := s.Delete(context.Background(), two); err != nil {
			t.Fatal(err)
		}
		poll(t, "Git cleanup while Argo is stopped", func(ctx context.Context) bool {
			current := &api.GitResource{}
			return s.Get(ctx, client.ObjectKeyFromObject(two), current) == nil && current.Status.Cleanup != nil && current.Status.Cleanup.Revision != ""
		})
		s.command(t, "kubectl", "--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev", "rollout", "restart", "-n", "git-state-system", "deployment/controller-manager")
		s.command(t, "kubectl", "--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev", "rollout", "status", "-n", "git-state-system", "deployment/controller-manager", "--timeout=180s")
		poll(t, "inventory removal after restart", func(ctx context.Context) bool {
			set := controller.ApplicationSetObject()
			if s.Get(ctx, types.NamespacedName{Namespace: "argocd", Name: "git-resources"}, set) != nil {
				return false
			}
			generators, _, _ := unstructured.NestedSlice(set.Object, "spec", "generators")
			b, _ := json.Marshal(generators)
			return !bytes.Contains(b, []byte(string(two.UID)))
		})
		s.scale(t, "argocd", "deployment", "argocd-applicationset-controller", 1)
		s.scale(t, "argocd", "statefulset", "argocd-application-controller", 1)
		restored = true
		s.deleted(t, two)
		s.downstreamDeleted(t, two)
	})
}
