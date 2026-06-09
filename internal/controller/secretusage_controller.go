package controller

import (
	"context"
	"reflect"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	usagev1alpha1 "github.com/SathvikMannam/secretusage/api/v1alpha1"
)

// SecretUsageReconciler reconciles SecretUsage status for one Secret name per request.
type SecretUsageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=usage.secretusage.io,resources=secretusages,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=usage.secretusage.io,resources=secretusages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods;replicationcontrollers;serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;daemonsets;replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs;cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func (r *SecretUsageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("secret", req.NamespacedName)

	exists, err := r.secretExists(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, err
	}

	usages, err := r.collectUsages(ctx, req.Namespace, req.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	if len(usages) == 0 {
		if err := r.deleteSecretUsageIfPresent(ctx, req.NamespacedName); err != nil {
			return ctrl.Result{}, err
		}
		logger.V(1).Info("secret has no usages")
		return ctrl.Result{}, nil
	}

	desiredStatus := usagev1alpha1.SecretUsageStatus{
		SecretName: req.Name,
		Exists:     exists,
		UsageCount: int32(len(usages)),
		Usages:     usages,
	}

	var current usagev1alpha1.SecretUsage
	if err := r.Get(ctx, req.NamespacedName, &current); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}

		current = usagev1alpha1.SecretUsage{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: req.Namespace,
				Name:      req.Name,
			},
		}
		if err := r.Create(ctx, &current); err != nil {
			return ctrl.Result{}, err
		}
		current.Status = desiredStatus
		if err := r.Status().Update(ctx, &current); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if reflect.DeepEqual(current.Status, desiredStatus) {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(current.DeepCopy())
	current.Status = desiredStatus
	if err := r.Status().Patch(ctx, &current, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SecretUsageReconciler) secretExists(ctx context.Context, key types.NamespacedName) (bool, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, key, &secret); err != nil {
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
		usages = append(usages, usageReferencesForObject(&list.Items[i], secretName)...)
	}
	return usages, nil
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

func indexedSecretListOptions(namespace, secretName string) []client.ListOption {
	return []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingFields{SecretNameIndexField: secretName},
	}
}

func (r *SecretUsageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	indexedObjects := []client.Object{
		&corev1.Pod{},
		&appsv1.Deployment{},
		&appsv1.StatefulSet{},
		&appsv1.DaemonSet{},
		&batchv1.Job{},
		&batchv1.CronJob{},
		&appsv1.ReplicaSet{},
		&corev1.ReplicationController{},
		&corev1.ServiceAccount{},
	}

	for _, obj := range indexedObjects {
		if err := mgr.GetFieldIndexer().IndexField(ctx, obj, SecretNameIndexField, func(raw client.Object) []string {
			return SecretNamesForObject(raw)
		}); err != nil {
			return err
		}
	}

	consumerHandler := r.enqueueForSecretConsumer()
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Secret{}).
		Watches(&corev1.Pod{}, consumerHandler).
		Watches(&appsv1.Deployment{}, consumerHandler).
		Watches(&appsv1.StatefulSet{}, consumerHandler).
		Watches(&appsv1.DaemonSet{}, consumerHandler).
		Watches(&batchv1.Job{}, consumerHandler).
		Watches(&batchv1.CronJob{}, consumerHandler).
		Watches(&appsv1.ReplicaSet{}, consumerHandler).
		Watches(&corev1.ReplicationController{}, consumerHandler).
		Watches(&corev1.ServiceAccount{}, consumerHandler).
		Watches(&usagev1alpha1.SecretUsage{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}}
		})).
		Complete(r)
}

func (r *SecretUsageReconciler) enqueueForSecretConsumer() handler.EventHandler {
	return handler.Funcs{
		CreateFunc: func(ctx context.Context, event event.CreateEvent, q workqueue.RateLimitingInterface) {
			enqueueObjectSecretReferences(event.Object, q)
		},
		UpdateFunc: func(ctx context.Context, event event.UpdateEvent, q workqueue.RateLimitingInterface) {
			enqueueObjectSecretReferences(event.ObjectOld, q)
			enqueueObjectSecretReferences(event.ObjectNew, q)
		},
		DeleteFunc: func(ctx context.Context, event event.DeleteEvent, q workqueue.RateLimitingInterface) {
			enqueueObjectSecretReferences(event.Object, q)
		},
		GenericFunc: func(ctx context.Context, event event.GenericEvent, q workqueue.RateLimitingInterface) {
			enqueueObjectSecretReferences(event.Object, q)
		},
	}
}

func enqueueObjectSecretReferences(obj client.Object, q workqueue.RateLimitingInterface) {
	if obj == nil {
		return
	}
	namespace := obj.GetNamespace()
	for _, secretName := range SecretNamesForObject(obj) {
		q.Add(reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: namespace,
				Name:      secretName,
			},
		})
	}
}
