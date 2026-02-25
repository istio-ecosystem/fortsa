# E2E Scripts

The shell scripts in this directory are a **fallback** for running end-to-end tests. The canonical e2e suite is the Ginkgo/Gomega tests in `test/e2e/`. Run them with:

```bash
make test-e2e          # All e2e tests
make test-e2e-istio    # Istio tests only (revision tags, namespace labels, in-place upgrade)
```

Use the shell scripts when Go tests are not preferred or for manual runs. They mirror the scenarios covered by the Ginkgo tests.
