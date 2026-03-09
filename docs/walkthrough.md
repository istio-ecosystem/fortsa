# Code Walkthrough

## Fortsa: linear code walkthrough (prose-only)

This is a linear walkthrough of how Fortsa works internally. It is intentionally **prose-only** (no fenced code blocks) so it reads like a narrative companion to the source.

### 1) Entry point and controller wiring (`cmd/main.go`)

Fortsa runs as a controller-runtime manager. The entry point is responsible for:

- Parsing flags that control safety and behavior (dry-run, compare-hub, delays, cooldowns, skip namespaces).
- Configuring logging and TLS options (notably: HTTP/2 is disabled by default).
- Creating the controller-runtime Manager (metrics, probes, leader election).
- Constructing the main reconciler and registering the controller via `controller.SetupIstioChangeController`.

The controller package defines the watches and wires them to the reconciler. Watches are registered for:

- ConfigMaps: `istio-sidecar-injector*` in `istio-system`.
- Istio's tag-mapping `MutatingWebhookConfiguration` objects (`istio-revision-tag-*`).
- Namespaces whose Istio labels change (`istio.io/rev` or `istio-injection`).
- An optional periodic reconcile source when `--reconcile-period` is set.

There is no primary `For` resource; all four are independent watches.

### 2) Watches and reconcile requests (`internal/configmap`, `internal/mwc`, `internal/namespace`, `internal/periodic`)

The controller package's `SetupIstioChangeController` registers watches using predicates and reconcile request builders from these packages. Fortsa watches four kinds of inputs:

- ConfigMaps: `istio-sidecar-injector*` in `istio-system`.
- MutatingWebhookConfigurations: `istio-revision-tag-*` (tag-to-revision mapping changes).
- Namespaces: changes to Istio-related labels (`istio.io/rev` and `istio-injection`).
- Optional periodic ticker when `--reconcile-period` is set.

ConfigMap and MWC watches do not reconcile the watched object directly. Instead, they enqueue a synthetic reconcile request named `__istio_change__`. Because both use the same request name, controller-runtime deduplicates: multiple rapid events (e.g., several ConfigMaps changing) coalesce into a single reconcile run.

The periodic source enqueues a request named `__periodic_reconcile__`.

The namespace watch encodes “scan only this namespace” by issuing a request with the namespace name in the request name and an empty namespace field, and the reconciler treats that shape specially.

### 3) The main reconciler: routing and convergence (`internal/controller`)

`IstioChangeReconciler` is the single reconciler implementation used by the controller. It routes reconcile requests by inspecting the request fields:

- Name equals `__periodic_reconcile__`: build last-modified from ConfigMaps and MWCs, await delay, then scan.
- Name equals `__istio_change__` (ConfigMap or MWC change): same as periodic—build last-modified from ConfigMaps and MWCs, await delay, then scan.
- Namespace is empty and name is set: treat name as a namespace and scan only that namespace.
- Otherwise: ignore (unknown request).

All paths converge into the same pipeline:

- Fetch tag-to-revision mapping and tag last-modified from MWCs.
- Scan pods and determine which workloads are out of date.
- Annotate those workloads (or log only, in dry-run mode).

### 4) ConfigMap parsing and last-modified computation (`internal/configmap`)

Fortsa reads the Istio sidecar injector ConfigMap data at `data["values"]` (JSON) and extracts:

- Revision: from `revision`, `global.revision`, or ConfigMap label `istio.io/rev`, defaulting to `default`.
- Hub, tag, and proxy image: from `global.hub`, `global.tag`, and `global.proxy.image`.

It also computes a “last modified” timestamp for each ConfigMap using managed fields timestamps when available (falling back to creation timestamp). This timestamp feeds pod skip logic: pods created after the config change plus `--istiod-config-read-delay` are skipped (they may already have the correct sidecar).

### 5) lastModifiedByRevision for pod skip logic

The reconciler builds `revision -> lastModified` from the current istio-sidecar-injector ConfigMaps and passes it to the scanner. Pods created after config change + delay are skipped.

- **reconcileAll**: Builds the map inline from the ConfigMap list. When limitToNamespaces is nil, scans all namespaces; when non-nil, restricts to those namespaces.

Deduplication comes from the shared request name `__istio_change__`, which causes multiple watch events to coalesce into one reconcile.

### 6) Tag mapping: tag -> revision and tag last-modified (`internal/mwc`)

Istio revision tags are exposed via `MutatingWebhookConfiguration` objects named `istio-revision-tag-*`. Fortsa lists these MWCs and builds:

- `tagToRevision`: from labels `istio.io/tag` and `istio.io/rev`.
- `lastModifiedByTag`: a last-modified timestamp per tag for conservative skip logic when tags change.

This lets Fortsa interpret workloads that select a tag rather than a concrete revision.

### 7) Pod scanning: detect workloads with outdated sidecars (`internal/podscanner`)

The pod scanner is the read-heavy engine. It constructs the Istio webhook client internally. Its job is:

“Which restartable workloads currently have at least one pod whose Istio sidecar image does not match the image that would be injected right now?”

High-level algorithm:

- List pods (optionally restricted to a specific set of namespaces).
- Skip configured namespaces (`--skip-namespaces`).
- For each pod:
  - Resolve the owning workload by traversing ownerReferences.
  - Determine the Istio revision (or tag) from workload template labels; if absent, fall back to namespace labels.
  - Apply skip logic using:
    - ConfigMap last-modified by revision, plus `--istiod-config-read-delay`.
    - Tag MWC last-modified by tag, plus the same delay, when a tag is in use.
  - Build a synthetic Pod from the workload’s pod template.
  - Call the Istio injection webhook to compute the expected injected pod for that template.
  - Extract `istio-proxy` image from the live pod and the expected pod and compare:
    - If `--compare-hub` is false: compare image name + tag, ignore registry.
    - If `--compare-hub` is true: require full string equality (including registry).
- Deduplicate results to unique workloads (so a workload with multiple outdated pods is annotated once).

### 8) Owner chain traversal (`internal/podscanner`)

Pods are usually not owned directly by the top-level workload:

- Deployments typically own ReplicaSets, which own Pods.
- StatefulSets and DaemonSets can be represented via ControllerRevisions in the chain.

Fortsa traverses ReplicaSets and ControllerRevisions as intermediate owner-chain nodes only, and returns a restartable owner only when it finds one of:

- Deployment
- StatefulSet
- DaemonSet

### 9) Computing the expected injected pod: Istio webhook client (`internal/webhook`)

Fortsa does not re-implement Istio sidecar injection. Instead, the pod scanner (which constructs the webhook client internally) asks Istio:

- Take the synthetic Pod built from the workload template.
- Send it to the injection webhook as an AdmissionReview Create request.
- Apply the returned JSON patch to obtain the mutated pod.
- Extract the expected `istio-proxy` image.

Important implementation details:

- The webhook URL can be derived either from a webhook’s ClientConfig (URL or Service) or from the `istiod` service DNS name.
- TLS verification is configured using the `caBundle` from MutatingWebhookConfiguration.
- The HTTP client uses a bounded timeout.
- If no patch is returned, Fortsa treats this as “no injection happened” and uses the original pod as the expected result.

### 10) Detect outdated sidecars (comparison logic)

Once the expected mutated pod is available, the scanner extracts the `istio-proxy` container image from:

- The live pod (regular containers first, then init containers to support Kubernetes native sidecars patterns).
- The expected mutated pod returned by Istio.

If the images do not match under the configured comparison mode, the owning workload is marked as needing a restart.

### 11) Actuation: trigger restarts by patching workloads (`internal/annotator`)

For each workload selected for restart, Fortsa patches the workload’s pod template annotations:

- `spec.template.metadata.annotations["fortsa.scaffidi.net/restartedAt"] = <RFC3339 timestamp>`

This triggers a rolling restart (same mechanism as `kubectl rollout restart`).

Safety features:

- `--annotation-cooldown`: skip re-annotating if a workload was annotated recently.
- `--restart-delay`: optional throttle between annotations.
- `--dry-run`: do not patch, only log “would annotate”.

### 12) All triggers feed the same pipeline

Regardless of trigger source (ConfigMap change, MWC change, namespace label change, or periodic tick), the system converges to:

- Fetch tag mapping.
- Scan pods and dedupe workloads.
- Annotate (or dry-run log) the workloads that appear outdated.

### End-to-end trace (high-level)

1. A watch event (or periodic tick) enqueues a reconcile request.
2. The reconciler routes the request type and builds last-modified data from ConfigMaps and MWCs.
3. Fortsa optionally waits `--istiod-config-read-delay` so that webhook reads reflect updated config.
4. Fortsa scans pods and uses the Istio webhook to compute expected injection output.
5. Fortsa patches qualifying workloads with `fortsa.scaffidi.net/restartedAt` (subject to cooldown and throttling).
6. Kubernetes rolls the workload and newly created pods get injected by Istio with the expected sidecar.
