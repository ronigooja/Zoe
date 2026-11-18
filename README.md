# Zoe

Zoe is a private, self-hosted AI first mate, named after Zoë Washburne from
*Firefly*. It carries authorized work forward beyond a conversation through a
Linux backend, an independent runner, and native Apple clients.

## Start here

- [Design overview](docs/design/data-flows.md): system boundaries and task-based
  navigation to the authoritative component contracts.
- [Development](docs/development.md): implementation entry points, verification,
  and documentation maintenance.
- [Operations](docs/operations.md): installation, configuration, diagnostics,
  backup, restore, and deletion.

Design approval is not an implementation, deployment, or acceptance claim.
Module-specific setup belongs in the [Apple client](clients/apple/README.md),
[memory worker](services/memory/README.md), and
[voice deployment](deploy/voice/README.md) guides.
