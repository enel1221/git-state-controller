package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var statusPatchCounter = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "git_state_status_patches_total", Help: "Status patches by writer and outcome."}, []string{"writer", "outcome"})
var observationCounter = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "git_state_observations_total", Help: "Single-target observation outcomes."}, []string{"outcome"})

func init() { metrics.Registry.MustRegister(statusPatchCounter, observationCounter) }
func outcome(err error) string {
	if apierrors.IsConflict(err) {
		return "conflict"
	}
	if err != nil {
		return "error"
	}
	return "success"
}
