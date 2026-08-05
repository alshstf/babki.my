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
  // T-Invest connections (see api/openapi.yaml, paths under /api/v1/tinvest).
  //
  // OF THE SIX BELOW, EXACTLY ONE IS CROSS-CHECKED TODAY: `connections.statuses`.
  // An entry in this map is not a check on its own — the enum comparison runs
  // only where a KEY IS BUILT DYNAMICALLY, at a t(`namespace.${expr}`) call
  // site, and connections have exactly one of those so far (the settings
  // screen's status badge). The other five are wired in ahead of the connection
  // screen a later task adds — the run log, the reconcile snapshot, the
  // mismatch list, the unparsed list — so that ru.json already carries Russian
  // for every member the server can send. Until that screen renders them
  // through the dynamic shape they assert NOTHING, and a new member added on
  // the server would not fail this build. Said plainly rather than left to
  // read as five guarantees this script does not give.
  "connections.statuses": "TinvestConnectionStatus",
  "connections.runStatuses": "TinvestSyncRunStatus",
  "connections.reconcileStatuses": "TinvestReconcileStatus",
  "connections.unparsedReasons": "TinvestUnparsedReason",
  "connections.triggers": "TinvestSyncTrigger",
  "connections.mismatchKinds": "TinvestReconcileMismatchKind",
};

/**
 * Returns a boolean array, one entry per character of `src` (which must
 * already be comment-blanked, so `//` and `/* ` inside a real string are not
 * mistaken for the start of a comment), true wherever that character sits
 * inside a string or template literal — the quote characters themselves
 * included.
 *
 * Used to keep `callRe` below from mistaking a `t(...)`-shaped run of text
 * that appears INSIDE somebody else's string literal for a real call: the
 * key-coverage rule is about code, and a string like
 * `"call it with t(\"positions.title\") please"` contains no call for that
 * rule to check. Before this existed, blankComments' deliberate choice to
 * leave literals untouched (needed so MESSAGE_READS below can still see
 * `err["message"]` spelled out) meant such a fixture failed the build citing
 * a call that was never there — the exact false positive this closes.
 */
function stringLiteralMask(src) {
  const mask = new Array(src.length).fill(false);
  let state = "code"; // "code" | '"' | "'" | "`"
  for (let i = 0; i < src.length; i++) {
    const c = src[i];
    if (state === "code") {
      if (c === '"' || c === "'" || c === "`") {
        state = c;
        mask[i] = true;
      }
    } else {
      mask[i] = true;
      if (c === "\\") {
        if (i + 1 < src.length) mask[i + 1] = true;
        i++;
      } else if (c === state) {
        state = "code";
      } else if (c === "\n" && state !== "`") {
        // Unterminated literal: do not let it swallow the rest of the file.
        state = "code";
        mask[i] = false;
      }
    }
  }
  return mask;
}

// The offending call, quoted back at its author so the report names something
// he can search for. Cut at the first ")" — a key never contains one, so for
// every shape this reports that is the end of the call — and otherwise at one
// line or 60 characters, whichever comes first, with an ellipsis to say the
// text was cut rather than that the call ends there.
function callExcerpt(src, index) {
  const line = src.slice(index, index + 60).split("\n")[0];
  const close = line.indexOf(")");
  return close >= 0 ? line.slice(0, close + 1) : `${line}…`;
}

function extractEnumMembers(schemaSrc, typeName) {
  const re = new RegExp(`\\b${typeName}:\\s*((?:"[^"]*"\\s*\\|?\\s*)+);`);
  const match = schemaSrc.match(re);
  if (!match) return null;
  return [...match[1].matchAll(/"([^"]*)"/g)].map((m) => m[1]);
}

/**
 * Blanks out the text of every comment, keeping the file's length and all of
 * its newlines so reported line numbers still line up.
 *
 * String and template literals are deliberately left alone: `err["message"]`
 * spells the read inside a string, and the rule below has to keep seeing it.
 */
function blankComments(src) {
  const out = src.split("");
  // "code", "line" and "block" are outside/inside comments; a quote character
  // means we are inside a literal of that kind and no comment can start there.
  let state = "code";
  for (let i = 0; i < src.length; i++) {
    const c = src[i];
    const next = src[i + 1];
    if (state === "code") {
      if (c === "/" && next === "/") {
        state = "line";
        out[i] = " ";
        out[i + 1] = " ";
        i++;
      } else if (c === "/" && next === "*") {
        state = "block";
        out[i] = " ";
        out[i + 1] = " ";
        i++;
      } else if (c === '"' || c === "'" || c === "`") {
        state = c;
      }
    } else if (state === "line") {
      if (c === "\n") state = "code";
      else out[i] = " ";
    } else if (state === "block") {
      if (c === "*" && next === "/") {
        state = "code";
        out[i] = " ";
        out[i + 1] = " ";
        i++;
      } else if (c !== "\n") {
        out[i] = " ";
      }
    } else if (c === "\\") {
      i++; // escaped character: whatever follows is literal, quote included
    } else if (c === state) {
      state = "code";
    } else if (c === "\n" && state !== "`") {
      state = "code"; // unterminated literal: do not swallow the rest of the file
    }
  }
  return out.join("");
}

// The three spellings of the same read. An Error's `message` is the one foreign
// string that kept getting past the key check, because it never needed a key:
// the API hooks build it out of the server's error body, which is English prose
// written for a log. Four dialogs printed it straight into a red panel, and two
// more picked their Russian sentence by searching it for an English phrase
// (#95). Rendering it is the defect itself; branching on it is the same
// dependency on wording the API never promised — what the contract promises is
// the status code (see api/openapi.yaml), and ApiError carries exactly that
// (api/operations.ts).
const MESSAGE_READS = [
  /\.message\b/g,
  /\[\s*(["'`])message\1\s*\]/g,
  // A binding, which is how the same read gets spelled without a dot:
  // `const { message } = err`. Restricted to const/let/var so that a component
  // whose own prop is called `message` — StartupNotice in router.tsx, holding a
  // t() string — is not accused of reading an error.
  /\b(?:const|let|var)\s*\{[^}]*\bmessage\b[^}]*\}/g,
];

// The rule: a .tsx file may not read `message` off anything. Hooks in .ts files
// may keep building one from the server's body — it ends up in the console,
// where English belongs.
//
// What this CANNOT see, said plainly rather than papered over:
//   - the same English reaching the screen without ever touching `.message`:
//     coercing the whole error to a string (`String(err)`, `` `${err}` ``)
//     runs Error.prototype.toString(), which is "Error: " followed by the
//     server's own sentence — the identical defect in a spelling MESSAGE_READS
//     does not match. Catching it reliably would mean knowing `err` names an
//     Error, and this script has no type information, only text; guessing
//     from the variable's name (`err`, `error`, `e`) would both miss a
//     renamed catch variable and flag ordinary values like `String(id)`,
//     which is exactly the false confidence this project has been burned by
//     before (see CLAUDE.md's "тихая неверность прячется в подписи"). Left as
//     a gap, not papered over with a heuristic that would claim more
//     precision than it has;
//   - an English literal typed straight into JSX;
//   - an error's text read in a .ts file, where reading it is allowed, and
//     handed to a component as a plain string: by then the .tsx holds a string
//     like any other and there is nothing left to match. Aliasing inside the
//     .tsx itself does NOT evade the rule — `const msg = err.message` carries
//     the read on its own line;
//   - a parameter destructuring, `function C({ message })`, left out on
//     purpose for the false positives it would cost (see MESSAGE_READS).
// Deciding "this string is not Russian" in general is not something a grep can
// do, and nothing was invented here to pretend otherwise. This closes the shape
// all six known cases had.
//
// Comments are blanked first: describing this rule must not violate it, and a
// file whose only mention of the read is the sentence explaining why it does
// not do it used to fail CI.
function checkErrorTextInMarkup(files) {
  const violations = [];
  for (const file of files) {
    if (!file.endsWith(".tsx")) continue;
    const relPath = relative(webRoot, file);
    const src = blankComments(readFileSync(file, "utf8"));
    for (const re of MESSAGE_READS) {
      for (const match of src.matchAll(re)) {
        const line = src.slice(0, match.index).split("\n").length;
        violations.push(
          `${relPath}:${line}: reads an error's message («${match[0].trim()}») — a component may not render or branch on the server's own words; use t() for what to say and the HTTP status (isConflict / ApiError) for what happened`,
        );
      }
    }
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
  // Every t() call, whatever its argument turns out to be — the two regexes
  // are split precisely so that "the argument is not a literal" is a case this
  // script reaches rather than a match it never makes.
  const callRe = /\bt\(\s*/g;
  // Matches t("literal"), t('literal') and t(`literal`) / t(`prefix.${expr}`),
  // anchored at the argument's first character.
  const literalRe = /^(`[^`]*`|"[^"]*"|'[^']*')/;

  const missing = [];
  const unverifiable = [];

  for (const file of files) {
    // Comments are not code, and a comment DESCRIBING an unverifiable call is
    // exactly what four files carry — the same reason checkErrorTextInMarkup
    // blanks them before applying its own rule. Blanking preserves length and
    // newlines, so both the keys and the line numbers below are unaffected;
    // verified against the real tree, where it changes none of the 320 keys
    // found and removes all nine comment-only matches.
    const src = blankComments(readFileSync(file, "utf8"));
    const mask = stringLiteralMask(src);
    const relPath = relative(webRoot, file);
    let m;
    while ((m = callRe.exec(src)) !== null) {
      if (mask[m.index]) {
        // This "t(" is text sitting inside someone else's string literal —
        // there is no call here for the key-coverage rule to check, and
        // reporting one would name a defect that does not exist.
        continue;
      }
      const line = src.slice(0, m.index).split("\n").length;
      const literal = literalRe.exec(src.slice(m.index + m[0].length));
      if (!literal) {
        // The key is computed, so nothing here can say whether ru.json has it.
        // Reported rather than skipped: an unchecked key is the state this
        // script exists to make impossible, and passing over it in silence
        // leaves the hole with nothing to find it by.
        unverifiable.push(
          `${relPath}:${line}: ${callExcerpt(src, m.index)} — the key is not a literal, so its coverage cannot be checked; use a literal key (a switch or an if over literals) or the verified t(\`namespace.\${expr}\`) shape`,
        );
        continue;
      }
      const raw = literal[1];
      const quote = raw[0];
      const inner = raw.slice(1, -1);

      // A literal glued to more text with "+" (t("a" + b), t("positions." +
      // name)) is a COMPUTED key wearing a literal's opening quote — the
      // regex above matches only the leading segment, so without this check
      // it either passed silently (when that segment happens to name a real
      // key, e.g. t("positions.title" + suffix)) or was reported as a
      // missing key for a fragment nobody meant as one (e.g. "positions."
      // from t("positions." + name)). Both are wrong for the same reason:
      // the key is not this literal, so neither "found" nor "missing" is a
      // question this script can answer, and it belongs with the other
      // unverifiable shapes instead.
      const afterLiteral = src.slice(m.index + m[0].length + raw.length).trimStart();
      if (afterLiteral.startsWith("+")) {
        unverifiable.push(
          `${relPath}:${line}: ${callExcerpt(src, m.index)} — the key is built with string concatenation, so its coverage cannot be checked; use a literal key or the verified t(\`namespace.\${expr}\`) shape`,
        );
        continue;
      }

      if (quote === "`" && inner.includes("${")) {
        // Dynamic key: only support the `namespace.${expr}` shape used in
        // this codebase (a static dotted prefix followed by one interpolation
        // and nothing else).
        const dynMatch = inner.match(/^([a-zA-Z0-9_.]*)\.\$\{[^}]*\}$/);
        if (!dynMatch) {
          // A template with an interpolation somewhere other than at the end
          // of a dotted prefix. Same standing as a non-literal argument above,
          // and reported in the same list: the key cannot be resolved here, so
          // nothing can say whether ru.json holds it. It used to be a warning,
          // which is what an unchecked key looks like when nobody reads the
          // build log.
          unverifiable.push(
            `${relPath}:${line}: t(${raw}) — only the t(\`namespace.\${expr}\`) shape can be resolved statically`,
          );
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

  const foreignText = checkErrorTextInMarkup(files);
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
