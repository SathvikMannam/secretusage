package controller

import (
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	usagev1alpha1 "github.com/SathvikMannam/secretusage/api/v1alpha1"
)

// SecretNameIndexField is the field index used to find objects that reference a Secret.
const SecretNameIndexField = ".metadata.secretusage.secretNames"

type objectSecretReference struct {
	SecretName string
	FieldPath  string
	Container  string
	Key        string
	Optional   *bool
}

// podControllerKinds are the controller kinds whose pod templates this controller
// already tracks. A Pod controlled by one of them adds no information that the
// controller object does not already carry, so its references can be skipped to
// avoid rewriting SecretUsage status on every pod replacement.
var podControllerKinds = map[string]struct{}{
	"apps/v1|ReplicaSet":       {},
	"apps/v1|StatefulSet":      {},
	"apps/v1|DaemonSet":        {},
	"batch/v1|Job":             {},
	"v1|ReplicationController": {},
}

// PodIsCoveredByController reports whether a Pod's Secret references are already
// recorded through a tracked controller object. Standalone Pods, and Pods owned by
// something this controller does not track, always return false so their references
// are never lost.
func PodIsCoveredByController(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Controller == nil || !*owner.Controller {
			continue
		}
		if _, ok := podControllerKinds[owner.APIVersion+"|"+owner.Kind]; ok {
			return true
		}
	}
	return false
}

// SecretNamesForObject returns sorted unique Secret names referenced by obj.
func SecretNamesForObject(obj client.Object) []string {
	return uniqueSortedSecretNames(referencesForObject(obj))
}

// uniqueSortedSecretNames collapses references to the distinct Secret names they
// point at, which is the shape the field index needs.
func uniqueSortedSecretNames(refs []objectSecretReference) []string {
	if len(refs) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(refs))
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.SecretName == "" {
			continue
		}
		if _, ok := seen[ref.SecretName]; ok {
			continue
		}
		seen[ref.SecretName] = struct{}{}
		names = append(names, ref.SecretName)
	}
	sort.Strings(names)
	return names
}

func usageReferencesForObject(obj client.Object, secretName string) []usagev1alpha1.SecretUsageReference {
	refs := referencesForObject(obj)
	if len(refs) == 0 {
		return nil
	}

	apiVersion, kind := apiVersionKindForObject(obj)
	usages := make([]usagev1alpha1.SecretUsageReference, 0, len(refs))
	for _, ref := range refs {
		if ref.SecretName != secretName {
			continue
		}
		usages = append(usages, usagev1alpha1.SecretUsageReference{
			APIVersion: apiVersion,
			Kind:       kind,
			Namespace:  obj.GetNamespace(),
			Name:       obj.GetName(),
			UID:        string(obj.GetUID()),
			FieldPath:  ref.FieldPath,
			Container:  ref.Container,
			Key:        ref.Key,
			Optional:   ref.Optional,
		})
	}
	return usages
}

func referencesForObject(obj client.Object) []objectSecretReference {
	switch typed := obj.(type) {
	case *corev1.Pod:
		return referencesForPodSpec(&typed.Spec, ".spec")
	case *appsv1.Deployment:
		return referencesForPodSpec(&typed.Spec.Template.Spec, ".spec.template.spec")
	case *appsv1.StatefulSet:
		return referencesForPodSpec(&typed.Spec.Template.Spec, ".spec.template.spec")
	case *appsv1.DaemonSet:
		return referencesForPodSpec(&typed.Spec.Template.Spec, ".spec.template.spec")
	case *appsv1.ReplicaSet:
		return referencesForPodSpec(&typed.Spec.Template.Spec, ".spec.template.spec")
	case *batchv1.Job:
		return referencesForPodSpec(&typed.Spec.Template.Spec, ".spec.template.spec")
	case *batchv1.CronJob:
		return referencesForPodSpec(&typed.Spec.JobTemplate.Spec.Template.Spec, ".spec.jobTemplate.spec.template.spec")
	case *corev1.ReplicationController:
		if typed.Spec.Template == nil {
			return nil
		}
		return referencesForPodSpec(&typed.Spec.Template.Spec, ".spec.template.spec")
	case *corev1.ServiceAccount:
		return referencesForServiceAccount(typed)
	case *networkingv1.Ingress:
		return referencesForIngress(typed)
	default:
		return nil
	}
}

func referencesForPodSpec(spec *corev1.PodSpec, prefix string) []objectSecretReference {
	var refs []objectSecretReference

	for i, pullSecret := range spec.ImagePullSecrets {
		refs = appendReference(refs, objectSecretReference{
			SecretName: pullSecret.Name,
			FieldPath:  fmt.Sprintf("%s.imagePullSecrets[%d].name", prefix, i),
		})
	}

	for i := range spec.Volumes {
		refs = append(refs, referencesForVolume(&spec.Volumes[i], fmt.Sprintf("%s.volumes[%d]", prefix, i))...)
	}

	for i := range spec.InitContainers {
		container := &spec.InitContainers[i]
		refs = append(refs, referencesForContainer(container.Name, &container.Env, &container.EnvFrom, fmt.Sprintf("%s.initContainers[%d]", prefix, i))...)
	}
	for i := range spec.Containers {
		container := &spec.Containers[i]
		refs = append(refs, referencesForContainer(container.Name, &container.Env, &container.EnvFrom, fmt.Sprintf("%s.containers[%d]", prefix, i))...)
	}
	for i := range spec.EphemeralContainers {
		container := &spec.EphemeralContainers[i]
		refs = append(refs, referencesForContainer(container.Name, &container.Env, &container.EnvFrom, fmt.Sprintf("%s.ephemeralContainers[%d]", prefix, i))...)
	}

	return dedupeReferences(refs)
}

// referencesForVolume covers every volume source that names a Secret, including the
// in-tree storage drivers whose secretRef fields are easy to miss and whose failure
// mode is a volume that will not mount.
func referencesForVolume(volume *corev1.Volume, volumePath string) []objectSecretReference {
	var refs []objectSecretReference

	if volume.Secret != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.Secret.SecretName,
			FieldPath:  volumePath + ".secret.secretName",
			Optional:   copyBoolPtr(volume.Secret.Optional),
		})
	}
	if volume.Projected != nil {
		for j, source := range volume.Projected.Sources {
			if source.Secret == nil {
				continue
			}
			refs = appendReference(refs, objectSecretReference{
				SecretName: source.Secret.Name,
				FieldPath:  fmt.Sprintf("%s.projected.sources[%d].secret.name", volumePath, j),
				Optional:   copyBoolPtr(source.Secret.Optional),
			})
		}
	}
	if volume.CSI != nil && volume.CSI.NodePublishSecretRef != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.CSI.NodePublishSecretRef.Name,
			FieldPath:  volumePath + ".csi.nodePublishSecretRef.name",
		})
	}
	if volume.AzureFile != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.AzureFile.SecretName,
			FieldPath:  volumePath + ".azureFile.secretName",
		})
	}
	if volume.CephFS != nil && volume.CephFS.SecretRef != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.CephFS.SecretRef.Name,
			FieldPath:  volumePath + ".cephfs.secretRef.name",
		})
	}
	if volume.Cinder != nil && volume.Cinder.SecretRef != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.Cinder.SecretRef.Name,
			FieldPath:  volumePath + ".cinder.secretRef.name",
		})
	}
	if volume.FlexVolume != nil && volume.FlexVolume.SecretRef != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.FlexVolume.SecretRef.Name,
			FieldPath:  volumePath + ".flexVolume.secretRef.name",
		})
	}
	if volume.ISCSI != nil && volume.ISCSI.SecretRef != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.ISCSI.SecretRef.Name,
			FieldPath:  volumePath + ".iscsi.secretRef.name",
		})
	}
	if volume.RBD != nil && volume.RBD.SecretRef != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.RBD.SecretRef.Name,
			FieldPath:  volumePath + ".rbd.secretRef.name",
		})
	}
	if volume.ScaleIO != nil && volume.ScaleIO.SecretRef != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.ScaleIO.SecretRef.Name,
			FieldPath:  volumePath + ".scaleIO.secretRef.name",
		})
	}
	if volume.StorageOS != nil && volume.StorageOS.SecretRef != nil {
		refs = appendReference(refs, objectSecretReference{
			SecretName: volume.StorageOS.SecretRef.Name,
			FieldPath:  volumePath + ".storageos.secretRef.name",
		})
	}

	return refs
}

func referencesForContainer(containerName string, envVars *[]corev1.EnvVar, envFromSources *[]corev1.EnvFromSource, prefix string) []objectSecretReference {
	var refs []objectSecretReference

	for i, env := range *envVars {
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			continue
		}
		refs = appendReference(refs, objectSecretReference{
			SecretName: env.ValueFrom.SecretKeyRef.Name,
			FieldPath:  fmt.Sprintf("%s.env[%d].valueFrom.secretKeyRef.name", prefix, i),
			Container:  containerName,
			Key:        env.ValueFrom.SecretKeyRef.Key,
			Optional:   copyBoolPtr(env.ValueFrom.SecretKeyRef.Optional),
		})
	}

	for i, envFrom := range *envFromSources {
		if envFrom.SecretRef == nil {
			continue
		}
		refs = appendReference(refs, objectSecretReference{
			SecretName: envFrom.SecretRef.Name,
			FieldPath:  fmt.Sprintf("%s.envFrom[%d].secretRef.name", prefix, i),
			Container:  containerName,
			Optional:   copyBoolPtr(envFrom.SecretRef.Optional),
		})
	}

	return refs
}

func referencesForServiceAccount(sa *corev1.ServiceAccount) []objectSecretReference {
	refs := make([]objectSecretReference, 0, len(sa.Secrets)+len(sa.ImagePullSecrets))
	for i, secret := range sa.Secrets {
		refs = appendReference(refs, objectSecretReference{
			SecretName: secret.Name,
			FieldPath:  fmt.Sprintf(".secrets[%d].name", i),
		})
	}
	for i, secret := range sa.ImagePullSecrets {
		refs = appendReference(refs, objectSecretReference{
			SecretName: secret.Name,
			FieldPath:  fmt.Sprintf(".imagePullSecrets[%d].name", i),
		})
	}
	return dedupeReferences(refs)
}

// referencesForIngress tracks TLS certificate Secrets. A missing Ingress TLS Secret
// is one of the highest-impact dangling references in a cluster because it breaks
// termination for every host in the rule.
func referencesForIngress(ingress *networkingv1.Ingress) []objectSecretReference {
	refs := make([]objectSecretReference, 0, len(ingress.Spec.TLS))
	for i, tls := range ingress.Spec.TLS {
		refs = appendReference(refs, objectSecretReference{
			SecretName: tls.SecretName,
			FieldPath:  fmt.Sprintf(".spec.tls[%d].secretName", i),
		})
	}
	return dedupeReferences(refs)
}

func appendReference(refs []objectSecretReference, ref objectSecretReference) []objectSecretReference {
	if ref.SecretName == "" {
		return refs
	}
	return append(refs, ref)
}

func dedupeReferences(refs []objectSecretReference) []objectSecretReference {
	if len(refs) < 2 {
		return refs
	}

	seen := make(map[string]struct{}, len(refs))
	out := make([]objectSecretReference, 0, len(refs))
	for _, ref := range refs {
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%v", ref.SecretName, ref.FieldPath, ref.Container, ref.Key, boolPtrValue(ref.Optional))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, ref)
	}
	return out
}

func copyBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func boolPtrValue(value *bool) string {
	if value == nil {
		return "<nil>"
	}
	if *value {
		return "true"
	}
	return "false"
}

func apiVersionKindForObject(obj client.Object) (string, string) {
	switch obj.(type) {
	case *corev1.Pod:
		return corev1.SchemeGroupVersion.String(), "Pod"
	case *corev1.ReplicationController:
		return corev1.SchemeGroupVersion.String(), "ReplicationController"
	case *corev1.ServiceAccount:
		return corev1.SchemeGroupVersion.String(), "ServiceAccount"
	case *appsv1.Deployment:
		return appsv1.SchemeGroupVersion.String(), "Deployment"
	case *appsv1.StatefulSet:
		return appsv1.SchemeGroupVersion.String(), "StatefulSet"
	case *appsv1.DaemonSet:
		return appsv1.SchemeGroupVersion.String(), "DaemonSet"
	case *appsv1.ReplicaSet:
		return appsv1.SchemeGroupVersion.String(), "ReplicaSet"
	case *batchv1.Job:
		return batchv1.SchemeGroupVersion.String(), "Job"
	case *batchv1.CronJob:
		return batchv1.SchemeGroupVersion.String(), "CronJob"
	case *networkingv1.Ingress:
		return networkingv1.SchemeGroupVersion.String(), "Ingress"
	default:
		gvk := obj.GetObjectKind().GroupVersionKind()
		return gvk.GroupVersion().String(), gvk.Kind
	}
}

func usageReferenceLess(a, b usagev1alpha1.SecretUsageReference) bool {
	left := []string{a.APIVersion, a.Kind, a.Namespace, a.Name, a.FieldPath, a.Container, a.Key, boolPtrValue(a.Optional), a.UID}
	right := []string{b.APIVersion, b.Kind, b.Namespace, b.Name, b.FieldPath, b.Container, b.Key, boolPtrValue(b.Optional), b.UID}
	for i := range left {
		if left[i] == right[i] {
			continue
		}
		return left[i] < right[i]
	}
	return false
}
