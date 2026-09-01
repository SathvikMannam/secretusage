package controller

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	usagev1alpha1 "github.com/SathvikMannam/secretusage/api/v1alpha1"
)

func TestSecretNamesForPod(t *testing.T) {
	optional := true
	pod := &corev1.Pod{}
	pod.Name = "app"
	pod.Namespace = "default"
	pod.UID = types.UID("pod-uid")
	pod.Spec = corev1.PodSpec{
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "pull-secret"}},
		Volumes: []corev1.Volume{
			{
				Name: "secret-volume",
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: "volume-secret", Optional: &optional},
				},
			},
			{
				Name: "projected-volume",
				VolumeSource: corev1.VolumeSource{
					Projected: &corev1.ProjectedVolumeSource{
						Sources: []corev1.VolumeProjection{
							{Secret: &corev1.SecretProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "projected-secret"}}},
						},
					},
				},
			},
		},
		InitContainers: []corev1.Container{
			containerWithSecretEnv("init", "init-secret", "password", &optional),
		},
		Containers: []corev1.Container{
			{
				Name: "app",
				Env: []corev1.EnvVar{
					{
						Name: "TOKEN",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: "env-secret"},
								Key:                  "token",
								Optional:             &optional,
							},
						},
					},
				},
				EnvFrom: []corev1.EnvFromSource{
					{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "envfrom-secret"}}},
				},
			},
		},
		EphemeralContainers: []corev1.EphemeralContainer{
			{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon{
					Name: "debugger",
					Env: []corev1.EnvVar{
						{
							Name: "DEBUG_TOKEN",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{Name: "debug-secret"},
									Key:                  "token",
								},
							},
						},
					},
				},
			},
		},
	}

	want := []string{
		"debug-secret",
		"env-secret",
		"envfrom-secret",
		"init-secret",
		"projected-secret",
		"pull-secret",
		"volume-secret",
	}
	assertStringSliceEqual(t, SecretNamesForObject(pod), want)

	usages := usageReferencesForObject(pod, "env-secret")
	if len(usages) != 1 {
		t.Fatalf("expected one env-secret usage, got %d", len(usages))
	}
	if usages[0].Kind != "Pod" || usages[0].Container != "app" || usages[0].Key != "token" {
		t.Fatalf("unexpected usage: %#v", usages[0])
	}
	if usages[0].FieldPath != ".spec.containers[0].env[0].valueFrom.secretKeyRef.name" {
		t.Fatalf("unexpected field path: %s", usages[0].FieldPath)
	}
}

func TestSecretNamesForWorkloads(t *testing.T) {
	deployment := &appsv1.Deployment{}
	deployment.Spec.Template.Spec = podSpecWithSecret("deployment-secret")
	assertStringSliceEqual(t, SecretNamesForObject(deployment), []string{"deployment-secret"})

	statefulSet := &appsv1.StatefulSet{}
	statefulSet.Spec.Template.Spec = podSpecWithSecret("statefulset-secret")
	assertStringSliceEqual(t, SecretNamesForObject(statefulSet), []string{"statefulset-secret"})

	daemonSet := &appsv1.DaemonSet{}
	daemonSet.Spec.Template.Spec = podSpecWithSecret("daemonset-secret")
	assertStringSliceEqual(t, SecretNamesForObject(daemonSet), []string{"daemonset-secret"})

	replicaSet := &appsv1.ReplicaSet{}
	replicaSet.Spec.Template.Spec = podSpecWithSecret("replicaset-secret")
	assertStringSliceEqual(t, SecretNamesForObject(replicaSet), []string{"replicaset-secret"})

	job := &batchv1.Job{}
	job.Spec.Template.Spec = podSpecWithSecret("job-secret")
	assertStringSliceEqual(t, SecretNamesForObject(job), []string{"job-secret"})

	cronJob := &batchv1.CronJob{}
	cronJob.Spec.JobTemplate.Spec.Template.Spec = podSpecWithSecret("cronjob-secret")
	assertStringSliceEqual(t, SecretNamesForObject(cronJob), []string{"cronjob-secret"})

	rc := &corev1.ReplicationController{}
	rc.Spec.Template = &corev1.PodTemplateSpec{}
	rc.Spec.Template.Spec = podSpecWithSecret("rc-secret")
	assertStringSliceEqual(t, SecretNamesForObject(rc), []string{"rc-secret"})
}

func TestSecretNamesForServiceAccount(t *testing.T) {
	sa := &corev1.ServiceAccount{
		Secrets: []corev1.ObjectReference{
			{Name: "token-secret"},
			{Name: "token-secret"},
		},
		ImagePullSecrets: []corev1.LocalObjectReference{
			{Name: "pull-secret"},
		},
	}

	assertStringSliceEqual(t, SecretNamesForObject(sa), []string{"pull-secret", "token-secret"})

	usages := usageReferencesForObject(sa, "token-secret")
	if len(usages) != 2 {
		t.Fatalf("expected both distinct service account field references, got %d", len(usages))
	}
}

func containerWithSecretEnv(name, secretName, key string, optional *bool) corev1.Container {
	return corev1.Container{
		Name: name,
		Env: []corev1.EnvVar{
			{
				Name: "SECRET",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
						Key:                  key,
						Optional:             optional,
					},
				},
			},
		},
	}
}

func podSpecWithSecret(secretName string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			containerWithSecretEnv("app", secretName, "token", nil),
		},
	}
}

func assertStringSliceEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSecretNamesForInTreeVolumeSecretRefs(t *testing.T) {
	pod := &corev1.Pod{}
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "csi", VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{
			Driver:               "secrets-store.csi.k8s.io",
			NodePublishSecretRef: &corev1.LocalObjectReference{Name: "csi-secret"},
		}}},
		{Name: "azure", VolumeSource: corev1.VolumeSource{AzureFile: &corev1.AzureFileVolumeSource{
			SecretName: "azure-secret", ShareName: "share",
		}}},
		{Name: "cephfs", VolumeSource: corev1.VolumeSource{CephFS: &corev1.CephFSVolumeSource{
			SecretRef: &corev1.LocalObjectReference{Name: "cephfs-secret"},
		}}},
		{Name: "iscsi", VolumeSource: corev1.VolumeSource{ISCSI: &corev1.ISCSIVolumeSource{
			SecretRef: &corev1.LocalObjectReference{Name: "iscsi-secret"},
		}}},
		{Name: "rbd", VolumeSource: corev1.VolumeSource{RBD: &corev1.RBDVolumeSource{
			SecretRef: &corev1.LocalObjectReference{Name: "rbd-secret"},
		}}},
		{Name: "flex", VolumeSource: corev1.VolumeSource{FlexVolume: &corev1.FlexVolumeSource{
			SecretRef: &corev1.LocalObjectReference{Name: "flex-secret"},
		}}},
		{Name: "cinder", VolumeSource: corev1.VolumeSource{Cinder: &corev1.CinderVolumeSource{
			SecretRef: &corev1.LocalObjectReference{Name: "cinder-secret"},
		}}},
		{Name: "scaleio", VolumeSource: corev1.VolumeSource{ScaleIO: &corev1.ScaleIOVolumeSource{
			SecretRef: &corev1.LocalObjectReference{Name: "scaleio-secret"},
		}}},
		{Name: "storageos", VolumeSource: corev1.VolumeSource{StorageOS: &corev1.StorageOSVolumeSource{
			SecretRef: &corev1.LocalObjectReference{Name: "storageos-secret"},
		}}},
	}

	assertStringSliceEqual(t, SecretNamesForObject(pod), []string{
		"azure-secret",
		"cephfs-secret",
		"cinder-secret",
		"csi-secret",
		"flex-secret",
		"iscsi-secret",
		"rbd-secret",
		"scaleio-secret",
		"storageos-secret",
	})

	usages := usageReferencesForObject(pod, "csi-secret")
	if len(usages) != 1 || usages[0].FieldPath != ".spec.volumes[0].csi.nodePublishSecretRef.name" {
		t.Fatalf("unexpected csi usage: %#v", usages)
	}
}

func TestSecretNamesForIngress(t *testing.T) {
	ing := &networkingv1.Ingress{}
	ing.Spec.TLS = []networkingv1.IngressTLS{
		{Hosts: []string{"a.example.com"}, SecretName: "a-tls"},
		{Hosts: []string{"b.example.com"}, SecretName: "b-tls"},
		{Hosts: []string{"c.example.com"}}, // no secretName: default certificate
	}

	assertStringSliceEqual(t, SecretNamesForObject(ing), []string{"a-tls", "b-tls"})

	usages := usageReferencesForObject(ing, "b-tls")
	if len(usages) != 1 || usages[0].FieldPath != ".spec.tls[1].secretName" {
		t.Fatalf("unexpected ingress usage: %#v", usages)
	}
	if usages[0].APIVersion != "networking.k8s.io/v1" || usages[0].Kind != "Ingress" {
		t.Fatalf("unexpected group/kind: %#v", usages[0])
	}
}

func TestPodIsCoveredByController(t *testing.T) {
	controller := true
	notController := false

	cases := []struct {
		name  string
		owner *metav1.OwnerReference
		want  bool
	}{
		{name: "standalone pod", owner: nil, want: false},
		{
			name:  "controlled by ReplicaSet",
			owner: &metav1.OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", Controller: &controller},
			want:  true,
		},
		{
			name:  "controlled by Job",
			owner: &metav1.OwnerReference{APIVersion: "batch/v1", Kind: "Job", Name: "job", Controller: &controller},
			want:  true,
		},
		{
			name:  "controlled by an untracked custom resource",
			owner: &metav1.OwnerReference{APIVersion: "example.com/v1", Kind: "MyWorkload", Name: "mw", Controller: &controller},
			want:  false,
		},
		{
			// A same-named kind in a different group must not be mistaken for the
			// built-in controller.
			name:  "same kind in a foreign group",
			owner: &metav1.OwnerReference{APIVersion: "example.com/v1", Kind: "Job", Name: "job", Controller: &controller},
			want:  false,
		},
		{
			name:  "non-controller owner reference",
			owner: &metav1.OwnerReference{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs", Controller: &notController},
			want:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{}
			if tc.owner != nil {
				pod.OwnerReferences = []metav1.OwnerReference{*tc.owner}
			}
			if got := PodIsCoveredByController(pod); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDesiredLabelsForOmitsOverlongSecretName(t *testing.T) {
	longName := strings.Repeat("a", 100)
	labels := desiredLabelsFor(longName, true, 0)
	if _, ok := labels[usagev1alpha1.SecretNameLabel]; ok {
		t.Fatal("a Secret name longer than 63 characters is not a valid label value and must be omitted")
	}
	if labels[usagev1alpha1.UnusedLabel] != "true" {
		t.Fatalf("expected unused label, got %v", labels)
	}
}
