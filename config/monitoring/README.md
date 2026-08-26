# Monitoring adoption profile

This is an explicit, non-default Kustomize profile for clusters running the
Prometheus Operator and a Grafana dashboard sidecar. It composes the canonical
cluster prerequisites and default controller bundle with the metrics Service,
`ServiceMonitor`, `PrometheusRule`, and dashboard `ConfigMap` in
`kube-symbiont-system`:

```sh
kubectl kustomize config/monitoring > /tmp/kube-symbiont-monitoring.yaml
```

Review that rendered file before applying it. The profile labels the
`ServiceMonitor` and `PrometheusRule` with
`release: kube-prometheus-stack`, the common selector used by that chart. If
your Prometheus stack selects another label, change the two patch values in
`kustomization.yaml`; do not add the label to application selectors.

`metrics-reader-binding.yaml` grants the non-resource `/metrics` permission to
the standard `kube-prometheus-stack-prometheus` ServiceAccount in `monitoring`.
Change that subject before rendering if your Prometheus release uses another
name or namespace; otherwise authenticated scrapes will correctly fail with
HTTP 403.

The bundled alerts assume the documented `30s` ShadowWorkload poll profile.
If a workload uses a materially longer poll interval, clone and tune the stale
threshold for that deployment rather than treating a longer interval as a
failure.

Run the offline packaging checks before adoption:

```sh
python3 hack/verify-monitoring.py
```

The check renders both Kustomize and Helm paths, validates dashboard JSON and
query identifiers, and guards the counter/alert contracts. It does not contact
a cluster.

The dashboard ConfigMap has `grafana_dashboard: "1"`, which the standard
kube-prometheus-stack dashboard sidecar discovers. Without a sidecar, import
`config/grafana/kube-symbiont-dashboard.json` directly in Grafana.

This Kustomize profile is deliberately development-only: it retains the
explicit `insecureSkipVerify` setting from `config/prometheus` and must not be
treated as a production-grade TLS path. For verified TLS, use the Helm path
with `metrics.tls.existingSecret` pointing at a Secret containing `ca.crt`,
`tls.crt`, and `tls.key`; the chart mounts the same Secret into the manager and
fails closed unless the Prometheus ServiceAccount is also declared.
