# Development

Start with the [data-flow overview](design/data-flows.md) and read the relevant
component contracts.

## Documentation maintenance

Keep definitions in their owning component and use links elsewhere. Update the
definition and affected links when an approved behavior changes.

## Evidence and acceptance

Record source revision, environment, action, outcome, and limits when accepting
an implementation. Schema/fixture, source review,
build, installation, health, render ACK, and real user-visible end-to-end success
are distinct. Design approval establishes no test, deployment, or acceptance.

Each component lists its verification obligations.

## Documentation checks

Run the following small local checks from the repository root:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 tools/check_docs.py --strict
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools -p test_check_docs.py
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tools -p test_ci_scope.py
git diff --check
```

These check structure, navigation, and checker behavior, not product acceptance.
