# Operations entry points

See the [configuration contract](design/flows/configuration.md) for independent
model consumers, atomic configuration changes, and retained deployment state.

Implementation entry points:

- [Operator configuration](../cmd/zoe/configure.go)
- [Menu and operations](../cmd/zoe/operations.go)
- [Runner](../internal/runner/engine.go)
- [Deployment implementation](../deploy/linux/)

Implementation locations do not establish conformance. Record any verification under the
[acceptance boundary](development.md#evidence-and-acceptance).
