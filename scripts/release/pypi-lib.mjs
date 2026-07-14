import { createHash } from "node:crypto";
import { readFile, readdir } from "node:fs/promises";
import path from "node:path";


const STABLE_SEMVER = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
const DISTRIBUTION = /^[A-Za-z0-9_.-]+$/;
const SHA256 = /^[a-f0-9]{64}$/;


async function localWheels(directory) {
  const entries = await readdir(directory, { withFileTypes: true });
  const wheels = new Map();
  for (const entry of entries) {
    if (!entry.isFile() || !entry.name.endsWith(".whl")) {
      continue;
    }
    const contents = await readFile(path.join(directory, entry.name));
    wheels.set(entry.name, createHash("sha256").update(contents).digest("hex"));
  }
  if (wheels.size === 0) {
    throw new Error("PyPI preflight found no local wheels");
  }
  return wheels;
}

export async function verifyPublishedWheels({
  distribution,
  fetchImpl = fetch,
  version,
  wheelDir,
}) {
  if (!DISTRIBUTION.test(distribution) || !STABLE_SEMVER.test(version)) {
    throw new Error("PyPI preflight distribution or version is invalid");
  }
  const local = await localWheels(wheelDir);
  const response = await fetchImpl(
    `https://pypi.org/pypi/${encodeURIComponent(distribution)}/${encodeURIComponent(version)}/json`,
    { headers: { accept: "application/json" }, signal: AbortSignal.timeout(30_000) },
  );
  if (response.status === 404) {
    return { existing: [], missing: [...local.keys()].sort() };
  }
  if (!response.ok) {
    throw new Error(`PyPI preflight request failed with HTTP ${response.status}`);
  }
  const payload = await response.json();
  if (!payload || !Array.isArray(payload.urls)) {
    throw new Error("PyPI preflight response is invalid");
  }
  const remote = new Map();
  for (const artifact of payload.urls) {
    if (artifact?.packagetype !== "bdist_wheel") {
      continue;
    }
    const filename = artifact.filename;
    const digest = artifact.digests?.sha256;
    if (typeof filename !== "string" || !SHA256.test(digest || "") || remote.has(filename)) {
      throw new Error("PyPI preflight response contains invalid wheel metadata");
    }
    remote.set(filename, digest);
  }
  for (const [filename, digest] of remote) {
    if (!local.has(filename)) {
      throw new Error(`unexpected published PyPI wheel: ${filename}`);
    }
    if (local.get(filename) !== digest) {
      throw new Error(`published PyPI digest differs for ${filename}`);
    }
  }
  return {
    existing: [...remote.keys()].sort(),
    missing: [...local.keys()].filter((filename) => !remote.has(filename)).sort(),
  };
}
