#!/usr/bin/env bash
set -euo pipefail
set -x

ISTIO_V1="1.22.4"
ISTIO_V2="1.23.1"

ISTIO_HELM_REPO="https://istio-release.storage.googleapis.com/charts"


function helmXinstall() {
  local ns="$1" rel="$2"
  shift 2
  local cmd="install"
  if helm status --namespace "$ns" "$rel" &>/dev/null; then
    cmd="upgrade"
  fi
  helm "$cmd" --namespace "$ns" "$rel" "$@"
}

function createXnamespace() {
  local ns="$1"
  if kubectl get ns "$ns" &>/dev/null; then
    return
  fi
  kubectl create ns "$ns"
}

function init-cluster() {
  if minikube status &>/dev/null; then
    return
  fi
  ### set up a new cluster
  minikube start --addons=metrics-server,storage-provisioner --cpus=max --memory=no-limit

  sleep 5

  # add a ns without istio to run debug pods
  createXnamespace debug
}

### install first istio revision
function install-istio() {
  helm repo add istio "$ISTIO_HELM_REPO"
  helm repo update

  createXnamespace istio-system

  helmXinstall istio-system istio-base istio/base \
    --wait \
    --version "$ISTIO_V1" \
    --values istio-base.values.yaml \
    --set revision="v${ISTIO_V1//./-}" \
    --set revisionTags="{default,stable}" \
    --set defaultRevision="default"

  helmXinstall istio-system istio-cni istio/cni \
    --wait \
    --version "$ISTIO_V1" \
    --values istio-cni.values.yaml \
    --set revision="v${ISTIO_V1//./-}"

  helmXinstall istio-system "istio-istiod-v${ISTIO_V1//./-}" istio/istiod  \
    --wait \
    --version "$ISTIO_V1" \
    --values istio-istiod.values.yaml \
    --set revision="v${ISTIO_V1//./-}"

  # install k8s Gateway API
  kubectl get crd gateways.gateway.networking.k8s.io &> /dev/null || \
    { kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.1.0/standard-install.yaml; }

  # set the "default" tag for this revision. This tag is special, see:
  #  https://istio.io/latest/docs/setup/upgrade/canary/#default-tag
  istioctl tag generate default --overwrite --revision "v${ISTIO_V1//./-}" | kubectl apply -f - 

  # set the stable tag for this revision (tag name is arbitrarty)
  istioctl tag generate stable --overwrite --revision "v${ISTIO_V1//./-}" | kubectl apply -f - 

  # set the canary tag for this revision (tag name is arbitrarty)
  istioctl tag generate canary --overwrite --revision "v${ISTIO_V1//./-}" | kubectl apply -f - 
}

function install-fortsa() {
  createXnamespace fortsa
  helmXinstall fortsa fortsa fortsa/fortsa \
    --wait \
    --version "v0.0.19"
}

function install-bookinfo() {
  local ns="${1:-bookinfo-stable}" rev="${2:-stable}"
  # enable auto-injection on the bookinfo namespace
  createXnamespace "$ns"
  kubectl label namespace "$ns" istio.io/rev="$rev"
  kubectl apply -n "$ns" -f https://raw.githubusercontent.com/istio/istio/release-1.23/samples/bookinfo/platform/kube/bookinfo.yaml
}

### install new istiod
function install-canary-istiod() {
  helmXinstall istio-system "istio-istiod-v${ISTIO_V2//./-}" istio/istiod \
    --wait \
    --version "$ISTIO_V2" \
    --values istio-istiod.values.yaml \
    --set revision="v${ISTIO_V2//./-}"

  # set the canary tag for this revision
  istioctl tag generate canary --overwrite --revision "v${ISTIO_V2//./-}" | kubectl apply -f - 
}

### if there were any canary-tagged namespaces, IPUC would restart the pods


### promote the new istiod to default and stable
function complete-istio-upgrade() {

  ### make the new istio revision the system default

  # set the default tag for this revision (should this be before or after the other upgrades?)
  istioctl tag generate default --overwrite --revision "v${ISTIO_V2//./-}" | kubectl apply -f -

  helm upgrade --wait --namespace istio-system istio-base istio/base \
    --version "$ISTIO_V2" \
    --values istio-base.values.yaml \
    --set revision="v${ISTIO_V2//./-}" \
    --set defaultRevision="v${ISTIO_V2//./-}" \
    --skip-crds

  # upgrade the CNI
  helm upgrade --wait --namespace istio-system istio-cni istio/cni \
    --version "$ISTIO_V2" \
    --values istio-cni.values.yaml \
    --set revision="v${ISTIO_V2//./-}"

  ### by now the controller should be running, and the next command should cause it
  ### to trigger the rollout restart of pods connected to the old istio version,
  ### because we labeled the "default" namespace with the stable tag

  # set the stable tag for this revision
  istioctl tag generate stable --overwrite --revision "v${ISTIO_V2//./-}" | kubectl apply -f -
}

function rollback-istio-upgrade() {
  istioctl tag generate canary --overwrite --revision "v${ISTIO_V1//./-}" | kubectl apply -f -
  istioctl tag generate stable --overwrite --revision "v${ISTIO_V1//./-}" | kubectl apply -f -
  istioctl tag generate default --overwrite --revision "v${ISTIO_V1//./-}" | kubectl apply -f -
}

function setup-demo() {
  init-cluster
  install-istio
  install-fortsa
  install-bookinfo bookinfo-stable stable
  install-bookinfo bookinfo-canary canary
}

function reset-demo() {
  minikube delete
  setup-demo
}


function main() {
  case "${1:-}" in
    reset    ) reset-demo ;;
    init     ) setup-demo ;;
    upgrade  )  install-canary-istiod ;;
    complete ) complete-istio-upgrade ;;
    rollback ) rollback-istio-upgrade ;;
    *        ) echo >&2 "Specify action: reset | init | upgrade | complete | rollback"
  esac
}

main "$@"

echo DONE
