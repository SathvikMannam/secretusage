package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
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
