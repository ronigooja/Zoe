# Apple client

The macOS menu-bar app has a Voice toggle and a standalone Settings window with one Connection tab.
Connection validation uses the authenticated snapshot endpoint; instance keys are stored per server in Keychain.

## Verification

Run Swift package checks from `Packages/ZoeKit` and build the `ZoeMac` scheme in `Zoe.xcodeproj`.
`tools/validate-apple-build.sh --help` describes isolated verification using an existing dependency cache.
