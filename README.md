# SecretUsage Controller

SecretUsage is a Kubernetes controller that maintains a **live reverse index of Secret
references**. For every Secret it tracks, it keeps one namespaced `SecretUsage` custom
resource (short name `su`) describing exactly which objects reference that Secret and
through which field.

It answers three questions that are awkward to answer any other way:

| Question | How |
| --- | --- |
| What breaks if I delete this Secret? | `kubectl get su <name> -n <ns> -o yaml` |
| Which references point at a Secret that does not exist? | `kubectl get su -A -l usage.secretusage.io/missing=true` |
| Which Secrets is nothing using? | `kubectl get su -A -l usage.secretusage.io/unused=true` |

The controller records references only. It never reads, stores, or logs Secret values —
see [Security model](#security-model) for how that is enforced rather than promised.

## Why a controller instead of a kubectl one-liner

You can extract this with `kubectl` and `jq`. The reasons not to:

- **It is a reverse index.** A `jq` query is `O(every workload in the cluster)` each
  time you ask. Here the answer is already computed and keyed by Secret name.
- **Correct coverage is genuinely hard.** A Secret can be referenced from container
  `env`, `envFrom`, Secret volumes, projected volumes, nine different in-tree volume
  `secretRef` fields, `imagePullSecrets`, ServiceAccount `secrets` and
  `imagePullSecrets`, and `Ingress` TLS — across ten object kinds, three of which nest
  the pod spec at different depths. Most hand-written queries miss half of these.
- **Dangling references become alertable.** `.status.exists == false` with a non-empty
  `.status.usages` means "this will fail the next time that workload restarts." A
  point-in-time query cannot page you; a Prometheus gauge can.

Prior art worth knowing about: [Kor](https://github.com/yonahd/kor) and
[Popeye](https://github.com/derailed/popeye) both report unused Secrets as one-shot
scans. This project overlaps with them there. What it adds is a continuously
maintained, queryable API object and per-Secret metrics.

## Tracked references

| Kind | Fields |
| --- | --- |
| Pod, Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, ReplicationController | container/initContainer/ephemeralContainer `env[].valueFrom.secretKeyRef`, `envFrom[].secretRef`, `volumes[].secret`, `volumes[].projected.sources[].secret`, `imagePullSecrets[]` |
| …and the volume `secretRef` fields | `csi.nodePublishSecretRef`, `azureFile.secretName`, `cephfs`, `cinder`, `flexVolume`, `iscsi`, `rbd`, `scaleIO`, `storageos` |
| ServiceAccount | `secrets[]`, `imagePullSecrets[]` |
| Ingress | `spec.tls[].secretName` |

Every entry in `.status.usages` carries the exact `fieldPath` (for example
`.spec.template.spec.containers[0].env[2].valueFrom.secretKeyRef.name`), the container
name where applicable, the referenced `key`, and whether the reference is `optional`.

Secrets referenced from custom resources — cert-manager `Certificate`, ExternalSecrets,
Istio gateway `credentialName`, and so on — are **not** tracked. See
[Limitations](#limitations).

## Install

```sh
helm install secretusage-controller \
  oci://registry-1.docker.io/sathvikm2002/secretusage-controller \
  --version 0.2.0 \
  --namespace secretusage-system \
  --create-namespace
```

Or from a local checkout:

```sh
helm install secretusage-controller ./charts/secretusage-controller \
  --namespace secretusage-system \
  --create-namespace \
  --set image.repository=docker.io/sathvikm2002/secretusage \
  --set image.tag=latest
```

### Scoping to one namespace

```sh
helm install secretusage-controller ./charts/secretusage-controller \
  --namespace secretusage-system \
  --create-namespace \
  --set watchNamespace=default
```

Setting `watchNamespace` narrows both the watch **and** the permissions: the chart
renders a namespaced `Role` instead of a `ClusterRole`, so the install is not able to
read Secrets anywhere outside that namespace.

## Usage

```sh
# Everything the controller tracks
kubectl get su -A

# References that will fail on next restart
kubectl get su -A -l usage.secretusage.io/missing=true

# Secrets nothing references
kubectl get su -A -l usage.secretusage.io/unused=true

# Exactly what references one Secret
kubectl get su db-credentials -n default \
  -o jsonpath='{range .status.usages[*]}{.kind}/{.name}{"\t"}{.fieldPath}{"\n"}{end}'
```

```
NAME              SECRET            EXISTS   USAGES   AGE
db-credentials    db-credentials    true     3        4d
registry-pull     registry-pull     true     41       4d
stale-tls         stale-tls         false    1        4d
```

## Metrics

Three gauges per tracked Secret, plus two aggregates. The per-Secret gauges record raw
state rather than pre-computed conditions, so conditions are derived in PromQL and the
series count stays predictable.

| Metric | Meaning |
| --- | --- |
| `secretusage_secret_references{namespace,secret}` | Number of references found |
| `secretusage_secret_exists{namespace,secret}` | 1 if the Secret exists, 0 if not |
| `secretusage_secret_usages_truncated{namespace,secret}` | 1 if there are more references than recorded in status |
| `secretusage_missing_secrets` | Cluster-wide count of referenced-but-absent Secrets |
| `secretusage_unused_secrets` | Cluster-wide count of unreferenced Secrets |

```yaml
groups:
  - name: secretusage
    rules:
      - alert: SecretReferencedButMissing
        expr: secretusage_secret_exists == 0 and secretusage_secret_references > 0
        for: 10m
        annotations:
          summary: "Secret {{ $labels.namespace }}/{{ $labels.secret }} is referenced but does not exist"

      - alert: SecretUsageStatusTruncated
        expr: secretusage_secret_usages_truncated == 1
        for: 30m
        annotations:
          summary: "More references to {{ $labels.namespace }}/{{ $labels.secret }} than status records"
```

Per-Secret gauges cost three series per Secret. On very large clusters set
`metrics.perSecret=false`; the aggregate gauges remain.

## Security model

The controller holds **cluster-wide read access to Secret metadata**, which is the
minimum needed to know whether a Secret exists. Three things make that a bounded risk
rather than a stated intention:

1. **Secrets are watched as `PartialObjectMetadata`.** The API server is asked for
   metadata only, so Secret values never cross the wire and never enter the
   controller's cache. A heap dump of this process contains no Secret data.
2. **RBAC grants `list` and `watch` on Secrets, not `get`.** Existence is answered from
   the metadata informer, so the permission that would let it fetch a Secret body is
   not granted at all.
3. **A cache transform strips `data` and `stringData`** from any typed Secret informer,
   as defense in depth if one is ever introduced.

The container runs as non-root from a distroless base with a read-only root filesystem,
all capabilities dropped, and `seccompProfile: RuntimeDefault`.

`SecretUsage` objects contain Secret *names*, *field paths*, and *key names* — never
values. Note that key names are mildly revealing (`.status.usages[].key` may say
`aws_secret_access_key`), so treat read access to `SecretUsage` as roughly equivalent
to read access to pod specs.

## Design notes

These are the decisions that matter if you plan to run this at scale.

**Owned Pods are skipped by default.** A Pod's Secret references always come from its
controller's pod template, so recording both is redundant — and Pods churn on every
rollout, which would rewrite `SecretUsage` status hundreds of times per deploy and turn
this controller into a source of etcd write amplification. Pods controlled by a tracked
`ReplicaSet`, `StatefulSet`, `DaemonSet`, `Job`, or `ReplicationController` are
therefore filtered, in both the event handler and the collector. **Standalone Pods, and
Pods owned by anything not in that list, are always recorded**, so nothing is silently
lost. Set `tracking.ownedPods=true` to record them anyway.

Because ReplicaSets are still tracked, an in-progress or rolled-back Deployment still
shows the old revision's Secret references — which is correct, since Pods from the old
ReplicaSet are still running.

**Status lists are capped.** A registry pull Secret referenced by every ServiceAccount
in a large cluster can produce thousands of references. Past roughly 1.5MB an object
cannot be written to etcd at all, so `tracking.maxUsages` (default 500) bounds the
recorded list. `.status.usageCount` always reports the true total and
`.status.truncated` marks objects where the list was cut short, so the count never lies
about what it found.

**Unused Secrets need an object to exist.** "Nothing references this Secret" cannot be
represented by the absence of a `SecretUsage`, because absence is indistinguishable
from "the controller has not seen it yet" or "the controller is down." So a Secret that
exists with zero references still gets an object, with `usageCount: 0` and the
`unused=true` label. That costs one object per Secret; set
`tracking.unusedSecrets=false` to trade the unused-Secret report for the smaller
footprint. An object is deleted only once neither the Secret nor any reference to it
remains.

**Reconciles are idempotent and write-free when nothing changed.** Status is compared
before patching, and the `SecretUsage` watch deliberately handles only Create and
Delete — watching Updates would mean every status write the controller makes
re-enqueues its own key. Create still validates objects seen at startup, and Delete
restores an object a user removed by hand.

**Aggregate metrics are recomputed on a 30s timer** under leader election, rather than
per reconcile, so startup does not become `O(n²)` in the number of tracked Secrets.

## Configuration

| Value | Default | Purpose |
| --- | --- | --- |
| `watchNamespace` | `""` (all) | Restrict watch and RBAC to one namespace |
| `tracking.maxUsages` | `500` | Cap on references recorded per object |
| `tracking.ownedPods` | `false` | Record Pods already covered by their controller |
| `tracking.unusedSecrets` | `true` | Keep objects for unreferenced Secrets |
| `metrics.perSecret` | `true` | Export per-Secret gauges |
| `leaderElection.enabled` | `true` | Run one active replica |

Each maps to a flag on the manager (`--max-usages`, `--track-owned-pods`,
`--track-unused-secrets`, `--per-secret-metrics`).

## Limitations

- **Custom resources are not tracked.** cert-manager `Certificate.spec.secretName`,
  ExternalSecrets, Istio `credentialName`, ArgoCD repository credentials, and Prometheus
  operator Secret fields are invisible to it. Enumerating these in Go does not scale;
  the intended fix is a config-driven extractor taking GVK + JSONPath rules, which is
  not implemented yet.
- **References, not usage.** This tracks which objects *name* a Secret. It cannot tell
  you who actually *read* one — that only comes from API server audit logs.
- **`usageCount` counts references, not workloads.** A Deployment and its ReplicaSets
  each count separately, so one logical workload contributes more than one.
- Secrets consumed by a kubelet-level mechanism outside a pod spec are not visible.

## Development

```sh
make test           # fmt, vet, unit tests
make manifests      # regenerate CRD and RBAC from kubebuilder markers
make helm-lint
make helm-template
```
