#!/usr/bin/env bash
#
# End-to-end lab test for the SecretUsage controller.
#
# Installs the chart into a throwaway namespace, creates workloads that reference
# Secrets every way the controller tracks, and asserts the resulting index. Every
# assertion polls, because reconciliation is asynchronous.
#
#   ./hack/lab-test.sh              # run everything, then clean up
#   KEEP=1 ./hack/lab-test.sh       # leave the lab in place to poke at
#   ./hack/lab-test.sh cleanup      # remove a lab left behind by KEEP=1
#   ./hack/lab-test.sh index        # run one phase (see PHASES below)
#
# Namespaces default to names that will not collide with a hand-made lab, so this is
# safe to run next to one. It is not safe to run next to another cluster-wide
# SecretUsage controller: both would reconcile the same objects and fight over status.
# Preflight refuses to start in that case.

set -euo pipefail

RELEASE=${RELEASE:-su-labtest}
CTRL_NS=${CTRL_NS:-su-labtest-system}
LAB_NS=${LAB_NS:-su-labtest}
CHART=${CHART:-charts/secretusage-controller}
CHART_VERSION=${CHART_VERSION:-}
CRD_GROUP=${CRD_GROUP:-sulabtest.example.com}
PF_PORT=${PF_PORT:-18080}
TIMEOUT=${TIMEOUT:-120}
KEEP=${KEEP:-0}
ALLOW_CONFLICT=${ALLOW_CONFLICT:-0}

PHASES=(preflight install workloads index ownedpods unused dangling truncation metrics rules rbac)

readonly RED=$'\033[31m' GREEN=$'\033[32m' YELLOW=$'\033[33m' BOLD=$'\033[1m' OFF=$'\033[0m'
FAILURES=0

step() { printf '\n%s==> %s%s\n' "$BOLD" "$*" "$OFF"; }
ok()   { printf '  %s✓%s %s\n' "$GREEN" "$OFF" "$*"; }
warn() { printf '  %s!%s %s\n' "$YELLOW" "$OFF" "$*"; }
bad()  { printf '  %s✗%s %s\n' "$RED" "$OFF" "$*"; FAILURES=$((FAILURES + 1)); }
die()  { printf '\n%serror:%s %s\n' "$RED" "$OFF" "$*" >&2; exit 1; }

# Every helm invocation goes through this. fullnameOverride pins the resource names to
# $RELEASE, because the chart would otherwise prefix them with the chart name and every
# "deploy/$RELEASE" reference below would miss. It is repeated on upgrades too, since
# helm upgrade resets values that are not passed again.
helm_args() {
  local args=("$CHART" --namespace "$CTRL_NS" --set "fullnameOverride=$RELEASE")
  [[ -n "$CHART_VERSION" ]] && args+=(--version "$CHART_VERSION")
  printf '%s\n' "${args[@]}"
}

# expect DESC EXPECTED COMMAND...  — polls COMMAND until its output equals EXPECTED.
expect() {
  local desc=$1 expected=$2
  shift 2
  local deadline=$((SECONDS + TIMEOUT)) actual=''
  while :; do
    actual=$(eval "$*" 2>/dev/null || true)
    if [[ "$actual" == "$expected" ]]; then
      ok "$desc → $actual"
      return 0
    fi
    ((SECONDS >= deadline)) && break
    sleep 2
  done
  bad "$desc: expected '$expected', got '$actual'"
  return 0 # keep going so one failure does not hide the rest
}

# expect_min DESC MINIMUM COMMAND...  — for counts that legitimately grow, such as a
# CronJob that fires mid-run and adds a Job reference.
expect_min() {
  local desc=$1 minimum=$2
  shift 2
  local deadline=$((SECONDS + TIMEOUT)) actual=''
  while :; do
    actual=$(eval "$*" 2>/dev/null || echo 0)
    if [[ "${actual:-0}" =~ ^[0-9]+$ ]] && ((actual >= minimum)); then
      ok "$desc → $actual (>= $minimum)"
      return 0
    fi
    ((SECONDS >= deadline)) && break
    sleep 2
  done
  bad "$desc: expected at least $minimum, got '$actual'"
  return 0
}

su_field() { kubectl get su "$1" -n "$LAB_NS" -o jsonpath="$2" 2>/dev/null; }

# refs_of NAME KIND — how many references to Secret NAME come from objects of KIND.
refs_of() {
  kubectl get su "$1" -n "$LAB_NS" -o jsonpath="{range .status.usages[?(@.kind=='$2')]}{.name}{'\n'}{end}" 2>/dev/null | grep -c . || true
}

controller_ready() {
  kubectl -n "$CTRL_NS" rollout status "deploy/$RELEASE" --timeout="${TIMEOUT}s" >/dev/null
}

# ---------------------------------------------------------------------------------

preflight() {
  step "Preflight"
  for tool in kubectl helm; do
    command -v "$tool" >/dev/null || die "$tool not found on PATH"
  done
  kubectl cluster-info --request-timeout=10s >/dev/null 2>&1 || die "no reachable cluster"
  ok "cluster: $(kubectl config current-context)"

  # Another controller watching these namespaces would reconcile the same SecretUsage
  # objects with its own settings, so the truncation phase in particular would flap.
  local others
  others=$(kubectl get deploy -A \
    -l app.kubernetes.io/name=secretusage-controller \
    -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' 2>/dev/null |
    grep -v "^${CTRL_NS}/" || true)
  if [[ -n "$others" ]]; then
    if [[ "$ALLOW_CONFLICT" == "1" ]]; then
      warn "other controller(s) running, results may flap: $(tr '\n' ' ' <<<"$others")"
    else
      die "another SecretUsage controller is running:
$(sed 's/^/    /' <<<"$others")
  Two controllers reconcile the same SecretUsage objects and will fight over status.
  Scale it down first, or re-run with ALLOW_CONFLICT=1 to accept flapping results."
    fi
  fi

  for ns in "$CTRL_NS" "$LAB_NS"; do
    if kubectl get ns "$ns" >/dev/null 2>&1; then
      die "namespace $ns already exists. Run '$0 cleanup' first, or set CTRL_NS/LAB_NS."
    fi
  done
  ok "namespaces $CTRL_NS and $LAB_NS are free"
}

install() {
  step "Installing chart"
  local args
  mapfile -t args < <(helm_args)
  helm install "$RELEASE" "${args[@]}" --create-namespace >/dev/null
  controller_ready
  ok "controller running from $(kubectl -n "$CTRL_NS" get deploy "$RELEASE" -o jsonpath='{.spec.template.spec.containers[0].image}')"
}

workloads() {
  step "Creating lab workloads in $LAB_NS"
  kubectl create ns "$LAB_NS" >/dev/null
  kubectl apply -n "$LAB_NS" -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata: {name: db-creds}
stringData: {username: app, password: hunter2}
---
apiVersion: v1
kind: Secret
metadata: {name: pull-creds}
type: kubernetes.io/dockerconfigjson
stringData: {.dockerconfigjson: '{"auths":{}}'}
---
apiVersion: v1
kind: Secret
metadata: {name: web-tls}
stringData: {tls.crt: fake, tls.key: fake}
---
apiVersion: v1
kind: Secret
metadata: {name: nobody-uses-me}
stringData: {key: value}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  replicas: 1
  selector: {matchLabels: {app: api}}
  template:
    metadata: {labels: {app: api}}
    spec:
      containers:
        - name: app
          image: registry.k8s.io/pause:3.9
          env:
            - name: DB_PASSWORD
              valueFrom:
                secretKeyRef: {name: db-creds, key: password}
          envFrom:
            - secretRef: {name: db-creds}
          volumeMounts:
            - {name: creds, mountPath: /etc/creds, readOnly: true}
      volumes:
        - name: creds
          secret: {secretName: db-creds}
---
apiVersion: v1
kind: Pod
metadata: {name: debug}
spec:
  containers:
    - name: app
      image: registry.k8s.io/pause:3.9
      env:
        - name: DB_PASSWORD
          valueFrom:
            secretKeyRef: {name: db-creds, key: password}
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: nightly}
spec:
  schedule: "0 0 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: job
              image: registry.k8s.io/pause:3.9
              envFrom:
                - secretRef: {name: db-creds}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: puller}
imagePullSecrets:
  - name: pull-creds
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: web}
spec:
  tls:
    - hosts: [lab.example.com]
      secretName: web-tls
  rules:
    - host: lab.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend: {service: {name: web, port: {number: 80}}}
EOF
  kubectl -n "$LAB_NS" rollout status deploy/api --timeout="${TIMEOUT}s" >/dev/null
  ok "workloads applied"
}

index() {
  step "Reverse index"
  expect "SecretUsage objects in $LAB_NS" 4 \
    "kubectl get su -n $LAB_NS --no-headers | grep -c ."

  # Three reference styles in one pod template, and again in the ReplicaSet it owns.
  expect "db-creds references from Deployment" 3 "refs_of db-creds Deployment"
  expect "db-creds references from ReplicaSet" 3 "refs_of db-creds ReplicaSet"
  expect "db-creds references from CronJob" 1 "refs_of db-creds CronJob"
  # A CronJob that fires mid-run adds a Job reference, so this is a floor not an equality.
  expect_min "db-creds total usageCount" 8 "su_field db-creds '{.status.usageCount}'"

  expect "web-tls tracked via Ingress TLS" ".spec.tls[0].secretName" \
    "su_field web-tls '{.status.usages[0].fieldPath}'"
  expect "pull-creds tracked via ServiceAccount" "ServiceAccount" \
    "su_field pull-creds '{.status.usages[0].kind}'"
  expect "secret-name label" "db-creds" \
    "su_field db-creds '{.metadata.labels.usage\\.secretusage\\.io/secret-name}'"
}

ownedpods() {
  step "Owned Pods are skipped"
  # The Deployment's Pod exists but adds nothing its ReplicaSet does not already say.
  expect_min "Pods running in $LAB_NS" 2 \
    "kubectl get pods -n $LAB_NS --no-headers | grep -c ."
  expect "only the standalone Pod is indexed" "debug" \
    "kubectl get su db-creds -n $LAB_NS -o jsonpath=\"{range .status.usages[?(@.kind=='Pod')]}{.name}{'\n'}{end}\""

  local before after
  before=$(su_field db-creds '{.status.usageCount}')
  local args
  mapfile -t args < <(helm_args)
  helm upgrade "$RELEASE" "${args[@]}" --set tracking.ownedPods=true >/dev/null
  controller_ready
  expect_min "usageCount grows with tracking.ownedPods=true" $((before + 1)) \
    "su_field db-creds '{.status.usageCount}'"

  helm upgrade "$RELEASE" "${args[@]}" --set tracking.ownedPods=false >/dev/null
  controller_ready
  after=$(su_field db-creds '{.status.usageCount}')
  ok "reverted to owned-Pod filtering (usageCount $after)"
}

unused() {
  step "Unused Secrets"
  expect "nobody-uses-me exists with no references" "true 0" \
    "su_field nobody-uses-me '{.status.exists} {.status.usageCount}'"
  expect "unused label set" "true" \
    "su_field nobody-uses-me '{.metadata.labels.usage\\.secretusage\\.io/unused}'"
  expect "unused selector finds it" 1 \
    "kubectl get su -n $LAB_NS -l usage.secretusage.io/unused=true --no-headers | grep -c ."
}

dangling() {
  step "Dangling references"
  kubectl delete secret db-creds -n "$LAB_NS" >/dev/null
  expect "db-creds now reported missing" "false" "su_field db-creds '{.status.exists}'"
  expect "missing label set" "true" \
    "su_field db-creds '{.metadata.labels.usage\\.secretusage\\.io/missing}'"
  expect_min "references survive the Secret" 8 "su_field db-creds '{.status.usageCount}'"

  # The running Pods keep working because their env was injected at start. That latency
  # is the reason this is worth alerting on rather than querying.
  ok "note: existing Pods keep running; only the next restart fails"

  kubectl create secret generic db-creds -n "$LAB_NS" \
    --from-literal=username=app --from-literal=password=hunter2 >/dev/null
  expect "recovers when the Secret returns" "true" "su_field db-creds '{.status.exists}'"
}

truncation() {
  step "Status truncation"
  for i in $(seq 1 6); do
    kubectl apply -n "$LAB_NS" -f - >/dev/null <<EOF
apiVersion: v1
kind: ServiceAccount
metadata: {name: puller-$i}
imagePullSecrets:
  - name: pull-creds
EOF
  done
  expect "pull-creds picks up all ServiceAccounts" 7 "su_field pull-creds '{.status.usageCount}'"

  local args
  mapfile -t args < <(helm_args)
  helm upgrade "$RELEASE" "${args[@]}" --set tracking.maxUsages=3 >/dev/null
  controller_ready
  expect "truncated flag set" "true" "su_field pull-creds '{.status.truncated}'"
  expect "usageCount still reports the true total" 7 "su_field pull-creds '{.status.usageCount}'"
  expect "recorded list is capped" 3 \
    "kubectl get su pull-creds -n $LAB_NS -o jsonpath='{.status.usages[*].name}' | wc -w | tr -d ' '"

  helm upgrade "$RELEASE" "${args[@]}" --set tracking.maxUsages=500 >/dev/null
  controller_ready
  expect "untruncated after raising the cap" "" "su_field pull-creds '{.status.truncated}'"
}

metrics() {
  step "Metrics"
  kubectl -n "$CTRL_NS" port-forward "deploy/$RELEASE" "$PF_PORT:8080" >/dev/null 2>&1 &
  local pf_pid=$!
  trap '{ kill '"$pf_pid"' && wait '"$pf_pid"'; } 2>/dev/null || true' RETURN

  local body='' deadline=$((SECONDS + 30))
  while ((SECONDS < deadline)); do
    body=$(curl -sf "http://localhost:$PF_PORT/metrics" 2>/dev/null || true)
    [[ -n "$body" ]] && break
    sleep 2
  done
  [[ -z "$body" ]] && { bad "could not scrape /metrics on port $PF_PORT"; return 0; }

  for metric in secretusage_secret_references secretusage_secret_exists \
    secretusage_secret_usages_truncated secretusage_missing_secrets secretusage_unused_secrets; do
    if grep -q "^$metric" <<<"$body"; then ok "$metric exported"; else bad "$metric missing"; fi
  done

  local unused_gauge
  unused_gauge=$(grep -E "^secretusage_secret_references\{.*secret=\"nobody-uses-me\"" <<<"$body" | awk '{print $NF}')
  [[ "$unused_gauge" == "0" ]] && ok "nobody-uses-me reference gauge is 0" ||
    bad "nobody-uses-me reference gauge is '$unused_gauge', want 0"
}

rules() {
  step "Custom resource rules"
  kubectl apply -f - >/dev/null <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata: {name: widgets.$CRD_GROUP}
spec:
  group: $CRD_GROUP
  scope: Namespaced
  names: {kind: Widget, listKind: WidgetList, plural: widgets, singular: widget}
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              type: object
              properties:
                secretName: {type: string}
EOF
  kubectl wait --for=condition=Established "crd/widgets.$CRD_GROUP" --timeout=60s >/dev/null

  local values
  values=$(mktemp)
  cat >"$values" <<EOF
customRules:
  - apiVersion: $CRD_GROUP/v1
    kind: Widget
    resource: widgets
    paths:
      - .spec.secretName
  # Deliberately absent from this cluster: proves a missing CRD is skipped, not fatal.
  - apiVersion: cert-manager.io/v1
    kind: Certificate
    resource: certificates
    paths:
      - .spec.secretName
EOF
  local args
  mapfile -t args < <(helm_args)
  helm upgrade "$RELEASE" "${args[@]}" -f "$values" >/dev/null
  rm -f "$values"
  controller_ready

  local logs
  logs=$(kubectl -n "$CTRL_NS" logs "deploy/$RELEASE" 2>/dev/null || true)
  grep -q 'tracking custom resource' <<<"$logs" && ok "Widget rule registered" ||
    bad "no 'tracking custom resource' log line"
  grep -q 'skipping rule' <<<"$logs" && ok "absent cert-manager CRD skipped, not fatal" ||
    bad "no 'skipping rule' log line for the absent CRD"

  kubectl create secret generic widget-secret -n "$LAB_NS" --from-literal=k=v >/dev/null
  kubectl apply -n "$LAB_NS" -f - >/dev/null <<EOF
apiVersion: $CRD_GROUP/v1
kind: Widget
metadata: {name: w1}
spec: {secretName: widget-secret}
EOF
  expect "custom resource reference indexed" "Widget w1 .spec.secretName" \
    "su_field widget-secret '{.status.usages[0].kind} {.status.usages[0].name} {.status.usages[0].fieldPath}'"
}

rbac() {
  step "Least privilege"
  local sa="system:serviceaccount:$CTRL_NS:$RELEASE"
  if ! kubectl auth can-i list secrets --as="$sa" -A >/dev/null 2>&1; then
    warn "cannot impersonate service accounts here; skipping RBAC assertions"
    return 0
  fi
  expect "may list Secrets" "yes" "kubectl auth can-i list secrets --as=$sa -A"
  # Existence is answered from the metadata informer, so fetching a Secret body is
  # a permission the controller never needs.
  expect "may NOT get Secrets" "no" "kubectl auth can-i get secrets --as=$sa -A 2>/dev/null || true"
}

cleanup() {
  step "Cleanup"
  helm uninstall "$RELEASE" -n "$CTRL_NS" >/dev/null 2>&1 || true
  kubectl delete ns "$LAB_NS" "$CTRL_NS" --wait=false >/dev/null 2>&1 || true
  kubectl delete crd "widgets.$CRD_GROUP" --wait=false >/dev/null 2>&1 || true
  # The SecretUsage CRD is intentionally left alone: helm does not remove CRDs, and
  # another install may be using it.
  ok "removed $RELEASE, $LAB_NS, $CTRL_NS and the Widget CRD"
}

main() {
  local requested=("$@")
  if [[ ${#requested[@]} -eq 0 ]]; then
    requested=("${PHASES[@]}")
    trap '[[ "$KEEP" == "1" ]] || cleanup' EXIT
  fi

  for phase in "${requested[@]}"; do
    if [[ " ${PHASES[*]} cleanup " != *" $phase "* ]]; then
      die "unknown phase '$phase'. Known: ${PHASES[*]} cleanup"
    fi
    "$phase"
  done

  printf '\n'
  if ((FAILURES > 0)); then
    printf '%s%d assertion(s) failed%s\n' "$RED" "$FAILURES" "$OFF"
    exit 1
  fi
  printf '%sall assertions passed%s\n' "$GREEN" "$OFF"
  [[ "$KEEP" == "1" ]] && printf 'lab left in place (KEEP=1); remove it with: %s cleanup\n' "$0"
  return 0
}

main "$@"
