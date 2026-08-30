#!/usr/bin/env python3
"""Regression tests for release version and chart metadata validation."""

from __future__ import annotations

import pathlib
import tempfile
import unittest

from hack.verify_release import (
    ROOT,
    ReleaseVersions,
    chart_metadata,
    expected_versions,
    verify_helm_image_contracts,
    verify_safe_candidate_inputs,
    verify_workflow_contracts,
)


class ReleaseVersionTests(unittest.TestCase):
    def test_candidate_keeps_v_prefix_for_image(self) -> None:
        self.assertEqual(
            expected_versions("candidate", "v0.2.0-rc.1"),
            ReleaseVersions("0.2.0-rc.1", "v0.2.0-rc.1"),
        )

    def test_stable_uses_semver_image_tag_without_v_prefix(self) -> None:
        self.assertEqual(
            expected_versions("stable", "v0.2.0"),
            ReleaseVersions("0.2.0", "0.2.0"),
        )

    def test_candidate_rejects_stable_tag(self) -> None:
        with self.assertRaises(ValueError):
            expected_versions("candidate", "v0.2.0")

    def test_stable_rejects_prerelease_tag(self) -> None:
        with self.assertRaises(ValueError):
            expected_versions("stable", "v0.2.0-rc.1")

    def test_unknown_release_kind_is_rejected(self) -> None:
        with self.assertRaises(ValueError):
            expected_versions("nightly", "v0.2.0")

    def test_semver_numbers_reject_leading_zeroes(self) -> None:
        for kind, tag in (
            ("candidate", "v0.02.0-rc.1"),
            ("candidate", "v0.2.0-rc.01"),
            ("stable", "v00.2.0"),
        ):
            with self.subTest(kind=kind, tag=tag), self.assertRaises(ValueError):
                expected_versions(kind, tag)

    def test_chart_metadata_preserves_prerelease(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "Chart.yaml"
            path.write_text('version: 0.2.0-rc.1\nappVersion: "v0.2.0-rc.1"\n')
            self.assertEqual(
                chart_metadata(path),
                ReleaseVersions("0.2.0-rc.1", "v0.2.0-rc.1"),
            )

    def test_duplicate_chart_metadata_is_rejected(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "Chart.yaml"
            path.write_text(
                'version: 0.2.0-rc.1\nversion: 0.2.0\nappVersion: "v0.2.0-rc.1"\n'
            )
            with self.assertRaises(ValueError):
                chart_metadata(path)


class WorkflowContractTests(unittest.TestCase):
    def test_repository_workflows_satisfy_release_contracts(self) -> None:
        verify_workflow_contracts()

    def test_candidate_shell_interpolation_is_rejected(self) -> None:
        path = ROOT / ".github/workflows/candidate.yml"
        unsafe = path.read_text() + '\n      run: echo "${{ inputs.image_tag }}"\n'
        with self.assertRaises(ValueError):
            verify_safe_candidate_inputs(unsafe, path)

    def test_helm_digest_contracts_are_present(self) -> None:
        verify_helm_image_contracts()


if __name__ == "__main__":
    unittest.main()
