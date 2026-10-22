#!/usr/bin/env bash
#
# Creates (or deletes) a least-privilege ServiceAccount, RBAC objects and a
# kubeconfig for k8s-rollout-restart.
#
# RBAC layout:
#   <ROLE_PREFIX>-cluster    ClusterRole + ClusterRoleBinding
#                            cluster-scoped needs: nodes (get/list/patch for
#                            cordon and reporting), namespaces (get/list),
#                            persistentvolumes + pods in all namespaces
#                            (list only, local-PV warning for cordoned nodes)
#   <ROLE_PREFIX>-workloads  ClusterRole with namespaced workload verbs, bound
#                            either cluster-wide via a ClusterRoleBinding
#                            (default, TARGET_NAMESPACES empty) or per namespace
#                            via RoleBindings (TARGET_NAMESPACES=ns1,ns2).
#
# All objects carry the label ${MANAGED_LABEL_KEY}=<SERVICE_ACCOUNT_NAME>, which
# "delete" uses to find and remove everything "create" made. "create" also
# reconciles: RoleBindings in namespaces no longer listed in TARGET_NAMESPACES
# are deleted, and the cluster-wide binding of older script versions
# (LEGACY_BINDING_NAME) is removed once verified to be ours.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configurable parameters (environment variables)
# ---------------------------------------------------------------------------
NAMESPACE="${NAMESPACE:-default}"
SERVICE_ACCOUNT_NAME="${SERVICE_ACCOUNT_NAME:-k8s-rollout-restart}"
ROLE_PREFIX="${ROLE_PREFIX:-k8s-rollout-restart}"
TARGET_NAMESPACES="${TARGET_NAMESPACES:-}"
ENABLE_POSTGRESQL="${ENABLE_POSTGRESQL:-true}"
TOKEN_DURATION="${TOKEN_DURATION:-1h}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-./restricted-kubeconfig.yaml}"
KUBECTL_CONTEXT="${KUBECTL_CONTEXT:-}"
K8S_API_SERVER="${K8S_API_SERVER:-}"
# Names used by older versions of this script (one cluster-wide binding).
LEGACY_BINDING_NAME="${LEGACY_BINDING_NAME:-k8s-rollout-restart}"
LEGACY_ROLE_NAME="${LEGACY_ROLE_NAME:-k8s-rollout-restart}"

MANAGED_LABEL_KEY="k8s-rollout-restart.uderik.github.io/access"
CLUSTER_ROLE_NAME="${ROLE_PREFIX}-cluster"
WORKLOAD_ROLE_NAME="${ROLE_PREFIX}-workloads"

TMP_FILE=""
ERR_FILE=""

log() {
  echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*" >&2
}

die() {
  log "Error: $*"
  exit 1
}

cleanup() {
  if [[ -n "${TMP_FILE}" && -f "${TMP_FILE}" ]]; then
    rm -f -- "${TMP_FILE}"
  fi
  if [[ -n "${ERR_FILE}" && -f "${ERR_FILE}" ]]; then
    rm -f -- "${ERR_FILE}"
  fi
}
trap cleanup EXIT

show_help() {
  cat <<EOF
Usage: $0 <command> [target-namespaces]

Commands:
  create [ns1,ns2,...]  Create ServiceAccount, RBAC and a kubeconfig.
                        Optional comma-separated namespaces override TARGET_NAMESPACES.
  delete                Delete everything created by "create" (labelled objects,
                        the ServiceAccount, its namespace if the script created it,
                        and the kubeconfig file).
  help                  Show this help.

Environment variables:
  NAMESPACE             Namespace of the ServiceAccount (default: default)
  SERVICE_ACCOUNT_NAME  ServiceAccount name (default: k8s-rollout-restart)
  ROLE_PREFIX           Prefix for ClusterRole/binding names (default: k8s-rollout-restart)
                        -> \${ROLE_PREFIX}-cluster and \${ROLE_PREFIX}-workloads
  TARGET_NAMESPACES     Comma-separated namespaces to grant workload permissions in
                        (RoleBinding per namespace). Empty (default) = cluster-wide
                        ClusterRoleBinding, which INCLUDES kube-system.
  ENABLE_POSTGRESQL     true|false (default: true). When false, the Zalando
                        postgresqls rule and the pods "patch" verb are omitted.
  TOKEN_DURATION        Requested token lifetime for "kubectl create token --duration"
                        (default: 1h). The API server may shorten it; the actual
                        expiry is printed.
  KUBECONFIG_PATH       Output kubeconfig path (default: ./restricted-kubeconfig.yaml)
  KUBECTL_CONTEXT       kubectl context to use (default: current context)
  K8S_API_SERVER        API server URL for the kubeconfig (default: from the context)
  LEGACY_BINDING_NAME   ClusterRoleBinding created by older script versions, removed
                        when it binds LEGACY_ROLE_NAME to this ServiceAccount only
                        (default: k8s-rollout-restart). Any error other than
                        NotFound while reading it aborts the script.
  LEGACY_ROLE_NAME      ClusterRole created by older script versions; deleted only
                        when no ClusterRoleBinding or RoleBinding references it
                        (default: k8s-rollout-restart)
EOF
}

check_command() {
  command -v "$1" >/dev/null 2>&1 || die "command '$1' not found"
}

# kubectl wrapper honouring KUBECTL_CONTEXT.
k() {
  if [[ -n "${KUBECTL_CONTEXT}" ]]; then
    kubectl --context "${KUBECTL_CONTEXT}" "$@"
  else
    kubectl "$@"
  fi
}

# Prints TARGET_NAMESPACES one per line (whitespace trimmed, empties dropped).
target_namespaces() {
  local ns
  local -a items
  IFS=',' read -r -a items <<<"${TARGET_NAMESPACES}"
  for ns in "${items[@]}"; do
    ns="${ns//[[:space:]]/}"
    [[ -n "${ns}" ]] && printf '%s\n' "${ns}"
  done
  return 0
}

# ---------------------------------------------------------------------------
# Manifests
# ---------------------------------------------------------------------------
cluster_role_manifest() {
  cat <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${CLUSTER_ROLE_NAME}
  labels:
    ${MANAGED_LABEL_KEY}: ${SERVICE_ACCOUNT_NAME}
rules:
# Node cordon (PATCH spec.unschedulable + cordoned-by annotation) and node report.
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "patch"]
# --all-namespaces discovery and namespace report.
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list"]
# Local-PV warning before cordon: pods on the selected nodes are listed in ALL
# namespaces (not only the target ones) and matched against local PVs.
# Read-only; without it the tool warns "could not check".
- apiGroups: [""]
  resources: ["persistentvolumes", "pods"]
  verbs: ["list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${CLUSTER_ROLE_NAME}
  labels:
    ${MANAGED_LABEL_KEY}: ${SERVICE_ACCOUNT_NAME}
subjects:
- kind: ServiceAccount
  name: ${SERVICE_ACCOUNT_NAME}
  namespace: ${NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${CLUSTER_ROLE_NAME}
EOF
}

workload_role_manifest() {
  local pod_verbs='["get", "list"]'
  if [[ "${ENABLE_POSTGRESQL}" == "true" ]]; then
    # Zalando PostgreSQL restart annotates pods.
    pod_verbs='["get", "list", "patch"]'
  fi

  cat <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${WORKLOAD_ROLE_NAME}
  labels:
    ${MANAGED_LABEL_KEY}: ${SERVICE_ACCOUNT_NAME}
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ${pod_verbs}
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets"]
  verbs: ["get", "list", "patch"]
- apiGroups: ["apps"]
  resources: ["replicasets"]
  verbs: ["get", "list"]
- apiGroups: ["kafka.strimzi.io"]
  resources: ["kafkas"]
  verbs: ["get", "list"]
- apiGroups: ["core.strimzi.io"]
  resources: ["strimzipodsets"]
  verbs: ["get", "list", "patch"]
EOF

  if [[ "${ENABLE_POSTGRESQL}" == "true" ]]; then
    cat <<EOF
- apiGroups: ["acid.zalan.do"]
  resources: ["postgresqls"]
  verbs: ["get", "list", "patch"]
EOF
  fi
}

workload_cluster_binding_manifest() {
  cat <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${WORKLOAD_ROLE_NAME}
  labels:
    ${MANAGED_LABEL_KEY}: ${SERVICE_ACCOUNT_NAME}
subjects:
- kind: ServiceAccount
  name: ${SERVICE_ACCOUNT_NAME}
  namespace: ${NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${WORKLOAD_ROLE_NAME}
EOF
}

workload_role_binding_manifest() {
  local target_ns="$1"
  cat <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${WORKLOAD_ROLE_NAME}
  namespace: ${target_ns}
  labels:
    ${MANAGED_LABEL_KEY}: ${SERVICE_ACCOUNT_NAME}
subjects:
- kind: ServiceAccount
  name: ${SERVICE_ACCOUNT_NAME}
  namespace: ${NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${WORKLOAD_ROLE_NAME}
EOF
}

# ---------------------------------------------------------------------------
# reconcile
# ---------------------------------------------------------------------------

# Deletes our RoleBindings (found by label) in namespaces that are not in the
# given list. With an empty list (cluster-wide mode) every one is deleted.
prune_role_bindings() {
  local selector="${MANAGED_LABEL_KEY}=${SERVICE_ACCOUNT_NAME}"
  # Space-delimited list (no associative arrays: macOS ships bash 3.2).
  local keep=" $* "
  local ns name bindings

  bindings="$(k get rolebinding --all-namespaces -l "${selector}" \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}')" \
    || die "failed to list RoleBindings labelled ${selector}"
  while read -r ns name; do
    [[ -n "${ns}" && -n "${name}" ]] || continue
    if [[ "${keep}" != *" ${ns} "* ]]; then
      log "Removing RoleBinding ${ns}/${name}: namespace is no longer a target"
      k delete rolebinding "${name}" -n "${ns}" --ignore-not-found >/dev/null
    fi
  done <<<"${bindings}"
}

# Removes the cluster-wide binding of older script versions, but only after
# verifying that it binds LEGACY_ROLE_NAME to exactly this ServiceAccount.
#
# Only a confirmed NotFound means "no legacy binding". Any other error
# (Forbidden, connection failure, ...) aborts: silently skipping the migration
# would leave the old cluster-wide permissions in place.
migrate_legacy_binding() {
  local actual expected err rc=0
  ERR_FILE="$(mktemp)"
  actual="$(k get clusterrolebinding "${LEGACY_BINDING_NAME}" --ignore-not-found \
    -o jsonpath='{.roleRef.kind}/{.roleRef.name}|{range .subjects[*]}{.kind}/{.namespace}/{.name};{end}' \
    2>"${ERR_FILE}")" || rc=$?
  err="$(cat -- "${ERR_FILE}")"
  rm -f -- "${ERR_FILE}"
  ERR_FILE=""
  if [[ ${rc} -ne 0 ]]; then
    # --ignore-not-found already turns NotFound into success; this is a
    # fallback for kubectl versions that still report it as an error.
    [[ "${err}" == *"(NotFound)"* ]] && return 0
    die "cannot read legacy ClusterRoleBinding ${LEGACY_BINDING_NAME} (kubectl exit ${rc}): ${err:-no error output}
Aborting: the legacy cluster-wide binding may still exist and keep granting broad permissions. Fix access and re-run."
  fi
  # Empty output with exit 0 = confirmed absent (--ignore-not-found).
  [[ -n "${actual}" ]] || return 0
  expected="ClusterRole/${LEGACY_ROLE_NAME}|ServiceAccount/${NAMESPACE}/${SERVICE_ACCOUNT_NAME};"
  if [[ "${actual}" != "${expected}" ]]; then
    log "WARNING: ClusterRoleBinding ${LEGACY_BINDING_NAME} exists but does not look like the one an older"
    log "WARNING: version of this script created (got '${actual}', expected '${expected}'); leaving it alone."
    return 0
  fi
  log "Removing legacy cluster-wide ClusterRoleBinding ${LEGACY_BINDING_NAME}"
  k delete clusterrolebinding "${LEGACY_BINDING_NAME}" --ignore-not-found >/dev/null
  delete_legacy_role_if_unused
}

# Deletes LEGACY_ROLE_NAME only when no ClusterRoleBinding and no RoleBinding
# (in any namespace) references it, ignoring the legacy binding just deleted.
# If either list cannot be read the role is kept: deleting it could strip
# permissions from subjects we cannot see.
delete_legacy_role_if_unused() {
  local crbs rbs refs line
  if ! crbs="$(k get clusterrolebinding \
    -o jsonpath='{range .items[*]}{"ClusterRoleBinding "}{.metadata.name}{" "}{.roleRef.kind}/{.roleRef.name}{"\n"}{end}')"; then
    log "WARNING: could not list ClusterRoleBindings; keeping ClusterRole ${LEGACY_ROLE_NAME}."
    log "WARNING: delete it manually once nothing references it."
    return 0
  fi
  if ! rbs="$(k get rolebinding --all-namespaces \
    -o jsonpath='{range .items[*]}{"RoleBinding "}{.metadata.namespace}/{.metadata.name}{" "}{.roleRef.kind}/{.roleRef.name}{"\n"}{end}')"; then
    log "WARNING: could not list RoleBindings in all namespaces; keeping ClusterRole ${LEGACY_ROLE_NAME}."
    log "WARNING: delete it manually once nothing references it."
    return 0
  fi
  refs="$(printf '%s\n%s\n' "${crbs}" "${rbs}" \
    | awk -v ref="ClusterRole/${LEGACY_ROLE_NAME}" -v self="${LEGACY_BINDING_NAME}" \
      '$3 == ref && !($1 == "ClusterRoleBinding" && $2 == self) { print $1 " " $2 }')"
  if [[ -n "${refs}" ]]; then
    log "WARNING: keeping ClusterRole ${LEGACY_ROLE_NAME}: still referenced by:"
    while IFS= read -r line; do
      log "WARNING:   ${line}"
    done <<<"${refs}"
    return 0
  fi
  log "Removing legacy ClusterRole ${LEGACY_ROLE_NAME}"
  k delete clusterrole "${LEGACY_ROLE_NAME}" --ignore-not-found >/dev/null
}

# ---------------------------------------------------------------------------
# create
# ---------------------------------------------------------------------------
create_resources() {
  local -a namespaces=()
  local ns
  while IFS= read -r ns; do
    namespaces+=("${ns}")
  done < <(target_namespaces)

  if ! k get namespace "${NAMESPACE}" >/dev/null 2>&1; then
    log "Namespace ${NAMESPACE} doesn't exist. Creating..."
    k create namespace "${NAMESPACE}"
    k label namespace "${NAMESPACE}" "${MANAGED_LABEL_KEY}=${SERVICE_ACCOUNT_NAME}" >/dev/null
  fi

  log "Applying ServiceAccount ${NAMESPACE}/${SERVICE_ACCOUNT_NAME}"
  k apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SERVICE_ACCOUNT_NAME}
  namespace: ${NAMESPACE}
  labels:
    ${MANAGED_LABEL_KEY}: ${SERVICE_ACCOUNT_NAME}
EOF

  log "Applying cluster-scoped ClusterRole/ClusterRoleBinding ${CLUSTER_ROLE_NAME}"
  cluster_role_manifest | k apply -f -

  log "Applying workload ClusterRole ${WORKLOAD_ROLE_NAME}"
  workload_role_manifest | k apply -f -

  if [[ ${#namespaces[@]} -eq 0 ]]; then
    log "WARNING: TARGET_NAMESPACES is empty - binding workload permissions CLUSTER-WIDE."
    log "WARNING: this includes kube-system and every other namespace. Set"
    log "WARNING: TARGET_NAMESPACES=ns1,ns2 to restrict it to specific namespaces."
    workload_cluster_binding_manifest | k apply -f -
    prune_role_bindings
  else
    for ns in "${namespaces[@]}"; do
      if ! k get namespace "${ns}" >/dev/null 2>&1; then
        die "target namespace '${ns}' does not exist"
      fi
    done
    for ns in "${namespaces[@]}"; do
      log "Binding workload permissions in namespace ${ns}"
      workload_role_binding_manifest "${ns}" | k apply -f -
    done
    # Switching from cluster-wide to per-namespace mode: drop the old binding,
    # and the bindings of namespaces that are no longer targets.
    k delete clusterrolebinding "${WORKLOAD_ROLE_NAME}" --ignore-not-found >/dev/null
    prune_role_bindings "${namespaces[@]}"
    log "Note: use the tool with -n $(IFS=,; echo "${namespaces[*]}"); -A would fail with 403 in other namespaces."
  fi

  migrate_legacy_binding

  log "RBAC resources successfully created"
}

get_ca_data() {
  local ca_data=""
  local cluster ca_file

  # Preferred: the cluster CA published in every namespace (Kubernetes 1.20+).
  ca_data="$(k get configmap kube-root-ca.crt -n "${NAMESPACE}" \
    -o jsonpath='{.data.ca\.crt}' 2>/dev/null | base64 | tr -d '\n')" || ca_data=""

  # Fallback: the CA from the current kubeconfig.
  if [[ -z "${ca_data}" ]]; then
    cluster="$(k config view --minify -o jsonpath='{.clusters[0].name}')"
    ca_data="$(k config view --raw --minify \
      -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')"
    if [[ -z "${ca_data}" ]]; then
      ca_file="$(k config view --raw --minify \
        -o jsonpath='{.clusters[0].cluster.certificate-authority}')"
      if [[ -n "${ca_file}" && -f "${ca_file}" ]]; then
        ca_data="$(base64 <"${ca_file}" | tr -d '\n')"
      fi
    fi
    [[ -n "${ca_data}" ]] && log "Using CA of cluster '${cluster}' from the current kubeconfig"
  fi

  [[ -n "${ca_data}" ]] || die "could not determine the cluster CA certificate (ConfigMap kube-root-ca.crt and current kubeconfig both empty)"
  printf '%s' "${ca_data}"
}

create_kubeconfig() {
  local ca_data token_out sa_token token_expiry out_dir

  log "Creating kubeconfig with restricted permissions..."

  if [[ -z "${K8S_API_SERVER}" ]]; then
    K8S_API_SERVER="$(k config view --minify -o jsonpath='{.clusters[0].cluster.server}')"
  fi
  [[ -n "${K8S_API_SERVER}" ]] || die "could not determine the API server URL; set K8S_API_SERVER"

  ca_data="$(get_ca_data)"

  token_out="$(k create token "${SERVICE_ACCOUNT_NAME}" -n "${NAMESPACE}" \
    --duration="${TOKEN_DURATION}" \
    -o jsonpath='{.status.token}{" "}{.status.expirationTimestamp}')"
  read -r sa_token token_expiry <<<"${token_out}"
  [[ -n "${sa_token}" ]] || die "failed to obtain a ServiceAccount token"

  if [[ -e "${KUBECONFIG_PATH}" || -L "${KUBECONFIG_PATH}" ]]; then
    [[ -f "${KUBECONFIG_PATH}" && ! -L "${KUBECONFIG_PATH}" ]] \
      || die "${KUBECONFIG_PATH} exists and is not a regular file; refusing to overwrite"
    log "Overwriting existing ${KUBECONFIG_PATH}"
  fi

  # Write to a private temp file in the destination directory, then rename
  # atomically, so the token is never readable by others and an existing
  # symlink at the destination is replaced rather than followed.
  out_dir="$(dirname -- "${KUBECONFIG_PATH}")"
  TMP_FILE="$(umask 077 && mktemp "${out_dir}/.restricted-kubeconfig.XXXXXX")"
  chmod 0600 "${TMP_FILE}"
  cat >"${TMP_FILE}" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: restricted-cluster
  cluster:
    server: ${K8S_API_SERVER}
    certificate-authority-data: ${ca_data}
contexts:
- name: restricted-context
  context:
    cluster: restricted-cluster
    user: ${SERVICE_ACCOUNT_NAME}
current-context: restricted-context
users:
- name: ${SERVICE_ACCOUNT_NAME}
  user:
    token: ${sa_token}
EOF
  mv -f -- "${TMP_FILE}" "${KUBECONFIG_PATH}"
  TMP_FILE=""

  log "Kubeconfig created at ${KUBECONFIG_PATH} (mode 0600)"
  log "Token requested duration: ${TOKEN_DURATION}; actual expiry: ${token_expiry:-unknown}"
  log "The kubeconfig context is named 'restricted-context' (use --context=restricted-context)."
}

# ---------------------------------------------------------------------------
# delete
# ---------------------------------------------------------------------------
delete_resources() {
  local selector="${MANAGED_LABEL_KEY}=${SERVICE_ACCOUNT_NAME}"

  log "Deleting RoleBindings labelled ${selector} in all namespaces"
  k delete rolebinding --all-namespaces -l "${selector}" --ignore-not-found

  log "Deleting ClusterRoleBindings and ClusterRoles labelled ${selector}"
  k delete clusterrolebinding -l "${selector}" --ignore-not-found
  k delete clusterrole -l "${selector}" --ignore-not-found
  migrate_legacy_binding

  log "Deleting ServiceAccount ${NAMESPACE}/${SERVICE_ACCOUNT_NAME}"
  k delete serviceaccount "${SERVICE_ACCOUNT_NAME}" -n "${NAMESPACE}" --ignore-not-found

  # Only delete the namespace if this script created it (it carries our label).
  if k get namespace -l "${selector}" -o name 2>/dev/null | grep -qx "namespace/${NAMESPACE}"; then
    log "Deleting namespace ${NAMESPACE} (created by this script)"
    k delete namespace "${NAMESPACE}" --ignore-not-found
  fi

  if [[ -f "${KUBECONFIG_PATH}" && ! -L "${KUBECONFIG_PATH}" ]]; then
    log "Deleting kubeconfig file: ${KUBECONFIG_PATH}"
    rm -f -- "${KUBECONFIG_PATH}"
  fi

  log "Resources successfully deleted"
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
main() {
  local cmd="${1:-help}"

  case "${cmd}" in
    create)
      if [[ $# -ge 2 ]]; then
        TARGET_NAMESPACES="$2"
      fi
      case "${ENABLE_POSTGRESQL}" in
        true|false) ;;
        *) die "ENABLE_POSTGRESQL must be 'true' or 'false'" ;;
      esac
      check_command kubectl
      check_command base64
      create_resources
      create_kubeconfig
      log "To use the restricted kubeconfig, run:"
      log "  export KUBECONFIG=${KUBECONFIG_PATH}"
      ;;
    delete)
      check_command kubectl
      delete_resources
      ;;
    help|-h|--help)
      show_help
      ;;
    *)
      show_help
      exit 1
      ;;
  esac
}

main "$@"
