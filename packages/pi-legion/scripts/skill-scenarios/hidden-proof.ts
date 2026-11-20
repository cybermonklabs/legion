// hidden-proof.ts: the proof that each run of a -hidden label had Dispatch's computed state hidden
// from its agent (rig.sh's header, hide_state_on).
//
//   bun hidden-proof.ts <work>/maps [<scenario>]      (rig.sh hidden-proof [<scenario>])
//
// Each run of the map (<maps>/runs/<name>) whose label ends in -hidden gets two checks:
//   - quiet: the rig's own reads of the run's results (rig.sh quiet_reads, <maps>/quiet/<id>.*),
//     just before and just after the switch went on. The route's answer after must equal the answer
//     before with each check's pass_stands and ci and each result's currency deleted, and nothing
//     else changed; the label's `dispatch results` after, and the same with --json, must hold no
//     state line and no such field. The answer before must hold a pass_stands and a currency, and
//     the text before a state line: without them the comparison could not fail, and the run is
//     UNKNOWN.
//   - transcripts: every tool result and background task report in every transcript of the run (its
//     agent's and each subagent's, at any depth: <run>/sessions/**/*.jsonl) must hold no state
//     line. A run whose transcripts hold no results answer is UNKNOWN: it saw nothing to hide.
// Each run of another label of a heads scenario gets the transcript scan alone, the control: an
// agent that read the results with the state shown has state lines in its transcripts.
//
// One row per run, then the counts. Exit 0 when the map holds a run of a -hidden label and each one
// reads `hidden`; exit 1 for a FAULT, a LEAK, an UNKNOWN, or no such run (the proof saw nothing);
// exit 2 for a usage error.
import { existsSync, readdirSync, readFileSync, realpathSync } from "node:fs";
import path from "node:path";
import { HEADS_SCENARIOS } from "./heads-scenarios";
import { parseSession, toolResults } from "./transcript";

/** A line that states what Dispatch computes from the results: the check's answer, the count of
 * heads that read yes, the line of a head, the pull request line under it, or the fields that hold
 * them in the JSON answer. */
const STATE_LINE = [
  /A pass stands: (yes|no|not known)/,
  /\b\d+ of \d+ heads read yes/,
  /- #\d+ at [0-9a-f]{40}: (yes|no|not known)/,
  /Pull request line: /,
  /"(pass_stands|subjects|currency)"\s*:/,
  /"ci"\s*:\s*\{/,
];
/** A line of a skill's template, whose `<word>` stands for a value: it states no state. */
const PLACEHOLDER = /<[a-z][a-z0-9 _-]*>/i;
/** A results answer: the text `dispatch results` prints, or the JSON of the route. */
const READ = [/Checks of [A-Z][A-Z0-9]*-\d+:/, /"verification_artifact_id"/];
/** A tool call that reaches the results route other than through `dispatch`. */
const API_CALL =
  /(curl|wget|fetch|python|urllib|http\.client)[^\n]*\/api\/v1\/issues\/[^\s/"']+\/results\b/;

export function stateLines(text: string): string[] {
  return text
    .split("\n")
    .filter((line) => !PLACEHOLDER.test(line) && STATE_LINE.some((pattern) => pattern.test(line)));
}

export interface Quiet {
  readonly verdict: "hidden" | "FAULT" | "UNKNOWN";
  readonly detail: string;
}

type Json = null | boolean | number | string | Json[] | { [key: string]: Json };

/** `value` with its object keys sorted at every depth, as one string. */
function canonical(value: Json): string {
  return JSON.stringify(value, (_key, inner: Json) =>
    inner !== null && typeof inner === "object" && !Array.isArray(inner)
      ? Object.fromEntries(Object.entries(inner).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0)))
      : inner
  );
}

/** The results answer `answer` with each check's pass_stands and ci and each result's currency
 * deleted, and how many of each it held. */
function withoutState(answer: Json): { rest: Json; passStands: number; currency: number } {
  const rest = structuredClone(answer);
  let passStands = 0;
  let currency = 0;
  const checks =
    rest !== null && typeof rest === "object" && !Array.isArray(rest) ? rest.checks : [];
  for (const check of Array.isArray(checks) ? checks : []) {
    if (check === null || typeof check !== "object" || Array.isArray(check)) continue;
    if ("pass_stands" in check) passStands++;
    delete check.pass_stands;
    delete check.ci;
    for (const key of ["results", "recorded_results"]) {
      const list = check[key];
      for (const result of Array.isArray(list) ? list : []) {
        if (result === null || typeof result !== "object" || Array.isArray(result)) continue;
        if ("currency" in result) currency++;
        delete result.currency;
      }
    }
  }
  return { rest, passStands, currency };
}

/** The quiet check of the reads whose files start `base` (<maps>/quiet/<id>). */
export function quietCheck(base: string): Quiet {
  const read = (which: string) => {
    const file = `${base}.${which}`;
    return existsSync(file) ? readFileSync(file, "utf8") : "";
  };
  const files = [
    "before-curl",
    "after-curl",
    "before-text",
    "after-text",
    "before-json",
    "after-json",
  ];
  const missing = files.filter((which) => read(which).trim() === "");
  if (missing.length > 0)
    return { verdict: "UNKNOWN", detail: `no read ${missing.join(", ")} at ${base}` };
  let before: Json;
  let after: Json;
  try {
    before = JSON.parse(read("before-curl"));
    after = JSON.parse(read("after-curl"));
  } catch (error) {
    return {
      verdict: "UNKNOWN",
      detail: `a curl read is no JSON answer: ${(error as Error).message}`,
    };
  }
  const { rest, passStands, currency } = withoutState(before);
  if (passStands === 0 || currency === 0)
    return {
      verdict: "UNKNOWN",
      detail: `the answer before holds ${passStands} pass_stands and ${currency} currency, so the comparison could not fail`,
    };
  const textBefore = stateLines(read("before-text")).length;
  if (textBefore === 0)
    return {
      verdict: "UNKNOWN",
      detail: "the text before holds no state line, so the text check could not fail",
    };
  const faults: string[] = [];
  if (canonical(rest) !== canonical(after))
    faults.push("the answer after is not the answer before less pass_stands, ci and currency");
  const textAfter = stateLines(read("after-text"));
  if (textAfter.length > 0)
    faults.push(`the text after holds ${textAfter.length} state lines: ${textAfter[0]?.trim()}`);
  const jsonAfter = stateLines(read("after-json"));
  if (jsonAfter.length > 0)
    faults.push(
      `the --json read after holds ${jsonAfter.length} state fields: ${jsonAfter[0]?.trim()}`
    );
  return faults.length > 0
    ? { verdict: "FAULT", detail: faults.join("; ") }
    : {
        verdict: "hidden",
        detail: `answer before less ${passStands} pass_stands and ${currency} currency equals the answer after; ${textBefore} state lines before, 0 after`,
      };
}

export interface Scan {
  readonly transcripts: number;
  readonly reads: number;
  readonly lines: number;
  readonly apiCalls: number;
  readonly first: string;
  readonly verdict: "hidden" | "LEAK" | "UNKNOWN";
}

/** Each .jsonl under `dir`, at any depth. */
function transcriptFiles(dir: string): string[] {
  if (!existsSync(dir)) return [];
  return (readdirSync(dir, { recursive: true }) as string[])
    .filter((file) => file.endsWith(".jsonl"))
    .sort()
    .map((file) => path.join(dir, file));
}

/** The scan of every transcript of the run in `runDir`. A line that is no transcript record is the
 * rig's fault, and throws. */
export function scanTranscripts(runDir: string): Scan {
  const files = transcriptFiles(path.join(runDir, "sessions"));
  let reads = 0;
  let lines = 0;
  let apiCalls = 0;
  let first = "";
  for (const file of files) {
    const entries = parseSession(file);
    const texts = [
      ...toolResults(entries),
      ...entries.flatMap((entry) =>
        entry.type === "custom_message" && typeof entry.content === "string" ? [entry.content] : []
      ),
    ];
    for (const text of texts) {
      if (READ.some((pattern) => pattern.test(text))) reads++;
      const found = stateLines(text);
      lines += found.length;
      if (first === "" && found.length > 0) first = (found[0] ?? "").trim().slice(0, 160);
    }
    for (const entry of entries)
      for (const part of entry.message?.role === "assistant" ? (entry.message.content ?? []) : [])
        if (part.type === "toolCall" && API_CALL.test(JSON.stringify(part.arguments ?? "")))
          apiCalls++;
  }
  const verdict = reads === 0 ? "UNKNOWN" : lines > 0 ? "LEAK" : "hidden";
  return { transcripts: files.length, reads, lines, apiCalls, first, verdict };
}

function main(argv: string[]): number {
  const [maps, scenario] = argv;
  if (maps === undefined || argv.length > 2) {
    console.error("usage: bun hidden-proof.ts <work>/maps [<scenario>]");
    return 2;
  }
  const runs = path.join(maps, "runs");
  const names = (existsSync(runs) ? readdirSync(runs) : [])
    .filter((name) => scenario === undefined || name.startsWith(`${scenario}-`))
    .filter((name) => HEADS_SCENARIOS.some((heads) => name.startsWith(`${heads}-`)))
    .sort();
  console.log(
    [
      "run",
      "kind",
      "quiet",
      "transcripts",
      "reads",
      "state lines",
      "api calls",
      "verdict",
      "detail",
    ].join("\t")
  );
  const counts = { hidden: 0, proved: 0, visible: 0, shown: 0 };
  for (const name of names) {
    const runDir = realpathSync(path.join(runs, name));
    const scan = scanTranscripts(runDir);
    if (!/-hidden-\d+$/.test(name)) {
      counts.visible++;
      if (scan.lines > 0) counts.shown++;
      console.log(
        [
          name,
          "visible",
          "-",
          scan.transcripts,
          scan.reads,
          scan.lines,
          scan.apiCalls,
          "control",
          scan.first,
        ].join("\t")
      );
      continue;
    }
    counts.hidden++;
    const quiet = quietCheck(path.join(maps, "quiet", path.basename(runDir)));
    const verdict =
      quiet.verdict === "hidden" && scan.verdict === "hidden"
        ? "hidden"
        : ([quiet.verdict, scan.verdict].find((v) => v !== "hidden") ?? "UNKNOWN");
    if (verdict === "hidden") counts.proved++;
    const detail = scan.lines > 0 ? `${quiet.detail}; transcript: ${scan.first}` : quiet.detail;
    console.log(
      [
        name,
        "hidden",
        quiet.verdict,
        scan.transcripts,
        scan.reads,
        scan.lines,
        scan.apiCalls,
        verdict,
        detail,
      ].join("\t")
    );
  }
  console.log(
    `runs of a -hidden label: ${counts.hidden}, hidden: ${counts.proved}; runs of another label: ${counts.visible}, with state lines in their transcripts: ${counts.shown}`
  );
  if (counts.hidden === 0) {
    console.log(
      `UNKNOWN: no run of a -hidden label in ${runs}${scenario === undefined ? "" : ` for ${scenario}`}`
    );
    return 1;
  }
  return counts.proved === counts.hidden ? 0 : 1;
}

if (import.meta.main) process.exit(main(Bun.argv.slice(2)));
