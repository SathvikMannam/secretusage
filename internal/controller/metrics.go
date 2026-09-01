package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Per-secret metrics deliberately record raw state rather than pre-computed
// conditions, so alerts are derived in PromQL and the series count stays at three
// per tracked Secret. See the README for the dangling-reference and unused-Secret
// alert expressions.
var (
	secretReferences = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "secretusage_secret_references",
		Help: "Number of references to a Secret found across tracked objects.",
	}, []string{"namespace", "secret"})

	secretExists = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "secretusage_secret_exists",
		Help: "1 if the referenced Secret exists in the namespace, 0 if it does not.",
	}, []string{"namespace", "secret"})

	secretUsagesTruncated = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "secretusage_secret_usages_truncated",
		Help: "1 if a Secret has more references than the controller records in status.",
	}, []string{"namespace", "secret"})

	// Aggregate counters are always registered, even when per-secret metrics are
	// disabled, so a cluster with cardinality concerns keeps its alerting signal.
	secretsMissingTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "secretusage_missing_secrets",
		Help: "Number of Secrets that are referenced but do not exist.",
	})

	secretsUnusedTotal = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "secretusage_unused_secrets",
		Help: "Number of Secrets that exist but have no references.",
	})
)

func init() {
	metrics.Registry.MustRegister(
		secretReferences,
		secretExists,
		secretUsagesTruncated,
		secretsMissingTotal,
		secretsUnusedTotal,
	)
}

// recordSecretMetrics publishes the observed state of one Secret.
func (r *SecretUsageReconciler) recordSecretMetrics(namespace, secret string, exists bool, usageCount int, truncated bool) {
	if !r.PerSecretMetrics {
		return
	}
	secretReferences.WithLabelValues(namespace, secret).Set(float64(usageCount))
	secretExists.WithLabelValues(namespace, secret).Set(boolToFloat(exists))
	secretUsagesTruncated.WithLabelValues(namespace, secret).Set(boolToFloat(truncated))
}

// clearSecretMetrics drops the series for a Secret that is no longer tracked, so
// deleted Secrets do not leave stale series behind.
func (r *SecretUsageReconciler) clearSecretMetrics(namespace, secret string) {
	if !r.PerSecretMetrics {
		return
	}
	secretReferences.DeleteLabelValues(namespace, secret)
	secretExists.DeleteLabelValues(namespace, secret)
	secretUsagesTruncated.DeleteLabelValues(namespace, secret)
}

func boolToFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
