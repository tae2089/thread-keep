#!/usr/bin/env node
import { TARGETS, stageTargetArtifacts } from "./release-lib.mjs";

const values = Object.fromEntries(process.argv.slice(2).map((argument) => {
  const index = argument.indexOf("=");
  if (index < 3 || !argument.startsWith("--")) {
    throw new Error(`expected --name=value, got ${argument}`);
  }
  return [argument.slice(2, index), argument.slice(index + 1)];
}));
const target = TARGETS.find((candidate) => candidate.id === values.target);
await stageTargetArtifacts({ distDir: values.dist, outDir: values.out, target });
