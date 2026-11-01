# ADR 0005: Separate voice turns, durable tasks, and lease termination

Status: accepted architecture baseline; delivery status and evidence remain in [the master plan](../../master-plan.md) and [the evidence register](../../acceptance/evidence.md).

## Context

A real-time voice session can be interrupted, retransmitted, or moved to a different upstream account while accepted work continues. Upstream speech does not prove a business command was accepted or a task completed. An expired lease does not prove microphone, media, account, or runtime resources have terminated.

The selected gateway handles account selection (choosing and persisting a sticky pool account for each voice session; the account pool works with the account-media adapter left at its default `false`) and SDP signaling, while the native client connects its WebRTC media and DataChannel directly to the upstream service. That client-direct topology is the root cause of the separate account-media lease contract: a gateway session deletion cannot prove that the client or upstream PeerConnection has ended. The contract protects account reuse; it does not make the gateway a media owner.

## Decision

For real-time voice, use Zoe-compatible `ronigooja/chatgpt-web-voice` fork as the selected ChatGPT web gateway/account pool and native WebRTC media with platform audio/lifecycle adapters. The current baseline accepts client-direct media and keeps Zoe account-media integration disabled. After connection and identity initialization, the client attempts to establish and maintain the session automatically only while OS microphone permission, audio policy, lifecycle/background limits, network, account-pool capacity, and server policy permit it. Permission/policy failures are not reconnectable transport failures and must be reported truthfully with Settings/system-settings recovery; text fallback remains available. Retain no raw audio by default.

Bridge user turns through stable identity, revisions, and deduplication to the same durable authorized command path as typed input. Partial captions and upstream spoken output do not authorize work. Authorization follows the impact-level policy in [ADR 0009](0009-voice-steward-impact-policy.md): high-impact actions require scoped explicit conversational confirmation with task/run/action identity, expected version, expiry, and audit metadata. Ambiguous language must not execute an action. Formal completion notices come only from the task ledger; voice transport is never a business-work completion or approval signal.

Bind device, account, and media resources to holder/generation leases and termination evidence when account-level reuse is enabled. A new account creates a new media session while keeping task associations. End the old client media session before account reuse. Quarantine uncertain sessions/accounts; expiry alone does not release them. Keep mute, stop playback, End voice, and Stop task distinct.

## Consequences

Task life survives voice loss and account changes without duplicate dispatch. Conversation may continue naturally during work. With client-direct media, safe account reuse depends on trusted account-scoped identity and termination evidence, which the selected gateway response has not established. The optional adapter remains disabled and fail-closed; `AccountMedia=true` is not a supported current deployment. A future gateway-terminated or relayed media architecture would require a separate decision and acceptance. Stable local fixtures do not establish real pool behavior, background/locked-device audio, interruptions, latency, or end-to-end voice acceptance. Cross-device continuation promises durable context, not live-audio migration.

## Sources

[Product chapters 3, 8, and 12](../../product.md); [voice architecture](../../architecture.md#voice-and-business-work); [selected upstream](https://github.com/ronigooja/chatgpt-web-voice); historical evidence: [voice/Super boundary](https://github.com/ronigooja/Zoe/blob/95ee55a5bbd8dccfb7f14af9f9703bb83727da6c/docs/archive/research/reports/12-voice-super-boundary.md).
