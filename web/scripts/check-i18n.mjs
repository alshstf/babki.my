#!/usr/bin/env node
// Two checks, both about the same rule: what a reader sees is Russian, and it
// comes from ru.json.
//
// 1. KEYS. Every `t("...")` / `t(`...`)` key used in web/src actually exists in
//    src/i18n/ru.json. Also the handful of enum-driven dynamic keys
//    (t(`accountTypes.${x}`) and friends), by cross-checking the enum members
//    declared in the generated OpenAPI schema against the ru.json namespace, so
//    a missing translation for a valid backend enum value is caught even though
//    the key itself isn't a static string literal.
//
//    AND A t() CALL WHOSE KEY THIS SCRIPT CANNOT READ IS A FAILURE, not a
//    silence. It used to match only calls that OPEN with a quote, so
//    `t(cond ? "a" : "b")`, `t(key)` and `t("a" + b)` produced no finding of
//    any kind — no missing key, no warning — and the coverage of those keys
//    simply stopped being checked with nothing saying so. Four call sites
//    already carry a comment claiming literal keys are "the only shape
//    scripts/check-i18n.mjs can verify"; that was a convention four authors
//    kept by hand, and this is it enforced. The escape hatch for a genuinely
//    dynamic key is the `namespace.${expr}` shape above, which IS verified —
//    so the rule is "write the key in a shape that can be checked", never
//    "do not compute keys".
//
// 2. THE SERVER'S OWN WORDS. See findErrorTextInSource in ./i18n-rules.mjs —
//    key coverage says nothing about a string that reaches the markup without
//    a key at all, and that is exactly how English got onto four screens (#95).
//
// THE RULES THEMSELVES LIVE IN ./i18n-rules.mjs, which is where their tests can
// reach them (#113). This file is the runner and nothing else: it walks the
// tree, reads the three inputs, prints what came back and picks the exit code.
// Everything that can be WRONG about a rule is over there and under test.
//
// Usage: node scripts/check-i18n.mjs   (run from web/, or via `npm run i18n:check`)

import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative, dirname } from "node:path";
import { fileURLToPath } from "node:url";

import { flatten, checkKeysInSource, findErrorTextInSource } from "./i18n-rules.mjs";

const webRoot = dirname(dirname(fileURLToPath(import.meta.url)));
const srcRoot = join(webRoot, "src");
const ruJsonPath = join(srcRoot, "i18n", "ru.json");
const schemaPath = join(srcRoot, "api", "schema.d.ts");

/** Recursively collect .ts/.tsx files under `dir`, skipping *.test.* files. */
function collectSourceFiles(dir) {
  const out = [];
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    const st = statSync(full);
    if (st.isDirectory()) {
      out.push(...collectSourceFiles(full));
    } else if (/\.(ts|tsx)$/.test(entry) && !/\.test\.(ts|tsx)$/.test(entry)) {
      out.push(full);
    }
  }
  return out;
}

function main() {
  const ru = JSON.parse(readFileSync(ruJsonPath, "utf8"));
  const leafKeys = new Set();
  const namespaces = new Set();
  flatten(ru, "", leafKeys, namespaces);

  const schemaSrc = readFileSync(schemaPath, "utf8");
  const files = collectSourceFiles(srcRoot);

  const missing = [];
  const unverifiable = [];
  const foreignText = [];

  for (const file of files) {
    const relPath = relative(webRoot, file);
    const src = readFileSync(file, "utf8");
    const found = checkKeysInSource(relPath, src, { leafKeys, namespaces, schemaSrc });
    missing.push(...found.missing);
    unverifiable.push(...found.unverifiable);
    // The rule is about MARKUP: a hook in a .ts file may still build a message
    // out of the server's body, because it ends up in the console.
    if (file.endsWith(".tsx")) {
      foreignText.push(...findErrorTextInSource(relPath, src));
    }
  }

  if (unverifiable.length) {
    console.error(
      `i18n:check failed — ${unverifiable.length} t() call(s) whose key cannot be checked:`,
    );
    for (const line of unverifiable) console.error(`  ${line}`);
    process.exit(1);
  }

  if (missing.length) {
    console.error(`i18n:check failed — ${missing.length} missing key(s):`);
    for (const line of missing) console.error(`  ${line}`);
    process.exit(1);
  }

  if (foreignText.length) {
    console.error(`i18n:check failed — ${foreignText.length} place(s) reading the server's own words:`);
    for (const line of foreignText) console.error(`  ${line}`);
    process.exit(1);
  }

  console.log(
    `i18n:check OK — every t() key across ${files.length} files is one this script could read and ru.json holds, and no .tsx reads an error's message.`,
  );
}

main();
