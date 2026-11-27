import { createIssue, createIssueArtifact, createProject, documentBase, search } from "./api";
import type { ChatReply, ChatRunScript } from "./fake-chat-model";
import { proposePlan, say } from "./fake-models-helpers";
import { answerText } from "./going-locators";
import { expect, type Page } from "./test";

// What the command bar's rows share beside the bar's locators (`e2e/going-locators.ts`): the
// issue a question is answered from, the blocks and the runs the fake writing model's scripts
// draw, what the bar asked the server, and the frames of the agent's runs.

/** Seeds project CORE and one issue of it titled `title`, and answers the issue's key. */
export async function seedCoreIssue(title: string): Promise<string> {
  await createProject({ key: "CORE", name: "Core" });
  return (await createIssue({ project: "CORE", title })).key;
}

/** Seeds project CORE with one issue for each of `titles`, and answers their keys. */
export async function seedIssues(titles: readonly string[]): Promise<string[]> {
  await createProject({ key: "CORE", name: "Core" });
  const keys: string[] = [];
  for (const title of titles) {
    keys.push((await createIssue({ project: "CORE", title })).key);
  }
  return keys;
}

/** Seeds project CORE and one issue titled `title`, and answers that issue's key and the href the
 *  search for `question` gives its hit, which is the link the model is given. */
export async function seedIssue(
  question: string,
  title: string
): Promise<{ readonly key: string; readonly href: string }> {
  const key = await seedCoreIssue(title);
  const hit = ((await search(question)).results ?? []).find(
    (result) => result.owner.kind === "issue" && result.owner.key === key
  );
  if (hit === undefined) {
    throw new Error(`search for "${question}" does not find ${key}`);
  }
  return { href: hit.href, key };
}

/** A parent issue of CORE and its two children, by key. */
export interface Split {
  readonly parent: string;
  readonly first: string;
  readonly second: string;
}

/** Seeds project CORE with the issue a split splits and its two children. */
export async function seedSplit(): Promise<Split> {
  await createProject({ key: "CORE", name: "Core" });
  const parent = await createIssue({ project: "CORE", title: "Build the observatory" });
  const first = await createIssue({
    parent: parent.key,
    project: "CORE",
    title: "Pour the foundation",
  });
  const second = await createIssue({
    parent: parent.key,
    project: "CORE",
    title: "Raise the dome",
  });
  return { first: first.key, parent: parent.key, second: second.key };
}

/** Writes `content` as the spec of `issue`, over the spec the issue was made with: an upload over
 *  a document names the token of the version it replaces. */
export async function writeSpec(
  issue: { readonly key: string; readonly primary_artifact_id: string },
  content: string
): Promise<void> {
  await createIssueArtifact(issue.key, {
    content,
    name: "spec.md",
    precondition: await documentBase(issue.primary_artifact_id),
  });
}

/** A chart of project CORE's issues by status, as bars: the spec a model writes for it names a
 *  read and its fields, never a number. */
export const STATUS_CHART_SPEC = {
  mark: "bar",
  params: { project: "CORE" },
  source: "read_progress",
  table: "statuses",
  x: "status",
  y: "count",
} as const;

/** Hands `take` the JSON body of each `POST` to `pathname` the page sends from here on. */
function onPost(page: Page, pathname: string, take: (body: unknown) => void): void {
  page.on("request", (request) => {
    const url = new URL(request.url());
    if (request.method() === "POST" && url.pathname === pathname) {
      take(request.postDataJSON());
    }
  });
}

/** Counts the bar's `POST /api/v1/going/requests` from here on, each with the words it asked. */
export function countAskedRuns(page: Page): { readonly words: string[] } {
  const asked = { words: [] as string[] };
  onPost(page, "/api/v1/going/requests", (body) => {
    const words =
      body !== null && typeof body === "object" && "words" in body ? String(body.words) : "";
    asked.words.push(words);
  });
  return asked;
}

/** Counts the agent runs the page starts, its `POST /api/v1/going/runs` from here on, each with
 *  the body it sent: the words, the level, the line pressed and the page's context. The bar's
 *  short answer is no run of these: it is `POST /api/v1/going/requests` (`countAskedRuns`). */
export function countStartedRuns(page: Page): { readonly bodies: Record<string, unknown>[] } {
  const started = { bodies: [] as Record<string, unknown>[] };
  onPost(page, "/api/v1/going/runs", (body) => {
    started.bodies.push(
      body !== null && typeof body === "object" ? (body as Record<string, unknown>) : {}
    );
  });
  return started;
}

/** A structured reply with a block of `kind`, its spec naming the read that fills it. */
export function withBlock(kind: string, spec: unknown, answer = "Here it is."): ChatReply {
  return { final: { answer, block: { kind, spec, title: `The ${kind}` } } };
}

/** `count` distinct plain words, none of which a renderer links or a lexicon reads as a block. */
export function plainWords(stem: string, count: number): string {
  return Array.from({ length: count }, (_, index) => `${stem}${index}`).join(" ");
}

/** The words the panel's text shows, split on whitespace. */
export async function shownWords(page: Page): Promise<string[]> {
  const text = (await answerText(page).textContent()) ?? "";
  return text.split(/\s+/).filter((word) => word !== "");
}

/** The run Enter on File as a new issue starts for `words`, as the fake writing model answers it:
 *  one plan of one create of an issue of CORE titled `title`, which the server files at once, as
 *  the agent's new-issue prompt asks (`going/agent/prompt.go`). */
export function newIssueRun(words: string, title: string): ChatRunScript {
  return {
    afterwards: say("Filed."),
    turns: [
      proposePlan(`File ${title} in CORE.`, [
        {
          arguments: { project: "CORE", spec: `${words}.\n`, title },
          tool: "dispatch_issue",
        },
      ]),
    ],
    words,
  };
}

/** One frame of a run's stream, as `GET /api/v1/going/runs/{id}/events` writes it. */
export interface RunFrame {
  readonly name: string;
  readonly data: Record<string, unknown>;
}

/** The frames of the ended run `id`, read as the page's person
 *  (`GET /api/v1/going/runs/{id}/events`), in the order the run stored them: the stream writes
 *  every frame of an ended run and ends. */
export async function runFrames(page: Page, id: string): Promise<RunFrame[]> {
  const response = await page.request.get(`/api/v1/going/runs/${id}/events`, {
    headers: { Accept: "text/event-stream" },
  });
  expect(response.status()).toBe(200);
  const frames: RunFrame[] = [];
  for (const event of (await response.text()).split(/\r?\n\r?\n/u)) {
    let name = "message";
    const data: string[] = [];
    for (const line of event.split(/\r?\n/u)) {
      if (line.startsWith("event:")) name = line.slice("event:".length).trim();
      else if (line.startsWith("data:")) data.push(line.slice("data:".length).trimStart());
    }
    if (data.length > 0) frames.push({ data: JSON.parse(data.join("\n")), name });
  }
  return frames;
}
