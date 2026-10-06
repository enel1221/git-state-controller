package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strings"
	"time"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var ApplicationSetGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "ApplicationSet"}

func ApplicationSetObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(ApplicationSetGVK)
	return obj
}

type ApplicationSetReconciler struct {
	client.Client
	Reader   client.Reader
	Key      types.NamespacedName
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=argoproj.io,resources=applicationsets,resourceNames=git-resources,verbs=get;list;watch;patch;update,namespace=argocd

func ApplicationName(cr *api.GitResource) string {
	base := cr.Namespace + "-" + cr.Name
	if len(base) > 48 {
		base = base[:48]
	}
	base = strings.Trim(base, "-.")
	// Dots are valid CR names but not in an Application's DNS-label name.
	base = strings.ReplaceAll(base, ".", "-")
	suffix := sha256.Sum256([]byte(cr.UID))
	return fmt.Sprintf("%s-%x", base, suffix[:5])
}
func PublicationElements(resources []api.GitResource) []interface{} {
	sort.Slice(resources, func(i, j int) bool {
		a, b := resources[i], resources[j]
		if a.Namespace == b.Namespace {
			return a.Name < b.Name
		}
		return a.Namespace < b.Namespace
	})
	elements := []interface{}{}
	for i := range resources {
		cr := &resources[i]
		// Keep last success through failed updates and finalization. Only absence removes an entry.
		if cr.Status.LastPublishedRevision == "" || cr.Status.LastPublishedGeneration == 0 {
			continue
		}
		elements = append(elements, map[string]interface{}{
			"namespace": cr.Namespace, "name": cr.Name, "uid": string(cr.UID), "applicationName": ApplicationName(cr),
			"repoURL": cr.Spec.Repository.URL, "directory": path.Dir(cr.Spec.Repository.Path), "filename": path.Base(cr.Spec.Repository.Path), "revision": cr.Status.LastPublishedRevision,
		})
	}
	return elements
}
func (r *ApplicationSetReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		resources := &api.GitResourceList{}
		if err := r.Reader.List(ctx, resources); err != nil {
			return err
		}
		set := ApplicationSetObject()
		if err := r.Reader.Get(ctx, r.Key, set); err != nil {
			return err
		}
		generators, _, err := unstructured.NestedSlice(set.Object, "spec", "generators")
		if err != nil {
			return err
		}
		if len(generators) == 0 {
			return fmt.Errorf("sample ApplicationSet requires its first List generator")
		}
		first, ok := generators[0].(map[string]interface{})
		if !ok {
			return fmt.Errorf("invalid ApplicationSet generator")
		}
		list, ok := first["list"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("sample ApplicationSet first generator must be List")
		}
		desired := PublicationElements(resources.Items)
		if reflect.DeepEqual(list["elements"], desired) {
			return nil
		}
		before := set.DeepCopy()
		list["elements"] = desired
		if err := unstructured.SetNestedSlice(set.Object, generators, "spec", "generators"); err != nil {
			return err
		}
		return r.Patch(ctx, set, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	})
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "ApplicationSet handoff failed", "applicationSet", r.Key)
		if r.Recorder != nil {
			obj := ApplicationSetObject()
			obj.SetName(r.Key.Name)
			obj.SetNamespace(r.Key.Namespace)
			r.Recorder.Eventf(obj, nil, corev1.EventTypeWarning, "HandoffFailed", "Handoff", "Cannot update publication inventory; Git publication remains independent.")
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
func (r *ApplicationSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}
	enqueue := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: r.Key}}
	})
	startup := make(chan event.GenericEvent, 1)
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		select {
		case startup <- event.GenericEvent{Object: ApplicationSetObject()}:
		case <-ctx.Done():
		}
		<-ctx.Done()
		return nil
	})); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).Named("applicationset-inventory").
		Watches(ApplicationSetObject(), enqueue).Watches(&api.GitResource{}, enqueue).
		WatchesRawSource(source.Channel(startup, enqueue)).Complete(r)
}
