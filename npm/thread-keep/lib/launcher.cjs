const path = require("node:path");
const { spawnSync } = require("node:child_process");

const COMMANDS = new Set([
  "thread-keep",
  "thread-keep-mcp",
]);

const TARGET_PACKAGES = new Map([
  ["darwin-arm64", "thread-keep-darwin-arm64"],
  ["darwin-x64", "thread-keep-darwin-x64"],
  ["linux-arm64", "thread-keep-linux-arm64"],
  ["linux-x64", "thread-keep-linux-x64"],
  ["win32-x64", "thread-keep-win32-x64"],
]);

function packageForTarget(platform, arch) {
  const target = `${platform}-${arch}`;
  const packageName = TARGET_PACKAGES.get(target);
  if (!packageName) {
    throw new Error(`unsupported platform: ${target}`);
  }
  return packageName;
}

function resolveExecutable(command, options = {}) {
  if (!COMMANDS.has(command)) {
    throw new Error(`unsupported thread-keep command: ${command}`);
  }
  const platform = options.platform || process.platform;
  const arch = options.arch || process.arch;
  const resolvePackage = options.resolvePackage || require.resolve;
  const packageName = packageForTarget(platform, arch);
  let packageJSON;
  try {
    packageJSON = resolvePackage(`${packageName}/package.json`);
  } catch (error) {
    throw new Error(
      `native package ${packageName} is not installed; reinstall thread-keep with optional dependencies enabled`,
      { cause: error },
    );
  }
  const extension = platform === "win32" ? ".exe" : "";
  return path.join(path.dirname(packageJSON), "bin", `${command}${extension}`);
}

function run(command, args, options = {}) {
  const executable = resolveExecutable(command, options);
  const spawn = options.spawn || spawnSync;
  const result = spawn(executable, args, {
    env: options.env || process.env,
    shell: false,
    stdio: "inherit",
  });
  if (result.error) {
    throw new Error(`failed to start ${command}: ${result.error.message}`, { cause: result.error });
  }
  if (result.signal) {
    throw new Error(`${command} terminated by signal ${result.signal}`);
  }
  return Number.isInteger(result.status) ? result.status : 1;
}

function main(command) {
  try {
    process.exitCode = run(command, process.argv.slice(2));
  } catch (error) {
    console.error(`thread-keep: ${error.message}`);
    process.exitCode = 1;
  }
}

module.exports = {
  main,
  packageForTarget,
  resolveExecutable,
  run,
};
