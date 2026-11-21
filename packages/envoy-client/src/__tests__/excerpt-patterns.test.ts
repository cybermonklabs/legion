import { describe, expect, test } from "bun:test";
import path from "node:path";
import { analyzerRows } from "../../test/pattern-analysis";
import { EXCERPT_PATTERNS, EXCERPT_SCAN_LIMIT } from "../excerpt";

// Each of `excerpt`'s patterns runs on text a gateway chose, on the host's event loop, so each must
// be linear in that text. `dispatch-http.test.ts` counts the characters each pass is handed, which
// an extra pass exceeds; these rows read each pattern itself, which a pattern that backtracks
// within one pass fails.
analyzerRows(EXCERPT_PATTERNS, path.join(import.meta.dir, "excerpt-patterns.recheck.json"));

/** The script and style pattern as it was before it spelled its two names out: one group and a
 * back-reference to it, which recheck's automaton checker cannot read. Only the row below runs
 * it, to hold the pattern that replaced it to the same output. */
const BACK_REFERENCE_BLOCK = /<(script|style)\b[^<>]*>(?:[\s\S]*?<\/\1\s*>|[\s\S]*$)/gi;

/** The most of a body an excerpt reads. */
const SCAN_LIMIT = EXCERPT_SCAN_LIMIT;

/** `unit` repeated to fill what an excerpt reads. */
function filled(unit: string): string {
  return unit.repeat(Math.ceil(SCAN_LIMIT / unit.length)).slice(0, SCAN_LIMIT);
}

/** The bodies `dispatch-http.test.ts`'s rows hand the excerpt, cut to what it reads. */
const ROW_BODIES = [
  "<html><head><style>body{margin:0}</style><script>var x=1;</script></head><body>" +
    "<h1>Error 522</h1><p>Connection timed out</p></body></html>",
  "<html><body><h1>401 Unauthorized</h1><pre>token=dsp_live_4f9a2c7e\r\n" +
    "Authorization: Basic dXNlcjpwYXNz\r\nX-Forwarded: Bearer other.jwt-value</pre></body></html>",
  "<p>token=dsp_live_4f9a2c7e</p>",
  "<p>\u00a0</p>",
  ...[
    "<",
    "<script",
    "<script x",
    "<a <b",
    "</script>",
    "<script></script ",
    "secret",
    "<script>secret",
    "secre",
  ].map(filled),
  filled("<script>"),
  `<style>${filled(`dsp_${"Zq9x".repeat(10)}`).slice(0, SCAN_LIMIT - 80)}</style>`,
  `<style>${"s".repeat(SCAN_LIMIT - 75)}</style>${"q".repeat(40)}`,
];

/** Every way the two forms could part: each name in three cases, with and without attributes and
 * a glued word; closed by its own name, by the other, by a near miss or not at all; alone, between
 * words, twice, and one inside the other. */
function generatedBodies(): string[] {
  const names = ["script", "SCRIPT", "Script", "style", "STYLE", "sTyLe"];
  const openers = names.flatMap((name) => [
    `<${name}>`,
    `<${name} type="text/javascript">`,
    `<${name}\n>`,
    `<${name}s>`,
    `<${name}-x>`,
    `<${name}`,
  ]);
  const closers = [
    "",
    "</script>",
    "</SCRIPT  >",
    "</script\n>",
    "</style>",
    "</Style\t>",
    "</scripts>",
    "</script x>",
    "</ script>",
    "</style",
  ];
  const bodies: string[] = [];
  for (const opener of openers) {
    for (const closer of closers) {
      const block = `${opener}var a = "<b>";${closer}`;
      bodies.push(
        block,
        `before ${block} after`,
        `${block}${block}`,
        `<p>${block}</p><script>x</script>`,
        `<style>${block}</style>`,
        `<script>${block}</script> tail`
      );
    }
  }
  return bodies;
}

describe("the script and style pattern", () => {
  test("drops what the form with a back-reference dropped, from every body a row builds and every block shape", () => {
    const block = EXCERPT_PATTERNS["script-or-style-block"];
    if (block === undefined) throw new Error("excerpt.ts has no script-or-style-block pattern");
    const bodies = [...ROW_BODIES, ...generatedBodies()];
    const parted = bodies.filter(
      (body) => body.replace(BACK_REFERENCE_BLOCK, " ") !== body.replace(block, " ")
    );
    expect(parted.map((body) => JSON.stringify(body.slice(0, 120)))).toEqual([]);
    // The pattern drops a block from most of the bodies, so the two forms agree on what each
    // drops, not only on leaving a body alone.
    const dropped = bodies.filter((body) => body.replace(block, " ") !== body);
    expect(dropped.length).toBeGreaterThan(bodies.length / 2);
    expect(bodies.length).toBeGreaterThan(700);
  });
});

/** The JWS pattern as it was before it took the character before a JWS into its first group: a
 * look-behind that starts a match only at the start of a run of base64url characters, which
 * recheck's automaton checker cannot read. Only the row below runs it, to hold the pattern that
 * replaced it to the same output. */
const LOOK_BEHIND_JWS = /(?<![A-Za-z0-9_-])eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*/g;

/** The JWS shapes the rows of this package and cipher-client's build, each with fixed characters
 * of the alphabets they draw from: a DPoP proof whose segments are fixture secrets
 * (`<prefix>-<letters>`), an RS256 subject of base64url segments, one of a hex segment and `sig`,
 * a token of one segment, and one of two. */
const ROW_TOKENS = [
  "eyJ0eXAiOiJkcG9wK2p3dCJ9.claims-qwertyuiopasdfgh.sig-zxcvbnmlkjhgfdsa",
  "eyJhbGciOiJSUzI1NiJ9.Zm9vYmFyYmF6cXV4LV9fLS0tMTIz.c2lnbmF0dXJlLV8tXzAxMjM0NTY3ODlhYmNkZWZn",
  "eyJhbGciOiJSUzI1NiJ9.0123456789abcdef01234567.sig",
  "eyJxyzABC012-_xyzABC012-_xyzABC012-_xyzABC012-_",
  "eyJabcDEF-_09.ghiJKL-_12",
  "eyJhbGciOiJub25lIn0.cGF5bG9hZA.c2ln",
];

/** Near misses of a JWS: a dot too few or too many, an empty segment, base64 padding and `+` or
 * `/` inside a segment, `eyJ` alone or cut, a lower or upper case near spelling, and two JWS
 * glued by a dot or by nothing. */
const NEAR_MISSES = [
  "eyJa.b",
  "eyJa.b.c.d",
  "eyJa.b.c.d.e",
  "eyJa..c",
  "eyJ.b.c",
  "eyJa.b.",
  "eyJa==.b=.c==",
  "eyJa.b==.c",
  "eyJa=.b.c",
  "eyJa+b.c.d",
  "eyJa.b+c.d",
  "eyJa.b.c+d",
  "eyJa/b.c.d",
  "eyJa.b.c/d",
  "eyJ",
  "eyJa",
  "eyJa.",
  "eyj.b.c",
  "EYJa.b.c",
  "eyJa.b.ceyJd.e.f",
  "eyJa.b.c.eyJd.e.f",
  "eyJa.b.c=eyJd.e.f",
];

/** Where a token stands: at the start and at the end of the text, between words, glued to a word
 * on either side or both, after each character the look-behind refused (every base64url class:
 * a lower and an upper letter, a digit, `-`, `_`; and `.`) and after each it accepted (white space
 * and punctuation, `=` and `(` among them, and a letter outside base64url), at the end of a longer
 * token (a longer base64url run, a longer JWS), and in each echo the excerpt's rows quote (a
 * header line, a JSON member and its list, escaped as JSON or HTML, the second value of a list,
 * an `Authorization` value). */
const PLACES: readonly ((token: string) => string)[] = [
  (token) => token,
  (token) => `${token} at the end`,
  (token) => `at the start ${token}`,
  (token) => `word${token}`,
  (token) => `${token}word`,
  (token) => `word${token}word`,
  (token) => `${token}.${token}`,
  (token) => `${token};${token}`,
  (token) => `${token}${token}`,
  (token) => `Zm9vYmFy-_0123${token}`,
  (token) => `eyJhbGciOiJSUzI1NiJ9.Zm9vYmFy.${token}`,
  (token) => `eyJhbGciOiJSUzI1NiJ9.${token}`,
  ...[" ", "\n", "\t", "\r", '"', "'", "`", "\\", "&", ";", "[", "]", "(", ")", "{", "}", ":"]
    .concat(["=", "+", "/", "*", "!", "?", "#", "%", "@", "|", "~", ",", "<", ">", "\u00a0"])
    .flatMap((mark) => [
      (token: string) => `x${mark}${token}`,
      (token: string) => `${token}${mark}x`,
    ]),
  ...["a", "Z", "0", "9", "_", "-", ".", "é"].flatMap((glue) => [
    (token: string) => `${glue}${token}`,
    (token: string) => `x${glue}${token}`,
  ]),
  (token) => `DPoP: ${token}`,
  (token) => `dpop:${token}`,
  (token) => `{"Dpop":"${token}"}`,
  (token) => `{ "DPOP" : "${token}" }`,
  (token) => `{"Dpop":["${token}"]}`,
  (token) => `{ "Dpop" : [ "${token}" ] }`,
  (token) => `{"Authorization":["DPoP ${token}"]}`,
  (token) => `{\\"Dpop\\":[\\"${token}\\"]}`,
  (token) => `&quot;Dpop&quot;:[&quot;${token}&quot;]`,
  (token) => `{"Dpop":["${token}","${token}"]}`,
  (token) => `Authorization: DPoP ${token}`,
];

/** Every body the two JWS forms are compared on: each row token and each near miss in each place,
 * and the hostile pages of `eyJ` at every fourth character the excerpt's rows build. */
function jwsBodies(): string[] {
  const bodies = [...ROW_TOKENS, ...NEAR_MISSES].flatMap((token) =>
    PLACES.map((place) => place(token))
  );
  bodies.push(filled("eyJ-"), `0${filled("eyJ-")}`, filled("eyJa.b."), filled(". eyJa.b.c"));
  return bodies;
}

describe("the JWS pattern", () => {
  test("redacts what the form with a look-behind redacted, from every JWS shape and the hostile page", () => {
    const jws = EXCERPT_PATTERNS["jws-value"];
    if (jws === undefined) throw new Error("excerpt.ts has no jws-value pattern");
    const bodies = jwsBodies();
    const parted = bodies.filter(
      (body) => body.replace(LOOK_BEHIND_JWS, "[r]") !== body.replace(jws, "$1[r]")
    );
    expect(parted.map((body) => JSON.stringify(body.slice(0, 120)))).toEqual([]);
    // The pattern redacts a JWS from most of the row tokens' bodies, so the two forms agree on
    // what each redacts, not only on leaving a body alone.
    const rowBodies = ROW_TOKENS.flatMap((token) => PLACES.map((place) => place(token)));
    const redacted = rowBodies.filter((body) => body.replace(jws, "$1[r]") !== body);
    expect(redacted.length).toBeGreaterThan(rowBodies.length / 2);
    expect(bodies.length).toBeGreaterThan(1_500);
  });
});
