#!/usr/bin/env python3
"""Validate release versions and publication-order workflow contracts."""

from __future__ import annotations

import argparse
import pathlib
import re
import sys
from dataclasses import dataclass


ROOT = pathlib.Path(__file__).resolve().parents[1]
SEMVER_NUMBER = r"(?:0|[1-9]\d*)"
SEMVER_CORE = rf"{SEMVER_NUMBER}\.{SEMVER_NUMBER}\.{SEMVER_NUMBER}"
CANDIDATE_PATTERN = re.compile(
    rf"^v({SEMVER_CORE}-rc\.{SEMVER_NUMBER})$"
)
STABLE_PATTERN = re.compile(rf"^v({SEMVER_CORE})$")


@dataclass(frozen=True)
class ReleaseVersions:
    chart_version: str
    image_tag: str


def expected_versions(kind: str, tag: str) -> ReleaseVersions:
    if kind == "candidate":
        pattern = CANDIDATE_PATTERN
    elif kind == "stable":
        pattern = STABLE_PATTERN
    else:
        raise ValueError(f"unknown release kind: {kind!r}")
    match = pattern.fullmatch(tag)
    if match is None:
        raise ValueError(f"invalid {kind} release tag: {tag!r}")
    chart_version = match.group(1)
    image_tag = tag if kind == "candidate" else chart_version
    return ReleaseVersions(chart_version=chart_version, image_tag=image_tag)


def chart_metadata(path: pathlib.Path) -> ReleaseVersions:
    fields: dict[str, str] = {}
    for line in path.read_text().splitlines():
        match = re.match(r"^(version|appVersion):\s*[\"']?([^\"']+?)[\"']?\s*$", line)
        if match:
            if match.group(1) in fields:
                raise ValueError(f"duplicate {match.group(1)} field in {path}")
            fields[match.group(1)] = match.group(2)
    if set(fields) != {"version", "appVersion"}:
        raise ValueError(f"could not read version and appVersion from {path}")
    return ReleaseVersions(
        chart_version=fields["version"], image_tag=fields["appVersion"]
    )


def require_in_order(text: str, path: pathlib.Path, *needles: str) -> None:
    positions = [text.find(needle) for needle in needles]
    if -1 in positions or positions != sorted(positions):
        raise ValueError(
            f"{path.relative_to(ROOT)} must contain these contracts in order: "
            + ", ".join(needles)
        )


def require_lines_in_order(text: str, path: pathlib.Path, *lines: str) -> None:
    actual = [line.strip() for line in text.splitlines()]
    position = -1
    for expected in lines:
        try:
            position = actual.index(expected, position + 1)
        except ValueError as error:
            raise ValueError(
                f"{path.relative_to(path.parents[2])} must contain this ordered "
                f"release gate after line {position + 1}: {expected}"
            ) from error


def require_count(
    text: str, path: pathlib.Path, needle: str, minimum: int
) -> None:
    actual = text.count(needle)
    if actual < minimum:
        raise ValueError(
            f"{path.name} must contain {needle!r} at least {minimum} times; got {actual}"
        )


def verify_safe_candidate_inputs(text: str, path: pathlib.Path) -> None:
    allowed = {
        "CANDIDATE_TAG: ${{ inputs.image_tag }}",
        "EXPECTED_SHA: ${{ inputs.expected_sha }}",
        "ref: ${{ inputs.expected_sha }}",
    }
    unsafe = [
        line.strip()
        for line in text.splitlines()
        if "${{ inputs." in line and line.strip() not in allowed
    ]
    if unsafe:
        raise ValueError(
            f"{path.name} interpolates dispatch inputs outside an env/ref boundary: "
            f"{unsafe}"
        )


def verify_safe_release_inputs(text: str, path: pathlib.Path) -> None:
    allowed = {
        "RELEASE_TAG: ${{ inputs.release_tag }}",
        "CANDIDATE_TAG: ${{ inputs.candidate_tag }}",
        "EXPECTED_DIGEST: ${{ inputs.expected_candidate_digest }}",
        "EXPECTED_SHA: ${{ inputs.expected_sha }}",
        "ref: ${{ inputs.expected_sha }}",
    }
    unsafe = [
        line.strip()
        for line in text.splitlines()
        if "${{ inputs." in line and line.strip() not in allowed
    ]
    if unsafe:
        raise ValueError(
            f"{path.name} interpolates dispatch inputs outside an env/ref boundary: "
            f"{unsafe}"
        )


def verify_stable_promotion_workflow(text: str, path: pathlib.Path) -> None:
    # Stable is a promotion of the already scanned/attested candidate OCI index.
    # Rebuilding here would produce a different artifact from the one that soaked.
    forbidden = (
        "docker/build-push-action",
        "Build untagged stable digest",
        "Scan stable digest",
        "push-by-digest=true",
        "name-canonical=true",
    )
    present = [contract for contract in forbidden if contract in text]
    if present:
        raise ValueError(
            f"{path.name} must promote the tested candidate without rebuilding: {present}"
        )

    required = (
        "workflow_dispatch:",
        "release_tag:",
        "candidate_tag:",
        "expected_candidate_digest:",
        "expected_sha:",
        'test "${CANDIDATE_TAG%-rc.*}" = "$RELEASE_TAG"',
        'test "$(git rev-parse HEAD)" = "$EXPECTED_SHA"',
        "refs/remotes/origin/main",
        "Verify the tested candidate digest",
        'candidate_ref="${IMAGE}:${CANDIDATE_TAG#v}"',
        'test "$candidate_digest" = "$EXPECTED_DIGEST"',
        "Promote exact candidate digest to stable image tags",
        '--tag "$immutable_tag"',
        '--tag "${IMAGE}:${minor}"',
        '--tag "${IMAGE}:latest"',
        '"${IMAGE}@${EXPECTED_DIGEST}"',
        'test "$published_digest" = "$EXPECTED_DIGEST"',
        "Create or verify immutable Git tag",
        '-f ref="refs/tags/${RELEASE_TAG}"',
        'test "$tag_sha" = "$EXPECTED_SHA"',
        'make build-installer IMG="${IMAGE}@${EXPECTED_DIGEST}"',
        "Create GitHub release",
        "Promoted unchanged from ${CANDIDATE_TAG}",
    )
    missing = [contract for contract in required if contract not in text]
    if missing:
        raise ValueError(
            f"{path.relative_to(ROOT)} is missing stable promotion contracts: {missing}"
        )

    require_in_order(
        text,
        path,
        "Verify the tested candidate digest",
        "Promote exact candidate digest to stable image tags",
        "Create or verify immutable Git tag",
        "Build digest-pinned installer",
        "Create GitHub release",
    )
    verify_safe_release_inputs(text, path)


def verify_workflow_contracts(root: pathlib.Path = ROOT) -> None:
    candidate_path = root / ".github/workflows/candidate.yml"
    release_path = root / ".github/workflows/release.yml"
    candidate = candidate_path.read_text()
    release = release_path.read_text()

    for path, text in ((candidate_path, candidate), (release_path, release)):
        for contract in (
            "group: kube-symbiont-image-publication",
            "cancel-in-progress: false",
        ):
            if contract not in text:
                raise ValueError(
                    f"{path.relative_to(ROOT)} is missing publication contract {contract!r}"
                )

        require_lines_in_order(
            text,
            path,
            "python3 -B -m unittest hack/test_verify_release.py",
            "go mod tidy",
            "git diff --exit-code go.mod go.sum",
            "make test",
            "make lint-config",
            "make lint",
            "make verify-installer",
            "helm lint charts/chart",
            "python3 -B hack/verify-monitoring.py",
            "git diff --exit-code",
            'test -z "$(git status --porcelain --untracked-files=all)"',
        )

    for contract in (
        "push-by-digest=true",
        "name-canonical=true",
        "expected_sha:",
        "ref: ${{ inputs.expected_sha }}",
        'test "$(git rev-parse HEAD)" = "$EXPECTED_SHA"',
        "refs/remotes/origin/main",
        "Attest build provenance",
    ):
        if contract not in candidate:
            raise ValueError(f"candidate workflow is missing exact-source contract {contract!r}")

    verify_safe_candidate_inputs(candidate, candidate_path)
    require_count(candidate, candidate_path, "Could not prove candidate tag", 2)
    require_in_order(
        candidate,
        candidate_path,
        "Build untagged candidate digest",
        "Scan candidate digest",
        "Attest build provenance",
        "Promote scanned digest to immutable candidate tag",
    )

    verify_stable_promotion_workflow(release, release_path)


def verify_helm_image_contracts(root: pathlib.Path = ROOT) -> None:
    makefile = (root / "Makefile").read_text()
    manager = (root / "charts/chart/templates/manager/manager.yaml").read_text()
    makefile_contracts = (
        r"@(sha256:[0-9a-f]{64})",
        '--set-string "manager.image.repository=$$image_repository"',
        '--set-string "manager.image.tag=$$image_tag"',
    )
    missing = [contract for contract in makefile_contracts if contract not in makefile]
    if missing:
        raise ValueError(f"Makefile is missing Helm image contracts: {missing}")
    if 'if not (contains "@" .Values.manager.image.repository)' not in manager:
        raise ValueError("Helm manager template would append a tag to a digest reference")


def verify(kind: str, tag: str) -> None:
    expected = expected_versions(kind, tag)
    actual = chart_metadata(ROOT / "charts/chart/Chart.yaml")
    if actual != expected:
        raise ValueError(
            "Helm metadata does not match the release target: "
            f"got version={actual.chart_version!r}, appVersion={actual.image_tag!r}; "
            f"want version={expected.chart_version!r}, appVersion={expected.image_tag!r}"
        )
    verify_workflow_contracts()
    verify_helm_image_contracts()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--kind", required=True, choices=("candidate", "stable"))
    parser.add_argument("--tag", required=True)
    args = parser.parse_args()
    try:
        verify(args.kind, args.tag)
    except ValueError as error:
        print(f"release validation: FAIL: {error}", file=sys.stderr)
        return 1
    print("release validation: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
