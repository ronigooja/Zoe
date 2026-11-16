# Full event recovery contract

When initializing a client without a consumed global cursor, or handling SSE
`snapshot_required`, use authenticated `GET /v1/recovery` (`client:read`). Ignore
client workspace/owner selectors. This read-only endpoint performs no upstream
HTTP calls; generation validation and all fields share one SQLite read transaction.

The exact top-level keys are `cursor` (decimal string), `snapshot` (the existing
`contracts.Snapshot`, including authority, projects, tasks, approvals, runs and
execution capacity), `ui` (the existing owner-scoped UI snapshot), and `voice`:

```typescript
type RecoveryVoice = {
  current_session: { id: string; generation: number; state: string } | null;
  pool: { blocked: boolean; reason_code: string };
};
```

`current_session` is the owner's active session if present, otherwise their newest
session. No other owner's session, lease, account or device identity is returned.
`cursor`, `snapshot.event_cursor`, and `ui.cursor` describe the same checkpoint.
Responses are `Cache-Control: no-store`; there is no database schema migration.

When changing connection generation, cancel the generation's ACK task. Journal
replay checks caller validity before/after receipt lookup and before submission;
the coordinator retains its original API client and workspace. Cancellation cannot
retract a transport request already sent or accepted. Such uncertainty retains
the original idempotency/receipt identity; there is no atomic network no-send claim.

When applying recovery, retain authoritative control state, reconcile voice by
exact session ID/generation/active state, reconcile UI versions and tombstones,
and separately recover pending command acknowledgements. ACK lookup/delivery
failure never gates authoritative state or cursor adoption. Stage control, pool,
and UI tracking until rendering and local voice shutdown finish, then commit them
and the checkpoint synchronously under the current connection generation.
When rendering fails, retain the old authoritative model/cursor and retry with
unavailable status. Track successful physical renders separately so retries do not
repeat them; failed revisions remain retryable. Native display/audio effects are
not physically atomic or rolled back: partial display may remain while unavailable,
and obsolete audio is reported stopped rather than revived. Normal UI-only repair never advances
the global cursor. This is source behavior, not deployment or acceptance evidence.
