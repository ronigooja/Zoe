# Operations entry points

See the [configuration contract](design/flows/configuration.md) for independent
model consumers, atomic configuration changes, and retained deployment state.

Implementation entry points:

- [Operator configuration](../cmd/zoe/configure.go)
- [Menu and operations](../cmd/zoe/operations.go)
- [Runner](../internal/runner/engine.go)
- [Deployment implementation](../deploy/linux/)

Deploy the gateway with request fencing before enabling the Zoe build that
uses it. After five minutes from creation or from an unknown-outcome quarantine,
an unbound call can clear its matching Zoe pool hold only on definitive gateway
attestation. A missing fence endpoint, an unresolved gateway account reservation,
or an indeterminate response leaves the hold in place.

Implementation locations do not establish conformance. Record any verification under the
[acceptance boundary](development.md#evidence-and-acceptance).
