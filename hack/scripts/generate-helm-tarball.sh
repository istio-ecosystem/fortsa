#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CHART_SRC="${REPO_ROOT}/helm/src/chart"
OUTPUT_DIR="${OUTPUT_DIR:-${REPO_ROOT}/build}"

# Prerequisites
command -v helm >/dev/null 2>&1 || { echo "helm is required but not installed. Aborting." >&2; exit 1; }
command -v envsubst >/dev/null 2>&1 || { echo "envsubst is required but not installed. Aborting." >&2; exit 1; }

[[ -d "${CHART_SRC}" ]] || { echo "Chart source not found: ${CHART_SRC}" >&2; exit 1; }

# Variable resolution (allow override via env)
__VERSION__="${__VERSION__:-$(git -C "${REPO_ROOT}" describe --tags 2>/dev/null | sed 's/^v//' || echo "0.0.0")}"
__IMAGE_TAG_BASE__="${__IMAGE_TAG_BASE__:-fortsa.scaffidi.net/fortsa}"
export __VERSION__ __IMAGE_TAG_BASE__

TMP_CHART=$(mktemp -d)
trap 'rm -rf "${TMP_CHART}"' EXIT

# Copy chart and process Chart.yaml and values.yaml with envsubst
cp -r "${CHART_SRC}"/. "${TMP_CHART}/"

# shellcheck disable=SC2016
envsubst '${__VERSION__} ${__IMAGE_TAG_BASE__}' < "${CHART_SRC}/Chart.yaml" > "${TMP_CHART}/Chart.yaml"
# shellcheck disable=SC2016
envsubst '${__VERSION__} ${__IMAGE_TAG_BASE__}' < "${CHART_SRC}/values.yaml" > "${TMP_CHART}/values.yaml"

mkdir -p "${OUTPUT_DIR}"
helm package "${TMP_CHART}" -d "${OUTPUT_DIR}"

TARBALL="${OUTPUT_DIR}/fortsa-${__VERSION__}.tgz"
echo "Built: ${TARBALL}"
