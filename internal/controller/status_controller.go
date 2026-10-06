package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"time"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var ApplicationGVK = schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Application"}

func ApplicationObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(ApplicationGVK)
	return o
}

const SourceAnnotation = "gitops.example.io/source"
const UIDAnnotation = "gitops.example.io/source-uid"
const SetAnnotation = "gitops.example.io/applicationset"

type StatusReconciler struct {
	client.Client
	Reader       client.Reader
	Mapper       meta.RESTMapper
	SetKey       types.NamespacedName
	PollInterval time.Duration
}

// +kubebuilder:rbac:groups=argoproj.io,resources=applications,verbs=get;list;watch,namespace=argocd
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get
// +kubebuilder:rbac:groups=testing.gitops.example.io,resources=statusobjects,verbs=get

func observationError(err error) api.Observation {
	switch {
	case apierrors.IsNotFound(err):
		return api.Observation{Reason: "NotFound", Message: "Referenced object was not found."}
	case apierrors.IsForbidden(err):
		return api.Observation{Reason: "Forbidden", Message: "Controller lacks permission to read the referenced object."}
	default:
		return api.Observation{Reason: "ReadFailed", Message: "Cannot read the referenced object; retrying."}
	}
}
func copyObjectField(o map[string]interface{}, key string) (*apiext.JSON, error) {
	value, exists := o[key]
	if !exists {
		return nil, nil
	}
	if _, ok := value.(map[string]interface{}); !ok {
		return nil, fmt.Errorf("%s must be a JSON object", key)
	}
	raw, err := json.Marshal(value)
	return &apiext.JSON{Raw: raw}, err
}
func appStable(old, next *api.ApplicationSnapshot) {
	if old == nil {
		if next.Observation.Reason != "Observed" {
			next.LastUpdatedAt = nil
		}
		return
	}
	a, b := old.DeepCopy(), next.DeepCopy()
	a.ResourceVersion = ""
	b.ResourceVersion = ""
	a.Generation = nil
	b.Generation = nil
	a.LastUpdatedAt = nil
	b.LastUpdatedAt = nil
	if reflect.DeepEqual(a, b) {
		next.Generation = old.Generation
		next.ResourceVersion = old.ResourceVersion
		next.LastUpdatedAt = old.LastUpdatedAt
	}
	if next.Observation.Reason != "Observed" {
		next.LastUpdatedAt = old.LastUpdatedAt
	}
}
func resourceStable(old, next *api.ResourceSnapshot) {
	if old == nil {
		if next.Observation.Reason != "Observed" {
			next.LastUpdatedAt = nil
		}
		return
	}
	a, b := old.DeepCopy(), next.DeepCopy()
	a.ResourceVersion = ""
	b.ResourceVersion = ""
	a.LastUpdatedAt = nil
	b.LastUpdatedAt = nil
	if reflect.DeepEqual(a, b) {
		next.ResourceVersion = old.ResourceVersion
		next.LastUpdatedAt = old.LastUpdatedAt
	}
	if next.Observation.Reason != "Observed" {
		next.LastUpdatedAt = old.LastUpdatedAt
	}
}
func applicationRef(cr *api.GitResource, namespace string) api.ApplicationReference {
	if cr.Status.ApplicationRef != nil {
		return *cr.Status.ApplicationRef
	}
	return api.ApplicationReference{Namespace: namespace, Name: ApplicationName(cr)}
}
func (r *StatusReconciler) tracked(ctx context.Context, cr *api.GitResource, app *unstructured.Unstructured) bool {
	annotations := app.GetAnnotations()
	if annotations[UIDAnnotation] != "" {
		return annotations[UIDAnnotation] == string(cr.UID) && annotations[SourceAnnotation] == cr.Namespace+"/"+cr.Name && annotations[SetAnnotation] == r.SetKey.Namespace+"/"+r.SetKey.Name
	}
	// Continuity for v0.1 Applications while the template upgrade propagates.
	set := ApplicationSetObject()
	if r.Reader.Get(ctx, r.SetKey, set) != nil {
		return false
	}
	owned := false
	for _, owner := range app.GetOwnerReferences() {
		if owner.UID == set.GetUID() && owner.Kind == "ApplicationSet" {
			owned = true
		}
	}
	if !owned {
		return false
	}
	entries, err := listElements(set)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if stringField(entry, "uid") == string(cr.UID) && stringField(entry, "applicationName") == app.GetName() && entryMatches(entry, cr) {
			return cr.Status.LastPublishedRevision != ""
		}
	}
	return false
}
func (r *StatusReconciler) Observe(ctx context.Context, cr *api.GitResource) (*api.ApplicationSnapshot, *api.ResourceSnapshot) {
	if !cr.DeletionTimestamp.IsZero() && cr.Status.Cleanup != nil {
		return r.observeCleanup(ctx, cr)
	}
	now := metav1.Now()
	a := &api.ApplicationSnapshot{LastUpdatedAt: &now, Observation: api.Observation{Reason: "Observed"}}
	ref := api.ResourceReference{}
	if cr.Status.PublishedResourceRef != nil {
		ref = *cr.Status.PublishedResourceRef
	}
	lastRef := ref
	// Keep the last exact reference during an Application read failure; absence of
	// an Application does not establish absence of its target.
	if old := cr.Status.Resource; old != nil && old.Ref.Name == ref.Name && old.Ref.Kind == ref.Kind {
		oldGV, e1 := schema.ParseGroupVersion(old.Ref.APIVersion)
		newGV, e2 := schema.ParseGroupVersion(ref.APIVersion)
		if e1 == nil && e2 == nil && oldGV.Group == newGV.Group && (old.Ref.Namespace == ref.Namespace || ref.Namespace == "" && old.Ref.Namespace == cr.Namespace || old.Ref.Namespace == "") {
			lastRef = old.Ref
		}
	}
	resource := &api.ResourceSnapshot{Ref: lastRef, LastUpdatedAt: &now, Observation: api.Observation{Reason: "NotTracked", Message: "No exact published object is tracked by the Application."}}
	app := ApplicationObject()
	appRef := applicationRef(cr, r.SetKey.Namespace)
	if appRef.Namespace != r.SetKey.Namespace {
		a.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Application must be in the configured Argo namespace."}
		return a, resource
	}
	if err := r.Reader.Get(ctx, types.NamespacedName{Namespace: appRef.Namespace, Name: appRef.Name}, app); err != nil {
		a.Observation = observationError(err)
		if cr.Status.ArgoCD != nil && !apierrors.IsNotFound(err) {
			a.UID = cr.Status.ArgoCD.UID
		}
		return a, resource
	}
	if !r.tracked(ctx, cr, app) {
		a.Observation = api.Observation{Reason: "NotTracked", Message: "Application tracking identity does not match this GitResource."}
		return a, resource
	}
	a.UID = string(app.GetUID())
	a.ResourceVersion = app.GetResourceVersion()
	if gen, found, _ := unstructured.NestedInt64(app.Object, "metadata", "generation"); found {
		a.Generation = &gen
	}
	a.DeletionTimestamp = app.GetDeletionTimestamp()
	var err error
	spec, _ := app.Object["spec"].(map[string]interface{})
	a.Source, err = copyObjectField(spec, "source")
	if err != nil {
		a.Observation = api.Observation{Reason: "InvalidStatus", Message: err.Error()}
		return a, resource
	}
	a.Destination, err = copyObjectField(spec, "destination")
	if err != nil {
		a.Observation = api.Observation{Reason: "InvalidStatus", Message: err.Error()}
		return a, resource
	}
	if sources, found, _ := unstructured.NestedSlice(app.Object, "spec", "sources"); found && len(sources) > 0 || textAt(rawObject(a.Destination), "server") != "https://kubernetes.default.svc" {
		a.Source = nil
		a.Destination = nil
		a.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Only one directory source and the local Kubernetes destination are supported."}
		return a, resource
	}
	a.Status, err = copyObjectField(app.Object, "status")
	if err != nil {
		a.Observation = api.Observation{Reason: "InvalidStatus", Message: err.Error()}
		return a, resource
	}
	resource.ObservedAgainstRevision = textAt(rawObject(a.Status), "sync", "revision")
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return a, resource
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		resource.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Invalid published API version."}
		return a, resource
	}
	if gv.Group == api.GroupVersion.Group && (ref.Kind == "GitResource" || ref.Kind == "ClusterGitConfig") || gv.Group == ApplicationGVK.Group && ref.Kind == "Application" && ref.Name == appRef.Name && (ref.Namespace == "" || ref.Namespace == appRef.Namespace) {
		resource.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Recursive observation of controller status is unsupported."}
		return a, resource
	}
	mapping, err := r.mapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind})
	if err != nil {
		resource.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Published kind is not available through API discovery."}
		return a, resource
	}
	namespace := ref.Namespace
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		if namespace == "" {
			namespace = textAt(rawObject(a.Destination), "namespace")
		}
	} else {
		namespace = ""
	}
	inventory, _, _ := unstructured.NestedSlice(app.Object, "status", "resources")
	matches := []map[string]interface{}{}
	for _, entry := range inventory {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if item["hook"] == true || textAt(item, "kind") != ref.Kind || textAt(item, "group") != gv.Group || textAt(item, "name") != ref.Name || textAt(item, "namespace") != namespace {
			continue
		}
		matches = append(matches, item)
	}
	if len(matches) != 1 {
		if len(matches) > 1 {
			resource.Observation = api.Observation{Reason: "ReadFailed", Message: "Application inventory has multiple ambiguous matches."}
		}
		return a, resource
	}
	version := textAt(matches[0], "version")
	if version == "" {
		version = gv.Version
	}
	mapping, err = r.mapping(schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, version)
	if err != nil {
		resource.Observation = api.Observation{Reason: "UnsupportedTarget", Message: "Inventory version is not served."}
		return a, resource
	}
	resource.Ref.APIVersion = schema.GroupVersion{Group: gv.Group, Version: version}.String()
	resource.Ref.Namespace = namespace
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(mapping.GroupVersionKind)
	if err = r.Reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, live); err != nil {
		resource.Observation = observationError(err)
		if apierrors.IsNotFound(err) {
			exists := false
			resource.Exists = &exists
			resource.Ref.UID = ""
		} else if old := cr.Status.Resource; old != nil && old.Ref.APIVersion == resource.Ref.APIVersion && old.Ref.Name == ref.Name && old.Ref.Namespace == namespace {
			resource.Ref.UID = old.Ref.UID
		}
		return a, resource
	}
	exists := true
	resource.Exists = &exists
	resource.Ref.UID = string(live.GetUID())
	resource.ResourceVersion = live.GetResourceVersion()
	resource.DeletionTimestamp = live.GetDeletionTimestamp()
	if generation, found, _ := unstructured.NestedInt64(live.Object, "metadata", "generation"); found {
		resource.Generation = &generation
	}
	resource.Observation = api.Observation{Reason: "Observed"}
	resource.Status, err = copyObjectField(live.Object, "status")
	if err != nil {
		resource.Status = nil
		resource.Observation = api.Observation{Reason: "InvalidStatus", Message: err.Error()}
	}
	return a, resource
}
func (r *StatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cr := &api.GitResource{}
	if err := r.Reader.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	a, resource := r.Observe(ctx, cr)
	prospective := cr.DeepCopy()
	prospective.Status.ArgoCD = a
	prospective.Status.Resource = resource
	limitMirrors(prospective)
	appStable(cr.Status.ArgoCD, a)
	resourceStable(cr.Status.Resource, resource)
	observationCounter.WithLabelValues(resource.Observation.Reason).Inc()
	err := patchStatus(ctx, r.Client, r.Reader, cr, "observer", func(current *api.GitResource) {
		if current.Generation != cr.Generation || current.Status.LastPublishedRevision != cr.Status.LastPublishedRevision || !reflect.DeepEqual(current.Status.PublishedResourceRef, cr.Status.PublishedResourceRef) {
			return
		}
		current.Status.ArgoCD = a
		current.Status.Resource = resource
	})
	period := r.PollInterval
	if period == 0 {
		period = 15 * time.Second
	}
	return ctrl.Result{RequeueAfter: period + time.Duration(rand.IntN(1000))*time.Millisecond}, err
}
func (r *StatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Reader == nil {
		r.Reader = mgr.GetAPIReader()
	}
	if r.Mapper == nil {
		r.Mapper = mgr.GetRESTMapper()
	}
	events := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		annotations := obj.GetAnnotations()
		source := annotations[SourceAnnotation]
		parts := stringsSplitSource(source)
		if len(parts) != 2 {
			return nil
		}
		cr := &api.GitResource{}
		key := types.NamespacedName{Namespace: parts[0], Name: parts[1]}
		if r.Reader.Get(ctx, key, cr) != nil || string(cr.UID) != annotations[UIDAnnotation] {
			return nil
		}
		return []reconcile.Request{{NamespacedName: key}}
	})
	return ctrl.NewControllerManagedBy(mgr).Named("gitresource-status").For(&api.GitResource{}, builder.WithPredicates(observerPredicate)).Watches(ApplicationObject(), events, builder.WithPredicates(applicationPredicate)).Complete(r)
}
func stringsSplitSource(source string) []string {
	for i, c := range source {
		if c == '/' && i > 0 && i < len(source)-1 {
			return []string{source[:i], source[i+1:]}
		}
	}
	return nil
}

// Refresh discovery for both the published and inventory-declared served version.
func (r *StatusReconciler) mapping(kind schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	mapping, err := r.Mapper.RESTMapping(kind, versions...)
	if meta.IsNoMatchError(err) {
		meta.MaybeResetRESTMapper(r.Mapper)
		return r.Mapper.RESTMapping(kind, versions...)
	}
	return mapping, err
}

func (r *StatusReconciler) observeCleanup(ctx context.Context, cr *api.GitResource) (*api.ApplicationSnapshot, *api.ResourceSnapshot) {
	now := metav1.Now()
	a := &api.ApplicationSnapshot{LastUpdatedAt: &now, Observation: api.Observation{Reason: "Observed"}}
	app := ApplicationObject()
	ref := applicationRef(cr, r.SetKey.Namespace)
	err := r.Reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, app)
	if err != nil {
		a.Observation = observationError(err)
	} else if cr.Status.Cleanup.ApplicationUID != "" && cr.Status.Cleanup.ApplicationUID != string(app.GetUID()) || app.GetAnnotations()[UIDAnnotation] != "" && app.GetAnnotations()[UIDAnnotation] != string(cr.UID) {
		a.Observation = api.Observation{Reason: "IdentityConflict", Message: "A replacement Application occupies the cleanup identity."}
	} else {
		a.UID, a.ResourceVersion, a.DeletionTimestamp = string(app.GetUID()), app.GetResourceVersion(), app.GetDeletionTimestamp()
		generation := app.GetGeneration()
		a.Generation = &generation
		spec, _ := app.Object["spec"].(map[string]interface{})
		a.Source, err = copyObjectField(spec, "source")
		if err == nil {
			a.Destination, err = copyObjectField(spec, "destination")
		}
		if err == nil {
			a.Status, err = copyObjectField(app.Object, "status")
		}
		if err != nil {
			a.Observation = api.Observation{Reason: "InvalidStatus", Message: err.Error()}
		}
	}
	target := cr.Status.Cleanup.ResourceRef
	if target == nil {
		target = cr.Status.PublishedResourceRef
	}
	if target == nil {
		return a, &api.ResourceSnapshot{Observation: api.Observation{Reason: "NotTracked", Message: "No published cleanup reference is available."}}
	}
	resource := observeExact(ctx, r.Reader, r.Mapper, *target, cr.Namespace)
	resource.ObservedAgainstRevision = cr.Status.LastPublishedRevision
	return a, resource
}
