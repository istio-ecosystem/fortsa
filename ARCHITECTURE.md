# Fortsa Architecture

This document describes how Fortsa works internally: its components, data flows, and package structure.

## Overview

**Purpose**: Fortsa is a Kubernetes operator that keeps Istio's data-plane up-to-date by automatically restarting workloads with outdated sidecar proxies when Istio configuration changes.

**Design principles**:

- No CRDs; uses only built-in Kubernetes resources
- Minimal dependencies (controller-runtime, k8s.io/client-go)
- Single binary deployed via bare container (FROM scratch)
- No access required outside the cluster

**Key mechanism**: Fortsa adds the `fortsa.scaffidi.net/restartedAt` annotation to Deployments, StatefulSets, and DaemonSets. This triggers a rolling restart of their pods—the same mechanism used by `kubectl rollout restart`. When a pod is recreated, Istio's MutatingWebhook injects the updated sidecar proxy.

## Package Structure

```text
fortsa/
├── cmd/main.go              # Entry point, flag parsing, manager setup
├── internal/
│   ├── cache/               # ConfigMap revision cache for pod skip logic
│   ├── controller/          # IstioChangeReconciler, reconcile routing
│   ├── mwc/                 # MWC predicate, tag mapping fetch, reconcile request
│   ├── namespace/            # Namespace predicates, reconcile request
│   ├── periodic/            # Periodic reconcile source, reconcile request
│   ├── podscanner/          # Pod scanning, outdated sidecar detection
│   ├── annotator/           # Workload annotation for restarts
│   ├── configmap/           # ConfigMap predicate, reconcile request, Istio sidecar injector parsing
│   └── webhook/             # Istio injection webhook client
├── config/                  # Kustomize manifests (RBAC, manager, Prometheus)
├── helm/src/chart/          # Helm chart for deployment
└── test/                    # E2E and integration tests
```

**Package dependency graph**:

```text
controller → annotator, configmap, mwc, periodic, podscanner, webhook
mwc        → (k8s client)
namespace  → (controller-runtime)
periodic   → (controller-runtime)
podscanner → configmap, webhook
annotator  → podscanner
```

The `configmap` package provides both the ConfigMap trigger (Filter, ReconcileRequest) and parser (ParseConfigMapValues, GetConfigMapLastModified). The `mwc` package provides the MWC trigger (Filter, ReconcileRequest) and tag mapping fetch (FetchTagToRevisionAndLastModified). ConfigMap and MWC both use the same reconcile request name (`__istio_change__`) so controller-runtime deduplicates their events.

## Component Architecture

```mermaid
flowchart TB
    subgraph watches [Fortsa Watches]
        ConfigMaps["ConfigMaps
        istio-sidecar-injector*"]
        MWCs["MutatingWebhookConfigurations
        istio-revision-tag-*"]
        Namespaces["Namespaces
        istio.io/rev, istio-injection"]
        Periodic["Periodic Reconcile"]
    end

    subgraph fortsa [Fortsa Operator]
        Reconciler[IstioChangeReconciler]
        Scanner[PodScanner]
        Annotator[WorkloadAnnotator]
        WebhookClient[WebhookClient]
    end

    subgraph k8s [Kubernetes Cluster]
        Pods["Pods with Istio sidecar"]
        Workloads["Deployments / DaemonSets / StatefulSets"]
    end

    subgraph istio [Istio]
        Webhook["Istio MutatingWebhook
        injects sidecar"]
    end

    ConfigMaps --> Reconciler
    MWCs --> Reconciler
    Namespaces --> Reconciler
    Periodic --> Reconciler

    Reconciler -->|"parse expected image"| Scanner
    Scanner -->|"CallWebhook"| WebhookClient
    WebhookClient -->|"AdmissionReview"| Webhook
    Scanner -->|"compare pod vs config"| Pods
    Scanner -->|"outdated workloads"| Annotator
    Annotator -->|"patch with restartedAt"| Workloads
    Workloads -->|"rolling restart"| Pods
    Pods -->|"new pod creation"| Webhook
    Webhook -->|"inject updated sidecar"| Pods
```

## Startup Flow

The application starts in [cmd/main.go](cmd/main.go) with the following sequence:

1. **Parse flags**: `--dry-run`, `--compare-hub`, `--restart-delay`, `--istiod-config-read-delay`, `--reconcile-period`, `--annotation-cooldown`, `--skip-namespaces`, TLS paths for webhook and metrics
2. **Register scheme**: `clientgoscheme` and `corev1` for Kubernetes API types
3. **Create Manager**: controller-runtime Manager with leader election (`71f32f9d.fortsa.scaffidi.net`), metrics on `:8080`, health probes on `:8081`
4. **Instantiate components**: `WebhookClient` and `IstioChangeReconciler` (which creates `PodScanner` and `WorkloadAnnotator` internally)
5. **Build controller**: `Watches` for ConfigMap (with predicate), MutatingWebhookConfiguration (with predicate), Namespace (with predicate), and optional periodic source. No primary `For` resource.
6. **Add health checks**: `healthz.Ping` for healthz and readyz
7. **Start manager**: `mgr.Start(ctrl.SetupSignalHandler())`

## Reconcile Flow

The `Reconcile` method in [internal/controller/istio_change_reconciler.go](internal/controller/istio_change_reconciler.go) routes requests by type:

```mermaid
flowchart TD
    Reconcile[Reconcile Request]
    Reconcile --> CheckName{Request.Name?}
    CheckName -->|"__periodic_reconcile__"| ReconcileAll[reconcileAll]
    CheckName -->|"__istio_change__"| ReconcileAll
    CheckName -->|Namespace only| ReconcileAllNS[reconcileAll with namespaces]
    CheckName -->|Other| Return[Return]
    ReconcileAll --> BuildRev[Build lastModifiedByRevision from ConfigMaps]
    ReconcileAllNS --> BuildRev
    BuildRev --> AwaitDelay[awaitIstiodConfigReadDelay]
    AwaitDelay --> FetchTag[fetchTagMappingAndScan]
    FetchTag --> ScanAnnotate[scanAndAnnotate]
    ScanAnnotate --> AnnotateDelay[annotateWorkloadsWithDelay]
```

**Request types**:

| Request | Trigger | Handler |
| ------- | ------- | ------- |
| `__periodic_reconcile__` | Periodic ticker | `reconcileAll()` — full scan of all ConfigMaps |
| `__istio_change__` | ConfigMap or MWC change | `reconcileAll()` — clear cache, repopulate, delay, scan |
| Namespace-only (req.Name = namespace, req.Namespace empty) | Namespace label change | `reconcileAll(ctx, []string{namespace})` — scan only that namespace |

ConfigMap and MWC watches both enqueue the same request name `__istio_change__`. Controller-runtime deduplicates by NamespacedName, so multiple rapid events coalesce into a single reconcile run.

All paths converge on `fetchTagMappingAndScan()` → `scanAndAnnotate()` → `annotateWorkloadsWithDelay()`.

## Per-Pod Outdated Detection Flow

The PodScanner in [internal/podscanner/pod_scanner.go](internal/podscanner/pod_scanner.go) determines whether a pod has an outdated Istio sidecar:

```mermaid
flowchart TD
    ListPods[List pods]
    ListPods --> ForEach[For each pod]
    ForEach --> FindOwner[findWorkloadOwner]
    FindOwner --> ResolveChain[Follow ReplicaSet / ControllerRevision]
    ResolveChain --> WorkloadRef[Deployment / StatefulSet / DaemonSet]
    WorkloadRef --> GetRev[getIstioRevFromWorkloadOrNamespace]
    GetRev --> ResolveTag[Resolve tag to revision via tagToRevision]
    ResolveTag --> SkipCheck{shouldSkipPodForConfigMap?}
    SkipCheck -->|Yes| NextPod[Next pod]
    SkipCheck -->|No| BuildPod[buildPodFromWorkload]
    BuildPod --> CallWebhook[WebhookClient.CallWebhook]
    CallWebhook --> Compare{istio-proxy image match?}
    Compare -->|Yes| NextPod
    Compare -->|No| AddResult[Add WorkloadRef to results]
    AddResult --> NextPod
```

**Steps**:

1. **List pods** — Optionally limited by namespace (e.g., for namespace-scoped reconciliation)
2. **findWorkloadOwner** — Follow ownerReferences: Pod → ReplicaSet/ControllerRevision → Deployment/StatefulSet/DaemonSet
3. **getIstioRevFromWorkloadOrNamespace** — Get `istio.io/rev` from workload pod template, or from namespace (`istio.io/rev` or `istio-injection=enabled`); returns `"default"` when namespace has `istio-injection=enabled`
4. **Resolve tag → revision** — Use `tagToRevision` map from `istio-revision-tag-*` MWCs
5. **shouldSkipPodForConfigMap** — Skip if pod was created at or after max(ConfigMap lastModified, MWC lastModified) + IstiodConfigReadDelay
6. **buildPodFromWorkload** — Construct a Pod from the workload's pod template
7. **WebhookClient.CallWebhook** — Send AdmissionReview to Istio `/inject`, get mutated pod with expected sidecar
8. **Compare images** — Extract `istio-proxy` image from current pod and from webhook response; compare (optionally including registry via `--compare-hub`)
9. **If mismatch** — Add WorkloadRef to results for annotation

## Webhook Client

[internal/webhook/webhook_client.go](internal/webhook/webhook_client.go) calls the Istio sidecar injection webhook to determine the expected pod shape:

- **URL and caBundle**: Read from `istio-sidecar-injector` or `istio-sidecar-injector-<revision>` MutatingWebhookConfiguration; for default tag, prefers `istio-revision-tag-default` if it exists
- **Request**: Sends `AdmissionReview` with the pod (built from workload template) to the webhook's `/inject` path
- **Response**: Applies the JSON patch from the response to obtain the mutated pod, then extracts the expected `istio-proxy` container image
- **Revisions**: Supports both default revision (`istiod.istio-system.svc`) and revision-specific (`istiod-<revision>.istio-system.svc`)

## ConfigMap Parser

[internal/configmap/configmap_parser.go](internal/configmap/configmap_parser.go) parses Istio sidecar injector ConfigMaps:

- **Input**: ConfigMap with `values` key containing JSON (e.g., `istio-sidecar-injector`, `istio-sidecar-injector-default`)
- **Extracts**: `Revision`, `Hub`, `Tag`, `Image` (global.proxy.image) from the values JSON; revision can also come from ConfigMap label `istio.io/rev`
- **LastModified**: Uses `metadata.managedFields` timestamps when available; falls back to `creationTimestamp`

## Workload Annotator

[internal/annotator/workload_annotator.go](internal/annotator/workload_annotator.go) triggers rolling restarts:

- **Annotation**: Patches `spec.template.metadata.annotations` with `fortsa.scaffidi.net/restartedAt` (RFC3339 timestamp) via JSON merge patch
- **Cooldown**: When `annotationCooldown` is set, skips re-annotating if the workload was annotated within that duration
- **Supported kinds**: Deployment, StatefulSet, DaemonSet

## Kubernetes Resources

| Resource | Usage |
| -------- | ----- |
| ConfigMap | Istio sidecar injector config (`istio-sidecar-injector*`) |
| MutatingWebhookConfiguration | Tag mapping (`istio-revision-tag-*`), webhook URL/caBundle |
| Namespace | Istio labels (`istio.io/rev`, `istio-injection`) |
| Pod | Sidecar image comparison |
| Deployment / StatefulSet / DaemonSet | Patched with restartedAt annotation |
| ReplicaSet / ControllerRevision | Owner chain traversal |

## Configuration Flags

| Flag | Default | Description |
| ---- | ------- | ----------- |
| `--dry-run` | false | Log what would be done without annotating workloads |
| `--compare-hub` | false | Require container image registry to match ConfigMap hub when detecting outdated pods |
| `--restart-delay` | 0 | Delay between restarting each workload (e.g., 5s) |
| `--istiod-config-read-delay` | 10s | Wait for Istiod to read updated ConfigMap before scanning |
| `--reconcile-period` | 0 | Period between full reconciliations; 0 disables periodic reconcile |
| `--annotation-cooldown` | 5m | Skip re-annotating if workload was annotated within this duration |
| `--skip-namespaces` | kube-system,istio-system | Comma-separated namespaces to skip when scanning pods |

## lastModifiedByRevision and Pod Skip Logic

The reconciler builds `lastModifiedByRevision` (revision -> ConfigMap LastModified) from the current istio-sidecar-injector ConfigMaps and passes it to the pod scanner for skip logic. Pods created after config change + IstiodConfigReadDelay are skipped (they may already have the correct sidecar).

- **reconcileAll**: Builds the map inline from the ConfigMap list before scanning. When limitToNamespaces is nil, scans all namespaces; when non-nil, restricts to those namespaces.

Deduplication comes from the shared request name `__istio_change__`: multiple ConfigMap and MWC watch events coalesce into one reconcile before the workqueue processes them.
