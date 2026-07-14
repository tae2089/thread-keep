import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const PACK_BINARIES = [
  "thread-keep-index-typescript",
  "thread-keep-index-javascript",
  "thread-keep-index-python",
  "thread-keep-index-java",
  "thread-keep-index-kotlin",
  "thread-keep-index-rust",
];

const IMAGES = [
  {
    component: "server",
    binaries: ["thread-keep-server"],
  },
  {
    component: "coordinator",
    binaries: ["thread-keep-coordinator", "thread-keep-runner", ...PACK_BINARIES],
  },
  {
    component: "runner",
    binaries: ["thread-keep-runner", ...PACK_BINARIES],
  },
];

test("GoReleaser runtime Dockerfiles copy prebuilt target binaries only", async () => {
  for (const image of IMAGES) {
    const dockerfile = await readFile(`Dockerfile.goreleaser.${image.component}`, "utf8");
    assert.match(dockerfile, /^ARG TARGETPLATFORM$/m);
    assert.doesNotMatch(dockerfile, /FROM\s+golang:|\bgo build\b|\bgo mod download\b/);
    for (const binary of image.binaries) {
      assert.match(dockerfile, new RegExp(`COPY .*\\$TARGETPLATFORM/${binary}(?: |$)`, "m"));
    }
    if (image.component === "server") {
      assert.match(dockerfile, /\/var\/lib\/thread-keep\//);
    }
    assert.match(dockerfile, new RegExp(`ENTRYPOINT \\["/usr/local/bin/thread-keep-${image.component}"\\]`));
  }
});

test("container GoReleaser config defines three dual-platform GHCR images", async () => {
  const config = await readFile(".goreleaser.docker.yaml", "utf8");
  assert.match(config, /^dockers_v2:$/m);
  assert.match(config, /CC=\{\{ if eq \.Arch "arm64" \}\}aarch64-linux-gnu-gcc\{\{ else \}\}gcc\{\{ end \}\}/);
  for (const image of IMAGES) {
    assert.match(config, new RegExp(`id: image-${image.component}`));
    assert.match(config, new RegExp(`ghcr\\.io/tae2089/thread-keep-${image.component}`));
  }
  assert.equal((config.match(/- linux\/amd64/g) || []).length, IMAGES.length);
  assert.equal((config.match(/- linux\/arm64/g) || []).length, IMAGES.length);
});

test("all released pack binaries report the release SemVer", async () => {
  for (const file of [".goreleaser.yaml", ".goreleaser.docker.yaml"]) {
    const config = await readFile(file, "utf8");
    assert.equal(
      (config.match(/-X main\.indexerVersion=\{\{ \.Version \}\}/g) || []).length,
      PACK_BINARIES.length,
      `${file} must inject the version into every pack binary`,
    );
  }
});

test("tag workflow publishes containers after GitHub and npm artifacts", async () => {
  const workflow = await readFile(".github/workflows/release.yml", "utf8");
  assert.match(workflow, /^  containers:$/m);
  assert.match(workflow, /^    needs: publish$/m);
  assert.match(workflow, /^      packages: write$/m);
  assert.match(workflow, /uses: docker\/login-action@v3/);
  assert.match(workflow, /release --clean --config \.goreleaser\.docker\.yaml/);
});

test("tag workflow publishes GoReleaser-derived wheels through PyPI Trusted Publishing", async () => {
  const workflow = await readFile(".github/workflows/release.yml", "utf8");
  assert.match(workflow, /python3 scripts\/release\/build_wheels\.py/);
  assert.match(workflow, /name: pypi-wheels/);
  assert.match(workflow, /^  pypi:$/m);
  assert.match(workflow, /^    needs: publish$/m);
  assert.match(workflow, /^      name: pypi$/m);
  assert.match(workflow, /^      id-token: write$/m);
  assert.match(workflow, /node scripts\/release\/verify-pypi\.mjs/);
  assert.match(workflow, /uses: pypa\/gh-action-pypi-publish@release\/v1/);
  assert.match(workflow, /^          packages-dir: release\/wheels\/$/m);
  assert.match(workflow, /^          skip-existing: true$/m);
});

test("pull request CI validates the container GoReleaser config", async () => {
  const workflow = await readFile(".github/workflows/ci.yml", "utf8");
  assert.match(workflow, /check --config \.goreleaser\.docker\.yaml/);
});
