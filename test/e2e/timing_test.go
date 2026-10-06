//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/inelson/git-state-controller/api/v1alpha1"
	"github.com/inelson/git-state-controller/internal/controller"
	"github.com/inelson/git-state-controller/internal/manifest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type caseTiming struct {
	StatusWrites                                 int
	GitOperations                                []map[string]interface{}
	RejectedPushRetries                          int
	Name                                         string `json:"name"`
	CreateStarted, CreatePublished, CreateSynced time.Time
	UpdateStarted, UpdatePublished, UpdateSynced time.Time
	DeleteStarted, CRRemoved, DownstreamDeleted  time.Time
	ApplicationBytes, ResourceBytes              int
}

func distribution(values []float64) map[string]interface{} {
	sort.Float64s(values)
	result := map[string]interface{}{"count": len(values)}
	if len(values) > 0 {
		for key, p := range map[string]float64{"p50": .5, "p95": .95, "max": 1} {
			result[key] = values[int(math.Ceil(float64(len(values))*p))-1]
		}
	}
	return result
}
func (s *stack) commandOutput(t *testing.T, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(s.root, "bin/kubectl"), append([]string{"--kubeconfig", filepath.Join(s.root, ".dev/kubeconfig"), "--context", "k3d-git-state-dev"}, args...)...)
	cmd.Env = append(os.Environ(), "KUBECTL_KUBERC=false")
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("measurement command failed: %v", err)
	}
	return b
}
func (s *stack) metrics(t *testing.T) (map[string]float64, []byte) {
	t.Helper()
	pods := &corev1.PodList{}
	if err := s.List(context.Background(), pods, client.InNamespace("git-state-system"), client.MatchingLabels{"app": "git-state-controller"}); err != nil {
		t.Fatal(err)
	}
	name := ""
	for _, p := range pods.Items {
		if p.DeletionTimestamp.IsZero() && p.Status.Phase == corev1.PodRunning {
			name = p.Name
		}
	}
	if name == "" {
		t.Fatal("manager pod missing for metrics")
	}
	raw := s.commandOutput(t, "get", "--raw", fmt.Sprintf("/api/v1/namespaces/git-state-system/pods/%s:8080/proxy/metrics", name))
	values := map[string]float64{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, e := strconv.ParseFloat(fields[1], 64)
		if e != nil {
			continue
		}
		name := strings.Split(fields[0], "{")[0]
		values[name] += v
		if fields[0] != name {
			values[fields[0]] = v
		}
	}
	return values, raw
}
func metricDeltas(before, after map[string]float64, prefix string) map[string]float64 {
	result := map[string]float64{}
	for key, value := range after {
		if strings.HasPrefix(key, prefix+"{") {
			result[key] = value - before[key]
		}
	}
	return result
}

func configurationPatches(before, after map[string]float64) float64 {
	var count float64
	for key, value := range metricDeltas(before, after, "git_state_status_patches_total") {
		if strings.Contains(key, `writer="config"`) {
			count += value
		}
	}
	return count
}

func (s *stack) measureTwenty(t *testing.T) {
	t.Helper()
	start := time.Now()
	before, _ := s.metrics(t)
	cases := map[string]*caseTiming{}
	crs := []*api.GitResource{}
	watchCtx, watchCancel := context.WithCancel(context.Background())
	defer watchCancel()
	stream, err := s.watcher.Watch(watchCtx, &api.GitResourceList{}, client.InNamespace(s.namespace))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Stop()
	configs := &api.ClusterGitConfigList{}
	if err := s.List(watchCtx, configs); err != nil {
		t.Fatal(err)
	}
	configStream, err := s.watcher.Watch(watchCtx, &api.ClusterGitConfigList{}, &client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: configs.ResourceVersion}})
	if err != nil {
		t.Fatal(err)
	}
	defer configStream.Stop()
	configWrites := 0
	configDone := make(chan struct{})
	go func() {
		defer close(configDone)
		previous := map[string][]byte{}
		for _, config := range configs.Items {
			previous[config.Name], _ = json.Marshal(config.Status)
		}
		for event := range configStream.ResultChan() {
			config, ok := event.Object.(*api.ClusterGitConfig)
			if !ok {
				continue
			}
			raw, _ := json.Marshal(config.Status)
			if !bytes.Equal(previous[config.Name], raw) {
				configWrites++
			}
			previous[config.Name] = raw
		}
		if watchCtx.Err() == nil {
			t.Error("configuration measurement watch closed before completion")
		}
	}()
	writes := map[string]int{}
	previous := map[string][]byte{}
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if watchCtx.Err() == nil {
				t.Error("measurement watch closed before completion")
			}
		}()
		for event := range stream.ResultChan() {
			cr, ok := event.Object.(*api.GitResource)
			if !ok {
				continue
			}
			raw, _ := json.Marshal(cr.Status)
			mu.Lock()
			if !bytes.Equal(previous[cr.Name], raw) && string(raw) != "{}" {
				writes[cr.Name]++
			}
			previous[cr.Name] = raw
			mu.Unlock()
		}
	}()

	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("concurrent-%02d", i)
		cases[name] = &caseTiming{Name: name, CreateStarted: time.Now()}
		crs = append(crs, s.create(t, name))
	}
	observe := func(update bool) {
		poll(t, "twenty current publications and observed syncs", func(ctx context.Context) bool {
			list := &api.GitResourceList{}
			if s.List(ctx, list, client.InNamespace(s.namespace)) != nil {
				return false
			}
			complete := 0
			for _, cr := range list.Items {
				timing, ok := cases[cr.Name]
				if !ok {
					continue
				}
				published, synced := &timing.CreatePublished, &timing.CreateSynced
				if update {
					published, synced = &timing.UpdatePublished, &timing.UpdateSynced
				}
				if cr.Status.LastPublishedGeneration == cr.Generation && meta.IsStatusConditionTrue(cr.Status.Conditions, "Published") && published.IsZero() {
					*published = time.Now()
				}
				ready := meta.FindStatusCondition(cr.Status.Conditions, "Ready")
				if ready != nil && ready.Status == "True" && ready.ObservedGeneration == cr.Generation && synced.IsZero() {
					*synced = time.Now()
				}
				if !synced.IsZero() {
					complete++
				}
				if cr.Status.ArgoCD != nil {
					b, _ := json.Marshal(cr.Status.ArgoCD)
					timing.ApplicationBytes = len(b)
				}
				if cr.Status.Resource != nil {
					b, _ := json.Marshal(cr.Status.Resource)
					timing.ResourceBytes = len(b)
				}
			}
			return complete == 20
		})
	}
	observe(false)
	repo := s.clone(t)
	head, _ := repo.Head()
	for _, cr := range crs {
		want, _, _ := manifest.RenderManaged(cr.Spec.Manifest.Raw, cr.Namespace, cr.Name, string(cr.UID), "managed")
		got, err := fileAt(repo, head.Hash().String(), cr.Spec.Repository.Path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("concurrent managed file lost", cr.Name)
		}
	}
	if _, err := fileAt(repo, head.Hash().String(), "README.md"); err != nil {
		t.Fatal("unrelated repository file lost")
	}
	for _, cr := range crs {
		cases[cr.Name].UpdateStarted = time.Now()
		s.edit(t, cr, func(current *api.GitResource) { changeValue(current, "timed update") })
	}
	observe(true)
	for _, cr := range crs {
		cases[cr.Name].DeleteStarted = time.Now()
		if err := s.Delete(context.Background(), cr); err != nil {
			t.Fatal(err)
		}
	}
	poll(t, "twenty CR removals and downstream deletions", func(ctx context.Context) bool {
		done := 0
		for _, cr := range crs {
			timing := cases[cr.Name]
			if timing.CRRemoved.IsZero() && apierrors.IsNotFound(s.Get(ctx, client.ObjectKeyFromObject(cr), &api.GitResource{})) {
				timing.CRRemoved = time.Now()
			}
			if !timing.CRRemoved.IsZero() && timing.DownstreamDeleted.IsZero() && apierrors.IsNotFound(s.Get(ctx, types.NamespacedName{Namespace: "argocd", Name: controller.ApplicationName(cr)}, appObject())) && apierrors.IsNotFound(s.Get(ctx, types.NamespacedName{Namespace: s.namespace, Name: cr.Name}, &corev1.ConfigMap{})) {
				timing.DownstreamDeleted = time.Now()
			}
			if !timing.DownstreamDeleted.IsZero() {
				done++
			}
		}
		return done == 20
	})
	after, metricRaw := s.metrics(t)
	logs := s.commandOutput(t, "logs", "-n", "git-state-system", "deployment/controller-manager", "--since-time", start.UTC().Format(time.RFC3339))
	gitOps := []map[string]interface{}{}
	retries := 0
	gitGroups := map[string][]float64{}
	for _, line := range strings.Split(string(logs), "\n") {
		var entry map[string]interface{}
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		name, _ := entry["name"].(string)
		timing, ok := cases[name]
		if !ok || entry["namespace"] != s.namespace {
			continue
		}
		if entry["msg"] == "Git push requires retry" {
			timing.RejectedPushRetries++
			retries++
		}
		if entry["msg"] == "Git operation" {
			gitOps = append(gitOps, entry)
			timing.GitOperations = append(timing.GitOperations, entry)
			duration, _ := entry["durationSeconds"].(float64)
			key := fmt.Sprint(entry["operation"], "/", entry["outcome"])
			gitGroups[key] = append(gitGroups[key], duration)
		}
	}
	watchCancel()
	stream.Stop()
	configStream.Stop()
	<-done
	<-configDone
	mu.Lock()
	for name, timing := range cases {
		timing.StatusWrites = writes[name]
	}
	mu.Unlock()
	raw := []*caseTiming{}
	measurements := map[string][]float64{}
	lastPublication := start
	for _, cr := range crs {
		v := cases[cr.Name]
		raw = append(raw, v)
		measurements["create_to_publication_seconds"] = append(measurements["create_to_publication_seconds"], v.CreatePublished.Sub(v.CreateStarted).Seconds())
		measurements["publication_to_observed_sync_seconds"] = append(measurements["publication_to_observed_sync_seconds"], v.CreateSynced.Sub(v.CreatePublished).Seconds())
		measurements["update_to_publication_seconds"] = append(measurements["update_to_publication_seconds"], v.UpdatePublished.Sub(v.UpdateStarted).Seconds())
		measurements["updated_publication_to_observed_sync_seconds"] = append(measurements["updated_publication_to_observed_sync_seconds"], v.UpdateSynced.Sub(v.UpdatePublished).Seconds())
		measurements["delete_to_cr_removal_seconds"] = append(measurements["delete_to_cr_removal_seconds"], v.CRRemoved.Sub(v.DeleteStarted).Seconds())
		measurements["cr_removal_to_downstream_deletion_seconds"] = append(measurements["cr_removal_to_downstream_deletion_seconds"], v.DownstreamDeleted.Sub(v.CRRemoved).Seconds())
		if v.CreatePublished.After(lastPublication) {
			lastPublication = v.CreatePublished
		}
	}
	summaries := map[string]interface{}{}
	for key, values := range measurements {
		summaries[key] = distribution(values)
	}
	gitSummaries := map[string]interface{}{}
	for key, values := range gitGroups {
		gitSummaries[key] = distribution(values)
	}
	report := map[string]interface{}{"startedAt": start, "count": 20, "publicationWorkers": 4, "throughput_publications_per_second": 20 / lastPublication.Sub(start).Seconds(), "summaries": summaries, "cases": raw, "git_operations": gitOps, "git_duration_seconds": gitSummaries, "rejected_push_retries_logged": retries, "rejected_push_retries_metric": after["git_state_rejected_push_retries_total"] - before["git_state_rejected_push_retries_total"], "status_patches": after["git_state_status_patches_total"] - before["git_state_status_patches_total"] - configurationPatches(before, after), "observations": after["git_state_observations_total"] - before["git_state_observations_total"], "configuration_status_changes": configWrites, "api_requests": metricDeltas(before, after, "rest_client_requests_total"), "reconciliations": metricDeltas(before, after, "controller_runtime_reconcile_total"), "status_patch_outcomes": metricDeltas(before, after, "git_state_status_patches_total"), "measurement_resolution": "one-second polling; API requests add overhead"}
	dir := filepath.Join(s.root, "reports", fmt.Sprintf("timing-v02-%d", start.Unix()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "results.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metrics.prom"), metricRaw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("20-resource timing results: %s; throughput %.2f publications/s; status patches %.0f", dir, 20/lastPublication.Sub(start).Seconds(), report["status_patches"].(float64))
}
