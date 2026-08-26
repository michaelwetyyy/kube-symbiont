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

This profile does not configure cert-manager. Its ServiceMonitor retains the
development-compatible `insecureSkipVerify` setting from `config/prometheus`;
use the existing TLS patch and cert-manager integration before treating this
as a production-grade deployment.
