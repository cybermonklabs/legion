import type { Page } from "@playwright/test";

import type { IssueTreeRead } from "../web/src/api/types";
import {
  answerAsk,
  createAsk,
  createComment,
  createIssue,
  createIssueArtifact,
  createMessage,
  createProject,
  editArtifact,
  getIssue,
  patchIssue,
  resolveAsk,
} from "./api";
import {
  asPlanner,
  PLANNER,
  PROJECT,
  pause,
  REVIEWER,
  seedSessions,
  TESTER,
} from "./issue-layout-seed-support";
import {
  checkBlockId,
  decisionBlockId,
  planTaskBlockId,
  requirementBlockId,
  specBlockRef,
} from "./issue-layout-seed-text";
import { type Gate, OWED_BY, TREE_PATH } from "./issue-page-t-tree";

/**
 * An issue at the shape the largest issue in production had when its load was measured, written
 * through the routes a person and an agent write through, as the seeds of
 * `e2e/issue-layout-seed.ts` and `e2e/issue-page-t-pages.e2e.ts` write theirs, and how a row reads
 * the first screen of the page that draws it.
 *
 * The shape (`LARGEST_SHAPE`): a spec of 42 statements, 11 questions, 170 paragraphs, 14 headings
 * and 4 tables, and a `## Decisions` part of 30 decisions; a Verification document of 49 checks,
 * each serving one to seven statements (one serves seven, and the last three statements are served
 * by none); a plan of 26 tasks, each serving statements, most needing the task before; 37
 * children in mixed statuses; 34 asks, 24 answered and 10 resolved, none left open; 188 comments,
 * which are 67 posts by two people and three agents, 120 replies under them (one thread of 20,
 * eight of 5 to 10, and most posts with one reply or none) and one comment on a statement's text;
 * and 12 messages. A post holds
 * three to ten lines of markdown with a list, links to a child and to a statement's address and
 * inline code; every tenth holds a fenced code block wider than the column and every twentieth a
 * table, since what a row costs depends on the elements it draws. 27 of the statements name, as
 * their `parent`, the ask or the post their `From:` line cites first, in production's proportion
 * (`ORIGINS`), and the other 15 none. `addWaitingAsk` adds the one ask the second variant holds,
 * which waits on the reader.
 *
 * The writes that do not depend on each other go out `BURST` at a time, so the seed stays short:
 * the children, the asks, and the posts, each post with its replies written after it. Each burst
 * waits for the one before, so the posts stand in the order of their bursts, and a post's replies
 * are newer than the post. The documents are written one at a time, since the server queues the
 * edits of one document.
 */

/** The counts of the largest issue's shape, as production held it on 2026-11-26. */
export const LARGEST_SHAPE = {
  answeredAsks: 24,
  asks: 34,
  checks: 49,
  children: 37,
  decisions: 30,
  headings: 14,
  messages: 12,
  paragraphs: 170,
  planTasks: 26,
  posts: 67,
  replies: 120,
  textComments: 1,
  questions: 11,
  requirements: 42,
  resolvedAsks: 10,
  tables: 4,
} as const;

/** How many writes go out at once. */
const BURST = 6;

/** How many statements each heading of the spec holds: 42 over 14 headings. */
const STATEMENTS_PER_HEADING = LARGEST_SHAPE.requirements / LARGEST_SHAPE.headings;

/** The statements the last checks leave unserved: the last three. */
const SERVED_STATEMENTS = LARGEST_SHAPE.requirements - 3;

/** The sections of the spec a table stands in, by their place. */
const TABLE_SECTIONS: ReadonlySet<number> = new Set([3, 6, 9, 12]);

/** The question of the ask that waits on the reader in the second variant. */
export const WAITING_QUESTION = "Does the children's section still open expanded at this size?";

/** What `seedLargestShape` wrote, each item by the id the page draws its row by. */
export interface LargestShapeSeed {
  readonly children: readonly string[];
  readonly key: string;
  readonly messages: readonly string[];
  /** By statement block id, the address of the ask or post its `From:` line cites (`ORIGINS`). */
  readonly origins: Readonly<Record<string, string>>;
  readonly planArtifactId: string;
  /** The 67 posts, comments tied to no text, in the order of their bursts. */
  readonly posts: readonly string[];
  readonly replies: readonly string[];
  /** The one comment on a statement's text, which the margin draws and no row does. */
  readonly textComment: string;
  /** How long the seed took, in ms. */
  readonly seedMs: number;
  readonly specArtifactId: string;
  readonly verificationArtifactId: string;
}

/** Who writes the posts and the replies, in turn: two people and three agents. */
const WRITERS = [
  {},
  asPlanner,
  { login: "bob" },
  { actor: { id: REVIEWER, kind: "session" as const }, as: "agent" as const },
  { actor: { id: TESTER, kind: "session" as const }, as: "agent" as const },
] as const;

/** The agents that ask the asks, in turn. */
const ASKERS = [WRITERS[1], WRITERS[3], WRITERS[4]] as const;

const SUBJECTS = [
  "The tree read",
  "The children's section",
  "The outline",
  "Each page of comments",
  "The view row",
  "The Contents pane",
  "A folded branch",
  "The Verification filter",
  "The plan's chart",
  "The first load",
  "The reply line",
] as const;

const VERBS = ["draws", "pages", "reads", "holds", "keeps", "orders", "counts", "names"] as const;

const OBJECTS = [
  "every node the answer holds, in the order the read gives",
  "the 200 newest comments and the position of the oldest",
  "the first 50 children of a node and a control for the rest",
  "one row for each statement, with its checks under it",
  "the questions that wait on the reader before anything else",
  "each reply under the post it answers",
  "the next page only when the reader presses for it",
  "a child's status, what blocks it and who holds it",
  "the words of a row whole, with no cut at six lines",
] as const;

const CODE = [
  "useIssueTree",
  "treeRows",
  "mergeTreePages",
  "comments_before",
  "pathTo",
  "RowText.tsx",
  "?kind=comment",
  "cursor",
  "IssueChildren.tsx",
] as const;

const HEADINGS = [
  "What the page reads",
  "The outline",
  "The children",
  "The questions",
  "The statements and their checks",
  "The plan",
  "The decisions",
  "The discussion",
  "Paging",
  "Widths",
  "Keys",
  "Addresses",
  "Writes",
  "What this leaves out",
] as const;

/** One sentence of words like a spec's or a comment's, the `n`th of a fixed run. */
function sentence(n: number): string {
  const subject = SUBJECTS[n % SUBJECTS.length] ?? "";
  const verb = VERBS[(n * 3) % VERBS.length] ?? "";
  const object = OBJECTS[(n * 5) % OBJECTS.length] ?? "";
  return `${subject} ${verb} ${object}`;
}

/** A name of code, the `n`th of a fixed run. */
function code(n: number): string {
  return CODE[n % CODE.length] ?? "";
}

/** `1` to `count`. */
function counted(count: number): number[] {
  return Array.from({ length: count }, (_, index) => index + 1);
}

/** Runs `write` on each of `items`, `BURST` at a time, each burst after the one before. */
async function inBursts<T, R>(
  items: readonly T[],
  write: (item: T, index: number) => Promise<R>
): Promise<R[]> {
  const written: R[] = [];
  for (let start = 0; start < items.length; start += BURST) {
    const burst = items.slice(start, start + BURST);
    written.push(...(await Promise.all(burst.map((item, at) => write(item, start + at)))));
  }
  return written;
}

/** A paragraph of the spec: two to four sentences, one naming code. */
function paragraph(n: number): string {
  const sentences = counted(2 + (n % 3)).map((at) => `${sentence(n + at * 7)}.`);
  sentences.splice(1, 0, `It reads \`${code(n)}\` once per load.`);
  return sentences.join(" ");
}

/** A table of four columns and four rows. */
function table(n: number): string {
  const line = (cells: readonly string[]) => `| ${cells.join(" | ")} |`;
  const rows = counted(4).map((row) =>
    line([`Row ${n}.${row}`, `${row * 20} px`, code(n + row), `${row * 3} reads`])
  );
  return [
    line(["Case", "Width", "Code", "Reads"]),
    line(["---", "---", "---", "---"]),
    ...rows,
  ].join("\n");
}

/** Statement `n`'s words: one or two sentences. */
function statementText(n: number): string {
  const second = n % 2 === 0 ? ` ${sentence(n + 3)} at every width.` : "";
  return `${sentence(n)}, whatever the size of the issue.${second}`;
}

/** Where the largest issue's statements came from, by statement: the ask or the post its `From:`
 *  line cites first, by its place among the seed's own asks or posts, as production's statements
 *  cite theirs (15 an ask, 12 a comment, 20 nodes in all: one ask cited by 4, one by 3, two by 2,
 *  the rest by 1). A statement this names nothing for cites none and stands under the issue. Each
 *  post named is one a person wrote (every fifth post is alice's, and the second of each five
 *  bob's: `WRITERS`), since the model takes a comment as a node of the conversation only where a
 *  person wrote it or wrote in its thread, and refuses a `parent` that names another. */
const ORIGINS: Readonly<Record<number, { readonly ask: number } | { readonly post: number }>> = {
  1: { post: 2 },
  2: { ask: 1 },
  5: { ask: 1 },
  6: { ask: 3 },
  8: { post: 5 },
  9: { ask: 3 },
  10: { ask: 5 },
  11: { ask: 3 },
  12: { ask: 8 },
  13: { ask: 5 },
  16: { ask: 5 },
  17: { ask: 5 },
  22: { post: 10 },
  23: { post: 12 },
  24: { post: 15 },
  25: { post: 20 },
  26: { post: 25 },
  30: { post: 30 },
  33: { ask: 11 },
  34: { ask: 14 },
  35: { ask: 14 },
  37: { post: 35 },
  38: { ask: 17 },
  39: { ask: 20 },
  40: { post: 40 },
  41: { post: 45 },
  42: { post: 50 },
};

/** The spec a person writes: 14 headings, each over twelve or thirteen paragraphs, and a table
 *  under four of them. Its statements are written once the discussion they came from is
 *  (`statementOps`); the questions and the decisions are the planner's (`questionsOps`,
 *  `decisionsPart`). */
function specText(): string {
  const parts: string[] = [];
  let paragraphs = 0;
  for (const section of counted(LARGEST_SHAPE.headings)) {
    parts.push(`## ${HEADINGS[section - 1] ?? `Part ${section}`}`);
    const here = section > LARGEST_SHAPE.headings - 2 ? 13 : 12;
    for (const at of counted(here)) {
      paragraphs += 1;
      parts.push(paragraph(paragraphs));
      if (at === 1 && TABLE_SECTIONS.has(section)) parts.push(table(section));
    }
  }
  return `${parts.join("\n\n")}\n`;
}

/** The 42 statements, three under each heading, each whose `From:` line cites an ask or a post
 *  (`ORIGINS`) carrying that node as its `parent`, so the model places it under that node. Answers
 *  the ops and, by statement block id, the address each statement names. */
function statementOps(
  key: string,
  asks: readonly string[],
  posts: readonly string[]
): { origins: Record<string, string>; ops: { after: string; markdown: string; op: "insert" }[] } {
  const origins: Record<string, string> = {};
  const ops = counted(LARGEST_SHAPE.headings).map((section) => {
    const blocks = counted(STATEMENTS_PER_HEADING).map((at) => {
      const n = (section - 1) * STATEMENTS_PER_HEADING + at;
      const origin = ORIGINS[n];
      const address =
        origin === undefined
          ? undefined
          : "ask" in origin
            ? `dispatch://${key}/ask/${asks[origin.ask - 1] ?? ""}`
            : `dispatch://${key}/comment/${posts[origin.post - 1] ?? ""}`;
      if (address !== undefined) origins[requirementBlockId(n)] = address;
      const parent = address === undefined ? "" : ` parent="${address}"`;
      return `:::requirement{#${requirementBlockId(n)}${parent}}\n${statementText(n)}\n:::`;
    });
    return {
      after: `heading:${HEADINGS[section - 1] ?? `Part ${section}`}`,
      markdown: blocks.join("\n\n"),
      op: "insert" as const,
    };
  });
  return { origins, ops };
}

/** The block id of question `n` of the spec. */
function questionBlockId(n: number): string {
  return `q${n}`;
}

/** The planner's 11 questions, each inserted after the first statement under one heading. */
function questionsOps(): { after: string; markdown: string; op: "insert" }[] {
  return counted(LARGEST_SHAPE.questions).map((n) => ({
    after: `block:${requirementBlockId((n - 1) * STATEMENTS_PER_HEADING + 1)}`,
    markdown:
      `:::ask{#${questionBlockId(n)} urgency="med"}\n` +
      `Does ${sentence(n * 2).toLowerCase()} on every issue?\n\n` +
      `- Yes: ${sentence(n + 1).toLowerCase()}.\n- No: the page pages it instead.\n:::`,
    op: "insert",
  }));
}

/** The planner's `## Decisions` part: 30 decisions, two in three shaping a statement, every
 *  seventh unsure. */
function decisionsPart(key: string): string {
  const blocks = counted(LARGEST_SHAPE.decisions).map((n) => {
    const shaped =
      n % 3 === 0
        ? ""
        : ` shaped="${specBlockRef(key, requirementBlockId(((n * 7) % LARGEST_SHAPE.requirements) + 1))}"`;
    const unsure = n % 7 === 0 ? ' uncertainty="high"' : "";
    return (
      `:::decision{#${decisionBlockId(n)}${shaped}${unsure}}\n### Decision ${n}: ${sentence(n * 5)}\n\n` +
      `${sentence(n + 2)}, since ${sentence(n + 4).toLowerCase()}. It changes \`${code(n)}\`.\n:::`
    );
  });
  return `## Decisions\n\n${blocks.join("\n\n")}\n`;
}

/** How many statements check `n` serves: seven for the first, one to three for the rest, and
 *  five for every eleventh. */
function servedBy(n: number): number {
  if (n === 1) return 7;
  return n % 11 === 0 ? 5 : 1 + (n % 3);
}

/** The Verification document: 49 checks, each serving a run of the first 39 statements. */
function verificationText(key: string): string {
  const blocks = counted(LARGEST_SHAPE.checks).map((n) => {
    const first = n === 1 ? 1 : ((n * 5) % (SERVED_STATEMENTS - servedBy(n))) + 1;
    const serves = counted(servedBy(n))
      .map((at) => specBlockRef(key, requirementBlockId(first + at - 1)))
      .join(" ");
    return (
      `:::check{#${checkBlockId(n)} serves="${serves}" surface="browser"` +
      ` run="bun run e2e -- e2e/issue-page-t-scale.e2e.ts" when="before_merge"` +
      ` negative_control="Draw the page without its paging and the test fails."}\n` +
      `Check ${n}: ${sentence(n + 9).toLowerCase()}, read on the page as it opens.\n:::`
    );
  });
  return `How the page at scale is proved.\n\n${blocks.join("\n\n")}\n`;
}

/** The plan: 26 tasks, each serving one to three statements, three in four needing the task
 *  before them. */
function planText(key: string): string {
  const taskRef = (n: number) => `dispatch://${key}/artifact/plan-md#b-${planTaskBlockId(n)}`;
  const blocks = counted(LARGEST_SHAPE.planTasks).map((n) => {
    const first = ((n * 3) % (LARGEST_SHAPE.requirements - 3)) + 1;
    const serves = counted(1 + (n % 3))
      .map((at) => specBlockRef(key, requirementBlockId(first + at - 1)))
      .join(" ");
    const needs = n > 1 && n % 4 !== 1 ? ` needs="${taskRef(n - 1)}"` : "";
    return (
      `:::plan_task{#${planTaskBlockId(n)} serves="${serves}"${needs}}\n` +
      `Task ${n}: ${sentence(n + 13).toLowerCase()}.\n:::`
    );
  });
  return `How the page at scale gets built.\n\n${blocks.join("\n\n")}\n`;
}

/** The statuses of the 37 children, in turn: most done, the rest at every stage before. */
const CHILD_STATUSES = [
  "done",
  "in_progress",
  "done",
  "todo",
  "done",
  "backlog",
  "needs_review",
  "done",
  "testing",
] as const;

/** Which children wait on which, each by its place in the order written: an open child waiting
 *  on another open one, since the server refuses a write to a closed issue. */
const BLOCKED_CHILDREN = [
  [5, 6],
  [10, 12],
  [15, 17],
  [21, 19],
  [28, 24],
] as const;

/** A post's words: three to ten lines of markdown with a list, a link to a child and to a
 *  statement's address and inline code; every tenth adds a fenced code block wider than the
 *  column, and every twentieth a table. */
function postBody(n: number, key: string, children: readonly string[]): string {
  const child = children[n % children.length] ?? key;
  const other = children[(n * 7) % children.length] ?? key;
  const statement = specBlockRef(key, requirementBlockId((n % LARGEST_SHAPE.requirements) + 1));
  const lines = [
    `${sentence(n)}, as \`${code(n)}\` does on dispatch://${child}.`,
    "",
    ...counted(1 + (n % 4)).map((at) =>
      at === 2
        ? `- See [${other}](dispatch://${other}) and ${statement} for where it stands.`
        : `- ${sentence(n + at * 3)}, read through \`${code(n + at)}\`.`
    ),
  ];
  if (n % 5 !== 4) lines.push("", `${sentence(n + 11)}.`);
  if (n % 10 === 0) {
    const flags = counted(24)
      .map(
        (at) =>
          `--${code(n + at)
            .replace(/[^a-z]/gi, "")
            .toLowerCase()}-${at}`
      )
      .join(" ");
    lines.push("", "```sh", `bun run e2e -- e2e/issue-page-t-scale.e2e.ts ${flags}`, "```");
  }
  if (n % 20 === 0) lines.push("", table(n));
  return lines.join("\n");
}

/** A reply's words: one to four lines. */
function replyBody(n: number): string {
  const lines = [`${sentence(n + 5)}, so \`${code(n)}\` stays as it is.`];
  if (n % 2 === 0) lines.push("", `- ${sentence(n + 8)}.`, `- ${sentence(n + 9)}.`);
  return lines.join("\n");
}

/** The posts that hold a thread of 5 replies or more, by their place, with its length: one of 20
 *  and eight of 5 to 10. */
const LONG_THREADS: Readonly<Record<number, number>> = {
  3: 20,
  7: 10,
  12: 9,
  18: 8,
  25: 7,
  33: 6,
  41: 6,
  50: 5,
  58: 5,
};

/** How many replies post `n` holds: its long thread's length, else one for three of each four
 *  other posts and none for the fourth, so the 67 posts hold 120 replies. */
function repliesTo(n: number): number {
  const long = LONG_THREADS[n];
  if (long !== undefined) return long;
  const other = n - Object.keys(LONG_THREADS).filter((place) => Number(place) < n).length;
  return other % 4 === 0 ? 0 : 1;
}

/** Waits until the server has indexed each of the spec's questions into an ask, and answers
 *  their ids in the questions' order. */
async function awaitQuestions(key: string): Promise<string[]> {
  const deadline = Date.now() + 30_000;
  for (;;) {
    const open = (await getIssue(key)).open_asks ?? [];
    const ids = counted(LARGEST_SHAPE.questions).map(
      (n) => open.find((ask) => ask.block_id === questionBlockId(n))?.id
    );
    if (ids.every((id) => id !== undefined)) return ids.map((id) => id ?? "");
    if (Date.now() > deadline) {
      throw new Error(
        `${key}'s questions were not all indexed within 30 s: ${JSON.stringify(ids)}`
      );
    }
    await pause(200);
  }
}

/** The issue at the largest issue's shape (`LARGEST_SHAPE`), with no ask that waits on anyone. */
export async function seedLargestShape(): Promise<LargestShapeSeed> {
  const started = Date.now();
  await createProject({ key: PROJECT, name: "Issue layout" });
  const issue = await createIssue({
    force: true,
    project: PROJECT,
    spec: specText(),
    title: "The issue page holds its load and its layout at the largest issue's size",
  });
  const key = issue.key;
  const specArtifactId = issue.primary_artifact_id ?? "";
  await seedSessions(key, [PLANNER, REVIEWER, TESTER]);
  // The planner holds the issue's route, so a message a person writes on it has a session to go
  // to, as on the whole issue of `seedIssueWithDocuments`.
  await patchIssue(key, { assignee: "alice", route: `session:${PLANNER}`, status: "in_progress" });

  const children = await inBursts(counted(LARGEST_SHAPE.children), async (n) => {
    const child = await createIssue({
      force: true,
      parent: key,
      project: PROJECT,
      title: `Child ${n}: ${sentence(n * 3).toLowerCase()}`,
    });
    const status = CHILD_STATUSES[n % CHILD_STATUSES.length] ?? "todo";
    if (status !== "todo") await patchIssue(child.key, { status });
    return child.key;
  });
  await inBursts(BLOCKED_CHILDREN, async ([blocked, blocker]) => {
    await patchIssue(children[blocked - 1] ?? "", { blocked_by: [children[blocker - 1] ?? ""] });
  });

  // Of the agents' own asks the first are answered and the rest resolved; with the spec's
  // questions, answered below, 24 are answered and 10 resolved in all.
  const ownAsks = LARGEST_SHAPE.asks - LARGEST_SHAPE.questions;
  const toAnswer = LARGEST_SHAPE.answeredAsks - LARGEST_SHAPE.questions;
  const asks = await inBursts(counted(ownAsks), async (n) => {
    const asker = ASKERS[n % ASKERS.length] ?? asPlanner;
    const options = n % 2 === 0 ? [{ label: "Yes" }, { label: "No" }] : undefined;
    const ask = await createAsk(
      key,
      {
        question: `Should ${sentence(n * 4).toLowerCase()}?`,
        urgency: "med",
        ...(options === undefined ? {} : { options }),
      },
      asker
    );
    if (n <= toAnswer) {
      await answerAsk(ask.id, {
        expected_edited_at: null,
        selected: options === undefined ? [] : ["Yes"],
        text: `${sentence(n + 6)}.\n\n${sentence(n + 7)}.`,
      });
    } else {
      await resolveAsk(ask.id, { kind: "resolved", reason: `${sentence(n + 1)}.` }, asker);
    }
    return ask.id;
  });

  // The posts with their replies, and a message after every sixth post.
  const replies: string[] = [];
  const messages: string[] = [];
  const posts = await inBursts(counted(LARGEST_SHAPE.posts), async (n) => {
    const writer = WRITERS[n % WRITERS.length] ?? {};
    const post = await createComment(key, { body: postBody(n, key, children) }, writer);
    for (const at of counted(repliesTo(n))) {
      const replier = WRITERS[(n + at) % WRITERS.length] ?? {};
      const reply = await createComment(
        key,
        { body: replyBody(n + at), reply_to: post.id },
        replier
      );
      replies.push(reply.id);
    }
    if (n % 6 === 0 && messages.length < LARGEST_SHAPE.messages) {
      const message = await createMessage(
        key,
        { body: `${sentence(n + 2)}.\n\n${sentence(n + 3)}, as \`${code(n)}\` reads it.` },
        n % 32 === 0 ? asPlanner : {}
      );
      messages.push(message.id);
    }
    return post.id;
  });
  while (messages.length < LARGEST_SHAPE.messages) {
    const n = messages.length;
    messages.push((await createMessage(key, { body: `${sentence(n + 17)}.` }, asPlanner)).id);
  }

  // The statements, each under the ask or the post it came from, then what stands on them: the
  // planner's questions and decisions, the Verification document and the plan.
  const statements = statementOps(key, asks, posts);
  await editArtifact(specArtifactId, { ops: statements.ops });
  await editArtifact(specArtifactId, { ops: questionsOps() }, asPlanner);
  await editArtifact(
    specArtifactId,
    { ops: [{ after: "end", markdown: decisionsPart(key), op: "insert" }] },
    asPlanner
  );
  const verification = await createIssueArtifact(key, {
    content: verificationText(key),
    name: "verification.md",
  });
  const plan = await createIssueArtifact(key, { content: planText(key), name: "plan.md" });
  const questions = await awaitQuestions(key);
  await inBursts(questions, async (id, at) => {
    await answerAsk(id, { expected_edited_at: null, selected: [], text: `${sentence(at + 2)}.` });
  });
  const textComment = await createComment(key, {
    anchor: { artifact: "spec", quote: statementText(1) },
    body: `${sentence(3)}, so this statement says it once.`,
  });

  return {
    children,
    key,
    messages,
    origins: statements.origins,
    planArtifactId: plan.artifact.id,
    posts,
    replies,
    seedMs: Date.now() - started,
    specArtifactId,
    textComment: textComment.id,
    verificationArtifactId: verification.artifact.id,
  };
}

/** The second variant's one more ask: the planner's, of the issue's assignee, the reader, and
 *  open, so it waits on her. Answers its id. */
export async function addWaitingAsk(key: string): Promise<string> {
  const ask = await createAsk(
    key,
    { options: [{ label: "Yes" }, { label: "No" }], question: WAITING_QUESTION, urgency: "high" },
    asPlanner
  );
  return ask.id;
}

/** The issue at the largest issue's shape with one ask that waits on the reader. */
export async function seedLargestShapeWaiting(): Promise<
  LargestShapeSeed & { readonly waiting: string }
> {
  const seeded = await seedLargestShape();
  return { ...seeded, waiting: await addWaitingAsk(seeded.key) };
}

// ---------------------------------------------------------------------------------------------
// The first screen.
// ---------------------------------------------------------------------------------------------

/** One thing the first screen draws, by its kind, with its top in the viewport and its height,
 *  in px. */
export interface ScreenPart {
  readonly height: number;
  readonly kind: string;
  readonly top: number;
}

/** What the page draws in its first screen, and where the discussion stands: its posts (comments
 *  tied to no text) and its questions (asks). */
export interface FirstScreen {
  /** Each thing the first screen draws, from the top. */
  readonly parts: readonly ScreenPart[];
  /** How many rows of the discussion have their first line inside the viewport, below the
   *  children's section. */
  readonly discussionInView: number;
  /** How far from the page's top the first line of the discussion's first row stands, in px;
   *  null where the page draws none. */
  readonly firstDiscussion: number | null;
  /** The kind of each outline row the page draws before the discussion's first row, in order. */
  readonly before: readonly string[];
  /** How many outline rows the page draws. */
  readonly rows: number;
  /** Each control that draws the next page of a node's children, by what it says and how far
   *  from the page's top it stands, in px. */
  readonly pageControls: readonly { readonly text: string; readonly top: number }[];
  /** How far the page stands scrolled, in px. */
  readonly scrolled: number;
  /** The viewport's height, in px. */
  readonly viewport: number;
}

/**
 * Reads what the first screen of the page draws, in one pass over the DOM: the header, the view
 * row, the three sentences, the Contents pane, the children's section with the count of children
 * it lists, each control of the outline and each outline row by its kind, where each is inside
 * the viewport, from the top; where the first line of each row of the discussion stands, a post
 * (a comment tied to no text, which `posts` names by id) or a question (an ask's row); and the
 * kinds of the rows drawn before the first of them. A row's first line is the first box of text
 * it draws.
 */
export function readFirstScreen(page: Page, posts: readonly string[]): Promise<FirstScreen> {
  return page.evaluate((postIds) => {
    const viewport = window.innerHeight;
    const named = new Set(postIds);
    const root = document.querySelector("[data-issue-page]");
    let scrolled = document.scrollingElement?.scrollTop ?? 0;
    for (let at = root?.parentElement ?? null; at !== null; at = at.parentElement) {
      if (at !== document.scrollingElement) scrolled += at.scrollTop;
    }
    // The first line is read from the boxes of the row's text nodes alone. A range over the
    // row's contents also answers the border box of each element it holds whole, and the row's
    // card spans the whole row, so the top box would be the card's, and its foot the row's.
    const firstLine = (element: Element): { bottom: number; top: number } => {
      const range = document.createRange();
      const texts = document.createTreeWalker(element, NodeFilter.SHOW_TEXT);
      let first: DOMRect | undefined;
      for (let text = texts.nextNode(); text !== null; text = texts.nextNode()) {
        range.selectNodeContents(text);
        for (const box of range.getClientRects()) {
          if (box.width > 0 && box.height > 0 && (first === undefined || box.top < first.top)) {
            first = box;
          }
        }
      }
      const own = element.getBoundingClientRect();
      return first === undefined
        ? { bottom: own.bottom, top: own.top }
        : { bottom: first.bottom, top: first.top };
    };
    const parts: { height: number; kind: string; top: number }[] = [];
    // An element that draws no box of its own (the header is `display: contents`) stands where
    // its children do.
    const boxOf = (element: Element): { bottom: number; top: number } => {
      const own = element.getBoundingClientRect();
      const boxes = [...element.children]
        .map((child) => child.getBoundingClientRect())
        .filter((box) => box.height > 0);
      return own.height > 0 || boxes.length === 0
        ? { bottom: own.bottom, top: own.top }
        : {
            bottom: Math.max(...boxes.map((box) => box.bottom)),
            top: Math.min(...boxes.map((box) => box.top)),
          };
    };
    const take = (kind: string, element: Element) => {
      const box = boxOf(element);
      if (box.bottom <= Math.max(box.top, 0) || box.top >= viewport) return;
      parts.push({ height: Math.round(box.bottom - box.top), kind, top: Math.round(box.top) });
    };
    const inPage = (selector: string) => [...(root?.querySelectorAll(selector) ?? [])];
    for (const [kind, selector] of [
      ["header", "header[data-issue-header]"],
      ["view row", "[data-filter-row]"],
      ["three sentences", "[data-testid='issue-three-things']"],
      ["contents pane", "[data-testid='issue-contents']"],
      ["older comments control", "[data-older-comments]"],
    ] as const) {
      for (const element of inPage(selector)) take(kind, element);
    }
    let sectionFoot = 0;
    for (const section of inPage("[data-children-section]")) {
      take(
        `children's section listing ${section.querySelectorAll("[data-child-entry]").length} children`,
        section
      );
      sectionFoot = Math.max(sectionFoot, boxOf(section).bottom);
    }
    const pageControls: { text: string; top: number }[] = [];
    for (const control of inPage("[data-tree-page]")) {
      take(`control "${(control.textContent ?? "").replace(/\s+/g, " ").trim()}"`, control);
      pageControls.push({
        text: (control.textContent ?? "").replace(/\s+/g, " ").trim(),
        top: Math.round(control.getBoundingClientRect().top + scrolled),
      });
    }
    const rows = inPage("[data-node-row]");
    let discussionInView = 0;
    let firstDiscussion: number | null = null;
    const before: string[] = [];
    for (const row of rows) {
      const type = row.getAttribute("data-node-type") ?? "untyped";
      const post = named.has(row.getAttribute("data-node-id") ?? "");
      const context = row.hasAttribute("data-node-context") ? " context" : "";
      take(`${type}${context} row${post ? " (post)" : ""}`, row);
      if (!post && type !== "ask") {
        if (firstDiscussion === null) before.push(type);
        continue;
      }
      const line = firstLine(row);
      if (line.top >= Math.max(0, sectionFoot) && line.bottom <= viewport) discussionInView += 1;
      const top = Math.round(line.top + scrolled);
      if (firstDiscussion === null || top < firstDiscussion) firstDiscussion = top;
    }
    parts.sort((a, b) => a.top - b.top);
    return {
      before,
      discussionInView,
      firstDiscussion,
      pageControls,
      parts,
      rows: rows.length,
      scrolled,
      viewport,
    };
  }, posts);
}

/** `screen` in words, for a failure: what the first screen holds from the top, each thing with its
 *  top and its height, and where the discussion's first row stands, in screens. */
export function describeFirstScreen(screen: FirstScreen): string {
  const parts = screen.parts.map((part) => `${part.kind} at ${part.top} px (${part.height} px)`);
  const first =
    screen.firstDiscussion === null
      ? `no post or question is drawn among its ${screen.rows} rows; its controls for the rest: ${
          screen.pageControls
            .map(
              (control) =>
                `"${control.text}" ${(control.top / screen.viewport).toFixed(1)} screens down`
            )
            .join(", ") || "none"
        }`
      : `the first post's or question's first line stands ${screen.firstDiscussion} px down, ${(
          screen.firstDiscussion / screen.viewport
        ).toFixed(1)} screens, after ${screen.before.length} rows`;
  return `the first screen (${screen.viewport} px, scrolled ${screen.scrolled} px) holds, from the top: ${parts.join("; ")}; ${first}`;
}

// ---------------------------------------------------------------------------------------------
// The gate: the tree read answering the discussion's order.
// ---------------------------------------------------------------------------------------------

/** The kinds of row that stand after the discussion until the read answers its order: what the
 *  discussion produced, and the nodes on them. */
export const PRODUCED_KINDS: readonly string[] = ["requirement", "check", "decision"];

/** The first page of Conversation's read the page receives from now on: the tree read with
 *  `kind`, with no `cursor` and no `comments_before`. Start it before the navigation. */
export function conversationFirstPage(page: Page): Promise<IssueTreeRead> {
  return page
    .waitForResponse((response) => {
      const url = new URL(response.url());
      return (
        TREE_PATH.test(url.pathname) &&
        url.searchParams.has("kind") &&
        !url.searchParams.has("cursor") &&
        !url.searchParams.has("comments_before")
      );
    })
    .then(async (response) => (await response.json()) as IssueTreeRead);
}

/** The tree read answering the order the discussion was written in: the first page of
 *  Conversation's read, `answer`, answers a post or a question at the top level, under the issue,
 *  before any requirement there. Until it does, the read puts the statements first and the
 *  discussion after them, and the page invents no order of its own. */
export function discussionOrderGate(answer: IssueTreeRead): Gate {
  const top = (answer.nodes ?? []).filter((node) => node.parent?.supertag === "issue");
  const discussion = top.findIndex(
    (node) => node.supertag === "ask" || node.supertag === "comment"
  );
  const statement = top.findIndex((node) => node.supertag === "requirement");
  return {
    met: discussion !== -1 && (statement === -1 || discussion < statement),
    owed:
      "owed until the tree read answers the discussion's order: the first page of Conversation's " +
      `read answers a requirement under the issue before any post or question; ${OWED_BY}`,
  };
}
