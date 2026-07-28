#!/usr/bin/env node
// Verifies that every `t("...")` / `t(`...`)` key used in web/src actually
// exists in src/i18n/ru.json. Also verifies the handful of enum-driven
// dynamic keys (t(`accountTypes.${x}`) and friends) by cross-checking the
// enum members declared in the generated OpenAPI schema against the ru.json
// namespace, so a missing translation for a valid backend enum value is
// caught even though the key itself isn't a static string literal.
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
};

function extractEnumMembers(schemaSrc, typeName) {
  const re = new RegExp(`\\b${typeName}:\\s*((?:"[^"]*"\\s*\\|?\\s*)+);`);
  const match = schemaSrc.match(re);
  if (!match) return null;
  return [...match[1].matchAll(/"([^"]*)"/g)].map((m) => m[1]);
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

  console.log(`i18n:check OK — verified keys across ${files.length} files against ru.json.`);
}

main();
