#!/usr/bin/env python3
"""Keep the Helm CRD template mechanically derived from controller-gen output."""

from __future__ import annotations

import argparse
import difflib
import pathlib
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
BASE = ROOT / "config/crd/bases/symbiont.tensorhost.com_shadowworkloads.yaml"
CHART = ROOT / "charts/chart/templates/crd/shadowworkloads.symbiont.tensorhost.com.yaml"


def render_chart_crd() -> str:
    generated = BASE.read_text()
    if generated.startswith("---\n"):
        generated = generated[4:]

    marker = "metadata:\n  annotations:\n"
    if generated.count(marker) != 1:
        raise RuntimeError("generated CRD must contain exactly one metadata annotations block")

    helm_annotations = (
        "metadata:\n"
        "  annotations:\n"
        "    {{- if .Values.crd.keep }}\n"
        '    "helm.sh/resource-policy": keep\n'
        "    {{- end }}\n"
    )
    generated = generated.replace(marker, helm_annotations, 1)
    return "{{- if .Values.crd.enabled }}\n" + generated.rstrip() + "\n{{- end }}\n"


def main() -> int:
    parser = argparse.ArgumentParser()
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--check", action="store_true")
    mode.add_argument("--write", action="store_true")
    args = parser.parse_args()

    expected = render_chart_crd()
    if args.write:
        CHART.write_text(expected)
        print(f"updated {CHART.relative_to(ROOT)}")
        return 0

    actual = CHART.read_text()
    if actual == expected:
        print("Helm CRD template matches controller-gen output")
        return 0

    diff = difflib.unified_diff(
        actual.splitlines(keepends=True),
        expected.splitlines(keepends=True),
        fromfile=str(CHART.relative_to(ROOT)),
        tofile=f"expected:{CHART.relative_to(ROOT)}",
    )
    sys.stderr.writelines(diff)
    print(
        "Helm CRD template is stale; run `make sync-helm-crd` after changing the API schema.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError) as error:
        print(f"Helm CRD sync failed: {error}", file=sys.stderr)
        raise SystemExit(1)
