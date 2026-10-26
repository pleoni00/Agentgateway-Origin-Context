# Contributing

Use English for documentation, code comments, commit messages and pull requests.

## Development

The repository is self-contained. Install Python 3.12, Go 1.25 and Docker Compose, then run:

```bash
make setup
make test-go
make check
make up
make test-integration
make down
```

The Go service and gateway configuration are the extension components. Agents, services, MCP servers, Keycloak realm and the local LLM provider are demonstration fixtures. Keep deployment-specific identities and credentials in those fixtures or local configuration.

## Changes

Explain the problem, the resulting behavior and the validation performed. Authorization changes should cover both discovery and invocation and confirm that denied requests do not execute a backend operation. Lifecycle changes should cover successful responses, upstream failures and cancellation.

Never commit `.env`, database contents, access tokens, private keys, generated reports or virtual environments. Published demo credentials are fixtures; do not reuse them in a real deployment. Keep the documented single-process and synchronous-operation limits explicit.

Run embedded SQL migrations through the separate `migrate` command. Do not create schema objects from request handlers or restore revoked grants when restarting the service.

Original contributions use the repository's Apache-2.0 license. Do not add a third-party license notice unless the corresponding material is actually included.
