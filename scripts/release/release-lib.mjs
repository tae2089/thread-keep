import { createHash } from "node:crypto";
import {
  chmod,
  cp,
  mkdir,
  readFile,
  readdir,
  rm,
  stat,
  writeFile,
} from "node:fs/promises";
import path from "node:path";

const releaseConfig = JSON.parse(
  await readFile(new URL("./release-config.json", import.meta.url), "utf8"),
);

export const CORE_BINARIES = Object.freeze([...releaseConfig.core_binaries]);

export const PACKS = Object.freeze(
  releaseConfig.packs.map((pack) => Object.freeze({ ...pack })),
);

export const ALL_BINARIES = Object.freeze([
  ...CORE_BINARIES,
  ...PACKS.map((pack) => pack.id),
]);

export const TARGETS = Object.freeze(
  releaseConfig.targets.map((target) => Object.freeze({ ...target })),
);

export function validateSigningKeyPair(publicKeyBase64, privateKeyBase64) {
  const publicKey = Buffer.from(String(publicKeyBase64 || "").trim(), "base64");
  const privateKey = Buffer.from(String(privateKeyBase64 || "").trim(), "base64");
  if (publicKey.length !== 32) {
    throw new Error("manifest public key must be a base64 Ed25519 public key");
  }
  if (privateKey.length !== 64) {
    throw new Error("manifest private key must be a base64 Go Ed25519 private key");
  }
  if (!privateKey.subarray(32).equals(publicKey)) {
    throw new Error("manifest signing key pair does not match");
  }
}

function executableExtension(goos) {
  return goos === "windows" ? ".exe" : "";
}

function assetName(binary, target) {
  return `${binary}_${target.goos}_${target.goarch}${executableExtension(target.goos)}`;
}

function assertVersion(version, tag) {
  if (!/^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.test(version)) {
    throw new Error(`release version is not SemVer: ${version}`);
  }
  if (tag !== `v${version}`) {
    throw new Error(`release tag ${tag} does not match version ${version}`);
  }
}

async function findGoReleaserBinary(distDir, binary, target) {
  const prefix = `${binary}_${target.goos}_${target.goarch}`;
  const entries = await readdir(distDir, { withFileTypes: true }).catch(() => []);
  const directories = entries
    .filter((entry) => entry.isDirectory() && (entry.name === prefix || entry.name.startsWith(`${prefix}_`)))
    .map((entry) => entry.name)
    .sort();
  if (directories.length !== 1) {
    throw new Error(`missing GoReleaser artifact for ${binary} (${target.goos}/${target.goarch})`);
  }
  const source = path.join(distDir, directories[0], `${binary}${executableExtension(target.goos)}`);
  const info = await stat(source).catch(() => null);
  if (!info?.isFile()) {
    throw new Error(`missing GoReleaser artifact for ${binary}: ${source}`);
  }
  return source;
}

export async function stageTargetArtifacts({ distDir, outDir, target }) {
  if (!TARGETS.some((candidate) => candidate.id === target?.id)) {
    throw new Error(`unsupported release target: ${target?.id || "missing"}`);
  }
  const sources = [];
  for (const binary of ALL_BINARIES) {
    sources.push({ binary, source: await findGoReleaserBinary(distDir, binary, target) });
  }
  await rm(outDir, { recursive: true, force: true });
  await mkdir(outDir, { recursive: true });
  const staged = [];
  for (const item of sources) {
    const destination = path.join(outDir, assetName(item.binary, target));
    await cp(item.source, destination);
    if (target.goos !== "windows") {
      await chmod(destination, 0o755);
    }
    staged.push(destination);
  }
  return staged;
}

async function inspectArtifact(file) {
  const contents = await readFile(file);
  return {
    contents,
    sha256: createHash("sha256").update(contents).digest("hex"),
    size: contents.length,
  };
}

async function validateStagedArtifacts(artifactsDir) {
  const artifacts = new Map();
  for (const target of TARGETS) {
    for (const binary of ALL_BINARIES) {
      const name = assetName(binary, target);
      const file = path.join(artifactsDir, name);
      const info = await stat(file).catch(() => null);
      if (!info?.isFile()) {
        throw new Error(`missing staged artifact: ${name}`);
      }
      artifacts.set(name, await inspectArtifact(file));
    }
  }
  return artifacts;
}

async function writeJSON(file, value) {
  await writeFile(file, `${JSON.stringify(value, null, 2)}\n`);
}

export async function assembleRelease({
  artifactsDir,
  outDir,
  repository,
  tag,
  version,
}) {
  assertVersion(version, tag);
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repository)) {
    throw new Error(`invalid GitHub repository: ${repository}`);
  }
  const artifacts = await validateStagedArtifacts(artifactsDir);
  await rm(outDir, { recursive: true, force: true });
  const assetsDir = path.join(outDir, "assets");
  await mkdir(assetsDir, { recursive: true });

  const checksumLines = [];
  for (const name of [...artifacts.keys()].sort()) {
    await cp(path.join(artifactsDir, name), path.join(assetsDir, name));
    checksumLines.push(`${artifacts.get(name).sha256}  ${name}`);
  }
  await writeFile(path.join(assetsDir, "checksums.txt"), `${checksumLines.join("\n")}\n`);

  const payload = {
    schema_version: 1,
    packs: PACKS.map((pack) => ({
      id: pack.id,
      version,
      protocol_version: 1,
      assets: TARGETS.map((target) => {
        const name = assetName(pack.id, target);
        const artifact = artifacts.get(name);
        return {
          goos: target.goos,
          goarch: target.goarch,
          url: `https://github.com/${repository}/releases/download/${tag}/${name}`,
          size: artifact.size,
          sha256: artifact.sha256,
        };
      }),
    })),
  };
  await writeJSON(path.join(assetsDir, "thread-keep-indexers-manifest-v1.payload.json"), payload);
}
