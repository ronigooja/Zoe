# CI verification

When validating a change, select checks by affected behavior and risk. The
[scope selector](../../tools/ci_scope.py) is the executable source of truth;
[scope regressions](../../tools/test_ci_scope.py) protect omission boundaries.

## Automatic checks

The [verification workflow](../../.github/workflows/ci.yml) runs on pull requests
and pushes to `main` and `codex/**`. It compares the complete changed-path set,
including deleted files and both sides of renames. Missing history and unknown
paths select all existing check groups. A new component still needs checks
appropriate to its behavior; selecting existing groups does not create coverage.

| Change | Selected checks |
|---|---|
| Markdown, documentation assets, documentation checker | Documentation checks |
| Files under the macOS app directory | macOS build |
| Files under the iOS app directory | iOS build |
| Other Apple files, including packages, project configuration, and shared resources | Swift package tests and both app builds |
| Go source | Full Go suite and the existing authorization/concurrency race package set |
| Control, voice, memory, or capabilities API packages; shared protocol; integration fixtures; or Go dependencies | Go/race checks plus Swift tests and both app builds |
| Linux deployment shell scripts | Release-script and CLI-installation regression checks |
| CI configuration, scope selector, unknown files, or unavailable diff history | All existing check groups |

Selections are combined for mixed changes. Source comments are not inferred from
diff text: a comment-only source change follows its source component's checks.
The current Go suite remains whole; the race package set also remains unchanged
until dependency evidence supports a safe reduction. Python services and external
Runtime checks retain their existing coverage limitations.

Each selected Apple build, Swift suite, Go suite, race suite, and script suite is
an independent job. Routine Apple checks build apps without producing an archive;
routine Linux checks do not package operator release binaries. Logs are uploaded
separately, with failure excerpts printed to the console.

## Completion gate and manual checks

When configuring branch protection, require **Required verification** from the
verification workflow. It requires successful scope selection and success for
every selected group; an unexpectedly skipped, failed, or cancelled selected
group fails the gate. This repository change does not alter remote branch
protection settings.

When impact cannot be bounded, run **Scoped verification** manually to execute
all existing check groups for the selected ref:

```sh
gh workflow run ci.yml --ref main
```

When unsigned Apple artifacts or Linux operator packages are needed, use the
corresponding workflow's manual `release` option. Each runs its complete relevant
validation before publishing artifacts for the same source revision:

```sh
gh workflow run apple.yml --ref main -f release=true
gh workflow run linux.yml --ref main -f release=true
```

The Apple release defaults to `platform=macos` and packages `ZoeMac-unsigned.dmg` with `SHA256SUMS`; `-f platform=ios` packages only `ZoeMobile-unsigned.app.zip`; `-f platform=all` packages both.

Without `release=true`, those manual workflows perform full platform checks
without release packages. Apple artifacts remain unsigned; artifact creation does
not install or deploy. Server and OpenCode runtime artifact workflows remain separate; see
[deployment operations](../operations/deployment.md).

When reporting verification, link the immutable workflow run and state the source
revision and selected checks; use the [evidence register](../acceptance/evidence.md#evidence-categories-and-acceptance-boundary)
for the authoritative evidence categories and acceptance boundary.

## Release and deployment-candidate verification

Pull requests and ordinary pushes validate the affected scope; they do not publish
release artifacts. A release tag on `main` is the only formal artifact entry point.
After required verification succeeds, the release workflow records one manifest for
the server, runner, operator CLI, memory worker, OpenCode runtime, and unsigned Apple artifacts.
The manifest includes the source revision, target architecture, image digests, and
SHA-256 values. A failed job must not be treated as a complete release.

Before transfer, run the read-only candidate check from the repository root:

```sh
deploy/linux/deployment-candidate.sh path/to/release-manifest.json \
  --compose deploy/linux/docker-compose.yml \
  --env-file /path/to/reviewed/zoe-stack.env \
  --output deployment-plan.json
```

The check validates the manifest schema and every artifact hash, asks Docker
Compose to render the supplied configuration, and emits the installation order
and immutable rollback target. It does not connect to a host, switch a release,
or start a service. Keep its output with the release evidence and record the
source revision, cache status, artifact sizes, and elapsed time.
