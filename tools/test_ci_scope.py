"""Regression checks for CI omission boundaries, run on the hosted runner."""
import unittest
import os
import subprocess
import sys
import tempfile
from pathlib import Path
from ci_scope import KEYS, changed_paths, select


class ScopeTests(unittest.TestCase):
    def selected(self, paths):
        return {key for key, value in select(paths).items() if value}

    def test_docs_do_not_build(self):
        self.assertEqual(self.selected(["README.md", "docs/design/reference.png"]), {"docs"})

    def test_documentation_checker_paths(self):
        for path in ("tools/check_docs.py", "tools/test_check_docs.py", "docs/design/flows/new.md"):
            with self.subTest(path=path):
                self.assertEqual(self.selected([path]), {"docs"})
        self.assertEqual(self.selected(["tools/docs_requirements.json"]), set(KEYS))

    def test_platform_specific_changes(self):
        self.assertEqual(self.selected(["clients/apple/Apps/macOS/ZoeMacApp.swift"]), {"macos"})
        self.assertEqual(self.selected(["clients/apple/Apps/iOS/ZoeMobileApp.swift"]), {"ios"})

    def test_shared_apple_changes(self):
        for path in ("clients/apple/Packages/ZoeKit/Package.swift", "clients/apple/Zoe.xcodeproj/project.pbxproj", "clients/apple/Apps/Resources/en.lproj/InfoPlist.strings"):
            with self.subTest(path=path):
                self.assertEqual(self.selected([path]), {"tests", "macos", "ios"})

    def test_go_and_shared_contract(self):
        self.assertEqual(self.selected(["internal/runner/engine.go"]), {"go", "race"})
        for path in ("contracts/http.go", "internal/control/views.go", "internal/voice/schema.go", "internal/memory/repository.go", "internal/capabilities/service.go"):
            self.assertEqual(self.selected([path]), {"go", "race", "tests", "macos", "ios"})
        self.assertEqual(self.selected(["go.mod"]), {"go", "race", "operator", "tests", "macos", "ios"})
        self.assertEqual(self.selected(["tests/integration/example_test.go"]), {"go", "tests", "macos", "ios"})

    def test_race_only_for_its_dependency_closure(self):
        for path in ("cmd/zoe-runner/main.go", "cmd/zoe-maintenance/main.go", "tests/ops/ops_test.go"):
            self.assertNotIn("race", self.selected([path]), path)
        for path in ("cmd/zoe/main.go", "cmd/zoe-server/main.go", "internal/storage/store.go", "internal/ops/lifecycle.go"):
            self.assertIn("race", self.selected([path]), path)

    def test_operator_package_follows_cli_changes(self):
        self.assertEqual(self.selected(["cmd/zoe/main.go"]), {"go", "race", "operator"})
        self.assertEqual(self.selected(["internal/ops/lifecycle.go"]), {"go", "race", "operator"})
        self.assertEqual(self.selected(["deploy/linux/install-cli.sh"]), {"scripts", "operator"})
        self.assertNotIn("operator", self.selected(["internal/runner/engine.go"]))

    def test_scripts_and_mixed_changes(self):
        self.assertEqual(self.selected(["deploy/linux/other.sh"]), {"scripts"})
        self.assertEqual(self.selected(["README.md", "internal/runner/engine.go"]), {"docs", "go", "race"})

    def test_workflow_changes_run_only_what_they_define(self):
        self.assertEqual(self.selected([".github/workflows/apple.yml"]), {"tests", "macos", "ios"})
        self.assertEqual(self.selected([".github/workflows/linux.yml"]), {"go", "race", "scripts", "operator"})
        self.assertEqual(self.selected([".github/workflows/docs.yml"]), {"docs"})
        for path in (".github/workflows/opencode-image.yml", ".github/workflows/server-release.yml"):
            self.assertEqual(self.selected([path]), set())

    def test_gitignore_needs_no_checks(self):
        self.assertEqual(self.selected([".gitignore"]), set())
        self.assertEqual(self.selected([".gitignore", "README.md"]), {"docs"})

    def test_unknown_and_ci_changes_expand(self):
        for path in ("new-component/file", "services/memory/worker.py", ".github/workflows/ci.yml", ".github/dependabot.yml", "tools/ci_scope.py"):
            self.assertEqual(self.selected([path]), set(KEYS))

    def test_full_and_empty(self):
        self.assertTrue(all(select([], full=True).values()))
        self.assertFalse(any(select([]).values()))

    def test_git_deletion_and_rename_keep_old_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            old_cwd = os.getcwd()
            try:
                os.chdir(directory)
                def git(*args):
                    return subprocess.check_output(["git", *args], stderr=subprocess.STDOUT)
                git("init", "-q")
                git("config", "user.name", "CI fixture")
                git("config", "user.email", "ci@example.invalid")
                Path("shared.go").write_text("package shared\n")
                Path("deleted.go").write_text("package deleted\n")
                git("add", ".")
                git("commit", "-qm", "base")
                base = git("rev-parse", "HEAD").decode().strip()
                Path("shared.go").rename("renamed.md")
                Path("deleted.go").unlink()
                git("add", "-A")
                git("commit", "-qm", "rename and delete")
                paths = changed_paths(base, "HEAD")
                self.assertEqual(set(paths), {"shared.go", "renamed.md", "deleted.go"})
                self.assertEqual(self.selected(paths), {"go", "docs"})
            finally:
                os.chdir(old_cwd)

    def test_first_push_of_a_branch_compares_with_main(self):
        with tempfile.TemporaryDirectory() as directory:
            old_cwd = os.getcwd()
            try:
                os.chdir(directory)
                def git(*args):
                    return subprocess.check_output(["git", *args], stderr=subprocess.STDOUT)
                git("init", "-q", "-b", "main")
                git("config", "user.name", "CI fixture")
                git("config", "user.email", "ci@example.invalid")
                Path("README.md").write_text("base\n")
                git("add", ".")
                git("commit", "-qm", "base")
                git("checkout", "-qb", "feature")
                Path("docs").mkdir()
                Path("docs/a.md").write_text("doc\n")
                git("add", ".")
                git("commit", "-qm", "docs only")
                output = Path(directory) / "scope-out"
                subprocess.run([sys.executable, str(Path(__file__).with_name("ci_scope.py")),
                                "--base", "0" * 40, "--head", "HEAD", "--output", str(output)],
                               check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
                lines = set(output.read_text().splitlines())
                self.assertIn("docs=true", lines)
                self.assertIn("go=false", lines)
                self.assertIn("macos=false", lines)
            finally:
                os.chdir(old_cwd)

    def test_missing_history_selects_everything(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "scope"
            subprocess.run([sys.executable, str(Path(__file__).with_name("ci_scope.py")),
                            "--base", "missing-history-ref", "--output", str(output)],
                           check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            self.assertEqual(set(output.read_text().splitlines()), {f"{key}=true" for key in KEYS})


if __name__ == "__main__":
    unittest.main()
