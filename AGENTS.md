# Repository guidance

## Documentation and product changes

- When changing product behavior or interfaces, follow [product requirements](docs/product.md) and [architecture](docs/architecture.md); update their applicable requirements or decisions when the approved baseline changes.
- When changing native UI, follow [product chapter 3](docs/product.md#3-client-requirements), [ADR 0008](docs/architecture/decisions/0008-ambient-screen-client.md), and the [A2UI screen contract](docs/design/screen-document.md).
- When reporting implementation or verification, follow [the evidence register](docs/acceptance/evidence.md) for evidence categories and the acceptance boundary, with delivery status in [the master plan](docs/master-plan.md).

## Coordination and operational safety

- When working with credentials or private account data, use secret names or deployment references; never put their values in source, documentation, logs, or fixtures.
- When modifying the repository, do not change external services or the existing CLIProxyAPI deployment unless explicitly authorized.
