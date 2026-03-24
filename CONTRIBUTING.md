# Contributing to Pod NSG Controller

This project welcomes contributions and suggestions. Most contributions require you to agree to a Contributor License Agreement (CLA) declaring that you have the right to, and actually do, grant us the rights to use your contribution. For details, visit https://cla.opensource.microsoft.com.

When you submit a pull request, a CLA bot will automatically determine whether you need to provide a CLA and decorate the PR appropriately (e.g., status check, comment). Simply follow the instructions provided by the bot. You will only need to do this once across all repos using our CLA.

This project has adopted the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/). For more information see the [Code of Conduct FAQ](https://opensource.microsoft.com/codeofconduct/faq/) or contact [opencode@microsoft.com](mailto:opencode@microsoft.com) with any additional questions or comments.

## Getting Started

1. **Fork** the repository and clone your fork locally.
2. Install prerequisites:
   - Go 1.22+
   - `kubectl` with access to a Kubernetes cluster
   - `make`
3. Create a feature branch from `main`:
   ```bash
   git checkout -b feature/my-change
   ```
4. Make your changes.
5. Run tests and linting:
   ```bash
   make test
   make lint
   ```
6. Commit your changes with a clear, descriptive commit message.
7. Push to your fork and open a pull request against the `main` branch.

## Development Workflow

### Building

```bash
make build
```

### Running Tests

```bash
# Unit tests
make test

# With coverage
make test-coverage
```

### Linting

```bash
make lint
```

### Generating Code and Manifests

If you modify API types or RBAC markers, regenerate the generated code:

```bash
make generate manifests
```

## Pull Request Guidelines

- **One concern per PR** — Keep pull requests focused on a single change.
- **Write tests** — All new functionality should include unit tests. Bug fixes should include a regression test.
- **Follow Go conventions** — Use `gofmt`, follow [Effective Go](https://go.dev/doc/effective_go), and pass the project linter.
- **Update documentation** — If your change affects user-facing behavior, update the relevant docs.
- **Descriptive commits** — Write clear commit messages. Use the imperative mood (e.g., "Add rule caching" not "Added rule caching").
- **Keep it small** — Smaller PRs are reviewed faster and are less likely to introduce issues.

## Reporting Issues

- Search [existing issues](https://github.com/Azure/pod-nsg-controller/issues) before opening a new one.
- Use the issue templates when available.
- Include steps to reproduce, expected behavior, and actual behavior for bug reports.

## Code of Conduct

This project follows the [Microsoft Open Source Code of Conduct](https://opensource.microsoft.com/codeofconduct/).
