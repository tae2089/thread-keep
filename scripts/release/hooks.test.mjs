import assert from "node:assert/strict";
import { access, chmod, mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const HOOK = path.resolve("examples/hooks/claude-code/draft.sh");

async function runHook({ dirty = false, freshnessFails = false, gitStatusFails = false, statusFails = false } = {}) {
  const root = await mkdtemp(path.join(os.tmpdir(), "thread-keep-hook-"));
  const bin = path.join(root, "bin");
  const marker = path.join(root, "claude-called");
  await mkdir(bin);
  await writeFile(
    path.join(bin, "thread-keep"),
    `#!/bin/sh\ncase "$*" in\n  "--json status") ${statusFails ? "exit 1" : "exit 0"} ;;\n  "--json search __thread_keep_freshness_probe__") ${freshnessFails ? "exit 5" : "exit 0"} ;;\n  *) exit 2 ;;\nesac\n`,
  );
  await writeFile(
    path.join(bin, "git"),
    `#!/bin/sh\ncase "$1 $2" in\n  "status --porcelain") ${gitStatusFails ? "exit 1" : dirty ? "printf '%s\\n' ' M changed.go'" : "exit 0"} ;;\n  *) exit 2 ;;\nesac\n`,
  );
  await writeFile(path.join(bin, "claude"), `#!/bin/sh\n: > '${marker}'\n`);
  await Promise.all(["thread-keep", "git", "claude"].map((name) => chmod(path.join(bin, name), 0o755)));
  try {
    const result = spawnSync("sh", [HOOK], {
      cwd: root,
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${bin}:${process.env.PATH}`,
        THREAD_KEEP_DRAFT_HEADLESS: "1",
        TMPDIR: root,
      },
    });
    let called = false;
    for (let attempt = 0; attempt < 50; attempt += 1) {
      try {
        await access(marker);
        called = true;
        break;
      } catch {
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    }
    return { called, result };
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

for (const [name, options] of [
  ["stale working set", { freshnessFails: true }],
  ["dirty worktree", { dirty: true }],
  ["unreadable git status", { gitStatusFails: true }],
  ["unavailable Thread Keep status", { statusFails: true }],
]) {
  test(`draft hook rejects ${name} before starting Claude`, async () => {
    const { called, result } = await runHook(options);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, "");
    assert.equal(called, false);
  });
}

test("draft hook starts Claude for a clean fresh working set", async () => {
  const { called, result } = await runHook();
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /headless draft pass started/);
  assert.equal(called, true);
});
