import pathlib
import unittest


class ServerReleaseCLIWorkflowTest(unittest.TestCase):
    def test_server_release_includes_cli_in_manifest_checksums(self):
        workflow = pathlib.Path(__file__).parents[1] / ".github/workflows/server-release.yml"
        text = workflow.read_text()
        self.assertIn('go build -trimpath -buildvcs=false -ldflags="-s -w -X main.sourceRevision=$GITHUB_SHA"', text)
        self.assertIn('sha256sum bin/zoe zoe-server', text)
        self.assertIn('"path": p.relative_to(root).as_posix()', text)


if __name__ == "__main__":
    unittest.main()
