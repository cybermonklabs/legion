/**
 * Runs a fresh agent with skills/spec-statement-review on fixed change requests that a real
 * Dispatch server holds, and judges each answer against expected.json.
 *
 *   DISPATCH_TEST_DATABASE_URL=<postgres> bun scripts/spec-review-eval/run.ts [--runs N]
 *     [--omp <path>] [--model <model>] [--fixture <id>]... [--out <dir>]
 *
 * For each run, the command starts a Dispatch server of this checkout with DISPATCH_SPEC_ROOTS
 * naming the roots of expected.json, and seeds it over HTTP. It creates the project ACME, each
 * root with its terms.md, which the person person@example.com approves, and each fixture as the
 * spec of a child of its root, with a Verification document where the fixture has one. In both
 * documents, `@ISSUE@` becomes the key of the fixture's own issue. Where a fixture names an
 * `edit`, an agent session then applies those ops to the spec (`POST /api/v1/artifacts/{id}/edits`,
 * the route of `dispatch doc-edit`), so the agent reviews the version that Dispatch stored.
 * Before any agent runs, it reads `GET /api/v1/artifacts/{id}/spec-form` for each fixture
 * document. Each must be in scope, and its form check must give exactly the findings that the
 * fixture declares in `form`: most declare none, because a fixture holds the faults that no
 * program decides. A document out of scope also means that DISPATCH_SPEC_ROOTS did not reach the
 * server. A fixture that declares findings tests that the reviewer leaves them to Dispatch: a
 * finding of the reviewer at one of those texts fails the run.
 *
 * The agent reads the change request's own text and nothing else that tells its answer. In each
 * run each fixture gets an opaque handle, its place in a shuffle that a seed of the run decides.
 * Its issue's key and title, and its agent's directory, name the handle and never the fixture's
 * id, and each root's title names only its place. The ids, and the roots' own names in
 * expected.json, stay in this command and in --out, and no agent's directory is in --out. Before
 * the stack starts, and again with the stack's address, the command refuses a run where a place
 * that an agent reads holds a word of its fixture's id, of a kind that the fixture plants or of
 * its kind, or its root's own name: the titles of its issue and its root, the names of the
 * documents, its working directory, each environment variable, each file beside its working
 * directory, the branch of the checkout and the prompt.
 *
 * Each agent is `omp` with a profile and a home of its own, as scripts/approval-eval/fresh-agent.ts
 * spawns it, with the tools read and bash. Its PATH is its own `dispatch` script, which runs this
 * checkout's CLI against the server, and then the system's directories. The prompt names only the
 * skill, the issue and the document, so the agent runs `dispatch spec-form` and `dispatch
 * doc-read` itself, as the skill tells a reviewer to.
 *
 * A run passes when the answer names exactly the planted items of its fixture and the
 * transcript holds a `dispatch spec-form` call. An answer with a `NOT REVIEWED:` line, or with a
 * line out of the skill's form, is no verdict and fails the run. A fixture that declares
 * `"expect": "not-reviewed"` gives its agent a DISPATCH_URL that reaches no server, and its run
 * passes only on the skill's `NOT REVIEWED:` line for the failed `dispatch spec-form`: the
 * reviewer must say that it could not see, and never that it found nothing. The exit status is 0
 * when every run passes, 1 when one fails, and 2 when the command cannot run: a flag it refuses,
 * an --out that is no directory or is not empty, a stack that does not start, or any other error
 * before every run has its verdict.
 */
import {
  chmod,
  copyFile,
  mkdir,
  mkdtemp,
  readdir,
  readFile,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { parseArgs } from "node:util";
import {
  type DispatchStack,
  startDispatchStack,
} from "../../packages/envoy-client/test/dispatch-stack";
import { newSeed, randomStream } from "../approval-eval/corpus-io";
import { answerOf } from "../approval-eval/fresh-agent";

/** The seven kinds of the skill's output form, in the order the skill prints them. */
export const KINDS = [
  "principle",
  "undefined-noun",
  "other-meaning",
  "repeated-meaning",
  "wide-claim",
  "agent-choice",
  "agent-where-program",
] as const;

export type Kind = (typeof KINDS)[number];

/** One `FINDING` line. `subject` is null where the line writes `-`. */
export interface Finding {
  readonly kind: Kind;
  readonly node: string;
  readonly subject: string | null;
  readonly sentence: string;
}

/**
 * What an answer says. `not-reviewed` is a `NOT REVIEWED:` line, which says that a command failed
 * and no review was made. `unreadable` is an answer with no line of the skill's form. Neither is
 * a verdict.
 */
export type Answer =
  | { readonly verdict: "findings"; readonly findings: readonly Finding[] }
  | { readonly verdict: "none" }
  | { readonly verdict: "not-reviewed"; readonly reason: string }
  | { readonly verdict: "unreadable"; readonly reason: string };

/** An item of expected.json. A field it leaves out matches any value. */
export interface Item {
  readonly kind?: Kind;
  readonly node: string;
  readonly subject?: string;
}

/** A planted item names its kind. */
export interface PlantedItem extends Item {
  readonly kind: Kind;
}

/**
 * A root of The Spec, with the text of its terms.md. `title` is this command's own name of the
 * root, which says what its list holds; it never reaches an agent, which reads only the root's
 * place.
 */
export interface Root {
  readonly name: string;
  readonly key: string;
  readonly title: string;
  readonly terms: string;
  readonly termsText: string;
}

/** A fixture of expected.json, with the texts of its documents. */
export interface Fixture {
  readonly id: string;
  readonly root: string;
  readonly spec: string;
  readonly verification?: string;
  /** A file of edit ops, as `dispatch doc-edit --ops-json-file` takes them, for the spec. */
  readonly edit?: string;
  readonly editOps: readonly Readonly<Record<string, string>>[];
  readonly planted: readonly PlantedItem[];
  readonly mustNotName: readonly Item[];
  /** The findings that the form check must give on the spec, which the reviewer must not name. */
  readonly form: readonly FormFinding[];
  /** `review` by default; `not-reviewed` where the fixture's agent reaches no server. */
  readonly expect: "review" | "not-reviewed";
  readonly specText: string;
  readonly verificationText?: string;
}

/** A finding of the form check of Dispatch, by its kind and its node. */
export interface FormFinding {
  readonly kind: string;
  readonly node: string;
}

/** What formProblems reads of the server's answer to `GET /api/v1/artifacts/{id}/spec-form`. */
export interface FormRead {
  readonly in_scope: boolean;
  readonly scope: string;
  readonly findings: readonly FormFinding[] | null;
}

export interface Plan {
  readonly roots: readonly Root[];
  readonly fixtures: readonly Fixture[];
}

/** What compareFindings decides about one answer. */
export interface Comparison {
  readonly pass: boolean;
  readonly problems: readonly string[];
}

const HERE = import.meta.dir;
const REPO = resolve(HERE, "..", "..");
const SKILL = join(REPO, "skills", "spec-statement-review", "SKILL.md");
const DISPATCH_ENTRY = join(REPO, "packages", "envoy-client", "bin", "dispatch.ts");
const PROJECT = "ACME";
const PERSON = "person@example.com";
/** The model fresh-agent.ts runs on, which a caller with another route changes with --model. */
const DEFAULT_MODEL = "claude-opus-5-5";
const PROFILE = "fresh";
/** How long one agent run may take before it is killed and counted as a failed run. */
const RUN_TIMEOUT_MS = 600_000;
/** The placeholder a fixture's Verification document writes for the key of its own issue. */
const ISSUE_PLACEHOLDER = "@ISSUE@";
const ROOT_SPEC =
  "# A root of The Spec\n\nIts children hold the change requests that the reviewer reads.\n";
/** The DISPATCH_URL of a fixture whose reviewer cannot see: a loopback port that no server holds. */
const UNREACHABLE_DISPATCH = "http://127.0.0.1:1";
/** The skill's line for a failed first command, as its step 2 writes it. */
const FAILED_SPEC_FORM = /^dispatch spec-form\b.* failed: \S/;
/** The system's directories of commands, which follow an agent's own `dispatch` in its PATH. */
const SYSTEM_PATH = "/usr/local/bin:/usr/bin:/bin";
/** The names of the documents that the seed writes, as `dispatch read` lists them. */
const SPEC_DOCUMENT = "spec.md";
const VERIFICATION_DOCUMENT = "verification.md";
const TERMS_DOCUMENT = "terms.md";

const EXIT_PASSES = 0;
const EXIT_FAILS = 1;
const EXIT_CANNOT_RUN = 2;

const FINDING_LINE = /^FINDING (\S+) (\S+) (.+?): (.+)$/;
const NOT_REVIEWED = "NOT REVIEWED:";
/** A verdict's word, which no line holds after its own opening: `FINDING`, `NO FINDING` or `NOT REVIEWED`. */
const VERDICT_WORD = /\b(?:FINDING|NOT REVIEWED)\b/;

function isKind(value: string): value is Kind {
  return (KINDS as readonly string[]).includes(value);
}

/**
 * Reads an answer in the skill's output form. A `NOT REVIEWED:` line alone is no verdict; the
 * skill says to print it "and stop", so beside any other line it is an answer of two verdicts, or
 * of a verdict and prose, and unreadable. The skill says "no other text", so any other line, a
 * `FINDING` line out of the form, and `NO FINDING` beside a finding make the answer unreadable,
 * and so does an answer with neither a `FINDING` nor a `NO FINDING` line. The answer holds one
 * verdict in any layout: a verdict's word after the opening of a `FINDING` or a `NOT REVIEWED:`
 * line is a second verdict on that line, and makes the answer unreadable too.
 */
export function parseFindings(text: string): Answer {
  const lines = text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
  const notReviewed = lines.find((line) => line.startsWith(NOT_REVIEWED));
  if (notReviewed !== undefined) {
    const reason = notReviewed.slice(NOT_REVIEWED.length).trim();
    if (lines.length > 1) {
      return {
        verdict: "unreadable",
        reason: `a NOT REVIEWED line beside another line: ${notReviewed}`,
      };
    }
    return VERDICT_WORD.test(reason)
      ? {
          verdict: "unreadable",
          reason: `a second verdict on the NOT REVIEWED line: ${notReviewed}`,
        }
      : { verdict: "not-reviewed", reason };
  }
  const findings: Finding[] = [];
  let none = false;
  for (const line of lines) {
    if (line === "NO FINDING") {
      none = true;
      continue;
    }
    const match = FINDING_LINE.exec(line);
    const [, kind = "", node = "", subject = "", sentence = ""] = match ?? [];
    if (match === null || !isKind(kind)) {
      return { verdict: "unreadable", reason: `a line out of the skill's form: ${line}` };
    }
    if (VERDICT_WORD.test(line.slice("FINDING".length))) {
      return { verdict: "unreadable", reason: `a second verdict on a FINDING line: ${line}` };
    }
    findings.push({ kind, node, subject: subject === "-" ? null : subject, sentence });
  }
  if (none && findings.length > 0) {
    return { verdict: "unreadable", reason: "NO FINDING beside a FINDING line" };
  }
  if (none) return { verdict: "none" };
  if (findings.length === 0) {
    return { verdict: "unreadable", reason: "no FINDING line and no NO FINDING line" };
  }
  return { verdict: "findings", findings };
}

/** A subject as it compares: no code quotes or quotation marks, one space, lower case. */
function normal(subject: string): string {
  return subject
    .replace(/[`"'“”‘’]/g, "")
    .replace(/\s+/g, " ")
    .trim()
    .toLowerCase();
}

function matches(finding: Finding, item: Item): boolean {
  return (
    finding.node === item.node &&
    (item.kind === undefined || finding.kind === item.kind) &&
    (item.subject === undefined ||
      (finding.subject !== null && normal(finding.subject) === normal(item.subject)))
  );
}

function label(node: string, subject: string | null | undefined): string {
  return subject === null || subject === undefined ? node : `${node} ${subject}`;
}

/**
 * The text of a finding of the form check, as the reviewer names a text: a definition is a line
 * of the node `terms` with the term as its subject, and every other kind is the block of its node.
 */
function formItem(finding: FormFinding): Item {
  return finding.kind === "definition"
    ? { node: "terms", subject: finding.node }
    : { node: finding.node };
}

/**
 * Judges one answer against its fixture. The answer passes when it names each planted item and
 * nothing else: no item of `mustNotName`, and no text where the form check finds a fault, which
 * Dispatch decided already. An answer that is no verdict never passes, whatever the fixture
 * plants. A fixture whose reviewer cannot see is the other way round: only the `NOT REVIEWED:`
 * line of its failed `dispatch spec-form` passes.
 */
export function compareFindings(fixture: Fixture, answer: Answer): Comparison {
  if (fixture.expect === "not-reviewed") {
    const blind = "its dispatch reaches no server, so it could not see the request";
    if (answer.verdict === "not-reviewed") {
      return FAILED_SPEC_FORM.test(answer.reason)
        ? { pass: true, problems: [] }
        : {
            pass: false,
            problems: [`gave NOT REVIEWED with no failed dispatch spec-form: ${answer.reason}`],
          };
    }
    if (answer.verdict === "unreadable") {
      return { pass: false, problems: [`gave an unreadable answer: ${answer.reason}`] };
    }
    return {
      pass: false,
      problems: [
        answer.verdict === "none"
          ? `answered NO FINDING, but ${blind}`
          : `named findings, but ${blind}`,
      ],
    };
  }
  if (answer.verdict === "not-reviewed") {
    return { pass: false, problems: [`gave no verdict: ${answer.reason}`] };
  }
  if (answer.verdict === "unreadable") {
    return { pass: false, problems: [`gave an unreadable answer: ${answer.reason}`] };
  }
  const named = answer.verdict === "none" ? [] : answer.findings;
  const problems: string[] = [];
  for (const item of fixture.planted) {
    if (!named.some((finding) => matches(finding, item))) {
      problems.push(`missed the planted ${item.kind} ${label(item.node, item.subject)}`);
    }
  }
  for (const finding of named) {
    if (fixture.planted.some((item) => matches(finding, item))) continue;
    const where = label(finding.node, finding.subject);
    const decided = fixture.form.find((item) => matches(finding, formItem(item)));
    if (decided !== undefined) {
      problems.push(
        `named ${finding.kind} ${where}, where the form check finds ${decided.kind} ${decided.node}`
      );
      continue;
    }
    problems.push(
      fixture.mustNotName.some((item) => matches(finding, item))
        ? `named ${where}, which it must not name (${finding.kind})`
        : `named ${finding.kind} ${where}, which is not planted`
    );
  }
  return { pass: problems.length === 0, problems };
}

/** Whether the agent's JSON event stream holds a bash call that runs `dispatch spec-form`. */
export function specFormCalled(stdout: string): boolean {
  for (const line of stdout.split("\n")) {
    let event: unknown;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (typeof event !== "object" || event === null) continue;
    const { type, toolName, args } = event as {
      type?: unknown;
      toolName?: unknown;
      args?: unknown;
    };
    if (type !== "tool_execution_start" || toolName !== "bash") continue;
    const command = (args as { command?: unknown } | null)?.command;
    if (typeof command === "string" && /\bdispatch\s+spec-form\b/.test(command)) return true;
  }
  return false;
}

/**
 * The ids of the requirement and check blocks of a document, and of its entry lines of the how-to
 * of The Spec (`- **<statement>** `<id>` · under: …`), in their order, as specform.ExtractDocument
 * reads them.
 */
export function blockIds(markdown: string): string[] {
  return [
    ...markdown.matchAll(
      /^(?::{3,}(?:requirement|check)\{#([^\s}]+)|\s*[-*+] \*\*.+?\*\* `([^`]+)` · under:)/gm
    ),
  ].map((match) => match[1] ?? match[2] ?? "");
}

/**
 * The term names of the `- **term**: definition` lines under a heading `Terms`, with `-`, `*` or
 * `+`, read as specform.ExtractDocument reads them: the section ends at the next heading of its
 * level or above, and no line of a code fence counts, a heading among them. No line of a
 * requirement or a check block counts either: it runs to a line of at least as many colons alone,
 * and a term line in it adds no term. A line of another typed block counts as any other line.
 */
export function termNames(markdown: string): string[] {
  const names: string[] = [];
  let level = 0;
  let fence = "";
  let block = 0;
  for (const line of markdown.split("\n")) {
    const trimmed = line.trim();
    if (block > 0) {
      if (/^:+$/.test(trimmed) && trimmed.length >= block) block = 0;
      continue;
    }
    if (fence !== "") {
      if (trimmed.startsWith(fence) && /^([`~])\1*$/.test(trimmed)) fence = "";
      continue;
    }
    const opening = /^(`{3,}|~{3,})/.exec(trimmed);
    if (opening !== null) {
      fence = opening[1] ?? "";
      continue;
    }
    const typed = /^(:{3,})(?:requirement|check)\{/.exec(trimmed);
    if (typed !== null) {
      block = (typed[1] ?? "").length;
      continue;
    }
    const heading = /^(#{1,6})\s+(.+?)\s*#*\s*$/.exec(trimmed);
    if (heading !== null) {
      const depth = (heading[1] ?? "").length;
      if (level > 0 && depth <= level) level = 0;
      if (heading[2] === "Terms") level = depth;
      continue;
    }
    const term = level > 0 ? /^\s*[-*+] \*\*(.+?)\*\*: /.exec(line) : null;
    if (term !== null) names.push((term[1] ?? "").trim());
  }
  return names;
}

/** A document with the key of its fixture's own issue in each `serves`. */
export function withIssueKey(markdown: string, key: string): string {
  return markdown.replaceAll(ISSUE_PLACEHOLDER, key);
}

/**
 * The opaque handle of each fixture of one run: its place, from 1, in a shuffle of the ids that
 * the seed decides. An agent reads its fixture's handle, never its id.
 */
export function handlesOf(ids: readonly string[], seed: number): Map<string, number> {
  const order = [...ids];
  const next = randomStream(seed);
  for (let last = order.length - 1; last > 0; last -= 1) {
    const other = Math.floor(next() * (last + 1));
    [order[last], order[other]] = [order[other], order[last]];
  }
  return new Map(order.map((id, place) => [id, place + 1]));
}

/** One agent of a run: its fixture, its handle, and what the command gives it. */
export interface Agent {
  readonly fixture: Fixture;
  readonly handle: number;
  /** The key of its fixture's issue, which the seed of the server gives in the order of handles. */
  readonly issue: string;
  readonly title: string;
  readonly directory: string;
  readonly home: string;
  readonly bin: string;
  readonly work: string;
  readonly state: string;
  readonly session: string;
  readonly prompt: string;
}

/**
 * The agents of one run, in the order of their handles. An agent's directory is
 * <agents>/<handle>, and its issue's title is a plain sentence with the handle in it, as a real
 * change request can have.
 */
export function layoutOf(
  plan: Plan,
  fixtures: readonly Fixture[],
  seed: number,
  agents: string,
  skill: string
): Agent[] {
  const handles = handlesOf(
    fixtures.map((fixture) => fixture.id),
    seed
  );
  return fixtures
    .map((fixture) => {
      const handle = handles.get(fixture.id) ?? 0;
      const issue = `${PROJECT}-${plan.roots.length + handle}`;
      const directory = join(agents, String(handle));
      return {
        fixture,
        handle,
        issue,
        title: `Request ${handle} proposes statements for The Spec`,
        directory,
        home: join(directory, "home"),
        bin: join(directory, "bin"),
        work: join(directory, "work"),
        state: join(directory, "dispatch-state"),
        session: crypto.randomUUID(),
        prompt: `You review a change request. Read ${skill} and follow it for issue ${issue}, document ${SPEC_DOCUMENT}.`,
      };
    })
    .sort((one, other) => one.handle - other.handle);
}

/** The title of the root at a place of expected.json, from 1, which names only the place. */
function rootTitle(place: number): string {
  return `Part ${place} of The Spec`;
}

function rootOf(plan: Plan, fixture: Fixture): Root {
  const root = plan.roots.find((one) => one.name === fixture.root);
  if (root === undefined) throw new Error(`expected.json holds no root ${fixture.root}`);
  return root;
}

/** The text of an agent's `dispatch`, which runs this checkout's CLI with this command's bun. */
export function dispatchShim(bun: string, entry: string): string {
  return `#!/bin/sh\nexec '${bun}' '${entry}' "$@"\n`;
}

/**
 * The environment of an agent: its own home and state, a PATH of its own `dispatch` and the
 * system's directories, the stack's address and token, and the caller's TMPDIR and the two
 * variables that reach the model gateway. The caller's PATH stays out: it names the caller's own
 * tools, which can hold any word. Before the stack starts, `stack` is null, and only a fixture
 * whose reviewer cannot see has its DISPATCH_URL.
 */
export function environmentOf(
  agent: Agent,
  stack: { readonly url: string; readonly token: string } | null,
  caller: Readonly<Record<string, string | undefined>>
): Record<string, string> {
  const environment: Record<string, string> = {
    HOME: agent.home,
    PATH: `${agent.bin}:${SYSTEM_PATH}`,
    DISPATCH_HOST: "omp",
    DISPATCH_SESSION_ID: agent.session,
    DISPATCH_STATE_DIR: agent.state,
  };
  if (agent.fixture.expect === "not-reviewed") environment.DISPATCH_URL = UNREACHABLE_DISPATCH;
  else if (stack !== null) environment.DISPATCH_URL = stack.url;
  if (stack !== null) environment.DISPATCH_TOKEN = stack.token;
  for (const name of ["TMPDIR", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY"]) {
    const value = caller[name];
    if (value !== undefined) environment[name] = value;
  }
  return environment;
}

/** What one agent reads apart from the change request's own text, place by place. */
export interface Places {
  readonly title: string;
  readonly rootTitle: string;
  /** The names of its issue's documents and of its root's terms.md. */
  readonly documents: readonly string[];
  readonly workingDirectory: string;
  readonly environment: Readonly<Record<string, string>>;
  /** Each path beside its working directory, from its own directory: a file's text, or "". */
  readonly filesBeside: Readonly<Record<string, string>>;
  /** The branch of the checkout whose skill the prompt names, or null where it has none. */
  readonly branch: string | null;
  readonly prompt: string;
}

export function placesOf(
  agent: Agent,
  plan: Plan,
  environment: Readonly<Record<string, string>>,
  filesBeside: Readonly<Record<string, string>>,
  branch: string | null
): Places {
  return {
    title: agent.title,
    rootTitle: rootTitle(plan.roots.indexOf(rootOf(plan, agent.fixture)) + 1),
    documents: [
      SPEC_DOCUMENT,
      ...(agent.fixture.verificationText === undefined ? [] : [VERIFICATION_DOCUMENT]),
      TERMS_DOCUMENT,
    ],
    workingDirectory: agent.work,
    environment,
    filesBeside,
    branch,
    prompt: agent.prompt,
  };
}

/** The words of a text: its runs of letters, in lower case. */
function wordsOf(text: string): string[] {
  return text.toLowerCase().match(/\p{L}+/gu) ?? [];
}

/**
 * Each place of a fixture's agent that would tell it the fixture's answer: a place with a word
 * that starts with a word of the fixture's id, of a kind that it plants or of its kind, or with
 * its root's own name in expected.json. The kind `review` is left out: every prompt names that
 * task, so it tells no answer. A problem names an environment variable and never its value, which
 * can be a credential.
 */
export function leaks(fixture: Fixture, root: Root, places: Places): string[] {
  const kind = fixture.expect === "review" ? [] : [fixture.expect];
  const telling = [
    ...new Set([fixture.id, ...fixture.planted.map((item) => item.kind), ...kind].flatMap(wordsOf)),
  ];
  const own = root.title.toLowerCase();
  const read: (readonly [string, string])[] = [
    ["its issue's title", places.title],
    ["its root's title", places.rootTitle],
    ...places.documents.map((name) => [`the name of the document ${name}`, name] as const),
    ["its working directory", places.workingDirectory],
    ...Object.entries(places.environment).map(
      ([name, value]) => [`the variable ${name}`, `${name}=${value}`] as const
    ),
    ...Object.entries(places.filesBeside).map(
      ([path, text]) =>
        [`the file ${path} beside its working directory`, `${path}\n${text}`] as const
    ),
    ...(places.branch === null ? [] : [["the branch of the checkout", places.branch] as const]),
    ["its prompt", places.prompt],
  ];
  const problems: string[] = [];
  for (const [where, text] of read) {
    const words = wordsOf(text);
    for (const word of telling) {
      if (words.some((one) => one.startsWith(word))) {
        problems.push(`${fixture.id}: ${where} holds "${word}"`);
      }
    }
    if (text.toLowerCase().includes(own)) {
      problems.push(`${fixture.id}: ${where} holds the name of its root in expected.json`);
    }
  }
  return problems;
}

/** A fixture's `expect` in expected.json, `review` where it gives none. */
function expectOf(id: string, value: string | undefined): Fixture["expect"] {
  if (value === undefined || value === "review") return "review";
  if (value === "not-reviewed") return value;
  throw new Error(
    `expected.json: fixture ${id} expects ${value}, which is not review or not-reviewed`
  );
}

/** expected.json with the texts of its fixtures, read from the directory that holds them. */
export async function readPlan(directory: string = HERE): Promise<Plan> {
  const raw = JSON.parse(await readFile(join(directory, "expected.json"), "utf8")) as {
    roots: Omit<Root, "termsText">[];
    fixtures: (Omit<Fixture, "specText" | "verificationText" | "form" | "editOps" | "expect"> & {
      form?: FormFinding[];
      expect?: string;
    })[];
  };
  const fixtures = join(directory, "fixtures");
  const text = (name: string): Promise<string> => readFile(join(fixtures, name), "utf8");
  const roots = await Promise.all(
    raw.roots.map(async (root) => ({ ...root, termsText: await text(root.terms) }))
  );
  return {
    roots,
    fixtures: await Promise.all(
      raw.fixtures.map(async (fixture) => {
        if (!roots.some((root) => root.name === fixture.root)) {
          throw new Error(
            `expected.json: fixture ${fixture.id} names the root ${fixture.root}, which it does not hold`
          );
        }
        for (const item of fixture.planted) {
          if (!isKind(item.kind))
            throw new Error(`expected.json: fixture ${fixture.id} plants the kind ${item.kind}`);
        }
        return {
          ...fixture,
          form: fixture.form ?? [],
          editOps: fixture.edit === undefined ? [] : JSON.parse(await text(fixture.edit)),
          expect: expectOf(fixture.id, fixture.expect),
          specText: await text(fixture.spec),
          ...(fixture.verification === undefined
            ? {}
            : { verificationText: await text(fixture.verification) }),
        };
      })
    ),
  };
}

/** The command could not run, so no run says anything about the skill. */
class CannotRunError extends Error {}

interface Flags {
  readonly runs: number;
  readonly omp: string;
  readonly model: string;
  readonly fixtures: readonly string[];
  readonly out: string | undefined;
}

const USAGE =
  "usage: DISPATCH_TEST_DATABASE_URL=<postgres> bun scripts/spec-review-eval/run.ts [--runs N] [--omp <path>] [--model <model>] [--fixture <id>]... [--out <dir>]";

function readFlags(argv: readonly string[]): Flags {
  let values: { runs?: string; omp?: string; model?: string; fixture?: string[]; out?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: {
        runs: { type: "string" },
        omp: { type: "string" },
        model: { type: "string" },
        fixture: { type: "string", multiple: true },
        out: { type: "string" },
      },
      strict: true,
      allowPositionals: false,
    }));
  } catch (error) {
    throw new CannotRunError(`${error instanceof Error ? error.message : String(error)}\n${USAGE}`);
  }
  const runs = Number(values.runs ?? "1");
  if (!Number.isInteger(runs) || runs < 1)
    throw new CannotRunError(`--runs takes a whole number of 1 or more\n${USAGE}`);
  const omp = Bun.which(values.omp ?? "omp");
  if (omp === null) throw new CannotRunError(`no omp at ${values.omp ?? "omp"} on PATH`);
  return {
    runs,
    omp,
    model: values.model ?? DEFAULT_MODEL,
    fixtures: values.fixture ?? [],
    out: values.out,
  };
}

/** The server's answer to one request, or a CannotRunError that names the request and the answer. */
async function call<T>(
  stack: DispatchStack,
  method: string,
  route: string,
  body: unknown,
  as: "person" | "agent"
): Promise<T> {
  const response = await fetch(`${stack.url}${route}`, {
    method,
    headers: {
      "Content-Type": "application/json",
      ...(as === "person"
        ? { "X-Dispatch-User": PERSON }
        : { Authorization: `Bearer ${stack.token}` }),
    },
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
  });
  const text = await response.text();
  if (!response.ok) throw new CannotRunError(`${method} ${route}: ${response.status} ${text}`);
  return JSON.parse(text) as T;
}

/** One document of a seeded fixture. */
interface SeededDocument {
  readonly name: string;
  readonly artifact: string;
}

interface Seeded {
  readonly fixture: Fixture;
  readonly documents: readonly SeededDocument[];
}

/**
 * The project, the roots with their approved terms, and one child issue for each agent's fixture.
 * Keys come in order on the stack's own database, and the agents come in the order of their
 * handles, so each issue gets the key of its agent, which its spec names in place of `@ISSUE@`;
 * the seed refuses a key that is not that one. A root's title names only its place.
 */
async function seed(stack: DispatchStack, plan: Plan, agents: readonly Agent[]): Promise<Seeded[]> {
  await call(stack, "POST", "/api/v1/projects", { key: PROJECT, name: PROJECT }, "person");
  const rootKeys = new Map<string, string>();
  for (const [index, root] of plan.roots.entries()) {
    const issue = await call<{ key: string }>(
      stack,
      "POST",
      "/api/v1/issues",
      { project: PROJECT, title: rootTitle(index + 1), force: true, spec: ROOT_SPEC },
      "person"
    );
    if (issue.key !== root.key) {
      throw new CannotRunError(
        `the root ${root.name} got the key ${issue.key}, and DISPATCH_SPEC_ROOTS names ${root.key}`
      );
    }
    const terms = await call<{ artifact: { id: string } }>(
      stack,
      "POST",
      `/api/v1/issues/${issue.key}/artifacts`,
      { name: TERMS_DOCUMENT, content: root.termsText },
      "person"
    );
    await call(
      stack,
      "POST",
      `/api/v1/artifacts/${terms.artifact.id}/reviews`,
      { state: "approved" },
      "person"
    );
    rootKeys.set(root.name, issue.key);
  }
  const seeded: Seeded[] = [];
  for (const { fixture, issue: key, title } of agents) {
    const issue = await call<{ key: string; primary_artifact_id: string }>(
      stack,
      "POST",
      "/api/v1/issues",
      {
        project: PROJECT,
        title,
        parent: rootKeys.get(fixture.root),
        force: true,
        spec: withIssueKey(fixture.specText, key),
      },
      "person"
    );
    if (issue.key !== key) {
      throw new CannotRunError(
        `the fixture ${fixture.id} got the key ${issue.key}, and its spec names ${key}`
      );
    }
    if (fixture.editOps.length > 0) {
      await call(
        stack,
        "POST",
        `/api/v1/artifacts/${issue.primary_artifact_id}/edits`,
        { ops: fixture.editOps, actor: { kind: "session", id: crypto.randomUUID() } },
        "agent"
      );
    }
    const documents: SeededDocument[] = [
      { name: SPEC_DOCUMENT, artifact: issue.primary_artifact_id },
    ];
    if (fixture.verificationText !== undefined) {
      const verification = await call<{ artifact: { id: string } }>(
        stack,
        "POST",
        `/api/v1/issues/${issue.key}/artifacts`,
        {
          name: VERIFICATION_DOCUMENT,
          content: withIssueKey(fixture.verificationText, issue.key),
        },
        "person"
      );
      documents.push({ name: VERIFICATION_DOCUMENT, artifact: verification.artifact.id });
    }
    seeded.push({ fixture, documents });
  }
  return seeded;
}

/**
 * What is wrong with the form check's read of one seeded document: it is out of scope, or its
 * findings are not exactly the ones that its fixture declares. A fixture declares the findings of
 * its spec only, so every other document must hold none.
 */
export function formProblems(fixture: Fixture, document: string, read: FormRead): string[] {
  const where = `${fixture.id} ${document}`;
  if (!read.in_scope) {
    return [
      `${where} is out of scope (${read.scope}), so DISPATCH_SPEC_ROOTS did not reach the server`,
    ];
  }
  const declared = document === SPEC_DOCUMENT ? fixture.form : [];
  const found = read.findings ?? [];
  return [
    ...declared
      .filter((one) => !found.some((other) => other.kind === one.kind && other.node === one.node))
      .map(
        (one) =>
          `the form check finds no ${one.kind} ${one.node} in ${where}, which the fixture declares`
      ),
    ...found
      .filter(
        (one) => !declared.some((other) => other.kind === one.kind && other.node === one.node)
      )
      .map(
        (one) =>
          `the form check finds ${one.kind} ${one.node} in ${where}, which the fixture does not declare`
      ),
  ];
}

/**
 * Refuses the run unless the form check of Dispatch reads each fixture document in scope with
 * exactly the findings that its fixture declares: a fixture holds what no program decides, and
 * the findings it declares are the texts that the reviewer must leave to Dispatch.
 */
async function checkForm(stack: DispatchStack, seeded: readonly Seeded[]): Promise<void> {
  const problems: string[] = [];
  for (const one of seeded) {
    for (const document of one.documents) {
      const read = await call<FormRead>(
        stack,
        "GET",
        `/api/v1/artifacts/${document.artifact}/spec-form`,
        undefined,
        "agent"
      );
      problems.push(...formProblems(one.fixture, document.name, read));
    }
  }
  if (problems.length > 0) throw new CannotRunError(problems.join("\n"));
}

/** The models file of the profile this command runs under, or null where it has none. */
async function modelsFileOf(omp: string): Promise<string | null> {
  const child = Bun.spawn({ cmd: [omp, "config", "path"], stdout: "pipe", stderr: "ignore" });
  const [printed, code] = await Promise.all([new Response(child.stdout).text(), child.exited]);
  if (code !== 0) throw new CannotRunError(`${omp} config path exited ${code}`);
  const path = join(printed.trim(), "models.yml");
  return (await Bun.file(path).exists()) ? path : null;
}

/** What one agent run returned. */
interface Run {
  readonly text: string;
  readonly model: string | null;
  readonly stdout: string;
  readonly code: number;
}

/** Makes an agent's directories, its omp profile and its `dispatch`, before the guard reads them. */
async function writeAgent(agent: Agent, models: string | null): Promise<void> {
  const profile = join(agent.home, ".omp", "profiles", PROFILE, "agent");
  for (const path of [profile, agent.bin, agent.work, agent.state]) {
    await mkdir(path, { recursive: true });
  }
  await writeFile(join(profile, "config.yml"), "skills:\n  customDirectories: []\n", "utf8");
  if (models !== null) await copyFile(models, join(profile, "models.yml"));
  const dispatch = join(agent.bin, "dispatch");
  await writeFile(dispatch, dispatchShim(process.execPath, DISPATCH_ENTRY), "utf8");
  await chmod(dispatch, 0o755);
}

/**
 * What an agent finds beside its working directory: the entries of the directory of its run's
 * agents, of its own directory and of each directory in it, with the text of each file there.
 * Deeper, its home holds omp's profile, whose path omp fixes and which is the same for every
 * fixture.
 */
async function besideOf(agent: Agent): Promise<Record<string, string>> {
  const found: Record<string, string> = {};
  for (const name of await readdir(dirname(agent.directory))) found[`../${name}`] = "";
  const entries = (await readdir(agent.directory, { withFileTypes: true })).map(
    (entry) => [entry.name, entry] as const
  );
  for (const [, entry] of [...entries]) {
    if (!entry.isDirectory()) continue;
    for (const inner of await readdir(join(agent.directory, entry.name), { withFileTypes: true })) {
      entries.push([join(entry.name, inner.name), inner]);
    }
  }
  for (const [path, entry] of entries) {
    found[path] = entry.isFile() ? await readFile(join(agent.directory, path), "utf8") : "";
  }
  return found;
}

/** The branch of the checkout at repo, or null where git finds no checkout or no branch there. */
function branchOf(repo: string): string | null {
  try {
    const result = Bun.spawnSync({
      cmd: ["git", "-C", repo, "branch", "--show-current"],
      stdout: "pipe",
      stderr: "ignore",
    });
    const name = result.success ? result.stdout.toString().trim() : "";
    return name === "" ? null : name;
  } catch {
    return null;
  }
}

/**
 * Refuses the run when a place that an agent reads would tell it its fixture's answer, so that a
 * run measures the skill and not a hint.
 */
async function refuseLeaks(
  plan: Plan,
  agents: readonly Agent[],
  stack: DispatchStack | null,
  branch: string | null
): Promise<void> {
  const problems: string[] = [];
  for (const agent of agents) {
    const environment = environmentOf(agent, stack, process.env);
    const places = placesOf(agent, plan, environment, await besideOf(agent), branch);
    problems.push(...leaks(agent.fixture, rootOf(plan, agent.fixture), places));
  }
  if (problems.length > 0) {
    throw new CannotRunError(
      `the command refuses the run: what an agent reads names its fixture's answer\n${problems.join("\n")}`
    );
  }
}

/**
 * One fresh agent on one fixture, in the directory that writeAgent made, with the environment of
 * environmentOf. Its transcript and its stderr go to records, which is in --out.
 */
async function review(
  flags: Flags,
  stack: DispatchStack,
  agent: Agent,
  records: string
): Promise<Run> {
  const child = Bun.spawn({
    cmd: [
      flags.omp,
      "--profile",
      PROFILE,
      "--cwd",
      agent.work,
      "--model",
      flags.model,
      "--tools",
      "read,bash",
      "--no-skills",
      "--no-extensions",
      "--no-rules",
      "--no-context-files",
      "--no-session",
      "--no-title",
      "--mode",
      "json",
      "-p",
      agent.prompt,
    ],
    cwd: agent.work,
    env: environmentOf(agent, stack, process.env),
    stdin: "ignore",
    stdout: "pipe",
    stderr: "pipe",
    timeout: RUN_TIMEOUT_MS,
  });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  await mkdir(records, { recursive: true });
  await writeFile(join(records, "events.jsonl"), stdout, "utf8");
  await writeFile(join(records, "stderr.txt"), stderr, "utf8");
  return { ...answerOf(stdout), stdout, code };
}

async function evaluate(flags: Flags): Promise<number> {
  const plan = await readPlan();
  const unknown = flags.fixtures.filter(
    (id) => !plan.fixtures.some((fixture) => fixture.id === id)
  );
  if (unknown.length > 0)
    throw new CannotRunError(`expected.json holds no fixture ${unknown.join(", ")}`);
  const fixtures = plan.fixtures.filter(
    (fixture) => flags.fixtures.length === 0 || flags.fixtures.includes(fixture.id)
  );
  if (flags.out !== undefined) {
    const earlier = await readdir(flags.out).catch((error: NodeJS.ErrnoException) => {
      if (error.code === "ENOENT") return [];
      if (error.code === "ENOTDIR")
        throw new CannotRunError(`--out ${flags.out} is not a directory`);
      throw error;
    });
    if (earlier.length > 0) {
      const shown = earlier.sort().slice(0, 3).join(", ");
      const more = earlier.length > 3 ? ` and ${earlier.length - 3} more` : "";
      throw new CannotRunError(
        `--out ${flags.out} is not empty: it holds ${shown}${more}; give a directory that is new or empty`
      );
    }
  }
  const out = flags.out ?? (await mkdtemp(join(tmpdir(), "spec-review-eval-")));
  await mkdir(out, { recursive: true });
  const models = await modelsFileOf(flags.omp);
  const branch = branchOf(REPO);
  const firstSeed = newSeed();
  const results: string[] = [];
  let failed = 0;
  for (let run = 1; run <= flags.runs; run += 1) {
    const shuffle = (firstSeed + run) >>> 0;
    // Outside --out, whose path and whose other entries name the fixtures.
    const agentsDirectory = await mkdtemp(join(tmpdir(), "x"));
    try {
      const agents = layoutOf(plan, fixtures, shuffle, agentsDirectory, SKILL);
      for (const agent of agents) await writeAgent(agent, models);
      await refuseLeaks(plan, agents, null, branch);
      const stackDirectory = join(out, `run-${run}`, "stack");
      await mkdir(stackDirectory, { recursive: true });
      const stack = await startDispatchStack({
        work: stackDirectory,
        env: { DISPATCH_SPEC_ROOTS: plan.roots.map((root) => root.key).join(",") },
      });
      try {
        await refuseLeaks(plan, agents, stack, branch);
        await checkForm(stack, await seed(stack, plan, agents));
        for (const fixture of fixtures) {
          const agent = agents.find((one) => one.fixture === fixture);
          if (agent === undefined) throw new Error(`no agent of ${fixture.id}`);
          const answer = await review(flags, stack, agent, join(out, `run-${run}`, fixture.id));
          const comparison = compareFindings(fixture, parseFindings(answer.text));
          const problems = [
            ...(answer.code === 0 ? [] : [`the agent exited ${answer.code}`]),
            ...comparison.problems,
            ...(specFormCalled(answer.stdout)
              ? []
              : ["the transcript holds no dispatch spec-form call"]),
          ];
          const pass = problems.length === 0;
          if (!pass) failed += 1;
          const line = `${pass ? "PASS" : "FAIL"} run ${run} ${fixture.id} (${agent.issue}, ${answer.model ?? "no model"})${pass ? "" : `: ${problems.join("; ")}`}`;
          console.log(line);
          console.log(answer.text.replace(/^/gm, "    "));
          results.push(
            JSON.stringify({
              run,
              seed: shuffle,
              handle: agent.handle,
              fixture: fixture.id,
              issue: agent.issue,
              model: answer.model,
              pass,
              problems,
              answer: answer.text,
            })
          );
        }
      } finally {
        await stack.stop();
      }
    } finally {
      await rm(agentsDirectory, { recursive: true, force: true });
    }
  }
  await writeFile(join(out, "results.jsonl"), `${results.join("\n")}\n`, "utf8");
  const total = flags.runs * fixtures.length;
  console.log(
    `${total - failed} of ${total} runs passed; transcripts and results.jsonl are in ${out}`
  );
  return failed === 0 ? EXIT_PASSES : EXIT_FAILS;
}

/**
 * Each error that ends the command, of the command or of the stack, the database and the server
 * under it, is the exit status 2: no run of it has a verdict that the status could report, and 1
 * would tell the reader that a reviewer failed.
 */
async function main(argv: readonly string[]): Promise<number> {
  try {
    return await evaluate(readFlags(argv));
  } catch (error) {
    console.error(
      error instanceof CannotRunError
        ? error.message
        : error instanceof Error
          ? (error.stack ?? error.message)
          : String(error)
    );
    return EXIT_CANNOT_RUN;
  }
}

// Only when run: a test that imports parseFindings must not start a server.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
