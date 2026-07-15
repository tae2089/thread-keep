import assert from "node:assert/strict";
import test from "node:test";

import { verifyVersionTagsAbsent } from "./ghcr-lib.mjs";

function response(status, body = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    async json() {
      return body;
    },
  };
}

function packageRecord(name) {
  return { name, package_type: "container", owner: { login: "tae2089" } };
}

test("GHCR preflight allows packages that are proven absent from the accessible namespace", async () => {
  const result = await verifyVersionTagsAbsent({
    fetchImpl: async () => response(200, []),
    owner: "tae2089",
    packages: ["thread-keep-server"],
    token: "test-token",
    version: "1.2.3",
  });

  assert.deepEqual(result, { checked: ["thread-keep-server"], version: "1.2.3" });
});

test("GHCR preflight rejects an existing immutable version tag", async () => {
  await assert.rejects(
    verifyVersionTagsAbsent({
      fetchImpl: async (url) => url.includes("/users/tae2089/packages?")
        ? response(200, [packageRecord("thread-keep-server")])
        : response(200, [{ metadata: { container: { tags: ["latest", "1.2.3"] } } }]),
      owner: "tae2089",
      packages: ["thread-keep-server"],
      token: "test-token",
      version: "1.2.3",
    }),
    /already exists/,
  );
});

test("GHCR preflight fails closed when the package namespace cannot be verified", async () => {
  await assert.rejects(
    verifyVersionTagsAbsent({
      fetchImpl: async () => response(404),
      owner: "tae2089",
      packages: ["thread-keep-server"],
      token: "test-token",
      version: "1.2.3",
    }),
    /HTTP 404/,
  );
});

test("GHCR preflight fails closed when accessible package versions cannot be verified", async () => {
  await assert.rejects(
    verifyVersionTagsAbsent({
      fetchImpl: async (url) => url.includes("/users/tae2089/packages?")
        ? response(200, [packageRecord("thread-keep-server")])
        : response(503),
      owner: "tae2089",
      packages: ["thread-keep-server"],
      token: "test-token",
      version: "1.2.3",
    }),
    /HTTP 503/,
  );
});

test("GHCR preflight follows full version pages before declaring a tag absent", async () => {
  const calls = [];
  const firstPage = Array.from({ length: 100 }, () => ({ metadata: { container: { tags: [] } } }));
  await assert.rejects(
    verifyVersionTagsAbsent({
      fetchImpl: async (url) => {
        calls.push(url);
        if (url.includes("/users/tae2089/packages?")) {
          return response(200, [packageRecord("thread-keep-server")]);
        }
        return calls.filter((value) => value.includes("/user/packages/container/")).length === 1
          ? response(200, firstPage)
          : response(200, [{ metadata: { container: { tags: ["1.2.3"] } } }]);
      },
      owner: "tae2089",
      packages: ["thread-keep-server"],
      token: "test-token",
      version: "1.2.3",
    }),
    /already exists/,
  );
  assert.equal(calls.length, 3);
  assert.match(calls[2], /page=2/);
});
