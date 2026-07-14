#!/usr/bin/env node
import { assembleRelease } from "./release-lib.mjs";

const values = Object.fromEntries(process.argv.slice(2).map((argument) => {
  const index = argument.indexOf("=");
  if (index < 3 || !argument.startsWith("--")) {
    throw new Error(`expected --name=value, got ${argument}`);
  }
  return [argument.slice(2, index), argument.slice(index + 1)];
}));
await assembleRelease({
  artifactsDir: values.artifacts,
  licenseFile: values.license,
  metaTemplateDir: values.meta,
  outDir: values.out,
  repository: values.repository,
  tag: values.tag,
  version: values.version,
});
