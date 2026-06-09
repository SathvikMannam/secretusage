# SecretUsage Controller

SecretUsage is a Kubernetes controller that tracks where Secrets are referenced by common workload objects and ServiceAccounts. It maintains one namespaced `SecretUsage` custom resource per referenced Secret, with short name `su`.

The controller records references only. It does not write Secret values into status.

## Tracked Objects

- Pods
- Deployments
- StatefulSets
- DaemonSets
- Jobs
- CronJobs
- ReplicaSets
- ReplicationControllers
- ServiceAccounts

Tracked reference paths include container `env`, `envFrom`, Secret volumes, projected Secret volumes, `imagePullSecrets`, ServiceAccount `secrets`, and ServiceAccount `imagePullSecrets`.

## Install With Helm

Install the published OCI chart:

```sh
helm install secretusage-controller \
  oci://registry-1.docker.io/sathvikm2002/secretusage-controller \
  --version 0.1.0 \
  --namespace secretusage-system \
  --create-namespace
```

Or install from a local checkout:

```sh
helm install secretusage-controller ./charts/secretusage-controller \
  --namespace secretusage-system \
  --create-namespace \
  --set image.repository=docker.io/sathvikm2002/secretusage \
  --set image.tag=latest
```

List usage records:

```sh
kubectl get secretusage -A
kubectl get su -A
```

Watch one namespace only:

```sh
helm install secretusage-controller ./charts/secretusage-controller \
  --namespace secretusage-system \
  --create-namespace \
  --set watchNamespace=default
```

## Security Notes

The controller needs `get`, `list`, and `watch` on Secrets so it can react when a Secret is created or deleted and keep `.status.exists` accurate. Kubernetes Secret read permissions can expose Secret data to a client. This controller does not persist Secret data in `SecretUsage` objects.

## Development

```sh
go test ./...
make manifests
helm lint charts/secretusage-controller
helm template secretusage-controller charts/secretusage-controller
```
