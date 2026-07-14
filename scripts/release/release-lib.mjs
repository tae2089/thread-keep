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
}
