package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	usagev1alpha1 "github.com/SathvikMannam/secretusage/api/v1alpha1"
)

func TestReconcileCreatesSecretUsageForReferencedSecret(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t,
		deployment("default", "app", "app-secret"),
	)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "default",
		Name:      "app-secret",
	}})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-secret"}, &usage); err != nil {
		t.Fatalf("expected SecretUsage to be created: %v", err)
	}
	if usage.Status.SecretName != "app-secret" {
		t.Fatalf("got secretName %q, want app-secret", usage.Status.SecretName)
	}
	if usage.Status.Exists {
		t.Fatal("expected exists=false when referenced Secret is missing")
	}
	if usage.Status.UsageCount != 1 {
		t.Fatalf("got usageCount %d, want 1", usage.Status.UsageCount)
	}
	if usage.Status.Usages[0].Kind != "Deployment" {
		t.Fatalf("got kind %q, want Deployment", usage.Status.Usages[0].Kind)
	}
}

func TestReconcileUpdatesSecretExists(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t,
		secret("default", "app-secret"),
		deployment("default", "app", "app-secret"),
	)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "default",
		Name:      "app-secret",
	}})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-secret"}, &usage); err != nil {
		t.Fatalf("expected SecretUsage to be created: %v", err)
	}
	if !usage.Status.Exists {
		t.Fatal("expected exists=true when referenced Secret exists")
	}
}

func TestReconcileDeletesSecretUsageWhenNoReferencesRemain(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t,
		&usagev1alpha1.SecretUsage{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "default",
				Name:      "unused-secret",
			},
			Status: usagev1alpha1.SecretUsageStatus{
				SecretName: "unused-secret",
				UsageCount: 1,
				Usages:     []usagev1alpha1.SecretUsageReference{{APIVersion: "v1", Kind: "Pod", Namespace: "default", Name: "old", FieldPath: ".spec"}},
				Exists:     false,
			},
		},
	)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: "default",
		Name:      "unused-secret",
	}})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	err = reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "unused-secret"}, &usage)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected SecretUsage to be deleted, got err=%v", err)
	}
}

func newFakeReconciler(t *testing.T, objects ...client.Object) *SecretUsageReconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := usagev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add usage scheme: %v", err)
	}

	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&usagev1alpha1.SecretUsage{}).
		WithObjects(objects...)

	for _, obj := range indexedObjectsForTests() {
		indexed := obj
		builder = builder.WithIndex(indexed, SecretNameIndexField, func(raw client.Object) []string {
			return SecretNamesForObject(raw)
		})
	}

	return &SecretUsageReconciler{
		Client: builder.Build(),
		Scheme: scheme,
	}
}

func indexedObjectsForTests() []client.Object {
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
	}
}

func secret(namespace, name string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	}
}

func deployment(namespace, name, secretName string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			UID:       types.UID(name + "-uid"),
		},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: podSpecWithSecret(secretName),
			},
		},
	}
}
