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
// 2. THE SERVER'S OWN WORDS. See checkErrorTextInMarkup below — key coverage
//    says nothing about a string that reaches the markup without a key at all,
//    and that is exactly how English got onto four screens (#95).
//
// Usage: node scripts/check-i18n.mjs   (run from web/, or via `npm run i18n:check`)

import { readFileSync, readdirSync, statSync } from "node:fs";
import { join, relative, dirname } from "node:path";
import { fileURLToPath } from "node:url";

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

/** Flatten ru.json into a Set of leaf key paths and a Set of namespace (object) paths. */
function flatten(obj, prefix, leafKeys, namespaces) {
  for (const [key, value] of Object.entries(obj)) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (value !== null && typeof value === "object" && !Array.isArray(value)) {
      namespaces.add(path);
      flatten(value, path, leafKeys, namespaces);
    } else {
      leafKeys.add(path);
    }
  }
}

// Maps the dynamic-key namespace prefix used in code to the OpenAPI enum
// type name it's populated from, so we can verify every enum member has a
// translation even though the call site never spells out the literal key.
const ENUM_NAMESPACE_TO_TYPE = {
  accountTypes: "AccountType",
  operationTypes: "OperationType",
  instrumentTypes: "InstrumentType",
  roles: "Role",
  // The ways this application's cost basis computation can fail to answer for
  // the owner's country. The server defines the set (one row per country in
  // internal/family/taxresidency.go) and sends codes, never prose; the
  // frontend must have Russian wording for every one of them or a country
  // will show a divergence with no explanation next to it. Cross-checking
  // against the generated enum is what makes adding a notice on the server a
  // build failure here rather than a silent blank.
  "costBasis.notices": "CostBasisNotice",
  // The country's own method and queue perimeter, shown in the notice's
  // tooltip. Same rule as the notices above: the server owns the set of
  // values, so every one of them has to have Russian wording here.
  "costBasis.methods": "CostBasisMethod",
  "costBasis.perimeters": "CostBasisPerimeter",
};

function extractEnumMembers(schemaSrc, typeName) {
  const re = new RegExp(`\\b${typeName}:\\s*((?:"[^"]*"\\s*\\|?\\s*)+);`);
  const match = schemaSrc.match(re);
  if (!match) return null;
  return [...match[1].matchAll(/"([^"]*)"/g)].map((m) => m[1]);
}

// An Error's `message` is the one foreign string that kept getting past the
// key check, because it never needed a key: the API hooks build it out of the
// server's error body, which is English prose written for a log. Four dialogs
// printed it straight into a red panel, and two more picked their Russian
// sentence by searching it for an English phrase (#95).
//
// The rule, therefore: a .tsx file may not touch `.message` at all. Rendering
// it is the defect itself; branching on it is the same dependency on wording
// the API never promised — what the contract promises is the status code (see
// api/openapi.yaml), and ApiError carries exactly that (api/operations.ts).
// Hooks in .ts files may keep building a message from the server's body: it
// ends up in the console, where English belongs.
//
// What this does NOT catch, said plainly rather than papered over: an English
// literal typed straight into JSX, or an error's text passed through a variable
// or a .ts helper and rendered from there. Deciding "this string is not
// Russian" in general is not something a grep can do, and nothing was invented
// here to pretend otherwise. This closes the shape all six known cases had.
function checkErrorTextInMarkup(files) {
  const violations = [];
  for (const file of files) {
    if (!file.endsWith(".tsx")) continue;
    const relPath = relative(webRoot, file);
    const lines = readFileSync(file, "utf8").split("\n");
    lines.forEach((line, index) => {
      if (/\.message\b/.test(line)) {
        violations.push(
          `${relPath}:${index + 1}: reads an error's .message — a component may not render or branch on the server's own words; use t() for what to say and the HTTP status (isConflict / ApiError) for what happened`,
        );
      }
    });
  }
  return violations;
}

function main() {
  const ru = JSON.parse(readFileSync(ruJsonPath, "utf8"));
  const leafKeys = new Set();
  const namespaces = new Set();
  flatten(ru, "", leafKeys, namespaces);

  const schemaSrc = readFileSync(schemaPath, "utf8");

  const files = collectSourceFiles(srcRoot);
  // Matches t("literal"), t('literal') and t(`literal`) / t(`prefix.${expr}`)
  const callRe = /\bt\(\s*(`[^`]*`|"[^"]*"|'[^']*')/g;

  const missing = [];
  const unresolved = [];

  for (const file of files) {
    const src = readFileSync(file, "utf8");
    const relPath = relative(webRoot, file);
    let m;
    while ((m = callRe.exec(src)) !== null) {
      const raw = m[1];
      const quote = raw[0];
      const inner = raw.slice(1, -1);

      if (quote === "`" && inner.includes("${")) {
        // Dynamic key: only support the `namespace.${expr}` shape used in
        // this codebase (a static dotted prefix followed by one interpolation
        // and nothing else).
        const dynMatch = inner.match(/^([a-zA-Z0-9_.]*)\.\$\{[^}]*\}$/);
        if (!dynMatch) {
          unresolved.push(`${relPath}: t(${raw}) — cannot statically verify this key`);
          continue;
        }
        const ns = dynMatch[1];
        if (!namespaces.has(ns)) {
          missing.push(`${relPath}: namespace "${ns}" (from t(${raw})) missing in ru.json`);
          continue;
        }
        const enumType = ENUM_NAMESPACE_TO_TYPE[ns];
        if (enumType) {
          const members = extractEnumMembers(schemaSrc, enumType);
          if (members) {
            for (const member of members) {
              if (!leafKeys.has(`${ns}.${member}`)) {
                missing.push(
                  `${relPath}: t(${raw}) — enum "${enumType}" member "${member}" has no ru.json key "${ns}.${member}"`,
                );
              }
            }
          }
        }
        continue;
      }

      // Static key.
      if (!leafKeys.has(inner)) {
        missing.push(`${relPath}: t(${raw}) — key "${inner}" missing in ru.json`);
      }
    }
  }

  if (unresolved.length) {
    console.warn("Could not statically verify the following t() calls (manual check needed):");
    for (const line of unresolved) console.warn(`  ${line}`);
    console.warn("");
  }

  if (missing.length) {
    console.error(`i18n:check failed — ${missing.length} missing key(s):`);
    for (const line of missing) console.error(`  ${line}`);
    process.exit(1);
  }

  const foreignText = checkErrorTextInMarkup(files);
  if (foreignText.length) {
    console.error(`i18n:check failed — ${foreignText.length} place(s) reading the server's own words:`);
    for (const line of foreignText) console.error(`  ${line}`);
    process.exit(1);
  }

  console.log(
    `i18n:check OK — verified keys across ${files.length} files against ru.json, and no .tsx reads an error's .message.`,
  );
}

main();
