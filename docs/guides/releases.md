# Release operations

Thread Keep uses GitHub Actions for CI and tag-driven releases. GoReleaser OSS builds every executable on its native target because the SQLite FTS5 dependency requires CGO. The release workflow then publishes raw GitHub assets and npm packages that contain the binaries directly.

## Supported targets

| Go target | npm target | GitHub runner |
| --- | --- | --- |
| `linux/amd64` | `linux-x64` (glibc) | `ubuntu-24.04` |
| `linux/arm64` | `linux-arm64` (glibc) | `ubuntu-24.04-arm` |
| `darwin/amd64` | `darwin-x64` | `macos-15-intel` |
| `darwin/arm64` | `darwin-arm64` | `macos-15` |
| `windows/amd64` | `win32-x64` | `windows-2025` |

Windows arm64 and Linux musl are not release targets yet. macOS binaries are not code-signed or notarized in this workflow.

GoReleaser also publishes three GHCR images as linux/amd64 and linux/arm64 manifests:

| Component | Image |
| --- | --- |
| Server | `ghcr.io/tae2089/thread-keep-server` |
| Coordinator | `ghcr.io/tae2089/thread-keep-coordinator` |
| Runner | `ghcr.io/tae2089/thread-keep-runner` |

Each image receives the release version and `latest` tags. The GoReleaser runtime Dockerfiles copy prebuilt target binaries; they do not compile source again.

## CI

`.github/workflows/ci.yml` runs on pull requests and pushes to `main`:

1. Go tests, including every language-pack module.
2. Go vet and runtime binary builds.
3. npm launcher and release-assembly tests.
4. GoReleaser v2.17 configuration validation.
5. Docker E2E after the quality job passes.

CI requires no publishing credentials.

## One-time release setup

Complete these steps before pushing the first release tag:

1. Generate an Ed25519 key pair in secure release infrastructure. Store only the base64 32-byte public key as the GitHub Actions repository variable `THREAD_KEEP_MANIFEST_PUBLIC_KEY_B64`.
2. Store the matching base64 64-byte Go Ed25519 private key as the Actions secret `THREAD_KEEP_MANIFEST_PRIVATE_KEY_B64`. Never place it in the repository, workflow input, command history, or logs.
3. Create a protected GitHub Actions environment named `npm` and require the desired deployment approvals.
4. Reserve and bootstrap these npm packages: `thread-keep`, `thread-keep-linux-x64`, `thread-keep-linux-arm64`, `thread-keep-darwin-x64`, `thread-keep-darwin-arm64`, and `thread-keep-win32-x64`.
5. For the first publish only, add a granular npm automation token with publish access as the Actions secret `NPM_TOKEN`.
6. After the packages exist, configure npm trusted publishing for each package with repository `tae2089/thread-keep`, workflow `release.yml`, and environment `npm`. Remove `NPM_TOKEN` after an OIDC release succeeds, then disallow token publishing in npm package settings.
7. After the first container publication, set each GHCR package to public visibility and confirm it inherits or grants Actions access to `tae2089/thread-keep`. Container publication uses the job-scoped `GITHUB_TOKEN`; it needs no registry secret.

npm trusted publishing requires GitHub-hosted runners and `id-token: write`. The workflow uses Node 24 and publishes provenance. The package `repository.url` is fixed to the official GitHub repository because npm validates that identity for trusted publishing.

## Release flow

Create and push a strict stable SemVer tag only after CI is green:

```bash
git tag v1.2.3
git push origin v1.2.3
```

The workflow performs these gates in order:

1. Validate the tag, official repository identity, source/tests, Docker E2E, GoReleaser config, and `LICENSE`.
2. Build five runtime binaries and six pack binaries on each native target with `CGO_ENABLED=1`.
3. Require all 55 target-qualified binaries before assembling metadata.
4. Generate SHA-256 checksums and the pack-manifest payload.
5. Verify the configured public/private signing keys match, sign the manifest with the existing Go signer, and delete the temporary private-key file.
6. Dry-run every npm tarball.
7. Publish the GitHub Release.
8. Publish all five platform npm packages.
9. Publish the `thread-keep` meta package last.
10. Cross-compile the container binaries for Linux amd64/arm64 and let GoReleaser `dockers_v2` publish the three GHCR manifests.

Publishing the meta package last prevents users from receiving a version whose platform package was never published. A rerun accepts existing GitHub/npm artifacts only when their full asset list, checksums, and npm tarball integrity exactly match the rebuilt output. Partial or mismatched remote publication fails without overwriting anything and requires deliberate operator recovery.

Container publication runs after GitHub and npm publication. If it fails, those earlier artifacts remain valid; inspect any GHCR manifest already pushed and rerun the failed container job. The workflow never deletes registry state during recovery.

## Artifact contract

GitHub Release assets use stable names such as:

```text
thread-keep_linux_amd64
thread-keep-mcp_darwin_arm64
thread-keep-index-typescript_windows_amd64.exe
checksums.txt
thread-keep-indexers-manifest-v1.json
```

The signed manifest contains exact target URLs, byte sizes, and SHA-256 values for all six language packs. The release `thread-keep` binary embeds only the matching public verification key.

The npm meta package exposes the two local user-facing commands:

```text
thread-keep
thread-keep-mcp
```

Operational server, coordinator, and runner binaries remain GitHub Release assets and container targets so every npm CLI installation does not carry those larger binaries. The npm package uses exact-version `optionalDependencies` plus npm `os`, `cpu`, and Linux `libc` filters. No lifecycle install script or secondary binary download is used.

The repository and all generated npm packages declare the SPDX license identifier `MIT`. The release assembler copies the repository-root `LICENSE` into every platform and meta package.

Pull a published component by release version:

```bash
docker pull ghcr.io/tae2089/thread-keep-server:1.2.3
docker pull ghcr.io/tae2089/thread-keep-coordinator:1.2.3
docker pull ghcr.io/tae2089/thread-keep-runner:1.2.3
```

## Local verification

```bash
make release-test
npm_config_cache=/tmp/thread-keep-npm-cache \
  npx --yes @goreleaser/goreleaser@2.17.0 check
npm_config_cache=/tmp/thread-keep-npm-cache \
  npx --yes @goreleaser/goreleaser@2.17.0 check \
  --config .goreleaser.docker.yaml
```

A native snapshot additionally needs a valid public-key-shaped test value or the real public release variable:

```bash
THREAD_KEEP_MANIFEST_PUBLIC_KEY_B64=<base64-public-key> \
  npx --yes @goreleaser/goreleaser@2.17.0 \
  build --single-target --clean --snapshot
```

The container configuration additionally requires Linux CGO cross-compilers plus Docker Buildx/QEMU. Local contract tests validate its image layout without publishing; the first real tag must verify all three remote multi-platform manifests.

## External contracts

- [GoReleaser GitHub Actions](https://www.goreleaser.com/customization/ci/actions/)
- [GoReleaser CGO limitation](https://www.goreleaser.com/resources/limitations/cgo/)
- [GoReleaser Docker v2](https://goreleaser.com/customization/package/dockers_v2/)
- [npm package `os`, `cpu`, `libc`, and optional dependencies](https://docs.npmjs.com/files/package.json/)
- [npm trusted publishing](https://docs.npmjs.com/trusted-publishers/)
- [GitHub-hosted runner labels](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
- [GitHub Container registry authentication](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
