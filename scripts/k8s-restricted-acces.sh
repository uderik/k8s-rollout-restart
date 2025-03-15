#!/bin/bash

set -e

# Configurable parameters
NAMESPACE=${NAMESPACE:-"default"}
SERVICE_ACCOUNT_NAME=${SERVICE_ACCOUNT_NAME:-"k8s-rollout-restart"}
ROLE_NAME=${ROLE_NAME:-"k8s-rollout-restart"}
ROLE_BINDING_NAME=${ROLE_BINDING_NAME:-"k8s-rollout-restart"}
KUBECONFIG_PATH=${KUBECONFIG_PATH:-"./restricted-kubeconfig.yaml"}

# Get API server from current context if not manually specified
if [ -z "$K8S_API_SERVER" ]; then
  K8S_API_SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
fi

# Function for message output
log() {
  echo "[$(date +%Y-%m-%d\ %H:%M:%S)] $1"
}

# Function to check command existence
check_command() {
  if ! command -v $1 &> /dev/null; then
    log "Error: command $1 not found"
    exit 1
  fi
}

# Check for kubectl
check_command kubectl

# Function to create temporary YAML files
create_yaml_files() {
  log "Creating temporary YAML files..."
  
  # Service Account
  cat > /tmp/sa.yaml << EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${SERVICE_ACCOUNT_NAME}
  namespace: ${NAMESPACE}
EOF

  # ClusterRole with restricted permissions for rollout restart
  cat > /tmp/role.yaml << EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${ROLE_NAME}
rules:
- apiGroups: [""]
  resources: ["nodes", "pods", "namespaces"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: ["apps"]
  resources: ["deployments", "statefulsets"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: ["kafka.strimzi.io"]
  resources: ["kafkas"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["core.strimzi.io"]
  resources: ["strimzipodsets"]
  verbs: ["get", "list", "watch", "patch"]
- apiGroups: ["acid.zalan.do"]
  resources: ["postgresqls"]
  verbs: ["get", "list", "watch", "patch"]
EOF

  # ClusterRoleBinding
  cat > /tmp/rolebinding.yaml << EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${ROLE_BINDING_NAME}
subjects:
- kind: ServiceAccount
  name: ${SERVICE_ACCOUNT_NAME}
  namespace: ${NAMESPACE}
roleRef:
  kind: ClusterRole
  name: ${ROLE_NAME}
  apiGroup: rbac.authorization.k8s.io
EOF
}

# Function to create resources
create_resources() {
  log "Creating cluster-wide resources..."
  
  # Check if namespace exists (only for ServiceAccount)
  if ! kubectl get namespace ${NAMESPACE} &> /dev/null; then
    log "Namespace ${NAMESPACE} doesn't exist. Creating..."
    kubectl create namespace ${NAMESPACE}
  fi
  
  # Apply YAML files
  kubectl apply -f /tmp/sa.yaml
  kubectl apply -f /tmp/role.yaml
  kubectl apply -f /tmp/rolebinding.yaml
  
  log "Resources successfully created"
}

# Function to create kubeconfig
create_kubeconfig() {
  log "Creating kubeconfig with restricted permissions..."
  
  # Get token for Service Account (valid cluster-wide but must reference a namespace)
  SA_TOKEN=$(kubectl create token ${SERVICE_ACCOUNT_NAME} -n ${NAMESPACE})
  
  # Get current context
  CURRENT_CONTEXT=$(kubectl config current-context)
  CURRENT_CLUSTER=$(kubectl config view --minify -o jsonpath='{.contexts[0].context.cluster}')
  
  # Get CA certificate
  CA_DATA=$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name==\"${CURRENT_CLUSTER}\")].cluster.certificate-authority-data}")
  
  # If CA_DATA is empty, try other methods
  if [ -z "$CA_DATA" ]; then
    # Check for certificate-authority file
    CA_FILE=$(kubectl config view --raw -o jsonpath="{.clusters[?(@.name==\"${CURRENT_CLUSTER}\")].cluster.certificate-authority}")
    if [ -n "$CA_FILE" ] && [ -f "$CA_FILE" ]; then
      CA_DATA=$(cat "$CA_FILE" | base64 | tr -d '\n')
    else
      # As a last resort, get from secret
      CA_DATA=$(kubectl get secret -n ${NAMESPACE} -o jsonpath="{.items[?(@.type==\"kubernetes.io/service-account-token\")].data.ca\.crt}" | head -n 1)
    fi
  fi
  
  # Create kubeconfig with cluster-wide access (no default namespace)
  cat > ${KUBECONFIG_PATH} << EOF
apiVersion: v1
kind: Config
clusters:
- name: restricted-cluster
  cluster:
    server: ${K8S_API_SERVER}
    certificate-authority-data: ${CA_DATA}
contexts:
- name: restricted-context
  context:
    cluster: restricted-cluster
    user: ${SERVICE_ACCOUNT_NAME}
current-context: restricted-context
users:
- name: ${SERVICE_ACCOUNT_NAME}
  user:
    token: ${SA_TOKEN}
EOF

  log "Kubeconfig created at path: ${KUBECONFIG_PATH}"
}

# Function to delete resources
delete_resources() {
  log "Deleting resources..."
  
  # Check if resources exist before deleting
  if kubectl get clusterrolebinding ${ROLE_BINDING_NAME} &> /dev/null; then
    kubectl delete -f /tmp/rolebinding.yaml
  fi
  
  if kubectl get clusterrole ${ROLE_NAME} &> /dev/null; then
    kubectl delete -f /tmp/role.yaml
  fi
  
  if kubectl get serviceaccount ${SERVICE_ACCOUNT_NAME} -n ${NAMESPACE} &> /dev/null; then
    kubectl delete -f /tmp/sa.yaml
  fi
  
  # Delete kubeconfig if it was created
  if [ -f "${KUBECONFIG_PATH}" ]; then
    log "Deleting kubeconfig file: ${KUBECONFIG_PATH}"
    rm ${KUBECONFIG_PATH}
  fi
  
  # Delete temporary files
  rm -f /tmp/sa.yaml /tmp/role.yaml /tmp/rolebinding.yaml
  
  log "Resources successfully deleted"
}

# Function to show help
show_help() {
  echo "Usage: $0 [options]"
  echo "Options:"
  echo "  create     Create resources with restricted permissions"
  echo "  delete     Delete created resources"
  echo "  help       Show help"
  echo ""
  echo "Environment variables:"
  echo "  NAMESPACE              Namespace for ServiceAccount (default: default)"
  echo "  SERVICE_ACCOUNT_NAME   Service Account name (default: k8s-rollout-restart)"
  echo "  ROLE_NAME              ClusterRole name (default: k8s-rollout-restart)"
  echo "  ROLE_BINDING_NAME      ClusterRoleBinding name (default: k8s-rollout-restart)"
  echo "  KUBECONFIG_PATH        Path to save cluster-wide kubeconfig (default: ./restricted-kubeconfig.yaml)"
  echo "  K8S_API_SERVER         Kubernetes API server address (default: from current kubectl context)"
}

# Main script logic
case "$1" in
  create)
    create_yaml_files
    create_resources
    create_kubeconfig
    log "To use the created cluster-wide kubeconfig, run:"
    log "export KUBECONFIG=${KUBECONFIG_PATH}"
    ;;
  delete)
    create_yaml_files  # Create YAML files for subsequent deletion
    delete_resources
    ;;
  *)
    show_help
    ;;
esac

exit 0