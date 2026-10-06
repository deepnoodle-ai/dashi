#!/usr/bin/env python3
"""Unit tests for the module requirement sync."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name("sync_module_versions.py")
SPEC = importlib.util.spec_from_file_location("sync_module_versions", SCRIPT)
assert SPEC and SPEC.loader
sync = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = sync
SPEC.loader.exec_module(sync)


class ModuleListTest(unittest.TestCase):
    def test_every_tracked_module_is_tagged(self) -> None:
        # A new go.mod that is missing from SUB_MODULES would never be tagged
        # or kept in sync, so adding one must also mean listing it.
        self.assertEqual(sorted(sync.SUB_MODULES), sync.tracked_modules())

    def test_examples_and_devtools_are_not_tracked(self) -> None:
        listed = "go.mod\nmcp/go.mod\nexamples/demo/go.mod\ndevtools/agent-archive/go.mod\n"
        result = subprocess.CompletedProcess(args=[], returncode=0, stdout=listed, stderr="")
        with mock.patch.object(sync.subprocess, "run", return_value=result):
            self.assertEqual(sync.tracked_modules(), ["mcp"])

    def test_root_is_not_a_sub_module(self) -> None:
        self.assertNotIn(".", sync.SUB_MODULES)
        self.assertNotIn("", sync.SUB_MODULES)

    def test_rejects_a_malformed_version(self) -> None:
        with self.assertRaises(SystemExit):
            sync.main(["0.1.0", "--check"])


class WriteTest(unittest.TestCase):
    def test_write_syncs_requirements_and_workspace(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "sub").mkdir()
            (root / "go.mod").write_text(f"module {sync.MODULE_PREFIX}\n\ngo 1.26\n")
            (root / "sub" / "go.mod").write_text(
                f"module {sync.MODULE_PREFIX}/sub\n\ngo 1.26\n\n"
                f"require {sync.MODULE_PREFIX} v0.1.0\n"
            )
            (root / "go.work").write_text(
                "go 1.26\n\nuse (\n\t.\n\t./sub\n)\n\n"
                f"replace {sync.MODULE_PREFIX} v0.1.0 => .\n"
            )
            with mock.patch.object(sync, "REPO_ROOT", root), \
                    mock.patch.object(sync, "SUB_MODULES", ("sub",)):
                self.assertEqual(sync.check("v0.2.0"), 1)
                self.assertEqual(sync.write("v0.2.0"), 0)
                self.assertEqual(sync.check("v0.2.0"), 0)
                self.assertEqual(
                    sync.workspace_replaces(), [(sync.MODULE_PREFIX, "v0.2.0", ".")]
                )
            # The rewritten workspace must still load.
            subprocess.run(["go", "list", "-m", "all"], cwd=root / "sub", check=True,
                           capture_output=True)


if __name__ == "__main__":
    unittest.main()
