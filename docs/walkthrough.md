## Fortsa: linear code walkthrough (prose-only)

This is a linear walkthrough of how Fortsa works internally. It is intentionally **prose-only** (no fenced code blocks) so it reads like a narrative companion to the source.

### 1) Entry point and controller wiring (`cmd/main.go`)

Fortsa runs as a controller-runtime manager. The entry point is responsible for:

- Parsing flags that control safety and behavior (dry-run, compare-hub, delays, cooldowns, skip namespaces).
- Configuring logging and TLS options (notably: HTTP/2 is disabled by default).
- Creating the controller-runtime Manager (metrics, probes, leader election).
- Constructing the Webhook client and the main reconciler.
- Defining which objects are watched and how events enqueue reconcile requests.

At startup it wires a controller that is primarily `For(ConfigMap)` with additional watches for:

- Istio’s tag-mapping `MutatingWebhookConfiguration` objects (`istio-revision-tag-*`).
- Namespaces whose Istio labels change (`istio.io/rev` or `istio-injection`).
- An optional periodic reconcile source when `--reconcile-period` is set.

### 2) Watches and reconcile requests (`internal/mwc`, `internal/namespace`, `internal/periodic`)

Fortsa watches three kinds of inputs:

- ConfigMaps: `istio-sidecar-injector*` in `istio-system` (primary signal).
- MutatingWebhookConfigurations: `istio-revision-tag-*` (tag-to-revision mapping changes).
- Namespaces: changes to Istio-related labels (`istio.io/rev` and `istio-injection`).

Two of these watches do not reconcile the watched object directly. Instead, they enqueue a synthetic reconcile request that the reconciler interprets:

- The MWC watch enqueues a request named `__mwc_reconcile__`.
- The periodic source enqueues a request named `__periodic_reconcile__`.

The namespace watch encodes “scan only this namespace” by issuing a request with a name but no namespace field, and the reconciler treats that shape specially.

### 3) The main reconciler: routing and convergence (`internal/controller`)

`IstioChangeReconciler` is the single reconciler implementation used by the controller. It routes reconcile requests by inspecting the request fields:

- Name equals `__periodic_reconcile__`: refresh the ConfigMap-derived cache from all matching ConfigMaps, then scan.
- Name equals `__mwc_reconcile__`: refresh tag mapping, then scan (ConfigMaps are unchanged).
- Namespace is empty and name is set: treat name as a namespace and scan only that namespace.
- Otherwise: treat it as a ConfigMap reconcile request and proceed with parsing and change detection.

All paths converge into the same pipeline:

- Fetch tag-to-revision mapping and tag last-modified from MWCs.
- Scan pods and determine which workloads are out of date.
- Annotate those workloads (or log only, in dry-run mode).

### 4) ConfigMap parsing and last-modified computation (`internal/configmap`)

Fortsa reads the Istio sidecar injector ConfigMap data at `data["values"]` (JSON) and extracts:

- Revision: from `revision`, `global.revision`, or ConfigMap label `istio.io/rev`, defaulting to `default`.
- Hub, tag, and proxy image: from `global.hub`, `global.tag`, and `global.proxy.image`.

It also computes a “last modified” timestamp for each ConfigMap using managed fields timestamps when available (falling back to creation timestamp). This timestamp is used for:

- Change detection (avoid rescanning on reconciles that do not represent a real config change).
- Skip logic (avoid scanning pods that were created after the config change plus a delay window).

### 5) Change detection state: `RevisionCache` (`internal/cache`)

The reconciler maintains a process-local cache:

- `revision -> lastModified`: used to detect changes and to feed skip logic.
- `configMapKey -> revision`: used to remove cache entries when a watched ConfigMap is deleted.

This cache is guarded by a mutex because reconciles can run concurrently.

### 6) Tag mapping: tag -> revision and tag last-modified (`internal/mwc`)

Istio revision tags are exposed via `MutatingWebhookConfiguration` objects named `istio-revision-tag-*`. Fortsa lists these MWCs and builds:

- `tagToRevision`: from labels `istio.io/tag` and `istio.io/rev`.
- `lastModifiedByTag`: a last-modified timestamp per tag for conservative skip logic when tags change.

This lets Fortsa interpret workloads that select a tag rather than a concrete revision.

### 7) Pod scanning: detect workloads with outdated sidecars (`internal/podscanner`)

The pod scanner is the read-heavy engine. Its job is:

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

Fortsa does not re-implement Istio sidecar injection. Instead, it asks Istio:

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

### 12) Periodic and namespace triggers feed the same pipeline

Regardless of trigger source (ConfigMap change, tag mapping change, namespace label change, or periodic tick), the system converges to:

- Fetch tag mapping.
- Scan pods and dedupe workloads.
- Annotate (or dry-run log) the workloads that appear outdated.

### End-to-end trace (high-level)

1. A watch event (or periodic tick) enqueues a reconcile request.
2. The reconciler routes the request type and optionally refreshes cache state.
3. Fortsa optionally waits `--istiod-config-read-delay` so that webhook reads reflect updated config.
4. Fortsa scans pods and uses the Istio webhook to compute expected injection output.
5. Fortsa patches qualifying workloads with `fortsa.scaffidi.net/restartedAt` (subject to cooldown and throttling).
6. Kubernetes rolls the workload and newly created pods get injected by Istio with the expected sidecar.

