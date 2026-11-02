#!/usr/bin/env python3
"""Conservative CI selection; unknown paths require all existing checks."""
import argparse
import subprocess
from pathlib import Path

KEYS = ("tests", "macos", "ios", "go", "race", "scripts", "docs", "operator")

# Packages in the dependency closure of the race-tested packages in linux.yml.
RACE_PREFIXES = (
    "cmd/zoe/", "cmd/zoe-server/", "contracts/", "internal/agents/opencode/",
    "internal/capabilities/", "internal/control/", "internal/memory/", "internal/ops/", "internal/profiles/",
    "internal/runner/", "internal/storage/", "internal/voice/",
)
OPERATOR_PREFIXES = ("cmd/zoe/", "internal/ops/", "deploy/linux/install-cli.sh")
WORKFLOW_SCOPES = {
    ".github/workflows/apple.yml": ("tests", "macos", "ios"),
    ".github/workflows/linux.yml": ("go", "race", "scripts", "operator"),
    ".github/workflows/docs.yml": ("docs",),
    ".github/workflows/opencode-image.yml": (),
    ".github/workflows/server-release.yml": (),
}


def select(paths, full=False):
    scope = dict.fromkeys(KEYS, full)
    for path in paths:
        if full:
            break
        if path == ".gitignore":
            continue
        if path in WORKFLOW_SCOPES:
            for key in WORKFLOW_SCOPES[path]:
                scope[key] = True
            continue
        if path.startswith(".github/") or path in ("tools/ci_scope.py", "tools/test_ci_scope.py"):
            return dict.fromkeys(KEYS, True)
        if path.endswith(".md") or path.startswith("docs/") or path in (
            "tools/check_docs.py", "tools/test_check_docs.py", "tools/docs_requirements.json",
        ):
            scope["docs"] = True
        elif path.startswith("clients/apple/Apps/macOS/"):
            scope["macos"] = True
        elif path.startswith("clients/apple/Apps/iOS/"):
            scope["ios"] = True
        elif path.startswith("clients/apple/"):
            for key in ("tests", "macos", "ios"):
                scope[key] = True
        elif path.endswith(".go") or path in ("go.mod", "go.sum"):
            scope["go"] = True
            if path.startswith(RACE_PREFIXES) or path in ("go.mod", "go.sum"):
                scope["race"] = True
            if path.startswith(OPERATOR_PREFIXES) or path in ("go.mod", "go.sum"):
                scope["operator"] = True
            # The API and wire models have native consumers. Dependency changes
            # and integration fixtures may alter that shared contract as well.
            if path.startswith(("contracts/", "internal/control/", "internal/voice/", "internal/memory/", "internal/capabilities/", "tests/")) or path in ("go.mod", "go.sum"):
                for key in ("tests", "macos", "ios"):
                    scope[key] = True
        elif path.startswith("deploy/linux/") and path.endswith(".sh"):
            scope["scripts"] = True
            if path.startswith(OPERATOR_PREFIXES):
                scope["operator"] = True
        else:
            return dict.fromkeys(KEYS, True)
    return scope


def changed_paths(base, head):
    # --no-renames includes both names, so moving shared code cannot narrow CI.
    raw = subprocess.check_output([
        "git", "diff", "--no-renames", "--name-only", "-z", base, head,
    ])
    return [p.decode("utf-8", "surrogateescape") for p in raw.split(b"\0") if p]


def branch_point(head):
    for ref in ("origin/main", "main"):
        try:
            return subprocess.check_output(["git", "merge-base", ref, head], stderr=subprocess.DEVNULL).decode().strip()
        except subprocess.CalledProcessError:
            continue
    return ""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="")
    parser.add_argument("--head", default="HEAD")
    parser.add_argument("--full", action="store_true")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    try:
        base = args.base
        if base and set(base) == {"0"}:
            # First push of a branch has no previous commit; compare with main.
            base = branch_point(args.head)
        if args.full or not base:
            scope = select([], full=True)
        else:
            scope = select(changed_paths(base, args.head))
    except subprocess.CalledProcessError:
        # Missing/rewritten history must never silently skip verification.
        scope = select([], full=True)
    lines = [f"{key}={str(value).lower()}" for key, value in scope.items()]
    with Path(args.output).open("a") as output:
        output.write("\n".join(lines) + "\n")
    print("CI selection: " + ", ".join(key for key, value in scope.items() if value))


if __name__ == "__main__":
    main()
