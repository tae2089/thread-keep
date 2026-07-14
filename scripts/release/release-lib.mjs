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

export const CORE_BINARIES = Object.freeze([
  "thread-keep",
  "thread-keep-mcp",
  "thread-keep-server",
  "thread-keep-coordinator",
  "thread-keep-runner",
]);

export const PACKS = Object.freeze([
  { id: "thread-keep-index-typescript", language: "typescript" },
  { id: "thread-keep-index-javascript", language: "javascript" },
  { id: "thread-keep-index-python", language: "python" },
  { id: "thread-keep-index-java", language: "java" },
  { id: "thread-keep-index-kotlin", language: "kotlin" },
  { id: "thread-keep-index-rust", language: "rust" },
]);

export const ALL_BINARIES = Object.freeze([
  ...CORE_BINARIES,
  ...PACKS.map((pack) => pack.id),
]);

export const NPM_BINARIES = Object.freeze([
  "thread-keep",
  "thread-keep-mcp",
]);

export const TARGETS = Object.freeze([
  { id: "linux-x64", goos: "linux", goarch: "amd64", npmOS: "linux", npmCPU: "x64" },
  { id: "linux-arm64", goos: "linux", goarch: "arm64", npmOS: "linux", npmCPU: "arm64" },
  { id: "darwin-x64", goos: "darwin", goarch: "amd64", npmOS: "darwin", npmCPU: "x64" },
  { id: "darwin-arm64", goos: "darwin", goarch: "arm64", npmOS: "darwin", npmCPU: "arm64" },
  { id: "win32-x64", goos: "windows", goarch: "amd64", npmOS: "win32", npmCPU: "x64" },
]);

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

function packageName(target) {
  return `thread-keep-${target.npmOS}-${target.npmCPU}`;
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

function repositoryMetadata(repository) {
  return {
    type: "git",
    url: `git+https://github.com/${repository}.git`,
  };
}

async function writeJSON(file, value) {
  await writeFile(file, `${JSON.stringify(value, null, 2)}\n`);
}

async function createPlatformPackage({ artifactsDir, licenseFile, npmDir, repository, target, version }) {
  const name = packageName(target);
  const root = path.join(npmDir, name);
  const binDir = path.join(root, "bin");
  await mkdir(binDir, { recursive: true });
  const manifest = {
    name,
    version,
    description: `Thread Keep native binaries for ${target.npmOS}-${target.npmCPU}`,
    repository: repositoryMetadata(repository),
    license: "MIT",
    os: [target.npmOS],
    cpu: [target.npmCPU],
    files: ["bin", "LICENSE"],
    publishConfig: { access: "public", provenance: true },
  };
  if (target.goos === "linux") {
    manifest.libc = ["glibc"];
  }
  await writeJSON(path.join(root, "package.json"), manifest);
  await cp(licenseFile, path.join(root, "LICENSE"));
  for (const binary of NPM_BINARIES) {
    const extension = executableExtension(target.goos);
    const source = path.join(artifactsDir, assetName(binary, target));
    const destination = path.join(binDir, `${binary}${extension}`);
    await cp(source, destination);
    if (target.goos !== "windows") {
      await chmod(destination, 0o755);
    }
  }
}

async function createMetaPackage({ licenseFile, metaTemplateDir, npmDir, repository, version }) {
  const root = path.join(npmDir, "thread-keep");
  await cp(metaTemplateDir, root, { recursive: true });
  const manifestPath = path.join(root, "package.json");
  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  manifest.version = version;
  manifest.repository = repositoryMetadata(repository);
  manifest.license = "MIT";
  if (!manifest.files.includes("LICENSE")) {
    manifest.files.push("LICENSE");
  }
  manifest.optionalDependencies = Object.fromEntries(
    TARGETS.map((target) => [packageName(target), version]),
  );
  await writeJSON(manifestPath, manifest);
  await cp(licenseFile, path.join(root, "LICENSE"));
}

export async function assembleRelease({
  artifactsDir,
  licenseFile,
  metaTemplateDir,
  outDir,
  repository,
  tag,
  version,
}) {
  assertVersion(version, tag);
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repository)) {
    throw new Error(`invalid GitHub repository: ${repository}`);
  }
  const licenseInfo = await stat(licenseFile).catch(() => null);
  if (!licenseInfo?.isFile()) {
    throw new Error("release license file is missing");
  }
  const artifacts = await validateStagedArtifacts(artifactsDir);
  await rm(outDir, { recursive: true, force: true });
  const assetsDir = path.join(outDir, "assets");
  const npmDir = path.join(outDir, "npm");
  await mkdir(assetsDir, { recursive: true });
  await mkdir(npmDir, { recursive: true });

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

  for (const target of TARGETS) {
    await createPlatformPackage({ artifactsDir, licenseFile, npmDir, repository, target, version });
  }
  await createMetaPackage({ licenseFile, metaTemplateDir, npmDir, repository, version });
}
