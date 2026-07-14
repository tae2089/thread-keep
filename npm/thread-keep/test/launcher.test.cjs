const assert = require("node:assert/strict");
const test = require("node:test");

const {
  packageForTarget,
  resolveExecutable,
  run,
} = require("../lib/launcher.cjs");

test("packageForTarget maps every supported npm target", () => {
  assert.equal(packageForTarget("darwin", "arm64"), "thread-keep-darwin-arm64");
  assert.equal(packageForTarget("darwin", "x64"), "thread-keep-darwin-x64");
  assert.equal(packageForTarget("linux", "arm64"), "thread-keep-linux-arm64");
  assert.equal(packageForTarget("linux", "x64"), "thread-keep-linux-x64");
  assert.equal(packageForTarget("win32", "x64"), "thread-keep-win32-x64");
});

test("packageForTarget rejects unsupported targets", () => {
  assert.throws(
    () => packageForTarget("win32", "arm64"),
    /unsupported platform: win32-arm64/,
  );
});

test("resolveExecutable selects a command from the matching package", () => {
  const executable = resolveExecutable("thread-keep-mcp", {
    platform: "darwin",
    arch: "arm64",
    resolvePackage: (specifier) => {
      assert.equal(specifier, "thread-keep-darwin-arm64/package.json");
      return "/tmp/node_modules/thread-keep-darwin-arm64/package.json";
    },
  });

  assert.equal(
    executable,
    "/tmp/node_modules/thread-keep-darwin-arm64/bin/thread-keep-mcp",
  );
});

test("resolveExecutable reports a missing optional package clearly", () => {
  assert.throws(
    () => resolveExecutable("thread-keep", {
      platform: "linux",
      arch: "x64",
      resolvePackage: () => {
        throw new Error("not found");
      },
    }),
    /native package thread-keep-linux-x64 is not installed/,
  );
});

test("resolveExecutable does not expose operational server binaries through npm", () => {
  assert.throws(
    () => resolveExecutable("thread-keep-server"),
    /unsupported thread-keep command/,
  );
});

test("run forwards arguments and returns the native exit status", () => {
  const calls = [];
  const status = run("thread-keep", ["status", "--json"], {
    platform: "linux",
    arch: "x64",
    resolvePackage: () => "/tmp/node_modules/thread-keep-linux-x64/package.json",
    spawn: (executable, args, options) => {
      calls.push({ executable, args, options });
      return { status: 7, signal: null, error: undefined };
    },
  });

  assert.equal(status, 7);
  assert.deepEqual(calls[0].args, ["status", "--json"]);
  assert.equal(
    calls[0].executable,
    "/tmp/node_modules/thread-keep-linux-x64/bin/thread-keep",
  );
  assert.equal(calls[0].options.stdio, "inherit");
  assert.equal(calls[0].options.shell, false);
});
