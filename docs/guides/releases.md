# Release operations

Thread Keep uses GitHub Actions for CI and tag-driven releases. GoReleaser OSS builds every executable on its native target because the SQLite FTS5 dependency requires CGO. The release workflow then publishes raw GitHub assets, platform-specific PyPI wheels, and container images from those target-qualified binaries.

## Supported targets

| Go target | PyPI wheel platform tag | GitHub runner |
| --- | --- | --- |
| `linux/amd64` | `manylinux_2_39_x86_64` | `ubuntu-24.04` |
| `linux/arm64` | `manylinux_2_39_aarch64` | `ubuntu-24.04-arm` |
| `darwin/amd64` | `macosx_15_0_x86_64` | `macos-15-intel` |
| `darwin/arm64` | `macosx_15_0_arm64` | `macos-15` |
| `windows/amd64` | `win_amd64` | `windows-2025` |

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
3. PyPI wheel/launcher and release-assembly tests.
4. GoReleaser v2.17 configuration validation.
5. Docker E2E after the quality job passes.

CI requires no publishing credentials.

## One-time release setup

Complete these steps before pushing the first release tag:

1. Generate an Ed25519 key pair in secure release infrastructure. Store only the base64 32-byte public key as the GitHub Actions repository variable `THREAD_KEEP_MANIFEST_PUBLIC_KEY_B64`.
2. Store the matching base64 64-byte Go Ed25519 private key as the Actions secret `THREAD_KEEP_MANIFEST_PRIVATE_KEY_B64`. Never place it in the repository, workflow input, command history, or logs.
3. Create a protected GitHub Actions environment named `release` and require the desired deployment approvals for GitHub Release publication.
4. Create a protected GitHub Actions environment named `pypi`. Reserve or create the PyPI project `thread-keep`, then configure its GitHub Trusted Publisher for owner `tae2089`, repository `thread-keep`, workflow `release.yml`, and environment `pypi`. No PyPI API token is stored in GitHub.
5. After the first container publication, set each GHCR package to public visibility and confirm it inherits or grants Actions access to `tae2089/thread-keep`. Container publication uses the job-scoped `GITHUB_TOKEN`; it needs no registry secret.

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
4. Generate SHA-256 checksums, the pack-manifest payload, and five deterministic platform wheels from the staged GoReleaser artifacts.
5. Validate every wheel archive, including its platform tag, packaged core binaries, six official packs, executable modes, entry points, and `RECORD` hashes.
6. Verify the configured public/private signing keys match, sign the manifest with the existing Go signer, and delete the temporary private-key file.
7. Publish or byte-for-byte verify the GitHub Release.
8. Verify any existing PyPI wheel filename and SHA-256 before using Trusted Publishing to upload missing wheels.
9. Independently cross-compile the container binaries for Linux amd64/arm64 and let GoReleaser `dockers_v2` publish the three GHCR manifests.

A rerun accepts existing GitHub artifacts only when their full asset list and checksums exactly match the rebuilt output. PyPI recovery permits `skip-existing` only after every existing wheel digest matches the deterministic local rebuild. Partial matching publication can resume; any mismatch fails before upload.

PyPI and container publication run independently after the GitHub Release job. If either fails, the GitHub Release remains valid and the other job is not rolled back. Inspect any already-published immutable artifact before rerunning the failed job; the workflow never deletes registry state during recovery.

## Artifact contract

GitHub Release assets use stable names such as:

```text
thread-keep_linux_amd64
thread-keep-mcp_darwin_arm64
thread-keep-index-typescript_windows_amd64.exe
checksums.txt
thread-keep-indexers-manifest-v1.json
```

The signed manifest contains the release SemVer plus exact target URLs, byte sizes, and SHA-256 values for all six language packs. GoReleaser injects that same SemVer into every pack's runtime descriptor, and the release `thread-keep` binary embeds only the matching public verification key. Managed installs retain immutable digest-addressed binaries and atomically switch a small activation document during `indexers sync`.

The single PyPI project publishes five files for the same release version, for example:

```text
thread_keep-1.2.3-py3-none-manylinux_2_39_x86_64.whl
thread_keep-1.2.3-py3-none-macosx_15_0_arm64.whl
thread_keep-1.2.3-py3-none-win_amd64.whl
```

pip selects one compatible wheel. Each wheel contains `thread-keep`, `thread-keep-mcp`, and all six official packs. Its Python console-script adapter passes the bundled directory and exact release version to the Go resolver; a signed managed activation or legacy local pack remains higher priority. Wheels are binary-only and require Python 3.9 or newer. No sdist is published because one source archive cannot reproduce all target-qualified CGO binaries.

The repository and every generated wheel declare the SPDX license identifier `MIT`. The wheel builder copies the repository-root `LICENSE` into every platform wheel.

Pull a published component by release version:

```bash
docker pull ghcr.io/tae2089/thread-keep-server:1.2.3
docker pull ghcr.io/tae2089/thread-keep-coordinator:1.2.3
docker pull ghcr.io/tae2089/thread-keep-runner:1.2.3
```

## Local verification

```bash
make release-test
python3 -m unittest \
  scripts/release/test_build_wheels.py \
  scripts/release/test_pypi_launcher.py
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
- [PyPA wheel binary distribution format](https://packaging.python.org/en/latest/specifications/binary-distribution-format/)
- [PyPI Trusted Publishing](https://docs.pypi.org/trusted-publishers/using-a-publisher/)
- [GoReleaser UV builder and `py3-none-any` limitation](https://www.goreleaser.com/customization/builds/builders/uv/)
- [GitHub-hosted runner labels](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)
- [GitHub Container registry authentication](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)
