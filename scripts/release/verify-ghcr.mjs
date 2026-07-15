#!/usr/bin/env node
import { verifyVersionTagsAbsent } from "./ghcr-lib.mjs";

const values = Object.fromEntries(process.argv.slice(2).map((argument) => {
  const index = argument.indexOf("=");
  if (index < 3 || !argument.startsWith("--")) {
    throw new Error(`expected --name=value, got ${argument}`);
  }
  return [argument.slice(2, index), argument.slice(index + 1)];
}));

const packages = (values.packages || "").split(",").filter(Boolean);
const result = await verifyVersionTagsAbsent({
  owner: values.owner,
  packages,
  token: process.env.GHCR_TOKEN,
  version: values.version,
});
console.log(JSON.stringify(result));
