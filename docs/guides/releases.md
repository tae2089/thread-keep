# Release operations

This guide is for maintainers publishing a Thread Keep release. Users looking to
install Thread Keep should follow the [Quickstart](quickstart.md); operators looking
for deployment commands should use the [Team server](team-server.md) or
[PR context planning](pr-context-coordinator.md) guide.

The release workflow publishes three distinct artifact families: PyPI wheels for
the local CLI/MCP and optional packs, unsigned raw GitHub Release binaries for all
runtime roles, and three GHCR images for the deployable server/coordinator/runner
boundaries.

Thread Keep uses GitHub Actions for CI and tag-driven releases. GoReleaser OSS builds every executable on its native target because the SQLite FTS5 dependency requires CGO. The release workflow then publishes raw GitHub assets, platform-specific PyPI wheels, and container images from those target-qualified binaries.

## Supported targets

| Go target | PyPI wheel platform tag | GitHub runner |
| --- | --- | --- |
| `linux/amd64` | `manylinux_2_39_x86_64` | `ubuntu-24.04` |
| `linux/arm64` | `manylinux_2_39_aarch64` | `ubuntu-24.04-arm` |
| `darwin/arm64` | `macosx_15_0_arm64` | `macos-15` |
| `windows/amd64` | `win_amd64` | `windows-2025` |

Windows arm64, Linux musl, and macOS Intel are not release targets. macOS Apple Silicon binaries are not code-signed or notarized in this workflow.
The wheel tags require Linux glibc 2.39 or newer and macOS 15 or newer; pip rejects these wheels on older OS baselines before installation.

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

1. Create a protected GitHub Actions environment named `release` and require the desired deployment approvals for GitHub Release publication.
2. Create a protected GitHub Actions environment named `pypi` for the core project. Create six additional protected environments named `pypi-typescript`, `pypi-javascript`, `pypi-python`, `pypi-java`, `pypi-kotlin`, and `pypi-rust`. Reserve or create all seven PyPI projects: `thread-keep` plus `thread-keep-pack-typescript`, `thread-keep-pack-javascript`, `thread-keep-pack-python`, `thread-keep-pack-java`, `thread-keep-pack-kotlin`, and `thread-keep-pack-rust`. Configure each project's GitHub Trusted Publisher for owner `tae2089`, repository `thread-keep`, and workflow `release.yml`; use environment `pypi` for `thread-keep` and the matching `pypi-<language>` environment for each pack. Distinct pack environments allow all six not-yet-created projects to be registered as Pending Publishers without an identical-configuration collision. No PyPI API token is stored in GitHub.
3. After the first container publication, set each GHCR package to public visibility and confirm it inherits or grants Actions access to `tae2089/thread-keep`. Container publication uses the job-scoped `GITHUB_TOKEN`; it needs no registry secret.

## Release flow

Create and push a strict stable SemVer tag only after CI is green:

```bash
git tag v1.2.3
git push origin v1.2.3
```

The workflow performs these gates in order:

1. Validate the tag, official repository identity, source/tests, Docker E2E, GoReleaser config, and `LICENSE`.
2. Build five runtime binaries and six pack binaries on each native target with `CGO_ENABLED=1`.
3. Require all 44 target-qualified binaries before assembling release output.
4. Generate SHA-256 checksums and 28 deterministic platform wheels from the staged GoReleaser artifacts: four core wheels and four wheels for each of six pack distributions.
5. Validate every wheel archive, including its platform tag, isolated core or single-pack contents, executable modes, extras metadata, entry points, and `RECORD` hashes.
6. Publish or byte-for-byte verify the unsigned raw GitHub Release artifacts.
7. Verify any existing PyPI wheel filename and SHA-256, publish all six pack projects, then publish the core project only after every pack job succeeds.
8. Independently cross-compile the container binaries for Linux amd64/arm64 and let GoReleaser `dockers_v2` publish the three GHCR manifests.

A rerun accepts existing GitHub artifacts only when their full asset list and checksums exactly match the rebuilt output. PyPI recovery permits `skip-existing` only after every existing wheel digest matches the deterministic local rebuild. Partial matching publication can resume; any mismatch fails before upload. GHCR release-version tags are immutable: the container job queries every package first and stops if the SemVer tag already exists, while `latest` advances only during a first successful publication of that version.

Pack publication and container publication run independently after the GitHub Release job. Core PyPI publication waits for every pack project, while containers do not. If either path fails, the GitHub Release remains valid and no immutable artifact is rolled back. A container rerun is safe only when none of the three SemVer tags was published; if publication was partial, inspect the registry and recover manually rather than overwriting the completed tag. The workflow never deletes registry state during recovery.

## Artifact contract

GitHub Release assets use stable names such as:

```text
thread-keep_linux_amd64
thread-keep-mcp_darwin_arm64
thread-keep-index-typescript_windows_amd64.exe
checksums.txt
```

GitHub Release binaries are unsigned raw artifacts. `checksums.txt` records every artifact's SHA-256 for deterministic recovery and corruption detection, but it is published beside the artifacts and is not an independent publisher signature. The raw binaries are retained for manual and operational use; PyPI remains the supported installation and version-management channel for the complete local CLI.

Each of the seven PyPI projects publishes four files for the same release version. Core examples:

```text
thread_keep-1.2.3-py3-none-manylinux_2_39_x86_64.whl
thread_keep-1.2.3-py3-none-manylinux_2_39_aarch64.whl
thread_keep-1.2.3-py3-none-macosx_15_0_arm64.whl
thread_keep-1.2.3-py3-none-win_amd64.whl
```

pip selects one compatible core wheel, which contains only `thread-keep` and `thread-keep-mcp`. Extras such as `thread-keep[typescript,python]` add the matching `thread-keep-pack-<language>` wheels with exact-version dependencies; `thread-keep[all]` adds all six. The Python console-script adapter passes validated installed-pack paths and the exact release version to the Go resolver, and those packs take precedence over legacy fixed-path manual packs. Wheels are binary-only and require Python 3.9 or newer. No sdist is published because one source archive cannot reproduce all target-qualified CGO binaries.

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

A native snapshot needs no release signing configuration:

```bash
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
