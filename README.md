# Zoe

Zoe is a private, self-hosted AI first mate, named after Zoë Washburne, Captain Malcolm Reynolds's second-in-command in *Firefly*. See [product positioning](docs/product.md#1-product-positioning) for the role this name expresses.

The system has a Linux control plane, independent runner, and native macOS, iPhone, and iPad clients. Implementation is underway; no product milestone has been accepted.

Choose a starting point:

- [Product requirements](docs/product.md): scope, user journeys, interaction rules, and exclusions.
- [Architecture](docs/architecture.md): components, authority, data flows, and [decisions](docs/architecture/decisions/).
- [Delivery plan](docs/master-plan.md): milestones, current status, blockers, and acceptance gates.
- [Deployment operations](docs/operations/deployment.md): installation, configuration, maintenance, and Zoe OpenCode runtime uncertainty reconciliation.
- [Data lifecycle](docs/operations/data-lifecycle.md): export, backup, restore, migration, and deletion.
- [Acceptance matrix](docs/acceptance/requirements.md) and [evidence](docs/acceptance/evidence.md): coverage, verification boundaries, and remaining gaps.
- [A2UI screen contract](docs/design/screen-document.md): accepted macOS ambient-screen design and client tools; implementation pending.
- [CI verification](docs/development/verification.md): change scope, hosted checks, and manual artifact generation.

## Where to write what

| If you are documenting... | Write the complete definition in... |
|---|---|
| A new or changed product constraint | [Product requirements](docs/product.md) (the sole definition home for stable CY-IDs) |
| Architecture boundaries and authoritative ownership | [Architecture](docs/architecture.md) |
| Technical trade-offs and their reasons | [Architecture decision records](docs/architecture/decisions/) |
| Acceptance thresholds and stable-ID mappings | [Acceptance requirements](docs/acceptance/requirements.md) |
| Test, CI, and installation facts and evidence-category definitions | [Evidence register](docs/acceptance/evidence.md) |
| Milestones and delivery status | [Master plan](docs/master-plan.md) |
| Operations commands and procedures | [Operations](docs/operations/) |
| CI scope and manual artifacts | [Verification](docs/development/verification.md) |
| Module implementation parameters | The corresponding module README |
