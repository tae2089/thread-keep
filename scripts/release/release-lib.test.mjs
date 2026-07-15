import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import * as releaseModule from "./release-lib.mjs";

const {
  ALL_BINARIES,
  TARGETS,
  assembleRelease,
  stageTargetArtifacts,
} = releaseModule;

async function withTempDir(runTest) {
  const root = await mkdtemp(path.join(os.tmpdir(), "thread-keep-release-"));
  try {
    await runTest(root);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

async function seedGoReleaserDist(root, target) {
  const extension = target.goos === "windows" ? ".exe" : "";
  for (const binary of ALL_BINARIES) {
    const directory = path.join(root, `${binary}_${target.goos}_${target.goarch}`);
    await mkdir(directory, { recursive: true });
    await writeFile(path.join(directory, `${binary}${extension}`), `${binary}:${target.id}`);
  }
}

async function seedStagedArtifacts(root) {
  for (const target of TARGETS) {
    const extension = target.goos === "windows" ? ".exe" : "";
    for (const binary of ALL_BINARIES) {
      await writeFile(
        path.join(root, `${binary}_${target.goos}_${target.goarch}${extension}`),
        `${binary}:${target.id}`,
      );
    }
  }
}

test("stageTargetArtifacts copies every GoReleaser binary with stable asset names", async () => {
  await withTempDir(async (root) => {
    const distDir = path.join(root, "dist");
    const outDir = path.join(root, "staged");
    const target = TARGETS.find((item) => item.id === "darwin-arm64");
    await seedGoReleaserDist(distDir, target);

    const staged = await stageTargetArtifacts({ distDir, outDir, target });

    assert.equal(staged.length, ALL_BINARIES.length);
    for (const binary of ALL_BINARIES) {
      const info = await stat(path.join(outDir, `${binary}_darwin_arm64`));
      assert.equal(info.isFile(), true);
    }
  });
});

test("stageTargetArtifacts fails if a required GoReleaser binary is missing", async () => {
  await withTempDir(async (root) => {
    const distDir = path.join(root, "dist");
    const target = TARGETS.find((item) => item.id === "linux-x64");
    await seedGoReleaserDist(distDir, target);
    await rm(path.join(distDir, "thread-keep-runner_linux_amd64", "thread-keep-runner"));

    await assert.rejects(
      stageTargetArtifacts({ distDir, outDir: path.join(root, "staged"), target }),
      /missing GoReleaser artifact.*thread-keep-runner/,
    );
  });
});

test("assembleRelease creates unsigned GitHub assets and checksums without a manifest", async () => {
  await withTempDir(async (root) => {
    const artifactsDir = path.join(root, "artifacts");
    const outDir = path.join(root, "release");
    await mkdir(artifactsDir, { recursive: true });
    await seedStagedArtifacts(artifactsDir);

    await assembleRelease({
      artifactsDir,
      outDir,
      repository: "tae2089/thread-keep",
      tag: "v1.2.3",
      version: "1.2.3",
    });

    const checksums = (await readFile(path.join(outDir, "assets", "checksums.txt"), "utf8")).trim().split("\n");
    assert.equal(checksums.length, ALL_BINARIES.length * TARGETS.length);
    await assert.rejects(
      stat(path.join(outDir, "assets", "thread-keep-indexers-manifest-v1.payload.json")),
      { code: "ENOENT" },
    );
    await assert.rejects(stat(path.join(outDir, "npm")), { code: "ENOENT" });
  });
});

test("assembleRelease rejects an incomplete target before writing publishable metadata", async () => {
  await withTempDir(async (root) => {
    const artifactsDir = path.join(root, "artifacts");
    await mkdir(artifactsDir, { recursive: true });
    await seedStagedArtifacts(artifactsDir);
    await rm(path.join(artifactsDir, "thread-keep-index-rust_linux_amd64"));

    await assert.rejects(
      assembleRelease({
        artifactsDir,
        outDir: path.join(root, "release"),
        repository: "tae2089/thread-keep",
        tag: "v1.2.3",
        version: "1.2.3",
      }),
      /missing staged artifact.*thread-keep-index-rust_linux_amd64/,
    );
  });
});

test("release assembly exposes no manifest signing helper", () => {
  assert.equal("validateSigningKeyPair" in releaseModule, false);
});

test("every native release target declares one platform wheel tag without npm metadata", () => {
  assert.deepEqual(
    TARGETS.map((target) => target.wheelTag),
    [
      "manylinux_2_39_x86_64",
      "manylinux_2_39_aarch64",
      "macosx_15_0_arm64",
      "win_amd64",
    ],
  );
  assert.equal(
    TARGETS.some((target) => target.goos === "darwin" && target.goarch === "amd64"),
    false,
  );
  for (const target of TARGETS) {
    assert.equal("npmOS" in target, false);
    assert.equal("npmCPU" in target, false);
  }
});
