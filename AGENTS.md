# Repository guidance

## Read selectively

- Start with [the design overview](docs/design/data-flows.md), then read only
  the contracts relevant to the task.
- Use [development guidance](docs/development.md) for verification and
  documentation maintenance, and [operations](docs/operations.md) for
  implementation entry points.

## Preserve the approved design

- Implement the approved contracts; do not change them merely to match existing
  code or make a check pass.
- Report conflicts, missing requirements, and source-to-contract differences.
  Obtain approval before changing intended product behavior.
- Keep each rule in one authoritative location. Update its definition and
  affected links when behavior changes; remove superseded normative text.
- Write repository documentation in English for selective reading. Do not add
  task journals, duplicated specifications, or historical verification logs.

## Verify honestly

- Select checks by affected behavior and risk. Report the source revision,
  environment, checks, results, and remaining coverage gaps.
- Distinguish source review, fixtures, builds, installation, health, transport
  acknowledgements, rendering, and end-to-end user-visible success.

## Protect operational boundaries

- Refer to secrets by name or deployment reference; never include their values
  or private account data in source, documentation, logs, or fixtures.
- Repository changes do not authorize changes to external services or the
  existing CLIProxyAPI deployment. Obtain explicit authorization before doing so.
