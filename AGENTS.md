# Agents Standards

## Purpose

This file defines **non-negotiable engineering standards** for anyone (human or AI agent) making changes to this repository. It is optimized for **high Go code quality**, long-term maintainability, and safety when interacting with Kubernetes clusters.

Fortsa is a Kubernetes operator. Small mistakes (reconcile loops, incorrect patching, unsafe retries, noisy logs) can have outsized impact in production. Treat this repo as **production-grade control-plane code**.

## Non-negotiables (read first)

- You **MUST** keep the operator **safe-by-default**:
  - **MUST NOT** cause uncontrolled reconcile storms.
  - **MUST NOT** introduce hot loops (tight polling, immediate requeues without backoff/jitter).
  - **MUST NOT** patch workloads repeatedly without cooldown/guards.
  - **MUST NOT** broaden RBAC needs without explicit justification and review.
- You **MUST** follow communication and documentation hygiene:
  - **NEVER** use emoji, or Unicode characters that emulate emoji (e.g. ✓, ✗).
  - **MUST** avoid redundant comments that are tautological/self-demonstrating (e.g. restating what a clearly named function does, or narrating code that is obvious at a glance).
- You **MUST** preserve the project’s architectural constraints:
  - **MUST NOT** introduce CRDs (this project explicitly aims for “no CRDs”).
  - **MUST** keep dependencies minimal and appropriate for controller-runtime/client-go ecosystems.
  - **MUST** keep the final artifact as a **single Go binary** suitable for a minimal container image.
- You **MUST** keep code formatted and lint-clean:
  - **MUST** run `make fmt`.
  - **MUST** run `make vet`.
  - **MUST** run `make lint` (golangci-lint).
  - **MUST** ensure tests relevant to your change pass (`make test`, and `make test-integration` / `make test-e2e*` when appropriate).

## Repository workflows (canonical commands)

Use the Makefile targets as the canonical workflow:

- **Formatting**: `make fmt`
- **Static analysis**: `make vet`
- **Lint**: `make lint` (and `make lint-fix` only for safe, mechanical fixes)
- **Unit tests**: `make test`
- **Integration tests**: `make test-integration`
- **E2E tests**: `make test-e2e` or `make test-e2e-istio`
- **Build**: `make build`

**MUST** prefer `make <target>` over ad-hoc `go test` / `golangci-lint` invocations so that versioned tooling and repo defaults are applied consistently.

## Go version and language features

- **MUST** assume Go version requirements defined by the repo (see README; currently Go `v1.26.0+`).
- **MUST** write idiomatic Go:
  - Prefer clarity over cleverness.
  - Keep functions small and single-purpose.
  - Avoid unnecessary generics; use generics only when they clearly reduce duplication without harming readability.

## Project structure and dependency boundaries

The repo uses a layered package structure under `internal/`. Follow it.

- **MUST** keep new code in the most appropriate existing package (`internal/controller`, `internal/podscanner`, `internal/annotator`, etc.).
- **MUST** avoid cyclic dependencies; prefer “controller → lower layers” flow.
- **SHOULD** keep packages cohesive:
  - `controller`: orchestration and reconcile routing.
  - `podscanner`: cluster reads and detection logic.
  - `annotator`: workload patching/updates.
  - `webhook`: calling Istio injection webhook and parsing.
- **MUST** keep `cmd/main.go` focused on wiring, flags, and manager startup.

## Formatting, naming, and API surface

### Formatting

- **MUST** use `gofmt` (via `make fmt`).
- **MUST NOT** commit code that depends on “manual formatting discipline.”

### Naming

- **MUST** follow Go naming conventions:
  - Package names: short, lower-case, no underscores.
  - Exported identifiers: only when needed by other packages; otherwise keep unexported.
  - Avoid stutter: e.g., `podscanner.Scanner` is better than `podscanner.PodScannerScanner`.
- **MUST** choose names that reflect intent and domain:
  - Use Kubernetes terms consistently: “workload” (Deployment/StatefulSet/DaemonSet), “pod template”, “owner reference”, “namespace label/annotation”, “reconcile request”.

### Public surface area

- **MUST** minimize exported APIs.
- If you export something:
  - **MUST** document it with a complete Go doc comment.
  - **MUST** keep it stable (expect downstream usage).

## Error handling (Go and Kubernetes-specific)

### General rules

- **MUST** wrap errors with context using `%w`:

```go
return fmt.Errorf("parse istio values JSON: %w", err)
```

- **MUST NOT** swallow errors unless the behavior is explicitly desired and safe; when intentionally ignoring an error, you **MUST** justify it in code with a short intent comment.
- **MUST** preserve sentinel error checks when used (e.g., `apierrors.IsNotFound(err)`).

### Controller-runtime reconciliation semantics

- **MUST** treat reconciliation as **idempotent**:
  - Re-running reconcile with the same cluster state must not change anything.
  - Reconcile should converge, not oscillate.
- **MUST** distinguish between:
  - transient errors (requeue is appropriate),
  - terminal/expected conditions (return success, no requeue),
  - “not found” (usually success; the object is gone).
- **MUST NOT** requeue immediately in a tight loop. Prefer returning errors (controller-runtime backoff) or deliberate `RequeueAfter` with a reasoned delay.

### Kubernetes API interactions

- **MUST** avoid unnecessary writes:
  - Prefer read/compare before patching.
  - Patch only when a change is required.
- **MUST** use patching correctly:
  - Prefer minimal merge patches to avoid clobbering fields.
  - **MUST** ensure patches are scoped only to fields you own (e.g., annotations under `spec.template.metadata.annotations` for workloads).
- **MUST** be careful with object mutations:
  - Do not mutate cached objects in-place in ways that leak across reconciles.
  - When constructing objects to send externally, prefer deep copies or fresh structs as appropriate.

## Context, timeouts, and cancellations

- **MUST** thread `context.Context` through all IO-bound operations.
- **MUST** respect cancellation:
  - If `ctx.Done()` is triggered, return promptly.
- **MUST** set bounded timeouts for outbound calls (e.g., webhook requests) unless the caller already enforces a timeout.
- **SHOULD** avoid “infinite” waits and sleeps. If sleeps are needed (e.g., Istiod config read delay), keep them explicit and configurable.

## Logging and observability

- **MUST** keep logs actionable:
  - Include enough identifying data to debug (namespace/name, workload kind, revision/tag).
  - Avoid dumping huge objects or full pod specs.
- **MUST** treat log volume as a production cost:
  - Frequent paths should use lower verbosity or be rate-limited by behavior (not a logging hack).
- **MUST NOT** log secrets or sensitive data (webhook CA bundles, tokens, raw admission payloads).
- **SHOULD** prefer structured logging fields over string concatenation.

## Concurrency and shared state

- **MUST** assume reconciles can run concurrently.
- **MUST** protect shared state with synchronization (mutex/atomic) and design for safe access.
- **MUST** avoid deadlocks and lock contention:
  - Keep lock scope minimal.
  - Do not call external IO while holding locks.
- **MUST** be explicit about thread-safety in types that are shared between reconciles.

## Determinism, ordering, and retries

- **MUST** design behavior to be deterministic under re-ordering:
  - Watch events can arrive in any order and may be duplicated.
  - Caches can be stale.
- **MUST** handle API conflicts (resourceVersion changes) gracefully:
  - Patch operations should be resilient.
  - If updates conflict, retry in a controlled manner.
- **SHOULD** avoid relying on object list ordering from the API server.

## Domain-specific safety rules (Fortsa/Istio)

- ReplicaSets and ControllerRevisions are traversed as intermediate owner-chain nodes only.
- Fortsa’s core action is to add/update `fortsa.scaffidi.net/restartedAt` on workloads.
  - **MUST** keep this mechanism correct and minimal.
  - **MUST** preserve cooldown semantics when present; do not add “helpful” restarts that can lead to churn.
- Fortsa compares desired vs actual proxy sidecar images.
  - **MUST** keep comparisons precise and well-defined (including behavior for hub comparison flags).
  - **MUST** avoid false positives that could restart large numbers of workloads unnecessarily.
- Fortsa depends on Istio injection webhook behavior.
  - **MUST** treat webhook calls as untrusted external input:
    - Validate response shapes.
    - Handle malformed patches gracefully.
    - Fail safely (no mutation if uncertain).

## Testing standards

### Minimum expectations

- **MUST** add or update tests when changing behavior.
- **MUST** ensure tests validate the important properties:
  - idempotency,
  - correct skip/cooldown behavior,
  - correct revision/tag mapping,
  - correct workload selection (Deployment/StatefulSet/DaemonSet),
  - correct patch payloads.

### Table-driven tests

- **SHOULD** use table-driven tests for logic-heavy helpers and parsing functions.
- **MUST** keep test cases readable:
  - Name each case with the behavior being tested.
  - Keep fixtures minimal; avoid massive YAML/JSON blobs unless the parsing logic requires realism.

### Fake clients and envtest

- **MUST** prefer unit tests for pure logic and parsing.
- **SHOULD** use envtest (via `make test` / `make test-integration`) when behavior depends on controller-runtime/client-go semantics.
- **MUST** keep tests deterministic:
  - Avoid sleeps; use polling with timeouts only when required.
  - Avoid reliance on real cluster behavior in unit/integration tests.

## Code style for Kubernetes operators

### Reconcile design

- **MUST** keep reconcile handlers small and composable:
  - Parse inputs → compute desired actions → apply minimal changes.
- **MUST** separate “what should happen” from “apply it”:
  - Business logic should be testable without a live API server.
- **MUST** avoid “do everything in reconcile” designs that grow unbounded:
  - Keep scanning and annotation steps isolated and individually testable.

### Resource ownership and field management

- **MUST** only modify fields that this controller owns.
- **MUST NOT** overwrite existing user annotations/labels unrelated to Fortsa’s function.
- **MUST** treat `managedFields` and server-side apply behavior carefully:
  - Do not depend on `managedFields` being present or stable for logic unless guarded with robust fallback.

## Security and supply chain

- **MUST** avoid adding large or risky dependencies.
- **MUST** prefer standard library and well-known Kubernetes libraries already in use.
- **MUST** validate external data (webhook responses, JSON/YAML parsing) and handle failure modes safely.
- **MUST NOT** introduce shelling-out or executing external binaries from the controller unless explicitly required and reviewed.

## Performance and scalability

- **MUST** treat cluster-wide scans as expensive:
  - Avoid repeated full scans on small changes.
  - Cache and short-circuit when safe (as the repo already does).
- **MUST** avoid per-pod expensive operations when a per-workload or cached approach is sufficient.
- **SHOULD** batch or deduplicate work:
  - Do not annotate the same workload multiple times in the same reconcile run.
  - Avoid repeated webhook calls for identical expected pod templates where caching is safe.

## Documentation expectations

- **MUST** update docs when changing user-facing behavior:
  - README for flags, build/run behavior, prerequisites.
  - ARCHITECTURE.md if control flow / package boundaries materially change.
- **SHOULD** add small examples to docs when changes are subtle (e.g., new skip logic).
- **MUST** keep code comments high-signal:
  - **MUST NOT** add redundant comments that simply restate what the code does.
  - Comments should explain intent, constraints, non-obvious trade-offs, or “why”, not narrate “what”.

## Review checklist (before considering work “done”)

- **Correctness**
  - **MUST** be idempotent.
  - **MUST** avoid reconcile storms/hot loops.
  - **MUST** handle NotFound and transient errors correctly.
- **Quality**
  - **MUST** be `gofmt`’d.
  - **MUST** pass `make vet` and `make lint`.
  - **MUST** have tests for behavior changes.
- **Operational safety**
  - **MUST** avoid unnecessary writes to the API server.
  - **MUST** keep log volume and verbosity appropriate.
  - **MUST** respect timeouts and cancellations.
- **Architecture**
  - **MUST NOT** introduce CRDs.
  - **MUST** keep dependencies minimal and package boundaries clean.
