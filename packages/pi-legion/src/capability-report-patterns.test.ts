import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { analyzerRows } from "@legion/envoy-client/test/pattern-analysis";
import {
  CLEAN_DETAIL_PATTERNS,
  CREDENTIAL_RULES,
  cleanDetail,
  trailingSpaceTrimCharacters,
} from "./capability-report";

/** The four rules whose analysis had no verdict in the 300 s each was given: INFRA-3149's
 * architect accepted that risk on 2026-11-14 and keeps it under "Not proven / risk" of the pull
 * request. What holds them instead is the count of the characters each rule's pass is handed
 * (`credentialRuleCharacters`, the 64 KiB rows of `capability-report.test.ts`), which an extra
 * pass exceeds and a pass that backtracks within itself does not. */
const WITHOUT_VERDICT = ["github-token", "prefixed-token", "google-api-key", "aws-access-key-id"];

// Every pattern `cleanDetail` runs reads a detail an upstream error chose, before the cut to
// 1,024 bytes, so each must be linear in that detail.
analyzerRows(
  CLEAN_DETAIL_PATTERNS,
  path.join(import.meta.dir, "capability-report-patterns.recheck.json"),
  WITHOUT_VERDICT
);

/** `HELD_SEQUENCE` and `JWT_FIRST_HELD_SEQUENCE` of capability-report.ts, for the two forms below. */
const HELD_SEQUENCE = String.raw`\ufffe|\ufdd0|\uffff(?:\[[0-?]*[@-~])?`;
const JWT_FIRST_HELD_SEQUENCE = String.raw`\ufffe|\uffff(?:\[[0-?]*[@-~])?`;

/** The two rules as they were before they read the text before a start in code, each with a
 * look-around recheck's automaton checker cannot read. Only the rows below run them, to hold the
 * rule that replaced each to the same output. */
const LOOK_AROUND_USERINFO = /(?<![a-z0-9+.-])([0-9+.-]*[a-z][a-z0-9+.-]*:\/\/)[^ /?#]+@/gi;
const LOOK_AROUND_JWT = new RegExp(
  String.raw`(?:(?=eyJ)(?<!\uffff\[[0-?]*(?:[@-~]?|\\u[0-9A-Fa-f]{4}))(?:(?<![A-Za-z0-9_\ufffe\uffff-])|(?<=%[0-9A-Fa-f]{2}|\\u[0-9A-Fa-f]{4}))eyJ(?:[A-Za-z0-9_-]|${JWT_FIRST_HELD_SEQUENCE}){8,}|(?<=\uffff)eyJ(?:[A-Za-z0-9_-]|\ufffe){8,}|(?=eyJ)(?<=\uffff\[[0-?]*(?:[@-~]|\\u[0-9A-Fa-f]{4})?)eyJ[A-Za-z0-9_-]*\ufffe(?:[A-Za-z0-9_-]|\ufffe)*)\.(?:[A-Za-z0-9_-]|${HELD_SEQUENCE}){8,}\.(?:[A-Za-z0-9_-]|${HELD_SEQUENCE})*`,
  "g"
);

interface RedactionFixture {
  readonly patterns: readonly { readonly detail: string; readonly want: string }[];
  readonly branches: readonly { readonly upstream: string; readonly redacted: string }[];
  readonly linear: readonly { readonly unit: string }[];
}

/** Every text `capability-report.test.ts`'s rows clean: the shared fixture's details and what
 * each reads cleaned, each branch's upstream text and its redaction, and each linear unit repeated
 * to 64 KiB. */
function rowTexts(): string[] {
  const fixtures = path.resolve(import.meta.dir, "../../contracts/fixtures");
  const redaction = JSON.parse(
    readFileSync(path.join(fixtures, "capability-redaction.json"), "utf8")
  ) as RedactionFixture;
  return [
    ...redaction.patterns.flatMap(({ detail, want }) => [detail, want]),
    ...redaction.branches.flatMap(({ upstream, redacted }) => [upstream, redacted]),
    ...redaction.linear.map(({ unit }) => unit.repeat(Math.ceil((64 * 1024) / unit.length))),
  ];
}

/** `text` in the alphabet the rules read: U+009B as the C1 introducer and every other control
 * character held, as `cleanDetail` marks a detail before its rules run. */
function held(text: string): string {
  let marked = "";
  for (const char of text) {
    const code = char.charCodeAt(0);
    const control = code < 0x20 || (code >= 0x7f && code < 0xa0);
    marked += code === 0x9b ? "\ufffe" : control ? "\uffff" : char;
  }
  return marked;
}

/** `body` at the start of a line, between words, at its end, twice over and glued to itself,
 * each right after `before`. */
function placed(before: string, body: string): string[] {
  return [
    `${before}${body} after`,
    `words ${before}${body} after`,
    `words ${before}${body}`,
    `${before}${body} ${before}${body}`,
    `${before}${body}${before}${body}`,
  ];
}

/** URLs with a userinfo, each right after every kind of character the look-behind tested: each
 * scheme character, upper and lower case, and characters that are none. */
function userinfoTexts(): string[] {
  const urls = [
    "https://user:secret@host/path",
    "HTTPS://USER@HOST",
    "a1+.-b://u@h",
    "0http://x@y",
    "+.-://u@h",
    "http://u@h/p@q",
    "ftp://u@h@i",
    "h://@",
    "://u@h",
    "http://u:p@h?x=1@y",
    "redis://:pw@cache:6379",
  ];
  const before = ["", "a", "Z", "0", "9", "+", ".", "-", " ", "@", "/", ":", "_", "(", '"', "="];
  const extra = ["\uffff", "\ufffe", "\ufdd0", "\uffff[", "\uffff[1m"];
  return [...before, ...extra].flatMap((prefix) => urls.flatMap((url) => placed(prefix, url)));
}

/** A JWT as one is made: each part base64url-encoded and the three joined by dots. */
function jwt(header: string, payload: string, signature: string): string {
  return [header, payload, signature]
    .map((part) => Buffer.from(part).toString("base64url"))
    .join(".");
}

/** JWTs and near misses, each right after every context the look-arounds tested: nothing, a value
 * character, a separator, a percent or `\u` escape whole and cut short, held, a C1 introducer, an
 * opener, and a held `[` with parameters and a final byte, a `\uXXXX` escape or neither. */
function jwtTexts(): string[] {
  const jwts = [
    jwt(`{"alg":"HS256"}`, `{"sub":"1234"}`, "signature"),
    "eyJabcdefgh.abcdefgh.",
    "eyJabcdefg.abcdefgh.x",
    "eyJabcdefgh.abcdefg.x",
    "eyJabcdefgh.abcdefgh",
    "eyJab\ufffecdefgh.abcdefgh.x",
    "eyJ\ufffe.abcdefgh.x",
    "eyJab\uffffcdefgh.abcdefgh.x",
    "eyJab\uffff[1mcdefgh.abcdefgh.x",
    "eyJab\uffff[1cdefgh.abcdefgh.x",
    "eyJabcdefgh.ab\ufdd0cdefgh.x\uffff[0my",
    "eyJabcdefgh.ab\uffff[31mcdefgh.x\ufffe",
    "eyJabcdefgh.abcdefgh.eyJabcdefgh.abcdefgh.x",
    "eyJeyJeyJabcdefgh.abcdefgh.x",
    "eyJabcdefgh..abcdefgh.x",
  ];
  const before = [
    "",
    "a",
    "_",
    "-",
    "9",
    " ",
    ":",
    '"',
    "%22",
    "%2",
    "%G2",
    "\\u0022",
    "\\u002",
    "\\uZZZZ",
    "\uffff",
    "\ufffe",
    "\ufdd0",
    "\uffff[",
    "\uffff[1",
    "\uffff[1;2",
    "\uffff[1m",
    "\uffff[5@",
    "\uffff[~",
    "\uffff[\\u0022",
    "\uffff[1\\u0022",
    "\uffff[12\\u00",
    "x\uffff[",
    "[1m",
    "\uffff\uffff[",
  ];
  return before.flatMap((prefix) => jwts.flatMap((jwt) => placed(prefix, jwt)));
}

describe("the rules that read the text before a start in code", () => {
  const texts = rowTexts();
  for (const { name, lookAround, replacement, generated } of [
    {
      name: "url-userinfo",
      lookAround: LOOK_AROUND_USERINFO,
      replacement: "$1***@",
      generated: userinfoTexts(),
    },
    { name: "jwt", lookAround: LOOK_AROUND_JWT, replacement: "***", generated: jwtTexts() },
  ]) {
    test(`${name} redacts what its look-around form redacted, in every row's detail and each start's context`, () => {
      const rule = CREDENTIAL_RULES.find((candidate) => candidate.name === name);
      if (rule === undefined) throw new Error(`capability-report.ts has no ${name} rule`);
      const inputs = [...texts, ...texts.map(held), ...generated];
      const parted = inputs
        .filter((text) => text.replace(lookAround, replacement) !== rule.redact(text))
        .map((text) => JSON.stringify(text.slice(0, 160)));
      expect(parted).toEqual([]);
      // The rule redacts in many of the texts and leaves many alone, so the two forms agree on
      // each, not only on leaving a text alone.
      const redacted = inputs.filter((text) => rule.redact(text) !== text).length;
      expect(redacted).toBeGreaterThan(inputs.length / 10);
      expect(inputs.length - redacted).toBeGreaterThan(inputs.length / 10);
      expect(inputs.length).toBeGreaterThan(500);
    });
  }
});

describe("the trailing-space trim", () => {
  /** The most of a detail the cut keeps, in UTF-8 bytes, and so in characters (capability-report.ts's
   * MAX_DETAIL_BYTES): the trim runs on what the cut kept. */
  const MAX_DETAIL_BYTES = 1024;

  test("is handed at most what the cut keeps, the attack on the pattern it replaced included", () => {
    // recheck's attack on `/ +$/`, the pattern the trim replaced, and a long detail whose cut ends
    // on a space.
    for (const [what, detail] of [
      ["recheck's attack on / +$/", `${" ".repeat(54_773)}\x00 `],
      ["64 KiB of words", "word ".repeat((64 * 1024) / 5)],
    ] as const) {
      const before = trailingSpaceTrimCharacters();
      const cleaned = cleanDetail(detail);
      const handed = trailingSpaceTrimCharacters() - before;
      expect(cleaned.endsWith(" ")).toBe(false);
      expect(
        handed,
        `cleaning ${what} handed the trim ${handed} characters, where at most ${MAX_DETAIL_BYTES} (what the cut keeps) are allowed`
      ).toBeLessThanOrEqual(MAX_DETAIL_BYTES);
    }
  });
});
