import assert from "node:assert/strict";
import { createHash, generateKeyPairSync } from "node:crypto";
import { mkdtemp, mkdir, readFile, rm, stat, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  ALL_BINARIES,
  NPM_BINARIES,
  PACKS,
  TARGETS,
  assembleRelease,
  stageTargetArtifacts,
  validateSigningKeyPair,
} from "./release-lib.mjs";

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

test("assembleRelease creates checksums, a complete pack manifest, and npm packages", async () => {
  await withTempDir(async (root) => {
    const artifactsDir = path.join(root, "artifacts");
    const licenseFile = path.join(root, "LICENSE");
    const outDir = path.join(root, "release");
    await mkdir(artifactsDir, { recursive: true });
    await seedStagedArtifacts(artifactsDir);
    await writeFile(licenseFile, "test license\n");

    await assembleRelease({
      artifactsDir,
      licenseFile,
      outDir,
      metaTemplateDir: path.resolve("npm/thread-keep"),
      repository: "tae2089/thread-keep",
      tag: "v1.2.3",
      version: "1.2.3",
    });

    const payload = JSON.parse(await readFile(path.join(outDir, "assets", "thread-keep-indexers-manifest-v1.payload.json"), "utf8"));
    assert.equal(payload.schema_version, 1);
    assert.equal(payload.packs.length, PACKS.length);
    for (const pack of payload.packs) {
      assert.equal(pack.version, "1.2.3");
      assert.equal(pack.protocol_version, 1);
      assert.equal(pack.assets.length, TARGETS.length);
      for (const asset of pack.assets) {
        const target = TARGETS.find((candidate) => candidate.goos === asset.goos && candidate.goarch === asset.goarch);
        assert.ok(target);
        const contents = Buffer.from(`${pack.id}:${target.id}`);
        assert.equal(asset.size, contents.length);
        assert.equal(asset.sha256, createHash("sha256").update(contents).digest("hex"));
        assert.equal(asset.url, `https://github.com/tae2089/thread-keep/releases/download/v1.2.3/${pack.id}_${asset.goos}_${asset.goarch}${asset.goos === "windows" ? ".exe" : ""}`);
      }
    }

    const checksums = (await readFile(path.join(outDir, "assets", "checksums.txt"), "utf8")).trim().split("\n");
    assert.equal(checksums.length, ALL_BINARIES.length * TARGETS.length);

    const metaPackage = JSON.parse(await readFile(path.join(outDir, "npm", "thread-keep", "package.json"), "utf8"));
    assert.equal(metaPackage.version, "1.2.3");
    assert.equal(metaPackage.license, "MIT");
    assert.equal(await readFile(path.join(outDir, "npm", "thread-keep", "LICENSE"), "utf8"), "test license\n");
    assert.deepEqual(Object.values(metaPackage.optionalDependencies), Array(TARGETS.length).fill("1.2.3"));

    for (const target of TARGETS) {
      const packageName = `thread-keep-${target.npmOS}-${target.npmCPU}`;
      const packageRoot = path.join(outDir, "npm", packageName);
      const packageJSON = JSON.parse(await readFile(path.join(packageRoot, "package.json"), "utf8"));
      assert.deepEqual(packageJSON.os, [target.npmOS]);
      assert.deepEqual(packageJSON.cpu, [target.npmCPU]);
      assert.equal(packageJSON.license, "MIT");
      assert.equal(await readFile(path.join(packageRoot, "LICENSE"), "utf8"), "test license\n");
      if (target.goos === "linux") {
        assert.deepEqual(packageJSON.libc, ["glibc"]);
      } else {
        assert.equal("libc" in packageJSON, false);
      }
      for (const binary of NPM_BINARIES) {
        const extension = target.goos === "windows" ? ".exe" : "";
        assert.equal((await stat(path.join(packageRoot, "bin", `${binary}${extension}`))).isFile(), true);
      }
      const serverExtension = target.goos === "windows" ? ".exe" : "";
      await assert.rejects(stat(path.join(packageRoot, "bin", `thread-keep-server${serverExtension}`)));
    }
  });
});

test("assembleRelease rejects an incomplete target before writing publishable metadata", async () => {
  await withTempDir(async (root) => {
    const artifactsDir = path.join(root, "artifacts");
    const licenseFile = path.join(root, "LICENSE");
    await mkdir(artifactsDir, { recursive: true });
    await seedStagedArtifacts(artifactsDir);
    await writeFile(licenseFile, "test license\n");
    await rm(path.join(artifactsDir, "thread-keep-index-rust_linux_amd64"));

    await assert.rejects(
      assembleRelease({
        artifactsDir,
        licenseFile,
        outDir: path.join(root, "release"),
        metaTemplateDir: path.resolve("npm/thread-keep"),
        repository: "tae2089/thread-keep",
        tag: "v1.2.3",
        version: "1.2.3",
      }),
      /missing staged artifact.*thread-keep-index-rust_linux_amd64/,
    );
  });
});

test("assembleRelease rejects a missing license before creating packages", async () => {
  await withTempDir(async (root) => {
    const artifactsDir = path.join(root, "artifacts");
    await mkdir(artifactsDir, { recursive: true });
    await seedStagedArtifacts(artifactsDir);

    await assert.rejects(
      assembleRelease({
        artifactsDir,
        licenseFile: path.join(root, "LICENSE"),
        outDir: path.join(root, "release"),
        metaTemplateDir: path.resolve("npm/thread-keep"),
        repository: "tae2089/thread-keep",
        tag: "v1.2.3",
        version: "1.2.3",
      }),
      /release license file is missing/,
    );
  });
});

test("validateSigningKeyPair accepts matching Ed25519 Go key material", () => {
  const { privateKey, publicKey } = generateKeyPairSync("ed25519");
  const publicJWK = publicKey.export({ format: "jwk" });
  const privateJWK = privateKey.export({ format: "jwk" });
  const rawPublic = Buffer.from(publicJWK.x, "base64url");
  const goPrivate = Buffer.concat([
    Buffer.from(privateJWK.d, "base64url"),
    rawPublic,
  ]);

  assert.doesNotThrow(() => validateSigningKeyPair(
    rawPublic.toString("base64"),
    goPrivate.toString("base64"),
  ));
});

test("validateSigningKeyPair rejects a public/private mismatch", () => {
  const privateKey = Buffer.alloc(64, 1);
  const publicKey = Buffer.alloc(32, 2);

  assert.throws(
    () => validateSigningKeyPair(publicKey.toString("base64"), privateKey.toString("base64")),
    /manifest signing key pair does not match/,
  );
});
