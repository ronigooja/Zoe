# Data lifecycle operations

This runbook applies when exporting, backing up, restoring, migrating, or deleting a Zoe instance. [Architecture](../architecture.md#backup-deletion-and-recovery) defines authority and fencing, and [acceptance evidence](../acceptance/evidence.md) records what has actually been checked; this document is operational guidance, not an acceptance or delivery claim.

## Logical export

Use a new destination and a private key file produced by `zoe key init` or `zoe key rotate`:

```sh
python3 tools/zoe_export.py --server URL --key-file PRIVATE_JSON --output NEW_ZIP --scope workspace
```

Use `--scope personal` for the authorized personal scope or `--key-format plain` for a private raw-key file. The [export tool](../../tools/zoe_export.py) requires an operator-owned regular credential file without group/other access, refuses redirects/overwrites, creates a mode-0600 ZIP, caps transfer at 1 GiB, and removes partial output on failure. Schema-2 contents include authority records, hashes, inventory, and explicit omissions. Runtime volumes, secrets, and backups are omitted. This command does not perform the native client's ZIP/hash validation. Export is not a deployable backup.

## Offline encrypted backup

Use the [backup inventory example](../../deploy/linux/backup/backup-inventory.example.json) to name service configuration, immutable artifacts, persistent volumes, and secret references. Keep secrets outside the payload. Prepare the Python dependencies from [requirements](../../tools/data_lifecycle/requirements.txt) in a private operator environment. The encryption key is a separate deployment-managed mode-0600 file containing exactly 32 bytes, not the client instance-key JSON.

Stop writers and run the tool under the authority-data account. It requires the server's exclusive database lock and uses SQLite's backup API plus integrity checking; copying an active database file can omit pending WAL frames. Persistent volumes require an executable quiesce adapter proving consistency before they can be included.

```sh
python3 tools/zoe_backup.py --database DATABASE --destination PRIVATE_BACKUP_DIR \
  --key-file BACKUP_KEY --inventory PRIVATE_INVENTORY \
  --backup-delete-manifest CURRENT_PRIVATE_DELETE_MANIFEST \
  --retention-days 30
```

Add `--data-root ID=PATH` for reviewed roots and `--volume-quiesce-adapter ABSOLUTE_ADAPTER` when including persistent volumes. [The source](../../tools/zoe_backup.py) defines the manifest and adapter contract. The encrypted artifact records schema, event water level, checksums, managed paths, omissions, and snapshot-bound deletion metadata. A nonempty deletion journal requires a matching independent durable deletion manifest; the journal cannot synthesize successful cleanup evidence.

The [retention registry](../../tools/data_lifecycle/retention.py) authenticates and locks the artifact inventory and purges expired registered artifacts when backup runs (default 30 days). This is not a continuously enforced wall-clock guarantee. Daily backup scheduling, RPO 24 hours, and RTO two hours remain unmeasured targets. Separately protect the current deletion ledger, head attestation, keys, configuration references, and required service data.

## Voice transcript authority and ingestion

When voice transcript ingestion is enabled, `voice_transcript_turns` stores both roles verbatim under the local Zoe session ID, ordered by gateway-assigned `seq`; `voice_call_summaries` is the additive summary schema (not yet populated). Both tables are retained forever, without TTL or routine deletion; complete instance destruction removes their containing database and registered backups. Logical export includes owner-visible sessions' transcripts and summaries in either scope. SQLite backup, migration, and staged restore preserve both tables without a table allowlist. The future Qdrant summary index is derived, rebuildable data; no indexing is implemented here.

The service-only endpoint is `POST /runner/v1/voice/turns` with `Authorization: Bearer TOKEN` and body `{"gateway_session_id":"opaque-gateway-id","media_generation":1,"turns":[{"seq":1,"role":"user","text":"verbatim text","at":"2026-11-06T10:00:00Z"}]}`. Workspace and local session identity come from the stored gateway binding, never the request. Wrong generation returns 409; ended calls accept completion through ten minutes after `ended_at`. A batch contains 1–200 turns, each text at most 64 KiB, and the entire encoded body at most 512 KiB (413 otherwise). Matching session/seq/text retries are no-ops; conflicting text returns 409 and rolls back the entire batch. Timestamps are normalized to UTC RFC3339Nano. `voice.ReadTranscriptTurns` reads an inclusive sequence range in order for a trusted workspace.

To provision, create a deployment-owned private file `/etc/zoe/secrets/voice-ingest-token` containing an independently generated 32-byte unpadded base64url bearer; do not reuse owner, runner, memory-worker, or gateway API credentials. Set host `ZOE_VOICE_INGEST_TOKEN_FILE` to that file in `/etc/zoe/zoe-stack.env`. Compose mounts secret `voice_ingest_token` at `/run/secrets/voice_ingest_token` and sets the server's `ZOE_VOICE_INGEST_TOKEN_FILE` to that container path; its default host source is `/dev/null`. Absent configuration or an empty file disables route registration. `ZOE_VOICE_INGEST_ID` defaults to `voice-gateway`; `ZOE_VOICE_INGEST_WORKSPACES` defaults to `default` and must be an exact, unique workspace allowlist. Scope is fixed in code to `voice:turn:ingest`; no scope override exists. Only a digest is stored in `runner_credentials`; startup checks the exact identity/allowlist/scope and preserves revocation. This credential is denied other authenticated endpoints. Provisioning and distributing it to the gateway is an operator action, not performed by this change.

## Migration and staged restore

1. Resolve outstanding runs, external effects, deployments, and voice sessions under their normal authority paths. Stop/mask every source unit capable of writing or dispatching. Preserve the old instance fence and independently retained current deletion generation/head/hash; an old backup's embedded head is not a freshness authority.
2. Generate source-isolation proof with the [migration tool](../../tools/zoe_migrate.py). It requires readiness with zero unresolved runs/effects/deployments/voice sessions and every named unit inactive and masked. The optional `--stop-mask-and-fence` performs those local actions before checking; it does not administer remote hosts.

```sh
python3 tools/zoe_migrate.py --maintenance ABSOLUTE_MAINTENANCE_BINARY \
  --database DATABASE --proof-key-file PRIVATE_PROOF_KEY --proof-output NEW_PROOF \
  --unit zoe-compose.service --unit zoe-runner.service \
  --stop-mask-and-fence
```

Enumerate any additional deployment writers; the example unit list does not inventory every installation. Proof keys are operator-owned mode-0600 32-byte files. A refusal leaves the source closed; investigate it rather than modifying records to manufacture readiness.

3. While holding the source offline, record its current deletion head using the reviewed [maintenance CLI](../../cmd/zoe-maintenance/main.go), and retain the current `.deletion-ledger` independently of the chosen backup. The attestation must come from the current authority, not the selected snapshot.

```sh
zoe-maintenance deletion-head --database DATABASE --output NEW_CURRENT_HEAD
python3 tools/zoe_restore.py --artifact ENCRYPTED_BACKUP --key-file BACKUP_KEY \
  --latest-deletion-ledger CURRENT_DELETION_LEDGER \
  --latest-deletion-head-file NEW_CURRENT_HEAD --restore-directory NEW_STAGE
```

4. Transfer the encrypted backup, independent ledger/head, isolation proof, and required references through the deployment-approved private channel. [Restore](../../tools/zoe_restore.py) verifies hashes, deletion continuity, and safe paths, then creates a new stage with `data/zoe.sqlite.restore-pending` and `restore-state.json`. It never overwrites a target or enables dispatch. The database itself carries a persistent restore-only guard; renaming cannot bypass it.
5. Read the restored epoch using offline maintenance status. Install the staged database under a new target identity and reconcile deletions:

```sh
zoe-maintenance prepare-restore --database NEW_STAGE/data/zoe.sqlite.restore-pending \
  --activate-as NEW_DATABASE --expected-epoch RESTORED_SOURCE_EPOCH
zoe-maintenance reconcile-deletions --database NEW_DATABASE --restore-state NEW_STAGE/restore-state.json
```

Reconciliation prepares deletion work; it does not prove external cleanup is complete. The deletion-only maintenance worker and cleanup adapters must produce the required checkpoints before activation or derived reads. Inspect [worker deployment configuration](../../services/memory/config.py) and [restore gates](../../internal/control/restore_gate.go). Unsupported Mem0/Qdrant or external resources remain `adapter_missing`; no operator success flag replaces proof.

6. Only after source isolation, required deletion checkpoints, task/run/service reconciliation, required managed data restoration, and target status checks pass, enable with the reviewed proof and current target epoch:

```sh
zoe-maintenance enable --database NEW_DATABASE --expected-epoch TARGET_EPOCH \
  --proof-file NEW_PROOF --proof-key-file PRIVATE_PROOF_KEY
```

Keep the old instance fenced. Validate permissions, task/run identities, memory versions/tombstones, artifact and volume inventory, index rebuild, and external effects before accepting recovery. Database rollback cannot undo a message or other external business action. Report index rebuild duration separately from core recovery time.

## Deletion and destruction

Follow the authority and deletion semantics in [ADR 0006](../architecture/decisions/0006-memory-authority-and-derived-stores.md) and the [backup/deletion architecture](../architecture.md#backup-deletion-and-recovery): authoritative deletion must block stale retrieval before cleanup, preserve deletion metadata across restore, and keep unsupported cleanup closed. This runbook supplies the operational commands and checks; actual erasure requires target-specific evidence. Conversations, formal memory, and task evidence are retained until user deletion by default, with configurable retention; raw audio retention is off by default.

For complete instance destruction, start with the [inventory example](../../deploy/linux/destroy-inventory.example.json) and inspect a read-only plan:

```sh
python3 tools/zoe_destroy.py --manifest PRIVATE_MANIFEST --receipt NEW_PRIVATE_RECEIPT
```

[Execution](../../tools/zoe_destroy.py) additionally requires `--execute --confirm-instance INSTANCE_ID`. The supported topology is one dedicated operator-owned directory with declared data/artifact/backup/config roots, enumerated systemd units, and exact external resources. The recorded split `/etc/zoe` and `/var/lib/zoe` installation does not automatically match this topology. Private receipt and cleanup executables must survive outside deletion roots. Mounted resources, including bind mounts, must be detached; mount identity is rechecked before actions/deletion.

Execution stops/masks services and fences dispatch, then challenges a deployment-owned adapter to prove complete inventory, no active/unaccounted runtime, and exact external cleanup. Only then may owned roots be removed after repeated identity/path checks. No real OpenCode/container/volume/Mem0/Qdrant destruction adapter is shipped. Missing proof retains owned files, but services may already be stopped/fenced; inspect the receipt and state before recovery. The existing external CLIProxyAPI is excluded. No command here claims an actual production migration or destruction was performed.
