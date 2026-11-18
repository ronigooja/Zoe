---
name: xcode-mcp-ssh
description: Connect to Apple's Xcode MCP over SSH from Linux or another host, inspect projects and devices, and run authorized Apple app builds or real-device tests.
---

# Xcode MCP over SSH

Use Apple's existing `xcrun mcpbridge` through `ssh -T`. The MCP process runs on the Mac; no HTTP bridge, listening port, or Mac server installation is needed. Use client name **Codex**, including for probes, so permission dialogs have a recognizable name.

Keep connection details in operator-provided environment variables or SSH configuration. Do not store keys, passwords, account identifiers, device serial numbers, or private logs in the repository. Preserve the user's restrictions on system changes. Connecting does not authorize changing macOS settings, signing accounts, Keychain, external services, or deleting app data. Project opening, source edits, builds, installation, and app termination must remain within the requested task.

## Connect and discover

1. Obtain the SSH destination and intended project path from the user or existing session. Use existing SSH authentication; never request a password or private key in chat. Avoid broad home-directory searches when a project path is available.
2. Check prerequisites, without changing them:

   ```sh
   ssh -T -o BatchMode=yes -o ConnectTimeout=8 "$XCODE_SSH_HOST" \
     'xcodebuild -version; xcrun --find mcpbridge; xcrun mcp-server status'
   ```

   If a command is unavailable, inspect the installed version's help. Do not install or upgrade Xcode or change the selected developer directory automatically.
3. Use native Xcode MCP tools when already available. Otherwise use the bundled Python 3 helper, which initializes MCP over SSH and preserves one bridge for the entire batch:

   ```sh
   python3 .agents/skills/xcode-mcp-ssh/scripts/mcp_ssh.py --list-tools
   python3 .agents/skills/xcode-mcp-ssh/scripts/mcp_ssh.py --schema RunProject
   printf '%s\n' '[["XcodeListWorkspaces", {}]]' |
     python3 .agents/skills/xcode-mcp-ssh/scripts/mcp_ssh.py
   ```

   `XCODE_SSH_HOST` is required; `--host` overrides it. The helper makes no persistent configuration changes. Tools differ by Xcode version: discover schemas before unfamiliar calls. Filter console queries at the source; tool output can contain private application data.
4. If project access is denied, open the intended project with `XcodeOpenWorkspace` and let the user handle Apple's permission dialog. General MCP enablement and per-agent/project authorization are separate. Do not claim tool access merely because initialization or `tools/list` succeeds.

## Open and run a project

See [operations.md](references/operations.md) for the single-session run example, optional persistent Codex registration, real-device troubleshooting, and evidence boundaries. Read it before device installation or diagnosing startup failures.

Always discover the current workspace, scheme, and eligible destination. Workspace identifiers are opaque and temporary. In the observed background-server workflow, new connections could revert to `ZoeMac`/`My Mac`. Select the intended scheme and physical destination in the **same bridge session** as `RunProject`; never assume the previous connection's selection persists. Prefer `attachDebugger=false` for manual testing unless debugging is needed.

## Completion

Separate MCP connectivity, project authorization, successful build, confirmed installation, launch, continued process existence, visible UI, and functional acceptance. Report only the stages actually checked. Follow the [design overview](../../../docs/design/data-flows.md) for repository contracts and [development guidance](../../../docs/development.md#evidence-and-acceptance) for verification reporting; retain run-specific evidence in the PR or linked CI run, not a repository journal. This skill is an operator workflow, not a Zoe runtime or a macOS control server.
