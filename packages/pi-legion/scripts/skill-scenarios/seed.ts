// Seeds a skill-scenario run's own scratch Dispatch through its API, and reads back what the run
// left there. DISPATCH_E2E_PORT names the server, which e2e/api.ts addresses. e2e/api.ts addresses
// PLAYWRIGHT_BASE_URL instead, with E2E_AGENT_TOKEN as its bearer, whenever it is set, so seed.ts
// refuses to run while either is set. Each mode is also an exported function, which seed.test.ts
// calls against a stubbed fetch.
//
//   seed.ts project                        project LWEVAL, which each run's own issue is filed in;
//                                          the world of every scenario but brainstorm
//   seed.ts tester-proof <file>            one run's own issue, the greet() issue the tester's world
//                                          implements; writes {issue} to <file>
//   seed.ts implement-handoff <file> <body file> <commit>
//                                          records <body file>, one JSON object, as the implement
//                                          handoff of the issue <file> names, under the session
//                                          IMPLEMENTER (never the run's), records its passing
//                                          result on check k1 at <commit>, and writes {issue,
//                                          implement: {id, version}} to <file>
//   seed.ts capture-handoffs <file> <run dir>
//                                          every handoff record of the issue <file> names, newest
//                                          first as Dispatch lists them, each with the body it
//                                          stores, as score.ts's testerProof reads them
//                                          (handoffs.json in <run dir>)
//   seed.ts ask-on-message <file>          one run's own issue and the owner's message posting its
//                                          plan; writes {issue, message} to <file>
//   seed.ts measure-before-ask <file>      one run's own issue and the owner's message naming three
//                                          ways to finish, one of which the export rig.sh writes
//                                          beside the agent rules out; writes {issue, message}
//   seed.ts capture <file> <run dir>       what the run left on the issue <file> names: the message,
//                                          every ask and the whole event log, as score.ts reads
//                                          them (message.json, asks.json, events.json in <run dir>)
//   seed.ts brainstorm                     project TODO, the to-do CLI rig.sh's run_brainstorm
//                                          writes into each agent's working directory, with two
//                                          unrelated issues; brainstorm's world, in place of
//                                          `project`
//   seed.ts capture-brainstorm <file> <run dir>
//                                          the issue <run dir>'s own transcript created (Dispatch's
//                                          `Created <KEY>:` answer in a tool result; null when it
//                                          created none) and that issue's asks, as score.ts's
//                                          brainstormSurface reads them (brainstorm.json in
//                                          <run dir>); <file> is unread, kept for dispatch_session's
//                                          one capture call shape
//   seed.ts spec-writing <file> <source dir>
//                                          one run's own issue in LWEVAL, filed by the person alice
//                                          with the source's title.txt and an empty spec. Then the
//                                          source's input in time order: each entry of messages.md
//                                          as a message by alice, and each answered question of
//                                          asks.json as an ask that the agent bearer opens (as the
//                                          session SEED_ASKER, never the run's) and alice answers
//                                          with its selection and text. Writes {issue,
//                                          spec_artifact} to <file> and the seeded asks' ids, as a
//                                          JSON array, to seeded.json beside <file>
//   seed.ts spec-rewrite <file> <source dir>
//                                          spec-writing on an issue whose spec is the source's
//                                          old-spec.md; refuses a source without one, naming it.
//                                          Dispatch opens each ask block of the old spec as an open
//                                          ask, whatever state it held; alice answers each, in time
//                                          order, with the source's answer to that question
//                                          (asks.json, matched by question text), that source ask is
//                                          not opened again, and its id goes to seeded.json. A block
//                                          the source holds no answer to fails the seeding, naming
//                                          the block and the source
//   seed.ts state <file> <turn> <run dir>  the open questions on the issue, no seeded ask among
//                                          them, and the spec's open approval request; writes them
//                                          to <run dir>/state-<turn>.json and prints the same JSON
//   seed.ts answer <file> <answers file>   answers, as alice, the ask of each JSONL line
//                                          {ask, selected, text} of <answers file>; refuses a line
//                                          that names a seeded ask
//   seed.ts approve <file>                 answers, as alice, the spec's open approval request with
//                                          Approve, which is the review
//   seed.ts capture-spec <file> <run dir>  what the run wrote on the issue, the seeded asks left
//                                          out: spec.md, spec-versions.json and spec-versions/<n>.md,
//                                          agent-asks.json, agent-texts.json (no message or comment
//                                          the run deleted), documents/<slug>.md and plan.md (the run
//                                          directory table of judge.ts and score.ts)
//   seed.ts askgate <file>                 one run's own issue, with an empty spec and one comment
//                                          by the person alice; writes {issue, comment,
//                                          comment_ref} to <file>
//   seed.ts capture-askgate <file> <run dir>
//                                          the asks and documents on the issue <file> names, as
//                                          score.ts's askgateReview reads them (asks.json,
//                                          artifacts.json in <run dir>)
//   seed.ts owner-walk <file> <kind> <answer>
//                                          owner-walk's world under LWEVAL, for an owner of <kind>
//                                          (root or child), from the run's recorded plans answer
//                                          <answer> (owner-walk-seed.ts ownerWalkIssues)
//   seed.ts capture-scored <file> <run dir>
//                                          the world's Dispatch records after a scored run, as
//                                          captured.json in <run dir> (owner-walk-seed.ts
//                                          captureScored)
//   seed.ts case-world                     seed the Dispatch part of $SKILL_SCENARIO_RUN/launch-world.json:
//                                          its records in case time order, then its step runs at
//                                          times among the records' stamps and a retro's rating
//                                          (seedCaseDispatch); writes dispatch-ids.json and
//                                          dispatch-fixture.json there
//   seed.ts case-readback <run dir>         capture the issue, documents, handoffs, comments,
//                                          asks, results, step runs and complete event log (with
//                                          seed cursor)
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import {
  type Actor,
  type ArtifactBlock,
  type Ask,
  type AskOption,
  type DispatchEvent,
  formatBlockReference,
  parseCanonicalBlockReference,
} from "@legion/contracts";
import { z } from "zod";
import {
  answerAsk,
  baseUrl,
  changeIssueStatus,
  createAsk,
  createComment,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  e2eAgentToken,
  getArtifact,
  getArtifactText,
  getArtifactVersion,
  getIssue,
  getIssueEvents,
  getIssueResults,
  getMessage,
  listComments,
  listIssueAsks,
  listIssueMessages,
  putStep,
  recordCheckResult,
  resolveAsk,
} from "../../../dispatch/e2e/api";
import type { Body } from "../../../dispatch/web/src/api/paths";
import { ENTRY_HEADER } from "./fetch-spec-sources";
import { World } from "./heldout-case";
import { captureScored, ownerWalkIssues } from "./owner-walk-seed";
import { parseInstant, type Sleep } from "./production-dispatch";
import { APPROVE, handoffsOf, PERSON } from "./seed-dispatch";
import { CREATED, session, sessionId, toolResults } from "./transcript";

/** What `ask-on-message` writes and `capture` reads. */
const Fixture = z.object({ issue: z.string(), message: z.string() });
/** What `askgate` writes and `capture-askgate` reads. */
const AskgateFixture = z.object({
  issue: z.string(),
  comment: z.string(),
  comment_ref: z.string(),
});
/** The most events one `GET /api/v1/issues/{key}/events` page holds. */
const EVENT_PAGE = 200;

/** What `spec-writing` and `spec-rewrite` write, and every later spec mode reads. */
const SpecFixture = z.object({ issue: z.string(), spec_artifact: z.string() });
type SpecFixture = z.infer<typeof SpecFixture>;
/** The session a seeded ask is opened as: another agent's, never the run's own. */
export const SEED_ASKER = { kind: "session", id: "spec-source-agent" } as const;
/** What `tester-proof` writes, and `implement-handoff` and `capture-handoffs` read: the run's issue,
 * and once `implement-handoff` recorded it, the implementer's record. */
const TesterFixture = z.object({
  issue: z.string(),
  implement: z.object({ id: z.string(), version: z.number().int() }).optional(),
});
/** The session the tester's predecessor wrote its implement handoff as: the implementer's, never
 * the run's own. */
export const IMPLEMENTER = { kind: "session", id: "rig-implementer" } as const;
/** How long spec-rewrite waits for Dispatch's settlement to open the old spec's ask blocks as asks
 * (it settles a seeded document 2 s after the issue is created), and how often it looks. */
const BLOCK_ASKS_WAIT_MS = 30_000;
const BLOCK_ASKS_POLL_MS = 250;

/** Every entry header of a source's messages.md: fetch-spec-sources.ts's ENTRY_HEADER, every match. */
const ENTRY = new RegExp(ENTRY_HEADER.source, "gm");
/** One ask of a source's asks.json, as fetch-spec-sources.ts writes it. */
const SourceAsk = z.object({
  id: z.string(),
  kind: z.enum(["question", "approval"]),
  question: z.string(),
  options: z.array(z.object({ label: z.string(), description: z.string().optional() })),
  multiple: z.boolean(),
  state: z.enum(["open", "answered", "resolved"]),
  created_at: z.string(),
  answer: z
    .object({
      selected: z.array(z.string()),
      text: z.string().nullable(),
      user: z.string(),
      at: z.string(),
    })
    .nullable(),
});
/** One line of an answers file, as judge.ts answer writes it. */
const StandInAnswer = z.object({
  ask: z.string(),
  selected: z.array(z.string()),
  text: z.string(),
});

/** One write of a source's input, at the time the source gives it. */
interface Step {
  at: bigint;
  write: () => Promise<void>;
}

/** The time `value` names, in nanoseconds since the epoch (production-dispatch.ts's
 * parseInstant), so the microseconds Dispatch stores keep their order; a value that is no RFC 3339
 * time is the source's fault. */
function time(value: string, where: string): bigint {
  const at = parseInstant(value);
  if (at === undefined)
    throw new Error(`${where}: ${JSON.stringify(value)} is not an RFC 3339 time`);
  return at;
}

/** Orders two times in nanoseconds for a stable sort. They are compared, not subtracted: a bigint
 * difference is no sort key. */
function byTime(a: bigint, b: bigint): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

/** The entries of a source's messages.md, oldest first as the file holds them. */
function sourceEntries(file: string): { kind: string; at: bigint; body: string }[] {
  const text = readFileSync(file, "utf8");
  const headers = [...text.matchAll(ENTRY)];
  const before = text.slice(0, headers[0]?.index ?? text.length);
  if (before.trim() !== "") throw new Error(`${file}: text comes before its first entry header`);
  return headers.map((header, index) => {
    const start = (header.index ?? 0) + header[0].length + 1;
    const body = text.slice(start, headers[index + 1]?.index ?? text.length).trimEnd();
    if (body.trim() === "") throw new Error(`${file}: the entry ${header[0]} has no text`);
    return { kind: header[1] ?? "", at: time(header[2] ?? "", file), body };
  });
}

/** The asks Dispatch opened for the ask blocks of the spec `spec` of `issue`, once its settlement
 * opened one for every block: the spec's blocks of type `ask` (`GET …/blocks`, as the agent bearer),
 * then the issue's asks, read again until each such block has its ask. */
async function specBlockAsks(issue: string, spec: string): Promise<Ask[]> {
  const response = await fetch(
    new URL(`/api/v1/artifacts/${encodeURIComponent(spec)}/blocks`, baseUrl),
    {
      headers: { Authorization: `Bearer ${e2eAgentToken}` },
    }
  );
  if (!response.ok)
    throw new Error(
      `GET the blocks of ${spec} failed: ${response.status} ${await response.text()}`
    );
  const blocks = ((await response.json()) as ArtifactBlock[]).filter(
    (block) => block.type === "ask"
  );
  if (blocks.length === 0) return [];
  const deadline = Date.now() + BLOCK_ASKS_WAIT_MS;
  for (;;) {
    const asks = await listIssueAsks(issue, { as: "agent" });
    const opened = blocks.map((block) => asks.find((ask) => ask.block_id === block.id));
    if (opened.every((ask) => ask !== undefined)) return opened as Ask[];
    if (Date.now() > deadline) {
      const missing = blocks.filter((_, index) => opened[index] === undefined);
      throw new Error(
        `${issue}'s spec holds ask blocks ${missing.map((block) => block.id).join(", ")} that Dispatch opened no ask for within ${BLOCK_ASKS_WAIT_MS / 1000} s`
      );
    }
    await Bun.sleep(BLOCK_ASKS_POLL_MS);
  }
}

/** Creates the run's issue as the person, posts the source's input in time order, and writes the
 * fixture and seeded.json. `spec` is the issue's first spec; an empty spec when it is absent. Each
 * ask block of `spec` is an ask Dispatch opens (an upload resets the block to open), answered here
 * as the source answered its question. */
async function seedSpecIssue(file: string, source: string, spec?: string): Promise<void> {
  const title = readFileSync(path.join(source, "title.txt"), "utf8").trim();
  if (title === "") throw new Error(`${path.join(source, "title.txt")} holds no title`);
  const entries = sourceEntries(path.join(source, "messages.md"));
  const asksFile = path.join(source, "asks.json");
  const asks = z.array(SourceAsk).parse(JSON.parse(readFileSync(asksFile, "utf8")));
  const issue = await createIssue(
    { project: "LWEVAL", force: true, title, ...(spec === undefined ? {} : { spec }) },
    { login: PERSON }
  );
  const person = { as: "user", login: PERSON } as const;
  const steps: Step[] = entries.map((entry) => ({
    at: entry.at,
    write: async () => {
      await createMessage(issue.key, { body: entry.body }, person);
    },
  }));
  const seeded: string[] = [];
  const answers: Step[] = [];
  const answeredInSource = asks.filter(
    (ask) => ask.kind === "question" && ask.state === "answered" && ask.answer !== null
  );
  // The source asks an old-spec block holds: each is on the issue as its block's ask, so it is not
  // opened a second time below.
  const inSpec = new Set<string>();
  const blockAsks =
    spec === undefined ? [] : await specBlockAsks(issue.key, issue.primary_artifact_id);
  for (const opened of blockAsks) {
    const source = answeredInSource.find((ask) => ask.question.trim() === opened.question.trim());
    const answer = source?.answer;
    if (source === undefined || answer == null)
      throw new Error(
        `${asksFile} holds no answer to ${JSON.stringify(opened.question)}, the question of the old spec's ask block ${opened.block_id} (ask ${opened.id} on ${issue.key}); spec-rewrite does not make one up`
      );
    inSpec.add(source.id);
    seeded.push(opened.id);
    answers.push({
      at: time(answer.at, asksFile),
      write: async () => {
        await answerAsk(
          opened.id,
          {
            selected: answer.selected,
            ...(answer.text === null ? {} : { text: answer.text }),
            expected_edited_at: opened.edited_at,
          },
          { login: PERSON }
        );
      },
    });
  }
  // Only an answered question is the person's input: an open ask holds none, and an approval is a
  // review of an old document version, which the run's issue does not hold.
  for (const ask of asks) {
    const answer = ask.answer;
    if (ask.kind !== "question" || ask.state !== "answered" || answer === null) continue;
    if (inSpec.has(ask.id)) continue;
    let opened: Ask | undefined;
    steps.push({
      at: time(ask.created_at, asksFile),
      write: async () => {
        opened = await createAsk(
          issue.key,
          { question: ask.question, options: ask.options, multiple: ask.multiple },
          { as: "agent", actor: SEED_ASKER }
        );
        seeded.push(opened.id);
      },
    });
    answers.push({
      at: time(answer.at, asksFile),
      write: async () => {
        if (opened === undefined)
          throw new Error(`the ask ${ask.id} was answered before it opened`);
        await answerAsk(
          opened.id,
          {
            selected: answer.selected,
            ...(answer.text === null ? {} : { text: answer.text }),
            expected_edited_at: opened.edited_at,
          },
          { login: PERSON }
        );
      },
    });
  }
  // A stable sort: at one time, the messages come first, then each ask opens before its answer.
  const ordered = [...steps, ...answers].sort((a, b) => byTime(a.at, b.at));
  for (const step of ordered) await step.write();
  const fixture: SpecFixture = { issue: issue.key, spec_artifact: issue.primary_artifact_id };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  writeFileSync(path.join(path.dirname(file), "seeded.json"), `${JSON.stringify(seeded)}\n`);
  console.log(`seeded ${issue.key}: ${entries.length} messages, ${seeded.length} answered asks`);
}

/** `spec-writing <file> <source dir>`: the run's issue with an empty spec. */
export function specWriting(file: string, source: string): Promise<void> {
  return seedSpecIssue(file, source);
}

/** `spec-rewrite <file> <source dir>`: the run's issue with the source's old spec. It refuses a
 * source without old-spec.md before it writes anything. */
export async function specRewrite(file: string, source: string): Promise<void> {
  const oldSpec = path.join(source, "old-spec.md");
  if (!existsSync(oldSpec)) throw new Error(`the source has no ${oldSpec}: spec-rewrite needs it`);
  const spec = readFileSync(oldSpec, "utf8");
  if (spec.trim() === "") throw new Error(`${oldSpec} is empty: spec-rewrite needs an old spec`);
  await seedSpecIssue(file, source, spec);
}

/** The fixture `file` names and the ids in seeded.json beside it. */
function loadSpecFixture(file: string): SpecFixture & { seeded: Set<string> } {
  const fixture = SpecFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const seededFile = path.join(path.dirname(file), "seeded.json");
  const seeded = z.array(z.string()).parse(JSON.parse(readFileSync(seededFile, "utf8")));
  return { ...fixture, seeded: new Set(seeded) };
}

/** The spec's open approval request among `asks`: the open approval ask on the primary spec. */
function openSpecApproval(asks: Ask[], fixture: SpecFixture): Ask | undefined {
  return asks.find(
    (ask) =>
      ask.kind === "approval" &&
      ask.state === "open" &&
      ask.approval?.artifact_id === fixture.spec_artifact
  );
}

/** What `state` writes to state-<turn>.json. */
export interface TurnState {
  turn: number;
  open_asks: {
    id: string;
    question: string;
    options: Required<Omit<AskOption, "id">>[];
    multiple: boolean;
  }[];
  approval: { ask_id: string; version: number } | null;
}

/** `state <file> <turn> <run dir>`: the open questions no seed opened, and the spec's open
 * approval request. */
export async function state(file: string, turn: number, runDir: string): Promise<TurnState> {
  const fixture = loadSpecFixture(file);
  const asks = await listIssueAsks(fixture.issue, { as: "agent" });
  const approval = openSpecApproval(asks, fixture);
  const result: TurnState = {
    turn,
    open_asks: asks
      .filter((ask) => ask.kind === "question" && ask.state === "open")
      .filter((ask) => !fixture.seeded.has(ask.id))
      .map((ask) => ({
        id: ask.id,
        question: ask.question,
        options: (ask.options ?? []).map((option) => ({
          label: option.label,
          description: option.description ?? "",
        })),
        multiple: ask.multiple,
      })),
    approval:
      approval?.approval === undefined
        ? null
        : { ask_id: approval.id, version: approval.approval.version },
  };
  const text = JSON.stringify(result);
  writeFileSync(path.join(runDir, `state-${turn}.json`), `${text}\n`);
  console.log(text);
  return result;
}

/** `answer <file> <answers file>`: the person answers each line's ask with the stand-in's choice.
 * It checks every line before it posts one. */
export async function answer(file: string, answersFile: string): Promise<void> {
  const fixture = loadSpecFixture(file);
  const lines = readFileSync(answersFile, "utf8")
    .split("\n")
    .filter((line) => line.trim() !== "")
    .map((line) => StandInAnswer.parse(JSON.parse(line)));
  const seededLine = lines.find((line) => fixture.seeded.has(line.ask));
  if (seededLine !== undefined)
    throw new Error(
      `${answersFile} answers the seeded ask ${seededLine.ask}, which is not the run's`
    );
  const asks = new Map(
    (await listIssueAsks(fixture.issue, { as: "agent" })).map((ask) => [ask.id, ask])
  );
  for (const line of lines) {
    const ask = asks.get(line.ask);
    if (ask === undefined) throw new Error(`${fixture.issue} has no ask ${line.ask}`);
    await answerAsk(
      ask.id,
      { selected: line.selected, text: line.text, expected_edited_at: ask.edited_at },
      { login: PERSON }
    );
  }
  console.log(`answered ${lines.length} asks on ${fixture.issue}`);
}

/** `approve <file>`: the person approves the spec, answering its open approval request. */
export async function approve(file: string): Promise<void> {
  const fixture = loadSpecFixture(file);
  const request = openSpecApproval(await listIssueAsks(fixture.issue, { as: "agent" }), fixture);
  if (request === undefined)
    throw new Error(`${fixture.issue} has no open approval request on ${fixture.spec_artifact}`);
  await answerAsk(
    request.id,
    { selected: [APPROVE], expected_edited_at: request.edited_at },
    { login: PERSON }
  );
  console.log(`approved ${fixture.issue}'s spec at version ${request.approval?.version}`);
}

/** One row of agent-texts.json. */
interface AgentText {
  id: string;
  kind: "message" | "comment" | "ask";
  body: string;
  origin_session_title: string | null;
  created_at: string;
}

/** `capture-spec <file> <run dir>`: what the run left on its issue, for judge.ts and score.ts. */
export async function captureSpec(file: string, runDir: string): Promise<void> {
  const fixture = loadSpecFixture(file);
  const reader = { as: "agent" } as const;
  const out = (name: string, text: string) => writeFileSync(path.join(runDir, name), text);
  const json = (name: string, value: unknown) => out(name, `${JSON.stringify(value)}\n`);

  out("spec.md", (await getArtifactText(fixture.spec_artifact, reader)).markdown);
  const spec = await getArtifact(fixture.spec_artifact, reader);
  mkdirSync(path.join(runDir, "spec-versions"), { recursive: true });
  for (const version of spec.versions ?? []) {
    const text = await getArtifactVersion(spec.id, version.number, reader);
    out(path.join("spec-versions", `${version.number}.md`), text.markdown);
  }
  json(
    "spec-versions.json",
    (spec.versions ?? []).map((version) => ({
      version: version.number,
      created_at: version.created_at,
      authors: version.authors,
      actor: (version.authors ?? [])[0] ?? null,
    }))
  );

  const asks = (await listIssueAsks(fixture.issue, reader)).filter(
    (ask) => !fixture.seeded.has(ask.id)
  );
  json(
    "agent-asks.json",
    asks.map(({ id, kind, question, options, state, answer, created_at }) => ({
      id,
      kind,
      question,
      options,
      state,
      answer,
      created_at,
    }))
  );

  // The run's session is the one its transcript's header names; a run with no transcript wrote
  // nothing. A message or comment the run deleted is left out and counted, as
  // fetch-spec-sources.ts's `standing` leaves a deleted post out: Dispatch reads it with its body
  // empty, which the judges would take as something the run said (dispatch://INFRA-1523).
  const runSession = sessionId(session(runDir));
  const texts: AgentText[] = [];
  let deleted = 0;
  const add = (
    id: string,
    kind: AgentText["kind"],
    author: Actor,
    body: string,
    at: string,
    deletedAt: string | null
  ) => {
    if (author.kind !== "session" || author.id !== runSession) return;
    if (deletedAt != null) {
      deleted += 1;
      return;
    }
    texts.push({
      id,
      kind,
      body,
      origin_session_title: author.origin?.session_title ?? null,
      created_at: at,
    });
  };
  for (const ask of asks) {
    const lines = [ask.question];
    for (const option of ask.options ?? []) {
      lines.push(option.label);
      if (option.description) lines.push(option.description);
    }
    add(ask.id, "ask", ask.author, lines.join("\n"), ask.created_at, null);
  }
  for (const thread of await listIssueMessages(fixture.issue, reader)) {
    for (const message of [thread.message, ...(thread.replies ?? [])]) {
      add(
        message.id,
        "message",
        message.author,
        message.body,
        message.created_at,
        message.deleted_at
      );
    }
  }
  for (const comment of await listComments(fixture.issue, undefined, reader)) {
    add(
      comment.id,
      "comment",
      comment.author,
      comment.body,
      comment.created_at,
      comment.deleted_at
    );
  }
  // Dispatch's times carry microseconds, which Date.parse drops; two texts of one time keep the order
  // the capture read them in.
  const when = (text: AgentText) => time(text.created_at, `${fixture.issue}'s text ${text.id}`);
  json(
    "agent-texts.json",
    texts.sort((a, b) => byTime(when(a), when(b)))
  );

  // Every other markdown document: plan.md on its own, the rest under documents/.
  rmSync(path.join(runDir, "documents"), { recursive: true, force: true });
  rmSync(path.join(runDir, "plan.md"), { force: true });
  mkdirSync(path.join(runDir, "documents"));
  const documents = ((await getIssue(fixture.issue, reader)).artifacts ?? []).filter(
    (artifact) => artifact.kind === "doc" && artifact.id !== fixture.spec_artifact
  );
  for (const document of documents) {
    const { markdown } = await getArtifactText(document.id, reader);
    out(
      document.name === "plan.md" ? "plan.md" : path.join("documents", `${document.slug}.md`),
      markdown
    );
  }
  console.log(
    `captured ${fixture.issue}: ${(spec.versions ?? []).length} spec versions, ${asks.length} asks, ${texts.length} texts (${deleted} deleted posts left out), ${documents.length} documents`
  );
}

async function seedProject(): Promise<void> {
  await createProject({ key: "LWEVAL", name: "skill scenarios" });
  console.log("seeded LWEVAL");
}

/** `tester-proof <file>`: the run's own greet() issue, whose spec marks what rig.sh's
 * worker_fixture meets as `requirement` blocks, and whose Verification document holds the one
 * `check` block the implementer's result and the tester's own run are recorded against. The check
 * names no `serves`, since a pointer must name a block of a document this seed has not read back;
 * the pointer rules are the Dispatch suites' to prove, not this fixture's. Each run's records go
 * on its own issue alone, and `force` passes Dispatch's duplicate check should the issue be filed
 * again. */
export async function testerProofIssue(file: string): Promise<void> {
  const issue = await createIssue(
    {
      project: "LWEVAL",
      force: true,
      title: "greet(): a greeting helper for the widgets CLI",
      spec: [
        "## Goal",
        "",
        "`greet(name)` returns `Hello, <name>!`, and `bun greet.ts <name>` prints it.",
        "",
        "## Requirements",
        "",
        ":::requirement{#r1}",
        "**R1.** `bun greet.ts Ada` prints `Hello, Ada!` and exits 0. From: the issue.",
        ":::",
        "",
        ":::requirement{#r2}",
        "**R2.** `bun greet.ts` with no name, or a blank one, exits 2 and prints `usage: greet.ts <name>` to stderr. From: the issue.",
        ":::",
        "",
        ":::requirement{#r3}",
        "**R3.** Surrounding whitespace in the name is trimmed. From: the issue.",
        ":::",
      ].join("\n"),
    },
    { as: "agent" }
  );
  await createIssueArtifact(
    issue.key,
    {
      name: "verification.md",
      content: [
        "# Verification",
        "",
        ':::check{#k1 surface="cli" run="bun greet.ts Ada" when="before_merge" negative_control="bun greet.ts with no name exits 2 and prints the usage line to stderr."}',
        "`bun greet.ts <name>` prints the greeting, trims the name, and exits 2 with a usage line when no name is given.",
        ":::",
      ].join("\n"),
    },
    { as: "agent" }
  );
  const fixture: z.infer<typeof TesterFixture> = { issue: issue.key };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key}`);
}

/** `implement-handoff <file> <body file> <commit>`: the implementer's handoff on the run's issue,
 * as `dispatch handoff-write` records it under the implementer's session, and its passing result
 * against the issue's check `k1` at `<commit>`, as `dispatch result` records it. A body that is no
 * JSON object is the fixture's fault, refused before anything is written. */
export async function implementHandoff(
  file: string,
  bodyFile: string,
  commit: string
): Promise<void> {
  const { issue } = TesterFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const body: unknown = JSON.parse(readFileSync(bodyFile, "utf8"));
  if (typeof body !== "object" || body === null || Array.isArray(body))
    throw new Error(`${bodyFile} holds no JSON object, which a handoff body is`);
  const recorded = await createHandoff(issue, "implement", body, IMPLEMENTER);
  await agentPost(`/api/v1/issues/${encodeURIComponent(issue)}/results`, {
    check: "k1",
    role: "implementer",
    verdict: "pass",
    commit,
    surface: "cli",
    run: "bun greet.ts Ada",
    observed: "prints Hello, Ada! and exits 0",
    negative_control: "bun greet.ts → exit 2, usage: greet.ts <name> on stderr",
    actor: IMPLEMENTER,
  });
  const fixture: z.infer<typeof TesterFixture> = {
    issue,
    implement: { id: recorded.id, version: recorded.version },
  };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue}'s implement handoff, version ${recorded.version} (${recorded.id})`);
}

/** `POST route` as the agent bearer, with `input` as its JSON body; a refusal names the route and
 * Dispatch's answer. */
async function agentPost(route: string, input: unknown): Promise<void> {
  const response = await fetch(new URL(route, baseUrl), {
    method: "POST",
    headers: {
      Authorization: `Bearer ${e2eAgentToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(input),
  });
  if (!response.ok)
    throw new Error(`POST ${route} failed: ${response.status} ${await response.text()}`);
}

/** `GET route` as the agent bearer, its JSON answer as Dispatch wrote it; a refusal names the
 * route and Dispatch's answer. */
async function agentGet(route: string): Promise<unknown> {
  const response = await fetch(new URL(route, baseUrl), {
    headers: { Authorization: `Bearer ${e2eAgentToken}` },
  });
  if (!response.ok)
    throw new Error(`GET ${route} failed: ${response.status} ${await response.text()}`);
  return response.json();
}

/** `capture-handoffs <file> <run dir>`: every handoff record of the run's issue, the seeded
 * implement record among them, newest first, each with its body (handoffsOf). */
export async function captureHandoffs(file: string, runDir: string): Promise<void> {
  const { issue } = TesterFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const records = await handoffsOf(issue);
  writeFileSync(path.join(runDir, "handoffs.json"), `${JSON.stringify(records)}\n`);
  console.log(`captured ${issue}: ${records.length} handoff records`);
}

async function askOnMessage(file: string): Promise<void> {
  // The owner session's world: an issue whose backfill was proposed and never run, the proposal
  // as an attached table, and the message that posts the plan for the rest of the work. Every run
  // seeds the same issue, so `force` passes Dispatch's duplicate check.
  const owner = { as: "agent", actor: { kind: "session", id: "dispatch-owner" } } as const;
  const issue = await createIssue(
    {
      project: "LWEVAL",
      force: true,
      title: "Make the Architecture page tell the truth: every issue attached to its components",
      spec: [
        "## Summary",
        "",
        "The Architecture page colours each component by the issues attached to it, but most issues",
        "in the two proving-ground trees (the Go refactor and the roadmap) are attached to nothing,",
        "so the page shows one gray box. Every issue in both trees should carry the components it",
        "changes, or a stated reason it is not architectural.",
      ].join("\n"),
    },
    owner
  );
  await createIssueArtifact(
    issue.key,
    {
      name: "attachment-dry-run.md",
      content: [
        "| issue | proposed components | why |",
        "| --- | --- | --- |",
        "| Go refactor: state store | daemon | changes the coordinator's store |",
        "| Go refactor: pod runtime | daemon, worker-image | changes the sandbox runtime |",
        "| Roadmap: hiring pipeline | none (not architectural) | hiring process work |",
        "| Roadmap: search ranking | dispatch-server | changes search |",
      ].join("\n"),
    },
    owner
  );
  const message = await createMessage(
    issue.key,
    {
      body: [
        "Taken by the Dispatch owner session, from tonight's owner audit. What shipped: the",
        "unattached-issue guidance and the coordinator's architecture model. What never ran: the",
        "backfill itself. The dry-run table (attachment-dry-run.md, attached here) only proposed",
        "attachments for the Go refactor's and the roadmap's trees. Next: apply that table through",
        "`dispatch issue-update`, after re-checking each row against the live component model. Then",
        "read both trees back and confirm every child carries components or a not-architectural",
        "reason.",
      ].join(" "),
    },
    owner
  );
  const fixture: z.infer<typeof Fixture> = { issue: issue.key, message: message.id };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key} with message ${message.id}`);
}

async function measureBeforeAsk(file: string): Promise<void> {
  // The owner session's world: the issues a retired component left stranded, and the message
  // naming three ways to finish. Option A needs each issue's owner, and rig.sh's stranded.csv
  // shows 412 of the 430 have none.
  const owner = { as: "agent", actor: { kind: "session", id: "dispatch-owner" } } as const;
  const issue = await createIssue(
    {
      project: "LWEVAL",
      force: true,
      title: "Re-home the issues the retired billing-api component left stranded",
      spec: [
        "## Summary",
        "",
        "The last architecture re-import retired the `billing-api` component. The issues attached",
        "to it still point at it, so the Architecture page files them under a component that no",
        "longer exists. Every stranded issue should end up attached to a live component or marked",
        "not architectural.",
      ].join("\n"),
    },
    owner
  );
  const message = await createMessage(
    issue.key,
    {
      body: [
        "Taken by the Dispatch owner session. The billing-api issues are stranded, and there are",
        "three ways to finish: (A) email each issue's owner and have them re-attach it to the right",
        "component; (B) move every stranded issue to `platform-core`, which absorbed most of",
        "billing-api; (C) detach them all and mark each not architectural. The stranded issues are",
        "exported as stranded.csv (key, title, component, owner).",
      ].join(" "),
    },
    owner
  );
  const fixture: z.infer<typeof Fixture> = { issue: issue.key, message: message.id };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key} with message ${message.id}`);
}

async function capture(file: string, runDir: string): Promise<void> {
  // The event log is read page by page to its end, as the agent bearer the run's session holds.
  const { issue, message } = Fixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const reader = { as: "agent" } as const;
  const events = await getIssueEvents(issue, { limit: EVENT_PAGE }, reader);
  for (let page = events; page.length === EVENT_PAGE; ) {
    page = await getIssueEvents(issue, { after: page.at(-1)?.seq, limit: EVENT_PAGE }, reader);
    events.push(...page);
  }
  const records = {
    message: await getMessage(issue, message, reader),
    asks: await listIssueAsks(issue, reader),
    events,
  };
  for (const [name, record] of Object.entries(records)) {
    writeFileSync(path.join(runDir, `${name}.json`), `${JSON.stringify(record)}\n`);
  }
  console.log(`captured ${issue}: ${records.asks.length} asks, ${events.length} events`);
}

async function seedBrainstorm(): Promise<void> {
  // The to-do CLI's Dispatch project, the batch's only one, with two issues unrelated to the
  // prompt's feature (a crash already understood and a display tweak). Each title names the "todo
  // CLI" in plain words: Postgres's English parser reads `todo.py` and `to-do` as single lexemes, so
  // a session's `dispatch search` for "todo CLI" finds a title only where `todo` stands alone.
  await createProject({ key: "TODO", name: "todo CLI" });
  for (const [title, problem] of [
    [
      "The todo CLI's done command crashes on a task number past the end of the list",
      "`python todo.py done 9` on a list of three tasks raises IndexError and prints a traceback instead of saying the task does not exist.",
    ],
    [
      "The todo CLI's list command shows no task count",
      "A long list gives no total; `list` should end with a line such as `3 tasks`.",
    ],
  ]) {
    const issue = await createIssue(
      { project: "TODO", force: true, title, spec: `## Problem\n\n${problem}` },
      { as: "agent" }
    );
    console.log(`seeded ${issue.key}`);
  }
}

async function seedAskgate(file: string): Promise<void> {
  // The person's own words are one comment on an issue with no spec, so a write that states what
  // the person asked for must link that comment. The title fits both the facts case's release
  // branch and the comment's nightly export, so no case's write is off-topic for the issue. Every
  // run seeds the same issue, so `force` passes Dispatch's duplicate check.
  const issue = await createIssue(
    { project: "LWEVAL", force: true, title: "Release and nightly job settings", spec: "" },
    { as: "agent" }
  );
  const comment = await createComment(
    issue.key,
    {
      body: "Please move the nightly export to 04:00 UTC; the 02:00 run collides with the backup.",
    },
    { as: "user", login: "alice" }
  );
  const fixture: z.infer<typeof AskgateFixture> = {
    issue: issue.key,
    comment: comment.id,
    comment_ref: `dispatch://${issue.key}/comment/${comment.id}`,
  };
  writeFileSync(file, `${JSON.stringify(fixture)}\n`);
  console.log(`seeded ${issue.key} with comment ${comment.id}`);
}

async function captureAskgate(file: string, runDir: string): Promise<void> {
  const { issue } = AskgateFixture.parse(JSON.parse(readFileSync(file, "utf8")));
  const reader = { as: "agent" } as const;
  const records = {
    asks: await listIssueAsks(issue, reader),
    artifacts: (await getIssue(issue, reader)).artifacts,
  };
  for (const [name, record] of Object.entries(records)) {
    writeFileSync(path.join(runDir, `${name}.json`), `${JSON.stringify(record)}\n`);
  }
  console.log(
    `captured ${issue}: ${records.asks.length} asks, ${(records.artifacts ?? []).length} documents`
  );
}

async function captureBrainstorm(runDir: string): Promise<void> {
  // The run's own issue, from its own transcript, never from the project, which holds the two
  // seeded issues too. Dispatch answers `Created <KEY>:` only when it creates the issue, whatever
  // reached it (score.ts's CREATED); a refused duplicate reads `Not created:`.
  let issue: string | null = null;
  for (const text of toolResults(session(runDir))) {
    const match = CREATED.exec(text);
    if (match?.[1] !== undefined) {
      issue = match[1];
      break;
    }
  }
  const asks = issue === null ? [] : await listIssueAsks(issue, { as: "agent" });
  writeFileSync(path.join(runDir, "brainstorm.json"), `${JSON.stringify({ issue, asks })}\n`);
  console.log(`captured ${issue ?? "no new issue"}: ${asks.length} asks`);
}

const CaseDispatchFixture = z.object({ issue: z.string(), seedThrough: z.number().int() });

async function caseEvents(issue: string): Promise<DispatchEvent[]> {
  const reader = { as: "agent" } as const;
  const events: DispatchEvent[] = [];
  let after: number | undefined;
  for (;;) {
    const page = await getIssueEvents(issue, { after, limit: EVENT_PAGE }, reader);
    events.push(...page);
    if (page.length < EVENT_PAGE) return events;
    const next = page.at(-1)?.seq;
    if (next === undefined || (after !== undefined && next <= after))
      throw new Error(`${issue}: Dispatch event cursor did not advance`);
    after = next;
  }
}

/** `text` with each block reference written as Dispatch writes one: a case names a block by its
 * id (`dispatch://ACME-12/spec#r1`, the block `:::requirement{#r1}`), and Dispatch refuses any
 * reference but `#b-<id>` (`dispatch://ACME-12/spec#b-r1`) in a block's attribute. The pointers of
 * each typed block are then written as typedBlockPointers writes them. `issue` is the case's
 * issue. `document` is the slug of the document the text is written as (`spec` for the issue's
 * spec), and undefined for the text of a comment or an ask. A reference already in that form, and
 * any other text, is left as it is. */
export function dispatchBlockRefs(text: string, issue: string, document?: string): string {
  return typedBlockPointers(
    text.replace(
      /(dispatch:\/\/[A-Z][A-Z0-9]*(?:-\d+)?\/(?:spec|artifact\/[A-Za-z0-9._-]+)(?:@v\d+)?)#(?!b-)([A-Za-z0-9][A-Za-z0-9-]*)/g,
      "$1#b-$2"
    ),
    issue,
    document
  );
}

/** The slug Dispatch gives a new document `name` on an issue that holds no other document of that
 * slug (artifactSlug in packages/envoy/internal/dispatch/api/artifacts.go): its ASCII letters and
 * digits in lower case, each run of other characters one `-`, no `-` at either end, and
 * `artifact` when nothing is left. */
function documentSlug(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return slug === "" ? "artifact" : slug;
}

/** The pointer attributes that a case writes with bare ids, as `<typed block>.<attribute>`
 * (pmdoc's schema/blocks.json). A `serves` points at a requirement of the issue's spec. A `needs`
 * points only at a block of its own document (its `scope`). */
const BARE_POINTERS: Record<string, true> = {
  "check.serves": true,
  "plan_task.serves": true,
  "plan_task.needs": true,
};
/** The opening line of a typed block: at most three spaces, three colons or more, the block's
 * name, `{`, the attributes, and the `}` that ends the line (pmdoc's ParseTypedDirectiveOpen). */
const TYPED_OPENER = /^( {0,3}:{3,}([A-Za-z0-9_-]+)\{)(.*)\}$/;
/** The opening line of a fenced code block: at most three spaces, then three backticks or more,
 * or three tildes or more. */
const CODE_FENCE = /^ {0,3}(`{3,}|~{3,})/;
/** In a typed block's attributes: a quoted value, which is skipped whole, or an attribute with a
 * value, whose name and value are the two groups. A quoted value holds no `"`, so no text in the
 * value of an attribute is read as another attribute. */
const POINTER = /"[^"]*"|(?<=^|\s)([A-Za-z0-9_-]+)="([^"]*)"/g;
/** A block id that a pointer names alone (`r1`), in the id grammar of the reference migration in
 * dispatchBlockRefs. */
const BARE_ID = /^[A-Za-z0-9][A-Za-z0-9-]*$/;
/** A reference whose pin stands before its fragment (`…/plan-md@v3#b-step-1`): the document, the
 * pin and the block id. */
const PIN_BEFORE_FRAGMENT =
  /^(dispatch:\/\/[^\s#@]+)@v([1-9][0-9]*)#(?:b-)?([A-Za-z0-9][A-Za-z0-9-]*)$/;

/** `text` with each pointer of BARE_POINTERS on the opening line of a typed block written as
 * Dispatch writes a pointer (pointerEntry). Dispatch refuses any other pointer (400
 * INVALID_MARKDOWN). Only an opening line outside fenced code is read, and in it only those
 * pointers. Every other line, attribute and entry is left as it is. */
function typedBlockPointers(text: string, issue: string, document: string | undefined): string {
  let fence: string | undefined;
  return text
    .split("\n")
    .map((line) => {
      if (fence !== undefined) {
        const rest = line.replace(/^ {0,3}/, "").replace(/[ \t]+$/, "");
        if (rest.length >= fence.length && rest === fence.charAt(0).repeat(rest.length))
          fence = undefined;
        return line;
      }
      fence = CODE_FENCE.exec(line)?.[1];
      const opener = fence === undefined ? TYPED_OPENER.exec(line) : null;
      if (opener === null) return line;
      const [, head = "", block = "", attributes = ""] = opener;
      const write = (whole: string, name?: string, value?: string): string => {
        if (value === undefined || BARE_POINTERS[`${block}.${name}`] !== true) return whole;
        const home = name === "needs" ? document : "spec";
        const entries = value.split(" ").map((entry) => pointerEntry(entry, issue, home));
        return `${name}="${entries.join(" ")}"`;
      };
      return `${head}${attributes.replace(POINTER, write)}}`;
    })
    .join("\n");
}

/** One entry of a pointer as formatBlockReference writes it. A block id alone (`r1`) names that
 * block of `home`, a document of `issue` (`dispatch://ACME-12/spec#b-r1`), and is left as it is
 * where `home` is undefined. A reference pinned before its fragment
 * (`dispatch://ACME-12/artifact/plan-md@v3#b-step-1`) takes the pin after the fragment
 * (`dispatch://ACME-12/artifact/plan-md#b-step-1@v3`). Any other entry, and one that would be no
 * canonical reference (parseCanonicalBlockReference), is left as it is. */
function pointerEntry(entry: string, issue: string, home: string | undefined): string {
  if (home !== undefined && BARE_ID.test(entry))
    return formatBlockReference({ issue, document: home, block: entry });
  const pinned = PIN_BEFORE_FRAGMENT.exec(entry);
  if (pinned === null) return entry;
  const [, document = "", pin = "", block = ""] = pinned;
  const reference = parseCanonicalBlockReference(`${document}#b-${block}`);
  return reference === undefined ? entry : formatBlockReference({ ...reference, pin });
}

/** `text` with each reference to a comment or an ask of `key` by the case's own id
 * (`dispatch://ACME-41/comment/c-1`) written with the id Dispatch gave it (`ids`, by
 * `comment/<case id>` or `ask/<case id>`): Dispatch refuses an id it did not give. Any other
 * reference, and any other text, is left as it is. */
export function caseDispatchRefs(
  text: string,
  key: string,
  ids: ReadonlyMap<string, string>
): string {
  return text.replace(
    /dispatch:\/\/([A-Z][A-Z0-9]*-\d+)\/(comment|ask)\/([A-Za-z0-9][A-Za-z0-9_-]*)/g,
    (whole, issue: string, kind: string, id: string) =>
      issue === key && ids.has(`${kind}/${id}`)
        ? `dispatch://${issue}/${kind}/${ids.get(`${kind}/${id}`)}`
        : whole
  );
}

/** The longest gap the seed keeps between two handoffs it writes one after the other, in
 * nanoseconds: it waits that gap between them, so two versions a case holds seconds apart are
 * seconds apart in the run's Dispatch too. A longer gap, and one between any other two records,
 * is not kept: each record follows the one before it at once. */
const KEPT_HANDOFF_GAP = 60_000_000_000n;
/** A second in nanoseconds: how far before the first seeded record a step run's time is placed
 * where no record comes before it, and after the last where none comes after it. */
const SECOND = 1_000_000_000n;
/** The namespace of the daemon's step record ids (stepRecordNamespace in
 * packages/daemon/internal/workflow/steps.go): Dispatch refuses a step id that is no uuid. */
const STEP_RECORD_NAMESPACE = Bun.randomUUIDv5("legion-step-record", "oid");
/** The run of the issue the seed writes every step run under: its first, as the daemon counts an
 * issue's runs (record.Issue.Generation). */
const SEEDED_GENERATION = 1;
/** The type of work of a step's role, as the daemon's plan records it (builtinRoles in
 * packages/daemon/internal/plan/plan.go). A retro and a merge have none. */
const STEP_TYPES: Readonly<Record<string, string>> = {
  planner: "planning",
  implementer: "implementation",
  tester: "testing",
  reviewer: "review",
  acceptance: "review",
};

/** A seeded record's time on the case's clock and Dispatch's stamp of it, in nanoseconds. */
interface Stamped {
  at: bigint;
  stamp: bigint;
}

/** `at`, nanoseconds since the epoch, as an RFC 3339 time in UTC with nine digits of fraction,
 * all of which Dispatch keeps for a step run's times. */
function instantText(at: bigint): string {
  const whole = new Date(Number(at / SECOND) * 1000).toISOString().slice(0, 19);
  return `${whole}.${(at % SECOND).toString().padStart(9, "0")}Z`;
}

/** One step run of a world, as the case holds it. */
type WorldStep = NonNullable<World["dispatch"]["steps"]>[number];

/** Each step run of `steps`, the world's runs of `issue` in project `project`, as the seed writes
 * it, in world order: the run, its place in the world (`<issue> steps[<n>]`), the id the daemon
 * gives its record, the fields of its step write but its times, and the retro's rating with its
 * reason where the world holds one.
 *
 * The id is the daemon's StepID (packages/daemon/internal/workflow/steps.go): the version 5 uuid
 * of `<issue>/<generation>/<task>/<kind>/<step>/<starts>`, where starts counts the world's runs of
 * that task, kind and step up to and including this one, as the daemon counts the starts of a
 * place. A run Dispatch cannot hold as the daemon writes it is refused here, before the seed writes
 * anything: one with no role, since Dispatch makes a run's record only from a write that names its
 * task, kind, step, role and generation (missingIdentity in
 * packages/envoy/internal/dispatch/workflowstore/write.go); a step whose role has no type of work
 * the seed knows; and a rating without its reason or a reason without its rating, since the rating
 * route takes the two together (stepRatingInput in packages/envoy/internal/dispatch/api/steps.go). */
function stepWrites(issue: string, project: string, steps: readonly WorldStep[]) {
  const starts = new Map<string, number>();
  return steps.map((run, index) => {
    const label = `${issue} steps[${index}]`;
    const { task, step, kind, role, planRating, planReason } = run;
    if (role === null)
      throw new Error(
        `${label}: a run with no role is no run the seed can write: Dispatch makes a run's record only from a write that names its task, kind, step, role and generation`
      );
    const type = kind === "step" ? STEP_TYPES[role] : undefined;
    if (kind === "step" && type === undefined)
      throw new Error(
        `${label}: the role ${role} has no type of work the seed knows (${Object.keys(STEP_TYPES).join(", ")})`
      );
    if ((planRating === undefined) !== (planReason === undefined))
      throw new Error(
        `${label}: the world holds ${planRating === undefined ? "a planReason with no planRating" : "a planRating with no planReason"}, and Dispatch takes a retro's rating only with its reason (PUT /api/v1/issues/{key}/steps/{id}/rating requires both)`
      );
    const place = JSON.stringify([task, kind, step]);
    const count = (starts.get(place) ?? 0) + 1;
    starts.set(place, count);
    const name = `${issue}/${SEEDED_GENERATION}/${task}/${kind}/${step}/${count}`;
    return {
      run,
      label,
      id: Bun.randomUUIDv5(name, STEP_RECORD_NAMESPACE),
      identity: {
        task,
        step,
        kind,
        role,
        ...(type === undefined ? {} : { type }),
        agent: `legion-${project}-${issue}-${role}`.toLowerCase(),
        generation: SEEDED_GENERATION,
      },
      rating:
        planRating === undefined || planReason === undefined
          ? undefined
          : { rating: planRating, rating_reason: planReason },
    };
  });
}

/** The rating a retro gave the plan, written onto the retro's record `id` of `issue` as the daemon
 * writes it (PutStepRating in packages/daemon/internal/dispatch/client.go): through the record's
 * rating route, PUT /api/v1/issues/{key}/steps/{id}/rating, which e2e/api.ts has no helper for,
 * with the step write's body and the rating with its reason, under the agent bearer as the session
 * of the daemon of the issue's project, as putStep writes the step. A refusal names the route and
 * Dispatch's answer. */
async function putStepRating(
  issue: string,
  id: string,
  input: Body<"/api/v1/issues/{key}/steps/{id}/rating", "put">
): Promise<void> {
  const project = issue.slice(0, issue.indexOf("-"));
  const route = `/api/v1/issues/${encodeURIComponent(issue)}/steps/${encodeURIComponent(id)}/rating`;
  const response = await fetch(new URL(route, baseUrl), {
    method: "PUT",
    headers: {
      Authorization: `Bearer ${e2eAgentToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      ...input,
      actor: { kind: "session", id: `legion-daemon:${project}` },
    }),
  });
  if (!response.ok)
    throw new Error(`PUT ${route} failed: ${response.status} ${await response.text()}`);
}

/** The time in the run's Dispatch of each case time of `times`, the starts and ends of the
 * world's step runs, by case time: strictly after Dispatch's stamp of the last of `records` (in
 * seeding order) whose case time comes before it, and strictly before the stamp of the record
 * after that one. A record at the very time a run starts so lies inside the run, and a record at
 * the time it ends outside it. Where no record comes before a time it is one second before the
 * first record's stamp, and where none comes after it one second after the last's. Times between
 * the same two records keep their order: spread evenly between the two stamps, or a second apart
 * before the first record and after the last. */
function placeStepTimes(
  issue: string,
  records: readonly Stamped[],
  times: readonly bigint[]
): Map<bigint, bigint> {
  const first = records[0];
  const last = records.at(-1);
  if (first === undefined || last === undefined)
    throw new Error(
      `${issue}: a step run's times are placed among the stamps of the world's records, and the world holds no handoff, comment, ask or result`
    );
  const gapped = [...new Set(times)].map((at) => ({
    at,
    after: records.filter((record) => record.at < at).length,
  }));
  const placed = new Map<bigint, bigint>();
  for (const { at, after } of gapped) {
    const peers = gapped
      .filter((other) => other.after === after)
      .map((other) => other.at)
      .sort(byTime);
    const count = BigInt(peers.length);
    const rank = BigInt(peers.indexOf(at));
    const low = records[after - 1];
    const high = records[after];
    if (low === undefined) placed.set(at, first.stamp - (count - rank) * SECOND);
    else if (high === undefined) placed.set(at, last.stamp + (rank + 1n) * SECOND);
    else {
      const stamp = low.stamp + ((high.stamp - low.stamp) * (rank + 1n)) / (count + 1n);
      if (stamp <= low.stamp || stamp >= high.stamp)
        throw new Error(
          `${issue}: Dispatch stamped two records seeded one after the other ${instantText(low.stamp)} and ${instantText(high.stamp)}, which leaves no room for a step run's time between them`
        );
      placed.set(at, stamp);
    }
  }
  return placed;
}

/** File in a fresh scratch Dispatch. Dispatch allocates issue numbers; the placeholders reserve
 * preceding keys so the case's own references (including its parent's key) resolve unchanged.
 * Dispatch owns the ids and stamps of records. Seed across record kinds in case time order: a
 * comment with an anchor on its document's words, a reply in the thread of the comment it names,
 * and between two handoffs seeded one after the other at most 60 s apart a wait (`sleep`) of that
 * gap. Then each step run, as the daemon writes it, under the id the daemon gives its record, at
 * times among the records' stamps (placeStepTimes), and a retro's rating after its run
 * (stepWrites); a run the seed cannot write so is refused before anything is written. Writes
 * dispatch-ids.json (each seeded handoff's and comment's Dispatch id by its case id) and
 * dispatch-fixture.json. */
export async function seedCaseDispatch(
  runDir: string,
  sleep: Sleep = (ms) => Bun.sleep(ms)
): Promise<void> {
  const world = World.parse(
    JSON.parse(readFileSync(path.join(runDir, "launch-world.json"), "utf8"))
  );
  const { issue, documents, handoffs, comments, asks, results, steps = [] } = world.dispatch;
  const seed = { as: "agent", actor: { kind: "session", id: "rig-seed" } } as const;
  const writer = (id: string) => ({ as: "agent", actor: { kind: "session", id } }) as const;
  // The case supplies a session id or a human login, not a Dispatch Actor object.
  const author = (id: string) =>
    id.startsWith("ses-") ? writer(id) : ({ login: id, as: "user" } as const);
  const keyParts = (key: string): [string, number] => {
    const [project, number] = key.split("-");
    const n = Number(number);
    if (project === undefined || !Number.isSafeInteger(n) || n < 1 || `${project}-${n}` !== key)
      throw new Error(`${key}: cannot allocate a Dispatch key`);
    return [project, n];
  };
  const [project, number] = keyParts(issue.key);
  const runs = stepWrites(issue.key, project, steps);
  const parent = issue.parent === null ? undefined : keyParts(issue.parent);
  if (parent?.[0] === project && parent[1] >= number)
    throw new Error(`${issue.key}: parent ${issue.parent} must precede its child in the project`);
  const createThrough = async (prefix: string, count: number, target: boolean) => {
    await createProject({ key: prefix, name: prefix });
    for (let n = 1; n <= count; n += 1) {
      const expected = `${prefix}-${n}`;
      const own = target && expected === issue.key;
      const created = await createIssue(
        {
          project: prefix,
          force: true,
          title: own
            ? issue.title
            : expected === issue.parent
              ? `Parent of ${issue.key}`
              : `Reserved key ${expected}`,
          ...(own
            ? {
                spec: dispatchBlockRefs(issue.spec, issue.key, "spec"),
                parent: issue.parent,
                repo: world.github?.repository,
              }
            : {}),
        },
        seed
      );
      if (created.key !== expected)
        throw new Error(`scratch Dispatch assigned ${created.key}, wanted ${expected}`);
    }
  };
  if (parent && parent[0] !== project) await createThrough(...parent, false);
  await createThrough(project, number, true);
  // The case names its documents by name and its records by its own ids; Dispatch gives each one
  // its own id.
  const ids = new Map<string, string>();
  for (const { name, body } of documents) {
    const uploaded = await createIssueArtifact(
      issue.key,
      { name, content: dispatchBlockRefs(body, issue.key, documentSlug(name)) },
      seed
    );
    ids.set(`document/${name}`, uploaded.artifact.id);
  }
  /** The id Dispatch gave the world's `kind` named `id`, which the seed wrote before it. */
  const dispatchId = (kind: "document" | "comment", id: string): string => {
    const given = ids.get(`${kind}/${id}`);
    if (given === undefined) throw new Error(`${id}: no ${kind} of the world was seeded so named`);
    return given;
  };
  /** What dispatch-ids.json holds: each seeded handoff's and comment's Dispatch id by case id. */
  const seededIds = new Map<string, string>();
  const remember = (kind: "comment" | "handoff", id: string, given: string) => {
    if (seededIds.has(id))
      throw new Error(`${id}: the world names two of its handoffs and comments by this id`);
    seededIds.set(id, given);
    ids.set(`${kind}/${id}`, given);
  };
  /** Each record of the world: its case id and time, whether it is a handoff, and its write,
   * which answers Dispatch's stamp of the record. */
  const records: { id: string; at: bigint; handoff: boolean; write: () => Promise<string> }[] = [
    ...handoffs.map((record) => ({
      id: record.id,
      at: time(record.at, record.id),
      handoff: true,
      write: async () => {
        const saved = await createHandoff(
          issue.key,
          record.phase,
          record.body,
          writer(record.author).actor
        );
        if (saved.version !== record.version)
          throw new Error(
            `${record.id}: got ${record.phase} v${saved.version}, wanted v${record.version}`
          );
        remember("handoff", record.id, saved.id);
        return saved.created_at;
      },
    })),
    ...comments.map(({ id, at, author: by, body, anchor, replyTo }) => ({
      id,
      at: time(at, id),
      handoff: false,
      write: async () => {
        const created = await createComment(
          issue.key,
          {
            body: dispatchBlockRefs(body, issue.key),
            ...(anchor === undefined
              ? {}
              : {
                  anchor: {
                    artifact: dispatchId("document", anchor.document),
                    quote: anchor.quote,
                  },
                }),
            ...(replyTo === undefined ? {} : { reply_to: dispatchId("comment", replyTo) }),
          },
          author(by)
        );
        remember("comment", id, created.id);
        return created.created_at;
      },
    })),
    ...asks.map((ask) => ({
      id: ask.id,
      at: time(ask.at, ask.id),
      handoff: false,
      write: async () => {
        const opened = await createAsk(
          issue.key,
          {
            question: dispatchBlockRefs(ask.question, issue.key),
            options: ask.options.map((label) => ({ label })),
          },
          author(ask.author)
        );
        ids.set(`ask/${ask.id}`, opened.id);
        if (ask.state === "answered") {
          if (ask.answer === null) throw new Error(`${ask.id}: answered with no answer`);
          const option = ask.options.find((label) => label === ask.answer);
          await answerAsk(
            opened.id,
            {
              selected: option === undefined ? [] : [option],
              text: ask.answer,
              expected_edited_at: opened.edited_at,
            },
            { login: PERSON }
          );
        } else if (ask.state === "dismissed") {
          await resolveAsk(
            opened.id,
            { kind: "resolved", reason: ask.answer ?? "No answer needed." },
            author(ask.author)
          );
        }
        return opened.created_at;
      },
    })),
    ...results.map((result) => ({
      id: `${result.check}/${result.role}`,
      at: time(result.at, result.check),
      handoff: false,
      write: async () => {
        const { at: _at, ...fields } = result;
        const recorded = await recordCheckResult(
          issue.key,
          { ...fields, observed: "Recorded in the case's starting state." },
          writer(`rig-seed-${result.role}`)
        );
        return recorded.result.created_at;
      },
    })),
  ];
  // Two versions of a phase's handoff seconds apart are what a reviewer reads as one double write,
  // so the gap between two handoffs seeded one after the other is kept when it is a minute or less.
  const stamped: Stamped[] = [];
  let previous: (typeof records)[number] | undefined;
  for (const record of records.sort((a, b) => byTime(a.at, b.at))) {
    const gap = previous?.handoff && record.handoff ? record.at - previous.at : 0n;
    if (gap > 0n && gap <= KEPT_HANDOFF_GAP) await sleep(Number(gap / 1_000_000n));
    const stamp = time(await record.write(), `Dispatch's stamp of ${record.id}`);
    stamped.push({ at: record.at, stamp });
    previous = record;
  }
  writeFileSync(
    path.join(runDir, "dispatch-ids.json"),
    `${JSON.stringify(Object.fromEntries(seededIds))}\n`
  );
  // Each step run as the daemon writes it, with times among the stamps of the records around it,
  // and then a retro's rating.
  if (runs.length > 0) {
    const placed = placeStepTimes(
      issue.key,
      stamped,
      runs.flatMap(({ run: { startedAt, endedAt }, label }) =>
        (endedAt === null ? [startedAt] : [startedAt, endedAt]).map((at) => time(at, label))
      )
    );
    const placedText = (at: string, label: string): string => {
      const stamp = placed.get(time(at, label));
      if (stamp === undefined) throw new Error(`${label}: its time ${at} was not placed`);
      return instantText(stamp);
    };
    for (const { run, label, id, identity, rating } of runs) {
      const write = {
        ...identity,
        started_at: placedText(run.startedAt, label),
        ...(run.endedAt === null || run.result === null
          ? {}
          : { ended_at: placedText(run.endedAt, label), result: run.result }),
      };
      await putStep(issue.key, id, write);
      if (rating !== undefined) await putStepRating(issue.key, id, { ...write, ...rating });
    }
  }
  // The run's task names a comment or an ask as the case does: it now names the id Dispatch gave.
  const prompt = path.join(runDir, "prompt.txt");
  if (existsSync(prompt))
    writeFileSync(prompt, caseDispatchRefs(readFileSync(prompt, "utf8"), issue.key, ids));
  // A closed issue refuses a document upload. Finish seeding the evidence before closing it.
  if (issue.status !== "triage") {
    await changeIssueStatus(issue.key, { status: issue.status, include_parent: true }, seed);
  }
  const seeded = await caseEvents(issue.key);
  const fixture = { issue: issue.key, seedThrough: seeded.at(-1)?.seq ?? 0 };
  writeFileSync(path.join(runDir, "dispatch-fixture.json"), `${JSON.stringify(fixture)}\n`);
}

/** Keep the native API envelopes and original timestamps; outcome.ts selects run writes and
 * reshapes them to the machine-independent schema. Each handoff carries its immutable body. */
export async function captureCaseDispatch(runDir: string): Promise<void> {
  const { issue: key, seedThrough } = CaseDispatchFixture.parse(
    JSON.parse(readFileSync(path.join(runDir, "dispatch-fixture.json"), "utf8"))
  );
  const reader = { as: "agent" } as const;
  const issue = await getIssue(key, reader);
  const documents = await Promise.all(
    (issue.artifacts ?? []).map(async (artifact) => ({
      ...artifact,
      ...(artifact.kind === "doc"
        ? { text: (await getArtifactText(artifact.id, reader)).markdown }
        : {}),
    }))
  );
  const handoffs = await handoffsOf(key);
  const captured = {
    issue,
    seedThrough,
    documents,
    handoffs,
    comments: await listComments(key, undefined, reader),
    asks: await listIssueAsks(key, reader),
    results: await getIssueResults(key, reader),
    steps: await agentGet(`/api/v1/issues/${encodeURIComponent(key)}/steps`),
    events: await caseEvents(key),
  };
  writeFileSync(path.join(runDir, "dispatch-readback.json"), `${JSON.stringify(captured)}\n`);
}

const USAGE =
  "usage: seed.ts project | ask-on-message <file> | measure-before-ask <file> | capture <file> <run dir> | brainstorm | capture-brainstorm <file> <run dir> | spec-writing <file> <source dir> | spec-rewrite <file> <source dir> | state <file> <turn> <run dir> | answer <file> <answers file> | approve <file> | capture-spec <file> <run dir> | askgate <file> | capture-askgate <file> <run dir> | tester-proof <file> | implement-handoff <file> <body file> <commit> | capture-handoffs <file> <run dir> | owner-walk <file> <kind> <answer> | capture-scored <file> <run dir> | case-world | case-readback <run dir>";

/** Runs the mode `argv` names; false when argv names no mode, with its arguments. The first six
 * modes take extra arguments and ignore them, as they always did; each new mode takes exactly its
 * own. */
async function main(argv: string[]): Promise<boolean> {
  const [command, file, second, third] = argv;
  const n = argv.length;
  if (command === "project") await seedProject();
  else if (command === "ask-on-message" && file !== undefined) await askOnMessage(file);
  else if (command === "measure-before-ask" && file !== undefined) await measureBeforeAsk(file);
  else if (command === "capture" && file !== undefined && second !== undefined)
    await capture(file, second);
  else if (command === "brainstorm") await seedBrainstorm();
  else if (command === "capture-brainstorm" && second !== undefined)
    await captureBrainstorm(second);
  else if (command === "spec-writing" && file !== undefined && second !== undefined && n === 3)
    await specWriting(file, second);
  else if (command === "spec-rewrite" && file !== undefined && second !== undefined && n === 3)
    await specRewrite(file, second);
  else if (command === "state" && file !== undefined && third !== undefined && n === 4) {
    if (!/^[1-9][0-9]*$/.test(second ?? "")) return false;
    await state(file, Number(second), third);
  } else if (command === "answer" && file !== undefined && second !== undefined && n === 3)
    await answer(file, second);
  else if (command === "approve" && file !== undefined && n === 2) await approve(file);
  else if (command === "capture-spec" && file !== undefined && second !== undefined && n === 3)
    await captureSpec(file, second);
  else if (command === "askgate" && file !== undefined && n === 2) await seedAskgate(file);
  else if (command === "capture-askgate" && file !== undefined && second !== undefined && n === 3)
    await captureAskgate(file, second);
  else if (command === "tester-proof" && file !== undefined && n === 2)
    await testerProofIssue(file);
  else if (
    command === "implement-handoff" &&
    file !== undefined &&
    second !== undefined &&
    third !== undefined &&
    n === 4
  )
    await implementHandoff(file, second, third);
  else if (command === "capture-handoffs" && file !== undefined && second !== undefined && n === 3)
    await captureHandoffs(file, second);
  else if (
    command === "owner-walk" &&
    file !== undefined &&
    second !== undefined &&
    third !== undefined &&
    n === 4
  )
    await ownerWalkIssues(file, second, third);
  else if (command === "capture-scored" && file !== undefined && second !== undefined && n === 3)
    await captureScored(file, second);
  else if (command === "case-world" && n === 1 && process.env.SKILL_SCENARIO_RUN !== undefined)
    await seedCaseDispatch(process.env.SKILL_SCENARIO_RUN);
  else if (command === "case-readback" && n === 2 && file !== undefined)
    await captureCaseDispatch(file);
  else return false;
  return true;
}

if (import.meta.main) {
  for (const name of ["PLAYWRIGHT_BASE_URL", "E2E_AGENT_TOKEN"]) {
    if (process.env[name] !== undefined) {
      console.error(
        `seed.ts: ${name} is set; seed.ts seeds only rig.sh's scratch Dispatch (DISPATCH_E2E_PORT), so it sends nothing`
      );
      process.exit(2);
    }
  }
  if (!(await main(Bun.argv.slice(2)))) {
    console.error(USAGE);
    process.exit(2);
  }
}
