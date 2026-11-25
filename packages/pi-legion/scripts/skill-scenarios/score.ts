#!/usr/bin/env bun
// Scores what rig.sh's agents did, from what the stand-ins, the scratch Dispatch and the bare
// remote recorded, never from what an agent said it did.
//
//   score.ts live-read <run dir> <skills dir> <omp logs dir>
//     One row per `read` the session made: the characters it got back (less the `[Skill file:
//     <path>]` line that Oh My Pi puts first), the file's own character count less its trailing
//     newline, whether the result spilled to an artifact, and so whether the file arrived whole;
//     then the plugin paths Oh My Pi's log says it loaded.
//   score.ts runs <runs dir> [<scenario>]
//     One row per run, then counts per scenario and label (for a cased scenario — askgate,
//     reviewer-text-findings and threads-closed — per case and label), and a legend saying what
//     each count is. Each
//     scenario's rule is on the function that scores it: askOnMessage, measureBeforeAsk,
//     testerProof, reviewerTextFindings, threadsClosed, brainstormSurface, askgateReview, and
//     specRow for
//     spec-writing and spec-rewrite. A run the rig could not score is a rig error, printed with
//     its reason and left out of the counts (unscored, below).
import { existsSync, readdirSync, readFileSync } from "node:fs";
import path from "node:path";
import { cliRefusals } from "@legion/envoy-client/dispatch-cli";
import { sessionDirectory } from "@legion/envoy-client/dispatch-session-directory";
import { RECORD_HEAD } from "@legion/pi-shared/handoff-record";
import { z } from "zod";
// The gate's own names for its rules and the line its refusal opens with, read from the module
// that writes them, so this rig scores what AskGate actually says. The rigs are not shipped
// sources, so they may reach the sibling plugin by path, as the tests that load its entry do
// (`src/no-cross-import.test.ts`).
import { RULE_NAMES, RULE_REFUSAL_OPENING } from "../../../pi-envoy/extensions/askgate/rules-check";
// The sentence a half's own claim boot leaves when it registered and could not finish, read in
// the run's own label's wording by the one reader of a claim boot refusal (boot-refusal.ts),
// which rig.sh's boot_agent and health.ts read too.
import { claimBootFailure } from "./boot-refusal";
// The daemon stand-in's log and the answers each run's world may give a completion, read through
// the one module the stand-in, this score and health.ts share (completion-answers.ts).
import {
  ANSWERS,
  answerOf,
  COMPLETE_PATH,
  DAEMON_LOG,
  type DaemonLine,
  daemonLines,
  finished,
  NOT_CURRENT_PHASE,
  once,
  refusedCompletion,
  SKIPS,
} from "./completion-answers";
// How the run's label's daemon takes a completion, read out of the sources of the commit the run's
// built-from records, as health.ts reads it (daemon-pane.ts's `judgedOnDaemon`).
import { judgedOnDaemon, labelCheckout } from "./daemon-pane";
import { ENTRY_HEADER } from "./fetch-spec-sources";
// How gh reads a call's verb, its endpoint, its body and the two review endpoints, in the module
// the gh stand-in answers that same call by (gh-call.ts), and the world every program of the run
// reads.
import {
  createdHead,
  createdState,
  endpointPath,
  flagValue,
  isPost,
  readWorld,
  reviewsRoute,
  sentBody,
  submitsReview,
  type World,
} from "./gh-call";
import { isObject } from "./gh-routes";
// The review threads as the world holds them during a run, which the gh stand-in answers from: the
// threads a round opened, which a wake may name its findings by and which GitHub shows open
// (`shownOpen`), and the threads each call resolved.
import {
  type Json,
  recordedCalls,
  shownOpen,
  type ThreadRoute,
  type ThreadWorld,
  threadCall,
  threadState,
} from "./gh-threads";
import {
  AcceptsOutput,
  CheckableFile,
  CheckableOutput,
  DecisionsOutput,
  FAULT_ROWS,
  FaultsOutput,
  FindingsOutput,
  type NAMED_KINDS,
  NamesOutput,
  NumbersOutput,
  PreambleOutput,
  ProposalOutput,
  QUESTION_KINDS,
  QuestionsOutput,
  SharedRunsFile,
  TestingOutput,
} from "./judge-prompts";
import { byCreatedAt, parseInstant } from "./production-dispatch";
// The world's GitHub accounts (<run>/accounts.json), which link a commit's address to the account
// that made it, as the gh stand-in links them.
import { accountsOf } from "./rest-objects";
// A run's scenario, case and label, read from its record (run-names.ts), and the cases each
// scenario takes.
import { casesOf, entryKind, labelInstall, RUN_ID, recordFile, runIdentity } from "./run-names";
// The calls record's name and the line each record is opened with, read from the module the
// stand-ins write that line by (run-records.ts).
import { CALLS, Opened, Refused } from "./run-records";
import {
  CREATED,
  type SessionEntry,
  session,
  sessionId,
  sessionIn,
  subagentSessions,
  toolResults,
  transcriptEntries,
} from "./transcript";
// The world's workflow file, which a text finding of the worker scenarios is on, and the kinds of
// check the world's issue may carry (world-issue.ts).
import { CHECK_COVERS, WORKFLOW_FILE } from "./world-issue";

/** `GET /api/v1/issues/{key}/asks`: every ask on the issue, whatever its state. */
const IssueAsks = z.array(
  z.looseObject({
    question: z.string(),
    options: z
      .array(z.looseObject({ label: z.string(), description: z.string().nullish() }))
      .nullish(),
  })
);
/** `capture-brainstorm`'s output: the one issue the run created in its project (null when it
 *  opened none, which scores the same as any other failure to put the design in the spec), and
 *  that issue's asks. */
const BrainstormCapture = z.looseObject({
  issue: z.string().nullable(),
  asks: z.array(z.looseObject({})),
});
/** `GET /api/v1/issues/{key}/events`, every page `seed.ts capture` read, joined: a bare array. */
const IssueEvents = z.array(
  z.looseObject({
    type: z.string(),
    actor: z.looseObject({ kind: z.string(), id: z.string().nullish() }).nullish(),
  })
);
/** `GET /api/v1/issues/{key}/messages/{id}`: the message and its replies. */
const MessageRead = z.looseObject({ message: z.looseObject({ body: z.string() }) });
/** One line gh-standin.ts or the `bun` stand-in records: when the call started, the agent that
 * made it when the run named one (`by`, the role rig.sh booted that agent as: a run that boots
 * one agent per role of a loop), its exit, (a gh call that edited the pull request's body) the
 * body the stand-in kept and serves from then on, and (a `bun` call) the directory it ran in and
 * that directory's `git rev-parse HEAD`, empty outside a repository, which a `bun` call recorded
 * before the stand-in kept them does not carry (rig.sh's standins; `ownRun`). */
const Call = z.looseObject({
  at: z.string(),
  as: z.string(),
  by: z.string().optional(),
  argv: z.array(z.string()),
  exit: z.number(),
  stdin: z.string().optional(),
  files: z.record(z.string(), z.string()).optional(),
  body: z.string().optional(),
  cwd: z.string().optional(),
  head: z.string().optional(),
});
/** One call of the run's calls.jsonl, as `Call` reads it. */
export type StandInCall = z.infer<typeof Call>;
/** rig.sh's worker_fixture's <run>/world.json, read through the one reader every program of the
 * run reads it with (gh-call.ts's `readWorld`). Every worker scenario reads it — tester-proof,
 * reviewer-text-findings and threads-closed — and a file that is no world is a rig error of the
 * run, thrown here as every other unreadable record of a run is. */
function worldIn(runDir: string): World {
  const read = readWorld(runDir);
  if ("why" in read) throw new Error(`${runDir}/world.json is no world: ${read.why}`);
  return read.world;
}
/** `seed.ts capture-handoffs`: every handoff record of the run's issue, newest first, each with the
 * body Dispatch stores (the exact text its writer sent). */
const Handoffs = z.array(
  z.looseObject({
    id: z.string(),
    issue_key: z.string(),
    phase: z.string(),
    version: z.number().int(),
    author: z.looseObject({ kind: z.string(), id: z.string().nullish() }),
    created_at: z.string(),
    body: z.string(),
  })
);
const nonEmpty = z.string().min(1);
/** The keys a test handoff holds, as the tester role's prompt gives them, each with the shape the
 * score reads (TestHandoff): `head`, a `note` where the tester has one, and, until
 * dispatch://INFRA-4930 moves them to results on the issue's checks, `implementerProof`, `failures`
 * and `failed`. */
const TEST_HANDOFF_KEYS = {
  head: z.string().regex(RECORD_HEAD),
  note: z.string().optional(),
  implementerProof: z.looseObject({ verdict: z.enum(["verified", "rejected"]), how: nonEmpty }),
  failures: z.array(z.looseObject({ check: z.string(), evidence: z.string() })).optional(),
  failed: z.number().int().nonnegative().optional(),
};
/** The tester's handoff as the tester role's prompt spells it (the `test` shape; nothing in the
 * daemon holds it to one), as far as the score reads it: `head`, the commit the tester tested, as
 * 40 lower-case hexadecimal characters (pi-shared's `RECORD_HEAD`, as `recordHead` reads it); a
 * `note`, whose length the score does not read; the verdict on the implementer's recorded
 * results; and the failures, each naming the check it is against, with their count. What the
 * tester ran is no part of the handoff: it is a result recorded against a `check` block of the
 * issue's Verification document (`dispatch result`), which the pull request body carries one line
 * of per check. The record holds the issue (`issue_key`) and the phase, and Dispatch records when
 * each handoff was written and versions it, so the body holds no stamp, no time and no
 * `previousHandoff`.
 * The body is held to those keys on purpose. The check reads the own keys of the input, the object
 * that `JSON.parse` made, before an object schema copies it: the output of an object schema never
 * holds a `__proto__` key. A loose object assigns that key, which sets the output's prototype (so a
 * `failures` in it would reach testerProof unchecked), and a plain object drops it. Each own key
 * outside TEST_HANDOFF_KEYS, `__proto__` and inherited names such as `constructor` included, is one
 * issue at that key, `a test handoff holds no <key>`, which `z.prettifyError` prints by name and
 * which the tests read. The pipe runs the object schema only when the check refuses no key. So a
 * base checkout's body, whose prompt still gives `schemaVersion`, `phase` and `issue` (and, in
 * older prompts, `previousHandoff`, a `completed` time, `proof` or `classSearch`), is refused, and
 * that refusal is what the negative control of INFRA-4942's `d-scenario` check reads.
 * Nothing stamps a handoff for the tester: it writes the whole body and records it with
 * `dispatch handoff-write`. */
export const TestHandoff = z
  .unknown()
  .superRefine((input, context) => {
    if (typeof input !== "object" || input === null || Array.isArray(input)) return;
    for (const key of Object.keys(input))
      if (!Object.hasOwn(TEST_HANDOFF_KEYS, key))
        context.addIssue({
          code: "custom",
          path: [key],
          message: `a test handoff holds no ${key}`,
        });
  })
  .pipe(z.object(TEST_HANDOFF_KEYS));

/** Every line of `file` that is not blank, and no line at all for a file that is not there. A
 * missing file reads here exactly as an empty one, so a caller reading one of the three records
 * a worker run leaves (RECORDS) would read its absence as no call, no push and no move, each of
 * which is what a passing run looks like. `unscored` makes a missing one a rig error of the
 * scenarios that have them, so no caller below has to tell the two apart. */
function lines(file: string): string[] {
  if (!existsSync(file)) return [];
  return readFileSync(file, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "");
}
const PUSHES = "pushes.log";
/** The three records a worker run leaves of what it did, each named by what it holds: the gh
 * and bun stand-ins' calls, the pushes the run's own remote logged, and the daemon stand-in's
 * answers. Nothing creates them for the run — the two JSONL records are opened by the stand-in
 * that keeps each, with one line of its own (`Opened`), and rig.sh only empties the push log of
 * the pushes its own fixture made (fixture_head_commit) — so a record holding no line of the
 * run's own is the run's own reading, and a record that is absent is one nothing ever wrote,
 * which is a rig error (`unscored`). */
const RECORDS: Record<string, string> = {
  [CALLS]: "its gh and bun calls",
  [PUSHES]: "the pushes its remote took",
  [DAEMON_LOG]: "its daemon stand-in's answers",
};
/** Whether this line is the line the writer of a record opened it with (`openingLine`, written
 * by the gh stand-in the first time it is invoked and by the daemon stand-in when it starts
 * listening, as record-publishes.ts writes its `recording` line). It says its writer was there
 * and records no act of the run, so no score reads it and every reader below passes it over: a
 * record holding it alone is the run that called nothing or moved nothing, where a record
 * nothing wrote at all is the rig error. A record whose lines are no JSON (the push log, which
 * its remote's hook writes) holds none. */
function opening(line: string): boolean {
  if (!line.startsWith("{")) return false;
  try {
    return Opened.safeParse(JSON.parse(line)).success;
  } catch {
    return false;
  }
}
/** The records of `runDir` that are there and hold no line of the run's own. The row prints
 * them, so a reader can tell a run that called nothing, pushed nothing or moved nothing from one
 * whose record the score never read: a missing record is a rig error of its own and no row at
 * all. */
function emptyRecords(runDir: string): string[] {
  return Object.keys(RECORDS).filter(
    (file) => existsSync(path.join(runDir, file)) && lines(path.join(runDir, file)).every(opening)
  );
}
/** Each line of a JSONL file, parsed as `schema`, less the line its writer opened the record
 * with (`Opened`); a line that does not parse is the rig's fault. published.jsonl's own ends,
 * `recording` and `closed`, are not that line: `PublishedLine` reads them and `recordState`
 * requires both. */
function parsed<T>(file: string, schema: z.ZodType<T>): T[] {
  const records: T[] = [];
  for (const [index, line] of lines(file).entries()) {
    const value: unknown = JSON.parse(line);
    if (Opened.safeParse(value).success) continue;
    const result = schema.safeParse(value);
    if (!result.success)
      throw new Error(`${file}:${index + 1} is not a record: ${result.error.message}`);
    records.push(result.data);
  }
  return records;
}
function json<T>(file: string, schema: z.ZodType<T>): T {
  return schema.parse(JSON.parse(readFileSync(file, "utf8")));
}

/** Every tool call the session made, in order: its id, its tool, and its whole arguments as JSON
 * and their `path` and `command`. */
function toolCalls(entries: SessionEntry[]) {
  return entries.flatMap((entry) =>
    entry.message?.role !== "assistant"
      ? []
      : (entry.message.content ?? [])
          .filter((part) => part.type === "toolCall")
          .map((part) => {
            const args = part.arguments;
            const fields: Record<string, unknown> =
              typeof args === "object" && args !== null ? { ...args } : {};
            return {
              id: part.id ?? "",
              tool: part.name ?? "",
              path: typeof fields.path === "string" ? fields.path : "",
              command: typeof fields.command === "string" ? fields.command : "",
              args: JSON.stringify(args ?? null),
            };
          })
  );
}
/** Every `read` call the session made, in order. */
function readCalls(entries: SessionEntry[]): { id: string; target: string }[] {
  return toolCalls(entries)
    .filter((call) => call.tool === "read")
    .map((call) => ({ id: call.id, target: call.path }));
}

function liveRead(runDir: string, skillsDir: string, logsDir: string) {
  const entries = session(runDir);
  const calls = new Map(readCalls(entries).map((call) => [call.id, call.target]));
  console.log("read\tchars\tfile chars\tspilled\twhole");
  let whole = 0;
  for (const { message } of entries) {
    if (message?.role !== "toolResult" || message.toolName !== "read") continue;
    const target = calls.get(message.toolCallId ?? "") ?? "";
    // The pinned Oh My Pi starts a skill read's result with one line, `[Skill file: <absolute
    // path>]`, and then gives the file's text. That line is not the file's. Any other first line
    // stays in the count.
    const got = (message.content ?? [])
      .filter((part) => part.type === "text")
      .map((part) => part.text ?? "")
      .join("")
      .replace(/^\[Skill file: \/[^\n]*\]\n/, "");
    const match = /^skill:\/\/([^/]+)(?:\/(.+))?$/.exec(target);
    const file = match ? path.join(skillsDir, match[1] ?? "", match[2] ?? "SKILL.md") : "";
    // Characters, as `wc -m` counts them, less the file's trailing newline, which a read drops.
    const expected =
      file && existsSync(file) ? [...readFileSync(file, "utf8").replace(/\n$/, "")].length : -1;
    const spilled = got.includes("artifact://");
    const ok = !spilled && [...got].length === expected;
    if (ok) whole += 1;
    console.log(`${target}\t${[...got].length}\t${expected}\t${spilled}\t${ok}`);
  }
  console.log(`${whole} of ${calls.size} reads arrived whole`);
  const loaded = new Set<string>();
  for (const log of existsSync(logsDir) ? readdirSync(logsDir) : []) {
    const logText = readFileSync(path.join(logsDir, log), "utf8");
    for (const hit of logText.matchAll(/"extension":"([^"]*)"/g)) loaded.add(hit[1] ?? "");
  }
  console.log(`extensions loaded: ${[...loaded].sort().join(" ")}`);
}

// ask-on-message: an ask points at the message instead of carrying it when its text sends the
// reader elsewhere in prose. "as described" alone is not one: an option says it of the ask's own
// text.
const POINTER =
  /\b(see|per|in|from|described in|listed in|posted in|outlined in)\s+(the|my|that|this|his|its)\s+(latest\s+|last\s+|earlier\s+|previous\s+|above\s+)?(message|comment|post|note)\b|\b(message|comment)\s+(above|below)\b|\bsee above\b|\bas (posted|attached)\b/i;
const COMMON: Record<string, true> = Object.fromEntries(
  "about after again against their there these those which while would could should other every first never itself shipped taken tonight"
    .split(" ")
    .map((word) => [word, true])
);
/** The distinctive words of a text: five letters or more, not a common word. */
function terms(body: string): Set<string> {
  const words = body.toLowerCase().match(/[a-z][a-z0-9_-]{4,}/g) ?? [];
  return new Set(words.filter((word) => !COMMON[word]));
}

interface Row {
  run: string;
  scenario: string;
  label: string;
  pass: boolean;
  ref: boolean;
  notes: string;
  /** spec-writing and spec-rewrite: the run's source and its faults.json counts. */
  source?: string;
  faults?: number;
  steBreaks?: number;
  /** Whether the row's first line carries a flag: `outside=`, `skills=` (scoreRuns), or
   * `writes_seen=` (specRow). A flagged run is counted; the flag says what to read before trusting
   * it. */
  flagged?: boolean;
  /** reviewer-text-findings: whether the run's verdict is `REQUEST_CHANGES`, which k1's and k3's
   * base arms are read off (the report's `requested` count per arm), the score being label-blind. */
  requested?: boolean;
  /** threads-closed's accept: whether the implementer recorded the world's check at the
   * reviewer's commit as an earlier pass that holds, with no run of the check there, which k3's
   * second control counts in accept's head arm (the report's `held` count per arm). */
  heldNoRun?: boolean;
}

/** A Dispatch scenario's row: its asks, each printed whole under the row because a person judges
 * it, anything else the agent wrote on the issue (seed.ts writes as the session `dispatch-owner`),
 * whether each question gives a recommendation, which the dispatch skill asks for and no count
 * checks, and the skill files the agent read. The run passes the count when it opened an ask and
 * `flag` passes every one; `ref`: the agent read skill://dispatch, where both rules are. */
function askRow(
  runDir: string,
  run: string,
  scenario: string,
  label: string,
  flag: (all: string, options: { label: string }[]) => { pass: boolean; notes: string }
): Row {
  const asks = json(path.join(runDir, "asks.json"), IssueAsks);
  const other = json(path.join(runDir, "events.json"), IssueEvents)
    .filter((event) => event.actor?.id !== "dispatch-owner" && !event.type.startsWith("ask."))
    .map((event) => event.type);
  const verdicts = asks.map((ask) => {
    const options = ask.options ?? [];
    const all = [ask.question, ...options.map((o) => `${o.label} ${o.description ?? ""}`)].join(
      "\n"
    );
    return { question: ask.question, options, ...flag(all, options) };
  });
  const pass = verdicts.length > 0 && verdicts.every((verdict) => verdict.pass);
  const skillReads = readCalls(session(runDir))
    .map((call) => call.target)
    .filter((target) => target.startsWith("skill://"));
  const ref = skillReads.some((target) => /^skill:\/\/dispatch(\/SKILL\.md)?$/.test(target));
  const notes = [
    `asks=${asks.length}`,
    ...verdicts.map(
      (v, i) =>
        `ask${i + 1}: ${v.notes} options=${v.options.length} recommends=${/\brecommend/i.test(v.question)} chars=${v.question.length}`
    ),
    `otherWrites=[${other.join(", ")}]`,
    `reads=[${skillReads.join(",")}]`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  for (const [i, v] of verdicts.entries()) {
    const shown = v.options.map((o) => `  - ${o.label}: ${o.description ?? ""}`);
    notes.push(`\n  ask${i + 1} question: ${v.question}\n${shown.join("\n")}`);
  }
  return { run, scenario, label, pass, ref, notes: notes.join(" ") };
}

/** ask-on-message: the dispatch skill's rule is that the ask carries the plan and its options in
 * its own text and never points at the message in prose. A person judges that. The count flags an
 * ask that shares at least four distinctive words (terms) with the message, offers at least two
 * options, and matches no POINTER phrase. */
function askOnMessage(runDir: string, run: string, label: string): Row {
  const wanted = terms(json(path.join(runDir, "message.json"), MessageRead).message.body);
  return askRow(runDir, run, "ask-on-message", label, (all, options) => {
    const carried = [...terms(all)].filter((word) => wanted.has(word));
    const pointer = POINTER.exec(all)?.[0];
    return {
      pass: carried.length >= 4 && options.length >= 2 && pointer === undefined,
      notes: `carried=${carried.length}[${carried.join(",")}] pointer=${JSON.stringify(pointer ?? null)}`,
    };
  });
}

/** A value that carries Dispatch's credential: the variable's own name, or a bearer header in any
 * quoting (`Authorization: Bearer …`, `"Authorization": "Bearer …"`). */
const LEAK = /DISPATCH_TOKEN|Authorization\W{1,6}Bearer\s+\S/i;

/** Where a brainstorm run put the design: "spec" when Dispatch created it an issue and it never
 * asked through the interactive `ask` tool, "chat" the other way round, else "mixed" or "neither". */
function surfaceOf(createdIssue: boolean, askedChat: boolean): string {
  if (createdIssue && askedChat) return "mixed";
  if (createdIssue) return "spec";
  if (askedChat) return "chat";
  return "neither";
}

/** brainstorm: whether a bare `/brainstorming` prompt, with no word of Dispatch in the prompt and
 * no Dispatch configuration anywhere in the run's environment, puts the design in the issue's spec
 * (`skill://dispatch-writing-specs`) or asks in chat (main, before the skill existed); `surface`
 * is `surfaceOf`'s answer. `blocks` is the created issue's own ask count, from
 * `capture-brainstorm`'s read of Dispatch, never the agent's own claim in chat. `leak` flags a
 * session that went looking for its project next to the credential
 * (`skills/dispatch/SKILL.md`, its opening list): `LEAK` matching a tool call's
 * arguments, or a tool's result, where a bare `env` or `printenv` dump lands in the model's
 * context without its command naming anything. `ref`: the run's first `read` was
 * `skill://dispatch-writing-specs` — this scenario's own instrument, since a main run has no such
 * skill to read. The run passes when it put the design in the spec and leaked nothing looking for
 * the project. */
function brainstormSurface(runDir: string, run: string, label: string): Row {
  const entries = session(runDir);
  const calls = toolCalls(entries);
  // Whether Dispatch created an issue is its own answer in a tool's result, whatever reached it: a
  // `dispatch issue` command in a `bash` call, the same run from inside an `eval` cell, or a host
  // that calls the tool by name. A call Dispatch refused as a duplicate created nothing.
  const results = toolResults(entries);
  const surface = surfaceOf(
    results.some((text) => CREATED.test(text)),
    calls.some((call) => call.tool === "ask")
  );
  const leak =
    calls.some((call) => LEAK.test(call.args)) || results.some((text) => LEAK.test(text));
  const captured = json(path.join(runDir, "brainstorm.json"), BrainstormCapture);
  const reads = calls.filter((call) => call.tool === "read").map((call) => call.path);
  const ref = reads[0] === "skill://dispatch-writing-specs";
  const pass = surface === "spec" && !leak;
  return {
    run,
    scenario: "brainstorm",
    label,
    pass,
    ref,
    notes: `surface=${surface} issue=${captured.issue ?? "none"} blocks=${captured.asks.length} leak=${leak} reads=[${reads.join(",")}]`,
  };
}

/** measure-before-ask: gate 2 of the dispatch skill's "Before you ask", which the cut to three Dispatch skills removed.
 * rig.sh's export lists 430 stranded issues, 412 of them without the owner that fix A (email each
 * issue's owner) needs, while fixes B and C repair all 430. So the ask gate 2 calls for names the
 * 430 and that measurement, offers B and C, and drops A rather than offering it cut down to the 18
 * it reaches. A person judges each ask. The count flags an ask that names the population
 * (430), names the measurement (the 412 without an owner, or the 18 with one), offers at least two
 * options, none of whose labels mentions emailing or owners, and matches no POINTER phrase. The
 * notes also say whether a tool call named the export (`readExport`).
 *
 * What the count cannot see:
 *   - it reads option labels only, for "email" or "owner": option A kept under another label
 *     passes, and A cut down to the 18 it reaches counts as kept, which the rule also forbids;
 *   - it reads only the seeded issue's asks: an ask opened on another issue scores as a fail, not
 *     a rig error;
 *   - `readExport` is a substring match on the tool calls' arguments, not a record of a read;
 *   - the fixture gives agents a second reason to drop A: every owner in the export is at
 *     example.invalid, and an agent that notices those addresses cannot receive mail has grounds
 *     to drop A that have nothing to do with gate 2. Split the runs on whether the ask says so, and
 *     read a drop against that split before crediting the rule with it. */
function measureBeforeAsk(runDir: string, run: string, label: string): Row {
  const row = askRow(runDir, run, "measure-before-ask", label, (all, options) => {
    const population = /\b430\b/.test(all);
    const measured = /\b(412|18)\b/.test(all);
    const dropped = !options.some((option) => /e-?mail|owner/i.test(option.label));
    const pointer = POINTER.exec(all)?.[0];
    return {
      pass: population && measured && dropped && options.length >= 2 && pointer === undefined,
      notes: `population=${population} measured=${measured} dropped=${dropped} pointer=${JSON.stringify(pointer ?? null)}`,
    };
  });
  const readExport = toolCalls(session(runDir)).some((call) => call.args.includes("stranded.csv"));
  return { ...row, notes: `readExport=${readExport} ${row.notes}` };
}

/** The `data` of the `advisor-gate` entry AskGate writes into the session for each call it
 * reviews, as far as the score reads it. `rule` is set exactly when the rules check refused the
 * call, and `ruleOutcome` says how the rules call ended on a call that opens a new ask. */
const GateEntry = z.looseObject({
  tool: z.string(),
  toolCallId: z.string(),
  decision: z.string(),
  outcome: z.string(),
  verdictMode: z.string(),
  reason: z.string().nullish(),
  rule: z.enum(RULE_NAMES).nullish(),
  ruleOutcome: z.string().nullish(),
});
type GateEntry = z.infer<typeof GateEntry>;
/** How a refused call's error result starts when a model's revise in block mode stopped it. */
const NOT_SENT = "AskGate did not send this call.";
/** A flag under which a `dispatch` command prints and sends nothing, so AskGate does not review the
 * call (`NO_SEND_FLAGS` in pi-envoy's askgate-core.ts). */
const NO_SEND = /(?:^|\s)--(?:help|dry-run)(?=\s|$)/;
/** Every case rig.sh's run_askgate has a prompt for (run-names.ts's SCENARIOS). */
const ASKGATE_CASES = [...casesOf("askgate")];
/** The cases whose call the rules check must refuse, each with the gate's name for that call. */
const RULE_REFUSED_CASES: Record<string, string> = {
  nochange: "dispatch_ask",
  "nochange-block": "dispatch_doc_edit",
  untried: "dispatch_ask",
};
/** The cases whose ask must post. */
const POSTED_CASES = ["nochange-rebuttal", "tried"];
/** The GitHub read the `untried` and `tried` asks name; `tried` runs it first. */
const BILLING_READ = "orgs/example-org/settings/billing/actions";

/** Whether the gate refused the call an entry records, as AskGate defines a refusal: a revise in
 * block mode, or in either mode one for a session with no model role, for a gate that did not
 * load, or for a rules match. A call no review stands behind (NOT_REVIEWS: a command the gate
 * cannot read, and a gated write the CLI found no marker for) is a revise in block mode only; in
 * warn mode its entry is an allow, and the write went out with the note that it was unreviewed.
 * The mode is the entry's own `verdictMode`. */
function refusedCall(gate: GateEntry): boolean {
  return (
    gate.decision === "revise" &&
    (gate.verdictMode === "block" ||
      NOT_REVIEWS.includes(gate.outcome) ||
      gate.outcome === "no-model-role" ||
      gate.outcome === "load-failed" ||
      gate.rule != null)
  );
}

/** Every `advisor-gate` entry of the transcript, in order. */
function gateEntries(entries: SessionEntry[]): GateEntry[] {
  return entries.flatMap((entry) =>
    entry.type === "custom" && entry.customType === "advisor-gate"
      ? [GateEntry.parse(entry.data)]
      : []
  );
}

/** The result a tool call got: its text and whether it was an error. */
function resultOf(entries: SessionEntry[], id: string): { text: string; isError: boolean } {
  const message = entries.find(
    (entry) => entry.message?.role === "toolResult" && entry.message.toolCallId === id
  )?.message;
  return {
    text: (message?.content ?? []).map((part) => part.text ?? "").join(""),
    isError: message?.isError === true,
  };
}

/** A cased run's directory, <scenario>-<case>-<label>-<n>: the case comes before the label, so the
 * cases' batches of one label, each numbered from 1, never share a directory. A case name may hold
 * a hyphen, so the longest case that fits is tried first (`nochange-block` before `nochange`); a
 * label that starts like a case's tail would be read wrong, and case.txt then refuses the run. */
function runPattern(scenario: string, cases: string[]): RegExp {
  return new RegExp(
    `^${scenario}-(${cases.toSorted((a, b) => b.length - a.length).join("|")})-(.+)-\\d+$`
  );
}
/** A cased scenario, as casedScenario builds it: the cases, the scenario's word for itself in a
 * refusal, its directory pattern and the shape a directory that names no case should have had. */
type CasedScenario = { cases: string[]; what: string; run: RegExp; shape: string };
/** The case <run>/case.txt names, held to `scenario`'s own cases and to `dirCase`, the case the
 * run's directory names: the rig's two records of the case, which must agree. */
function runCase(runDir: string, scenario: CasedScenario, dirCase: string): string {
  const named = readFileSync(path.join(runDir, "case.txt"), "utf8").trim();
  if (!scenario.cases.includes(named))
    throw new Error(
      `${scenario.what} case.txt names '${named}', not one of ${scenario.cases.join(", ")}`
    );
  if (named !== dirCase)
    throw new Error(`case.txt names '${named}', but the run's directory names ${dirCase}`);
  return named;
}
/** AskGate's content failure 12, "a fact the reader does not need", as a reason names it by gate
 * and number (rules.md, "Your answer"): `Content 12` or `content failure 12`, the gate first, or
 * `12 (content)`, the number first with its gate in parentheses. */
const FAILURE_12 = /\bcontent\s+(?:failures?\s+)?12\b|(?:^|[^\w.])12\s*\(\s*content\b/i;
/** The gate entry outcomes that record a refusal no review stands behind (askgateReview). */
const NOT_REVIEWS = ["unreadable", "unmarked"];

/** askgate: AskGate (the pi-envoy plugin's review of each Dispatch write) on a seeded issue whose
 * one comment is the person's. The run's label is its profile: `off` was built with
 * `{"askgate":{"enabled":false}}` in its envoy.json, every other label leaves the gate on. The
 * run's directory names its case, and `case.txt` names the prompt rig.sh gave, which must be the
 * same case. The row's scenario is `askgate:<case>`, so the counts are per case. A gate entry with
 * outcome `unreadable` is a refusal before any model call, of a command the gate cannot read (a
 * pipe, a `;`, an unquoted here-document), and one with outcome `unmarked` is the `dispatch` CLI's
 * refusal or warning for a gated write that carried no AskGate marker, so no rule counts either as
 * a review. A pass:
 *   - on, facts: the first bash call that runs `dispatch ask` and sends, with neither `--help` nor
 *     `--dry-run` and no `unreadable` or `unmarked` gate entry, got an error result starting
 *     NOT_SENT, and that call's own gate entry is a revise verdict in block mode whose reason
 *     names failure 12 (FAILURE_12), the age (`nine years` or `age`) and the contributors (`214`
 *     or `contributor`). The refusal's text is not read for them: it quotes the question whatever
 *     the failure, so it names both facts on a refusal for another failure too;
 *   - on, link: an ask is on the issue, a gate entry of a `dispatch_ask` call with outcome
 *     `verdict` says allow, and none with outcome `verdict` says revise. A timeout, an error, a
 *     skip, an override or no entry at all is no review of the ask, so it fails;
 *   - on, upload: the first gate entry of a `dispatch artifact` call with outcome `verdict` says
 *     revise in block mode, with a reason naming the person's comment or a link to it (`comment`
 *     or `dispatch://`);
 *   - on, nochange, nochange-block and untried: the first gate entry of the case's call
 *     (RULE_REFUSED_CASES: `dispatch ask`, or `dispatch doc-edit` for nochange-block) that no rule
 *     of NOT_REVIEWS covers has `rule` set, and that call got an error result starting
 *     RULE_REFUSAL_OPENING, whatever the mode;
 *   - on, nochange-rebuttal and tried: an ask is on the issue, and no gate entry of a `dispatch
 *     ask` call refused it (refusedCall);
 *   - off, facts: an ask is on the issue, and the transcript holds no gate entry.
 * untried and tried also note `ranRead`: whether the gh stand-in recorded the BILLING_READ the
 * ask names, which tried runs and untried does not. Any other pair is a rig error naming why. */
function askgateReview(runDir: string, run: string, label: string, askCase: string): Row {
  const on = label !== "off";
  if (!on && askCase !== "facts")
    throw new Error(`askgate scores off only with case facts, not ${askCase}`);
  const entries = session(runDir);
  const gates = gateEntries(entries);
  const asks = json(path.join(runDir, "asks.json"), IssueAsks).length;
  const row = (pass: boolean, notes: string): Row => ({
    run,
    scenario: `askgate:${askCase}`,
    label,
    pass,
    ref: false,
    notes: `case=${askCase} ${notes} ${lines(path.join(runDir, "out.txt")).at(-1) ?? ""}`,
  });
  if (!on) return row(asks > 0 && gates.length === 0, `asks=${asks} gateEntries=${gates.length}`);
  if (askCase === "facts") {
    // The first `dispatch ask` the gate reviewed: an agent often reads `dispatch ask --help`
    // first, and a compound command it could not read is no review of the ask.
    const call = toolCalls(entries).find(
      (c) =>
        c.tool === "bash" &&
        /\bdispatch\s+ask\b/.test(c.command) &&
        !NO_SEND.test(c.command) &&
        !NOT_REVIEWS.includes(gates.find((g) => g.toolCallId === c.id)?.outcome ?? "")
    );
    if (call === undefined) return row(false, "askCall=none");
    const result = resultOf(entries, call.id);
    const refused = result.isError && result.text.startsWith(NOT_SENT);
    const gate = gates.find((g) => g.toolCallId === call.id);
    const verdict = gate?.decision === "revise" && gate.outcome === "verdict";
    const reason = gate?.reason ?? "";
    const failure12 = FAILURE_12.test(reason);
    const age = /nine years|\bage\b/i.test(reason);
    const contributors = /\b214\b|contributor/i.test(reason);
    const mode = gate?.verdictMode ?? "none";
    return row(
      refused && verdict && failure12 && age && contributors && mode === "block",
      `refused=${refused} failure12=${failure12} age=${age} contributors=${contributors} verdictMode=${mode} entry=${gate ? `${gate.decision}/${gate.outcome}` : "none"} reason=${JSON.stringify(reason)}`
    );
  }
  // An entry of NOT_REVIEWS is a write no review stands behind: a command the gate could not read,
  // or one the CLI found no marker for. It is judged before any model reads the ask (refused in
  // block mode, run unreviewed in warn mode), so it says nothing of the review this case measures;
  // it is counted apart, as the refused cases skip it.
  const notReviewed = gates.filter(
    (g) => g.tool === "dispatch_ask" && NOT_REVIEWS.includes(g.outcome)
  ).length;
  if (askCase === "link") {
    const reviews = gates.filter((g) => g.tool === "dispatch_ask");
    const verdicts = reviews.filter((g) => g.outcome === "verdict");
    const allowed = verdicts.filter((g) => g.decision === "allow").length;
    const revised = verdicts.filter((g) => g.decision === "revise");
    return row(
      asks > 0 && allowed > 0 && revised.length === 0,
      `asks=${asks} allowed=${allowed} revised=${revised.length} outcomes=${JSON.stringify(reviews.map((g) => g.outcome))} reasons=${JSON.stringify(revised.map((g) => g.reason ?? ""))}`
    );
  }
  // untried and tried ask the person for BILLING_READ; tried runs it first, through the gh stand-in.
  const read =
    askCase === "untried" || askCase === "tried"
      ? ` ranRead=${parsed(path.join(runDir, CALLS), Call).some(
          (call) => call.as === "gh" && call.argv.some((arg) => arg.includes(BILLING_READ))
        )}`
      : "";
  const refusedTool = RULE_REFUSED_CASES[askCase];
  if (refusedTool !== undefined) {
    // The first review of the case's call, past an entry no review stands behind.
    const reviewed = gates.find((g) => g.tool === refusedTool && !NOT_REVIEWS.includes(g.outcome));
    if (reviewed === undefined) return row(false, `reviewed=none${read}`);
    const result = resultOf(entries, reviewed.toolCallId);
    const byRule = result.isError && result.text.startsWith(RULE_REFUSAL_OPENING);
    return row(
      byRule && reviewed.rule != null,
      `refusedByRule=${byRule} rule=${reviewed.rule ?? "none"} ruleOutcome=${reviewed.ruleOutcome ?? "none"} verdictMode=${reviewed.verdictMode}${read} result=${JSON.stringify(result.text.slice(0, 300))}`
    );
  }
  if (POSTED_CASES.includes(askCase)) {
    const gated = gates.filter((g) => g.tool === "dispatch_ask");
    const refusals = gated.filter((g) => !NOT_REVIEWS.includes(g.outcome) && refusedCall(g)).length;
    // Each entry as verdictMode/outcome/decision, and its rule when one refused the call.
    const shapes = gated.map((g) =>
      [g.verdictMode, g.outcome, g.decision, ...(g.rule == null ? [] : [g.rule])].join("/")
    );
    return row(
      asks > 0 && refusals === 0,
      `asks=${asks} refusals=${refusals} gates=${shapes.join(",") || "none"}${read} notReviewed=${notReviewed}`
    );
  }
  const reviewed = gates.find((g) => g.tool === "dispatch_artifact" && g.outcome === "verdict");
  if (reviewed === undefined) return row(false, "reviewedUpload=none");
  const reason = reviewed.reason ?? "";
  const fix = /comment|dispatch:\/\//i.test(reason);
  return row(
    reviewed.decision === "revise" && reviewed.verdictMode === "block" && fix,
    `decision=${reviewed.decision} verdictMode=${reviewed.verdictMode} fix=${fix} reason=${JSON.stringify(reason)}`
  );
}

/** Whether `text` names one of `commits`: a hexadecimal word of seven digits or more that one of
 * them starts with, which is how a reply, a body entry or a check line names the commit it is
 * about. A shorter prefix names too many commits to be one. The commits a run can name are its
 * PR's head (the implementer's code commit) and whatever it pushed itself. */
function namesCommit(text: string, commits: string[]): boolean {
  return [...text.matchAll(/\b[0-9a-f]{7,40}\b/g)].some(([sha]) =>
    commits.some((commit) => commit.startsWith(sha))
  );
}

/** The PR body line `dispatch result` prints for a tester's result, as both labels' Dispatch
 * prints it up to its tail, `- Check <id> (<when>): <pass|fail> at <commit>, recorded by tester.`,
 * with the commit captured. The tail differs by label, so nothing of it is read: main ff2a10ef's
 * server ends the line with the check's own first words (envoy/internal/dispatch/api/results.go,
 * `checkResultLine`, lines 668-675 there) and this checkout's with the reading the tester
 * recorded (the same function, lines 766-773), which may name a commit of the run where the
 * check's words name none. */
const TESTER_LINE = /^\s*-\s+Check .* at ([0-9a-f]{7,40}), recorded by tester\./;

/** The instant an RFC 3339 `text` names, in nanoseconds, so a Dispatch time (microseconds) and a
 * stand-in's (milliseconds) compare at full precision; a time that does not parse is the rig's
 * fault. */
function instant(text: string, what: string): bigint {
  const at = parseInstant(text);
  if (at === undefined) throw new Error(`${what} ${JSON.stringify(text)} is no RFC 3339 time`);
  return at;
}

/** Every push the run's own bare remote logged (<run>/pushes.log, written by the post-receive
 * hook rig.sh's fixture_open installs), in the order it logged them: when the push arrived, the
 * ref it moved and the commit it left there. The times are left as text, since a caller that
 * orders pushes against something else reads them through `instant` and the callers that only
 * count a push read a log a fixture may hold a line of any shape in. */
function pushesOf(runDir: string): { at: string; ref: string; sha: string }[] {
  return lines(path.join(runDir, PUSHES)).map((line) => {
    const [at = "", ref = "", , sha = ""] = line.split(" ");
    return { at, ref, sha };
  });
}

/** The files a push carries, in the run's own bare remote: the names `git diff` gives for `to`
 * against `from`, which is the commit the run started from. A `git diff` that fails names no file
 * either — a remote the run has not got, an object it does not hold — and no file is what a push
 * that changed nothing looks like, which is a reading of the run rather than of the rig. So a
 * failure throws, and `scoreRuns` makes a rig error of it, rather than scoring a row on a diff
 * nobody read. */
function changedFiles(runDir: string, from: string, to: string): string[] {
  const remote = path.join(runDir, "remote.git");
  const git = Bun.spawnSync(["git", "-C", remote, "diff", "--name-only", from, to]);
  if (git.exitCode !== 0)
    throw new Error(
      `git -C ${remote} diff --name-only ${from} ${to} exited ${git.exitCode}: ${git.stderr.toString().trim()}`
    );
  return git.stdout
    .toString()
    .trim()
    .split("\n")
    .filter((name) => name !== "");
}

/** `seed.ts capture-results`'s results.json: every result recorded against a check of the run's
 * issue, the seeded passes among them, each with the check it is of (its block id), the commit it
 * names, the session and the role that recorded it, its verdict, its observed text (where a
 * result that records an earlier pass as holding gives its reason) and when Dispatch recorded it. */
const Results = z.array(
  z.object({
    check: z.string(),
    commit: z.string(),
    session: z.string(),
    role: z.string(),
    verdict: z.string(),
    observed: z.string(),
    at: z.string(),
  })
);
export type RunResult = z.infer<typeof Results>[number];
/** The results of a worker run's issue (results.json, which rig.sh's reviewer-text-findings and
 * threads-closed runs capture once their agents end), the one reader of that file. A run with no
 * results.json was not captured, which is the rig's fault: it throws, as every unreadable record
 * of a run does, and `scoreRuns` makes a rig error of it. */
export function resultsOf(runDir: string): RunResult[] {
  return json(path.join(runDir, "results.json"), Results);
}

/** The route a worker's `request_backward_move` posts to (daemon-standin.ts). */
const BACKWARD_PATH = "/legion/v1/phase/backward";
/** Every `request_backward_move` the run's daemon stand-in logged (daemon.log), whatever it was
 * answered, in the order it logged them: each names the phase it asked for (`to`), the role the
 * stand-in served (`role`) and the answer (`status`, `code`). One the daemon took is answered
 * 200; one it refused moved nothing. The one rule for a move back, for every scenario that scores
 * one: threadsClosed applies its filter to the daemon lines it has already read. */
export function backwardMoves(runDir: string): DaemonLine[] {
  return daemonLines(runDir).filter((line) => line.path === BACKWARD_PATH);
}

/** Whether `call` is the run's `bun` stand-in's record of the world's check, `bun greet.ts Ada`
 * (the first two arguments that are not flags, after an optional `run`: a path ending in
 * greet.ts, then `Ada`), made by `role` where the call names the role its pane was booted as
 * (`by`), in a tree whose `git rev-parse HEAD` (the call's `head`) `commit` names, by a prefix of
 * seven digits or more as a review names a head (`namesHead`). A call recorded before the
 * stand-in kept its tree carries no `head`, and is a run at no commit. */
function ranCheck(call: StandInCall, role: string, commit: string): boolean {
  if (call.as !== "bun" || call.head === undefined) return false;
  if (call.by !== undefined && call.by !== role) return false;
  const args = call.argv.filter((arg) => !arg.startsWith("-"));
  const [script, name] = args[0] === "run" ? args.slice(1) : args;
  return (script?.endsWith("greet.ts") ?? false) && name === "Ada" && namesHead(call.head, commit);
}
/** Whether `result` is its recorder's own run of the world's check (Verification v47's rule 8):
 * a run of it by the recording role in a tree at the result's commit (`ranCheck`), before
 * Dispatch recorded the result. */
export function ownRun(calls: readonly StandInCall[], result: RunResult): boolean {
  const recorded = instant(result.at, "a result's time");
  return calls.some(
    (call) =>
      ranCheck(call, result.role, result.commit) && instant(call.at, "a bun call's time") < recorded
  );
}

/** The reads of a commit `readsCommit` counts, each a `bash` command of the transcript, read
 * within one simple command (up to a `;`, `&`, `|` or line break):
 *   git show … <sha> …        the commit with its patch and its files, `<sha>:<path>` among them
 *   git diff … <sha> …        a diff with the commit at one end, a range (`<a>..<sha>`) too
 *   git log -p … <sha> …      the log's patches through the commit (`-p`, `-u` or `--patch`)
 *   git cat-file … <sha> …    the commit's object
 *   jj show|diff … <sha> …    the same, as jj reads them, the pane's own tool (`jj diff -r <sha>`)
 *   jj file show … <sha> …    one file of the commit
 *   jj log -p … <sha> …       the log's patches through the commit (`-p`, `--patch` or `--git`)
 *   gh pr diff …              the pull request's diff
 *   gh api …/commits/<sha>    the commit's files and patches, as GitHub's REST answers them
 *   gh api …/pulls/<n>/files  the pull request's files, each with its patch
 * git's own options may stand before its subcommand (`-C <dir>`, `-c <name>=<value>`,
 * `--no-pager`, `-P`), and jj's (`-R <dir>`, `--repository <dir>`, `--no-pager`,
 * `--ignore-working-copy`, `--color <when>`). Each git and jj form and the commits route name the
 * commit by a SHA prefix of seven digits or more (namesCommit), so a jj change id, which is no
 * hexadecimal, names none; `gh pr diff` and the files route read the pull request at whatever
 * head it stands, and name no commit. */
const GIT_READ =
  /\bgit(?:\s+(?:-C\s+\S+|-c\s+\S+|--no-pager|-P))*\s+(show|diff|log|cat-file)\b([^;&|\n]*)/g;
const JJ_READ =
  /\bjj(?:\s+(?:-R\s+\S+|--repository\s+\S+|--no-pager|--ignore-working-copy|--color(?:=|\s+)\S+))*\s+(show|diff|log|file\s+show)\b([^;&|\n]*)/g;
const LOG_PATCH = /(?:^|\s)(?:-p|-u|--patch)(?=\s|$)/;
const JJ_LOG_PATCH = /(?:^|\s)(?:-p|--patch|--git)(?=\s|$)/;
const GH_PR_DIFF = /\bgh\s+pr\s+diff\b/;
const GH_COMMIT = /\bgh\s+api\b[^;&|\n]*?\brepos\/[^/\s]+\/[^/\s]+\/commits\/([0-9a-f]{7,40})\b/g;
const GH_PR_FILES = /\bgh\s+api\b[^;&|\n]*?\brepos\/[^/\s]+\/[^/\s]+\/pulls\/\d+\/files\b/;
/** Whether the session read the commit `sha` (Verification v47's k2 `accept` (a)): a read, in its
 * transcript, of the commit's diff, its files or its patch, in one of the forms GIT_READ, JJ_READ,
 * GH_PR_DIFF, GH_COMMIT and GH_PR_FILES name. */
export function readsCommit(entries: SessionEntry[], sha: string): boolean {
  return toolCalls(entries).some(({ tool, command }) => {
    if (tool !== "bash") return false;
    if (GH_PR_DIFF.test(command) || GH_PR_FILES.test(command)) return true;
    if ([...command.matchAll(GH_COMMIT)].some(([, named = ""]) => sha.startsWith(named)))
      return true;
    return (
      [...command.matchAll(GIT_READ)].some(
        ([, verb, rest = ""]) =>
          (verb !== "log" || LOG_PATCH.test(rest)) && namesCommit(rest, [sha])
      ) ||
      [...command.matchAll(JJ_READ)].some(
        ([, verb, rest = ""]) =>
          (verb !== "log" || JJ_LOG_PATCH.test(rest)) && namesCommit(rest, [sha])
      )
    );
  });
}

/** tester-proof: the legion-worker skill's rule for a tester whose predecessor's results hold.
 * worker_fixture's implement record (seed.ts implement-handoff) is the implementer's, and seed.ts
 * records its passing result against the issue's one `before_merge` check, whose words the CLI
 * meets, so the skill's answer is `verified` with a result of the tester's own and no red test to
 * push. Each fact is read from what recorded it: the
 * tester's record from the run's issue on Dispatch (handoffs.json, `seed.ts capture-handoffs`),
 * timed at Dispatch's `created_at`; the accepted completion from the daemon stand-in's log
 * (daemon.log), its first 200 answer to `handoff/complete`, timed at the answer and naming the
 * record the `legion` tool reported; the pushes from the run's own remote. A run passes when all
 * of these hold:
 *   1. before Dispatch recorded the tester's record, the run's `bun` stand-in recorded a run of
 *      greet.ts (the first argument that is not a flag, after an optional `run`): the tester drove
 *      the CLI, whatever command line it wrote to do so. The stand-in records only a bun reached
 *      through PATH: `mise exec bun@… -- bun greet.ts` and a bun named by its absolute path bypass
 *      it, and such a run reads as having run nothing;
 *   2. the accepted completion names a record of the run's issue, at its version, recorded before
 *      the completion: the record comes first, since the `legion` tool's handoff_complete reports
 *      the phase's newest record and refuses when there is none;
 *   3. that record is a `test` record of this issue (the record's `issue_key`, since the body holds
 *      no issue) that this run's own session wrote (the transcript header's id, the session
 *      `dispatch handoff-write` names), whose body is a test handoff (TestHandoff) with
 *      `implementerProof.verdict` `verified` and no failures; and the branch as the tester
 *      completed it (its last push before the completion, if it pushed) changes nothing under the
 *      PR's head: the handoff is never a commit, and the fixture leaves no defect for a red test;
 *   4. the PR body the gh stand-in kept last carries the line `dispatch result` printed for the
 *      tester, `- Check <id> (<when>): <pass|fail> at <commit>, recorded by tester.`, whose
 *      commit is one of this run's own. The line's tail is never read: each label's Dispatch
 *      prints its own (TESTER_LINE).
 * Without an accepted completion, rule 3 reads the run's own newest test record, so the notes say
 * what was wrong with it. A line naming another run's PR head instead is two runs meeting in the
 * shared /tmp, where agents draft the body whatever TMPDIR says: a rig error, thrown, not a
 * failure. `ref`: the agent read skill://legion-worker/references/pr-body.md, where the line is
 * defined. */
function testerProof(runDir: string, run: string, label: string, heads: string[]): Row {
  const world = worldIn(runDir);
  const calls = parsed(path.join(runDir, CALLS), Call);
  const records = json(path.join(runDir, "handoffs.json"), Handoffs);
  const entries = session(runDir);
  const own = ownSession(entries);
  // The `legion` tool's handoff_complete calls it refused, before posting or with the daemon's
  // refusal.
  const refusedIds = new Set(
    entries.flatMap(({ message }) =>
      message?.role === "toolResult" && message.isError === true && message.toolCallId
        ? [message.toolCallId]
        : []
    )
  );
  const refused = entries
    .flatMap((entry) => (entry.message?.role === "assistant" ? (entry.message.content ?? []) : []))
    .filter((part) => {
      const args = part.arguments;
      return (
        part.type === "toolCall" &&
        part.name === "legion" &&
        typeof args === "object" &&
        args !== null &&
        "op" in args &&
        args.op === "handoff_complete" &&
        refusedIds.has(part.id ?? "")
      );
    }).length;
  const complete = daemonLines(runDir).find(
    (line) => line.path === COMPLETE_PATH && line.status === 200
  );
  const completedAt =
    complete === undefined ? undefined : instant(complete.at, "the completion's time");
  const named =
    complete === undefined ? undefined : records.find((r) => r.id === complete.handoffId);
  const record =
    complete === undefined
      ? records.find(
          (r) => r.phase === "test" && r.author.kind === "session" && r.author.id === own
        )
      : named;
  const recordedAt =
    record === undefined ? undefined : instant(record.created_at, `record ${record.id}'s time`);
  const pushes = pushesOf(runDir)
    .map((push) => ({ ...push, at: instant(push.at, "a push's time") }))
    .filter((p) => p.ref === `refs/heads/${world.branch}`);
  const commits = [world.head, ...pushes.map((p) => p.sha)];
  const drove = calls.find((c) => {
    if (c.as !== "bun") return false;
    const args = c.argv.filter((arg) => !arg.startsWith("-"));
    return (args[0] === "run" ? args[1] : args[0])?.endsWith("greet.ts") ?? false;
  });
  const ran =
    drove !== undefined &&
    recordedAt !== undefined &&
    instant(drove.at, "a bun call's time") < recordedAt;
  const ordered =
    named !== undefined &&
    recordedAt !== undefined &&
    completedAt !== undefined &&
    complete?.handoffVersion === named.version &&
    recordedAt < completedAt;
  const problems: string[] = [];
  if (record === undefined) problems.push(`no test record of this run's session on ${world.key}`);
  else {
    if (record.phase !== "test") problems.push(`the record is a ${record.phase} record`);
    if (record.issue_key !== world.key)
      problems.push(`the record is ${record.issue_key}'s, not ${world.key}'s`);
    if (record.author.kind !== "session" || record.author.id !== own)
      problems.push(
        `${record.author.kind} ${record.author.id ?? ""} wrote the record, not this run's session ${own}`
      );
    let body: unknown;
    try {
      body = JSON.parse(record.body);
    } catch {
      body = undefined;
    }
    const read = TestHandoff.safeParse(body);
    if (!read.success) problems.push(`not a test handoff: ${z.prettifyError(read.error)}`);
    else {
      const { implementerProof, failures = [] } = read.data;
      if (implementerProof.verdict !== "verified")
        problems.push(`implementerProof.verdict is ${implementerProof.verdict}`);
      if (failures.length > 0) problems.push(`${failures.length} failures`);
    }
  }
  const tip = pushes.filter((p) => completedAt === undefined || p.at < completedAt).at(-1);
  if (tip !== undefined) {
    const changed = changedFiles(runDir, world.head, tip.sha);
    if (changed.length > 0) problems.push(`the push changed [${changed}]`);
  }
  const proof = problems.length === 0;
  const bodies = calls.filter((c) => c.as === "gh" && c.exit === 0 && c.body !== undefined);
  const edit = bodies.find((c) => c.body?.includes("recorded by tester"));
  const testerLine =
    (bodies.at(-1)?.body ?? "").split("\n").find((line) => TESTER_LINE.test(line)) ?? "";
  const testerAt = TESTER_LINE.exec(testerLine)?.[1] ?? "";
  const ownHead = namesCommit(testerAt, commits);
  const foreign = heads.filter((head) => head !== world.head);
  if (!ownHead && namesCommit(testerAt, foreign))
    throw new Error(`its PR body carries another run's check line: ${testerLine.slice(0, 160)}`);
  const pass = ran && ordered && proof && ownHead;
  const opened = readCalls(entries).map((call) => call.target);
  const ref = opened.some((target) =>
    target.includes("skill://legion-worker/references/pr-body.md")
  );
  const worker = opened
    .filter((target) => target.includes("legion-worker"))
    .map((target) => target.replace("skill://legion-worker", "") || "/");
  const notes = [
    `ran=${ran} record=${record === undefined ? "none" : `v${record.version}`} proof=${proof} pushes=${pushes.length} complete=${complete !== undefined} refusedCompletions=${refused} ordered=${ordered} testerLine=${testerLine !== ""} ownHead=${ownHead}`,
    `editBeforeRecord=${edit !== undefined && recordedAt !== undefined && instant(edit.at, "a gh call's time") < recordedAt}`,
    ...(proof ? [] : [`proofProblems=${JSON.stringify(problems)}`]),
    `refs=[${worker.join(",")}]`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  return { run, scenario: "tester-proof", label, pass, ref, notes: notes.join(" ") };
}

/** Every case rig.sh's run_reviewer_text_findings builds a world for (SCENARIOS). */
const REVIEWER_CASES = [...casesOf("reviewer-text-findings")];
/** The two files worker_fixture puts a text finding in (rig.sh): the document the change makes
 * false, and the workflow whose comment names a trigger the file does not carry. */
const FINDING_DOC = "docs/cli.md";
const FINDING_WORKFLOW = ".github/workflows/";
/** The sentence of FINDING_DOC the change makes false, rig.sh's `text_finding_doc_line`: the line
 * a reviewer's text commit removes, and the one line it may remove (`reviewerTextFindings`). */
const FINDING_SENTENCE = "It prints the name exactly as it was given, spaces and all.";
/** The sentence threads-closed's correct world has the reviewer's own commit write into
 * FINDING_DOC, which says the CLI strips every space from the name where it trims only the spaces
 * around it (rig.sh's approved_world): the line the implementer's push corrects (`threadsClosed`). */
const REVIEWER_SENTENCE = "It strips every space from the name.";
/** The push trigger the workflow's comment names and `on:` does not carry, rig.sh's
 * `text_finding_trigger`: the words a fix of the workflow's thread takes out (`threadsClosed`). */
const PUSH_TRIGGER = "on every push to main";
/** The text of `file` at commit `sha` in the run's own remote, "" where the commit holds no such
 * file. A read that fails otherwise is the rig's, for the reason `changedFiles` gives. */
function fileAt(runDir: string, sha: string, file: string): string {
  const remote = path.join(runDir, "remote.git");
  const git = Bun.spawnSync(["git", "-C", remote, "show", `${sha}:${file}`]);
  if (git.exitCode === 0) return git.stdout.toString();
  const said = git.stderr.toString().trim();
  if (/does not exist in|exists on disk, but not in/.test(said)) return "";
  throw new Error(`git -C ${remote} show ${sha}:${file} exited ${git.exitCode}: ${said}`);
}
/** The world's one check (world-issue.ts's worldVerification), the block id every result the
 * world seeds or a run records names. */
const WORLD_CHECK = "k1";
/** The inline comments of a review submission, as the reviewer's own prompt writes them: one per
 * finding, each on the file its `path` names. The head and the event of a create are read by the
 * rules the gh stand-in answers the same call by (`reviewOf`). */
const Review = z.looseObject({
  comments: z.array(z.looseObject({ path: z.string().optional() })).optional(),
});
/** The event each `gh pr review` flag submits. */
const REVIEW_FLAGS: Record<string, string> = {
  "-a": "APPROVE",
  "--approve": "APPROVE",
  "-r": "REQUEST_CHANGES",
  "--request-changes": "REQUEST_CHANGES",
  "-c": "COMMENT",
  "--comment": "COMMENT",
};
/** One review a run submitted, as the score reads it: the verdict, the head it names, "" for a
 * route that carries none, the file of each inline comment on it, its body, and whether the call
 * that carried it submitted a pending review the round had already created (`submitsReview`). */
type Submission = {
  at: string;
  event: string;
  head: string;
  comments: string[];
  body: string;
  submits: boolean;
};
/** The review `call` submitted, or undefined when it submitted none: the
 * `gh api --method POST repos/{owner}/{repo}/pulls/{n}/reviews` the reviewer's prompt prescribes,
 * on the run's own repository and no other (`reviewsRoute`),
 * whose JSON body comes from `--input <file>` (the stand-in records the file it read),
 * `--input -` (its stdin) or its fields, read as the stand-in reads one (`sentBody`); or a
 * `gh pr review` of that same repository, which carries its event as a flag, names no head and
 * can name no inline comment, and which GitHub takes at the pull request's head when the call is
 * made — the gh stand-in answers it at its remote's tip at that call, and this reads it at
 * `tipAt(call.at)` (`headAt`), where it once read it at `""` and failed `namesHead` on a review
 * the world had answered at the head; or the call that submits a pending review
 * (`submitsReview`), whose body carries the
 * verdict and whose create carries the rest (`reviewsIn`). A call that names the verb GET submits
 * nothing: `gh api repos/…/pulls/<n>/reviews` is the read of the reviews already there, which a
 * round often makes after it submitted, and `gh api repos/…/pulls/<n>/reviews --method GET
 * -f per_page=100` is that same read with its query's parameters written as fields; a reader
 * that took those fields for a body made it the round's last submission, carrying no event — the
 * row of a round that submitted nothing, which is the one thing such a round did not do. The verb
 * is read in every spelling gh takes it in and in any case (`isPost`), since a GET written
 * `--method=GET` read as a call naming none is that same false row.
 *
 * A create is read by the rules the gh stand-in answered it by (gh-call.ts's `createdState` and
 * `createdHead`), so the world and the score read one call one way: a create GitHub refuses
 * submits nothing; one naming no event — no `event`, `event: ""`, or no body at all — is a
 * pending review, carrying the empty event; and one naming no head is at `headAt(call.at)`, the
 * pull request's head when it was made, where the stand-in answered it at its remote's tip at
 * that call. Each side once had a rule of its own: a bodyless create was no review here and a
 * pending review at the head there, so the approval that submitted it named `""` and failed
 * `namesHead`, and `event: ""` was pending here and no pending review there.
 *
 * The repository is read on both routes, since a review of another repository's pull request
 * approves nothing of this one and the stand-in answers such a call as GitHub would answer the
 * right one (`reviewsRoute`): `gh pr review -R other/repo --request-changes` is a review of
 * other/repo, and the repository rule covering the `gh api` route alone counted it as this
 * round's verdict. */
function reviewOf(
  call: z.infer<typeof Call>,
  repo: string,
  tipAt: (at: string) => string
): Submission | undefined {
  if (call.argv[0] === "pr" && call.argv[1] === "review") {
    // The repository the call names, which a call naming none leaves to the checkout it was made
    // in: the run's own clone of the world's repository.
    const named = flagValue(call.argv, ["-R", "--repo"]);
    if (named !== undefined && named !== repo) return undefined;
    const flag = call.argv.find((arg) => REVIEW_FLAGS[arg] !== undefined) ?? "";
    // The body as gh sends it: `--body`, or the file `--body-file` names, read by the stand-in
    // (`-` its stdin).
    const file = flagValue(call.argv, ["-F", "--body-file"]);
    const text =
      flagValue(call.argv, ["-b", "--body"]) ??
      (file === "-" ? call.stdin : file === undefined ? undefined : call.files?.[file]);
    return {
      at: call.at,
      event: REVIEW_FLAGS[flag] ?? "",
      head: tipAt(call.at),
      comments: [],
      body: text ?? "",
      submits: false,
    };
  }
  // gh sends a call carrying a field as a POST and one carrying none as a GET, so the verb is
  // read to tell an explicit GET — a read of the route — from a submission, by the one reader
  // the gh stand-in answers that call with (`isPost`).
  if (call.argv[0] !== "api" || !isPost(call.argv)) return undefined;
  const submits = call.argv.some((arg) => submitsReview(arg, repo) !== undefined);
  if (!submits && !call.argv.some((arg) => reviewsRoute(arg, repo))) return undefined;
  const sent = sentBody(call);
  // A submit that sends nothing names no event, which GitHub refuses, and the stand-in with it.
  if (submits && sent === undefined) return undefined;
  const body = sent ?? {};
  const state = submits ? "" : createdState(body);
  if (state === undefined) return undefined;
  const read = Review.safeParse(body);
  const comments = read.success ? (read.data.comments ?? []) : [];
  const named = typeof body.commit_id === "string" ? body.commit_id : "";
  return {
    // The time the stand-in recorded the call at, which is when the review was submitted: the
    // ordering of this loop's first message is read against it (`toldImplementer`).
    at: call.at,
    event: state === "PENDING" || typeof body.event !== "string" ? "" : body.event,
    head: submits ? named : (createdHead(body, tipAt(call.at)) ?? ""),
    comments: comments.map((comment) => comment.path ?? ""),
    body: typeof body.body === "string" ? body.body : "",
    submits,
  };
}

/** The head the world's pull request was at when a call recorded at `at` began: the commit the
 * last push onto the world's branch before that left there (<run>/pushes.log), or the world's
 * own head before any. It is the tip a create naming no head is read at (`reviewOf`), as the gh
 * stand-in reads it off its remote at the call. */
function headAt(runDir: string, world: World): (at: string) => string {
  const pushes = pushesOf(runDir).filter((push) => push.ref === `refs/heads/${world.branch}`);
  return (at) => {
    const began = instant(at, "a gh call's time");
    return pushes.findLast((push) => instant(push.at, "a push's time") <= began)?.sha ?? world.head;
  };
}

/** Every review `calls` submitted on `repo`'s pull request, in the order the gh stand-in recorded
 * them: a call that submitted none, and one that submitted to another repository, is no review of
 * this run (reviewOf). A create naming no head is read at `tipAt(call.at)` (`headAt`).
 *
 * A call that submits a pending review and the create it submits are one submission and not two:
 * the create carries the head and the inline comments, the submit call carries the verdict, and
 * the pair takes the submit call's own place in the order, which is when the verdict was made.
 * The create it folds in is the last one before it that named no event, which is the one pending
 * review a run can have: GitHub refuses the create of a second while one stands, and the stand-in
 * with it (gh-standin.ts's `reviewCreate`), so the refused call exits 1, and every caller of this
 * reads the calls that exited 0 alone. The pair is read by that order and not by the id the submit
 * names: a world gives each create an id of its own (gh-threads.ts's `createdReviewId`), but the
 * calls this reads carry the id the agent wrote, never the one the create was answered with. A
 * create no submit call follows stays as it is — a pending review is no verdict, and it scores as
 * the empty event it carries. */
function reviewsIn(
  calls: z.infer<typeof Call>[],
  repo: string,
  tipAt: (at: string) => string
): Submission[] {
  const found: Submission[] = [];
  for (const call of calls) {
    const review = reviewOf(call, repo, tipAt);
    if (review === undefined) continue;
    const pending = review.submits
      ? found.findLastIndex((earlier) => !earlier.submits && earlier.event === "")
      : -1;
    if (pending < 0) {
      found.push(review);
      continue;
    }
    const created = found.splice(pending, 1)[0];
    found.push({
      ...review,
      head: review.head === "" ? (created?.head ?? "") : review.head,
      comments: review.comments.length > 0 ? review.comments : (created?.comments ?? []),
      body: review.body === "" ? (created?.body ?? "") : review.body,
    });
  }
  return found;
}
/** The verdict standing on the pull request after `submissions`, or undefined when none does:
 * the last `APPROVE` or `REQUEST_CHANGES`. A `COMMENT` review is no verdict of its own — on
 * GitHub it leaves the approval or the change request before it standing — so a round that
 * approved and then posted a comment review is approved, and one that requested changes and then
 * commented has its change request standing, which is what the defect control reads. A pending
 * review carries no event either and stands for nothing (`reviewsIn`). */
function verdictOf(submissions: Submission[]): Submission | undefined {
  return submissions.findLast(
    (review) => review.event === "APPROVE" || review.event === "REQUEST_CHANGES"
  );
}

/** Everything one recorded gh call could carry a query in: its arguments, its stdin, and each
 * file it read a body from. */
function callText(call: z.infer<typeof Call>): string {
  return [call.argv.join(" "), call.stdin ?? "", ...Object.values(call.files ?? {})].join("\n");
}

/** Whether a recorded gh call listed the pull request's review threads: the `reviewThreads` query
 * `skills/legion-worker/references/review-threads.md` gives, asked of GitHub itself, which is a
 * `gh api` call to the GraphQL endpoint whose text names the field. The endpoint is read as the
 * world reads it (gh-call.ts's `endpointPath`): `graphql`, `/graphql` and `graphql?…` all reach
 * it (gh-threads.ts's `graphqlOf`), and a reader that wanted the argument `graphql` itself missed
 * the other two. A call that only names the field in text it writes — a pull-request body, a
 * comment saying what the reviewer ran — asks GitHub for nothing. The reviewer reads the open
 * threads there rather than from the pull request's body, which is the implementer's record
 * while GitHub is the fact: a run that takes the ids from anywhere else closes the threads that
 * source lists and leaves the rest. One run of 2026-11-06 took them from the gh stand-in's own
 * fixtures file and scored a pass. */
function listsTheThreads(call: z.infer<typeof Call>): boolean {
  return (
    call.as === "gh" &&
    call.argv[0] === "api" &&
    endpointPath(call.argv) === "graphql" &&
    callText(call).includes("reviewThreads")
  );
}

/** The parents of `sha` in the run's own bare remote, as `git rev-list --parents` names them. A
 * read that fails is the rig's, for the reason `changedFiles` gives. */
function parentsOf(runDir: string, sha: string): string[] {
  const remote = path.join(runDir, "remote.git");
  const git = Bun.spawnSync(["git", "-C", remote, "rev-list", "--parents", "-n", "1", sha]);
  if (git.exitCode !== 0)
    throw new Error(
      `git -C ${remote} rev-list --parents -n 1 ${sha} exited ${git.exitCode}: ${git.stderr.toString().trim()}`
    );
  return git.stdout.toString().trim().split(" ").slice(1);
}

/** The address the committer of `sha` carries, in the run's own bare remote (`git log -1
 * --format=%ce`). A read that fails is the rig's, for the reason `changedFiles` gives. */
function committerOf(runDir: string, sha: string): string {
  const remote = path.join(runDir, "remote.git");
  const git = Bun.spawnSync(["git", "-C", remote, "log", "-1", "--format=%ce", sha]);
  if (git.exitCode !== 0)
    throw new Error(
      `git -C ${remote} log -1 --format=%ce ${sha} exited ${git.exitCode}: ${git.stderr.toString().trim()}`
    );
  return git.stdout.toString().trim();
}

/** The lines `to` removes from `file` and the lines it adds there, against `from`, in the run's
 * own bare remote (`git diff -U0`): each line of a hunk, read after the hunk's own header, so a
 * removed line that starts with dashes is a line of the file and never a header. A read that
 * fails is the rig's, for the reason `changedFiles` gives. */
function changedLines(
  runDir: string,
  from: string,
  to: string,
  file: string
): { removed: string[]; added: string[] } {
  const remote = path.join(runDir, "remote.git");
  const git = Bun.spawnSync([
    "git",
    "-C",
    remote,
    "diff",
    "-U0",
    "--no-color",
    from,
    to,
    "--",
    file,
  ]);
  if (git.exitCode !== 0)
    throw new Error(
      `git -C ${remote} diff -U0 ${from} ${to} -- ${file} exited ${git.exitCode}: ${git.stderr.toString().trim()}`
    );
  const removed: string[] = [];
  const added: string[] = [];
  let hunk = false;
  for (const line of git.stdout.toString().split("\n")) {
    if (line.startsWith("@@")) hunk = true;
    else if (hunk && line.startsWith("-")) removed.push(line.slice(1));
    else if (hunk && line.startsWith("+")) added.push(line.slice(1));
  }
  return { removed, added };
}

/** reviewer-text-findings: the reviewer's rule for text findings (spec v36's R1) —
 * `REQUEST_CHANGES` only when a behavioural finding stands; when every finding is text, one commit
 * of the reviewer's own that fixes each finding it can push, no pass recorded at that commit, each
 * finding it cannot push left as an inline thread of a `COMMENT` review, and the implementer told
 * on its role topic, the approval waiting for the implementer's last chance; and on a base whose
 * rules require the last push approved by someone other than its pusher, no commit, the head
 * approved as it stands and the implementer told of each thread. Scored as Verification v47's k1
 * and k3 read it, each lettered bar a term the row's notes report (`terms=`).
 *
 * The score reads the round's submissions, the pushes the run's own remote took, the review
 * threads as the world holds them once the run has ended (`openedThreads`, each shown open or not,
 * gh-threads.ts's `shownOpen`), the results of the run's issue (`resultsOf`), the moves back its
 * daemon logged (`backwardMoves`) and the messages Envoy accepted on its role topics
 * (`ownMessages`), never the pull request's state. The verdict is the round's last `APPROVE` or
 * `REQUEST_CHANGES` (`verdictOf`), and the last review the stand-in recorded when it ended with
 * neither, so the row still names what such a round did submit; and the run's world is rig.sh's
 * worker_fixture under the case <run>/case.txt and the run's directory both name, which must
 * agree. Every case carries the same two text findings: the sentence of docs/cli.md the change
 * makes false (FINDING_SENTENCE), which the review App may push, and the comment of the workflow,
 * which it may not, since it holds no `workflows` permission (the run's remote refuses that push
 * as GitHub does, rig.sh's fixture_first_commit):
 *   text-only  the change's code is correct (k1). A run passes on six terms: (a) the run's remote
 *              took one push, onto the issue branch, of one commit whose parent is the head the
 *              reviewer read (`commit`), which changes docs/cli.md alone, every line it removes
 *              there holding the false sentence and no line it adds holding it (`docFixed`);
 *              (b) a thread the run opened on the workflow file, an inline comment of a `COMMENT`
 *              review or one of its own, is shown open (`workflowOpen`); (c) no result of the
 *              world's check that the reviewer's own session recorded at that commit
 *              (`noOwnResult`); (d) no move back the daemon took (`moved`; each one asked for is
 *              reported with its answer, `moves`); (e) no `APPROVE` and no `REQUEST_CHANGES` among
 *              the round's submissions (`approved`, `requestedAny`); (f) a message of the
 *              reviewer's on the implementer's role topic, after that push, that names the commit
 *              by a SHA prefix of seven digits or more (`namesCommit`, the scorer's, never the
 *              judge's) and the workflow file's thread (`findingNamed`) (`toldImplementer`);
 *   defect     the same change with the CLI's blank-name guard dropped, so a behavioural finding
 *              stands: k1's negative control, which passes only when the verdict is
 *              `REQUEST_CHANGES`, the run's remote took no push, and both text findings are inline
 *              comments of that review (`doc`, `workflow`), since beside a behavioural finding the
 *              reviewer pushes nothing and each text finding is a thread of the change request;
 *   last-push  text-only's world on a base whose rules require the last push approved by someone
 *              other than its pusher (k3). A run passes on four terms: (a) the run's remote took
 *              no push; (b) a thread the run opened on docs/cli.md and one on the workflow file
 *              are each shown open (`docOpen`, `workflowOpen`); (c) the verdict is `APPROVE` of the
 *              head the world was built at, by SHA (`namesHead`); (d) one message of the
 *              reviewer's on the implementer's role topic names both threads (`findingNamed`)
 *              (`toldImplementer`).
 * The rule is every run's, whatever its label: the score is label-blind, as the class search's "a
 * branch on a label's name" holds it. k1 and k3 read their base arms, whose checkout carries
 * main's rule, as the cost this change measures, off the runs whose verdict is `REQUEST_CHANGES`
 * (`requested`, each row's and the arm's count in the report). A call that resolved a thread is
 * reported (`resolveThread`), read from the world's replay as threads-closed reads one
 * (`threadActs`); the world's one seeded thread is a bot's, which the reviewer may resolve.
 * `ref`: the agent read skill://legion-worker/references/merge-gate.md. */
function reviewerTextFindings(runDir: string, run: string, label: string, reviewCase: string): Row {
  const world = worldIn(runDir);
  const calls = parsed(path.join(runDir, CALLS), Call);
  const tipAt = headAt(runDir, world);
  const reviews = reviewsIn(
    calls.filter((call) => call.as === "gh" && call.exit === 0),
    world.repo,
    tipAt
  );
  const review = verdictOf(reviews) ?? reviews.at(-1);
  const resolves = [...threadActs(runDir, world, calls).values()].filter(
    ({ resolved }) => resolved.length > 0
  ).length;
  // Every push the run's remote took, whatever ref it moved: text-only's one push is its text
  // commit, onto the issue branch, and a push anywhere else is a push the rule does not make.
  const pushes = pushesOf(runDir);
  const push =
    pushes.length === 1 && pushes[0]?.ref === `refs/heads/${world.branch}` ? pushes[0] : undefined;
  const changed = push === undefined ? [] : changedFiles(runDir, world.head, push.sha);
  const commit = push !== undefined && parentsOf(runDir, push.sha).join(" ") === world.head;
  const docLines =
    push === undefined ? undefined : changedLines(runDir, world.head, push.sha, FINDING_DOC);
  const docFixed =
    docLines !== undefined &&
    changed.join("\n") === FINDING_DOC &&
    docLines.removed.length > 0 &&
    docLines.removed.every((line) => line.includes(FINDING_SENTENCE)) &&
    docLines.added.every((line) => !line.includes(FINDING_SENTENCE));
  const paths = review?.comments ?? [];
  const onDoc = paths.includes(FINDING_DOC);
  const onWorkflow = paths.some((named) => named.startsWith(FINDING_WORKFLOW));
  // The verdict names the head the pull request was at when it was submitted.
  const head = review !== undefined && namesHead(tipAt(review.at), review.head);
  const entries = session(runDir);
  const sender = ownSession(entries);
  // The threads the run opened on each finding's file, which the world did not seed, as the world
  // holds them once the run has ended (`openedThreads`), whose node ids also name a finding in a
  // message; a finding is left open where one of them is shown open.
  const opened = openedThreads(runDir, world);
  const workflowThreads = opened.filter((thread) =>
    String(thread.path).startsWith(FINDING_WORKFLOW)
  );
  const docThreads = opened.filter((thread) => thread.path === FINDING_DOC);
  const workflowOpen = workflowThreads.some(shownOpen);
  const docOpen = docThreads.some(shownOpen);
  const approved = reviews.some((one) => one.event === "APPROVE");
  const requestedAny = reviews.some((one) => one.event === "REQUEST_CHANGES");
  const requested = review?.event === "REQUEST_CHANGES";
  // No result of the world's check that the reviewer's own session recorded at its own commit
  // (`resultsOf`), and each move back the run asked the daemon for, with its answer
  // (`backwardMoves`), of which one the daemon took is a move.
  const noOwnResult = !resultsOf(runDir).some(
    (result) =>
      result.check === WORLD_CHECK &&
      result.session === sender &&
      push !== undefined &&
      namesHead(push.sha, result.commit)
  );
  const moves = backwardMoves(runDir);
  const moved = moves.find((move) => move.status === 200);
  // The wake: the first message of the reviewer's on the implementer's role topic that names what
  // the case has it name, in one message, since the implementer acts on the message it is handed,
  // with its id and how it named each finding (`findingNamed`). The names judge's readings are read
  // at the first message that names a finding in no word the scorer reads, and never before.
  const judged = once(() => workerReadings(runDir, "reviewer-text-findings", reviewCase).names);
  const workflow: Finding = {
    named: FINDING_WORKFLOW,
    files: [...new Set([WORKFLOW_FILE, ...workflowThreads.map((thread) => String(thread.path))])],
    threads: workflowThreads,
  };
  const doc: Finding = { named: FINDING_DOC, files: [FINDING_DOC], threads: docThreads };
  const wanted =
    reviewCase === "text-only" ? [workflow] : reviewCase === "last-push" ? [doc, workflow] : [];
  let toldBy: string[] | undefined;
  for (const message of ownMessagesTo(runDir, sender, FIXES)) {
    if (wanted.length === 0) break;
    const { sent } = message;
    if (
      reviewCase === "text-only" &&
      (push === undefined ||
        instant(sent.at, "a message's time") <= instant(push.at, "a push's time") ||
        !namesCommit(sent.message, [push.sha]))
    )
      continue;
    const how: string[] = [];
    for (const finding of wanted) {
      const named = findingNamed(message, finding, judged);
      if (named === undefined) break;
      how.push(named);
    }
    if (how.length === wanted.length) {
      toldBy = [message.id, ...how];
      break;
    }
  }
  const toldImplementer = toldBy !== undefined;
  const terms: Record<string, boolean> =
    reviewCase === "text-only"
      ? {
          a: commit && docFixed,
          b: workflowOpen,
          c: noOwnResult,
          d: moved === undefined,
          e: !approved && !requestedAny,
          f: toldImplementer,
        }
      : reviewCase === "last-push"
        ? {
            a: pushes.length === 0,
            b: docOpen && workflowOpen,
            c: review?.event === "APPROVE" && namesHead(world.head, review.head),
            d: toldImplementer,
          }
        : { requested, noPush: pushes.length === 0, doc: onDoc, workflow: onWorkflow };
  const pass = Object.values(terms).every(Boolean);
  const records = json(path.join(runDir, "handoffs.json"), Handoffs).filter(
    (record) => record.phase === "review"
  ).length;
  const ref = readCalls(entries).some((call) =>
    call.target.includes("skill://legion-worker/references/merge-gate.md")
  );
  const notes = [
    `case=${reviewCase} event=${review?.event || "none"} head=${head} doc=${onDoc} workflow=${onWorkflow} toldImplementer=${toldImplementer} submissions=${reviews.length} resolveThread=${resolves} pushes=${pushes.length} reviewRecords=${records}`,
    termsNote(terms),
    `commit=${commit} docFixed=${docFixed} workflowOpen=${workflowOpen} docOpen=${docOpen} approved=${approved} requestedAny=${requestedAny} moved=${moved === undefined ? "none" : (moved.to ?? "")}`,
    `toldBy=${JSON.stringify(toldBy ?? [])}`,
    `commentPaths=${JSON.stringify(paths)} changed=${JSON.stringify(changed)} requested=${requested}`,
    `noOwnResult=${noOwnResult} moves=${JSON.stringify(moves.map((move) => `${move.to ?? ""} ${answerOf(move)}`))}`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  // The row's scenario carries the case, as an askgate row's does, so the counts are per case and
  // label and they key as the rig errors of the same pair do.
  return {
    run,
    scenario: `reviewer-text-findings:${reviewCase}`,
    label,
    pass,
    ref,
    notes: notes.join(" "),
    requested,
  };
}

/** One row of the names judge's answer (names.json): whether a text names a thing it was given. */
type NamesRow = z.infer<typeof NamesOutput>["rows"][number];
/** A finding a wake names (`findingNamed`): the path, or the directory, every name of it by path
 * holds; the files the names judge was given it by (`workerGivens`' `file:<path>`); and the
 * threads the run opened on it, each named by its node id. */
interface Finding {
  readonly named: string;
  readonly files: readonly string[];
  readonly threads: readonly Json[];
}
/** How `message` names `finding`, or undefined where it does not: by its path or one of its
 * threads' node ids in the message's body, which the scorer reads; or else, as Verification v47's
 * rule 8 has a text that names a file another way read, by the names judge's reading of the
 * message against a file it was given the finding by (`judged`, `workerReadings`), with the words
 * that judge quoted. */
function findingNamed(
  message: { readonly id: string; readonly sent: Publish },
  finding: Finding,
  judged: () => ReadonlyMap<string, NamesRow>
): string | undefined {
  const body = message.sent.message;
  if (body.includes(finding.named)) return "path";
  if (finding.threads.some((thread) => body.includes(String(thread.id)))) return "node id";
  for (const file of finding.files) {
    const row = judged().get(`${message.id} file:${file}`);
    if (row?.names === true) return `judge: ${row.quote ?? ""}`;
  }
  return undefined;
}

/** Every case rig.sh's run_threads_closed builds a world for (SCENARIOS). */
const THREADS_CASES = [...casesOf("threads-closed")];
/** <run>/threads.json, rig.sh's worker_fixture: the threads the reviewer left open (correct's one,
 * an inline comment of its `COMMENT` review; code-defect's one, of its approval; none in accept and
 * crossing), each with the node id the `reviewThreads` route serves it under and the file its
 * finding is on. */
const Threads = z.array(z.looseObject({ id: z.string(), path: z.string() }));
/** The role a message of this loop is addressed to, which its topic ends with: a role topic is
 * `notifications.role.legion-<project>-<issue>-<role>` (contracts' roleToken, rig.sh's
 * run_project). One word names the role twice over, since it is also the role rig.sh booted an
 * agent as, which every gh call that agent made carries (its pane's LEGION_ROLE, gh-standin.ts's
 * `by`). */
const ROLES = ["implementer", "reviewer", "architect"] as const;
type Role = (typeof ROLES)[number];
/** The role of each half of the loop that answers a reviewer's text findings: the implementer,
 * which answers the reviewer's commit and fixes the threads it left (FIXES), and the reviewer,
 * which resolves them (RESOLVES). */
const FIXES: Role = "implementer";
const RESOLVES: Role = "reviewer";
/** Whether `named` is `sha`: the prefix of seven hexadecimal digits or more that the commit
 * starts with, which is how a review names the head it was submitted on. A shorter prefix names
 * too many commits to be a head, and a route that carries no head at all names "". */
function namesHead(sha: string, named: string): boolean {
  return named.length >= 7 && sha.startsWith(named);
}
/** A daemon answer the agent's own plugin refused, as `LegionDaemonContractError` words it
 * (`packages/pi-shared/src/daemon-client.ts`): the route, the status and what of the body that
 * checkout's contract does not describe. A session prints it wherever the call was made, so it
 * is read from the transcript's whole text rather than from one tool's results. */
const CONTRACT_REFUSED =
  /([A-Z]+ \/legion\/\S+) answered \d+ with a body the daemon API contract does not describe: ([^"\\\n]+)/;
/** Every word of a transcript: each text part of each message, the tool results among them. */
function transcriptText(entries: SessionEntry[]): string {
  return entries
    .flatMap((entry) => (entry.message?.content ?? []).map((part) => part.text ?? ""))
    .join("\n");
}
/** One line of `<run>/published.jsonl`. The file is the recorder's own record of its run as much
 * as of the run's messages (record-publishes.ts): it opens with `recording`, written once the
 * subscription is registered with the server, ends with `closed`, written when the rig stops it,
 * and holds one message between them. A reader that took a short file for a quiet run would
 * score a recorder that died as a loop whose agents sent nothing, so `unscored` requires both
 * ends before any send of the run is read. */
const PublishedMessage = z.looseObject({
  at: z.string(),
  topic: z.string(),
  message: z.string(),
  sender: z.string(),
});
const PublishedLine = z.union([
  z.looseObject({ at: z.string(), recording: z.string() }),
  z.looseObject({ at: z.string(), closed: z.string() }),
  z.looseObject({ at: z.string(), unreadable: z.string() }),
  PublishedMessage,
]);
/** A message that reached a role topic: the fact, not an intention read out of an agent's code. */
type Publish = z.infer<typeof PublishedMessage>;
/** How the run's record stands: whether the recorder said it had subscribed, whether it said it
 * was stopped, and the first message it could not read, each of which decides whether any send of
 * the run can be read at all (`unscored`). */
function recordState(runDir: string): {
  opened: boolean;
  closed: boolean;
  unreadable: string | undefined;
} {
  const lines = parsed(path.join(runDir, "published.jsonl"), PublishedLine);
  return {
    opened: lines.some((line) => "recording" in line),
    closed: lines.some((line) => "closed" in line),
    unreadable: lines
      .flatMap((line) => ("unreadable" in line ? [String(line.unreadable)] : []))
      .at(0),
  };
}
/** The session id a transcript names, which is who its agent's messages were sent as. A
 * transcript that names none is the rig's fault, as a line that does not parse is, so this
 * throws rather than returning a blank that another blank could match: `scoreRuns` turns a throw
 * into a rig error naming the run. */
function ownSession(entries: SessionEntry[]): string {
  const id = sessionId(entries);
  if (id === undefined)
    throw new Error("the transcript names no session, so no message can be read as its agent's");
  return id;
}
/** The messages one agent sent on a role topic of this run, in the order Envoy accepted them:
 * those whose recorded sender is `sender`.
 *
 * What the servers did is what is read: the rig subscribes to the bus for the length of a run
 * and appends every message Envoy published on one of this run's role topics, and the score and
 * the harness read that one file, so no publish can score a wake the next agent never had
 * (`rig.sh`'s reviewer_wake calls `score.ts published`). Why it is read there and not from the
 * agent's own transcript: record-publishes.ts's header.
 *
 * Who sent a message is the sender Envoy recorded (`source_session`) against the session id of
 * that half's own transcript, and nothing else: the rig writes no mark of its own into the
 * record, so one writer decides one fact. The caller passes that id, since every caller already
 * holds the transcript it means — a second rule for finding a half's transcript here would be a
 * second answer to one question, and a missing one would read as another half's messages. A run
 * whose transcript names no session is a rig error, which `ownSession`'s throw makes of it. */
function publishes(runDir: string, sender: string): Publish[] {
  // The record's other kinds are the recorder's own words about its run (`recordState`). Each
  // carries a key of its own and none of a message's, so the line `parsed` already read tells a
  // message from them and nothing is parsed twice.
  return parsed(path.join(runDir, "published.jsonl"), PublishedLine).filter(
    (line): line is Publish => "message" in line && line.sender === sender
  );
}
/** Whether a message `sender` sent reached `role`'s topic naming every one of `names`. The
 * daemon starts no phase for the implementer's last chance (spec v36's R2: "The daemon starts no
 * phase for this read"), so the reviewer and the architect learn of the implementer's push from
 * these messages alone, and the architect of the head the reviewer approved from the reviewer's:
 * a step that sends none leaves the next role never learning its turn began. The role is read
 * from the topic the message reached and from nothing else: a message to the architect that says
 * "the reviewer can resolve these" is no wake of the reviewer. `names` are the strings the rule
 * says the message must carry, all of them in the one message, since Envoy hands the role that
 * reads it each message on its own; they are matched against the body alone for the same reason,
 * and a commit is named by any prefix of seven hexadecimal digits or more, as a reply names one
 * (namesCommit). A caller with nothing to require passes an empty list, which no message
 * satisfies rather than every message satisfying it. `after` is a time Envoy's own record is
 * ordered against, so a message that reached the topic before it is no answer to what happened
 * then; both times are read through `instant`, where a time that does not parse is the rig's
 * fault, since a message whose record carries no time would otherwise sort before everything that
 * happened. A caller that orders a message against nothing passes no `after` and reads no time at
 * all. */
function publishedTo(
  runDir: string,
  sender: string,
  role: Role,
  names: readonly string[],
  after?: string
): boolean {
  const from = after === undefined ? undefined : instant(after, "the time a message must follow");
  return (
    names.length > 0 &&
    publishes(runDir, sender).some(
      (sent) =>
        sent.topic.endsWith(`-${role}`) &&
        (from === undefined || instant(sent.at, "a message's time") > from) &&
        names.every((name) => bodyNames(sent.message, name))
    )
  );
}
/** Whether `message` names `name`: the string itself, or, when `name` is a commit, any prefix of
 * it of seven hexadecimal digits or more, as a reply names one (namesCommit). "" is no name and
 * no message carries it, so a caller with nothing to require requires nothing of every message
 * rather than everything of none. */
function bodyNames(message: string, name: string): boolean {
  return name !== "" && (message.includes(name) || namesCommit(message, [name]));
}
/** What the run's review threads are replayed in (gh-threads.ts's `ThreadWorld`): the world's own
 * routes (<run>/fixtures.json, which rig.sh writes for every run and whose graph holds the
 * threads the world seeds) and its world.json. */
function threadWorldOf(runDir: string, world: World): ThreadWorld {
  const fixtures = JSON.parse(readFileSync(path.join(runDir, "fixtures.json"), "utf8")) as {
    routes?: ThreadRoute[];
  };
  return { routes: fixtures.routes ?? [], world };
}
/** The review threads the run's gh calls opened, as the world holds them once the run has ended:
 * the replay both stand-ins answer every thread read from (gh-threads.ts's `threadState`), over
 * the world's own routes (`threadWorldOf`) and every call of the record, less the threads the
 * world seeded. Each carries the node id GitHub gives it, the path it is on and whether a call of
 * the run resolved it. */
function openedThreads(runDir: string, world: World): Json[] {
  const at = threadWorldOf(runDir, world);
  const seeded = new Set(threadState(at, []).list.map((thread) => thread.id));
  const calls = recordedCalls(path.join(runDir, CALLS));
  return threadState(at, calls).list.filter((thread) => !seeded.has(thread.id));
}
/** What one call did to the review threads, as the world holds it: the node id of each thread
 * the world resolved at it. */
interface ThreadActs {
  readonly resolved: readonly string[];
}
/** What each call of `record` did to the review threads, for each call that resolved one. The
 * world keeps no record of which thread a call resolved, so this replays it as both stand-ins do
 * (gh-threads.ts's `threadState`: the world's own threads, then `threadCall` for each call of the
 * record in the order the run made them, every role's and every refused one among them) over the
 * world's own routes (`threadWorldOf`), and reads what each call did there. `record` is every call
 * of <run>/calls.jsonl, which are the lines the stand-ins replay (`recordedCalls`). A call
 * resolved each thread the world answered one of its `resolveReviewThread` fields with, the thread
 * that field's `input.threadId` names (an id the call read from a file among them, as the record
 * keeps that file's text), once however many fields named it, and none where the world has no
 * such thread. A node id the call's text names anywhere else (a `--jq` selector, a comment,
 * another query) resolves nothing. */
function threadActs(
  runDir: string,
  world: World,
  record: readonly z.infer<typeof Call>[]
): Map<z.infer<typeof Call>, ThreadActs> {
  const at = threadWorldOf(runDir, world);
  const state = threadState(at, []);
  const acts = new Map<z.infer<typeof Call>, ThreadActs>();
  for (const call of record) {
    const answer = threadCall(at, state, call);
    const resolved =
      answer !== undefined && "fields" in answer
        ? answer.fields.flatMap(({ payload }) =>
            isObject(payload) && isObject(payload.thread) ? [String(payload.thread.id)] : []
          )
        : [];
    if (resolved.length > 0) acts.set(call, { resolved: [...new Set(resolved)] });
  }
  return acts;
}
/** Every message of this run that reached `role`'s topic, in the order Envoy accepted them, as
 * the rig's own reader wants them: what the next agent is handed, since Envoy delivers each one
 * and a half given only the last would be missing a wake its own run sent. */
function publishesTo(runDir: string, role: Role): Publish[] {
  // The run's first agent's own messages: the harness hands these to the half the rig boots next
  // (rig.sh's reviewer_wake), whose wake is the agent before it and nobody else, and the score
  // reads that half's sends under the same filter.
  return publishes(runDir, ownSession(session(runDir))).filter((sent) =>
    sent.topic.endsWith(`-${role}`)
  );
}
/** The text of `call` when it is a `gh pr comment` on the world's pull request, the call the
 * implementer's role prompt gives its answer to a reviewer's text commit by, or undefined when it
 * is none: its arguments, its stdin and each file it read a body from (`callText`). A call naming
 * another repository, or naming pull requests none of which is the world's (by number, URL or
 * branch), comments on none of this run's; one naming none comments on the branch's own. */
function prComment(call: z.infer<typeof Call>, world: World): string | undefined {
  if (call.argv[0] !== "pr" || call.argv[1] !== "comment") return undefined;
  const repo = flagValue(call.argv, ["-R", "--repo"]);
  if (repo !== undefined && repo !== world.repo) return undefined;
  const own = (arg: string) =>
    arg === String(world.pr) || arg.endsWith(`/pull/${world.pr}`) || arg === world.branch;
  const named = call.argv.slice(2).filter((arg) => /^\d+$|\/pull\/\d+$/.test(arg) || own(arg));
  return named.length === 0 || named.some(own) ? callText(call) : undefined;
}

/** The scenarios whose agents' texts the rig's judge reads (judge.ts's `judgeWorker`). */
export const JUDGED_SCENARIOS: readonly string[] = ["reviewer-text-findings", "threads-closed"];
/** The three worker judges' answer files (judge-prompts.ts's acceptsPrompt, namesPrompt and
 * testingPrompt), in the order judge.ts writes them. */
const WORKER_FILES = ["accepts.json", "names.json", "testing.json"] as const;
/** The judge files each case of a judged scenario reads, through `workerReadings`, at the terms
 * that need a judge's reading: threads-closed's accept the accepts judge's ((d), `accepted`) and
 * the names judge's ((c), `judgedTester`); crossing all three ((a) the names judge's,
 * `judgedTester`; (b) the accepts judge's, `accepted`, and the testing and names judges',
 * `backTo`); correct none; code-defect the names judge's alone (`told`, through `findingNamed`);
 * reviewer-text-findings' text-only and last-push the names judge's (`toldImplementer`, through
 * `findingNamed`), and defect none, since its rule asks for no wake. */
const CASE_JUDGES: Readonly<Record<string, Readonly<Record<string, readonly string[]>>>> = {
  "reviewer-text-findings": {
    "text-only": ["names.json"],
    defect: [],
    "last-push": ["names.json"],
  },
  "threads-closed": {
    accept: ["accepts.json", "names.json"],
    correct: [],
    crossing: ["accepts.json", "names.json", "testing.json"],
    "code-defect": ["names.json"],
  },
};
/** The judge files a run of `scenario` in `runCase` reads (CASE_JUDGES): every worker judge's
 * file for a case the table does not name, or none (`undefined`, a run whose case cannot be read).
 * judge.ts's `judgeWorker` starts a judge only for one of these files and writes every other with
 * no row, which `workerCoverage` takes as covering; a file of one of these that holds no row for a
 * text the run's agents wrote is a rig error, as every uncovered answer is. */
export function caseJudges(scenario: string, runCase: string | undefined): readonly string[] {
  return (runCase === undefined ? undefined : CASE_JUDGES[scenario]?.[runCase]) ?? WORKER_FILES;
}
/** One text an agent of a worker run wrote, as the rig's judge is given it (texts.json) and its
 * answers name it (Verification v47's rule 8): a message one of the run's agents sent on a role
 * topic (`message-<n>`, in the order Envoy accepted them), a `gh pr comment` on the world's pull
 * request (`comment-<n>`, in the order the gh stand-in recorded them; its body as gh sends it,
 * else the call's whole text), or the observed text of a result one of the run's agents recorded
 * (`result-<n>`, in results.json's order), where a pass recorded as holding gives its reason. */
export interface WorkerText {
  id: string;
  kind: "message" | "comment" | "result";
  body: string;
}
/** One thing the names judge looks for in each text (given.json): the world's check, a file a
 * finding of the world is on, with each review thread on it, or a result recorded before the run's
 * agents started, by a session none of them is. */
export interface WorkerGiven {
  id: string;
  kind: (typeof NAMED_KINDS)[number];
  what: string;
}
/** The session of each agent the run booted: the first one's transcript, and the reviewer half's
 * (`<run>/sessions/reviewer`) where the run booted one. */
function runSessions(runDir: string): string[] {
  const second = sessionIn(path.join(runDir, "sessions", RESOLVES));
  return [ownSession(session(runDir)), ...(second.length === 0 ? [] : [ownSession(second)])];
}
/** Every message an agent the run booted (`runSessions`) sent on a role topic of the run, in the
 * order Envoy accepted them, each with the id the rig's judge names it by (`message-<n>`,
 * WorkerText): the one numbering of a run's messages, which `workerTexts` gives the judge and a
 * scorer reads the judge's answers by. */
function ownMessages(runDir: string): { id: string; sent: Publish }[] {
  const own = runSessions(runDir);
  return parsed(path.join(runDir, "published.jsonl"), PublishedLine)
    .filter((line): line is Publish => "message" in line && own.includes(String(line.sender)))
    .map((sent, index) => ({ id: `message-${index + 1}`, sent }));
}
/** The messages of `ownMessages` that `sender` sent on `role`'s topic, each with its id. */
function ownMessagesTo(
  runDir: string,
  sender: string,
  role: Role
): { id: string; sent: Publish }[] {
  return ownMessages(runDir).filter(
    ({ sent }) => sent.sender === sender && sent.topic.endsWith(`-${role}`)
  );
}
/** Every result an agent the run booted (`runSessions`) recorded, in results.json's order, each
 * with the id the rig's judge names it by (`result-<n>`, WorkerText): the one numbering of a run's
 * own results, which `workerTexts` gives the judge and a scorer reads the judge's answers by. */
function ownResults(runDir: string): { id: string; result: RunResult }[] {
  const own = runSessions(runDir);
  return resultsOf(runDir)
    .filter((result) => own.includes(result.session))
    .map((result, index) => ({ id: `result-${index + 1}`, result }));
}
/** Every result no agent the run booted recorded, the seeded passes among them, in results.json's
 * order, each with the id the names judge is given it by (`earlier-<n>`, WorkerGiven): the one
 * numbering of a run's earlier results, which `workerGivens` gives the judge and a scorer reads
 * the judge's answers by. */
function earlierResults(runDir: string): { id: string; result: RunResult }[] {
  const own = runSessions(runDir);
  return resultsOf(runDir)
    .filter((result) => !own.includes(result.session))
    .map((result, index) => ({ id: `earlier-${index + 1}`, result }));
}
/** Every text the run's agents wrote that the judge reads (WorkerText). */
export function workerTexts(runDir: string): WorkerText[] {
  const world = worldIn(runDir);
  const results = ownResults(runDir);
  const comments = parsed(path.join(runDir, CALLS), Call).flatMap((call) => {
    if (call.as !== "gh" || call.exit !== 0 || prComment(call, world) === undefined) return [];
    const file = flagValue(call.argv, ["-F", "--body-file"]);
    const body =
      flagValue(call.argv, ["-b", "--body"]) ??
      (file === "-" ? call.stdin : file === undefined ? undefined : call.files?.[file]);
    return [body ?? callText(call)];
  });
  return [
    ...ownMessages(runDir).map(({ id, sent }) => ({
      id,
      kind: "message" as const,
      body: sent.message,
    })),
    ...comments.map((body, index) => ({
      id: `comment-${index + 1}`,
      kind: "comment" as const,
      body,
    })),
    ...results.map(({ id, result }) => ({
      id,
      kind: "result" as const,
      body: result.observed,
    })),
  ];
}
/** <run>/fixture.json as far as the names judge's givens read it: what the issue's one check
 * covers (seed-handoffs.ts's TesterFixture, world-issue.ts's CheckCovers, `r2` where the fixture
 * predates the field). */
const CheckFixture = z.looseObject({ covers: z.enum(CHECK_COVERS).default("r2") });
/** What the names judge looks for in each text of the run (WorkerGiven): the world's check, in
 * the words its block gives what it runs (crossing's compares the command's output with the
 * example line of docs/cli.md: world-issue.ts's `doc-example`; a run with no fixture.json seeded
 * no issue, and its check is the command alone); the document the change makes false, the
 * world's workflow file and the file of each thread of <run>/threads.json or that the run opened,
 * each with the review threads on it; and each result no agent of the run recorded, in
 * results.json's order (`earlier-<n>`). */
export function workerGivens(runDir: string): WorkerGiven[] {
  const world = worldIn(runDir);
  const seeded = path.join(runDir, "threads.json");
  const threads = [
    ...(existsSync(seeded) ? json(seeded, Threads) : []),
    ...openedThreads(runDir, world).map((thread) => ({
      id: String(thread.id),
      path: String(thread.path),
    })),
  ];
  const files = [...new Set([FINDING_DOC, WORKFLOW_FILE, ...threads.map((thread) => thread.path)])];
  const earlier = earlierResults(runDir);
  const fixture = path.join(runDir, "fixture.json");
  const covers = existsSync(fixture) ? json(fixture, CheckFixture).covers : "r2";
  return [
    {
      id: "check",
      kind: "check",
      what:
        covers === "doc-example"
          ? `check ${WORLD_CHECK} of the issue's Verification document, which runs \`bun greet.ts Ada\` and compares its output with the example line ${FINDING_DOC} gives for that command`
          : `check ${WORLD_CHECK} of the issue's Verification document, which runs \`bun greet.ts Ada\``,
    },
    ...files.map((file) => {
      const on = threads.filter((thread) => thread.path === file).map((thread) => thread.id);
      return {
        id: `file:${file}`,
        kind: "file" as const,
        what:
          on.length === 0
            ? `the file ${file}`
            : `the file ${file}, and the review ${on.length === 1 ? "thread" : "threads"} ${on.join(", ")} on it`,
      };
    }),
    ...earlier.map(({ id, result }) => ({
      id,
      kind: "result" as const,
      what: `the ${result.role}'s ${result.verdict} of check ${result.check} at commit ${result.commit}, recorded at ${result.at}, which observed: ${result.observed}`,
    })),
  ];
}
/** The commit the accepts judge reads each text of a run of `scenario` against, and whose earlier
 * passes the testing judge's reading is of: the world's head, the reviewer's own commit, which
 * threads-closed's implementer reads as its last chance (k2 `accept` (d), k3 `crossing` (b));
 * none for reviewer-text-findings, whose bars accept no commit and send no issue back to testing,
 * and whose accepts.json and testing.json hold no row. */
export function workerCommit(runDir: string, scenario: string): string | undefined {
  return scenario === "threads-closed" ? worldIn(runDir).head : undefined;
}
/** What a worker judge's answer to `file` leaves out of, repeats from or adds to what judge.ts gave
 * its judge; empty when it covers its input exactly. accepts.json and testing.json each hold one
 * row for each text, where there is a commit the run's agents decide at, and none where there is
 * not; names.json one row for each text and each thing given. A file of a judge the run's case
 * does not read (`reads`, `caseJudges`) that holds no row covers it, since judge.ts starts no
 * judge for it and writes it with none; one that holds rows, as a judging written before that
 * rule holds them, is held to its input as every other is. judge.ts, after each answer parses,
 * and `workerReadings` use it. */
export function workerCoverage(
  file: string,
  answer: unknown,
  texts: readonly WorkerText[],
  givens: readonly WorkerGiven[],
  commit: string | undefined,
  reads: readonly string[]
): string[] {
  const ids = texts.map((text) => text.id);
  const unread = (rows: readonly unknown[]) => rows.length === 0 && !reads.includes(file);
  if (file === "accepts.json" || file === "testing.json") {
    const rows = (file === "accepts.json" ? AcceptsOutput : TestingOutput).parse(answer).rows;
    return unread(rows)
      ? []
      : covered(
          "text",
          commit === undefined ? [] : ids,
          rows.map((row) => row.id),
          true
        );
  }
  const rows = NamesOutput.parse(answer).rows;
  return unread(rows)
    ? []
    : covered(
        "text and thing",
        ids.flatMap((id) => givens.map((given) => `${id} ${given.id}`)),
        rows.map((row) => `${row.id} ${row.given}`),
        true
      );
}
/** The judge's readings of a worker run (`workerReadings`): by text id, whether it accepts the
 * run's commit and whether it says the issue goes back to testing; by `<text id> <thing id>`,
 * whether it names that thing. */
export interface WorkerReadings {
  readonly accepts: ReadonlyMap<string, z.infer<typeof AcceptsOutput>["rows"][number]>;
  readonly names: ReadonlyMap<string, NamesRow>;
  readonly testing: ReadonlyMap<string, z.infer<typeof TestingOutput>["rows"][number]>;
}
/** The judge's readings of a worker run of `scenario` in `runCase` (judge.ts's `judgeWorker`),
 * which a judged term records in its row, as rule 8 has it: for each text (`workerTexts`),
 * whether it accepts the commit `workerCommit` names, and whether it says that the issue goes back
 * to testing, each keyed by the text's id, the second read only where there is such a commit, as
 * judge.ts asks it only there; and whether it names each thing `workerGivens` lists, keyed
 * `<text id> <thing id>`; each with the words the judge quoted. A file missing, malformed, or not
 * covering what judge.ts gave its judge (`workerCoverage`, under the files the case reads,
 * `caseJudges`) is a rig error that `rig.sh judge` fixes, as a spec run's judge file is
 * (`judged`). */
export function workerReadings(runDir: string, scenario: string, runCase: string): WorkerReadings {
  const texts = workerTexts(runDir);
  const givens = workerGivens(runDir);
  const commit = workerCommit(runDir, scenario);
  const reads = caseJudges(scenario, runCase);
  const read = <T>(file: string, schema: z.ZodType<T>): T =>
    judgeAnswer(runDir, file, schema, (answer) =>
      workerCoverage(file, answer, texts, givens, commit, reads)
    );
  return {
    accepts: new Map(read("accepts.json", AcceptsOutput).rows.map((row) => [row.id, row])),
    names: new Map(
      read("names.json", NamesOutput).rows.map((row) => [`${row.id} ${row.given}`, row])
    ),
    testing: new Map(
      commit === undefined
        ? []
        : read("testing.json", TestingOutput).rows.map((row) => [row.id, row])
    ),
  };
}

/** threads-closed: the implementer's last chance at a reviewer's text commit and the reviewer's
 * answer to its push (spec v36's R2, which the implementer's and the reviewer's role prompts
 * state), scored as Verification v47's k2 and k3 read it, each lettered bar a term the row's notes
 * report (`terms=`), over the agents one run boots: the implementer first, then the reviewer
 * where the case has one (rig.sh's run_threads_closed).
 *
 * The score reads what the run wrote, never the stand-in's static routes, which answer the same
 * whatever the run did: the gh calls the stand-in recorded (calls.jsonl), each carrying the role
 * that made it, and what each of them did to the threads, replayed from those calls over the
 * threads the world seeds (`threadActs`); the bun stand-in's runs of the world's check (`ownRun`);
 * the pushes the run's own bare remote logged (pushes.log), the diff and the files they carry and
 * the role App whose bot committed each (`fixPushes`); the results of the run's issue
 * (`resultsOf`); the moves back its daemon logged (`backwardMoves`); the messages Envoy accepted on
 * the run's role topics (`ownMessages`); the rig's judge's readings of what the agents wrote
 * (`workerReadings`, read at the first term that needs one, as Verification v47's rule 8 has a
 * commit accepted and a check, a file or an earlier result named decided, and k3's `crossing` (b)
 * the issue sent back to testing); and the agents' transcripts. The world's head is the reviewer's
 * own text commit in accept, correct and crossing, which no approval stands on and whose message
 * on the implementer's role topic is the run's prompt, and the implementer's commit in
 * code-defect, which the reviewer's approval stands on; the tester's pass of the world's check
 * stands at the implementer's commit (an earlier result, `earlier-<n>` to the names judge); the
 * threads the reviewer left are the world's own (<run>/threads.json); and the case is the one
 * <run>/case.txt and the run's directory both name, which must agree.
 *   accept       the reviewer's commit is right and it left no thread (k2). The implementer's
 *                terms: (a) its transcript reads the commit (`readsCommit`); (b) a `gh pr comment`
 *                on the pull request names the commit (`commented`, `prComment`); (c) a result of
 *                the world's check at the commit by its session, its own run of the check there
 *                (`ownRun`), or a pass recorded as holding whose reason names the tester's result,
 *                by the names judge, and its commit, by a SHA prefix of 7 or more (`namesCommit`);
 *                (d) a message of its own on the reviewer's role topic and one on the
 *                architect's, each naming the commit by SHA and accepting it, by the accepts judge
 *                (`accepted`); (e) no push, no thread resolved by any call of the run, and no move
 *                back the daemon took. The row also says whether it recorded the pass as holding
 *                with no run of the check at the commit (`heldNoRun`), which k3's second control
 *                counts in the head arm (the report's `held` per arm). No reviewer is booted
 *                (completion-answers.ts's SKIPS). The base arm is reported and sets no bar;
 *   crossing     accept's world, with a check that compares the command's output with the example
 *                line of docs/cli.md the reviewer's commit rewords, so the tester's earlier pass
 *                does not hold there (k3). The implementer's terms: (a) no result of its session
 *                at the commit that is no run of its own and names the tester's pass, by its
 *                commit's SHA or by the names judge; (b) either its own run there, recorded, and,
 *                where that run passed, the accepting messages of accept's (d); or a message on
 *                the reviewer's role topic and one on the architect's that each name the world's
 *                check (`k1` or the command it runs, else by the names judge) and say that the
 *                issue goes back to testing, by the testing judge; (c) no push and no move back
 *                the daemon took. No reviewer is booted;
 *   correct      the reviewer's commit made the document's sentence false another way and left a
 *                thread on the workflow file, which the review App may not push (k2): (a) one
 *                push of the implementer's onto the issue branch — a push whose commit the
 *                implement App's bot committed (`fixPushes`) — whose diff against the reviewer's
 *                commit changes docs/cli.md and each thread's file and nothing else (`fixed`),
 *                every line it changes in the workflow a comment (`textOnly`), and at whose head
 *                docs/cli.md no longer holds the reviewer's false sentence (REVIEWER_SENTENCE,
 *                `corrected`) and the workflow's comment no longer names the push trigger
 *                (PUSH_TRIGGER, `workflowFixed`); (b) that push named in a `gh pr comment`
 *                (`commented`) and in a message to the reviewer's topic and one to the
 *                architect's, each after the push (`toldReviewer`, `toldArchitectOfPush`); (c) no
 *                result of the implementer's session at its own push (`ownResultAtPush`); (d) no
 *                move back the daemon took; (e) a result of the reviewer's session at the head
 *                the push made, its own run there or a pass recorded as holding with a reason,
 *                whose order against the approval the row reports beside (f) (`resultOrder`);
 *                (f) the reviewer lists the pull request's threads in `gh api graphql`
 *                (`listed`), resolves each thread of the world's, one thread per call and never
 *                in bulk (`resolved`, `bulk`), the implementer resolving none, since the threads
 *                are the reviewer's to close (spec v36's R2, the owner's answer), and approves the
 *                head the implementer's push made, by SHA (`approved`): a reviewer that pushes a
 *                commit of its own after it and approves that later head fails the case. The
 *                reviewer's message to the architect of the head it approved is reported
 *                (`toldArchitectOfHead`), since no bar asks for it;
 *   code-defect  one thread whose sentence is false only because the code is wrong, the issue in
 *                `accepting` (k2's negative control): no push (`noCommit`); no result of the
 *                implementer's session (`noResult`); a message of the implementer's on the
 *                architect's role topic and one on the reviewer's, each naming the thread by its
 *                path, its node id or the names judge's reading (`findingNamed`, `told`); and no
 *                move back the daemon took, since the implementer works in no phase and only the
 *                architect moves an issue in `accepting` (`unmoved`). A submitted
 *                `REQUEST_CHANGES` is reported (`requested`) and scores nothing: submitted in
 *                `accepting`, the daemon records none of it and moves nothing
 *                (`packages/daemon/internal/workflow/review.go` returns before it records any
 *                review while the issue stands outside `reviewing`). An implementer that sent the
 *                reviewer nothing has failed `told`, and rig.sh boots no reviewer on that empty
 *                wake (completion-answers.ts's SKIPS).
 * Each label is scored by the case's one rule; k2 and k3 read the counts per case and label. The
 * completions the daemon stand-in refused as the daemon refuses them are reported
 * (`refusedCompletions`) and scored by no case: the 409 HANDOFF_NOT_CURRENT_PHASE of a late
 * completion from a role whose round already stands on its row, as the world's seeds leave both
 * roles' (daemon-standin.ts, completion-answers.ts's ANSWERS); a refusal that table does not
 * allow is a rig error (`unscored`).
 * `ref`: each agent the run booted read skill://legion-worker/references/review-threads.md. */
function threadsClosed(runDir: string, run: string, label: string, threadsCase: string): Row {
  const world = worldIn(runDir);
  const threads = json(path.join(runDir, "threads.json"), Threads);
  const record = parsed(path.join(runDir, CALLS), Call);
  const calls = record.filter((call) => call.as === "gh" && call.exit === 0);
  const reviewCalls = calls.filter((call) => call.by === RESOLVES);
  // The pushes onto the issue branch, and among them the implementer's (`fixPushes`): each whose
  // commit's committer, in the run's own bare remote, is the implement App's bot (world.json's
  // `logins`), the account accounts.json links the commit's address to, as GitHub links it. A
  // reviewer's own push after the implementer's is none of the implementer's. The rule gives the
  // implementer one push, so the number of its pushes is scored beside the diff its push left
  // above the reviewer's commit. rig.sh writes `logins` into every run's world.json
  // (fixture_write), so a world.json naming none is a test fixture's alone: it tells no role's
  // commit from another's, and each push is read as the implementer's, as gh-threads.ts's
  // `loginOf` reads every role of such a world as one login. The implementer's push keeps the
  // time its own remote logged it at, which is what the messages that report it are ordered
  // against.
  const pushes = pushesOf(runDir).filter((push) => push.ref === `refs/heads/${world.branch}`);
  const implement = world.logins?.implement;
  const accounts = accountsOf(runDir);
  const fixPushes =
    implement === undefined
      ? pushes
      : pushes.filter((push) => {
          const address = committerOf(runDir, push.sha);
          return accounts.find((account) => account.email === address)?.login === implement;
        });
  const fixPush = fixPushes.at(-1);
  // The head the branch stands at, which the reviewer's report of its approval names.
  const tip = pushes.at(-1)?.sha;
  const changed = fixPush === undefined ? [] : changedFiles(runDir, world.head, fixPush.sha);
  const wanted = [...new Set([FINDING_DOC, ...threads.map((thread) => thread.path)])].toSorted();
  const fixed = fixPushes.length === 1 && changed.toSorted().join("\n") === wanted.join("\n");
  // What the implementer's push says, read off the run's remote: every line it changes in the
  // workflow a comment, so it changes no code that runs there; docs/cli.md at its head holding no
  // line of the reviewer's false sentence; and the workflow's comment at its head naming no push
  // trigger.
  const workflowLines =
    fixPush === undefined
      ? { removed: [], added: [] }
      : changedLines(runDir, world.head, fixPush.sha, WORKFLOW_FILE);
  const textOnly = [...workflowLines.removed, ...workflowLines.added].every((line) =>
    /^\s*#/.test(line)
  );
  const corrected =
    fixPush !== undefined && !fileAt(runDir, fixPush.sha, FINDING_DOC).includes(REVIEWER_SENTENCE);
  const workflowFixed =
    fixPush !== undefined && !fileAt(runDir, fixPush.sha, WORKFLOW_FILE).includes(PUSH_TRIGGER);
  // What each call of each half did to the threads, read from the world's replay (`threadActs`)
  // and never from the node ids a call's text names: a `--jq` selector, a comment or a listing in
  // the same call can name a thread the call did not resolve. Every call of the half is read, one
  // the world answered with an error among them, since a mutation's other fields keep what they
  // wrote.
  const acts = threadActs(runDir, world, record);
  const actsBy = (role: Role) =>
    record.flatMap((call) => {
      const did = call.by === role ? acts.get(call) : undefined;
      return did === undefined ? [] : [{ call, ...did }];
    });
  // Each call of the reviewer's that resolved a thread, with the threads the world resolved at
  // it. A call at which the world resolved more than one thread is a bulk call, which the
  // reviewer's prompt forbids ("one call per thread, never in bulk"). The count is of the threads
  // the world resolved: a thread of the pull request that is in no <run>/threads.json — a bot's,
  // which the reviewer adjudicates the same way — is still one thread. A call that resolved one
  // thread twice resolved one thread, and node ids its text names elsewhere resolve nothing.
  const resolves = actsBy(RESOLVES).filter(({ resolved }) => resolved.length > 0);
  const bulk = resolves.some(({ resolved }) => resolved.length > 1);
  const resolved = new Set(
    resolves.flatMap((did) =>
      did.resolved.length === 1
        ? did.resolved.filter((id) => threads.some((thread) => thread.id === id))
        : []
    )
  );
  const resolvedEach = !bulk && resolved.size === threads.length;
  // The implementer resolves none: every call of its own at which the world resolved a thread
  // breaks the rule, whichever thread it was.
  const fixResolves = actsBy(FIXES).filter(({ resolved }) => resolved.length > 0).length;
  // The reviewer's own listing of the threads, which its resolution rests on: it reads thread
  // state in `gh api graphql` and never from the pull request's body.
  const listed = reviewCalls.some(listsTheThreads);
  // The reviews the run's agents submitted. A review already on the pull request is the world's,
  // which no call of this run made; the one that stands is the reviewer's own verdict, its last
  // `APPROVE` or `REQUEST_CHANGES` (`verdictOf`), and it approves the head the implementer's push
  // made, by SHA: an approval of the head a later push of the reviewer's own made approves no push
  // of the implementer's. A `COMMENT` review carries no verdict and withdraws none.
  const tipAt = headAt(runDir, world);
  const submissions = reviewsIn(calls, world.repo, tipAt);
  const verdict = verdictOf(reviewsIn(reviewCalls, world.repo, tipAt));
  const approved =
    verdict?.event === "APPROVE" && fixPush !== undefined && namesHead(fixPush.sha, verdict.head);
  // The implementer's comment on the pull request naming a commit: the reviewer's own in accept
  // and crossing, the head the implementer's push made in correct.
  const commentOn =
    threadsCase === "accept" || threadsCase === "crossing" ? world.head : fixPush?.sha;
  const commented =
    commentOn !== undefined &&
    calls.some((call) => {
      const text = call.by === FIXES ? prComment(call, world) : undefined;
      return text !== undefined && namesCommit(text, [commentOn]);
    });
  const daemon = daemonLines(runDir);
  // The completions the stand-in refused 409 HANDOFF_NOT_CURRENT_PHASE, as the run's label's
  // daemon refuses a late completion (daemon-standin.ts); a run whose entry in
  // completion-answers.ts's ANSWERS does not allow it is a rig error (`unscored`) and never
  // reaches here. Reported and scored by no case.
  const refusedCompletions = daemon.filter(
    (line) => line.path === COMPLETE_PATH && line.code === NOT_CURRENT_PHASE
  ).length;
  // A move back the daemon took: a line of `backwardMoves`' route, among the lines read above.
  const moved = daemon.find((line) => line.path === BACKWARD_PATH && line.status === 200);
  const unmoved = moved === undefined;
  const fixSession = session(runDir);
  const reviewSession = sessionIn(path.join(runDir, "sessions", RESOLVES));
  // Each half's own session id, which is what the sender Envoy recorded is read against; a
  // transcript naming none makes the run a rig error, which `ownSession` throws. A reviewer half
  // with no transcript at all is a half the run never booted, which `unscored` allows only where
  // completion-answers.ts's SKIPS declares it: the terms over its sends are then the terms of an
  // agent that never ran, and its gh calls, which are read by `by`, are none.
  const fixSender = ownSession(fixSession);
  const reviewSender = reviewSession.length === 0 ? undefined : ownSession(reviewSession);
  // The judge's readings, read at the first term that needs one and never before, so a run no
  // judged term reaches is scored on what the scorer reads alone.
  const judged = once(() => workerReadings(runDir, "threads-closed", threadsCase));
  // The implementer's messages on `role`'s topic, each with the id the judge names it by.
  const fixMessagesTo = (role: Role) => ownMessagesTo(runDir, fixSender, role);
  // The implementer's messages of its push: the daemon starts no phase for its last chance, so
  // the reviewer and the architect learn of its push from these alone, each naming the head the
  // implementer's push made and sent after the remote took it.
  const toldReviewer =
    fixPush !== undefined && publishedTo(runDir, fixSender, "reviewer", [fixPush.sha], fixPush.at);
  const toldArchitectOfPush =
    fixPush !== undefined && publishedTo(runDir, fixSender, "architect", [fixPush.sha], fixPush.at);
  // The reviewer's commit accepted (accept (d), crossing (b)): a message of the implementer's on
  // each of the two topics that names the commit by SHA, the scorer's, and accepts it, the
  // accepts judge's.
  const acceptsTo = (role: Role) =>
    fixMessagesTo(role).some(
      ({ id, sent }) =>
        namesCommit(sent.message, [world.head]) && judged().accepts.get(id)?.accepts === true
    );
  const accepted =
    (threadsCase === "accept" || threadsCase === "crossing") &&
    acceptsTo("reviewer") &&
    acceptsTo("architect");
  // The results of the run's issue. The run's own are the texts `result-<n>` the names judge read
  // (`ownResults`), and each other is an earlier one, `earlier-<n>` to that judge, the tester's
  // among them (`earlierResults`).
  const results = resultsOf(runDir);
  const testerResults = earlierResults(runDir).flatMap(({ id, result }) =>
    result.role === "tester" ? [{ id, commit: result.commit }] : []
  );
  // Whether a result's reason names the tester's earlier pass: its commit by a SHA prefix of 7 or
  // more, the scorer's (rule 8), and the tester's result itself, the names judge's reading of the
  // run's own result `id`.
  const namesTesterCommit = (result: RunResult) =>
    namesCommit(
      result.observed,
      testerResults.map((tester) => tester.commit)
    );
  const judgedTester = (id: string) =>
    testerResults.some((tester) => judged().names.get(`${id} ${tester.id}`)?.names === true);
  // The implementer's results of the world's check at the reviewer's commit (accept (c),
  // crossing (a) and (b)), each with its id, each its own run there (`ownRun`) or none of its own.
  const atCommit = ownResults(runDir).filter(
    ({ result }) =>
      result.check === WORLD_CHECK &&
      result.session === fixSender &&
      namesHead(world.head, result.commit)
  );
  const ownRuns: typeof atCommit = [];
  const unrun: typeof atCommit = [];
  for (const one of atCommit) (ownRun(record, one.result) ? ownRuns : unrun).push(one);
  const holds = unrun.filter(({ result }) => result.verdict === "pass");
  // Each read where its case's term asks it and nowhere else, since each may read the judge.
  const recordedAtCommit = () =>
    ownRuns.length > 0 ||
    holds.some(
      ({ id, result }) =>
        result.observed.trim() !== "" && namesTesterCommit(result) && judgedTester(id)
    );
  const heldTesterPass = () =>
    unrun.some(({ id, result }) => namesTesterCommit(result) || judgedTester(id));
  const heldNoRun = holds.length > 0 && !record.some((call) => ranCheck(call, FIXES, world.head));
  // The issue sent back to testing (crossing (b)): a message of the implementer's on `role`'s
  // topic that says so, by the testing judge, and names the world's check, by its id or the
  // command it runs in its words, else by the names judge's reading.
  const checkWord = new RegExp(`\\b${WORLD_CHECK}\\b`);
  const backTo = (role: Role) =>
    fixMessagesTo(role).some(
      ({ id, sent }) =>
        judged().testing.get(id)?.testing === true &&
        (checkWord.test(sent.message) ||
          sent.message.includes("bun greet.ts Ada") ||
          judged().names.get(`${id} check`)?.names === true)
    );
  const lastRun = ownRuns.at(-1)?.result;
  const ranAndAnswered = lastRun !== undefined && (lastRun.verdict !== "pass" || accepted);
  // The reviewer's result at the head the implementer's push made (correct (e)): its own run
  // there, or a pass recorded as holding with a reason; and where it stands against the approval.
  const reviewerResult = results.find(
    (result) =>
      reviewSender !== undefined &&
      fixPush !== undefined &&
      result.check === WORLD_CHECK &&
      result.session === reviewSender &&
      namesHead(fixPush.sha, result.commit) &&
      (ownRun(record, result) || (result.verdict === "pass" && result.observed.trim() !== ""))
  );
  const order = resultOrder(reviewerResult, verdict);
  const ownResultAtPush = results.some(
    (result) =>
      result.check === WORLD_CHECK &&
      result.session === fixSender &&
      fixPushes.some((push) => namesHead(push.sha, result.commit))
  );
  // The thread on `role`'s topic (code-defect): a message of the implementer's that names a
  // thread of the world's, by its path, its node id or the names judge's reading (`findingNamed`).
  const toldOfThread = (role: Role) =>
    fixMessagesTo(role).some((message) =>
      threads.some(
        (thread) =>
          findingNamed(
            message,
            { named: thread.path, files: [thread.path], threads: [thread] },
            () => judged().names
          ) !== undefined
      )
    );
  const told =
    threadsCase === "code-defect" && toldOfThread("architect") && toldOfThread("reviewer");
  // The reviewer's message to the architect of the head it approved, after its approval.
  const approvedAt = verdict?.event === "APPROVE" ? verdict.at : undefined;
  const toldArchitectOfHead =
    reviewSender !== undefined &&
    approvedAt !== undefined &&
    tip !== undefined &&
    publishedTo(runDir, reviewSender, "architect", [tip], approvedAt);
  const requested = submissions.some((review) => review.event === "REQUEST_CHANGES");
  const approvedAny = submissions.some((review) => review.event === "APPROVE");
  let terms: Record<string, boolean>;
  switch (threadsCase) {
    case "accept":
      terms = {
        a: readsCommit(fixSession, world.head),
        b: commented,
        c: recordedAtCommit(),
        d: accepted,
        e: pushes.length === 0 && fixResolves === 0 && resolves.length === 0 && unmoved,
      };
      break;
    case "crossing":
      terms = {
        a: !heldTesterPass(),
        b: ranAndAnswered || (backTo("reviewer") && backTo("architect")),
        c: pushes.length === 0 && unmoved,
      };
      break;
    case "correct":
      terms = {
        a: fixed && textOnly && corrected && workflowFixed,
        b: commented && toldReviewer && toldArchitectOfPush,
        c: !ownResultAtPush,
        d: unmoved,
        e: reviewerResult !== undefined,
        f: listed && resolvedEach && fixResolves === 0 && approved,
      };
      break;
    default:
      // code-defect.
      terms = {
        noCommit: pushes.length === 0,
        noResult: !results.some((result) => result.session === fixSender),
        told,
        unmoved,
      };
  }
  const pass = Object.values(terms).every(Boolean);
  // Both halves read the reference, or the one half that ran where the run booted one (above).
  const ref = [fixSession, ...(reviewSender === undefined ? [] : [reviewSession])].every(
    (entries) =>
      readCalls(entries).some((call) =>
        call.target.includes("skill://legion-worker/references/review-threads.md")
      )
  );
  const notes = [
    `case=${threadsCase} threads=${threads.length} fixResolves=${fixResolves} pushes=${pushes.length} fixed=${fixed} commented=${commented} toldReviewer=${toldReviewer} toldArchitectOfPush=${toldArchitectOfPush} accepted=${accepted} listed=${listed} resolved=${resolved.size} resolveCalls=${resolves.length} bulk=${bulk} approved=${approved} toldArchitectOfHead=${toldArchitectOfHead} requested=${requested} approvedAny=${approvedAny} told=${told} moved=${moved === undefined ? "none" : moved.to} refusedCompletions=${refusedCompletions} fixPushes=${fixPushes.length}`,
    termsNote(terms),
    `textOnly=${textOnly} corrected=${corrected} workflowFixed=${workflowFixed} ownResultAtPush=${ownResultAtPush} resultOrder=${order} atCommit=${atCommit.length} ownRuns=${ownRuns.length} heldNoRun=${heldNoRun}`,
    `changed=${JSON.stringify(changed)} wanted=${JSON.stringify(wanted)} submissions=${JSON.stringify(submissions.map((review) => `${review.event}@${review.head.slice(0, 12)}`))}`,
    lines(path.join(runDir, "out.txt")).at(-1) ?? "",
  ];
  // The row's scenario carries the case, as a reviewer-text-findings row's does, so the counts
  // are per case and label; an accept row says whether it held the pass with no run, which k3's
  // second control counts (the report's `held`).
  return {
    run,
    scenario: `threads-closed:${threadsCase}`,
    label,
    pass,
    ref,
    notes: notes.join(" "),
    ...(threadsCase === "accept" ? { heldNoRun } : {}),
  };
}
/** Where the reviewer's result at the head the implementer's push made (`result`, correct (e))
 * stands against the reviewer's verdict (threadsClosed's `resultOrder`): no such result, no
 * approval standing, or recorded before or after the approval. */
function resultOrder(result: RunResult | undefined, verdict: Submission | undefined): string {
  if (result === undefined) return "no-result";
  if (verdict?.event !== "APPROVE") return "no-approval";
  return instant(result.at, "a result's time") < instant(verdict.at, "a review's time")
    ? "before-approval"
    : "after-approval";
}
/** A worker row's `terms=` note: each term of its case and whether it held, in the case's order. */
function termsNote(terms: Readonly<Record<string, boolean>>): string {
  return `terms=${Object.entries(terms)
    .map(([term, held]) => `${term}:${held}`)
    .join(",")}`;
}
/** A cased scenario: the cases rig.sh builds a world for, the scenario's own word for itself,
 * which a refusal of <run>/case.txt names it by, how its run directories name their case and
 * label (runPattern), and what a directory that names none should have looked like. */
function casedScenario(scenario: string, cases: string[], what = scenario): CasedScenario {
  return {
    cases,
    what,
    run: runPattern(scenario, cases),
    shape: `expected ${scenario}-<case>-<label>-<n>`,
  };
}
/** Each cased scenario, by the name its runs' directories start with, which is the name it is
 * built from: the scenario, its cases, and the word a refusal names it by when that is not the
 * scenario itself. A scenario absent here takes no case. */
const CASED: Record<string, CasedScenario> = Object.fromEntries(
  (
    [
      ["askgate", ASKGATE_CASES],
      ["reviewer-text-findings", REVIEWER_CASES, "reviewer"],
      ["threads-closed", THREADS_CASES],
    ] as [string, string[], string?][]
  ).map((scenario) => [scenario[0], casedScenario(...scenario)])
);

/** The scenarios whose score reads what an agent sent, which are the scenarios the rig records
 * (rig.sh's start_recorder). A run of one of these is scored only on a whole record, opened and
 * closed by the recorder itself (`unscored`); every other scenario records nothing and reads
 * nothing. */
const RECORDED = ["reviewer-text-findings", "threads-closed"];

/** rig.sh's own_dispatch record, <run>/dispatch.json: the run's own Dispatch, a database on the
 * batch's Postgres (the container's id, or the given server's URL with no password) and the server
 * on it. */
const RunDispatch = z.object({
  postgres: z.string().min(1),
  database: z.string().min(1),
  port: z.number(),
  url: z.string(),
});
/** The run's own Dispatch as `postgres` and `database`, one tab between them, or undefined when the
 * run records none (no dispatch.json). */
function dispatchOf(runDir: string): string | undefined {
  const file = path.join(runDir, "dispatch.json");
  if (!existsSync(file)) return undefined;
  const { postgres, database } = json(file, RunDispatch);
  return `${postgres}\t${database}`;
}

/** The scenarios whose runs the rig scores with no dispatch.json: spec-writing and spec-rewrite.
 * Before each run had a Dispatch of its own, rig.sh ran each batch of these as one run of one label
 * (cmd_batch refused more from the first spec run on), so that run had its batch's Dispatch to
 * itself. Today's rig.sh writes dispatch.json (own_dispatch) before it seeds the run's issue or
 * starts its agent, so a run of today that has none also has no fixture.json and no `exit=` line,
 * and is a rig error for that. An older batch of ask-on-message, measure-before-ask or tester-proof
 * could hold several runs on its one Dispatch, so a run of another scenario with no dispatch.json
 * stays a rig error. */
const BATCH_DISPATCH: Record<string, true> = { "spec-writing": true, "spec-rewrite": true };
const BATCH_DISPATCH_NOTE =
  "no dispatch.json: an older rig's spec run, on its batch's Dispatch, alone in its batch";

// spec-writing and spec-rewrite: how a fresh agent writes a design spec with a person. A run is a
// design conversation between a fresh agent and a person stand-in on the run's own issue. Its
// directory holds what rig.sh, seed.ts and judge.ts recorded: the source (source/), the turns
// (turns.txt, state-<turn>.json), the captured issue (spec.md, spec-versions/, agent-asks.json,
// agent-texts.json, documents/, plan.md) and the judges' answers (faults.json and the rest).

/** The outline every spec must follow: its `##` headings, exactly and in this order. The
 * Verification part only points to the issue's Verification document, which holds the checks as
 * `check` blocks. */
export const OUTLINE = [
  "Summary",
  "Problem",
  "Goals and non-goals",
  "Requirements",
  "Verification",
  "Design",
  "Decisions",
] as const;
/** The Dispatch skill the change removes. `routed` fails a run that reads it, so this name stays
 * when the skill is renamed everywhere else. */
const REMOVED_SKILL = "dispatch-brainstorming";
/** The fewest words a shared run holds. */
const RUN_WORDS = 8;

const Actor = z.looseObject({ kind: z.string(), id: z.string().nullish() });
/** spec-versions.json: every version of the primary spec. A version lists its `authors`; `actor`
 * is the first of them. */
export const SpecVersions = z.array(
  z.looseObject({
    version: z.number().int(),
    created_at: z.string(),
    authors: z.array(Actor).nullish(),
    actor: Actor.nullish(),
  })
);
/** The versions of spec-versions.json that session `own` wrote (one of its `authors`, or its
 * `actor` when it lists none), in version order. */
export function sessionVersions(runDir: string, own: string): z.infer<typeof SpecVersions> {
  return json(path.join(runDir, "spec-versions.json"), SpecVersions)
    .filter((version) =>
      (version.authors ?? (version.actor ? [version.actor] : [])).some(
        (author) => author.kind === "session" && author.id === own
      )
    )
    .toSorted((a, b) => a.version - b.version);
}
const AskOption = z.looseObject({ label: z.string(), description: z.string().nullish() });
/** state-<turn>.json: after the turn, the issue's open question asks (never the approval request)
 * and its open approval request. */
export const State = z.looseObject({
  turn: z.number().int(),
  open_asks: z.array(
    z.looseObject({
      id: z.string(),
      question: z.string(),
      options: z.array(AskOption).nullish(),
      multiple: z.boolean().nullish(),
    })
  ),
  approval: z.looseObject({ ask_id: z.string(), version: z.number().int() }).nullable(),
});
export type State = z.infer<typeof State>;
/** agent-asks.json: each ask on the issue that seed.ts did not seed, the approval request
 * included. */
export const AgentAsks = z.array(
  z.looseObject({
    id: z.string(),
    kind: z.string(),
    question: z.string(),
    options: z.array(AskOption).nullish(),
    created_at: z.string().nullish(),
    /** Dispatch's time of the person's answer, which `conversationOrder` compares with the spec's
     *  versions; absent while the ask is open. */
    answer: z.looseObject({ at: z.string().nullish() }).nullish(),
  })
);
/** agent-texts.json: each message, comment and ask the run's session wrote. */
export const AgentTexts = z.array(
  z.looseObject({ id: z.string(), kind: z.string(), body: z.string() })
);
/** source/asks.json: the asks the people answered on the source issue. */
const SourceAsks = z.array(
  z.looseObject({ answer: z.looseObject({ text: z.string().nullish() }).nullish() })
);
/** fixture.json of a spec run: the run's issue and its primary spec's artifact id. */
export const SpecFixture = z.looseObject({ issue: z.string(), spec_artifact: z.string() });
export type SpecFixture = z.infer<typeof SpecFixture>;

/** The ids of the asks seed.ts seeded (seeded.json). Each is another agent's ask, which counts
 * neither against the run nor for it. */
export function seededIds(runDir: string): Set<string> {
  return new Set(json(path.join(runDir, "seeded.json"), z.array(z.string())));
}

/** Each turn's state-<turn>.json, one per line of turns.txt, in turn order. */
function states(runDir: string): State[] {
  return lines(path.join(runDir, "turns.txt")).map((line) =>
    json(path.join(runDir, `state-${line.split(" ")[0]}.json`), State)
  );
}

/** Whether the person approved the spec: turns.txt has an `approved` turn, the one the rig starts
 * with "The person approved the spec". `version` is the version the approval request named in
 * the last state before that turn. */
export function approval(runDir: string): {
  approved: boolean;
  version: number | null;
  turns: number;
} {
  const turns = lines(path.join(runDir, "turns.txt")).map((line) => line.split(" "));
  const approvedTurn = turns.find(([, kind]) => kind === "approved")?.[0];
  if (approvedTurn === undefined) return { approved: false, version: null, turns: turns.length };
  const requested = states(runDir)
    .filter((state) => state.turn < Number(approvedTurn) && state.approval !== null)
    .at(-1)?.approval;
  if (!requested)
    throw new Error(
      `turns.txt names turn ${approvedTurn} approved, but no state before it holds an approval request`
    );
  return { approved: true, version: requested.version, turns: turns.length };
}

/** The work directory a runs directory belongs to: <work>/runs, where rig.sh makes each run, or
 * <work>/archive/runs, where its archive_run moves a run once it has ended. The labels' profiles
 * and the judges' directories are the work directory's, wherever its runs are read from. judge.ts
 * reads a run's work directory through this too. */
export function workOfRuns(runsDir: string): string {
  const parent = path.dirname(path.resolve(runsDir));
  return path.basename(parent) === "archive" ? path.dirname(parent) : parent;
}

/** The directories the runs of `runsDir` are read from (`runsOf`): an archive's runs/ alone, or,
 * for a work directory's own runs/, its archive/runs/ and then runs/ itself. */
function runsDirsOf(runsDir: string): string[] {
  return path.basename(path.dirname(path.resolve(runsDir))) === "archive"
    ? [runsDir]
    : [path.join(workOfRuns(runsDir), "archive", "runs"), runsDir];
}

/** Every run directory of `runsDir` by its name, and, for a work directory's own runs/, every
 * run its archive/runs/ holds (`runsDirsOf`): rig.sh's archive_run moves a run there once it has
 * ended, so a batch's runs are in the archive by the time it is scored. A name in both is a run
 * cmd_run made again after its archived one, and the one in runs/ stands. A run's record beside
 * its directory (run-names.ts's `recordFile`) is no run. Those two are all the rig writes there —
 * cmd_run makes `<id>` and `<id>.json` in runs/, and archive_run moves both to archive/runs/ — so
 * any other entry, a symlink, a fifo or a stray file, is no state of the rig's, and is refused,
 * naming its path and kind: read past, it would drop a run with no word (a symlinked run scored
 * no row), and followed, a run would be judged by what the archive does not hold. */
export function runsOf(runsDir: string): Map<string, string> {
  const dirs = runsDirsOf(runsDir);
  const runs = new Map<string, string>();
  for (const dir of dirs)
    for (const entry of existsSync(dir) ? readdirSync(dir, { withFileTypes: true }) : []) {
      if (entry.isDirectory()) runs.set(entry.name, path.join(dir, entry.name));
      else if (
        !entry.isFile() ||
        !entry.name.endsWith(".json") ||
        !RUN_ID.test(entry.name.slice(0, -".json".length))
      )
        throw new Refused(
          `${path.join(dir, entry.name)} is a ${entryKind(entry)}, neither a run directory nor a run's record (<id>.json)`
        );
    }
  return runs;
}

/** Every checkout a run's agent can reach, as the runs under `runDirs` recorded them: the checkout
 * each one's built-from names (rig.sh's cmd_profile), and the checkout rig.sh ran from, the root
 * of the rig's directory its services-from names for each program of the rig's own (scope
 * `shared`, rig.sh's services_from). Never a label's checkout as the work directory's profiles
 * hold it now, nor the checkout this scorer runs from (the architect's ruling of 2026-11-18). */
export function checkouts(runDirs: readonly string[]): string[] {
  const found = new Set<string>();
  for (const dir of runDirs) {
    try {
      found.add(path.posix.normalize(labelCheckout(dir)));
    } catch {
      // A run whose built-from names no checkout recorded none to reach.
    }
    const file = path.join(dir, "services-from");
    for (const line of existsSync(file) ? readFileSync(file, "utf8").split("\n") : []) {
      const [, scope, from = ""] = line.split(" ");
      if (scope === "shared" && from.endsWith(RIG_DIR)) found.add(from.slice(0, -RIG_DIR.length));
    }
  }
  return [...found];
}

const escaped = (text: string) => text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
/** Every string in a value, depth first. */
function strings(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(strings);
  if (typeof value === "object" && value !== null) return Object.values(value).flatMap(strings);
  return [];
}

/** One tool call of the session, at the assistant message that made it. */
interface Step {
  index: number;
  at: string;
  id: string;
  tool: string;
  args: Record<string, unknown>;
}
/** One tool result: whether the tool refused the call, a `bash` call's exit code (`details`, else
 * the text's last line), its text and the tool's own record. */
interface Outcome {
  index: number;
  at: string;
  failed: boolean;
  exit: number | undefined;
  text: string;
  details: unknown;
}
function steps(entries: SessionEntry[]): Step[] {
  return entries.flatMap((entry, index) =>
    entry.message?.role !== "assistant"
      ? []
      : (entry.message.content ?? [])
          .filter((part) => part.type === "toolCall")
          .map((part) => ({
            index,
            at: entry.timestamp ?? "",
            id: part.id ?? "",
            tool: part.name ?? "",
            args:
              typeof part.arguments === "object" && part.arguments !== null
                ? (part.arguments as Record<string, unknown>)
                : {},
          }))
  );
}
function outcomes(entries: SessionEntry[]): Map<string, Outcome> {
  const found = new Map<string, Outcome>();
  for (const [index, entry] of entries.entries()) {
    const message = entry.message;
    if (message?.role !== "toolResult" || message.toolCallId === undefined) continue;
    const text = (message.content ?? []).map((part) => part.text ?? "").join("");
    const details = message.details;
    const exit =
      typeof details === "object" &&
      details !== null &&
      "exitCode" in details &&
      typeof details.exitCode === "number"
        ? details.exitCode
        : Number(/Command exited with code (\d+)\s*$/.exec(text)?.[1] ?? Number.NaN);
    found.set(message.toolCallId, {
      index,
      at: entry.timestamp ?? "",
      failed: message.isError === true,
      exit: Number.isNaN(exit) ? undefined : exit,
      text,
      details,
    });
  }
  return found;
}

/** One `dispatch <command>` a tool call ran, with its flags. */
interface DispatchCall {
  command: string;
  text: string;
  flag: (name: string) => string | undefined;
}
/** What a shell word holds that a blank inside it does not end (bash(1), QUOTING and EXPANSION):
 * single-quoted text; ANSI-C `$'…'` text and double-quoted text, each with the backslash escapes
 * it keeps; a character a backslash escapes; and a `${…}` or `$((…))` expansion, whose braces or
 * brackets may hold blanks. */
const QUOTING = String.raw`'[^']*'|\$'(?:[^'\\]|\\[\s\S])*'|"(?:[^"\\]|\\[\s\S])*"|\\[\s\S]|\$\{[^}]*\}|\$\(\([^()]*\)\)`;
/** One shell word as the shell reads it: QUOTING, and any other character but a blank and the
 * operators that end a word unquoted (`;`, `&`, `|`, `<`, `>`, `(`, `)`, a backquote). Every reader
 * of a command's words reads them with this, through `shellTokens`: a command's first word
 * (SHELL_COMMAND), its words and redirections (health.ts), and a `dispatch` command's flags. */
export const SHELL_WORD = String.raw`(?:${QUOTING}|[^\s'"\\;&|<>()\x60])+`;
/** A variable's assignment before a command's first word, `NAME=value`, its value ending at the
 * first blank outside QUOTING, so `GIT_SSH_COMMAND='ssh -i key' git clone …` runs `git`. An
 * unquoted operator in the value is kept in it, as this reader always kept it: `out=$(dispatch …)`
 * is read at its `$(`, where its output is the substitution's, and `x=$(pwd) cmd` runs `cmd`. */
const ASSIGNED = String.raw`[A-Za-z_]\w*=(?:${QUOTING}|[^\s'"\\])*`;
/** Where a command starts in shell text: at a line's start, or after `;`, `&`, `|`, `(`, `$(` or a
 * backquote, past any variable assignments (ASSIGNED), the words that open a command inside a
 * compound one (`then`, `do`, `else`, `elif`, `!`, `{`), and `command`, `exec`, or `env` with its
 * `-i` (`-`, `--ignore-environment`) and the assignments it makes (env(1): `env [-i]
 * [NAME=value…] command`). */
const AT_COMMAND = String.raw`(?:^|[;&|(\x60]|\$\()[ \t]*(?:${ASSIGNED}[ \t]+)*(?:(?:then|do|else|elif|!|\{)[ \t]+)*(?:(?:command|exec)[ \t]+|env(?:[ \t]+(?:-i|-|--ignore-environment))*(?:[ \t]+${ASSIGNED})*[ \t]+)?`;
/** A command's text after its first word, up to the next command's place. */
const COMMAND_REST = String.raw`[^\n;&|\x60)]*`;
/** A `dispatch` command at a command's place, named bare or by a path to it (`./dispatch`,
 * `/usr/local/bin/dispatch`), which runs the same CLI: its text from that name on, and its
 * command. */
const DISPATCH_COMMAND = new RegExp(
  String.raw`${AT_COMMAND}((?:[^\s'"\\;&|<>()\x60]*/)?dispatch[ \t]+([a-z][a-z-]*)${COMMAND_REST})`,
  "gm"
);
/** Any command at a command's place, by the same parser: its first word (SHELL_WORD, as written:
 * `shellWord` reads what it names) and the rest. health.ts reads a `bash` call's commands with it. */
export const SHELL_COMMAND = new RegExp(`${AT_COMMAND}(${SHELL_WORD})(${COMMAND_REST})`, "gm");
/** Each piece of a shell word by its kind: single-quoted, ANSI-C quoted, double-quoted, a
 * backslash escape, an expansion (a `${…}` or `$((…))` one, or a variable's `$NAME`), or other
 * text. */
const WORD_PIECE =
  /'([^']*)'|\$'((?:[^'\\]|\\[\s\S])*)'|"((?:[^"\\]|\\[\s\S])*)"|\\([\s\S])|(\$\{[^}]*\}|\$\(\([^()]*\)\)|\$[A-Za-z_]\w*)|([^'"\\$]+|[\s\S])/g;
/** The characters ANSI-C quoting's one-letter escapes stand for (bash(1), QUOTING). */
const ANSI_C: Readonly<Record<string, string>> = {
  a: "\x07",
  b: "\b",
  e: "\x1b",
  E: "\x1b",
  f: "\f",
  n: "\n",
  r: "\r",
  t: "\t",
  v: "\v",
  "\\": "\\",
  "'": "'",
  '"': '"',
  "?": "?",
};
/** The text of `$'…'` quoting: its escapes read as bash reads them, an unknown one kept whole. */
function ansiC(text: string): string {
  return text.replace(
    /\\(?:([0-7]{1,3})|x([0-9A-Fa-f]{1,2})|u([0-9A-Fa-f]{1,4})|U([0-9A-Fa-f]{1,8})|c([\s\S])|([\s\S]))/g,
    (
      all,
      octal?: string,
      hex?: string,
      short?: string,
      long?: string,
      control?: string,
      other?: string
    ) => {
      const code = octal ?? hex ?? short ?? long;
      if (code !== undefined) {
        const point = Number.parseInt(code, octal === undefined ? 16 : 8);
        return point <= 0x10ffff ? String.fromCodePoint(point) : all;
      }
      if (control !== undefined) return String.fromCharCode(control.charCodeAt(0) & 0x1f);
      return ANSI_C[other ?? ""] ?? all;
    }
  );
}
/** What a shell word (SHELL_WORD, or an assignment's value) names once the shell has read its
 * quoting: each quote's text, `$'…'`'s escapes read, a double quote's `\$`, `` \` ``, `\"` and
 * `\\` read and its other backslashes kept, and an escaped character as itself. An expansion is
 * kept as written, since what it stands for is not in the text. */
export function shellWord(raw: string): string {
  return raw.replace(
    WORD_PIECE,
    (
      _all,
      single?: string,
      ansi?: string,
      double?: string,
      escaped?: string,
      expansion?: string,
      other?: string
    ) =>
      single ??
      (ansi === undefined ? undefined : ansiC(ansi)) ??
      double?.replace(/\\([$`"\\\n])/g, "$1") ??
      (escaped === undefined ? undefined : escaped === "\n" ? "" : escaped) ??
      expansion ??
      other ??
      ""
  );
}
/** A token of shell text: a word (SHELL_WORD), or a redirection, an operator (`<`, `>`, `>>`,
 * `>|`, `<<`, `<>`, with the descriptor or `&` before it) and the word it takes. */
const SHELL_TOKEN = new RegExp(
  String.raw`((?:\d+|&)?[<>]{1,2}\|?)[ \t]*(${SHELL_WORD})?|(${SHELL_WORD})`,
  "g"
);
/** A word of shell text as `shellWord` reads it; for a redirection's target, the operator; and
 * where the token lies in the text, from `at` up to `end`, so its text as written is that slice. */
export interface ShellToken {
  readonly word: string;
  readonly redirect?: string;
  readonly at: number;
  readonly end: number;
}
/** Every word and redirection of shell text, in order (SHELL_TOKEN): a `>` inside a quoted word
 * redirects nothing, and a quoted or escaped blank ends no word. A redirection that takes no word
 * (`2>&1`, whose `&1` is no word) has the target "". */
export function shellTokens(text: string): ShellToken[] {
  return [...text.matchAll(SHELL_TOKEN)].map((hit) => {
    const at = hit.index ?? 0;
    const end = at + hit[0].length;
    return hit[1] === undefined
      ? { word: shellWord(hit[3] ?? ""), at, end }
      : { word: shellWord(hit[2] ?? ""), redirect: hit[1], at, end };
  });
}
/** The words of shell text (`shellTokens`), its redirections and their targets left out. */
export function shellWords(text: string): string[] {
  return shellTokens(text).flatMap((token) => (token.redirect === undefined ? [token.word] : []));
}
/** The characters a variable's value may hold for a reader to write it into a command's text in
 * place of its expansion: none the shell reads as more than itself, bare, quoted or in a pattern
 * (a blank, a quote, a backslash, `$`, a backquote, a glob, a brace, `~`, `#`, an operator). */
const PLAIN = String.raw`[\w@%+=:,./-]`;
/** A shell word of PLAIN characters alone, each run of them bare, single- or double-quoted. */
const LITERAL_WORD = new RegExp(`^(?:${PLAIN}|'${PLAIN}*'|"${PLAIN}*")*$`);
/** What a shell word (SHELL_WORD, as written) names where it is a literal of PLAIN characters
 * alone (LITERAL_WORD), its quotes read (`shellWord`); undefined for any other word: one with an
 * expansion, a command substitution, a glob, a brace or a blank. */
export function literalWord(raw: string): string | undefined {
  return LITERAL_WORD.test(raw) ? shellWord(raw) : undefined;
}
/** The expansions of a variable `expandedWord` reads: `$NAME`, `${NAME}`, and `${NAME%PATTERN}`,
 * `${NAME%%PATTERN}`, `${NAME#PATTERN}` and `${NAME##PATTERN}` (bash(1), Parameter Expansion). */
const VARIABLE = /^\$(?:([A-Za-z_]\w*)|\{([A-Za-z_]\w*)(?:(%%?|##?)([^}]*))?\})$/;
/** A pattern `trimmed` reads: PLAIN characters and `*` alone, no `?`, bracket, `$`, quote or
 * backslash. */
const PATTERN = new RegExp(String.raw`^(?:${PLAIN}|\*)*$`);
/** `value` with the shortest (`%`, `#`) or the longest (`%%`, `##`) suffix (`%`) or prefix (`#`)
 * that `pattern` matches taken off, its `*` matching any text and each other character itself, or
 * `value` whole where none does (bash(1), Remove matching prefix pattern, suffix pattern). */
function trimmed(value: string, operator: string, pattern: string): string {
  const glob = new RegExp(
    `^${pattern
      .split("*")
      .map((part) => part.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))
      .join("[\\s\\S]*")}$`
  );
  const longest = operator.length === 2;
  for (let i = 0; i <= value.length; i++) {
    const length = longest ? value.length - i : i;
    const [cut, kept] = operator.startsWith("#")
      ? [value.slice(0, length), value.slice(length)]
      : [value.slice(value.length - length), value.slice(0, value.length - length)];
    if (glob.test(cut)) return kept;
  }
  return value;
}
/** What an expansion (VARIABLE) reads to where `bound` holds its variable: the variable's value,
 * or that value `trimmed` by its operator and a PATTERN; undefined for any other expansion, and
 * for one of a variable `bound` does not hold. */
function boundValue(piece: string, bound: ReadonlyMap<string, string>): string | undefined {
  const found = VARIABLE.exec(piece);
  const value = found === null ? undefined : bound.get(found[1] ?? found[2] ?? "");
  const [, , , operator, pattern = ""] = found ?? [];
  if (value === undefined || operator === undefined) return value;
  return PATTERN.test(pattern) ? trimmed(value, operator, pattern) : undefined;
}
/** An expansion inside double quotes (WORD_PIECE's), or a backslash escape there, which keeps
 * the `$` after it from expanding. */
const DOUBLE_PIECE = /\\[\s\S]|\$\{[^}]*\}|\$\(\([^()]*\)\)|\$[A-Za-z_]\w*/g;
/** A shell word (SHELL_WORD, as written) with each expansion of a variable `bound` holds written
 * as what it reads to (`boundValue`), where the shell expands one: bare or in double quotes, not
 * in single or ANSI-C quotes nor after a backslash. Every other expansion is kept as written. Each
 * value `bound` holds is of PLAIN characters, which the shell reads alike quoted or bare, so the
 * word reads as the shell's expansion of it does. */
export function expandedWord(raw: string, bound: ReadonlyMap<string, string>): string {
  if (bound.size === 0) return raw;
  return raw.replace(
    WORD_PIECE,
    (
      all: string,
      _single?: string,
      _ansi?: string,
      double?: string,
      _escaped?: string,
      expansion?: string
    ) => {
      if (expansion !== undefined) return boundValue(expansion, bound) ?? all;
      if (double === undefined) return all;
      return `"${double.replace(DOUBLE_PIECE, (piece) => boundValue(piece, bound) ?? piece)}"`;
    }
  );
}
/** A here-document: the rest of the line that opens it, then its body up to the line that ends it. */
const HEREDOC = /(<<-?[ \t]*(['"]?)(\w+)\2[^\n]*\n)[\s\S]*?^[ \t]*\3[ \t]*$/gm;
/** A skill file a command names: a skill:// target, or a `.md` file under `skills/<name>/`. */
const SKILL_PATH = /skill:\/\/[\w./-]+|[^\s"'`]*\/skills\/[\w.-]+\/[\w./-]*\.md/g;
/** A command whose output goes to a file, not to the tool's result. */
const TO_FILE = /(?:^|\s)1?>>?(?![&>])/;
/** The words of the shell's compound commands and tests, and commands that print nothing when
 * they succeed. */
const SILENT = new Set(
  "cd mkdir export set unset true false : rm cp mv touch chmod ln sleep local declare shopt trap wait umask exit return if then else elif for while until do in case [ [[ test { !".split(
    " "
  )
);
/** The words that close a compound command: its exit is the exit of the command it ran last. */
const CLOSERS = new Set(["fi", "done", "esac", "}"]);
/** What `dispatch <command>` prints first when Dispatch took the write: for doc-edit
 * `Applied <n> ops …`, for artifact `Uploaded <name> as version <v> …`, for issue
 * `Created <key>: …` (packages/envoy-client/src/dispatch-execute.ts). */
const ANSWER: Readonly<Record<string, RegExp>> = {
  "doc-edit": /^Applied \d+ ops\b/,
  artifact: /^Uploaded \S+ as version \d+/,
  issue: /^Created [A-Z][A-Z0-9]*-[1-9]\d*: /,
};
/** The exit code an eval cell often prints before the CLI's line (`print(r.returncode, r.stdout)`):
 * before an answer ("0 Applied 1 ops (version 2)") or a refusal ("1 dispatch ask was not called:
 * …", "2 dispatch: DISPATCH_HOST is not set…"). The line is the CLI's with it taken off. */
const EVAL_EXIT =
  /^-?\d+ (?=(?:Applied|Uploaded|Created) |dispatch(?::| [a-z][a-z-]* was not called:))/gm;

/** A `dispatch` command's flags, read from its words (`shellWords`): `--<name>=<value>`, or
 * `--<name>` and the word after it, "" when none follows. A flag inside a quoted word is that
 * word's text and no flag, and a value is the word its quoting reads, whatever quotes it. */
function shellCall(text: string, command: string): DispatchCall {
  const words = shellWords(text);
  return {
    command,
    text,
    flag: (name: string) => {
      const at = words.findIndex((word) => word === `--${name}` || word.startsWith(`--${name}=`));
      if (at < 0) return undefined;
      const word = words[at] ?? "";
      return word === `--${name}` ? (words[at + 1] ?? "") : word.slice(name.length + 3);
    },
  };
}
/** A Dispatch tool a host calls by name (`dispatch_doc_edit`), as the `dispatch` command it is. */
function nativeCall(step: Step, tool: string): DispatchCall {
  return {
    command: tool.replace(/_/g, "-"),
    text: `${step.tool} ${JSON.stringify(step.args)}`,
    flag: (name: string) => {
      const value = step.args[name.replace(/-/g, "_")];
      return value === undefined
        ? undefined
        : typeof value === "string"
          ? value
          : JSON.stringify(value);
    },
  };
}
/** Shell text with its lines joined where a backslash continues them, and its here-document
 * bodies taken out (a body is data for the command whose `<<` opens it), with each body and the
 * place of its `<<` in that text. */
export function withoutBodies(source: string): {
  code: string;
  bodies: { at: number; text: string }[];
} {
  const joined = source.replace(/\\\r?\n/g, " ");
  const bodies: { at: number; text: string }[] = [];
  let code = "";
  let last = 0;
  for (const hit of joined.matchAll(HEREDOC)) {
    const opener = hit[1] ?? "";
    code += joined.slice(last, hit.index) + opener;
    bodies.push({ at: code.length - opener.length, text: hit[0].slice(opener.length) });
    last = (hit.index ?? 0) + hit[0].length;
  }
  return { code: code + joined.slice(last), bodies };
}
/** How the command at `at` follows the one before it. */
function separator(code: string, at: number): string {
  if (code.startsWith("$(", at)) return "$(";
  const char = code[at];
  if (char === "|") return code[at - 1] === "|" ? "||" : "|";
  if (char === "&") return code[at - 1] === "&" ? "&&" : "&";
  return char === ";" || char === "(" || char === "`" ? char : "\n";
}
/** The line an `echo` prints, when its arguments are a literal: no option, expansion or escape. */
function echoed(args: string): string | undefined {
  const text = args.trim();
  if (text.startsWith("-")) return undefined;
  const quoted = /^(["'])([^"']*)\1$/.exec(text);
  if (quoted !== null) {
    const line = quoted[2] ?? "";
    return quoted[1] === '"' && /[$`\\]/.test(line) ? undefined : line;
  }
  return /["'$`\\*?[\]{}~]/.test(text) ? undefined : text.replace(/[ \t]+/g, " ");
}
/** The refusal the dispatch CLI printed for `command` at `lines[at]`: that line, a
 * `dispatch <command> was not called: …` line or a usage refusal (which names no command), and
 * the `- ` lines under it. */
function refusalAt(
  lines: string[],
  at: number,
  command: string
): { text: string; end: number } | undefined {
  const [found] = cliRefusals(lines[at] ?? "");
  if (found === undefined || (found.command !== null && found.command !== command))
    return undefined;
  let end = at + 1;
  while (lines[end]?.startsWith("- ")) end++;
  return { text: lines.slice(at, end).join("\n"), end };
}

/** One thing a tool call did, in the order it did them: a read of a skill file, or one `dispatch`
 * command, with what the call's result shows of it. `answer` is the line of Dispatch's answer to a
 * write (ANSWER), or "". `refusal` is the refusal the result shows for the command, or undefined.
 * Whether Dispatch took the command is the CLI's ledger's to say (refusalBlocks). */
type Operation =
  | { kind: "read"; step: Step; target: string }
  | {
      kind: "dispatch";
      step: Step;
      call: DispatchCall;
      answer: string;
      refusal: string | undefined;
    };
/** A shell command of a call, in order, as operationsOf reads it. */
type ShellCommand =
  | { kind: "dispatch"; at: number; sep: string; call: DispatchCall; quiet: boolean }
  | { kind: "label"; at: number; sep: string; line: string }
  | { kind: "silent"; at: number; sep: string }
  | { kind: "closer"; at: number; sep: string }
  | { kind: "other"; at: number; sep: string };

/** An eval cell's code with each Python list-literal `dispatch` invocation, such as
 * `subprocess.run(["dispatch", "doc-edit", "--issue", "K-1", ...])`, its CLI named bare or by a
 * path to it (`"/usr/local/bin/dispatch"`), also written out as the shell command it runs, on a
 * line of its own after the list, so DISPATCH_COMMAND finds it as it finds a command in a bash
 * call. Each string literal of the list is one shell word; any other element (a variable,
 * `*args`) is the word `__EXPR__`, so a flag whose value is not a literal reads as no value the
 * run names. The characters that end a command in COMMAND_REST are dropped from a word, so a
 * summary holding `)` or `;` does not cut the command short. A call through a helper of the
 * agent's own (`d("doc-edit", ...)`) is still not seen. */
function withPythonDispatch(code: string): string {
  const list = /\[\s*(["'])(?:[^"'\]\s]*\/)?dispatch\1\s*,([^\]]*)\]/g;
  let out = "";
  let last = 0;
  for (const hit of code.matchAll(list)) {
    const end = (hit.index ?? 0) + hit[0].length;
    const words: string[] = [];
    for (const element of (hit[2] ?? "").split(/,(?=(?:[^"']*["'][^"']*["'])*[^"']*$)/)) {
      const item = element.trim();
      if (item === "") continue;
      const literal = /^(["'])(.*)\1$/s.exec(item);
      const word = literal
        ? (literal[2] ?? "").replace(/[\n;&|\x60)]/g, " ").replace(/'/g, "")
        : "__EXPR__";
      words.push(/^[\w@%+=:,./-]+$/.test(word) ? word : `'${word}'`);
    }
    const [command, ...rest] = words;
    if (command === undefined) continue;
    out += `${code.slice(last, end)}\ndispatch ${command} ${rest.join(" ")}\n`;
    last = end;
  }
  return out + code.slice(last);
}

/**
 * What a tool call did, in order (Operation).
 * - A `task` call does nothing here, and a `read` of a skill:// target or of a file under
 *   `skills/<name>/` reads it.
 * - A Dispatch tool a host calls by name is one `dispatch` command. Its result is that command's
 *   output, and the tool's failure is its refusal.
 * - A `bash` command or an `eval` cell runs each `dispatch` command DISPATCH_COMMAND finds in it
 *   (here-document bodies left out), and reads each skill file it names outside them: in its text,
 *   or in a here-document a command other than `dispatch` reads, at that command's place.
 * - Another tool reads the first skill file its arguments name.
 *
 * One result holds the output of every command of the call, with nothing to tell one command's
 * output from the next. So a command's output is attributed only where the output shows it. The
 * commands are walked in order with the line where the next output starts, while it is known:
 * the call's first line, then the line after a literal `echo` label the output shows, or after a
 * refusal attributed to the command before. A `dispatch` command's refusal is the refusal line at
 * the start of its own output (refusalAt). A write takes the next line of its kind of answer
 * (ANSWER), unless a refusal naming its command comes first. The call's failure (a `bash` exit of
 * 1 or 2, any failure of another tool) is the refusal of its last `dispatch` command when that
 * command ends the call, except one after `&&` that follows a shown refusal: the shell never ran
 * it.
 *
 * The refusals score does not rest on this attribution: the CLI logs each call it ran, refused or
 * not, in its ledger (refusalBlocks), and the attribution only says which command of a call a
 * ledger line belongs to. The output does not show:
 * - the end of another command's output (`cat`, `sed`, a `dispatch` command that succeeded), so a
 *   refusal after it could be that output's text;
 * - output a `$(…)` or a redirect to a file takes;
 * - how often a command in a loop ran: it is one command here;
 * - a write's answer, when another command prints a line of that answer before the write's own;
 * - which command of a failed call set its exit, when the call does not end with a `dispatch`
 *   command.
 */
function operationsOf(step: Step, outcome: Outcome | undefined): Operation[] {
  if (step.tool === "task") return [];
  if (step.tool === "read") {
    const target = typeof step.args.path === "string" ? step.args.path : "";
    return /^skill:\/\//.test(target) || /(^|\/)skills\/[^/]+\//.test(target)
      ? [{ kind: "read", step, target }]
      : [];
  }
  const text = outcome?.text ?? "";
  const output = text.replace(/\s*Command exited with code \d+\s*$/, "");
  const lines = (step.tool === "eval" ? output.replace(EVAL_EXIT, "") : output).split("\n");
  const failed =
    outcome?.failed === true && (step.tool !== "bash" || outcome.exit === 1 || outcome.exit === 2);
  const native = /^dispatch_([a-z_]+)$/.exec(step.tool)?.[1];
  if (native !== undefined) {
    const call = nativeCall(step, native);
    const answer = lines.find((line) => ANSWER[call.command]?.test(line)) ?? "";
    const refusal = failed ? output : undefined;
    return [{ kind: "dispatch", step, call, answer, refusal }];
  }
  if (step.tool !== "bash" && step.tool !== "eval") {
    const target = new RegExp(SKILL_PATH.source).exec(strings(step.args).join("\n"))?.[0];
    return target === undefined ? [] : [{ kind: "read", step, target }];
  }
  const raw = (step.tool === "bash" ? strings(step.args.command) : strings(step.args)).join("\n");
  const source = step.tool === "eval" ? withPythonDispatch(raw) : raw;
  const { code, bodies } = withoutBodies(source);
  const spans = [...code.matchAll(DISPATCH_COMMAND)].map((hit) => ({
    at: hit.index ?? 0,
    end: (hit.index ?? 0) + hit[0].length,
    command: hit[2] ?? "",
    text: (hit[1] ?? "").trim(),
  }));
  const inside = (at: number) => spans.some((span) => at >= span.at && at < span.end);
  const commands: ShellCommand[] = spans.map((span) => {
    const sep = separator(code, span.at);
    const call = shellCall(span.text, span.command);
    const quiet = sep === "$(" || sep === "`" || TO_FILE.test(span.text);
    return { kind: "dispatch", at: span.at, sep, call, quiet };
  });
  for (const hit of code.matchAll(SHELL_COMMAND)) {
    const at = hit.index ?? 0;
    const end = at + hit[0].length;
    const word = shellWord(hit[1] ?? "");
    // The `1` of a `2>&1`, a comment, or a command that overlaps a `dispatch` one.
    if (/^\d+$/.test(word) || word.startsWith("#")) continue;
    if (spans.some((span) => at < span.end && end > span.at)) continue;
    const sep = separator(code, at);
    const args = hit[2] ?? "";
    // The command a path names is the one its bare name names: `/bin/echo` prints as `echo` does.
    const name = path.posix.basename(word);
    const line = name === "echo" && !TO_FILE.test(args) ? echoed(args) : undefined;
    if (line !== undefined) commands.push({ kind: "label", at, sep, line });
    else if (CLOSERS.has(name)) commands.push({ kind: "closer", at, sep });
    else if (SILENT.has(name) || TO_FILE.test(args)) commands.push({ kind: "silent", at, sep });
    else commands.push({ kind: "other", at, sep });
  }
  commands.sort((a, b) => a.at - b.at);

  const operations: { at: number; operation: Operation }[] = [];
  for (const hit of code.matchAll(SKILL_PATH))
    if (!inside(hit.index ?? 0))
      operations.push({ at: hit.index ?? 0, operation: { kind: "read", step, target: hit[0] } });
  for (const body of bodies)
    if (!inside(body.at))
      for (const [target] of body.text.matchAll(SKILL_PATH))
        operations.push({ at: body.at, operation: { kind: "read", step, target } });

  const invocations: Extract<Operation, { kind: "dispatch" }>[] = [];
  let cursor: number | undefined = 0;
  let anchor = 0;
  for (const [place, command] of commands.entries()) {
    if (command.kind === "dispatch") {
      const start: number | undefined = command.quiet ? undefined : cursor;
      const shown: { text: string; end: number } | undefined =
        start === undefined ? undefined : refusalAt(lines, start, command.call.command);
      let answer = "";
      if (shown !== undefined) {
        cursor = shown.end;
        anchor = shown.end;
      } else {
        const kind = ANSWER[command.call.command];
        const writes =
          kind !== undefined &&
          command.call.flag("help") === undefined &&
          command.call.flag("dry-run") === undefined;
        if (writes && !command.quiet)
          for (let line = start ?? anchor; line < lines.length; line++) {
            const own = cliRefusals(lines[line] ?? "")[0];
            if (own?.command === command.call.command) {
              anchor = line + 1;
              break;
            }
            if (kind.test(lines[line] ?? "")) {
              answer = lines[line] ?? "";
              anchor = line + 1;
              break;
            }
          }
        if (!command.quiet) cursor = undefined;
      }
      const invocation = {
        kind: "dispatch" as const,
        step,
        call: command.call,
        answer,
        refusal: shown?.text,
      };
      invocations.push(invocation);
      operations.push({ at: command.at, operation: invocation });
      continue;
    }
    // A command whose output goes into a pipe prints nothing itself.
    if (command.kind === "silent" || command.kind === "closer") continue;
    if (commands[place + 1]?.sep === "|") continue;
    if (command.kind === "other") {
      cursor = undefined;
      continue;
    }
    const label = command.line;
    if (cursor !== undefined && lines[cursor] === label) {
      cursor += 1;
      anchor = Math.max(anchor, cursor);
      continue;
    }
    const found = label === "" ? -1 : lines.indexOf(label, anchor);
    cursor = found < 0 ? undefined : found + 1;
    if (found >= 0) anchor = found + 1;
  }
  const last = invocations.at(-1);
  const ending = commands.filter((command) => command.kind !== "closer").at(-1);
  if (failed && last !== undefined && ending?.kind === "dispatch" && ending.call === last.call) {
    const skipped =
      ending.sep === "&&" && invocations.slice(0, -1).some((other) => other.refusal !== undefined);
    if (!skipped) last.refusal ??= output;
  }
  return operations.sort((a, b) => a.at - b.at).map(({ operation }) => operation);
}
/** A reference to an issue or to one of its documents, as `--ref` takes it: `dispatch://<issue>`
 * or a dashboard URL (`…/issues/<issue>`), then `/spec` or `/artifact/<id>` (`/artifacts/<id>` in
 * a URL). It gives the issue, and the document: `spec`, or the artifact's id. */
const ISSUE_REF =
  /^(?:dispatch:\/\/|https?:\/\/[^/\s]+\/issues\/)([A-Z][A-Z0-9]{1,9}-[1-9]\d*)(?:\/(spec)|\/artifacts?\/([^/@?#\s]+)(?:@v\d+)?)?(?:[?#]\S*)?$/;
/** Whether one `dispatch` command wrote the run's primary spec: what it names, and the line of
 * Dispatch's answer operationsOf attributed to it (`answer`). The answer, not the exit, says
 * whether Dispatch made a version, so a command the CLI refused (`was not called`), an edit that
 * changed nothing (`no new version`) and a failed command give none, and a shell that exits
 * non-zero after the write does not take it back. A `--help` or `--dry-run` command writes nothing.
 * - `dispatch issue` with a spec, answered with the run's issue made (`Created <issue>:`).
 * - `dispatch doc-edit` of the run's issue (`--issue`, or the issue of a `--ref`) and of its spec
 *   (`--artifact`, or the document of the `--ref`; no document named is the issue's primary
 *   document, as the CLI resolves it), answered `Applied <n> ops (version <v>)`.
 * - `dispatch artifact` of the run's issue and of the spec's file, answered
 *   `Uploaded <name> as version <v>`.
 * A command of another issue, another document or a project's document writes no spec. */
function writesSpec(call: DispatchCall, fixture: SpecFixture, answer: string): boolean {
  if (call.flag("help") !== undefined || call.flag("dry-run") !== undefined) return false;
  const isSpec = (name: string | undefined) =>
    name !== undefined &&
    ["spec", "spec.md", "spec-md", fixture.spec_artifact.toLowerCase()].includes(
      name.toLowerCase()
    );
  if (call.command === "issue")
    return (
      (call.flag("spec") !== undefined || call.flag("spec-file") !== undefined) &&
      answer.startsWith(`Created ${fixture.issue}: `)
    );
  if (call.command === "doc-edit") {
    const named = ISSUE_REF.exec(call.flag("ref") ?? "");
    const document = call.flag("artifact") ?? named?.[2] ?? named?.[3];
    return (
      (named?.[1] ?? call.flag("issue")) === fixture.issue &&
      (document === undefined || isSpec(document)) &&
      /^Applied \d+ ops[^\n]*\(version \d+\)/.test(answer)
    );
  }
  if (call.command === "artifact")
    return (
      call.flag("issue") === fixture.issue &&
      isSpec(call.flag("name")) &&
      /^Uploaded \S+ as version \d+/.test(answer)
    );
  return false;
}
/** Every operation of a transcript's tool calls, in order: each call's (operationsOf), in the
 * order the calls were made. */
function operations(entries: SessionEntry[]): Operation[] {
  const results = outcomes(entries);
  return steps(entries).flatMap((step) => operationsOf(step, results.get(step.id)));
}
/** One line of the dispatch CLI's ledger (dispatch-session-state.ts `ResultEntry`): when the CLI
 * ended a call, the call's tool, and the refusal (`error`) when Dispatch or the call's own check
 * refused it. A line with no `error` is a call Dispatch took. */
const LedgerLine = z.looseObject({
  at: z.string(),
  tool: z.string(),
  error: z.string().optional(),
});
type LedgerLine = z.infer<typeof LedgerLine>;
/** The run's dispatch CLI ledger: `results.jsonl` in the CLI's state directory for the session the
 * transcript's header names, under the run's HOME (rig.sh base_env sets no other state directory).
 * The CLI logs every call it ran, refused or not, whatever the shell did with its output: a pipe,
 * `$(…)`, a loop or another program (Python's subprocess.run) hides nothing from it. A refusal the
 * CLI makes before the call reaches Dispatch is logged too whenever the call knows its session: a
 * refusal of a command's own arguments (`dispatch <command> was not called`, exit 1) under its
 * tool, and a usage refusal (`dispatch: …`, exit 2) under its tool, or under `dispatch` when the
 * CLI could not name the command. It logs no `--help` call that printed help and no dry run that
 * passed; a `--help` of an unknown command is a usage refusal and is logged. A run with
 * no ledger is the rig's fault: its CLI kept no record where the rig reads it. */
function ledgerOf(runDir: string, session: string): LedgerLine[] {
  const state = { XDG_STATE_HOME: path.join(runDir, "home", ".local", "state") };
  const file = path.join(sessionDirectory(state, session), "results.jsonl");
  if (!existsSync(file))
    throw new Error(
      `${file} is missing: the run's dispatch CLI logged no call where the rig reads`
    );
  return parsed(file, LedgerLine);
}
/** The ledger's name of a `dispatch` command's tool. */
const toolOf = (call: DispatchCall) => `dispatch_${call.command.replace(/-/g, "_")}`;
/** refusals: one block per refused call that no later call fixed from its refusal alone.
 * - The refusals: each ledger line with an `error`, and each refusal a call's output shows
 *   (operationsOf) that no ledger line belongs to, which is a refusal the CLI printed and did not
 *   log.
 * - A ledger line belongs to the tool call whose call time is at or before the line's time and
 *   whose result time is at or after it; of two such calls (one message's parallel calls), to the
 *   one whose result came first. Within that call it belongs to the next `dispatch` command of its
 *   tool, in the call's order, that the CLI logs. A line that no command takes (a command in a
 *   loop, one Python ran) belongs to the call alone, and one outside every call's time to no call.
 * - A refusal is fixed by the first ledger line with no `error` whose tool call the agent made after
 *   the refused call's result came back (after the line's time, for a line of no call): a call
 *   made before then cannot show that the agent fixed the refusal from its text.
 * - Each skill file read between the two breaks the fix: a read of the refused call that
 *   operationsOf does not order before the refused command, a read of the fixing call that it does
 *   not order after the fixing command, and every read of a call between them, or of another call
 *   in the message of either.
 * Each block names the call, the ledger's tool and time when the ledger holds it, its refusal text,
 * and the read or the missing fix. */
function refusalBlocks(
  runDir: string,
  entries: SessionEntry[],
  ops: Operation[],
  session: string
): string[] {
  const ledger = ledgerOf(runDir, session);
  const results = outcomes(entries);
  const calls = steps(entries).map((step) => {
    const result = results.get(step.id);
    return {
      step,
      start: Date.parse(step.at),
      end: Date.parse(result?.at ?? ""),
      seen: result?.index ?? Number.POSITIVE_INFINITY,
    };
  });
  type Call = (typeof calls)[number];
  const owner = (line: LedgerLine): Call | undefined => {
    const at = Date.parse(line.at);
    return calls
      .filter((call) => call.start <= at && at <= call.end)
      .sort((a, b) => a.end - b.end)[0];
  };
  // Each ledger line's call and, when a command of that call takes it, the command's place in ops.
  const lines = ledger.map((line) => ({
    line,
    call: owner(line),
    op: undefined as number | undefined,
  }));
  const logged = new Set<number>();
  for (const call of calls) {
    // The commands of the call the CLI logged: every one but a `--help` that printed help, and a
    // `--dry-run` only when its argument check refused it. The CLI checks a command's name before
    // its flags, so a help call of an unknown command is refused and logged like any usage
    // refusal. A usage refusal (`dispatch: …`) is logged under tool `dispatch` when the CLI could
    // not name the command, so the ledger line and the printed refusal are one refusal and match
    // here.
    const candidates = ops.flatMap((op, place) =>
      op.kind === "dispatch" &&
      op.step.id === call.step.id &&
      (op.call.flag("help") === undefined || op.refusal !== undefined) &&
      (op.call.flag("dry-run") === undefined || op.refusal !== undefined)
        ? [
            {
              place,
              tool: toolOf(op.call),
              refused: op.refusal !== undefined,
              usage: cliRefusals(op.refusal ?? "")[0]?.command === null,
            },
          ]
        : []
    );
    let next = 0;
    for (const entry of lines.filter((one) => one.call === call)) {
      // A line the CLI logged as a success is no command whose output shows a refusal.
      const found = candidates.findIndex(
        (candidate, index) =>
          index >= next &&
          (candidate.tool === entry.line.tool ||
            (entry.line.tool === "dispatch" && candidate.usage)) &&
          (entry.line.error !== undefined || !candidate.refused)
      );
      const candidate = candidates[found];
      if (candidate === undefined) continue;
      entry.op = candidate.place;
      logged.add(candidate.place);
      next = found + 1;
    }
  }
  const callOf = (step: Step) => calls.find((call) => call.step.id === step.id);
  const refusals = [
    ...lines.flatMap((entry) => {
      if (entry.line.error === undefined) return [];
      const op = entry.op === undefined ? undefined : ops[entry.op];
      const where =
        op?.kind === "dispatch"
          ? `${op.call.text} (${entry.line.tool}, logged at ${entry.line.at})`
          : `${entry.line.tool}, logged at ${entry.line.at}, ${entry.call ? `in tool call ${entry.call.step.id}` : "in no tool call's time"}`;
      return [
        {
          where,
          text: entry.line.error.trim(),
          call: entry.call,
          op: entry.op,
          at: Date.parse(entry.line.at),
        },
      ];
    }),
    ...ops.flatMap((op, place) => {
      if (op.kind !== "dispatch" || op.refusal === undefined || logged.has(place)) return [];
      const call = callOf(op.step);
      return [{ where: op.call.text, text: op.refusal, call, op: place, at: call?.start ?? 0 }];
    }),
    // An `eval` cell runs `dispatch` through its own language, and Oh My Pi gives its kernel an
    // environment of allowlisted names alone, with no session id in it, so the CLI knows no
    // session and logs nothing. withPythonDispatch reads a list-literal call as a command, but its
    // refusal is attributed to that command only when refusalAt finds it; otherwise the cell's
    // refusals are in its output alone, and each counts once, for the cell.
    ...calls.flatMap((call) => {
      if (call.step.tool !== "eval") return [];
      if (lines.some((entry) => entry.call === call)) return [];
      if (
        ops.some(
          (op) => op.kind === "dispatch" && op.step.id === call.step.id && op.refusal !== undefined
        )
      )
        return [];
      return cliRefusals((results.get(call.step.id)?.text ?? "").replace(EVAL_EXIT, "")).map(
        (found) => ({
          where: `eval cell ${call.step.id}, which the CLI logged no call of`,
          text: found.line,
          call: call as Call | undefined,
          op: undefined as number | undefined,
          at: call.start,
        })
      );
    }),
  ].sort((a, b) => a.at - b.at || (a.op ?? 0) - (b.op ?? 0));

  const blocks: string[] = [];
  for (const refusal of refusals) {
    const after = (call: Call) =>
      refusal.call === undefined ? call.start > refusal.at : call.step.index > refusal.call.seen;
    const fix = lines.find(
      (entry) => entry.line.error === undefined && entry.call !== undefined && after(entry.call)
    );
    type Read = Extract<Operation, { kind: "read" }>;
    const between = (op: Operation, place: number): op is Read => {
      if (op.kind !== "read" || fix?.call === undefined) return false;
      const call = callOf(op.step);
      if (call === undefined) return false;
      if (call === refusal.call) return refusal.op === undefined || place > refusal.op;
      if (call === fix.call) return fix.op === undefined || place < fix.op;
      const from =
        refusal.call === undefined
          ? call.start >= refusal.at
          : call.step.index >= refusal.call.step.index;
      return from && call.step.index <= fix.call.step.index;
    };
    const read = ops.find(between);
    if (fix !== undefined && read === undefined) continue;
    blocks.push(
      [
        `refused call: ${refusal.where}`,
        indented(refusal.text, 4),
        fix === undefined || read === undefined
          ? "    no later dispatch call succeeded"
          : `    then read ${read.target} before the next dispatch call succeeded`,
      ].join("\n")
    );
  }
  return blocks;
}
/** Whether a skill file read is skill `name` itself. */
const readsSkill = (target: string, name: string) =>
  new RegExp(`^skill://${escaped(name)}(/SKILL\\.md)?$|/skills/${escaped(name)}/SKILL\\.md$`).test(
    target
  );

/** A spec-only check that counts for a spec write (specWork): a `task` call whose every task
 * completed with a report that came back before the write (checkOutcome), or a subagent of the
 * session started some other way, such as from an eval cell, whose transcript holds its report
 * (subagentCheck). `assignment` is every string of the call's arguments, or the first user message
 * of the subagent's transcript; `report` each task's report, or the subagent's; and `write` the
 * place of the write in SpecWork.writes. */
export interface SpecCheck {
  assignment: string;
  report: string;
  write: number;
}
/** A spec-only check that counts for no spec write: its tasks, where the scorer found it (`call
 * <id>`, or `transcript <name>.jsonl`), the place of the write after it, and why. */
export interface RejectedCheck {
  tasks: string;
  via: string;
  write: number;
  problem: string;
}
export interface SpecWork {
  /** Each spec write, in order, and whether a spec-only check that counts came after the write
   * before it (or the session's start) and before it: each write the transcript shows Dispatch
   * accepted, and each version Dispatch recorded for the session that none of those made
   * (`unseen`), at its place in time. */
  writes: { index: number; at: string; text: string; checked: boolean }[];
  checks: SpecCheck[];
  /** Spec-only checks that count for a write's `checked` but whose findings are not judged: the
   * assignment pastes the run's own spec and a sibling issue's spec together (in one assignment,
   * or in one task of a call), so a finding of its report cannot be told to be about this spec. A
   * call's tasks that review only the run's own spec stay in `checks`, with their reports alone. */
  unjudged: RejectedCheck[];
  rejected: RejectedCheck[];
  /** The spec writes the transcript shows, and the versions spec-versions.json records for the
   * session. */
  seen: number;
  versions: number;
  /** The recorded versions no write the transcript shows made, each at the first entry after its
   * time (`index`), and the texts of the writes it shows that no recorded version matches. */
  unseen: { version: number; at: string; index: number }[];
  unmatched: string[];
}
/** The opening tag of Oh My Pi's envelope of one task's end (prompts/tools/task-summary.md). Its
 * attributes give the task's id. taskNames reads the opening tags alone, so a closing tag that a
 * report quotes cannot make its list shorter. A greedy capture of the body would merge the
 * envelopes of a blocking result of several tasks into one match and drop every name after the
 * first, so a task that failed would not be required.
 * The scan lists names conservatively; it does not tell which task is which. An opening tag that a
 * report quotes can add a name, and no tag can remove one. An added name with no completed record
 * rejects the check (`no report of task <name> arrived`). An added name that has a completed record
 * of its own, such as another task that finished after this call, brings that task's unrelated
 * report into the check, and the check counts. A task of the call that failed still rejects it,
 * since its name stays in the list. To close the gap, take the call's task ids from its structured
 * results, not from the text. */
const TASK_RESULT_OPEN = /<task-result\b([^>]*)>/g;
const attribute = (attributes: string, name: string) =>
  new RegExp(`\\b${name}="([^"]*)"`).exec(attributes)?.[1];
/** A task's report in its envelope's body: the `<output>` or `<preview>` part, else the body less
 * its `<meta>` and `<abort-reason>` lines. Oh My Pi's `(no output)` placeholder is no report. */
function reportOf(body: string): string {
  const inner =
    /<(output|preview)\b[^>]*>([\s\S]*?)<\/\1>/.exec(body)?.[2] ??
    body.replace(/<meta\b[^>]*\/>|<abort-reason>[\s\S]*?<\/abort-reason>/g, "");
  const report = inner.trim();
  return /^\(no output\)/.test(report) ? "" : report;
}
/** A record's field, when the record is an object. */
const field = (value: unknown, name: string): unknown =>
  typeof value === "object" && value !== null
    ? (value as Record<string, unknown>)[name]
    : undefined;
/** The status Oh My Pi's envelope gives a task from its `details.results` entry
 * (task/result-summary.ts). */
function statusOf(result: unknown): string {
  const exit = field(result, "exitCode");
  if (field(result, "aborted") === true) return "cancelled";
  if (exit === 0) return field(result, "error") ? "merge failed" : "completed";
  return `failed (exit ${typeof exit === "number" ? exit : "?"})`;
}
/** The names of a `task` call's tasks: each one its arguments name, each a background spawn's
 * result names (`` `<name>` (job ``), and each a blocking result's envelope names. */
function taskNames(step: Step, own: Outcome | undefined): string[] {
  const names = new Set<string>();
  const tasks = Array.isArray(step.args.tasks) ? step.args.tasks : [step.args];
  for (const task of tasks) {
    const name = field(task, "name");
    if (typeof name === "string" && name !== "") names.add(name);
  }
  const text = own?.text ?? "";
  for (const [, name] of text.matchAll(/`([^`\s]+)` \(job/g)) if (name) names.add(name);
  for (const [, attributes = ""] of text.matchAll(TASK_RESULT_OPEN)) {
    const id = attribute(attributes, "id");
    if (id) names.add(id);
  }
  return [...names];
}
/** The text Oh My Pi delivered for job `name` in the body of an `async-result` message that
 * delivers the jobs `names`. Its task jobs carry no `resultText`, so the body is the only report.
 * A message of one job is a `Background job <name> has completed…` line, then the job's envelope;
 * a message of several is a count line, then each job under a `── Job <id> (<label>) ──` line.
 * Only a line naming a job this message delivers frames a section, since a report may quote such a
 * line of its own. A job's section runs to the next framing line of another job, or to the end. A
 * message that frames the job's own section other than once gives it no text: which section is the
 * job's own cannot be told. */
function jobSection(body: string, name: string, names: string[]): string {
  const text = body.replace(/^\s*<system-notice>\s*/, "").replace(/\s*<\/system-notice>\s*$/, "");
  if (names.length === 1) {
    const lead = new RegExp(`^Background job ${escaped(name)} has completed\\.[^\\n]*\\n`).exec(
      text
    );
    return lead === null ? "" : text.slice(lead[0].length);
  }
  const marks = [...text.matchAll(/^── Job (\S+) \(.*\) ──$/gm)]
    .map((mark) => ({
      id: mark[1],
      start: mark.index ?? 0,
      end: (mark.index ?? 0) + mark[0].length,
    }))
    .filter((mark) => mark.id !== undefined && names.includes(mark.id));
  const own = marks.filter((mark) => mark.id === name);
  const mark = own[0];
  if (own.length !== 1 || mark === undefined) return "";
  const after = marks.find((other) => other.start > mark.start && other.id !== name);
  return text.slice(mark.end, after?.start ?? text.length);
}
/** What a spec-only check came to before the spec write whose assistant message is entry `before`:
 * its tasks' reports, or why it does not count. Every task the call started must have ended
 * `completed` with a report that is not empty, in an entry before the write. A report counts only
 * from Oh My Pi's structured task delivery: a blocking task call's own `details.results`, or the
 * `details.jobs` entry of a `wait` result or `async-result` message whose job is that task. A
 * blocking result's envelope is read only when Oh My Pi recorded no `details.results`. A `wait` that
 * returned a message, or an async delivery of another job, delivers text and never a task's end.
 * A report body is opaque: no envelope inside it counts as another task's report. A report that
 * arrives after the write is no report for it. A task call the tool failed (`isError`) checked
 * nothing. */
function checkOutcome(
  entries: SessionEntry[],
  step: Step,
  own: Outcome | undefined,
  before: number,
  /** The tasks whose reports make the check's report; every task of the call must still end. */
  only?: string[]
): { report: string } | { problem: string } {
  if (own === undefined) return { problem: "the task call has no result" };
  if (own.index > before) return { problem: "the task call's result arrived after the spec write" };
  if (own.failed)
    return { problem: `the task call failed: ${own.text.trim().split("\n")[0] ?? ""}` };
  const ended: { name: string; status: string; report: string; index: number }[] = [];
  const results = field(own.details, "results");
  if (Array.isArray(results) && results.length > 0)
    for (const result of results) {
      const output = field(result, "output");
      ended.push({
        name: String(field(result, "id") ?? ""),
        status: statusOf(result),
        report: typeof output === "string" ? output.trim() : "",
        index: own.index,
      });
    }
  // As in the delivered branch below: no end anchor, and a greedy capture. A blocking result has no
  // sections, so only reportOf bounds the report, and the same limit applies.
  else {
    const envelope = /^\s*<task-result\b([^>]*)>([\s\S]*)<\/task-result>/.exec(own.text);
    if (envelope !== null)
      ended.push({
        name: attribute(envelope[1] ?? "", "id") ?? "",
        status: attribute(envelope[1] ?? "", "status") ?? "no status",
        report: reportOf(envelope[2] ?? ""),
        index: own.index,
      });
  }
  for (const [index, entry] of entries.entries()) {
    if (index <= step.index || index === own.index) continue;
    const details =
      entry.type === "custom_message" && entry.customType === "async-result"
        ? entry.details
        : entry.message?.role === "toolResult" && entry.message.toolName === "wait"
          ? entry.message.details
          : undefined;
    const jobs = field(details, "jobs");
    if (!Array.isArray(jobs)) continue;
    // The jobs this message delivers, by the name each is recorded under: a section's framing line
    // names one of these, and nothing else frames a section.
    const delivered = jobs
      .map((job) => field(job, "id") ?? field(job, "jobId") ?? field(job, "agentId"))
      .filter((name): name is string => typeof name === "string");
    for (const job of jobs) {
      if (field(job, "type") !== "task") continue;
      const name = field(job, "id") ?? field(job, "jobId") ?? field(job, "agentId");
      const status = field(job, "status");
      if (typeof name !== "string" || typeof status !== "string") continue;
      const resultText = field(job, "resultText");
      const text =
        typeof resultText === "string"
          ? resultText
          : jobSection(strings(entry.content).join(""), name, delivered);
      // Oh My Pi writes text of its own after the closing tag of most deliveries (the agent is
      // idle, how to message it, where its transcript is), so the envelope is not the text's end.
      // The capture is greedy. A lazy capture would end at the first `</task-result>` that the
      // report quotes and drop every finding after it; the agent being scored writes its checker's
      // prompt, so it could ask for exactly that. The greedy body can run past the real closing
      // tag: Oh My Pi escapes no `task-result` tag, and text it writes after the envelope can hold
      // one. Two rules bound the report. In a message that delivers several jobs, jobSection ends
      // this job's section at the framing line of the next job the message delivers. reportOf
      // takes the first complete `<output>` or `<preview>` part, and task-summary.md always writes
      // one, so text after that part is not in the report.
      // Limit: two envelopes with neither part, with text between them, put that text and the
      // second body into the report. The pinned task-summary.md always writes one of the two parts,
      // so no supported producer gives that shape.
      const envelope = /^\s*<task-result\b([^>]*)>([\s\S]*)<\/task-result>/.exec(text);
      const body =
        envelope !== null && attribute(envelope[1] ?? "", "id") === name ? (envelope[2] ?? "") : "";
      ended.push({ name, status, report: reportOf(body), index });
    }
  }
  const names = taskNames(step, own);
  if (names.length === 0)
    return { problem: "the call names no task, and no task's report arrived" };
  const problems: string[] = [];
  const reports: string[] = [];
  for (const name of names) {
    // A job snapshot of a task still running is no end: a `wait` answers with every job it waited
    // on, the running ones included, and the completion follows in a later entry. Taking a
    // completed record over a running one is safe only because Oh My Pi gives a repeated task name
    // a new id (`C`, then `C-2`), so every record of one name belongs to one run of that task.
    // This code ties no record to the call that started it: were two runs to share a name, a
    // second run's completion could answer for the first.
    const seen = ended.filter((one) => one.name === name && one.index < before);
    const end = seen.find((one) => one.status === "completed") ?? seen[0];
    if (end === undefined)
      problems.push(
        ended.some((one) => one.name === name)
          ? `the report of task ${name} arrived after the spec write`
          : `no report of task ${name} arrived`
      );
    else if (end.status !== "completed") problems.push(`task ${name} ended ${end.status}`);
    else if (end.report === "") problems.push(`the report of task ${name} is empty`);
    else if (only === undefined || only.includes(name)) reports.push(end.report);
  }
  return problems.length > 0 ? { problem: problems.join("; ") } : { report: reports.join("\n\n") };
}
/** A `task` call's tasks, each with the assignment it was given: the call's shared arguments (its
 * `context`) and the task's own. Undefined for a call of one task, or one whose every task does not
 * carry a name, since a task's report is found by its name. */
function namedTasks(step: Step): { name: string; assignment: string }[] | undefined {
  const tasks = step.args.tasks;
  if (!Array.isArray(tasks) || tasks.length < 2) return undefined;
  const shared = strings(
    Object.fromEntries(Object.entries(step.args).filter(([key]) => key !== "tasks"))
  );
  const parts: { name: string; assignment: string }[] = [];
  for (const task of tasks) {
    const name = field(task, "name");
    if (typeof name !== "string" || name === "") return undefined;
    parts.push({ name, assignment: [...shared, ...strings(task)].join("\n\n") });
  }
  return parts;
}
/** Whether a check's assignment is a spec-only check's: it holds the outline's headings as `##`
 * lines and names no path under a checkout in `reach`. */
function specOnly(assignment: string, reach: string[]): boolean {
  const outlined = OUTLINE.every((heading) =>
    new RegExp(`^[ \\t]*## ${escaped(heading)}[ \\t]*$`, "m").test(assignment)
  );
  const local = reach.some((dir) => new RegExp(`${escaped(dir)}(?![\\w.-])`).test(assignment));
  return outlined && !local;
}
/** A subagent's run as its own transcript records it: its assignment (the first user message) and
 * when that came, and its report and when that came. The report is the subagent's last assistant
 * message: the arguments of the `yield` call it ends with, else its text. A `yield` that carries
 * nothing but its `type` (`{"type":"result"}`) hands back the text the subagent wrote before it, so
 * the report is then the text of the last assistant message that has text. A last message that is
 * neither (the subagent never finished) is no report. */
function subagentRun(
  entries: SessionEntry[]
): { assignment: string; started: number; report: string; reported: number } | undefined {
  const first = entries.find((entry) => entry.message?.role === "user");
  if (first === undefined) return undefined;
  const assistant = entries.filter((entry) => entry.message?.role === "assistant");
  const last = assistant.at(-1);
  const textOf = (entry: SessionEntry | undefined) =>
    (entry?.message?.content ?? [])
      .flatMap((part) => (part.type === "text" ? [part.text ?? ""] : []))
      .join("");
  const ending = (last?.message?.content ?? []).find(
    (part) => part.type === "toolCall" && part.name === "yield"
  );
  let report = textOf(last);
  if (ending !== undefined) {
    const { type: _type, ...carried } =
      typeof ending.arguments === "object" && ending.arguments !== null
        ? (ending.arguments as Record<string, unknown>)
        : {};
    report = strings(carried).join("\n\n");
    if (report.trim() === "")
      report = textOf(assistant.findLast((entry) => textOf(entry).trim() !== ""));
  }
  return {
    assignment: (first.message?.content ?? [])
      .flatMap((part) => (part.type === "text" ? [part.text ?? ""] : []))
      .join(""),
    started: Date.parse(first.timestamp ?? ""),
    report: report.trim(),
    reported: Date.parse(last?.timestamp ?? ""),
  };
}
/** The 5-word runs of a text, lowercased, its words its runs of letters and digits. */
function wordRuns(text: string): Set<string> {
  const words = text.toLowerCase().match(/[a-z0-9]+/g) ?? [];
  const runs = new Set<string>();
  for (let start = 0; start + 5 <= words.length; start++)
    runs.add(words.slice(start, start + 5).join(" "));
  return runs;
}
/** Each spec an assignment pastes: from a `## Summary` line to the end of the `## Decisions`
 * section after it, the last of the outline (the next `#` or `##` heading, or the end), or to the
 * next `## Summary` line.
 * What the assignment adds after a spec (instructions, the person's messages, the answered asks)
 * is in no slice, so it neither lowers nor raises a spec's share. */
function specSlices(assignment: string): string[] {
  const lines = assignment.split("\n");
  const summary = /^[ \t]*## Summary[ \t]*$/;
  const slices: string[] = [];
  for (let start = 0; start < lines.length; start++) {
    if (!summary.test(lines[start] ?? "")) continue;
    let end = start + 1;
    let decisions = false;
    for (; end < lines.length; end++) {
      const line = lines[end] ?? "";
      if (summary.test(line)) break;
      if (/^[ \t]*## Decisions[ \t]*$/.test(line)) decisions = true;
      else if (decisions && /^[ \t]*#{1,2} /.test(line)) break;
    }
    slices.push(lines.slice(start, end).join("\n"));
    start = end - 1;
  }
  return slices;
}
/** The least share of a version the run's session wrote that a pasted spec covers, for the check
 * to be of this run's spec. An agent that splits out a sibling issue can send the sibling's spec
 * for review, alone or after its own spec in one assignment; that spec holds the outline's
 * headings too, but the report's findings on it are about another spec. Each spec of an
 * assignment is measured on its own (specWork's kindOf), and one that pastes both is `mixed`.
 * The share is recall over the written version (how much of the version's 5-word runs the
 * pasted spec holds, the best over the versions), not over the pasted text: text pasted after
 * the spec, with no heading to end its slice, cannot lower it, and a sibling spec that borrows
 * a few of the parent's sections covers little of the parent. On the 67 spec runs of the
 * dispatch skills' acceptance measurement, each of the 9 sibling specs sent for review (7 alone,
 * 2 after the run's own) covered at most 0.01 of any version, and each of the run's own specs at
 * least 0.24, a draft the agent rewrote in plain words after the check included. */
const OWN_SPEC = 0.1;
/** Each pasted spec's best share, over the versions the session wrote (each version's 5-word
 * runs), of a version's runs that the spec holds, in order. */
function ownSpecShares(assignment: string, versions: Set<string>[]): number[] {
  return specSlices(assignment).map((slice) => {
    const pasted = wordRuns(slice);
    if (pasted.size === 0) return 1;
    return Math.max(
      0,
      ...versions.map((version) =>
        version.size === 0 ? 0 : [...version].filter((run) => pasted.has(run)).length / version.size
      )
    );
  });
}
/** The spec writes of the run's session (writesSpec, against the run's fixture.json) and the
 * spec-only checks before them: the one list of checks that both specRow's `checked` and the
 * findings judge (judgeInputs, judge.ts) read. A spec-only check's assignment holds the outline's
 * headings as `##` lines and names no path under a checkout in `reach` (specOnly). A check
 * is either of:
 * - a `task` call of the session's transcript. It counts for the first spec write after it when its
 *   tasks' reports came back before that write (checkOutcome); either way it counts for no later
 *   write. Places are transcript order.
 * - a subagent transcript beside the session's (subagentSessions) that no `task` call of the
 *   session names, as a subagent started from an eval cell (`tool.task(…)`, `agent(…)`) is. Its
 *   assignment is its first user message and its report its last assistant message
 *   (subagentRun). It counts for the first spec write after its report when it started after the
 *   write before that one, by the transcripts' timestamps. A subagent that a `task` call names is
 *   that call's check alone, so a call rejected for its path or its failed task stays rejected.
 * Each spec a check pastes is the run's own or a sibling issue's (ownSpecShares). A check of only
 * a sibling's spec counts for no write. One that pastes both, in one assignment or in one task of
 * a call, counts for `checked`, since the run's own spec was reviewed, but is in `unjudged`, not
 * `checks`: no finding of its report can be told to be about this spec. A call's tasks are taken
 * one by one, so its tasks that review only the run's own spec stay in `checks`, their reports
 * alone.
 * Each `dispatch` command of a call is a write of its own (operationsOf), so two writes in one
 * call with no check between them leave the second unchecked.
 *
 * A write the transcript cannot show, as one through a helper function of the agent's own, a list
 * built in a variable, or an answer line `>/dev/null` hid, still made a version, and
 * spec-versions.json records each version with its session and time. So the writes the
 * transcript shows are matched to the versions recorded for the session (sessionVersions): each to
 * the version its answer names, else to the first one left that Dispatch made at or after the
 * write's call. A recorded version no shown write matches (`unseen`) is a spec write too, at its
 * recorded time and at the place in the transcript that time falls, and is checked only when a
 * check above counts for it. A shown write no recorded version matches (`unmatched`) is still a
 * write; specRow flags both. The checks are in the order of the writes they count for, and of when
 * each started. */
export function specWork(runDir: string, reach: string[], fixture: SpecFixture): SpecWork {
  const entries = session(runDir);
  const results = outcomes(entries);
  const own = sessionId(entries);
  // Each call of the session with the spec writes it shows, apart from the checks below, so the
  // writes are known before they are matched to the recorded versions.
  const calls = steps(entries).map((step) => ({
    step,
    shown:
      step.tool === "task"
        ? []
        : operationsOf(step, results.get(step.id)).flatMap((operation) =>
            operation.kind === "dispatch" && writesSpec(operation.call, fixture, operation.answer)
              ? [{ text: operation.call.text, answer: operation.answer }]
              : []
          ),
  }));
  const recorded = (own === undefined ? [] : sessionVersions(runDir, own)).map((version) => ({
    version: version.version,
    at: version.created_at,
    matched: false,
  }));
  const unmatched: string[] = [];
  for (const { step, shown } of calls)
    for (const write of shown) {
      const named = Number(/\bversion (\d+)\b/.exec(write.answer)?.[1]);
      const match =
        recorded.find((version) => !version.matched && version.version === named) ??
        recorded.find(
          (version) => !version.matched && Date.parse(version.at) >= Date.parse(step.at)
        );
      if (match === undefined) unmatched.push(write.text);
      else match.matched = true;
    }
  // Each unseen version at the first entry after its time: a call whose message came later cannot
  // have checked it.
  const unseen = recorded
    .filter((version) => !version.matched)
    .map((version) => {
      const after = entries.findIndex(
        (entry) => Date.parse(entry.timestamp ?? "") > Date.parse(version.at)
      );
      return { ...version, index: after < 0 ? entries.length : after };
    });
  // The 5-word runs of each version the session wrote: each spec a check pastes must cover enough
  // of one of them to be this run's spec, not a sibling issue's (ownSpecShares, OWN_SPEC).
  const versionRuns: Set<string>[] = [];
  for (const { version } of recorded) {
    const file = path.join(runDir, "spec-versions", `${version}.md`);
    if (existsSync(file)) versionRuns.push(wordRuns(readFileSync(file, "utf8")));
  }
  // What the specs an assignment pastes are: `own` when each is the run's own (or it pastes none),
  // `sibling` when none is, `mixed` when it pastes both. Only `own` gives findings a judge can tell
  // to be about this spec; `mixed` still shows the run's own spec was reviewed before the write.
  const kindOf = (assignment: string): { kind: "own" | "sibling" | "mixed"; note: string } => {
    const shares = ownSpecShares(assignment, versionRuns);
    const low = shares.findIndex((share) => share < OWN_SPEC);
    if (low < 0) return { kind: "own", note: "" };
    return {
      kind: shares.some((share) => share >= OWN_SPEC) ? "mixed" : "sibling",
      note: `spec ${low + 1} of ${shares.length} it pastes covers at most ${Math.round((shares[low] ?? 0) * 100)}% of the 5-word runs of any version the session wrote`,
    };
  };
  const notOwn = (note: string) => `its spec is not this issue's: ${note}`;
  const notJudged = (kind: "sibling" | "mixed", note: string) =>
    kind === "mixed"
      ? `its findings are not judged: it pastes a sibling issue's spec beside this one (${note})`
      : `its findings are not judged: it pastes a sibling issue's spec only (${note})`;

  const writes: SpecWork["writes"] = [];
  const checks: (SpecCheck & { started: number })[] = [];
  const unjudged: RejectedCheck[] = [];
  const rejected: RejectedCheck[] = [];
  const named = new Set<string>();
  interface Pending {
    step: Step;
    assignment: string;
    /** The tasks whose reports are the check's report, when the call's other tasks are left out. */
    only?: string[];
    /** Why the check counts for no write (no spec of the run's own), or why its findings are not
     * judged; `judged` false with no `reject` means it counts for `checked` alone. */
    reject?: string;
    judged: boolean;
    /** The call's tasks left out of the findings, each with what it pastes and why. */
    dropped?: { tasks: string; kind: "sibling" | "mixed"; note: string }[];
  }
  let pending: Pending[] = [];
  // A write at entry `index`: each pending `task` call's check counts for it, or is rejected.
  const record = (index: number, at: string, text: string) => {
    let checked = false;
    for (const check of pending) {
      const result = results.get(check.step.id);
      const tasks = taskNames(check.step, result).join(", ") || "(unnamed)";
      const via = `call ${check.step.id}`;
      const outcome =
        check.reject === undefined
          ? checkOutcome(entries, check.step, result, index, check.only)
          : { problem: check.reject };
      if ("problem" in outcome) {
        rejected.push({ tasks, via, write: writes.length, problem: outcome.problem });
        continue;
      }
      checked = true;
      for (const left of check.dropped ?? [])
        unjudged.push({
          tasks: left.tasks || tasks,
          via,
          write: writes.length,
          problem: notJudged(left.kind, left.note),
        });
      if (check.judged)
        checks.push({
          assignment: check.assignment,
          report: outcome.report,
          write: writes.length,
          started: Date.parse(check.step.at),
        });
    }
    writes.push({ index, at, text, checked });
    pending = [];
  };
  let next = 0;
  for (const { step, shown } of [...calls, { step: undefined, shown: [] }]) {
    for (; next < unseen.length; next++) {
      const version = unseen[next];
      if (version === undefined || (step !== undefined && version.index > step.index)) break;
      record(
        version.index,
        version.at,
        `version ${version.version} (recorded ${version.at}; no command of the transcript shows it)`
      );
    }
    if (step === undefined) break;
    if (step.tool === "task") {
      for (const name of taskNames(step, results.get(step.id))) named.add(name);
      const assignment = strings(step.args).join("\n\n");
      if (!specOnly(assignment, reach)) continue;
      // A call of several named tasks is judged task by task, each on its own assignment (the
      // call's shared context and the task's own text): a task that is not itself a spec-only check
      // (specOnly), as a fact check given no spec is, is no part of the check, even when the call's
      // other tasks carry the outline. Of the rest, a task that reviews only the run's own spec is
      // the check, with its report alone; one that pastes a sibling's spec too, or only a
      // sibling's, is left out of the findings. The call counts for `checked` when any task
      // reviews the run's own spec.
      const tasks = namedTasks(step);
      const parts = (tasks ?? [{ name: "", assignment }])
        .filter((part) => specOnly(part.assignment, reach))
        .map((part) => ({ ...part, ...kindOf(part.assignment) }));
      if (parts.length === 0) continue;
      const own = parts.filter((part) => part.kind === "own");
      const left = parts.filter((part) => part.kind !== "own");
      const first = left[0];
      const whole = tasks === undefined || own.length === tasks.length;
      if (first === undefined)
        pending.push(
          whole
            ? { step, assignment, judged: true }
            : {
                step,
                assignment: own.map((part) => part.assignment).join("\n\n"),
                only: own.map((part) => part.name),
                judged: true,
              }
        );
      else if (!parts.some((part) => part.kind === "mixed") && own.length === 0)
        pending.push({ step, assignment, judged: false, reject: notOwn(first.note) });
      else
        pending.push({
          step,
          assignment: own.map((part) => part.assignment).join("\n\n"),
          only: own.length > 0 ? own.map((part) => part.name) : undefined,
          judged: own.length > 0,
          dropped: left.map((part) => ({
            tasks: part.name,
            kind: part.kind === "mixed" ? "mixed" : "sibling",
            note: part.note,
          })),
        });
      continue;
    }
    for (const write of shown) record(step.index, step.at, write.text);
  }
  const writeTimes = writes.map((write) => Date.parse(write.at));
  for (const { name, entries: subagent } of subagentSessions(runDir)) {
    if (named.has(name)) continue;
    const run = subagentRun(subagent);
    if (run === undefined || !specOnly(run.assignment, reach)) continue;
    const write = writeTimes.findIndex((at) =>
      run.report === "" ? at > run.started : at > run.reported
    );
    if (write < 0) continue;
    const before = writeTimes[write - 1];
    const { kind, note } = kindOf(run.assignment);
    const problem =
      run.report === ""
        ? "its transcript holds no report"
        : before !== undefined && run.started <= before
          ? `it started before spec write ${write}, so it checked an earlier version`
          : kind === "sibling"
            ? notOwn(note)
            : undefined;
    if (problem !== undefined) {
      rejected.push({ tasks: name, via: `transcript ${name}.jsonl`, write, problem });
      continue;
    }
    const counted = writes[write];
    if (counted !== undefined) counted.checked = true;
    if (kind === "mixed")
      unjudged.push({
        tasks: name,
        via: `transcript ${name}.jsonl`,
        write,
        problem: notJudged("mixed", note),
      });
    else
      checks.push({ assignment: run.assignment, report: run.report, write, started: run.started });
  }
  return {
    writes,
    checks: checks
      .toSorted((a, b) => a.write - b.write || a.started - b.started)
      .map(({ assignment, report, write }) => ({ assignment, report, write })),
    unjudged: unjudged.toSorted((a, b) => a.write - b.write),
    rejected: rejected.toSorted((a, b) => a.write - b.write),
    seen: calls.reduce((sum, call) => sum + call.shown.length, 0),
    versions: recorded.length,
    unseen: unseen.map(({ version, at, index }) => ({ version, at, index })),
    unmatched,
  };
}

/** Each `**R<n>.**` paragraph of a spec: its mark, the line it starts on, and its text, up to a
 * blank line, a heading or the next list item. */
function marked(spec: string): { mark: string; line: number; body: string }[] {
  const found: { mark: string; line: number; body: string }[] = [];
  let current: { mark: string; line: number; body: string } | undefined;
  spec.split("\n").forEach((line, index) => {
    const mark = /^\s*(?:[-*+]\s+|\d+[.)]\s+)?\*\*(R\d+)\.\*\*/.exec(line)?.[1];
    if (mark !== undefined) {
      current = { mark, line: index, body: line };
      found.push(current);
    } else if (
      current === undefined ||
      line.trim() === "" ||
      /^\s*(#|:::|[-*+]\s|\d+[.)]\s)/.test(line)
    )
      current = undefined;
    else current.body += `\n${line}`;
  });
  return found;
}
/** Every line of a spec that a `:::requirement` block holds. */
function requirementBlockLines(spec: string): Set<number> {
  const inside = new Set<number>();
  let open = false;
  spec.split("\n").forEach((line, index) => {
    if (/^\s*:::+\s*requirement\b/.test(line)) open = true;
    else if (/^\s*:::+\s*$/.test(line)) open = false;
    else if (open) inside.add(index);
  });
  return inside;
}
/** The requirement marks of a spec (`R1`, `R2`, …), in order. */
export function requirements(spec: string): string[] {
  return marked(withoutFences(spec)).map((paragraph) => paragraph.mark);
}
/** Replaces each match of `pattern` with spaces and keeps the line breaks, so an offset into the
 * result still points into the text. */
function blank(text: string, pattern: RegExp): string {
  return text.replace(pattern, (hit) => hit.replace(/[^\n]/g, " "));
}
const withoutFences = (text: string) => blank(text, /^(```|~~~)[^\n]*\n[\s\S]*?^\1[^\n]*$/gm);
/** outline: what keeps a spec from the outline. Its `##` headings are exactly OUTLINE's, in order.
 * It holds a requirement, each one a `:::requirement` block whose `**R<n>.**` paragraph names its
 * source after `From:`. The checks are not in the spec: they are the `check` blocks of the issue's
 * Verification document. */
function outlineProblems(spec: string): string[] {
  const text = withoutFences(spec);
  const headings = [...text.matchAll(/^##[ \t]+(.+?)[ \t#]*$/gm)].map((hit) => hit[1] ?? "");
  const problems: string[] = [];
  if (headings.join("\n") !== OUTLINE.join("\n"))
    problems.push(
      `headings are ${headings.map((heading) => `## ${heading}`).join(", ") || "none"}`
    );
  const reqs = marked(text);
  const inBlock = requirementBlockLines(text);
  if (reqs.length === 0) problems.push("no **R<n>.** requirement");
  for (const req of reqs) {
    if (!/From:\s*\S/.test(req.body)) problems.push(`${req.mark} names no source (From:)`);
    if (!inBlock.has(req.line)) problems.push(`${req.mark} is in no :::requirement block`);
  }
  return problems;
}

/** A text the `quoted` score compares: `display` is what a reader sees of it, and `compare` the
 * same text with what the score leaves out blanked. */
interface Piece {
  source: string;
  display: string;
  compare: string;
}
/** What `quoted` leaves out of a text: fenced code, code spans, links and images whole, URLs and
 * dispatch:// references, HTML comments, character references, and typed-block lines, whose
 * attributes Dispatch writes (an answered ask's line holds the person's answer). */
function piece(source: string, display: string): Piece {
  let compare = withoutFences(display);
  for (const pattern of [
    /``[^`]*``|`[^`\n]*`/g,
    /!?\[[^\]\n]*\]\([^)\n]*\)/g,
    /<[a-z][a-z0-9+.-]*:[^>\s]*>/gi,
    /\b[a-z][a-z0-9+.-]*:\/\/\S+/gi,
    /<!--[\s\S]*?-->/g,
    /&#?\w+;/g,
    /^:::[\w-]*(\{.*\})?[ \t]*$/gm,
  ])
    compare = blank(compare, pattern);
  return { source, display, compare };
}
interface Word {
  word: string;
  piece: number;
  start: number;
  end: number;
}
/** The words of pieces, with case and punctuation folded: letters and digits, an apostrophe inside
 * a word dropped. A line break is a space like any other. */
function wordsOf(pieces: Piece[]): Word[] {
  return pieces.flatMap((text, index) =>
    [...text.compare.matchAll(/[\p{L}\p{N}]+(?:['’][\p{L}\p{N}]+)*/gu)].map((hit) => ({
      word: hit[0].toLowerCase().replace(/['’]/g, ""),
      piece: index,
      start: hit.index ?? 0,
      end: (hit.index ?? 0) + hit[0].length,
    }))
  );
}
/** The sentences of `display` that hold [from, to), with one sentence on each side. */
function context(display: string, from: number, to: number): string {
  const bounds = [
    0,
    ...[...display.matchAll(/[.!?]+["')\]*_]*\s+|\n+/g)].map(
      (hit) => (hit.index ?? 0) + hit[0].length
    ),
    display.length,
  ];
  const sentences = bounds
    .slice(0, -1)
    .map((start, index) => ({ start, end: bounds[index + 1] ?? display.length }))
    .filter((sentence) => display.slice(sentence.start, sentence.end).trim() !== "");
  const first = Math.max(
    0,
    sentences.findIndex((sentence) => sentence.end > from)
  );
  const last = sentences.findLastIndex((sentence) => sentence.start < to);
  const begin = sentences[Math.max(0, first - 1)]?.start ?? 0;
  const end = sentences[Math.min(sentences.length - 1, last + 1)]?.end ?? display.length;
  return display.slice(begin, end).replace(/\s+/g, " ").trim();
}
/** Where `words` sit in `pieces`: each piece's sentences that hold them, with one on each side. */
function contextOf(pieces: Piece[], words: Word[]): string {
  return [...new Set(words.map((word) => word.piece))]
    .map((index) => {
      const inPiece = words.filter((word) => word.piece === index);
      return context(
        pieces[index]?.display ?? "",
        inPiece[0]?.start ?? 0,
        inPiece.at(-1)?.end ?? 0
      );
    })
    .join(" … ");
}

/** One shared run: the words a text of the run and the person's input both hold, where the text
 * is, and the context of the run in the text and in the input. */
export interface SharedRun {
  source: string;
  run: string;
  words: number;
  text: string;
  input: string;
}
/** The `>` lines fetch-spec-sources.ts writes for an anchored comment's quote: the lines right
 * after a `<!-- comment <time> anchored -->` header, up to its first line that is not a `>` line.
 * The quote is the old spec's text, not the person's. Every other `>` line of messages.md is the
 * person's own: a block quote in a message, one after a comment's quote and body, or one an
 * unanchored comment starts with. */
const ANCHORED_QUOTE = /^(<!-- comment \S+ anchored -->[ \t]*\n)((?:>[^\n]*(?:\n|$))+)/gm;
/** Every entry header of messages.md (fetch-spec-sources.ts's ENTRY_HEADER), which is no input. */
const ENTRY_HEADERS = new RegExp(ENTRY_HEADER.source, "gm");
/** The shared runs the `quoted` score finds. The run's texts are spec.md, each text of
 * agent-texts.json (seeded asks left out) and each file of documents/. The person's input is
 * source/messages.md less its entry headers and each comment's anchored quote (ANCHORED_QUOTE),
 * and the text of each answer in source/asks.json. A shared run is a longest stretch of RUN_WORDS
 * words or more that a text and the input both hold, so a copied paragraph is one run: a stretch
 * inside a longer one, or a repeat of one, is not a run of its own. */
export function sharedRuns(runDir: string): SharedRun[] {
  const seeded = seededIds(runDir);
  const messages = readFileSync(path.join(runDir, "source", "messages.md"), "utf8");
  const quotesBlanked = messages.replace(
    ANCHORED_QUOTE,
    (_, header: string, quote: string) => header + quote.replace(/[^\n]/g, " ")
  );
  const input = [
    piece("messages.md", blank(quotesBlanked, ENTRY_HEADERS)),
    ...json(path.join(runDir, "source", "asks.json"), SourceAsks).flatMap((ask, index) =>
      ask.answer?.text ? [piece(`asks.json#${index}`, ask.answer.text)] : []
    ),
  ];
  const documents = path.join(runDir, "documents");
  const texts = [
    piece("spec.md", readFileSync(path.join(runDir, "spec.md"), "utf8")),
    ...json(path.join(runDir, "agent-texts.json"), AgentTexts)
      .filter((text) => !seeded.has(text.id))
      .map((text) => piece(`agent-texts.json#${text.id}`, text.body)),
    ...(existsSync(documents) ? readdirSync(documents).sort() : []).map((file) =>
      piece(`documents/${file}`, readFileSync(path.join(documents, file), "utf8"))
    ),
  ];
  const held = wordsOf(input);
  const grams = new Map<string, number[]>();
  const gram = (words: Word[], at: number) =>
    words
      .slice(at, at + RUN_WORDS)
      .map((word) => word.word)
      .join(" ");
  for (let at = 0; at + RUN_WORDS <= held.length; at++)
    grams.set(gram(held, at), [...(grams.get(gram(held, at)) ?? []), at]);
  const runs: SharedRun[] = [];
  for (const text of texts) {
    const words = wordsOf([text]);
    const spans: { start: number; end: number; from: number }[] = [];
    for (let at = 0; at + RUN_WORDS <= words.length; at++)
      for (const from of grams.get(gram(words, at)) ?? []) {
        // A stretch that the word before both continues starts earlier.
        if (at > 0 && from > 0 && words[at - 1]?.word === held[from - 1]?.word) continue;
        let length = RUN_WORDS;
        while (
          words[at + length] !== undefined &&
          words[at + length]?.word === held[from + length]?.word
        )
          length++;
        spans.push({ start: at, end: at + length, from });
      }
    const longest = spans.filter(
      (span, index) =>
        !spans.some(
          (other, o) =>
            other.start <= span.start &&
            other.end >= span.end &&
            (other.end - other.start > span.end - span.start || o < index)
        )
    );
    for (const span of longest.sort((a, b) => a.start - b.start)) {
      const own = words.slice(span.start, span.end);
      runs.push({
        source: text.source,
        run: own.map((word) => word.word).join(" "),
        words: own.length,
        text: contextOf([text], own),
        input: contextOf(input, held.slice(span.from, span.from + own.length)),
      });
    }
  }
  return runs;
}

/** What judge.ts gives each judge of a run, and so what each judge's answer must cover:
 * - `requirements`: the `**R<n>.**` marks of the approved version's spec-versions/<v>.md, or none
 *   when the person approved none (then no checkable judge runs);
 * - `asks`: agent-asks.json less the approval request and the seeded asks, oldest first at full
 *   precision (Dispatch writes `…:00.5Z` before `…:00.51Z`, which text order reverses);
 * - `texts`: agent-texts.json less the seeded asks. agent-texts.json holds no post the run
 *   deleted, because the capture (seed.ts's captureSpec) leaves each out (dispatch://INFRA-1523);
 * - `documents`: each file of documents/, as `documents/<file>`;
 * - `checks`: how many spec-only checks count for a spec write, from specWork, the one list of
 *   checks specRow's `checked` reads too: with none, no findings judge runs and findings.json has
 *   no row. */
export interface JudgeInputs {
  requirements: string[];
  asks: z.infer<typeof AgentAsks>;
  texts: z.infer<typeof AgentTexts>;
  documents: string[];
  checks: number;
}
export function judgeInputs(runDir: string, work: SpecWork): JudgeInputs {
  const seeded = seededIds(runDir);
  const { approved, version } = approval(runDir);
  const documents = path.join(runDir, "documents");
  return {
    requirements:
      approved && version !== null
        ? requirements(readFileSync(path.join(runDir, "spec-versions", `${version}.md`), "utf8"))
        : [],
    asks: json(path.join(runDir, "agent-asks.json"), AgentAsks)
      .filter((ask) => ask.kind !== "approval" && !seeded.has(ask.id))
      .toSorted((a, b) =>
        byCreatedAt({ created_at: a.created_at ?? "" }, { created_at: b.created_at ?? "" })
      ),
    texts: json(path.join(runDir, "agent-texts.json"), AgentTexts).filter(
      (text) => !seeded.has(text.id)
    ),
    documents: (existsSync(documents) ? readdirSync(documents).sort() : []).map(
      (file) => `documents/${file}`
    ),
    checks: work.checks.length,
  };
}
/** What a judge file's ids leave out of, repeat from or add to the ids its judge was given:
 * `exact` holds the answer to one row per given id, else each id it names must be one given. */
function covered(what: string, given: string[], named: string[], exact: boolean): string[] {
  const problems: string[] = [];
  if (exact) {
    for (const id of given) {
      const count = named.filter((one) => one === id).length;
      if (count === 0) problems.push(`no row for ${what} ${id}`);
      if (count > 1) problems.push(`two or more rows for ${what} ${id}`);
    }
  }
  const unknown = [...new Set(named.filter((one) => !given.includes(one)))];
  if (unknown.length > 0) problems.push(`rows for ${what} it was not given: ${unknown.join(", ")}`);
  return problems;
}
/** What a judge's answer to `file` (a judge-prompts.ts output) leaves out of, repeats from or adds
 * to what judge.ts gave its judge (judgeInputs); empty when it covers its input exactly. Both
 * judge.ts, after each judge's answer parses, and specRow use it: judge.ts fails the run naming the
 * answer, and specRow reports the file as a rig error.
 * - checkable.json: one row for each requirement of the approved version;
 * - questions.json: one row for each ask given;
 * - numbers.json: one row for each text given;
 * - proposal.json: each `ask` a row names is one given;
 * - preamble.json: each `source` is spec.md, a given document, or agent-texts.json#<given id>;
 * - faults.json: each row's count is the number of its items;
 * - findings.json: each check 1 to the number of checks given has a row or is in `clean`, never
 *   both, and no row or `clean` entry names another number. A file with neither for a check is one
 *   judged before that check was known (judge.ts writes an empty file only when there is no
 *   check), or one whose judge skipped the check, so it is a rig error too. */
export function coverage(file: string, answer: unknown, inputs: JudgeInputs): string[] {
  const asks = inputs.asks.map((ask) => ask.id);
  const texts = inputs.texts.map((text) => text.id);
  switch (file) {
    case "checkable.json":
      return covered(
        "requirement",
        inputs.requirements,
        CheckableOutput.parse(answer).rows.map((row) => row.requirement),
        true
      );
    case "questions.json":
      return covered(
        "ask",
        asks,
        QuestionsOutput.parse(answer).rows.map((row) => row.id),
        true
      );
    case "numbers.json":
      return covered(
        "text",
        texts,
        NumbersOutput.parse(answer).rows.map((row) => row.id),
        true
      );
    case "proposal.json":
      return covered(
        "ask",
        asks,
        ProposalOutput.parse(answer).rows.flatMap((row) => (row.ask ? [row.ask] : [])),
        false
      );
    case "preamble.json":
      return covered(
        "source",
        ["spec.md", ...inputs.documents, ...texts.map((id) => `agent-texts.json#${id}`)],
        PreambleOutput.parse(answer).rows.map((row) => row.source),
        false
      );
    case "faults.json": {
      const faults = FaultsOutput.parse(answer);
      return FAULT_ROWS.flatMap((row) => {
        const items = faults.items.filter((item) => item.row === row).length;
        return items === faults.faults[row]
          ? []
          : [`faults["${row}"] is ${faults.faults[row]}, but ${items} items name that row`];
      });
    }
    case "findings.json": {
      const { rows, clean } = FindingsOutput.parse(answer);
      const given = Array.from({ length: inputs.checks }, (_, index) => String(index + 1));
      const found = new Set(rows.map((row) => String(row.check)));
      const cleanIds = clean.map(String);
      return [
        ...given.flatMap((id) =>
          found.has(id) && cleanIds.includes(id)
            ? [`check ${id} has rows and is in clean`]
            : !found.has(id) && !cleanIds.includes(id)
              ? [`no row for check ${id}, and it is not in clean`]
              : []
        ),
        ...covered("check", given, [...found], false),
        ...covered("clean check", given, cleanIds, false),
        ...(new Set(cleanIds).size < cleanIds.length ? ["a check is in clean twice"] : []),
      ];
    }
    default:
      return [];
  }
}
/** A judge's answer `file` in the run directory, as judge.ts wrote it, parsed by `schema` and held
 * to `covers`, what the answer leaves out of, repeats from or adds to what judge.ts gave its judge
 * (empty when it covers it): a file missing, malformed, or not covering is a rig error that
 * `rig.sh judge` fixes. Every judge file a score reads is read through it (`judged`,
 * `workerReadings`). */
function judgeAnswer<T>(
  runDir: string,
  file: string,
  schema: z.ZodType<T>,
  covers: (answer: T) => string[]
): T {
  const full = path.join(runDir, file);
  if (!existsSync(full)) throw new Error(`${file} is missing: run rig.sh judge ${runDir}`);
  const result = schema.safeParse(JSON.parse(readFileSync(full, "utf8")));
  if (!result.success)
    throw new Error(`${file} is not a judge's answer: ${z.prettifyError(result.error)}`);
  const problems = covers(result.data);
  if (problems.length > 0)
    throw new Error(
      `${file} does not cover what judge.ts gave its judge: ${problems.join("; ")}: run rig.sh judge ${runDir}`
    );
  return result.data;
}
/** A spec run's judge answer in the run directory (`judgeAnswer`), held to what judge.ts gave its
 * judge (coverage). */
function judged<T>(runDir: string, file: string, schema: z.ZodType<T>, inputs: JudgeInputs): T {
  return judgeAnswer(runDir, file, schema, (answer) => coverage(file, answer, inputs));
}
/** Each count of `values` in `kinds`, as `kind:n,…`. */
const tally = (kinds: readonly string[], values: string[]) =>
  kinds.map((kind) => `${kind}:${values.filter((value) => value === kind).length}`).join(",");
/** Lines under a row, each indented past the row's own notes. */
const indented = (text: string, depth = 6) =>
  text
    .split("\n")
    .map((line) => `${" ".repeat(depth)}${line}`)
    .join("\n");

/** spec-writing and spec-rewrite: one row per run, with the ten scores below.
 * The row passes when its ten scores pass; `judged` gives the judges' counts, which pass or fail
 * nothing here: the acceptance run of the skills that replace dispatch-brainstorming holds each
 * count to its own bound.
 *   - asked and grew: the run held the conversation before it wrote the design
 *     (conversationOrder). `asked` is the session's first question to the person being older than
 *     its first spec version, so a run that writes a draft and asks after it fails however short
 *     that draft is; `grew` is no version stating a design (`## Requirements` or `## Design`)
 *     before the person has answered a question of the run's own. A session that wrote no
 *     version fails both.
 *   - outline: outlineProblems finds nothing.
 *   - checked: the transcript holds at least one spec write (writesSpec: a write Dispatch made of
 *     the run's own primary spec, each `dispatch` command of a call apart), a spec-only check
 *     (specWork) counts for every one, the first included, and findings.json has no `neither`
 *     row. A `task` call's check counts for a write when its task call and every task's completed,
 *     non-empty report came after the write before it and before this write, in transcript order
 *     (checkOutcome); a subagent started another way, as from an eval cell, counts from its own
 *     transcript when it started after the write before and reported before this one. A version
 *     Dispatch recorded for the session that no write the transcript shows made is a write too
 *     (specWork), and the row is flagged `writes_seen=N versions=M` with each gap under it. Each
 *     check that counts for no write is printed with why. A check that counts but whose findings
 *     are not judged (SpecWork.unjudged: it pastes a sibling issue's spec) is printed with why, and
 *     the row is flagged `unjudged=N`: a write whose only check is one of these has no answer to
 *     whether the agent acted on what came back.
 *   - refusals: every refused `dispatch` call (refusalBlocks: an `error` line of the CLI's ledger,
 *     or a refusal a call's output shows that the ledger holds no line of: an `eval` cell's, whose
 *     kernel knows no session, or one the CLI made with no session) is followed by a call
 *     the ledger logs as a success, in a later tool call, with no skill file read between them. A
 *     call the agent made before the refusal came back is no fix: it cannot show that the agent
 *     fixed the call from the refusal alone. Each failing refusal is printed with its text.
 *   - plan: the person approved the spec, the issue holds plan.md (capture-spec runs after the
 *     approval), and the transcript read skill://writing-plans.
 *   - routed: the transcript read skill://dispatch-writing-specs before its first spec write (a
 *     version no write it shows made included), and never read REMOVED_SKILL. `ref` is the first
 *     half.
 *   - chat: no call of the interactive `ask` tool (surfaceOf's chat), and every state-<turn>.json
 *     but the last lists an open approval request, or an open ask whose id is not in seeded.json.
 *   - quoted: every run sharedRuns finds has the verdict `names` in shared-runs.json. Each run is
 *     printed with its context in the text and in the input.
 *   - self: spec.md and each text of agent-texts.json hold neither the session id the
 *     transcript's header names nor its first 8 characters.
 *   - judged: faults.json's faults and STE breaks; decisions.json's choices not recorded, or
 *     recorded without a reason, the options or the parts; findings.json's findings and `neither`
 *     rows; checkable.json (`checkable`: the approved version's every requirement has a check);
 *     questions.json's `dominated`, `settled` and kinds; numbers.json's `names_number`;
 *     preamble.json's passages by kind; proposal.json's verdicts. Each row whose id is in
 *     seeded.json is left out. */
/** The `##` headings whose sections hold a design, compared without case or inner spacing. */
const DESIGN_HEADINGS = ["requirements", "design"];

/** Whether this version of a spec states a design, which is what the conversation rule holds back
 *  until the person has answered.
 *
 *  Read from the text's own structure, never from three literal heading strings: a spec written
 *  under `##  Requirements` with two spaces states a design as plainly as one written under
 *  `## Requirements`, and a `## Design` heading with nothing under it states none (both measured
 *  on this scorer by the review of 2026-11-05). So a version states a design when either holds:
 *
 *  - it carries a `**R<n>.**` requirement paragraph, the outline's own design content, wherever
 *    in the document it sits - which also catches a design written out under `## Summary` before
 *    the conventional headings arrive;
 *  - a heading whose title is Requirements or Design has text of its own under it,
 *    read with the heading pattern `outlineProblems` uses, so spacing and a trailing `#` change
 *    nothing.
 *
 *  Fenced code is blanked first, so a heading or a marked paragraph inside an example counts for
 *  neither. */
export function statesDesign(spec: string): boolean {
  const text = withoutFences(spec);
  if (marked(text).some((paragraph) => /^R\d/.test(paragraph.mark))) return true;
  const headings = [...text.matchAll(/^(#{1,6})[ \t]+(.+?)[ \t#]*$/gm)];
  for (const [index, heading] of headings.entries()) {
    const title = (heading[2] ?? "").trim().toLowerCase().replace(/\s+/g, " ");
    if (!DESIGN_HEADINGS.includes(title)) continue;
    const start = (heading.index ?? 0) + heading[0].length;
    const end = headings[index + 1]?.index ?? text.length;
    if (text.slice(start, end).trim() !== "") return true;
  }
  return false;
}

/** Whether the run held the conversation before it wrote the design (`skill://dispatch-writing-specs`,
 *  "The conversation"), read from what Dispatch recorded rather than from the transcript's prose.
 *  Both scores are measured against the first version that states a design (`DESIGN_HEADINGS`),
 *  which is what the rule names - "before the spec states any design" - and not against the
 *  document's first version: creating the document and opening the first question are one act of
 *  an agent's turn, and a measurement two seconds wide would read the order of two writes nobody
 *  made in an order.
 *
 *  - `asked`: the session's first question to the person (agent-asks.json, the seeded asks and
 *    the approval request left out) is older than that version. A run that states a design and
 *    asks after it fails, however short that first draft is.
 *  - `grew`: a person had answered a question of the run's own before that version.
 *
 *  A run whose session wrote no version at all fails both, as a run that wrote a spec with no
 *  question does. A run that stated no design yet passes both: it has written nothing the rule
 *  holds back. */
export function conversationOrder(
  runDir: string,
  own: string
): { asked: boolean; grew: boolean; notes: string[] } {
  const seeded = seededIds(runDir);
  const asks = json(path.join(runDir, "agent-asks.json"), AgentAsks).filter(
    (ask) => ask.kind !== "approval" && !seeded.has(ask.id)
  );
  const versions = sessionVersions(runDir, own);
  const first = versions[0];
  if (first === undefined)
    return {
      asked: false,
      grew: false,
      notes: ["conversation: the session wrote no spec version"],
    };
  // Every comparison below is between instants, never between the strings Dispatch wrote: with a
  // variable-width fraction ".5Z" is before ".51Z" and sorts after it, which the review of
  // 2026-11-05 reproduced both ways round on this function.
  const earliest = (times: (string | null | undefined)[]): bigint | undefined =>
    times
      .filter((at): at is string => typeof at === "string" && at !== "")
      .map((at) => parseInstant(at))
      .filter((at): at is bigint => at !== undefined)
      .reduce<bigint | undefined>(
        (best, at) => (best === undefined || at < best ? at : best),
        undefined
      );
  const firstAsk = earliest(asks.map((ask) => ask.created_at));
  const firstAnswer = earliest(asks.map((ask) => ask.answer?.at));
  const design = versions.find((version) => {
    const file = path.join(runDir, "spec-versions", `${version.version}.md`);
    return existsSync(file) && statesDesign(readFileSync(file, "utf8"));
  });
  // A design whose own time is unreadable decides nothing, as a version that states no design
  // does not: both leave the run passing, since nothing provably came before anything.
  const designAt = design === undefined ? undefined : earliest([design.created_at]);
  const asked = designAt === undefined || (firstAsk !== undefined && firstAsk < designAt);
  const grew = designAt === undefined || (firstAnswer !== undefined && firstAnswer < designAt);
  const notes: string[] = [];
  if (!asked && design !== undefined)
    notes.push(
      `conversation: version ${design.version} (${design.created_at ?? "?"}) states a design, and the run had asked the person nothing by then (${asks.find((ask) => ask.created_at)?.created_at ?? "no question"})`
    );
  if (!grew && design !== undefined)
    notes.push(
      `conversation: version ${design.version} (${design.created_at ?? "?"}) states a design, and the person had answered nothing by then (${asks.find((ask) => ask.answer?.at)?.answer?.at ?? "no answer"})`
    );
  return { asked, grew, notes };
}

function specRow(
  runDir: string,
  run: string,
  scenario: string,
  label: string,
  reach: string[]
): Row {
  const entries = session(runDir);
  const seeded = seededIds(runDir);
  const spec = readFileSync(path.join(runDir, "spec.md"), "utf8");
  const source = readFileSync(path.join(runDir, "source.txt"), "utf8").trim();
  const fixture = json(path.join(runDir, "fixture.json"), SpecFixture);
  const { approved, version, turns } = approval(runDir);
  const all = steps(entries);
  const work = specWork(runDir, reach, fixture);
  const ops = operations(entries);
  // Places in `ops`, so a read and a write of one call keep their order.
  const firstWrite = ops.findIndex(
    (op) => op.kind === "dispatch" && writesSpec(op.call, fixture, op.answer)
  );
  const reads = ops.flatMap((op, place) =>
    op.kind === "read" ? [{ place, index: op.step.index, target: op.target }] : []
  );
  // A version no write the transcript shows made is a spec write at its own place too.
  const firstUnseen = work.unseen[0]?.index ?? Number.POSITIVE_INFINITY;
  const details: string[] = [];

  const outline = outlineProblems(spec);
  details.push(...outline.map((problem) => `outline: ${problem}`));

  const inputs = judgeInputs(runDir, work);
  const findings = judged(runDir, "findings.json", FindingsOutput, inputs);
  const neither = findings.rows.filter((row) => row.verdict === "neither");
  const unchecked = work.writes.filter((write) => !write.checked);
  const checked = work.writes.length > 0 && unchecked.length === 0 && neither.length === 0;
  if (work.writes.length === 0) details.push("checked: the session made no spec write");
  // Dispatch's record of the session's versions against the writes the transcript shows.
  const disagree =
    work.seen !== work.versions || work.unseen.length > 0 || work.unmatched.length > 0;
  if (disagree)
    details.push(
      `versions: Dispatch recorded ${work.versions} versions of the spec by the run's session, and the transcript shows ${work.seen} spec writes`,
      ...work.unseen.map(
        (version) =>
          `  version ${version.version} (${version.at}): no write the transcript shows made it; scored as a spec write, unchecked unless a check counts for it`
      ),
      ...work.unmatched.map((text) => `  no recorded version matches the write: ${text}`)
    );
  if (unchecked.length > 0)
    details.push(
      `unchecked writes: ${unchecked.length} of ${work.writes.length}`,
      ...unchecked.map((write) => indented(write.text, 2))
    );
  for (const row of neither)
    details.push(`finding of check ${row.check} neither fixed nor recorded: ${row.finding}`);
  for (const check of work.rejected)
    details.push(
      `check ${check.tasks} (${check.via}) for spec write ${check.write + 1} does not count: ${check.problem}`
    );
  for (const check of work.unjudged)
    details.push(
      `check ${check.tasks} (${check.via}) for spec write ${check.write + 1} counts, but ${check.problem}`
    );

  const id = ownSession(entries);
  const refused = refusalBlocks(runDir, entries, ops, id);
  details.push(...refused);

  const planned = reads.some((read) => readsSkill(read.target, "writing-plans"));
  const plan = approved && existsSync(path.join(runDir, "plan.md")) && planned;
  const routedRead = reads.some(
    (read) =>
      (firstWrite < 0 || read.place < firstWrite) &&
      read.index < firstUnseen &&
      readsSkill(read.target, "dispatch-writing-specs")
  );
  const removed = reads.filter(
    (read) =>
      read.target.startsWith(`skill://${REMOVED_SKILL}`) ||
      read.target.includes(`/skills/${REMOVED_SKILL}/`)
  );
  const routed = routedRead && removed.length === 0;
  details.push(...removed.map((read) => `routed: read ${read.target}`));

  const chatAsks = all.filter((step) => step.tool === "ask").length;
  const idle = states(runDir)
    .slice(0, -1)
    .filter(
      (state) => state.approval === null && !state.open_asks.some((ask) => !seeded.has(ask.id))
    )
    .map((state) => state.turn);
  const chat = chatAsks === 0 && idle.length === 0;
  if (chatAsks > 0) details.push(`chat: ${chatAsks} calls of the interactive ask tool`);
  if (idle.length > 0) details.push(`turns with nothing open for the person: ${idle.join(", ")}`);

  const shared = sharedRuns(runDir);
  const verdicts = judged(runDir, "shared-runs.json", SharedRunsFile, inputs).rows;
  const key = (row: { source: string; run: string }) => `${row.source}\n${row.run}`;
  if (
    verdicts.length !== shared.length ||
    shared.some((found) => !verdicts.some((row) => key(row) === key(found)))
  )
    throw new Error(
      `shared-runs.json judges ${verdicts.length} runs, and sharedRuns finds ${shared.length} others: run rig.sh judge ${runDir}`
    );
  let copies = 0;
  for (const [index, found] of shared.entries()) {
    const verdict = verdicts.find((row) => key(row) === key(found));
    if (verdict?.verdict === "copies") copies++;
    details.push(
      `shared run ${index + 1} in ${found.source} (${found.words} words, ${verdict?.verdict}: ${verdict?.reason}): ${found.run}`,
      `    text: ${found.text}`,
      `    input: ${found.input}`
    );
  }
  const quoted = copies === 0;

  const selfTexts = [
    { source: "spec.md", body: spec },
    ...json(path.join(runDir, "agent-texts.json"), AgentTexts)
      .filter((text) => !seeded.has(text.id))
      .map((text) => ({ source: `agent-texts.json#${text.id}`, body: text.body })),
  ].filter((text) => text.body.toLowerCase().includes(id.slice(0, 8).toLowerCase()));
  const self = selfTexts.length === 0;
  details.push(...selfTexts.map((text) => `self: ${text.source} names the session`));

  const faults = judged(runDir, "faults.json", FaultsOutput, inputs);
  const faultCount = Object.values(faults.faults).reduce((sum, n) => sum + n, 0);
  const decisions = judged(runDir, "decisions.json", DecisionsOutput, inputs).rows;
  const checkable = judged(runDir, "checkable.json", CheckableFile, inputs);
  if (approved && checkable.version !== version)
    throw new Error(
      `checkable.json judged version ${checkable.version}, and the person approved version ${version}: run rig.sh judge ${runDir}`
    );
  const uncheckable = checkable.rows.filter((row) => row.uncheckable);
  for (const row of uncheckable) details.push(`uncheckable ${row.requirement}: ${row.check}`);
  const asks = judged(runDir, "questions.json", QuestionsOutput, inputs).rows;
  const texts = judged(runDir, "numbers.json", NumbersOutput, inputs).rows;
  const passages = judged(runDir, "preamble.json", PreambleOutput, inputs).rows;
  for (const row of passages) details.push(`preamble ${row.kind} in ${row.source}: ${row.passage}`);
  const proposals = judged(runDir, "proposal.json", ProposalOutput, inputs).rows;

  const conversation = conversationOrder(runDir, id);
  details.push(...conversation.notes);

  const scores = {
    outline: outline.length === 0,
    checked,
    refusals: refused.length === 0,
    plan,
    routed,
    chat,
    quoted,
    self,
    asked: conversation.asked,
    grew: conversation.grew,
  };
  const notes = [
    `source=${source} turns=${turns} approved=${approved}`,
    ...Object.entries(scores).map(([name, ok]) => `${name}=${ok}`),
    `faults=${faultCount} ste_breaks=${faults.ste_breaks}`,
    `decisions=${decisions.length} unrecorded=${decisions.filter((row) => !row.recorded).length}`,
    `incomplete=${decisions.filter((row) => row.recorded && !(row.reason && row.options && row.parts)).length}`,
    `findings=${findings.rows.length} neither=${neither.length}`,
    `checkable=${approved && checkable.rows.length > 0 && uncheckable.length === 0} uncheckable=${uncheckable.length}`,
    `asks=${asks.length} dominated=${asks.filter((row) => row.dominated).length}`,
    `settled=${asks.filter((row) => row.settled).length}`,
    `kinds=${tally(
      QUESTION_KINDS,
      asks.map((row) => row.kind)
    )}`,
    `texts=${texts.length} names_number=${texts.filter((row) => row.names_number).length}`,
    `preamble=${tally(
      ["retold", "dated", "self"],
      passages.map((row) => row.kind)
    )}`,
    `proposal=${tally(
      ["proposed", "asked", "missing"],
      proposals.map((row) => row.verdict)
    )}`,
    `writes=${work.writes.length} shared=${shared.length} copies=${copies}`,
    ...(disagree ? [`writes_seen=${work.seen} versions=${work.versions}`] : []),
    ...(work.unjudged.length > 0 ? [`unjudged=${work.unjudged.length}`] : []),
  ].join(" ");
  return {
    run,
    scenario,
    label,
    pass: Object.values(scores).every(Boolean),
    ref: routedRead,
    notes: [notes, ...details.map((line) => `  ${line}`)].join("\n"),
    source,
    faults: faultCount,
    steBreaks: faults.ste_breaks,
    flagged: disagree || work.unjudged.length > 0,
  };
}

/** The median of numbers: the middle one, or the mean of the two middle ones. */
function median(values: number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 1
    ? (sorted[middle] ?? 0)
    : ((sorted[middle - 1] ?? 0) + (sorted[middle] ?? 0)) / 2;
}

/** Every half's own output, as the file it is in and the text in it: `<run>/out-<role>.txt`,
 * which rig.sh copies for each agent another is booted after, and `<run>/out.txt`, the last
 * agent's, which the launch of that next agent starts again (worker_boot). A run that booted
 * one agent has that file alone. */
export function outputs(runDir: string): [file: string, text: string][] {
  return readdirSync(runDir)
    .filter((file) => /^out(-.+)?\.txt$/.test(file))
    .sort()
    .map((file) => [file, lines(path.join(runDir, file)).join("\n")]);
}
/** The time of the last request a run's daemon stand-in recorded, or undefined for a run that
 * made none: a log holding its opening line alone, or none at all, which is itself a rig error
 * for the scenarios that keep one (RECORDS). */
function lastDaemonRequest(runDir: string): string | undefined {
  return daemonLines(runDir).at(-1)?.at;
}

/** Why the rig cannot score a run, or undefined when it can:
 *   - it did not have a Dispatch of its own: it records none (`dispatch` undefined: no
 *     dispatch.json) and is no spec run (BATCH_DISPATCH), or another run names the same database
 *     in the same Postgres (the container's id or the given server's URL) (`sharing`, each
 *     Dispatch's runs). A `dispatch search` searches every project of its server, so runs that
 *     share one can read each other's issue and asks, and one run's answer can be another's;
 *   - it never finished: out.txt has no `exit=` line;
 *   - a half of it died in its claim boot: it registered with the daemon and could not finish
 *     the boot, and the pane then exited so the daemon would launch it again, which no run of
 *     this rig does (claimBootFailure). The half is gone from the middle of the run, so what
 *     it had done by then answers nothing the scenario asks of it;
 *   - its agent got no model turn: it left no transcript, or one with no assistant message, as when
 *     the profile's gateway key command ran past its budget. The key command's own record of each
 *     call, which scripts/e2e/lib/model-gateway-unserved.sh reads, is a better signal this rig does
 *     not set up;
 *   - one of the agents it boots after the first (ANSWERS' `boots`) started and got no model
 *     turn: that role's session directory holds no transcript, or one with no assistant
 *     message. The half that did run cannot do what the score asks of the other, so the row would
 *     read as a failure of the text under test. A half the run never booted at all, which has no
 *     session directory, is a rig error too, unless the case is one not booting it is the act of
 *     (SKIPS);
 *   - it compared the wrong text: a tool call of the run (its own or a subagent's) read a file under
 *     the other label's checkout, or a skill file (a path to skills/dispatch, skills/dispatch-first,
 *     skills/dispatch-writing-specs or skills/legion-worker) anywhere but `allowed`: its own run
 *     directory (its HOME is there) and its label's profile and checkout. A read is a `read` call
 *     of such an absolute path, a command of READERS in a `bash` call or an eval cell that names
 *     one (here-document bodies left out), or a file an eval cell opens in its own language
 *     (OPENED). An agent reads the whole filesystem, and `legion` on its PATH resolves into the
 *     checkout rig.sh runs from. A read of the rig's own harness under that checkout is none of
 *     this (RIG_DIR): the stand-ins and panes on every label's PATH are the one harness, not a
 *     label's text.
 *   - a daemon answer its agent's own contract refused: the agent could not read its record, so
 *     it could not take the step the rule names, whatever else it did (CONTRACT_REFUSED);
 *   - for a scenario whose score reads what an agent sent (RECORDED), a record of the bus that is
 *     not whole: no `recording` line, so nothing was listening; no `closed` line, so what the
 *     recorder missed is unknown; or a message of one of this run's topics it could not read
 *     (`unreadable`), so what reached that topic is unknown.
 *   - for a scenario whose score reads what the stand-ins recorded (RECORDED), one of the three
 *     records a worker run leaves is missing: calls.jsonl, pushes.log or daemon.log (RECORDS).
 *     Each is kept by the thing that writes it — the gh and bun stand-ins, the remote's
 *     post-receive hook, the daemon stand-in — and the rig creates none of them: it has the gh
 *     stand-in open its own record with one line (`Opened`, rig.sh's worker_boot), the daemon
 *     stand-in opens its log the same way when it starts listening, and rig.sh empties the push
 *     log of the pushes its own fixture made. So an absent
 *     one is a record nothing ever wrote. `lines` reads it as no line at all, which is what no
 *     call, no push and no move look like, and each of those is an answer a passing run gives,
 *     so such a run would be scored on silence nobody made: the audit of 2026-11-10 deleted the
 *     three from a recorded failing run and it scored a pass. A record that is there and holds
 *     no line of the run's own — its opening line alone, or nothing at all where its writer
 *     opens it with none — is the other reading: the run made no call, pushed nothing, moved
 *     nothing. It scores, with the row naming each such record (`emptyRecords`) so a reader can
 *     tell which of the two the score saw;
 *   - a completion the daemon stand-in refused: a 4xx on `handoff/complete` in daemon.log whose
 *     answer the run's world does not allow the completing role (completion-answers.ts's
 *     `ANSWERS`, read through `refusedCompletion`, which health.ts reads too), by how the run's
 *     label's daemon takes a completion where the answer turns on it, read out of the sources of
 *     the commit the run's built-from records, rebuilt out of the repositories the caller names
 *     (daemon-pane.ts's `judgedOnDaemon`), so a run whose rebuilt daemon cannot be read is a rig
 *     error too (one whose daemon cannot be rebuilt is refused, which stops the score): the row
 *     names the role, the status and the code
 *     the line carries; and a completion line, whatever its answer, that names no role or a role
 *     the scenario boots none of, whose completion it was being unknown (`keyOf`);
 * A run whose tool calls only name such a file or checkout (in a check's pasted spec, a plan, a
 * command's text) is scored, and flagged (skillReaches), as is one that names a path outside its
 * sandbox (outsideReaches). A record missing or malformed is a rig error too, thrown by the
 * scenario's function, as is a transcript that names no session (`ownSession`). */
function unscored(
  runDir: string,
  scenario: string,
  caseName: string,
  dispatch: string | undefined,
  label: string,
  labels: readonly (readonly [label: string, checkout: string])[],
  sharing: Map<string, string[]>,
  calls: RunCall[],
  allowed: string[],
  repos: readonly string[]
): string | undefined {
  if (dispatch === undefined) {
    if (!BATCH_DISPATCH[scenario]) return "it records no Dispatch of its own (no dispatch.json)";
  } else {
    const others = (sharing.get(dispatch) ?? []).filter((run) => run !== path.basename(runDir));
    if (others.length > 0) {
      const [postgres, database] = dispatch.split("\t");
      return `it shares Dispatch database ${database} of Postgres ${postgres} with ${others.join(", ")}, so each could read the other's issue and asks`;
    }
  }
  if (!finished(runDir)) return "out.txt has no exit= line: the run never finished";
  // A half that died in its claim boot measured nothing: it registered with the daemon, could
  // not finish the boot — whichever step its line names — and exited so the daemon would launch
  // it again. The rig launches it once more only when that came before its first turn, and reads
  // the run on that second attempt (rig.sh's boot_agent), so a half whose own output holds the
  // line here was refused after a turn or on both attempts. Six of the twelve runs of 2026-11-11
  // exited 1 with `claim boot failed after claims/register registered <role>; exiting so the
  // daemon launches it again: ECONNREFUSED`, and each was scored as a failure of the text under
  // test, which is a scorer reading an agent that was stopped as an agent that chose. The row
  // names the half by its own output file, the sentence it left, and when the run last reached
  // its daemon, which is where a reader of the rig looks next.
  for (const [file, text] of outputs(runDir)) {
    const failed = claimBootFailure(text, runDir);
    if (failed !== undefined)
      return `the agent of <run>/${file} died in its claim boot, so nothing it did is its own behaviour: ${failed} (the run's last recorded daemon request: ${lastDaemonRequest(runDir) ?? "none"})`;
  }
  const entries = session(runDir);
  if (!entries.some((entry) => entry.message?.role === "assistant"))
    return "the agent got no model turn: no transcript, or one with no assistant message";
  // Every agent the scenario boots after the first, by the session directory rig.sh's worker_boot
  // gives it (`<run>/sessions/<role>`): the roles of the scenario's one definition of what it
  // boots, the one rig.sh and health read too (completion-answers.ts's ANSWERS' `boots`), but its
  // first. A scenario that definition does not name boots one agent.
  const halves: readonly (readonly [string, SessionEntry[]])[] = [
    ["", entries],
    ...(ANSWERS[scenario]?.boots.slice(1) ?? []).map(
      (role) => [role, sessionIn(path.join(runDir, "sessions", role))] as const
    ),
  ];
  // A half with no session directory is one the run never booted; a half with a directory and no
  // model turn in it is one that started and measured nothing. The first is the act of a case
  // that asks for it — one completion-answers.ts's SKIPS declares a skip of the role in, as
  // threads-closed's code-defect does the reviewer's once the implementer sent it nothing — and a
  // rig error elsewhere, the second a rig error always.
  const optional = Object.keys(SKIPS[scenario]?.[caseName] ?? {});
  for (const [role, booted] of halves.slice(1)) {
    if (optional.includes(role) && !existsSync(path.join(runDir, "sessions", role))) continue;
    if (!booted.some((entry) => entry.message?.role === "assistant"))
      return `the ${role} half never started: sessions/${role} holds no transcript, or one with no assistant message`;
  }
  // A daemon answer the agent's own plugin refused is a rig error, whatever the run then did: an
  // agent whose `read_record` is refused cannot take the step the rule names, and it spends its
  // turns on the daemon instead of on its work. Each label's agents load the plugin of their own
  // checkout, so a stand-in body one contract describes and another does not breaks one label
  // alone and silently; the measurement that found it is told at `standin-record.ts`'s `settings`
  // line, and this is what says so the next time one is missed.
  for (const [role, booted] of halves) {
    const refused = CONTRACT_REFUSED.exec(transcriptText(booted));
    if (refused !== null)
      return `${role === "" ? "the run's own agent" : `the ${role} half`} had a daemon answer refused by its own checkout's contract: ${refused[1]} ${refused[2]}`;
  }
  // The three records a worker run leaves of what it did (RECORDS). Each is written by the
  // thing that keeps it and by nothing else — rig.sh creates none of them: each stand-in opens
  // its own record with one line of its own, and rig.sh empties only the push log, of the
  // pushes its own fixture made — so an absent one
  // is a record nothing wrote: `lines` would read it as no call, no push and no move, each of
  // which a passing run shows, so it is a rig error naming the file rather than a row scored on
  // a file nobody wrote. A record holding no line of the run's own is the other reading, and
  // the row names it (`emptyRecords`).
  if (RECORDED.includes(scenario)) {
    for (const [file, reads] of Object.entries(RECORDS)) {
      if (!existsSync(path.join(runDir, file)))
        return `<run>/${file} is missing: nothing wrote it, so the run left no record of ${reads} rather than a record of none`;
    }
    // What each agent sent is Envoy's own record of this run (`publishes`), and a record is read
    // only when the recorder said both that it had subscribed and that it was stopped. A recorder
    // that died mid-run — its connection lost, its process killed — leaves the second line out,
    // and every message it missed would otherwise read as a message never sent, which is the
    // parser's old fault in a new place. That is a rig error, as a run whose agent got no model
    // turn is.
    const record = recordState(runDir);
    if (!record.opened)
      return "<run>/published.jsonl has no `recording` line: the publish recorder never subscribed to the bus";
    if (!record.closed)
      return "<run>/published.jsonl has no `closed` line: the publish recorder did not run to the end of the run, so what it missed is unknown";
    if (record.unreadable !== undefined)
      return `<run>/published.jsonl holds a message of ${record.unreadable} the recorder could not read, so what reached that topic is unknown`;
  }
  // A completion the daemon stand-in refused is the rig's fault and never the run's, unless the
  // run's label's daemon refuses that role's completion so in this world
  // (completion-answers.ts's ANSWERS): threads-closed's implementer and its reviewer, whose
  // rounds the world's seeds leave standing on their rows, are refused a late completion 409
  // HANDOFF_NOT_CURRENT_PHASE by a daemon of either shape, which threadsClosed reports; so is a
  // reviewer-text-findings reviewer whose round has moved the issue on a daemon of the older
  // `refused` engine, and on one of this checkout's `recorded` engine after its own earlier
  // completion was answered 200; and a role's repeat of a completion the workflow was handed is
  // refused as the daemon refuses one. Which daemon the label runs is read out of the sources of
  // the commit the run's built-from records, rebuilt out of the caller's repositories, never off
  // the label's name or its checkout as the pod holds it; a run whose sources cannot be rebuilt
  // is refused (`recordedCheckout`'s Refused, which stops the score). Every other refusal says the
  // stand-in and the agent's own plugin disagree about the completion — a body the stand-in's
  // request contract does not take (400 with no code), a completion with no record the tool always
  // attaches (400 HANDOFF_RECORD_REQUIRED), a record on a phase that ends with none (409
  // HANDOFF_RECORD_MISMATCH), a grant it minted and does not know (403) — so what the run did
  // next answers a world the daemon would not have served. Measured on 2026-11-12: every base
  // run of k1 and k2 logged `handoff/complete` 400 and nothing else, the base label sending a
  // `ready` the stand-in's contract refused, and each was scored as base behaviour (review
  // 5887659447, finding 1). A 409 whose line carries no code is a log of an older stand-in,
  // which cannot say which refusal it was, and is a rig error with the rest. So is a completion
  // line that names no role, or a role the scenario boots none of, whatever it was answered:
  // whose completion it was is unknown, and no role is guessed for it (completion-answers.ts's
  // `unkeyed`). health.ts reads the same refusal through the same function, so a run it calls
  // healthy is never this rig error.
  const refusal = judgedOnDaemon(
    runDir,
    (daemon) => {
      const refused = refusedCompletion(daemonLines(runDir), scenario, daemon);
      if (refused !== undefined && "unkeyed" in refused)
        return `the daemon stand-in logged a completion no role can be read from: ${refused.unkeyed} (POST ${refused.line.path} answered ${answerOf(refused.line)}), so whose completion the run's world answered is unknown`;
      if (refused !== undefined)
        return `the daemon stand-in refused a completion: POST ${refused.line.path} answered ${refused.line.status} ${refused.line.code ?? "with no code"} to the ${refused.role}, so the run went on in a world the daemon would not have served`;
      return undefined;
    },
    (why) =>
      `an answer the daemon stand-in logged turns on how the ${label} label's daemon takes a completion, which cannot be read: ${why}`,
    repos
  );
  if (refusal !== undefined) return refusal;
  for (const call of calls) {
    const named: string[] = [];
    if (call.tool === "read") named.push(strings(field(call.args, "path")).join(""));
    if (call.tool === "bash" || call.tool === "eval") {
      const source = strings(call.tool === "bash" ? field(call.args, "command") : call.args).join(
        "\n"
      );
      for (const hit of withoutBodies(source).code.matchAll(SHELL_COMMAND))
        if (READERS[path.posix.basename(shellWord(hit[1] ?? ""))] === true)
          named.push(...[...(hit[2] ?? "").matchAll(ABSOLUTE_PATH)].map(([found]) => found));
      if (call.tool === "eval")
        named.push(...[...source.matchAll(OPENED)].map((hit) => hit[2] ?? ""));
    }
    for (const target of named) {
      if (!target.startsWith("/")) continue;
      const read = path.posix.normalize(target);
      // The rig's own harness, which every label's run reaches through its PATH (RIG_DIR).
      if (RIG_PATH.test(read)) continue;
      for (const [other, checkout] of labels)
        if (other !== label && (read === checkout || read.startsWith(`${checkout}/`)))
          return `a ${call.tool} call reads ${other}'s checkout: ${read}`;
      if (new RegExp(SKILL_NAMED.source).test(read) && !allowed.some((dir) => read.startsWith(dir)))
        return `a ${call.tool} call reads a skill outside ${label}'s own: ${read}`;
    }
  }
  return undefined;
}
/** Commands that print or copy a file they name: the ones a `bash` call reads a file with, named
 * bare or by a path to them (`/bin/cat`). */
const READERS: Record<string, true> = Object.fromEntries(
  "cat head tail sed awk less more bat nl view grep rg cp diff"
    .split(" ")
    .map((word) => [word, true])
);
/** A file an eval cell opens in its own language: Python's `open(…)` or `Path(…)`, Bun's
 * `Bun.file(…)`, given a literal path. */
const OPENED = /\b(?:open|Path|Bun\.file)\(\s*[rbf]?(["'`])([^"'`\n]+)\1/g;

/** Where a run's tools may name an absolute path without reaching outside its sandbox, besides the
 * work directory and the label's checkout. */
const SYSTEM_DIRS = [
  "/usr",
  "/bin",
  "/sbin",
  "/lib",
  "/lib64",
  "/etc",
  "/tmp",
  "/proc",
  "/sys",
  "/dev",
  "/var",
  "/opt",
  "/run",
];
/** An absolute path in text: a `/` that starts no URL, ref (`dispatch://…/spec`) or relative path. */
export const ABSOLUTE_PATH = /(?<![\w:/.~-])\/[A-Za-z0-9._-]+(?:\/[A-Za-z0-9._@+-]+)*/g;
/** Argument fields that carry the text a write puts in a file: a path inside that text is no access. */
const FILE_CONTENT = new Set([
  "content",
  "input",
  "ops",
  "body",
  "text",
  "new_string",
  "old_string",
]);
function argumentStrings(value: unknown): string[] {
  if (typeof value === "string") return [value];
  if (Array.isArray(value)) return value.flatMap(argumentStrings);
  if (typeof value !== "object" || value === null) return [];
  return Object.entries(value).flatMap(([key, field]) =>
    FILE_CONTENT.has(key) ? [] : argumentStrings(field)
  );
}
/** Where a path a run named sits in a tool call: a command or path the agent ran (`ran`), text it
 * wrote into a file (`wrote`: FILE_CONTENT), or a subagent's assignment (`assigned`: a `task`
 * call). Only `ran` comes close to a reach; the other two are prose. */
type ReachKind = "ran" | "wrote" | "assigned";
interface Reach {
  kind: ReachKind;
  tool: string;
  path: string;
  context: string;
}
/** One tool call of the run, its own or a subagent's (every transcript under sessions/): its tool,
 * its arguments, and its text by where it sits (ReachKind). */
interface RunCall {
  tool: string;
  args: unknown;
  texts: [ReachKind, string][];
}
/** Every transcript of the run: each agent's and each subagent's, at any depth under sessions/.
 * health.ts reads the run's tool calls and their results through this too. */
export function transcriptFiles(runDir: string): string[] {
  const under = (dir: string): string[] =>
    existsSync(dir)
      ? readdirSync(dir, { withFileTypes: true }).flatMap((entry) =>
          entry.isDirectory()
            ? under(path.join(dir, entry.name))
            : entry.name.endsWith(".jsonl")
              ? [path.join(dir, entry.name)]
              : []
        )
      : [];
  return under(path.join(runDir, "sessions"));
}
function runCalls(runDir: string): RunCall[] {
  const contentStrings = (value: unknown): string[] =>
    Array.isArray(value)
      ? value.flatMap(contentStrings)
      : typeof value === "object" && value !== null
        ? Object.entries(value).flatMap(([key, field]) =>
            FILE_CONTENT.has(key) ? strings(field) : contentStrings(field)
          )
        : [];
  const calls: RunCall[] = [];
  for (const file of transcriptFiles(runDir))
    for (const entry of transcriptEntries(file)) {
      if (entry.message?.role !== "assistant") continue;
      for (const part of entry.message.content ?? []) {
        if (part.type !== "toolCall") continue;
        const tool = part.name ?? "tool";
        calls.push({
          tool,
          args: part.arguments,
          texts:
            tool === "task"
              ? [["assigned", strings(part.arguments).join("\n")]]
              : [
                  ["ran", argumentStrings(part.arguments).join("\n")],
                  ["wrote", contentStrings(part.arguments).join("\n")],
                ],
        });
      }
    }
  return calls;
}
/** A hit at `at` in `text`, with the text around it on one line. */
function reachAt(kind: ReachKind, tool: string, target: string, text: string, at: number): Reach {
  const context = text.slice(Math.max(0, at - 80), at + target.length + 60);
  return { kind, tool, path: target, context: context.replace(/\s+/g, " ") };
}
/** Every absolute path the run's tool calls name outside `allowed` and SYSTEM_DIRS, whose top-level
 * directory exists on this machine (so a route such as `/v1/messages` is no path), normalized
 * (`/../tmp` is /tmp), with the call's text around it. Nothing isolates an agent from the
 * operator's files (rig.sh runs it with no filesystem sandbox), so such a run may have drawn on
 * them; but tool-call arguments carry the agent's prose as well as its reaches, and no rule over
 * them can tell the two apart in every case. So a run is flagged, never unscored, and the flag
 * prints enough to judge each hit. A path a call reaches without naming it, as a glob from `/`
 * does, is not caught. */
function outsideReaches(calls: RunCall[], allowed: string[]): Reach[] {
  const inside = (target: string) =>
    [...allowed, ...SYSTEM_DIRS].some((dir) => target === dir || target.startsWith(`${dir}/`));
  return calls.flatMap((call) =>
    call.texts.flatMap(([kind, text]) =>
      [...text.matchAll(ABSOLUTE_PATH)].flatMap((hit) => {
        const target = path.posix.normalize(hit[0]);
        const top = `/${target.split("/")[1] ?? ""}`;
        if (top === "/" || !existsSync(top) || inside(target)) return [];
        return [reachAt(kind, call.tool, target, text, hit.index ?? 0)];
      })
    )
  );
}
/** A path to one of the Dispatch and worker skills whose versions the labels compare. */
const SKILL_NAMED =
  /[^\s"'`=:;|&<>()]*skills\/(?:dispatch-first|dispatch-writing-specs|dispatch|legion-worker)\b/g;
/** The rig's own directory in a checkout: rig.sh, the stand-ins it puts first on every label's
 * PATH (gh-standin.ts) and the panes it writes (worker-pane.ts, daemon-standin.ts). That
 * directory, and a file under it, is no reach into another label's text, whichever label the run
 * is: the rig runs every label out of one checkout's harness, so the `gh` a run is given is a
 * script naming that checkout's gh-standin.ts, and an agent that reads the `gh` on its own PATH
 * reads a file under whichever label's checkout the rig was run from. One run of every batch
 * reaches that harness whatever its label, so refusing the read would refuse the ablate arm and
 * leave the comparison without one. What it is not is a label's own text: the harness holds no
 * skill and no role prompt, so it is no part of what the labels compare, and a pull request that
 * changes the harness — this one does — changes it for every label at once, since the rig runs
 * them all out of the one checkout it was started from. `outsideReaches` still records the
 * reach on the row. The harness alone is exempt: another label's skills/ and the rest of its
 * source are read as they were. */
const RIG_DIR = "/packages/pi-legion/scripts/skill-scenarios";
/** RIG_DIR, or a path under it, in a path or in a command's text: the name ends there or a `/`
 * follows, so a directory whose name merely begins with it is no hit. RIG_UNDER_CHECKOUT is the
 * same from a checkout's own end. */
const RIG_PATH = new RegExp(`${escaped(RIG_DIR)}(?![\\w.-])`);
const RIG_UNDER_CHECKOUT = new RegExp(`^${RIG_PATH.source}`);
/** Every name in the run's tool calls of another label's checkout, or of one of those skills (any
 * path to it, a relative one too) outside `allowed`, the label's own. A spec about the Dispatch
 * skills names them in every check of it, and a plan names the files it changes, so a name is
 * flagged, never unscored; a read of one is a rig error (unscored). A name of the rig's own
 * harness under that checkout is neither (RIG_DIR). */
function skillReaches(
  calls: RunCall[],
  allowed: string[],
  label: string,
  labels: readonly (readonly [label: string, checkout: string])[]
): Reach[] {
  return calls.flatMap((call) =>
    call.texts.flatMap(([kind, text]) => [
      ...[...labels]
        .filter(([other]) => other !== label)
        .flatMap(([, checkout]) =>
          [...text.matchAll(new RegExp(escaped(checkout), "g"))]
            .filter(
              (hit) => !RIG_UNDER_CHECKOUT.test(text.slice((hit.index ?? 0) + checkout.length))
            )
            .map((hit) => reachAt(kind, call.tool, checkout, text, hit.index ?? 0))
        ),
      ...[...text.matchAll(SKILL_NAMED)]
        .filter(([named]) => !allowed.some((dir) => named.startsWith(dir)))
        .map((hit) => reachAt(kind, call.tool, hit[0], text, hit.index ?? 0)),
    ])
  );
}
/** A flag for a run's reaches: a count of each kind on the row's first line (` <name>=ran:N,…`),
 * then up to three hits of each kind, each with its call's text, and what the flag does not
 * establish (`note`). */
function flagOf(name: string, reaches: Reach[], note: string): { inline: string; lines: string[] } {
  const kinds: ReachKind[] = ["ran", "wrote", "assigned"];
  const count = (kind: ReachKind) => reaches.filter((reach) => reach.kind === kind).length;
  return {
    inline: ` ${name}=${kinds.map((kind) => `${kind}:${count(kind)}`).join(",")}`,
    lines: [
      ...kinds.flatMap((kind) =>
        reaches
          .filter((reach) => reach.kind === kind)
          .slice(0, 3)
          .map(
            (reach) =>
              `${name} (${kind}): a ${reach.tool} call names ${reach.path}: …${reach.context}…`
          )
      ),
      `${name}: ${note}`,
    ],
  };
}

/** The directory a run had when it ran: the one its record names (run-names.ts's `ran`). A run
 * recorded before the names left the world has no record: rig.sh started every agent in
 * `<run dir>/cwd`, and its session's header records that cwd, so a run scored from a copy
 * (another work directory) still reads its own paths as its own. A header whose cwd is not
 * `…/runs/<this run's name>/cwd`, or no header, gives `dir` itself. */
function ranAt(dir: string, entries: SessionEntry[], recorded: string | undefined): string {
  if (recorded !== undefined) return recorded;
  const cwd = entries.find((entry) => entry.type === "session")?.cwd;
  return typeof cwd === "string" && cwd.endsWith(`/runs/${path.basename(dir)}/cwd`)
    ? path.dirname(cwd)
    : dir;
}
/** The key a run whose record cannot be read is counted under: it is of no arm, so the arm it
 * was run for holds one run fewer. */
const NO_ARM = "?\t?";
/** An arm as `--arm` names it: the score's key of a scenario (with its case) and a label, `:` in
 * place of the tab (`tester-proof:head`, `askgate:facts:on`). A key holds one tab (the arm counts'
 * keys, built in `scoreRuns`, and `NO_ARM`); `replaceAll` says so whatever it holds. */
const armOf = (key: string) => key.replaceAll("\t", ":");
/** Ends the score: each of `lines` on stderr after `score: `, no row and no count, and exit 2. */
function stop(lines: readonly string[]): never {
  for (const line of lines) console.error(`score: ${line}`);
  process.exit(2);
}
function scoreRuns(
  runsDir: string,
  only: string | undefined,
  repos: readonly string[],
  arms: ReadonlyMap<string, number>
) {
  const rows: Row[] = [];
  const errors: { run: string; key: string; reason: string }[] = [];
  // The runs no score is given on, each with why: an input the run did not record, or one the
  // caller's repositories cannot rebuild (run-records.ts's Refused). Any one stops the score.
  const refused: string[] = [];
  // Every run of the work directory, by its name: each in `runsDir`, and, for a work directory's
  // own runs/, each archive_run moved to its archive/runs/ once it ended (`runsOf`), which
  // refuses an entry there that is neither a run nor its record.
  let runs: Map<string, string>;
  try {
    runs = runsOf(runsDir);
  } catch (error) {
    stop([error instanceof Error ? error.message : String(error)]);
  }
  // Every run's PR head, to tell another run's line from a wrong one (`testerProof`, which is
  // the one reader). A world.json that does not parse is its own run's rig error, below.
  const heads = [...runs.values()].flatMap((dir) => {
    try {
      return [worldIn(dir).head];
    } catch {
      return [];
    }
  });
  // Every run's own Dispatch, keyed by its Postgres and database, with the runs that
  // name it. A run that records none is scored or not by unscored, below; a dispatch.json that does
  // not parse is its own run's rig error, below.
  const sharing = new Map<string, string[]>();
  for (const [run, dir] of runs) {
    let dispatch: string | undefined;
    try {
      dispatch = dispatchOf(dir);
    } catch {
      continue;
    }
    if (dispatch !== undefined) sharing.set(dispatch, [...(sharing.get(dispatch) ?? []), run]);
  }
  // Every checkout a spec run's agent can reach, as the runs recorded them, so that a check
  // naming one is no spec-only check.
  const reach = checkouts([...runs.values()]);
  // Each scenario's scorer, keyed by the scenario its runs' records name (for a run recorded
  // before the names left the world, the name its directory starts with). A cased scenario's
  // scorer takes the case the record names, or both that directory and its case.txt name (CASED,
  // runCase); a scenario that takes none is handed "".
  const scorers: Record<string, (dir: string, run: string, label: string, name: string) => Row> = {
    "ask-on-message": askOnMessage,
    "measure-before-ask": measureBeforeAsk,
    "tester-proof": (dir, run, label) => testerProof(dir, run, label, heads),
    "reviewer-text-findings": reviewerTextFindings,
    "threads-closed": threadsClosed,
    brainstorm: brainstormSurface,
    askgate: askgateReview,
    "spec-writing": (dir, run, label) => specRow(dir, run, "spec-writing", label, reach),
    "spec-rewrite": (dir, run, label) => specRow(dir, run, "spec-rewrite", label, reach),
  };
  const name = new RegExp(`^(${Object.keys(scorers).join("|")})-(.+)-(\\d+)$`);
  const named = [...runs]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .flatMap(([run, dir]) => {
      // The run's record, beside its directory; none for a run recorded before the names left
      // the world, whose directory's name is its record.
      const identity =
        existsSync(recordFile(dir)) || RUN_ID.test(run) ? runIdentity(dir) : undefined;
      if (typeof identity === "string") {
        if (!only) errors.push({ run, key: NO_ARM, reason: identity });
        return [];
      }
      const match = identity === undefined ? name.exec(run) : null;
      const scenario = identity?.scenario ?? match?.[1] ?? "";
      const score = scorers[scenario];
      if ((identity === undefined && !match) || !score) return [];
      // undefined for a scenario that takes no case, null for a cased scenario's directory that
      // names none.
      const cased = CASED[scenario];
      const casedRun =
        identity !== undefined || cased === undefined ? undefined : cased.run.exec(run);
      const [, dirCase = identity?.case ?? "", label = identity?.label ?? match?.[2] ?? ""] =
        casedRun ?? [];
      // The checkout the run's built-from names (rig.sh's cmd_profile writes it, cmd_run copies
      // it into the run), none for a run that records none.
      let checkout: string | undefined;
      try {
        checkout = labelCheckout(dir);
      } catch {
        checkout = undefined;
      }
      return [{ run, dir, identity, scenario, score, cased, casedRun, dirCase, label, checkout }];
    });
  // Each label's checkouts, as the built-from of each of its runs names them: never the work
  // directory's profiles as they stand now, which a copy read elsewhere does not have.
  const labels = [
    ...new Map(
      named.flatMap(({ label, checkout }) =>
        checkout === undefined ? [] : [[`${label}\0${checkout}`, [label, checkout] as const]]
      )
    ).values(),
  ];
  for (const {
    run,
    dir,
    identity,
    scenario,
    score,
    cased,
    casedRun,
    dirCase,
    label,
    checkout,
  } of named) {
    if (only && scenario !== only) continue;
    const key = `${cased !== undefined && casedRun !== null ? `${scenario}:${dirCase}` : scenario}\t${label}`;
    let reason: string | undefined;
    try {
      if (casedRun === null) throw new Error(`the directory names no case: ${cased?.shape ?? ""}`);
      const dispatch = dispatchOf(dir);
      const calls = runCalls(dir);
      // The paths the run had when it ran, which its transcripts name, never those of a copy
      // scored elsewhere (ranAt).
      const ran = ranAt(dir, session(dir), identity?.ran);
      const work = workOfRuns(path.dirname(ran));
      // Where the label's profile was installed, as the run recorded it (run-names.ts's
      // `labelInstall`, its namespace.txt).
      const profile = labelInstall(dir, label, work);
      // The run's own skills: its HOME (in its directory), its label's profile and checkout.
      const own = [ran, profile, checkout ?? profile].map((place) => `${place}/`);
      // The case the run was built for, which is what decides the halves it boots: its record's,
      // or the one its directory names. A run recorded before the names left the world also kept
      // its case in case.txt, which is read below, where it is held to the name (runCase), and a
      // run whose two records disagree is a rig error of its own.
      reason = unscored(
        dir,
        scenario,
        dirCase,
        dispatch,
        label,
        labels,
        sharing,
        calls,
        own,
        repos
      );
      if (reason === undefined) {
        // A scenario that takes no case reads no case.txt and is handed "".
        const scored = score(
          dir,
          run,
          label,
          identity !== undefined || cased === undefined ? dirCase : runCase(dir, cased, dirCase)
        );
        const outside = outsideReaches(calls, [work, checkout ?? work]);
        const skills = skillReaches(calls, own, label, labels);
        const flags = [
          ...(outside.length === 0
            ? []
            : [
                flagOf(
                  "outside",
                  outside,
                  "the run named a path outside its work directory, its label's checkout and the system locations. That does not show it read the path, nor that its result is wrong."
                ),
              ]),
          ...(skills.length === 0
            ? []
            : [
                flagOf(
                  "skills",
                  skills,
                  "the run named another label's checkout, or a skill file outside its label's own, without reading it (a read is a rig error). That does not show it drew on the file."
                ),
              ]),
        ];
        // A record holding no line of the run's own is a reading of the run and not a gap in
        // the rig (a missing one never reaches here: `unscored` makes it a rig error), so the
        // row says which it saw rather than leaving a reader to guess why a count is zero.
        const empty = RECORDED.includes(scenario) ? emptyRecords(dir) : [];
        const [first = "", ...rest] = scored.notes.split("\n");
        const row = {
          ...scored,
          flagged: scored.flagged === true || flags.length > 0,
          notes: [
            `${first}${flags.map((flag) => flag.inline).join("")}`,
            ...rest,
            ...(empty.length === 0
              ? []
              : [
                  `  empty records: ${empty.join(", ")} — there, and holding no line of the run's own`,
                ]),
            ...flags.flatMap((flag) => flag.lines.map((line) => `  ${line}`)),
          ].join("\n"),
        };
        rows.push(
          dispatch === undefined ? { ...row, notes: `${row.notes}\n  ${BATCH_DISPATCH_NOTE}` } : row
        );
      }
    } catch (error) {
      if (error instanceof Refused) {
        refused.push(error.message);
        continue;
      }
      reason = error instanceof Error ? error.message : String(error);
    }
    if (reason !== undefined) errors.push({ run, key, reason });
  }
  // A run refused is neither scored nor a rig error: either would move its arm's counts, or have a
  // batch runner replace it, on a reading that never happened (the architect's condition of
  // 2026-11-18T18:56Z). So the score stops, naming each and the input it lacks, prints no row and
  // no count, and exits 2.
  if (refused.length > 0)
    stop([
      ...refused.map((why) => `no score on ${why}`),
      `${refused.length} runs refused, so nothing is scored`,
    ]);
  // A score that read no run is no score: an empty or mistyped work directory, or a scenario no
  // run is of, would otherwise read as a clean score of nothing.
  if (rows.length === 0 && errors.length === 0)
    stop([
      `no run${only === undefined ? "" : ` of ${only}`} under ${runsDirsOf(runsDir).join(" or ")}`,
    ]);
  // Each arm holds the runs the caller made of it, no more and no fewer (the architect's ruling
  // of 2026-11-18): every run score read under its key, scored or a rig error, since what is
  // held is which runs are there, not how they came out. An arm no `--arm` names, an `--arm`
  // whose arm holds no run, and an arm holding another number stop the score, as a refusal does.
  const found = new Map<string, number>();
  for (const key of [
    ...rows.map((row) => `${row.scenario}\t${row.label}`),
    ...errors.map((e) => e.key),
  ])
    if (key !== NO_ARM) found.set(key, (found.get(key) ?? 0) + 1);
  const unheld = [
    ...[...found]
      .filter(([key]) => !arms.has(key))
      .map(([key, count]) => `arm ${armOf(key)}: found ${count} runs, and no --arm names it`),
    ...[...arms]
      .filter(([key, n]) => (found.get(key) ?? 0) !== n)
      .map(
        ([key, n]) =>
          `arm ${armOf(key)}: found ${found.get(key) ?? 0} runs, expected ${n} (--arm ${armOf(key)}=${n})`
      ),
  ];
  if (unheld.length > 0)
    stop([...unheld, `${unheld.length} arms do not hold the runs expected, so nothing is scored`]);
  for (const row of rows) console.log(`${row.run}\tpass=${row.pass}\tref=${row.ref}\t${row.notes}`);
  for (const error of errors) console.log(`${error.run}\trig error: ${error.reason}`);
  console.log("\nscenario\tlabel\tpass\tpass+ref\trig errors\tflagged\texpected\tfound");
  const keys = [
    ...new Set([
      ...rows.map((row) => `${row.scenario}\t${row.label}`),
      ...errors.map((e) => e.key),
    ]),
  ];
  for (const key of keys) {
    const group = rows.filter((row) => `${row.scenario}\t${row.label}` === key);
    const passed = group.filter((row) => row.pass);
    const withRef = passed.filter((row) => row.ref);
    const failed = errors.filter((error) => error.key === key).length;
    const flagged = group.filter((row) => row.flagged).length;
    console.log(
      `${key}\t${passed.length}/${group.length}\t${withRef.length}/${group.length}\t${failed}\t${flagged}\t${arms.get(key) ?? "-"}\t${group.length + failed}`
    );
  }
  // For each source and label, the median fault count of its spec runs (faults.json): the count
  // that compares two versions of the skills on the same source.
  const specKeys = [
    ...new Set(
      rows.flatMap((row) =>
        row.source === undefined ? [] : [`${row.source}\t${row.scenario}\t${row.label}`]
      )
    ),
  ];
  if (specKeys.length > 0) {
    console.log("\nsource\tscenario\tlabel\truns\tmedian faults\tmedian ste breaks\tflagged");
    for (const key of specKeys) {
      const group = rows.filter((row) => `${row.source}\t${row.scenario}\t${row.label}` === key);
      console.log(
        `${key}\t${group.length}\t${median(group.map((row) => row.faults ?? 0))}\t${median(group.map((row) => row.steBreaks ?? 0))}\t${group.filter((row) => row.flagged).length}`
      );
    }
  }
  // For each arm whose rows carry `field`, how many hold it, in a table headed `column`.
  const armCounts = (
    column: string,
    field: (row: (typeof rows)[number]) => boolean | undefined
  ) => {
    const counted = keys.filter((key) =>
      rows.some((row) => `${row.scenario}\t${row.label}` === key && field(row) !== undefined)
    );
    if (counted.length === 0) return;
    console.log(`\nscenario\tlabel\t${column}`);
    for (const key of counted) {
      const group = rows.filter((row) => `${row.scenario}\t${row.label}` === key);
      console.log(`${key}\t${group.filter((row) => field(row)).length}/${group.length}`);
    }
  };
  // Whether the verdict was `REQUEST_CHANGES` (reviewer-text-findings): k1's and k3's base arms
  // are read off this count, since the score is label-blind.
  armCounts("requested", (row) => row.requested);
  // Whether the implementer recorded the pass as holding with no run (threads-closed's accept):
  // k3's second control is read off the head arm's count.
  armCounts("held", (row) => row.heldNoRun);
  console.log(
    [
      "",
      "pass       tester-proof: the four conditions on testerProof. ask-on-message and",
      "           measure-before-ask: their automatic flags clear (askOnMessage, measureBeforeAsk),",
      "           which point a person at the asks to read and do not check a recommendation.",
      "           brainstorm: the design went to the issue's spec and nothing leaked the project",
      "           lookup (brainstormSurface); a person still reads notes for unagreed points.",
      "           spec-writing and spec-rewrite: the ten scores of specRow pass (outline,",
      "           checked, refusals, plan, routed, chat, quoted, self, asked, grew); the judges'",
      "           counts follow",
      "           them, and the source table gives each source and label's median fault count",
      "           askgate: the rule for the run's label and case (askgateReview)",
      "           reviewer-text-findings: each term the case asks for, reported in the row's",
      "           terms= (reviewerTextFindings: k1's text-only a-f, k3's last-push a-d, defect)",
      "           threads-closed: each term the case asks for, reported in the row's terms=",
      "           (threadsClosed: k2's accept a-e and correct a-f, k3's crossing a-c, code-defect)",
      "pass+ref   passed, and the agent read the file the rule is in (askgate: never counted)",
      "requested  reviewer-text-findings: the runs whose verdict is REQUEST_CHANGES, which k1's",
      "           and k3's base arms are read off",
      "held       threads-closed's accept: the runs that recorded the pass as holding at the",
      "           reviewer's commit with no run of the check there, which k3's second control is",
      "           read off",
      "rig errors runs the rig could not score (unscored), left out of both counts",
      "flagged    counted runs whose row carries a flag (outside=, skills=, writes_seen=,",
      "           unjudged=): each is",
      "           in the counts and medians, and its notes say what to read before trusting it",
      "ran=false  a tester-proof run the bun stand-in saw no greet.ts run in: read its transcript",
      "           before believing it, since the stand-in sees only a bun reached through PATH",
      "expected   the runs the arm's --arm names (- for runs of no arm, whose record is unread)",
      "found      the runs read under the arm, scored and rig errors, which equals expected",
    ].join("\n")
  );
}

// judge.ts imports sharedRuns from this file: the command line runs only when bun runs score.ts.
if (import.meta.main) {
  const [command, ...rest] = Bun.argv.slice(2);
  if (command === "live-read" && rest.length === 3) {
    liveRead(rest[0] ?? "", rest[1] ?? "", rest[2] ?? "");
  } else if (command === "runs" && rest.length >= 1) {
    // The git directories each run's label sources are rebuilt from by the commit its built-from
    // records (daemon-pane.ts's `recordedCheckout`), every `--repo <git dir>`, and the runs each
    // arm holds, every `--arm <scenario>[:<case>]:<label>=<n>`, in any order after the runs
    // directory; then the scenario, if one is named.
    const [runsDir = "", ...after] = rest;
    const repos: string[] = [];
    const arms = new Map<string, number>();
    while ((after[0] === "--repo" || after[0] === "--arm") && after[1] !== undefined) {
      const [flag, value = ""] = after.splice(0, 2);
      if (flag === "--repo") {
        repos.push(path.resolve(value));
        continue;
      }
      const [, arm, label, n = ""] =
        /^([a-z][a-z-]*(?::[a-z0-9-]+)?):([a-z0-9][a-z0-9-]*)=(.*)$/.exec(value) ?? [];
      if (arm === undefined || label === undefined)
        stop([`--arm ${value} names no arm: <scenario>[:<case>]:<label>=<n>`]);
      if (!/^[1-9]\d*$/.test(n)) stop([`--arm ${value}: ${n} is no whole number of 1 or more`]);
      if (arms.has(`${arm}\t${label}`))
        stop([`--arm ${value}: arm ${arm}:${label} is named twice`]);
      arms.set(`${arm}\t${label}`, Number(n));
    }
    scoreRuns(runsDir, after[0], repos, arms);
  } else if (command === "published" && rest.length === 2) {
    // The harness's own reader of a publish, so the rig and the score read one: rig.sh's
    // reviewer_wake asks for every message a role's topic was sent, in Envoy's own order, and
    // hands each to the next agent as a delivery of its own, and the score asks the same code
    // whether the message the rule names was among them. What two readers of that one fact cost
    // is in record-publishes.ts's header.
    // Every throw is caught here, not the missing session alone, and that is deliberate: the
    // harness must not die of a reader's limit first. `rig.sh`'s reviewer_wake runs under
    // `set -e`, so a throw would end the run with "the reviewer half never started" and hide
    // whatever the real reason was — a transcript with no session header, a `published.jsonl`
    // line this cannot parse, any of them. Printing an empty list leaves the next half with no
    // wake, which is what a run that published nothing leaves, and the reason goes to stderr.
    // Nothing is lost by catching widely: the score reads the same files afterwards and throws
    // there too, so the run is a rig error naming the real reason either way.
    const runDir = rest[0] ?? "";
    const role = ROLES.find((named) => named === rest[1]);
    try {
      if (role === undefined) throw new Error(`no role topic is named ${rest[1] ?? ""}`);
      console.log(JSON.stringify(publishesTo(runDir, role)));
    } catch (error) {
      console.error(
        `score.ts published: ${error instanceof Error ? error.message : String(error)}`
      );
      console.log("[]");
    }
  } else {
    console.error(
      "usage: score.ts live-read <run dir> <skills dir> <omp logs dir> | runs <runs dir> [--repo <git dir> | --arm <scenario>[:<case>]:<label>=<n>]... [<scenario>] | published <run dir> <role>"
    );
    process.exit(2);
  }
}
