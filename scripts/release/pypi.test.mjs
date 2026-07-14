import assert from "node:assert/strict";
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import { verifyPublishedWheels } from "./pypi-lib.mjs";


async function withWheels(runTest) {
  const root = await mkdtemp(path.join(os.tmpdir(), "thread-keep-pypi-"));
  const wheelDir = path.join(root, "wheels");
  await mkdir(wheelDir);
  const wheels = new Map([
    ["thread_keep-1.2.3-py3-none-manylinux_2_39_x86_64.whl", "linux"],
    ["thread_keep-1.2.3-py3-none-macosx_15_0_arm64.whl", "darwin"],
  ]);
  for (const [name, contents] of wheels) {
    await writeFile(path.join(wheelDir, name), contents);
  }
  try {
    await runTest(wheelDir, wheels);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

function response(status, body = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    async json() {
      return body;
    },
  };
}

test("PyPI preflight treats an unpublished version as entirely missing", async () => {
  await withWheels(async (wheelDir, wheels) => {
    const result = await verifyPublishedWheels({
      distribution: "thread-keep",
      fetchImpl: async () => response(404),
      version: "1.2.3",
      wheelDir,
    });

    assert.deepEqual(result.existing, []);
    assert.deepEqual(result.missing, [...wheels.keys()].sort());
  });
});

test("PyPI preflight accepts matching partial publication for recovery", async () => {
  await withWheels(async (wheelDir, wheels) => {
    const [published] = wheels;
    const digest = await import("node:crypto").then(({ createHash }) => createHash("sha256").update(published[1]).digest("hex"));
    const result = await verifyPublishedWheels({
      distribution: "thread-keep",
      fetchImpl: async () => response(200, {
        urls: [{ filename: published[0], packagetype: "bdist_wheel", digests: { sha256: digest } }],
      }),
      version: "1.2.3",
      wheelDir,
    });

    assert.deepEqual(result.existing, [published[0]]);
    assert.equal(result.missing.length, 1);
  });
});

test("PyPI preflight rejects a published wheel with different bytes", async () => {
  await withWheels(async (wheelDir, wheels) => {
    const [published] = wheels;
    await assert.rejects(
      verifyPublishedWheels({
        distribution: "thread-keep",
        fetchImpl: async () => response(200, {
          urls: [{ filename: published[0], packagetype: "bdist_wheel", digests: { sha256: "0".repeat(64) } }],
        }),
        version: "1.2.3",
        wheelDir,
      }),
      /published PyPI digest differs/,
    );
  });
});

test("PyPI preflight rejects an unexpected published wheel", async () => {
  await withWheels(async (wheelDir) => {
    await assert.rejects(
      verifyPublishedWheels({
        distribution: "thread-keep",
        fetchImpl: async () => response(200, {
          urls: [{ filename: "thread_keep-1.2.3-py3-none-win32.whl", packagetype: "bdist_wheel", digests: { sha256: "0".repeat(64) } }],
        }),
        version: "1.2.3",
        wheelDir,
      }),
      /unexpected published PyPI wheel/,
    );
  });
});
