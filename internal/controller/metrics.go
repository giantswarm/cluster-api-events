package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	metricNamespace = "cluster_api_events"
	metricSubsystem = "cluster"
)

// Counters for cluster deletions
var (
	transitionLabels = []string{"cluster_id", "namespace", "organization", "installation", "release_version"}
	infoLabels       = []string{"cluster_id", "namespace", "organization", "installation", "release_version"}

	clusterInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "info",
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Help:      "Cluster info.",
		},
		infoLabels,
	)

	clusterTransitionCreate = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "create_transition",
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Help:      "Latest cluster creation transition.",
		},
		transitionLabels,
	)
	clusterTransitionUpdate = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:      "update_transition",
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Help:      "Latest cluster update transition.",
		},
		transitionLabels,
	)
)

func init() {
	metrics.Registry.MustRegister(clusterInfo, clusterTransitionCreate, clusterTransitionUpdate)
}
