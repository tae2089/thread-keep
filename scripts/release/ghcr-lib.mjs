const STABLE_SEMVER = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
const OWNER = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/;
const PACKAGE = /^[A-Za-z0-9_.-]+$/;

function validateInputs({ owner, packages, token, version }) {
  if (!OWNER.test(owner || "") || !Array.isArray(packages) || packages.length === 0) {
    throw new Error("GHCR preflight owner or package list is invalid");
  }
  if (packages.some((name) => !PACKAGE.test(name)) || new Set(packages).size !== packages.length) {
    throw new Error("GHCR preflight package list is invalid");
  }
  if (!STABLE_SEMVER.test(version || "") || typeof token !== "string" || token.length === 0) {
    throw new Error("GHCR preflight version or token is invalid");
  }
}

function packageTags(payload, packageName) {
  return payload.flatMap((item) => {
    const tags = item?.metadata?.container?.tags;
    if (!Array.isArray(tags) || tags.some((tag) => typeof tag !== "string")) {
      throw new Error(`GHCR preflight response for ${packageName} is invalid`);
    }
    return tags;
  });
}

async function fetchPage(fetchImpl, url, token, subject) {
  const response = await fetchImpl(url.toString(), {
    headers: {
      accept: "application/vnd.github+json",
      authorization: `Bearer ${token}`,
      "x-github-api-version": "2022-11-28",
    },
    signal: AbortSignal.timeout(30_000),
  });
  if (!response.ok) {
    throw new Error(`GHCR preflight request for ${subject} failed with HTTP ${response.status}`);
  }
  const payload = await response.json();
  if (!Array.isArray(payload)) {
    throw new Error(`GHCR preflight response for ${subject} is invalid`);
  }
  return payload;
}

async function accessiblePackageNames(fetchImpl, owner, token) {
  const names = new Set();
  for (let page = 1; ; page += 1) {
    const url = new URL(`https://api.github.com/users/${encodeURIComponent(owner)}/packages`);
    url.searchParams.set("package_type", "container");
    url.searchParams.set("per_page", "100");
    url.searchParams.set("page", String(page));
    const payload = await fetchPage(fetchImpl, url, token, `${owner} package namespace`);
    for (const item of payload) {
      if (typeof item?.name !== "string" || item.package_type !== "container" || item.owner?.login?.toLowerCase() !== owner.toLowerCase()) {
        throw new Error(`GHCR preflight response for ${owner} package namespace is invalid`);
      }
      names.add(item.name);
    }
    if (payload.length < 100) {
      return names;
    }
  }
}

export async function verifyVersionTagsAbsent({
  fetchImpl = fetch,
  owner,
  packages,
  token,
  version,
}) {
  validateInputs({ owner, packages, token, version });
  const accessible = await accessiblePackageNames(fetchImpl, owner, token);
  for (const packageName of packages) {
    if (!accessible.has(packageName)) {
      continue;
    }
    for (let page = 1; ; page += 1) {
      const url = new URL(`https://api.github.com/user/packages/container/${encodeURIComponent(packageName)}/versions`);
      url.searchParams.set("per_page", "100");
      url.searchParams.set("page", String(page));
      const payload = await fetchPage(fetchImpl, url, token, packageName);
      if (packageTags(payload, packageName).includes(version)) {
        throw new Error(`immutable GHCR tag ${packageName}:${version} already exists`);
      }
      if (payload.length < 100) {
        break;
      }
    }
  }
  return { checked: [...packages], version };
}
