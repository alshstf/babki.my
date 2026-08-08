import { describe, expect, it } from "vitest";

import {
  blankComments,
  callExcerpt,
  checkKeysInSource,
  extractEnumMembers,
  findErrorTextInSource,
  flatten,
  stringLiteralMask,
} from "./i18n-rules.mjs";

// #113. `npm run i18n:check` is the only thing standing between the interface
// and an English string on a screen, and its rules were covered by nothing.
//
// The concrete failure that prompted this: the script's comment claimed
// `t(cond ? "a" : "b")`, `t(key)` and `t("a" + b)` were all caught as
// unverifiable, and for the concatenation that was not true — the call either
// passed in silence or was reported under the wrong reason. Reintroduce it and
// the script still exits 0 with every other test green. So the cases below are
// not a sampler; they are one per shape the rule has to tell apart, and each
// asserts the REASON it reports, not merely that something was reported. A test
// that only counted findings would pass on a script that reported every call as
// missing.
//
// Fixtures are written as source text rather than read off the tree: a rule
// tested against the real tree is tested against whatever the tree happens to
// contain today, and the shapes that matter most are precisely the ones nobody
// has written yet.

/** ru.json as these tests use it, flattened the way the runner flattens it. */
function dictionary(obj) {
  const leafKeys = new Set();
  const namespaces = new Set();
  flatten(obj, "", leafKeys, namespaces);
  return { leafKeys, namespaces };
}

const RU = {
  positions: { title: "Позиции", empty: "Пусто" },
  accountTypes: { cash: "Наличные", brokerage: "Брокерский" },
};

/** A generated-schema stand-in holding one enum. */
const SCHEMA = `
    AccountType: "brokerage" | "checking" | "cash";
    Role: "owner" | "editor" | "viewer";
`;

function check(src, { ru = RU, schemaSrc = SCHEMA } = {}) {
  return checkKeysInSource("src/fixture.tsx", src, { ...dictionary(ru), schemaSrc });
}

describe("checkKeysInSource: keys it can read", () => {
  it("accepts a literal key ru.json holds", () => {
    const found = check(`const a = t("positions.title");`);
    expect(found).toEqual({ missing: [], unverifiable: [] });
  });

  it("accepts single quotes and backticks with no interpolation", () => {
    const found = check("const a = t('positions.title'); const b = t(`positions.empty`);");
    expect(found).toEqual({ missing: [], unverifiable: [] });
  });

  it("reports a literal key ru.json does not hold, naming the key", () => {
    const found = check(`const a = t("positions.missing");`);
    expect(found.unverifiable).toEqual([]);
    expect(found.missing).toHaveLength(1);
    expect(found.missing[0]).toContain(`key "positions.missing" missing in ru.json`);
  });
});

describe("checkKeysInSource: keys it cannot read are failures, not silence", () => {
  // The three shapes named in the script's own comment. Each must land in
  // `unverifiable`, and NOT in `missing`: "missing" would send the author
  // looking for a key nobody wrote, which is a wrong reason, not a wrong count.
  it("reports a bare identifier key", () => {
    const found = check(`const a = t(key);`);
    expect(found.missing).toEqual([]);
    expect(found.unverifiable).toHaveLength(1);
    expect(found.unverifiable[0]).toContain("the key is not a literal");
  });

  it("reports a ternary key", () => {
    const found = check(`const a = t(cond ? "positions.title" : "positions.empty");`);
    expect(found.missing).toEqual([]);
    expect(found.unverifiable).toHaveLength(1);
    expect(found.unverifiable[0]).toContain("the key is not a literal");
  });

  // THE REGRESSION #113 NAMES. A literal glued to more text with "+" opens with
  // a quote, so the literal regex matches its leading segment and the call
  // sails past every other rule. The two ways it went wrong are both covered:
  // here the segment is a real key (so it would pass in silence), and below the
  // segment is a fragment (so it would be reported as a missing key nobody
  // meant to write).
  it("reports concatenation whose leading segment is a real key", () => {
    const found = check(`const a = t("positions.title" + suffix);`);
    expect(found.missing).toEqual([]);
    expect(found.unverifiable).toHaveLength(1);
    expect(found.unverifiable[0]).toContain("built with string concatenation");
  });

  it("reports concatenation whose leading segment is a fragment, and not as a missing key", () => {
    const found = check(`const a = t("positions." + name);`);
    expect(found.missing).toEqual([]);
    expect(found.unverifiable).toHaveLength(1);
    expect(found.unverifiable[0]).toContain("built with string concatenation");
  });

  it("sees the concatenation through whitespace and a line break", () => {
    const found = check('const a = t("positions."\n  + name);');
    expect(found.missing).toEqual([]);
    expect(found.unverifiable[0]).toContain("built with string concatenation");
  });

  it("reports a template whose interpolation is not at the end of a dotted prefix", () => {
    const found = check("const a = t(`positions.${x}.title`);");
    expect(found.missing).toEqual([]);
    expect(found.unverifiable).toHaveLength(1);
    expect(found.unverifiable[0]).toContain(
      "only the t(`namespace.${expr}`) shape can be resolved statically",
    );
  });

  it("quotes the offending call back, cut at its closing paren", () => {
    const found = check(`const a = t(key); const b = 1;`);
    expect(found.unverifiable[0]).toContain("t(key)");
    expect(found.unverifiable[0]).not.toContain("const b");
  });

  it("names the line the call is on", () => {
    const found = check(`const a = 1;\nconst b = 2;\nconst c = t(key);`);
    expect(found.unverifiable[0]).toContain("src/fixture.tsx:3:");
  });
});

describe("checkKeysInSource: the verified dynamic shape", () => {
  it("accepts t(`namespace.${expr}`) when every enum member has a key", () => {
    const found = check("const a = t(`accountTypes.${type}`);", {
      ru: { accountTypes: { brokerage: "Б", checking: "Р", cash: "Н" } },
    });
    expect(found).toEqual({ missing: [], unverifiable: [] });
  });

  it("reports the enum member ru.json has no wording for, by name", () => {
    // RU holds accountTypes.cash and accountTypes.brokerage; the schema's
    // AccountType also has "checking".
    const found = check("const a = t(`accountTypes.${type}`);");
    expect(found.unverifiable).toEqual([]);
    expect(found.missing).toHaveLength(1);
    expect(found.missing[0]).toContain(`member "checking" has no ru.json key "accountTypes.checking"`);
  });

  it("reports a namespace ru.json does not hold at all", () => {
    const found = check("const a = t(`operationTypes.${type}`);");
    expect(found.missing).toHaveLength(1);
    expect(found.missing[0]).toContain(`namespace "operationTypes"`);
  });

  it("accepts a namespace with no enum behind it, checking only that ru.json has it", () => {
    const found = check("const a = t(`positions.${x}`);");
    expect(found).toEqual({ missing: [], unverifiable: [] });
  });
});

describe("checkKeysInSource: text that is not code", () => {
  it("ignores a t(...) written inside somebody else's string literal", () => {
    const found = check(`const help = "call it with t(\\"positions.nope\\") please";`);
    expect(found).toEqual({ missing: [], unverifiable: [] });
  });

  it("ignores a t(...) written in a line comment", () => {
    const found = check(`// use t(key) here\nconst a = t("positions.title");`);
    expect(found).toEqual({ missing: [], unverifiable: [] });
  });

  it("ignores a t(...) written in a block comment", () => {
    const found = check(`/* t("positions.nope") */\nconst a = t("positions.title");`);
    expect(found).toEqual({ missing: [], unverifiable: [] });
  });

  it("does not mistake a longer identifier ending in t for a call", () => {
    const found = check(`const a = format(key); const b = commit(key);`);
    expect(found).toEqual({ missing: [], unverifiable: [] });
  });
});

describe("findErrorTextInSource", () => {
  it("reports a dotted read", () => {
    const found = findErrorTextInSource("src/d.tsx", `<p>{err.message}</p>`);
    expect(found).toHaveLength(1);
    expect(found[0]).toContain("reads an error's message");
  });

  it("reports a bracketed read", () => {
    expect(findErrorTextInSource("src/d.tsx", `const m = err["message"];`)).toHaveLength(1);
  });

  it("reports a const destructuring", () => {
    expect(findErrorTextInSource("src/d.tsx", `const { message } = err;`)).toHaveLength(1);
  });

  it("leaves a parameter destructuring alone, which is the false positive it declines to make", () => {
    expect(findErrorTextInSource("src/d.tsx", `function C({ message }) { return null; }`)).toEqual([]);
  });

  it("does not accuse a comment that merely describes the rule", () => {
    // The rule's own documentation used to fail CI. Blanking comments is what
    // fixed it, and this is the case that keeps it fixed.
    expect(
      findErrorTextInSource("src/d.tsx", `// never render err.message here\nreturn null;`),
    ).toEqual([]);
  });

  it("names the line", () => {
    const found = findErrorTextInSource("src/d.tsx", `const a = 1;\nconst b = err.message;`);
    expect(found[0]).toContain("src/d.tsx:2:");
  });
});

describe("blankComments", () => {
  it("keeps the file's length and its newlines, so line numbers still line up", () => {
    const src = `const a = 1; // gone\nconst b = 2;\n`;
    const out = blankComments(src);
    expect(out).toHaveLength(src.length);
    expect(out.split("\n")).toHaveLength(src.split("\n").length);
    expect(out).toContain("const a = 1;");
    expect(out).not.toContain("gone");
  });

  it("leaves string literals alone, which is what lets the message rule see err[\"message\"]", () => {
    const src = `const a = "http://not-a-comment";`;
    expect(blankComments(src)).toBe(src);
  });

  it("does not start a comment inside a template literal", () => {
    const src = "const a = `a // b`; const c = 1;";
    expect(blankComments(src)).toBe(src);
  });

  it("blanks a block comment without eating the code after it", () => {
    const out = blankComments(`/* x */const a = 1;`);
    expect(out).toContain("const a = 1;");
    expect(out).not.toContain("x");
  });

  it("does not let an unterminated quote swallow the rest of the file", () => {
    // An apostrophe in prose is the ordinary way this happens.
    const out = blankComments(`const a = 1; // it's fine\nconst b = 2; // gone too\n`);
    expect(out).toContain("const b = 2;");
    expect(out).not.toContain("gone too");
  });
});

describe("stringLiteralMask", () => {
  it("marks the quotes themselves and everything between them", () => {
    const src = `a"bc"d`;
    expect(stringLiteralMask(src)).toEqual([false, true, true, true, true, false]);
  });

  it("treats an escaped quote as part of the literal", () => {
    const src = `"a\\"b" x`;
    const mask = stringLiteralMask(src);
    // The trailing " x" is code: the escaped quote did not close the literal.
    expect(mask[src.length - 1]).toBe(false);
    expect(mask[src.length - 3]).toBe(true);
  });

  it("lets a template literal span lines but not a single-quoted one", () => {
    const backtick = "`a\nb` x";
    expect(stringLiteralMask(backtick)[backtick.indexOf("b")]).toBe(true);
    const single = "'a\nb' x";
    expect(stringLiteralMask(single)[single.indexOf("b")]).toBe(false);
  });
});

describe("extractEnumMembers", () => {
  it("reads every member of a union", () => {
    expect(extractEnumMembers(SCHEMA, "AccountType")).toEqual(["brokerage", "checking", "cash"]);
  });

  it("returns null for a type the schema does not declare", () => {
    // Null and not [] on purpose: the caller skips the cross-check entirely
    // rather than concluding the enum is empty and every key covered.
    expect(extractEnumMembers(SCHEMA, "NoSuchType")).toBeNull();
  });
});

describe("flatten", () => {
  it("separates leaf keys from namespaces", () => {
    const { leafKeys, namespaces } = dictionary({ a: { b: "x" }, c: "y" });
    expect([...leafKeys].sort()).toEqual(["a.b", "c"]);
    expect([...namespaces]).toEqual(["a"]);
  });

  it("treats an array as a leaf rather than a namespace", () => {
    const { leafKeys, namespaces } = dictionary({ a: ["x", "y"] });
    expect([...leafKeys]).toEqual(["a"]);
    expect([...namespaces]).toEqual([]);
  });
});

describe("callExcerpt", () => {
  it("cuts at the closing paren", () => {
    const src = `x = t(key); y = 2;`;
    expect(callExcerpt(src, src.indexOf("t(key)"))).toBe("t(key)");
  });

  it("marks a cut with an ellipsis when there is no closing paren in reach", () => {
    const src = `t(${"a".repeat(80)}`;
    expect(callExcerpt(src, 0).endsWith("…")).toBe(true);
  });
});
