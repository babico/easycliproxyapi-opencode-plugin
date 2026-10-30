# Contributing to OpenCode Go CLIProxyAPI Plugin

Thank you for contributing. This guide covers how to set up the development environment, build the plugin, and submit pull requests.

---

## Development Setup

### Prerequisites
- Go 1.26+ (with CGO enabled for C-shared builds)
- Git

### Build & Test Commands

**Always build and test using the `debug` build tag for local development. Do not build in release mode.**

Run the test suite:
```powershell
go test -tags debug ./...
```

Run static analysis:
```powershell
go vet ./...
```

Build the native shared library for local testing:
```powershell
go build -tags debug -buildmode=c-shared -o plugins/windows/amd64/easycliproxyapi-opencode-provider.dll .
```

---

## Pull Request Guidelines

1. **Focused Scope:** Keep each PR targeted to a single fix or feature. Avoid bundling unrelated refactors or features together.
2. **Debug-Mode Verification:** Verify all changes using `-tags debug`. Release packaging is handled separately by CI.
3. **Test Coverage:** All new or modified behavioral paths require unit tests. Both streaming and non-streaming tests are required for protocol changes.
4. **Offline Execution:** Unit tests must run locally and offline against mock host clients. Do not introduce dependencies on live external APIs.
5. **Documentation:** Append or merge changes into `RELEASE_NOTES.md` and update `README.md` if user-facing configuration changes.
6. **Code Style:** Format all code with standard `gofmt` and adhere to idiomatic Go conventions.

---

## Workflow

1. Fork the repository and create a feature branch from `main`.
2. Implement your changes following the code style and testing guidelines above.
3. Ensure `go test -tags debug ./...` and `go vet ./...` pass cleanly.
4. Open a pull request against `main` with a clear description of the change and any related issue references.

---

## Release Checklist

1. Bump `pluginVersion` in `internal/plugin/plugin.go`.
2. Rewrite `RELEASE_NOTES.md` as the release notes for that version — the file is published verbatim as the GitHub release body — and point its compare link at the new tag.
3. Set `version` in `registry.json` to the same version. The plugin store falls back to this field when its GitHub release lookup fails (anonymous API quota, network, proxy); if it is missing or stale the store cannot show an update badge.
4. Run `go test ./...`, `go test -tags debug ./...`, `go vet ./...` and `gofmt -l .`.
5. Commit `chore(release): vX.Y.Z`, push `main`, then create and push the annotated tag. CI builds the six release targets and publishes the GitHub release from `RELEASE_NOTES.md`.
