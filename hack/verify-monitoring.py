#!/usr/bin/env python3
"""Offline contract checks for kube-symbiont monitoring assets."""

from __future__ import annotations

import json
import pathlib
import shutil
import subprocess
import sys


ROOT = pathlib.Path(__file__).resolve().parents[1]
DASHBOARD = ROOT / "config/grafana/kube-symbiont-dashboard.json"
RULES = ROOT / "config/prometheus/rules.yaml"
HELM_RULES = ROOT / "charts/chart/templates/prometheus/prometheusrule.yaml"


def fail(message: str) -> None:
    raise AssertionError(message)


def run(*args: str) -> str:
    executable = shutil.which(args[0])
    if executable is None:
        fail(f"required offline renderer not found: {args[0]}")
    result = subprocess.run(
        [executable, *args[1:]],
        cwd=ROOT,
        check=False,
        capture_output=True,
        text=True,
    )
    if result.returncode:
        fail(f"{' '.join(args)} failed:\n{result.stderr}")
    return result.stdout


def require_failure(expected: str, *args: str) -> None:
    executable = shutil.which(args[0])
    if executable is None:
        fail(f"required offline renderer not found: {args[0]}")
    result = subprocess.run(
        [executable, *args[1:]],
        cwd=ROOT,
        check=False,
        capture_output=True,
        text=True,
    )
    output = result.stdout + result.stderr
    if result.returncode == 0 or expected not in output:
        fail(f"{' '.join(args)} did not fail with expected message {expected!r}")


def validate_dashboard() -> None:
    dashboard = json.loads(DASHBOARD.read_text())
    variables = dashboard["templating"]["list"]
    variable_ref_ids = [
        item["query"]["refId"]
        for item in variables
        if isinstance(item.get("query"), dict) and "refId" in item["query"]
    ]
    if len(variable_ref_ids) != len(set(variable_ref_ids)):
        fail("dashboard variable query refIds must be unique")

    for panel in dashboard["panels"]:
        targets = panel.get("targets", [])
        ref_ids = [target.get("refId") for target in targets]
        if None in ref_ids or len(ref_ids) != len(set(ref_ids)):
            fail(f"panel {panel.get('id')} target refIds must be present and unique")
        for target in targets:
            legend = target.get("legendFormat", "")
            if "{{name}}" in legend and "{{namespace}}" not in legend:
                fail(f"panel {panel.get('id')} legend is not namespace-scoped")

    text = DASHBOARD.read_text()
    if "changes(" in text:
        fail("counter activity must use increase(), not changes()")
    if "KubeSymbiont" in text:
        fail("dashboard unexpectedly contains alert definitions")


def validate_rule_sources() -> None:
    required_alerts = {
        "KubeSymbiontMeasurementStale",
        "KubeSymbiontReconcileFailures",
        "KubeSymbiontReservationFlapping",
        "KubeSymbiontDriftBelowReservationCPU",
        "KubeSymbiontDriftBelowReservationMemory",
        "KubeSymbiontMetricsTargetDown",
    }
    for path in (RULES, HELM_RULES):
        text = path.read_text()
        if "changes(" in text:
            fail(f"{path.relative_to(ROOT)} uses changes() on a counter")
        missing = [alert for alert in required_alerts if f"alert: {alert}" not in text]
        if missing:
            fail(f"{path.relative_to(ROOT)} is missing alerts: {', '.join(missing)}")
        if "interval: 30s" not in text:
            fail(f"{path.relative_to(ROOT)} does not declare the 30s rule profile")


def validate_kustomize() -> None:
    rendered = run("kubectl", "kustomize", "config/monitoring")
    required = (
        "kind: ServiceMonitor",
        "kind: PrometheusRule",
        "kind: ConfigMap",
        "kind: PriorityClass",
        "name: controller-manager-metrics-monitor",
        "name: controller-manager-rules",
        "name: controller-manager-dashboard",
        "namespace: kube-symbiont-system",
        "release: kube-prometheus-stack",
        'grafana_dashboard: "1"',
        "kube-symbiont-dashboard.json: |",
    )
    missing = [item for item in required if item not in rendered]
    if missing:
        fail(f"monitoring overlay is missing rendered contracts: {missing}")
    if "changes(" in rendered:
        fail("rendered monitoring overlay uses changes() on a counter")


def validate_helm() -> None:
    run("helm", "lint", "charts/chart")
    require_failure(
        "prometheus.enabled=true requires metrics.enabled=true",
        "helm",
        "template",
        "invalid-monitoring",
        "charts/chart",
        "--set",
        "prometheus.enabled=true",
        "--set",
        "metrics.enabled=false",
    )
    rendered = run(
        "helm",
        "template",
        "verify-monitoring",
        "charts/chart",
        "--namespace",
        "kube-symbiont-system",
        "--set",
        "prometheus.enabled=true",
        "--set",
        "prometheus.rule.enabled=true",
        "--set",
        "prometheus.additionalLabels.release=kube-prometheus-stack",
        "--set",
        "metrics.tls.existingSecret=metrics-server-cert",
        "--set",
        "prometheus.scraperServiceAccount.name=kube-prometheus-stack-prometheus",
        "--set",
        "prometheus.scraperServiceAccount.namespace=monitoring",
    )
    for item in (
        "kind: ServiceMonitor",
        "kind: PrometheusRule",
        "KubeSymbiontDriftBelowReservationMemory",
        "KubeSymbiontMetricsTargetDown",
        "release: kube-prometheus-stack",
        "secretName: metrics-server-cert",
        "--metrics-cert-path=/tmp/k8s-metrics-server/metrics-certs",
        "name: kube-prometheus-stack-prometheus",
        "namespace: monitoring",
    ):
        if item not in rendered:
            fail(f"Helm monitoring render is missing {item}")
    if "changes(" in rendered:
        fail("rendered Helm rules use changes() on a counter")


def main() -> int:
    validate_dashboard()
    validate_rule_sources()
    validate_kustomize()
    validate_helm()
    print("monitoring assets: PASS")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (AssertionError, json.JSONDecodeError) as error:
        print(f"monitoring assets: FAIL: {error}", file=sys.stderr)
        sys.exit(1)
