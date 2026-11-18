# Operations

Use these runbooks only within the authorized deployment scope. They describe
procedures and source constraints, not proof that a host implements the approved
[data flows](design/data-flows.md) or has passed acceptance.

| Operation | Runbook |
| --- | --- |
| Install/update server and CLI, configure/test models, connect clients | [Deployment](operations/deployment.md) |
| Export, encrypted backup, migration, staged restore, deletion/destruction limits | [Data lifecycle](operations/data-lifecycle.md) |
| Investigate client diagnostic acceptance and task/voice symptoms | [Diagnostics](operations/diagnostics.md) |
| Select log levels and collect bounded private diagnostics | [Logging](operations/logging.md) |
| Deploy the separately authorized gateway | [Voice gateway](../deploy/voice/README.md) |

Keep credentials and private account data in deployment-managed private files;
use only secret names/references in source, tickets and shared output. Do not
dump environment files, credentials, account data, transcripts or raw media.
Backups, exports and DEBUG logs are private content, not safe attachments.

Inspect read-only state before diagnosis or mutation. Unknown command or
external-effect outcomes reconcile their original identities; reconnect is not
authorization to resubmit. Do not clear fences, quarantine, receipts or deletion
records to make a procedure pass. Preserve network, keys and data on upgrades;
reset, backup, restore and deletion are explicit, separately reviewed operations.

The existing external CLIProxyAPI deployment is outside Zoe's lifecycle. Changes
to it or other external services need explicit authorization. Build/test host
policy and the known hosted-Linux workflow conflict are in
[development](development.md#ci-selection-and-execution-policy). Report source,
fixture, build, installation, health, transport, render and end-to-end outcomes
separately under [evidence and acceptance](development.md#evidence-and-acceptance).
