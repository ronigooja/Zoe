#!/usr/bin/env bash
# Re-sign the unsigned Zoe macOS app with a code-signing identity from this Mac's Keychain.
# Run it from the mounted disk image (double-click) or: Sign-Zoe.command [path/to/ZoeMac.app] [--no-install]
# The signing private key never leaves this Mac. Use a stable identity across builds.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
app=""
install=1
for arg in "$@"; do
  case "$arg" in
    --no-install) install=0 ;;
    -*) echo "unknown option: $arg" >&2; exit 2 ;;
    *) app="$arg" ;;
  esac
done
app="${app:-$here/ZoeMac.app}"
[[ -d "$app" ]] || { echo "App not found: $app" >&2; exit 1; }

work="$(mktemp -d /tmp/zoe-sign.XXXXXX)"
trap 'rm -rf "$work"' EXIT
ditto "$app" "$work/Zoe.app"

identities=()
while IFS= read -r line; do identities+=("$line"); done < <(security find-identity -v -p codesigning | sed -n 's/^ *[0-9]*) \([0-9A-F]\{40\}\) "\(.*\)"$/\1 \2/p')
((${#identities[@]})) || { echo "No code-signing identity found in Keychain (Xcode > Settings > Accounts > Manage Certificates)." >&2; exit 1; }
if ((${#identities[@]} == 1)); then
  choice=0
else
  echo "Choose a signing identity:"
  for i in "${!identities[@]}"; do echo "  $((i + 1))) ${identities[$i]#* }"; done
  read -r -p "Number: " n
  [[ "$n" =~ ^[0-9]+$ ]] && ((n >= 1 && n <= ${#identities[@]})) || { echo "Invalid choice" >&2; exit 1; }
  choice=$((n - 1))
fi
hash="${identities[$choice]%% *}"
echo "Signing with: ${identities[$choice]#* }"

# Push notifications need a provisioning profile; without one a signed app that claims the
# entitlement will not launch, so the re-signed copy carries no entitlements.
find "$work/Zoe.app/Contents" \( -name '*.framework' -o -name '*.dylib' -o -name '*.xpc' -o -name '*.app' \) -not -path "$work/Zoe.app" -print0 \
  | sort -rz | while IFS= read -r -d '' item; do
      codesign --force --timestamp=none --sign "$hash" "$item"
    done
codesign --force --timestamp=none --sign "$hash" "$work/Zoe.app"
codesign --verify --deep --strict "$work/Zoe.app"
echo "Signature verified: $(codesign -dv "$work/Zoe.app" 2>&1 | sed -n 's/^TeamIdentifier=//p')"

if ((install)); then
  dest="/Applications/Zoe.app"
  osascript -e 'tell application "Zoe" to quit' >/dev/null 2>&1 || true
  rm -rf "$dest"
  ditto "$work/Zoe.app" "$dest"
  xattr -dr com.apple.quarantine "$dest" 2>/dev/null || true
  echo "Installed $dest"
  open "$dest"
else
  out="$HOME/Desktop/Zoe-signed.app"
  rm -rf "$out"
  ditto "$work/Zoe.app" "$out"
  xattr -dr com.apple.quarantine "$out" 2>/dev/null || true
  echo "Saved $out"
fi
