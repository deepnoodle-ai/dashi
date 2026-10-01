#!/usr/bin/env python3
"""Keep intra-repo module requirements in sync with the version being released.

Every sub-module builds locally through the repository's ``go.work``, so a stale
``github.com/deepnoodle-ai/dashi vX`` requirement is invisible to CI. Consumers
do not see the workspace: they resolve the required version, so a sub-module
tagged while requiring an older root release fails to build for everyone
downstream.

``--check`` is run by ``make tag-modules`` to make that mismatch a hard stop, and
``--write`` (``make release-prep``) updates the requirements before tagging.
``--list`` prints the tagged sub-modules, which is where the Makefile gets them.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
import subprocess
import sys
from typing import Sequence


REPO_ROOT = Path(__file__).resolve().parents[1]
MODULE_PREFIX = "github.com/deepnoodle-ai/dashi"
VERSION_RE = re.compile(r"^v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$")

# Tagged sub-modules, each as "<dir>/vX.Y.Z". The root module is tagged on its
# own as "vX.Y.Z". Their requirements are what downstream consumers resolve, so
# a stale entry here is a broken release.
SUB_MODULES = (
    "mcp",
    "providers/google/gemini",
    "cmd/dashi",
)


def tracked_modules() -> list[str]:
    """Directories of every go.mod tracked by git, other than the root."""
    process = subprocess.run(
        ["git", "ls-files", "*go.mod"],
        cwd=REPO_ROOT,
        text=True,
        capture_output=True,
        check=True,
    )
    return sorted(
        str(Path(path).parent)
        for path in process.stdout.splitlines()
        if Path(path).name == "go.mod" and path != "go.mod"
    )


def go_mod_json(directory: Path) -> dict:
    process = subprocess.run(
        ["go", "mod", "edit", "-json"],
        cwd=directory,
        text=True,
        capture_output=True,
        check=False,
    )
    if process.returncode != 0:
        raise SystemExit(f"go mod edit -json failed in {directory}: {process.stderr.strip()}")
    return json.loads(process.stdout)


def dashi_requirements(directory: Path) -> list[tuple[str, str]]:
    requires = go_mod_json(directory).get("Require") or []
    return [
        (require["Path"], require["Version"])
        for require in requires
        if require["Path"] == MODULE_PREFIX
        or require["Path"].startswith(f"{MODULE_PREFIX}/")
    ]


def check(version: str) -> int:
    stale: list[str] = []
    for module in SUB_MODULES:
        for path, current in dashi_requirements(REPO_ROOT / module):
            if current != version:
                stale.append(f"  {module}: requires {path} {current}, expected {version}")
    if stale:
        print(
            "Intra-repo module requirements do not match the release version:",
            file=sys.stderr,
        )
        print("\n".join(stale), file=sys.stderr)
        print(
            f"\nRun: make release-prep VERSION={version}\n"
            "then commit the go.mod updates before tagging.",
            file=sys.stderr,
        )
        return 1
    return 0


def write(version: str) -> int:
    for module in SUB_MODULES:
        directory = REPO_ROOT / module
        for path, current in dashi_requirements(directory):
            if current == version:
                continue
            subprocess.run(
                ["go", "mod", "edit", f"-require={path}@{version}"],
                cwd=directory,
                check=True,
            )
            print(f"{module}: {path} {current} -> {version}")
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("version", nargs="?", help="release version, for example v0.2.0")
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument(
        "--check", action="store_true", help="fail if any requirement is stale"
    )
    mode.add_argument(
        "--write", action="store_true", help="rewrite requirements to the version"
    )
    mode.add_argument(
        "--list", action="store_true", help="print the tagged sub-modules"
    )
    args = parser.parse_args(argv)
    if args.list:
        print(" ".join(SUB_MODULES))
        return 0
    if not args.version or not VERSION_RE.fullmatch(args.version):
        raise SystemExit(f"invalid version {args.version!r}; expected a vX.Y.Z tag")
    return check(args.version) if args.check else write(args.version)


if __name__ == "__main__":
    raise SystemExit(main())
