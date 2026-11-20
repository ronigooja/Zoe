# Repository guidance

## Read selectively

- Start with [the design overview](docs/design/data-flows.md), then read only
  the contracts relevant to the task.
- Use [development guidance](docs/development.md) for verification and
  documentation maintenance, and [operations](docs/operations.md) for
  implementation entry points.

## Implement the approved contracts

- Implement the contracts in `docs/design/`. Do not change a contract to match
  existing code or to make a check pass; report conflicts, missing
  requirements and source-to-contract differences, and get approval before
  changing intended behavior.

## Keep documentation durable

- Record only key behavior, data flows, contracts and how the system works,
  enough for an agent to act without reading the code. Leave out simple
  implementation, presentation detail, task journals, change history, option
  comparisons and verification logs.
- Keep each rule in one authoritative location. When behavior changes, update
  that definition and its links, and remove superseded text.
- Write documentation in English for selective reading.

## Verify honestly

- Select checks by affected behavior and risk. Report the source revision,
  environment, checks, results, and remaining coverage gaps.
- Distinguish source review, fixtures, builds, installation, health, transport
  acknowledgements, rendering, and end-to-end user-visible success.

## Protect operational boundaries

- Refer to secrets by name or deployment reference; never include their values
  or private account data in source, documentation, logs, or fixtures.
