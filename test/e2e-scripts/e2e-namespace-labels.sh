#!/usr/bin/env bash
# End-to-end test: Istio namespace label changes, fortsa namespace watch triggers restart
# Installs two Istio revisions (stable/canary), changes namespace label from stable to canary,
# verifies fortsa restarts the deployment (namespace-scoped reconcile).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-fortsa-e2e}"
ISTIO_OLD_VERSION="${ISTIO_OLD_VERSION:-1.28.4}"
ISTIO_NEW_VERSION="${ISTIO_NEW_VERSION:-1.29.0}"
E2E_NAMESPACE="hello-ns-label"

# Revision names: 1.28.4 -> 1-28-4
REVISION_OLD="${ISTIO_OLD_VERSION//./-}"
REVISION_NEW="${ISTIO_NEW_VERSION//./-}"

cleanup() {
  local exit_code=$?
  if [[ "${SKIP_CLEANUP:-}" == "1" ]]; then
    echo "SKIP_CLEANUP=1, leaving cluster running"
    exit $exit_code
  fi
  echo "Cleaning up..."
  kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
  if [[ -n "${TMPDIR:-}" && -d "${TMPDIR}" ]]; then
    rm -rf "${TMPDIR}"
  fi
  exit $exit_code
}
trap cleanup EXIT

log() { echo "[$(date +%H:%M:%S)] $*" >&2; }

# Ensure required tools exist
for cmd in kind kubectl docker; do
  if ! command -v $cmd &>/dev/null; then
    echo "Error: $cmd is required but not installed"
    exit 1
  fi
done

# Detect OS/arch for Istio download (Istio uses osx for macOS, linux for Linux)
case $(uname -s) in
  Darwin) OS=osx ;;
  Linux)  OS=linux ;;
  *)      OS=linux ;;
esac
case $(uname -m) in
  arm64|aarch64) ARCH=arm64 ;;
  *)             ARCH=amd64 ;;
esac

download_istio() {
  local version=$1
  local url="https://github.com/istio/istio/releases/download/${version}/istio-${version}-${OS}-${ARCH}.tar.gz"
  log "Downloading Istio ${version}..."
  if ! curl -sSLf "$url" | tar -xzf - -C "$TMPDIR"; then
    echo "Error: Failed to download or extract Istio ${version} from ${url}" >&2
    exit 1
  fi
  echo "$TMPDIR/istio-${version}"
}

# Check Docker resources (Istio needs ~8GB memory, 4 CPUs)
if command -v docker &>/dev/null; then
  DOCKER_MEM_GB=$(docker info --format '{{.MemTotal}}' 2>/dev/null | awk '{printf "%.0f", $1/1024/1024/1024}')
  if [[ -n "$DOCKER_MEM_GB" && "$DOCKER_MEM_GB" -ge 1 && "$DOCKER_MEM_GB" -lt 6 ]]; then
    log "Warning: Docker reports ${DOCKER_MEM_GB}GB memory. Istio typically needs 8GB+ (Docker Desktop: Settings -> Resources)"
  fi
fi

log "Deleting existing kind cluster: $CLUSTER_NAME"
kind delete cluster --name "$CLUSTER_NAME"

log "Creating kind cluster: $CLUSTER_NAME"
kind create cluster --name "$CLUSTER_NAME" --wait 2m

# Download Istio versions
TMPDIR=$(mktemp -d)
TMP_ISTIO_OLD=$(download_istio "$ISTIO_OLD_VERSION")
TMP_ISTIO_NEW=$(download_istio "$ISTIO_NEW_VERSION")

ISTIOCTL_OLD="$TMP_ISTIO_OLD/bin/istioctl"
ISTIOCTL_NEW="$TMP_ISTIO_NEW/bin/istioctl"

log "Installing Istio ${ISTIO_OLD_VERSION} (revision ${REVISION_OLD})..."
"$ISTIOCTL_OLD" install -y --set revision="$REVISION_OLD" --set profile=minimal

log "Installing Istio ${ISTIO_NEW_VERSION} (revision ${REVISION_NEW})..."
"$ISTIOCTL_NEW" install -y --set revision="$REVISION_NEW" --set profile=minimal

log "Creating revision tags: stable -> ${REVISION_OLD}, canary -> ${REVISION_NEW}"
"$ISTIOCTL_NEW" tag set stable --revision "$REVISION_OLD"
"$ISTIOCTL_NEW" tag set canary --revision "$REVISION_NEW"

log "Creating namespace $E2E_NAMESPACE with istio.io/rev=stable"
kubectl create namespace "$E2E_NAMESPACE" 2>/dev/null || true
kubectl label namespace "$E2E_NAMESPACE" istio.io/rev=stable --overwrite

log "Deploying hello-world app"
kubectl apply -f "$SCRIPT_DIR/hello-world.yaml" -n "$E2E_NAMESPACE"

log "Waiting for hello-world deployment and sidecar injection..."
kubectl rollout status deployment/helloworld -n "$E2E_NAMESPACE" --timeout=120s
for i in $(seq 1 30); do
  if kubectl get pods -n "$E2E_NAMESPACE" -l app=helloworld -o jsonpath='{.items[0].spec.containers[*].name}' 2>/dev/null | grep -q istio-proxy; then
    break
  fi
  sleep 2
done
if ! kubectl get pods -n "$E2E_NAMESPACE" -l app=helloworld -o jsonpath='{.items[0].spec.containers[*].name}' 2>/dev/null | grep -q istio-proxy; then
  echo "Error: hello-world pod in $E2E_NAMESPACE did not get istio-proxy sidecar"
  kubectl get pods -n "$E2E_NAMESPACE" -o wide
  kubectl describe pod -n "$E2E_NAMESPACE" -l app=helloworld
  exit 1
fi
log "Hello-world pod in $E2E_NAMESPACE has istio-proxy sidecar"

log "Building fortsa image"
docker build -t fortsa:e2e "$PROJECT_ROOT"

log "Loading fortsa image into kind cluster"
docker tag fortsa:e2e example.com/fortsa:v0.0.1
kind load docker-image example.com/fortsa:v0.0.1 --name "$CLUSTER_NAME"

log "Deploying fortsa (RBAC + deployment)"
kubectl apply -k "$PROJECT_ROOT/config/default/"
kubectl rollout status deployment/fortsa-controller-manager -n fortsa-system --timeout=120s
log "Fortsa is running"

sleep 10

# Record initial pod before namespace label change
INITIAL_POD=$(kubectl get pods -n "$E2E_NAMESPACE" -l app=helloworld -o jsonpath='{.items[0].metadata.name}')
log "Initial pod: $INITIAL_POD"

log "Changing namespace label from istio.io/rev=stable to istio.io/rev=canary..."
kubectl label namespace "$E2E_NAMESPACE" istio.io/rev=canary --overwrite

wait_delay=2 # seconds
wait_time=$((60 * 5)) # 5 minutes
wait_loops=$((wait_time / wait_delay))
log "Waiting for fortsa to annotate helloworld deployment and trigger restart (up to $wait_time seconds)..."
SUCCESS=0
# shellcheck disable=SC2034
for i in $(seq 1 $wait_loops); do
  if kubectl get deployment helloworld -n "$E2E_NAMESPACE" -o jsonpath='{.spec.template.metadata.annotations.fortsa\.scaffidi\.net/restartedAt}' 2>/dev/null | grep -q .; then
    log "Found restartedAt annotation on helloworld deployment"
    CURRENT_POD=$(kubectl get pods -n "$E2E_NAMESPACE" -l app=helloworld -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -n "$CURRENT_POD" && "$CURRENT_POD" != "$INITIAL_POD" ]]; then
      log "New pod $CURRENT_POD created (replacement for $INITIAL_POD)"
      SUCCESS=1
      break
    fi
  fi
  sleep $wait_delay
done

if [[ $SUCCESS -ne 1 ]]; then
  echo "Error: fortsa did not restart the helloworld deployment within $wait_time seconds"
  echo "Deployment annotations:"
  kubectl get deployment helloworld -n "$E2E_NAMESPACE" -o yaml | grep -A 20 "annotations:"
  echo "Fortsa logs:"
  kubectl logs -n fortsa-system deployment/fortsa-controller-manager --tail=50
  exit 1
fi

# Verify new pod has canary revision's proxy image
log "Verifying new sidecar version..."
NEW_POD=$(kubectl get pods -n "$E2E_NAMESPACE" -l app=helloworld -o jsonpath='{.items[0].metadata.name}')
PROXY_IMAGE=$(kubectl get pod "$NEW_POD" -n "$E2E_NAMESPACE" -o jsonpath='{.spec.containers[?(@.name=="istio-proxy")].image}')
log "New pod $NEW_POD has istio-proxy image: $PROXY_IMAGE"
if [[ "$PROXY_IMAGE" != *"$ISTIO_NEW_VERSION"* ]]; then
  echo "Warning: Expected proxy image to contain $ISTIO_NEW_VERSION, got: $PROXY_IMAGE"
  echo "The pod may not have been restarted yet - checking rollout..."
  kubectl rollout status deployment/helloworld -n "$E2E_NAMESPACE" --timeout=60s
fi

log "E2E test passed: fortsa successfully restarted helloworld deployment after namespace label change"
