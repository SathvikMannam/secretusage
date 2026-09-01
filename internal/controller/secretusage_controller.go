package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	usagev1alpha1 "github.com/SathvikMannam/secretusage/api/v1alpha1"
)

// DefaultMaxUsages bounds the number of references written to a single SecretUsage
// object. A widely shared Secret, such as a registry pull Secret referenced by every
// ServiceAccount in a large cluster, can otherwise produce an object that exceeds the
// etcd per-object size limit and fails to write at all.
const DefaultMaxUsages = 500

// aggregateMetricsInterval controls how often cluster-wide summary gauges are
// recomputed from the cache. Per-secret gauges are updated inline during reconcile.
const aggregateMetricsInterval = 30 * time.Second

// SecretUsageReconciler reconciles SecretUsage status for one Secret name per request.
type SecretUsageReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// MaxUsages caps the references recorded in status. Zero selects DefaultMaxUsages.
	MaxUsages int

	// TrackOwnedPods records Pods whose Secret references are already covered by a
	// tracked controller object. It is off by default because those Pods add no new
	// information and their churn rewrites status on every rollout.
	TrackOwnedPods bool

	// TrackUnusedSecrets keeps a SecretUsage object for a Secret that exists but has
	// no references, which is what makes unused Secrets discoverable. Turning it off
	// trades that report for one fewer object per Secret in large clusters.
	TrackUnusedSecrets bool

	// PerSecretMetrics enables the per-Secret gauges. Aggregate gauges are always on.
	PerSecretMetrics bool

	// Rules track Secret references in custom resources. They are loaded once at
	// startup because a field index cannot be registered after the cache has started.
	Rules []CompiledRule

	// rulesByGVK indexes Rules for event handling, and is built by SetupWithManager.
	rulesByGVK map[schema.GroupVersionKind]CompiledRule
}

// +kubebuilder:rbac:groups=usage.secretusage.io,resources=secretusages,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=usage.secretusage.io,resources=secretusages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=pods;replicationcontrollers;serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets;replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func (r *SecretUsageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("secret", req.NamespacedName)

	exists, err := r.secretExists(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}

	allUsages, err := r.collectUsages(ctx, req.Namespace, req.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	usages, total, truncated := r.truncateUsages(allUsages)

	// A Secret with no references is still worth an object while the Secret exists:
	// that is the unused-Secret report. Once neither the Secret nor any reference to
	// it remains, there is nothing left to describe.
	if total == 0 && !(exists && r.TrackUnusedSecrets) {
		if err := r.deleteSecretUsageIfPresent(ctx, req.NamespacedName); err != nil {
			return ctrl.Result{}, err
		}
		r.clearSecretMetrics(req.Namespace, req.Name)
		logger.V(1).Info("no references and nothing to track")
		return ctrl.Result{}, nil
	}

	if truncated {
		logger.Info("reference list truncated", "total", total, "recorded", len(usages), "limit", r.maxUsages())
	}
	r.recordSecretMetrics(req.Namespace, req.Name, exists, total, truncated)

	desiredStatus := usagev1alpha1.SecretUsageStatus{
		SecretName: req.Name,
		Exists:     exists,
		UsageCount: int32(total),
		Truncated:  truncated,
		Usages:     usages,
	}
	desiredLabels := desiredLabelsFor(req.Name, exists, total)

	var current usagev1alpha1.SecretUsage
	if err := r.Get(ctx, req.NamespacedName, &current); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		current = usagev1alpha1.SecretUsage{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: req.Namespace,
				Name:      req.Name,
				Labels:    desiredLabels,
			},
		}
		// Create and the status write are two calls because status is a subresource.
		// If the second call fails the object is left with an empty status and the
		// next reconcile fills it in, so no state is lost.
		if err := r.Create(ctx, &current); err != nil {
			return ctrl.Result{}, err
		}
		current.Status = desiredStatus
		return ctrl.Result{}, r.Status().Update(ctx, &current)
	}

	if !reflect.DeepEqual(current.Labels, desiredLabels) {
		patch := client.MergeFrom(current.DeepCopy())
		current.Labels = desiredLabels
		if err := r.Patch(ctx, &current, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	if reflect.DeepEqual(current.Status, desiredStatus) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(current.DeepCopy())
	current.Status = desiredStatus
	return ctrl.Result{}, r.Status().Patch(ctx, &current, patch)
}

// desiredLabelsFor builds the selector labels for a SecretUsage object. Conditions
// that are false are represented by an absent label rather than "false", so
// `-l usage.secretusage.io/unused=true` needs no negation to be useful.
func desiredLabelsFor(secretName string, exists bool, total int) map[string]string {
	labels := map[string]string{}
	// Secret names may be up to 253 characters, but a label value may not exceed 63.
	if len(validation.IsValidLabelValue(secretName)) == 0 {
		labels[usagev1alpha1.SecretNameLabel] = secretName
	}
	if exists && total == 0 {
		labels[usagev1alpha1.UnusedLabel] = "true"
	}
	if !exists && total > 0 {
		labels[usagev1alpha1.MissingLabel] = "true"
	}
	return labels
}

func (r *SecretUsageReconciler) maxUsages() int {
	if r.MaxUsages > 0 {
		return r.MaxUsages
	}
	return DefaultMaxUsages
}

// truncateUsages returns the references to record, the true total, and whether the
// recorded list is shorter than the total.
func (r *SecretUsageReconciler) truncateUsages(usages []usagev1alpha1.SecretUsageReference) ([]usagev1alpha1.SecretUsageReference, int, bool) {
	limit := r.maxUsages()
	if len(usages) <= limit {
		return usages, len(usages), false
	}
	return usages[:limit], len(usages), true
}

func (r *SecretUsageReconciler) secretExists(ctx context.Context, key types.NamespacedName) (bool, error) {
	// Requesting PartialObjectMetadata rather than the Secret itself means Secret
	// values never cross the wire and never enter the controller's cache. Existence
	// is all this controller needs.
	if err := r.Get(ctx, key, secretMetadataObject()); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *SecretUsageReconciler) deleteSecretUsageIfPresent(ctx context.Context, key types.NamespacedName) error {
	var current usagev1alpha1.SecretUsage
	if err := r.Get(ctx, key, &current); err != nil {
		return client.IgnoreNotFound(err)
	}
	return client.IgnoreNotFound(r.Delete(ctx, &current))
}

func (r *SecretUsageReconciler) collectUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	usages := make([]usagev1alpha1.SecretUsageReference, 0)

	collectors := []func(context.Context, string, string) ([]usagev1alpha1.SecretUsageReference, error){
		r.collectPodUsages,
		r.collectDeploymentUsages,
		r.collectStatefulSetUsages,
		r.collectDaemonSetUsages,
		r.collectJobUsages,
		r.collectCronJobUsages,
		r.collectReplicaSetUsages,
		r.collectReplicationControllerUsages,
		r.collectServiceAccountUsages,
		r.collectIngressUsages,
		r.collectCustomResourceUsages,
	}

	for _, collect := range collectors {
		refs, err := collect(ctx, namespace, secretName)
		if err != nil {
			return nil, err
		}
		usages = append(usages, refs...)
	}

	sort.Slice(usages, func(i, j int) bool {
		return usageReferenceLess(usages[i], usages[j])
	})
	return usages, nil
}

func (r *SecretUsageReconciler) collectPodUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list corev1.PodList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		if r.skipPod(&list.Items[i]) {
			continue
		}
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

// skipPod reports whether a Pod's references are redundant with a tracked controller.
func (r *SecretUsageReconciler) skipPod(pod *corev1.Pod) bool {
	return !r.TrackOwnedPods && PodIsCoveredByController(pod)
}

func (r *SecretUsageReconciler) collectDeploymentUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list appsv1.DeploymentList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

func (r *SecretUsageReconciler) collectStatefulSetUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list appsv1.StatefulSetList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

func (r *SecretUsageReconciler) collectDaemonSetUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list appsv1.DaemonSetList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

func (r *SecretUsageReconciler) collectJobUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list batchv1.JobList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

func (r *SecretUsageReconciler) collectCronJobUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list batchv1.CronJobList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

func (r *SecretUsageReconciler) collectReplicaSetUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list appsv1.ReplicaSetList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

func (r *SecretUsageReconciler) collectReplicationControllerUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list corev1.ReplicationControllerList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

func (r *SecretUsageReconciler) collectServiceAccountUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list corev1.ServiceAccountList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

func (r *SecretUsageReconciler) collectIngressUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	var list networkingv1.IngressList
	if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
		return nil, err
	}
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(list.Items))
	for i := range list.Items {
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
}

// collectCustomResourceUsages walks the configured rules. Each rule's kind has its own
// field index, so this is the same indexed lookup the built-in kinds use.
func (r *SecretUsageReconciler) collectCustomResourceUsages(ctx context.Context, namespace, secretName string) ([]usagev1alpha1.SecretUsageReference, error) {
	if len(r.Rules) == 0 {
		return nil, nil
	}

	usages := make([]usagev1alpha1.SecretUsageReference, 0)
	for _, rule := range r.Rules {
		var list unstructured.UnstructuredList
		list.SetGroupVersionKind(rule.GVK)
		if err := r.List(ctx, &list, indexedSecretListOptions(namespace, secretName)...); err != nil {
			return nil, fmt.Errorf("list %s: %w", rule.GVK, err)
		}
		for i := range list.Items {
			usages = append(usages, ruleUsageReferences(rule, &list.Items[i], secretName)...)
		}
	}
	return usages, nil
}

// ruleUsageReferences converts a rule match into status entries.
func ruleUsageReferences(rule CompiledRule, obj *unstructured.Unstructured, secretName string) []usagev1alpha1.SecretUsageReference {
	var usages []usagev1alpha1.SecretUsageReference
	for _, ref := range rule.references(obj) {
		if ref.SecretName != secretName {
			continue
		}
		usages = append(usages, usagev1alpha1.SecretUsageReference{
			APIVersion: rule.GVK.GroupVersion().String(),
			Kind:       rule.GVK.Kind,
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
			UID:        string(obj.GetUID()),
			FieldPath:  ref.FieldPath,
		})
	}
	return usages
}

func indexedSecretListOptions(namespace, secretName string) []client.ListOption {
	return []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{SecretNameIndexField: secretName},
	}
}

// IndexedObjects lists every kind whose Secret references are indexed and watched.
func IndexedObjects() []client.Object {
	return []client.Object{
		&corev1.Pod{},
		&appsv1.Deployment{},
		&appsv1.StatefulSet{},
		&appsv1.DaemonSet{},
		&batchv1.Job{},
		&batchv1.CronJob{},
		&appsv1.ReplicaSet{},
		&corev1.ReplicationController{},
		&corev1.ServiceAccount{},
		&networkingv1.Ingress{},
	}
}

func (r *SecretUsageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()

	for _, obj := range IndexedObjects() {
		if err := mgr.GetFieldIndexer().IndexField(ctx, obj, SecretNameIndexField, func(raw client.Object) []string {
			return SecretNamesForObject(raw)
		}); err != nil {
			return err
		}
	}

	if err := r.setupRules(mgr); err != nil {
		return err
	}

	if err := mgr.Add(manager.RunnableFunc(r.runAggregateMetrics)); err != nil {
		return err
	}

	consumerHandler := r.enqueueForSecretConsumer()
	builder := ctrl.NewControllerManagedBy(mgr).
		// The Secret cache is metadata-only, so the watched type is metadata too.
		For(secretMetadataObject())
	for _, obj := range IndexedObjects() {
		builder = builder.Watches(obj, consumerHandler)
	}
	for _, rule := range r.Rules {
		builder = builder.Watches(rule.Object(), consumerHandler)
	}

	// Only Create and Delete are watched on SecretUsage. Every status write this
	// controller makes would otherwise re-enqueue the same key and double the
	// reconcile count for no benefit. Create still validates objects seen at startup,
	// and Delete restores an object a user removed by hand.
	return builder.
		Watches(&usagev1alpha1.SecretUsage{}, handler.Funcs{
			CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				enqueueOwnKey(e.Object, q)
			},
			DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				enqueueOwnKey(e.Object, q)
			},
		}).
		Complete(r)
}

// setupRules drops rules whose CRD is not installed and registers a field index for
// each of the rest. A rule for an absent kind would otherwise fail cache sync and
// crash-loop the manager, which is a poor trade for one optional integration.
func (r *SecretUsageReconciler) setupRules(mgr ctrl.Manager) error {
	if len(r.Rules) == 0 {
		return nil
	}

	logger := mgr.GetLogger().WithName("rules")
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(mgr.GetConfig())
	if err != nil {
		return fmt.Errorf("build discovery client: %w", err)
	}

	installed := make([]CompiledRule, 0, len(r.Rules))
	for _, rule := range r.Rules {
		present, err := kindIsInstalled(discoveryClient, rule.GVK)
		if err != nil {
			return fmt.Errorf("check %s: %w", rule.GVK, err)
		}
		if !present {
			logger.Info("skipping rule, kind is not installed in this cluster",
				"apiVersion", rule.GVK.GroupVersion().String(), "kind", rule.GVK.Kind)
			continue
		}
		installed = append(installed, rule)
	}
	r.Rules = installed

	r.rulesByGVK = make(map[schema.GroupVersionKind]CompiledRule, len(r.Rules))
	for _, rule := range r.Rules {
		r.rulesByGVK[rule.GVK] = rule

		indexRule := rule
		if err := mgr.GetFieldIndexer().IndexField(context.Background(), indexRule.Object(), SecretNameIndexField, func(raw client.Object) []string {
			return indexRule.secretNames(raw)
		}); err != nil {
			return fmt.Errorf("index %s: %w", indexRule.GVK, err)
		}
		logger.Info("tracking custom resource",
			"apiVersion", rule.GVK.GroupVersion().String(), "kind", rule.GVK.Kind)
	}
	return nil
}

// kindIsInstalled reports whether the cluster serves the given kind.
func kindIsInstalled(discoveryClient discovery.DiscoveryInterface, gvk schema.GroupVersionKind) (bool, error) {
	resources, err := discoveryClient.ServerResourcesForGroupVersion(gvk.GroupVersion().String())
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, err
	}
	for _, resource := range resources.APIResources {
		if resource.Kind == gvk.Kind {
			return true, nil
		}
	}
	return false, nil
}

func secretMetadataObject() *metav1.PartialObjectMetadata {
	secret := &metav1.PartialObjectMetadata{}
	secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	return secret
}

func enqueueOwnKey(obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if obj == nil {
		return
	}
	q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}})
}

// runAggregateMetrics recomputes cluster-wide summary gauges from the cache. It runs
// under leader election, so exactly one replica publishes these numbers.
func (r *SecretUsageReconciler) runAggregateMetrics(ctx context.Context) error {
	ticker := time.NewTicker(aggregateMetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			var list usagev1alpha1.SecretUsageList
			if err := r.List(ctx, &list); err != nil {
				log.FromContext(ctx).Error(err, "unable to list SecretUsage for aggregate metrics")
				continue
			}
			var missing, unused float64
			for i := range list.Items {
				status := list.Items[i].Status
				switch {
				case !status.Exists && status.UsageCount > 0:
					missing++
				case status.Exists && status.UsageCount == 0:
					unused++
				}
			}
			secretsMissingTotal.Set(missing)
			secretsUnusedTotal.Set(unused)
		}
	}
}

// secretNamesFor dispatches between the built-in extractor and the configured rules.
func (r *SecretUsageReconciler) secretNamesFor(obj client.Object) []string {
	if unstructuredObj, ok := obj.(*unstructured.Unstructured); ok {
		rule, found := r.rulesByGVK[unstructuredObj.GroupVersionKind()]
		if !found {
			return nil
		}
		return rule.secretNames(unstructuredObj)
	}
	return SecretNamesForObject(obj)
}

func (r *SecretUsageReconciler) enqueueForSecretConsumer() handler.EventHandler {
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueObjectSecretReferences(e.Object, q)
		},
		UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueObjectSecretReferences(e.ObjectOld, q)
			r.enqueueObjectSecretReferences(e.ObjectNew, q)
		},
		DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueObjectSecretReferences(e.Object, q)
		},
		GenericFunc: func(ctx context.Context, e event.GenericEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
			r.enqueueObjectSecretReferences(e.Object, q)
		},
	}
}

func (r *SecretUsageReconciler) enqueueObjectSecretReferences(obj client.Object, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if obj == nil {
		return
	}
	// Pods covered by a tracked controller are filtered here as well as during
	// collection, so a rollout does not queue a reconcile per replaced Pod.
	if pod, ok := obj.(*corev1.Pod); ok && r.skipPod(pod) {
		return
	}
	namespace := obj.GetNamespace()
	for _, secretName := range r.secretNamesFor(obj) {
		q.Add(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: namespace,
				Name:      secretName,
			},
		})
	}
}
