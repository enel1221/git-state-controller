package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var statusPatchCounter = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "git_state_status_patches_total", Help: "GitResource status patches by outcome."}, []string{"outcome"})
var observationCounter = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "git_state_observations_total", Help: "Single-target observation outcomes."}, []string{"outcome"})

func init() { metrics.Registry.MustRegister(statusPatchCounter, observationCounter) }
func outcome(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}
