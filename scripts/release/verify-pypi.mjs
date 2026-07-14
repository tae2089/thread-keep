#!/usr/bin/env node
import { verifyPublishedWheels } from "./pypi-lib.mjs";


const values = Object.fromEntries(process.argv.slice(2).map((argument) => {
  const index = argument.indexOf("=");
  if (index < 3 || !argument.startsWith("--")) {
    throw new Error(`expected --name=value, got ${argument}`);
  }
  return [argument.slice(2, index), argument.slice(index + 1)];
}));

const result = await verifyPublishedWheels({
  distribution: values.distribution,
  version: values.version,
  wheelDir: values.wheels,
});
console.log(JSON.stringify(result));
