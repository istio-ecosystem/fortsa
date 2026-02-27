# Helm Chart Sources

Because I don't really like how the kubebuilder helm plugin works, this
is a hack to generate the helm chart for release...

The `Chart.yaml` and `values.yaml` files are processed using `envsubst` to
set values that pertain to the release. Then the helm chart tarball is generated.
