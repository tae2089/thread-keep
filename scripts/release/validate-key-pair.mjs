#!/usr/bin/env node
import { validateSigningKeyPair } from "./release-lib.mjs";

validateSigningKeyPair(
  process.env.THREAD_KEEP_MANIFEST_PUBLIC_KEY_B64,
  process.env.THREAD_KEEP_MANIFEST_PRIVATE_KEY_B64,
);
