package main

import (
	"flag"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	usagev1alpha1 "github.com/SathvikMannam/secretusage/api/v1alpha1"
	"github.com/SathvikMannam/secretusage/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(usagev1alpha1.AddToScheme(scheme))
	// PartialObjectMetadata is how Secrets are watched, so the meta types must be
	// registered for the metadata-only informer and client to work.
	utilruntime.Must(metav1.AddMetaToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var watchNamespace string
	var maxUsages int
	var trackOwnedPods bool
	var trackUnusedSecrets bool
	var perSecretMetrics bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&watchNamespace, "watch-namespace", os.Getenv("WATCH_NAMESPACE"), "Namespace to watch. Empty means all namespaces.")
	flag.IntVar(&maxUsages, "max-usages", envInt("MAX_USAGES", controller.DefaultMaxUsages),
		"Maximum references recorded in a single SecretUsage object. Keeps objects below the etcd size limit; the true count stays in .status.usageCount.")
	flag.BoolVar(&trackOwnedPods, "track-owned-pods", envBool("TRACK_OWNED_PODS", false),
		"Record Pods whose Secret references are already covered by their controller. Adds no new information and rewrites status on every rollout.")
	flag.BoolVar(&trackUnusedSecrets, "track-unused-secrets", envBool("TRACK_UNUSED_SECRETS", true),
		"Keep a SecretUsage object for Secrets that exist but have no references, so unused Secrets are reportable. Costs one object per Secret.")
	flag.BoolVar(&perSecretMetrics, "per-secret-metrics", envBool("PER_SECRET_METRICS", true),
		"Export per-Secret gauges. Disable on clusters where three series per Secret is too much cardinality; aggregate gauges stay on.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	managerOptions := ctrl.Options{
		Scheme: scheme,
		// PartialObjectMetadata reads bypass the cache by default. Enabling it here
		// keeps the Secret existence check served from the metadata informer instead
		// of issuing a live GET per reconcile, which is also why the controller needs
		// only list and watch on Secrets, never get.
		Client: client.Options{
			Cache: &client.CacheOptions{Unstructured: true},
		},
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "secretusage-controller.usage.secretusage.io",
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				// Defense in depth. Secrets are watched as PartialObjectMetadata, so
				// values are not fetched at all, but if a typed Secret informer is
				// ever created this strips the payload before it reaches the cache.
				&corev1.Secret{}: {
					Transform: func(obj interface{}) (interface{}, error) {
						if secret, ok := obj.(*corev1.Secret); ok {
							secret.Data = nil
							secret.StringData = nil
						}
						return obj, nil
					},
				},
			},
		},
	}
	if watchNamespace != "" {
		managerOptions.Cache.DefaultNamespaces = map[string]cache.Config{
			watchNamespace: {},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), managerOptions)
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.SecretUsageReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		MaxUsages:          maxUsages,
		TrackOwnedPods:     trackOwnedPods,
		TrackUnusedSecrets: trackUnusedSecrets,
		PerSecretMetrics:   perSecretMetrics,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "SecretUsage")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	ctrl.Log.Info("starting manager",
		"watchNamespace", watchNamespace,
		"maxUsages", maxUsages,
		"trackOwnedPods", trackOwnedPods,
		"trackUnusedSecrets", trackUnusedSecrets,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// envInt and envBool let the Helm chart configure behaviour through the environment
// without templating a different argument list for every value.
func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}

func envBool(name string, fallback bool) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}
