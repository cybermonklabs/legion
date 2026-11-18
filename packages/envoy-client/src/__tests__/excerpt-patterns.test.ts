import { describe, expect, test } from "bun:test";
import path from "node:path";
import { analyzerRows } from "../../test/pattern-analysis";
import { EXCERPT_PATTERNS } from "../dispatch-http";

// Each of `excerpt`'s patterns runs on text a gateway chose, on the host's event loop, so each must
// be linear in that text. `dispatch-http.test.ts` counts the characters each pass is handed, which
// an extra pass exceeds; these rows read each pattern itself, which a pattern that backtracks
// within one pass fails.
analyzerRows(EXCERPT_PATTERNS, path.join(import.meta.dir, "excerpt-patterns.recheck.json"));

/** The script and style pattern as it was before it spelled its two names out: one group and a
 * back-reference to it, which recheck's automaton checker cannot read. Only the row below runs
 * it, to hold the pattern that replaced it to the same output. */
const BACK_REFERENCE_BLOCK = /<(script|style)\b[^<>]*>(?:[\s\S]*?<\/\1\s*>|[\s\S]*$)/gi;

/** The most of a body an excerpt reads (dispatch-http.ts's EXCERPT_SCAN_LIMIT). */
const SCAN_LIMIT = 64 * 1024;

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
    if (block === undefined)
      throw new Error("dispatch-http.ts has no script-or-style-block pattern");
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
