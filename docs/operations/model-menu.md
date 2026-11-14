# Operator model menu

When configuring model endpoints, run `zoe` without arguments. The menu names
models by purpose; Configure and Test are paired:

| Configure / Test | Purpose | Configuration |
| --- | --- | --- |
| 6 / 7 | Worker: runs tasks | Existing `provider configure` / `provider test` |
| 8 / 9 | Register: routes every sentence | `ZOE_REGISTER_*` |
| 10 / 11 | Compact: hourly conversation compaction | `ZOE_COMPACT_*` |
| 12 / 13 | Steward: task routing and replies | `ZOE_STEWARD_*` |
| 14 / 15 | Memory extraction: worker ingestion and explicit write decisions | `ZOE_MEM0_LLM_*` |

When using Register or Compact, each group independently requires `BASE_URL`,
`MODEL`, and `API_KEY_FILE`. Optional suffixes are `REASONING_EFFORT`, `THINKING`,
and `TIMEOUT_MS`. Neither group falls back to another group's configuration.
Missing required variables fail Test with the variable name; its current error
also says the server will not start, which is not the server's startup behavior. Server startup
permits incomplete groups but disables the register; enabling it requires both
complete groups. See [routing configuration](deployment.md#register-routing-configuration).

When configuring these menu-only groups, Enter keeps the current value, hidden
key input rotates only the selected credential, and empty key input retains it.
Default secret paths are `/etc/zoe/secrets/register-api-key` and
`/etc/zoe/secrets/compact-api-key`. Keys use mode `0600` and server ownership
`65532:65532`, matching Steward. Changes preserve unrelated env lines and the
env file's mode. Apply saved settings with `zoe update` to restart/redeploy the
server; Configure does not restart services.

When testing Register or Compact, Zoe sends one real connectivity request through
`internal/modelclient`, using only the selected group's endpoint and key.
The probe uses `/responses` relative to the configured base URL; a failed route
fails the probe rather than issuing a second request. No Register, Compact, or
Steward CLI subcommands or flags are provided.

When using the current menu, triage, call-summary and answer-model Configure/Test
entries are unavailable. Recall uses the register restatement to search
the memory worker and appends retrieved text to the upstream voice model; see
[deployment configuration](deployment.md#register-routing-configuration).

When navigating non-model entries, Voice is 16-19, Access is 20-21, Client policy
is 22-23, and Exit is 0; Overview and System retain entries 1-5.
