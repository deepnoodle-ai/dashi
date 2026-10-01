#!/usr/bin/env python3
"""Unit tests for the module requirement sync."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import sys
import unittest


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

    def test_root_is_not_a_sub_module(self) -> None:
        self.assertNotIn(".", sync.SUB_MODULES)
        self.assertNotIn("", sync.SUB_MODULES)

    def test_rejects_a_malformed_version(self) -> None:
        with self.assertRaises(SystemExit):
            sync.main(["0.1.0", "--check"])


if __name__ == "__main__":
    unittest.main()
