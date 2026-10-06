package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	apiext "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	PausedAnnotation = "gitops.example.io/paused"
	AdoptAnnotation  = "gitops.example.io/adopt-existing"
	MirrorLimit      = 128 * 1024
	ObjectLimit      = 768 * 1024
)

func paused(cr *api.GitResource) bool   { return cr.Annotations[PausedAnnotation] == "true" }
func adopting(cr *api.GitResource) bool { return cr.Annotations[AdoptAnnotation] == "true" }
func rawObject(raw *apiext.JSON) map[string]interface{} {
	if raw == nil {
		return nil
	}
	var obj map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw.Raw))
	decoder.UseNumber()
	if decoder.Decode(&obj) != nil {
		return nil
	}
	return obj
}
func nested(obj map[string]interface{}, keys ...string) interface{} {
	var value interface{} = obj
	for _, key := range keys {
		m, ok := value.(map[string]interface{})
		if !ok {
			return nil
		}
		value = m[key]
	}
	return value
}
func textAt(obj map[string]interface{}, keys ...string) string {
	s, _ := nested(obj, keys...).(string)
	return s
}
func bounded(s string) string {
	if len(s) > 1024 {
		return strings.ToValidUTF8(s[:1024], "")
	}
	return s
}

var conditionReason = regexp.MustCompile(`^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`)

func condition(cr *api.GitResource, kind string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{Type: kind, Status: status, Reason: reason, Message: bounded(message), ObservedGeneration: cr.Generation})
}

// Only the fields owned by our directory template participate in correlation.
func sourceMatches(source map[string]interface{}, cr *api.GitResource) (bool, bool) {
	wanted := map[string]string{"repoURL": cr.Spec.Repository.URL, "targetRevision": cr.Status.LastPublishedRevision, "path": path.Dir(cr.Spec.Repository.Path)}
	complete := true
	for key, want := range wanted {
		actual, ok := source[key].(string)
		if !ok {
			complete = false
			continue
		}
		if actual != want {
			return false, true
		}
	}
	include := textAt(source, "directory", "include")
	if include == "" {
		complete = false
	} else if include != path.Base(cr.Spec.Repository.Path) {
		return false, true
	}
	if value := nested(source, "directory", "recurse"); value != nil && value != false {
		return false, true
	}
	return complete, complete
}
func destinationMatches(destination map[string]interface{}, cr *api.GitResource) (bool, bool) {
	server := textAt(destination, "server")
	namespace := textAt(destination, "namespace")
	if (server != "" && server != "https://kubernetes.default.svc") || (namespace != "" && namespace != cr.Namespace) {
		return false, true
	}
	if server == "" || namespace == "" {
		return false, false
	}
	return true, true
}
func publicationCurrent(cr *api.GitResource) bool {
	p := meta.FindStatusCondition(cr.Status.Conditions, "Published")
	return p != nil && p.Status == metav1.ConditionTrue && p.ObservedGeneration == cr.Generation && cr.Status.LastPublishedGeneration == cr.Generation && cr.Status.LastPublishedRevision != ""
}
func syncDecision(cr *api.GitResource) (metav1.ConditionStatus, string, string) {
	if !publicationCurrent(cr) {
		return metav1.ConditionUnknown, "PublishPending", "Desired generation has not been published."
	}
	a := cr.Status.ArgoCD
	if a == nil {
		return metav1.ConditionUnknown, "NotFound", "Application has not been observed."
	}
	if a.Observation.Reason != "Observed" {
		return metav1.ConditionUnknown, a.Observation.Reason, a.Observation.Message
	}
	if a.DeletionTimestamp != nil {
		return metav1.ConditionFalse, "ApplicationDeleting", "Application is deleting."
	}
	status := rawObject(a.Status)
	pairs := [][2]bool{}
	match, complete := sourceMatches(rawObject(a.Source), cr)
	pairs = append(pairs, [2]bool{match, complete})
	match, complete = destinationMatches(rawObject(a.Destination), cr)
	pairs = append(pairs, [2]bool{match, complete})
	comparedSource, _ := nested(status, "sync", "comparedTo", "source").(map[string]interface{})
	comparedDestination, _ := nested(status, "sync", "comparedTo", "destination").(map[string]interface{})
	match, complete = sourceMatches(comparedSource, cr)
	pairs = append(pairs, [2]bool{match, complete})
	match, complete = destinationMatches(comparedDestination, cr)
	pairs = append(pairs, [2]bool{match, complete})
	missing := false
	for _, pair := range pairs {
		if !pair[0] && pair[1] {
			return metav1.ConditionFalse, "RevisionPending", "Argo source or comparison does not match the published revision."
		}
		missing = missing || !pair[1]
	}
	revision := textAt(status, "sync", "revision")
	if revision != "" && revision != cr.Status.LastPublishedRevision {
		return metav1.ConditionFalse, "RevisionPending", "Argo has compared another revision."
	}
	if missing || revision == "" {
		return metav1.ConditionUnknown, "RevisionPending", "Argo has not reported a complete comparison for this publication."
	}
	switch textAt(status, "sync", "status") {
	case "Synced":
		return metav1.ConditionTrue, "Synced", "Argo reports the expected source and revision synchronized."
	case "OutOfSync":
		return metav1.ConditionFalse, "OutOfSync", "Argo reports this publication out of sync."
	default:
		return metav1.ConditionUnknown, "RevisionPending", "Argo sync status is not yet known."
	}
}
func readyDecision(cr *api.GitResource, synced metav1.ConditionStatus, reason, message string) (metav1.ConditionStatus, string, string) {
	if paused(cr) {
		return metav1.ConditionFalse, "ReconcilePaused", "Mutations are paused; existing deployment and observations continue."
	}
	if !cr.DeletionTimestamp.IsZero() {
		p := meta.FindStatusCondition(cr.Status.Conditions, "Published")
		msg := "Cleanup is pending."
		if p != nil && p.Reason == "DeleteFailed" {
			msg = p.Message
		}
		return metav1.ConditionFalse, "Deleting", msg
	}
	if !publicationCurrent(cr) {
		p := meta.FindStatusCondition(cr.Status.Conditions, "Published")
		if p != nil && p.Status == metav1.ConditionFalse && p.ObservedGeneration == cr.Generation {
			return metav1.ConditionFalse, p.Reason, p.Message
		}
		return metav1.ConditionFalse, "PublishPending", "Desired generation has not been published."
	}
	if cr.Status.ArgoCD != nil && cr.Status.ArgoCD.Observation.Reason != "Observed" {
		return metav1.ConditionUnknown, cr.Status.ArgoCD.Observation.Reason, cr.Status.ArgoCD.Observation.Message
	}
	resource := cr.Status.Resource
	if resource != nil && resource.Observation.Reason != "Observed" && resource.Observation.Reason != "NotFound" {
		return metav1.ConditionUnknown, resource.Observation.Reason, resource.Observation.Message
	}
	if synced != metav1.ConditionTrue {
		a := cr.Status.ArgoCD
		if a != nil && textAt(rawObject(a.Status), "operationState", "phase") == "Running" && textAt(rawObject(a.Status), "operationState", "operation", "sync", "revision") == cr.Status.LastPublishedRevision {
			return metav1.ConditionFalse, "Syncing", "Argo is running a sync for the pending publication."
		}
		return synced, reason, message
	}
	if resource == nil || resource.Exists == nil {
		return metav1.ConditionUnknown, "NotTracked", "The exact published object has not been observed."
	}
	if !*resource.Exists {
		return metav1.ConditionFalse, "ResourceMissing", "The tracked object is absent."
	}
	if resource.DeletionTimestamp != nil {
		return metav1.ConditionFalse, "ResourceDeleting", "The tracked object is deleting."
	}
	if value, present, why, msg := targetReady(resource); present && value != metav1.ConditionTrue {
		return value, why, msg
	}
	switch textAt(rawObject(cr.Status.ArgoCD.Status), "health", "status") {
	case "Healthy":
		return metav1.ConditionTrue, "Healthy", "Published revision is synchronized and Argo reports Healthy."
	case "Degraded", "Missing", "Progressing", "Suspended":
		h := textAt(rawObject(cr.Status.ArgoCD.Status), "health", "status")
		return metav1.ConditionFalse, h, textAt(rawObject(cr.Status.ArgoCD.Status), "health", "message")
	default:
		return metav1.ConditionUnknown, "HealthUnknown", "Argo health is absent, unknown or unsupported."
	}
}
func targetReady(resource *api.ResourceSnapshot) (metav1.ConditionStatus, bool, string, string) {
	raw := rawObject(resource.Status)
	value := raw["conditions"]
	if value == nil {
		return "", false, "", ""
	}
	list, ok := value.([]interface{})
	if !ok {
		return metav1.ConditionUnknown, true, "InvalidStatus", "Target conditions are malformed."
	}
	var ready map[string]interface{}
	for _, item := range list {
		c, ok := item.(map[string]interface{})
		if !ok {
			return metav1.ConditionUnknown, true, "InvalidStatus", "Target conditions are malformed."
		}
		if c["type"] == "Ready" {
			if ready != nil {
				return metav1.ConditionUnknown, true, "InvalidStatus", "Target has ambiguous Ready conditions."
			}
			ready = c
		}
	}
	if ready == nil {
		return "", false, "", ""
	}
	if gen, exists := ready["observedGeneration"]; exists {
		number, ok := gen.(json.Number)
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		if !ok || err != nil {
			return metav1.ConditionUnknown, true, "InvalidStatus", "Target Ready observedGeneration is malformed."
		}
		if resource.Generation != nil && parsed != *resource.Generation {
			return metav1.ConditionUnknown, true, "StaleResourceStatus", "Target Ready does not describe its current generation."
		}
	}
	for _, field := range []string{"reason", "message"} {
		if v, present := ready[field]; present {
			if _, ok := v.(string); !ok {
				return metav1.ConditionUnknown, true, "InvalidStatus", "Target Ready " + field + " is malformed."
			}
		}
	}
	status, ok := ready["status"].(string)
	reason, _ := ready["reason"].(string)
	message, _ := ready["message"].(string)
	if !ok || (status != "True" && status != "False" && status != "Unknown") {
		return metav1.ConditionUnknown, true, "InvalidStatus", "Target Ready status is malformed."
	}
	if reason == "" {
		reason = "ResourceNotReady"
	} else if len(reason) > 1024 || !conditionReason.MatchString(reason) {
		message = reason + ": " + message
		reason = "ResourceNotReady"
	}
	return metav1.ConditionStatus(status), true, reason, message
}
func summarize(cr *api.GitResource) {
	synced, reason, message := syncDecision(cr)
	condition(cr, "Synced", synced, reason, message)
	value, why, msg := readyDecision(cr, synced, reason, message)
	condition(cr, "Ready", value, why, msg)
}
func encodedSize(value interface{}) int { data, _ := json.Marshal(value); return len(data) }
func omitMirrors(cr *api.GitResource, message string) {
	if a := cr.Status.ArgoCD; a != nil {
		a.Source = nil
		a.Destination = nil
		a.Status = nil
		a.Observation = api.Observation{Reason: "SnapshotTooLarge", Message: message}
	}
	if r := cr.Status.Resource; r != nil {
		r.Status = nil
		r.Observation = api.Observation{Reason: "SnapshotTooLarge", Message: message}
	}
}
func limitMirrors(cr *api.GitResource) {
	if a := cr.Status.ArgoCD; a != nil {
		if size := encodedSize(a); size > MirrorLimit {
			a.Source = nil
			a.Destination = nil
			a.Status = nil
			a.Observation = api.Observation{Reason: "SnapshotTooLarge", Message: fmt.Sprintf("Application mirror is %d bytes; limit is %d.", size, MirrorLimit)}
		}
	}
	if r := cr.Status.Resource; r != nil {
		if size := encodedSize(r); size > MirrorLimit {
			r.Status = nil
			r.Observation = api.Observation{Reason: "SnapshotTooLarge", Message: fmt.Sprintf("Resource mirror is %d bytes; limit is %d.", size, MirrorLimit)}
		}
	}
	if size := encodedSize(cr); size > ObjectLimit {
		omitMirrors(cr, fmt.Sprintf("Prospective GitResource is %d bytes; soft limit is %d.", size, ObjectLimit))
	}
}

// Every writer merges with the latest API object. Mutators own only their fields.
func patchStatus(ctx context.Context, c client.Client, reader client.Reader, processed *api.GitResource, writer string, mutate func(*api.GitResource)) error {
	fallback := false
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &api.GitResource{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(processed), current); err != nil {
			return client.IgnoreNotFound(err)
		}
		if current.UID != processed.UID {
			return nil
		}
		before := current.DeepCopy()
		mutate(current)
		limitMirrors(current)
		if fallback {
			omitMirrors(current, "API rejected the snapshot size; raw data remains available at the source references.")
		}
		summarize(current)
		if reflect.DeepEqual(before.Status, current.Status) {
			return nil
		}
		err := c.Status().Patch(ctx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
		if apierrors.IsRequestEntityTooLargeError(err) && !fallback {
			fallback = true
			// Re-enter through the same fresh-read conflict path once with small diagnostics.
			return apierrors.NewConflict(api.GroupVersion.WithResource("gitresources").GroupResource(), current.Name, fmt.Errorf("retry without oversized snapshots"))
		}
		statusPatchCounter.WithLabelValues(writer, outcome(err)).Inc()
		return err
	})
}
