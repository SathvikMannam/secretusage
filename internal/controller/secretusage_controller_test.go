package controller

import (
	"context"
	"strconv"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
	// The Secret is absent as well, so there is nothing left to describe.
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
	return newFakeReconcilerWithRules(t, nil, objects...)
}

// newFakeReconcilerWithRules also registers a scheme entry and field index for each
// rule's kind, which is what SetupWithManager does against a real cluster.
func newFakeReconcilerWithRules(t *testing.T, rules []CompiledRule, objects ...client.Object) *SecretUsageReconciler {
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

	for _, obj := range IndexedObjects() {
		indexed := obj
		builder = builder.WithIndex(indexed, SecretNameIndexField, func(raw client.Object) []string {
			return SecretNamesForObject(raw)
		})
	}

	rulesByGVK := make(map[schema.GroupVersionKind]CompiledRule, len(rules))
	for _, rule := range rules {
		indexRule := rule
		rulesByGVK[indexRule.GVK] = indexRule
		scheme.AddKnownTypeWithName(indexRule.GVK, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(indexRule.GVK.GroupVersion().WithKind(indexRule.GVK.Kind+"List"), &unstructured.UnstructuredList{})
		builder = builder.WithIndex(indexRule.Object(), SecretNameIndexField, func(raw client.Object) []string {
			return indexRule.secretNames(raw)
		})
	}

	return &SecretUsageReconciler{
		Client:             builder.Build(),
		Scheme:             scheme,
		TrackUnusedSecrets: true,
		Rules:              rules,
		rulesByGVK:         rulesByGVK,
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

func TestReconcileTracksUnusedSecretThatExists(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t, secret("default", "orphan"))

	if _, err := reconciler.Reconcile(ctx, request("default", "orphan")); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "orphan"}, &usage); err != nil {
		t.Fatalf("expected an unused Secret to be tracked: %v", err)
	}
	if !usage.Status.Exists || usage.Status.UsageCount != 0 {
		t.Fatalf("want exists=true usageCount=0, got exists=%v usageCount=%d", usage.Status.Exists, usage.Status.UsageCount)
	}
	if usage.Labels[usagev1alpha1.UnusedLabel] != "true" {
		t.Fatalf("expected unused label, got labels %v", usage.Labels)
	}
	if _, ok := usage.Labels[usagev1alpha1.MissingLabel]; ok {
		t.Fatal("an existing Secret must not carry the missing label")
	}
}

func TestReconcileDropsUnusedRecordWhenTrackingDisabled(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t, secret("default", "orphan"))
	reconciler.TrackUnusedSecrets = false

	if _, err := reconciler.Reconcile(ctx, request("default", "orphan")); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "orphan"}, &usage)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no SecretUsage object, got err=%v", err)
	}
}

func TestReconcileLabelsDanglingReference(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t, deployment("default", "app", "app-secret"))

	if _, err := reconciler.Reconcile(ctx, request("default", "app-secret")); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-secret"}, &usage); err != nil {
		t.Fatalf("get SecretUsage: %v", err)
	}
	if usage.Labels[usagev1alpha1.MissingLabel] != "true" {
		t.Fatalf("expected missing label on a dangling reference, got %v", usage.Labels)
	}
	if usage.Labels[usagev1alpha1.SecretNameLabel] != "app-secret" {
		t.Fatalf("expected secret-name label, got %v", usage.Labels)
	}
}

func TestReconcileSkipsPodsCoveredByTheirController(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t,
		secret("default", "app-secret"),
		replicaSet("default", "app-abc123", "app-secret"),
		podOwnedBy("default", "app-abc123-xyz", "app-secret", "apps/v1", "ReplicaSet", "app-abc123"),
		standalonePod("default", "debug", "app-secret"),
	)

	if _, err := reconciler.Reconcile(ctx, request("default", "app-secret")); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-secret"}, &usage); err != nil {
		t.Fatalf("get SecretUsage: %v", err)
	}

	names := map[string]string{}
	for _, ref := range usage.Status.Usages {
		names[ref.Name] = ref.Kind
	}
	if _, ok := names["app-abc123-xyz"]; ok {
		t.Fatalf("Pod owned by a tracked ReplicaSet should be skipped, got %v", names)
	}
	if names["app-abc123"] != "ReplicaSet" {
		t.Fatalf("expected the owning ReplicaSet to be recorded, got %v", names)
	}
	if names["debug"] != "Pod" {
		t.Fatalf("standalone Pods must always be recorded, got %v", names)
	}
}

func TestReconcileRecordsOwnedPodsWhenEnabled(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t,
		replicaSet("default", "app-abc123", "app-secret"),
		podOwnedBy("default", "app-abc123-xyz", "app-secret", "apps/v1", "ReplicaSet", "app-abc123"),
	)
	reconciler.TrackOwnedPods = true

	if _, err := reconciler.Reconcile(ctx, request("default", "app-secret")); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-secret"}, &usage); err != nil {
		t.Fatalf("get SecretUsage: %v", err)
	}
	if usage.Status.UsageCount != 2 {
		t.Fatalf("want both ReplicaSet and Pod recorded, got usageCount=%d", usage.Status.UsageCount)
	}
}

func TestReconcileTracksIngressTLSSecret(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t, ingress("default", "web", "web-tls"))

	if _, err := reconciler.Reconcile(ctx, request("default", "web-tls")); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "web-tls"}, &usage); err != nil {
		t.Fatalf("expected the Ingress TLS Secret to be tracked: %v", err)
	}
	if usage.Status.Usages[0].Kind != "Ingress" || usage.Status.Usages[0].FieldPath != ".spec.tls[0].secretName" {
		t.Fatalf("unexpected usage: %#v", usage.Status.Usages[0])
	}
}

func TestReconcileTruncatesOversizedUsageLists(t *testing.T) {
	ctx := context.Background()
	objects := []client.Object{secret("default", "pull-secret")}
	for i := 0; i < 5; i++ {
		objects = append(objects, serviceAccountWithPullSecret("default", "sa-"+strconv.Itoa(i), "pull-secret"))
	}

	reconciler := newFakeReconciler(t, objects...)
	reconciler.MaxUsages = 3

	if _, err := reconciler.Reconcile(ctx, request("default", "pull-secret")); err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var usage usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "pull-secret"}, &usage); err != nil {
		t.Fatalf("get SecretUsage: %v", err)
	}
	if !usage.Status.Truncated {
		t.Fatal("expected truncated=true")
	}
	if usage.Status.UsageCount != 5 {
		t.Fatalf("usageCount must report the true total, got %d", usage.Status.UsageCount)
	}
	if len(usage.Status.Usages) != 3 {
		t.Fatalf("expected 3 recorded usages, got %d", len(usage.Status.Usages))
	}
}

func TestReconcileIsStableAcrossRepeatedRuns(t *testing.T) {
	ctx := context.Background()
	reconciler := newFakeReconciler(t,
		secret("default", "app-secret"),
		deployment("default", "app", "app-secret"),
	)

	if _, err := reconciler.Reconcile(ctx, request("default", "app-secret")); err != nil {
		t.Fatalf("first reconcile failed: %v", err)
	}
	var first usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-secret"}, &first); err != nil {
		t.Fatalf("get SecretUsage: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, request("default", "app-secret")); err != nil {
		t.Fatalf("second reconcile failed: %v", err)
	}
	var second usagev1alpha1.SecretUsage
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "app-secret"}, &second); err != nil {
		t.Fatalf("get SecretUsage: %v", err)
	}

	// A no-op reconcile must not write, or every watch event would cost an etcd write.
	if first.ResourceVersion != second.ResourceVersion {
		t.Fatalf("reconcile is not idempotent: resourceVersion moved %s -> %s", first.ResourceVersion, second.ResourceVersion)
	}
}

func request(namespace, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

func replicaSet(namespace, name, secretName string) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid")},
	}
	rs.Spec.Template.Spec = podSpecWithSecret(secretName)
	return rs
}

func standalonePod(namespace, name, secretName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid")},
		Spec:       podSpecWithSecret(secretName),
	}
}

func podOwnedBy(namespace, name, secretName, apiVersion, kind, ownerName string) *corev1.Pod {
	controller := true
	pod := standalonePod(namespace, name, secretName)
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       ownerName,
		UID:        types.UID(ownerName + "-uid"),
		Controller: &controller,
	}}
	return pod
}

func serviceAccountWithPullSecret(namespace, name, secretName string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta:       metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid")},
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: secretName}},
	}
}

func ingress(namespace, name, secretName string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, UID: types.UID(name + "-uid")},
		Spec: networkingv1.IngressSpec{
			TLS: []networkingv1.IngressTLS{{Hosts: []string{"example.com"}, SecretName: secretName}},
		},
	}
}
