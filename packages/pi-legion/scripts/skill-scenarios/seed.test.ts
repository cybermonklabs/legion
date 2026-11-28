import { afterEach, beforeAll, beforeEach, describe, expect, spyOn, test } from "bun:test";
import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { restoreEnvironment } from "@legion/test-fixtures/environment";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { parseHeldoutFile, parseSkillUseFile, type StepRun, World } from "./heldout-case";
import type * as ownerWalkSeedModule from "./owner-walk-seed";
import { parseInstant } from "./production-dispatch";
import type * as seedModule from "./seed";

// e2e/api.ts, which seed.ts and owner-walk-seed.ts call, reads its port when it loads: set it as
// rig.sh's seed_ts does, then load both. PLAYWRIGHT_BASE_URL would point api.ts somewhere else.
// Both go back to what the caller held once this file ends: a plain `bun test` runs every file in
// one process. The names are written, and the modules loaded, in the `beforeAll` below: a
// `bun test -t <pattern>` that selects no row of this file runs neither that hook nor the
// `afterEach` and `afterAll` the capture registered, so a write at module scope would stay written
// for every later file.
restoreEnvironment(["DISPATCH_E2E_PORT", "PLAYWRIGHT_BASE_URL"]);
let seed: typeof seedModule;
let ownerWalkSeed: typeof ownerWalkSeedModule;

const origin = "http://127.0.0.1:18777";
const work = temporaryDirectory("skill-scenarios-seed");

beforeAll(async () => {
  process.env.DISPATCH_E2E_PORT = "18777";
  delete process.env.PLAYWRIGHT_BASE_URL;
  seed = await import("./seed");
  ownerWalkSeed = await import("./owner-walk-seed");
});
let dirs = 0;
function freshDir(name: string): string {
  const dir = path.join(work, `${name}-${++dirs}`);
  mkdirSync(dir, { recursive: true });
  return dir;
}
/** The ask blocks of a spec as Dispatch's settlement indexes them: each `:::ask{#<id> …}` block's
 * id, its question (the lines before its option list) and its options (`- Label: description`). */
function askBlocks(spec: string) {
  return [...spec.matchAll(/^:::ask\{#([^\s}]+)[^}]*\}\n([\s\S]*?)\n:::$/gm)].map(
    ([, id, body]) => {
      const lines = (body ?? "").split("\n");
      const options = lines
        .filter((line) => line.startsWith("- "))
        .map((line) => {
          const [label = "", ...rest] = line.slice(2).split(": ");
          const description = rest.join(": ");
          return description === "" ? { label } : { label, description };
        });
      const question = lines
        .filter((line) => !line.startsWith("- "))
        .join("\n")
        .trim();
      return { block_id: id ?? "", question, options };
    }
  );
}

type Actor =
  | { kind: "user"; id: string }
  | { kind: "session"; id: string; origin?: { session_title?: string } };
interface FakeAsk {
  id: string;
  issue_key: string;
  author: Actor;
  kind: "question" | "approval";
  question: string;
  options: { label: string; description?: string }[];
  multiple: boolean;
  state: "open" | "answered" | "resolved";
  answer: { user: string; selected: string[]; text: string | null; at: string } | null;
  edited_at: string | null;
  created_at: string;
  approval?: { artifact_id: string; name: string; version: number; requested_version: number };
  block_id?: string | null;
}
interface FakeArtifact {
  id: string;
  issue_key: string;
  name: string;
  slug: string;
  kind: "doc" | "image" | "file";
  primary: boolean;
  markdown: string;
  versions: { number: number; created_at: string; authors: Actor[]; markdown: string }[];
  /** The ids of the document's ask blocks, which `GET …/blocks` lists. */
  askBlocks?: string[];
}
interface Call {
  method: string;
  path: string;
  /** `alice` (or another login) for a cookie call, `agent` for a bearer call. */
  who: string;
  body: Record<string, unknown> | undefined;
}

/** A place on the fake's board as Dispatch answers a rank: a text whose order is the board's. */
function rankText(place: number): string {
  return place.toFixed(4).padStart(16, "0");
}

/** The scratch Dispatch seed.ts talks to, held in memory: just the routes seed.ts calls. */
class FakeDispatch {
  calls: Call[] = [];
  issues = new Map<
    string,
    {
      key: string;
      title: string;
      spec_artifact: string;
      parent: string | null;
      status: string;
      labels: string[];
      blocked_by: string[];
      claim: { actor: Actor } | null;
      /** The issue's place on the board, a number here: the read answers it as a rank text. */
      rank: number;
      created_at: string;
    }
  >();
  /** The fake's clock, in nanoseconds since the epoch, which it stamps an issue, a handoff, a
   * comment, an ask and a result from, to the microsecond as PostgreSQL does (stamp). By default
   * it is the host's, each read at least a microsecond after the read before it; a row that holds
   * the clock sets its own, under which two writes the seed does not wait between share a stamp. */
  now: () => bigint = (() => {
    let last = 0n;
    return () => {
      const host = BigInt(Date.now()) * 1_000_000n;
      last = host > last ? host : last + 1_000n;
      return last;
    };
  })();
  /** `now` as Dispatch answers a record's created_at: RFC 3339 in UTC, to the microsecond. */
  stamp(): string {
    const micros = this.now() / 1_000n;
    const whole = new Date(Number(micros / 1_000n)).toISOString().slice(0, 19);
    return `${whole}.${String(micros % 1_000_000n).padStart(6, "0")}Z`;
  }
  /** What the step list answers of a run, after the fake's own answer: a Dispatch that lists a
   * run otherwise than it was written. */
  misreadRun: (run: Record<string, unknown>) => Record<string, unknown> = (run) => run;
  /** Each step run written, in the order written, with the rating written on it. */
  runs: Record<string, unknown>[] = [];
  /** What the issue read answers of an issue, after the fake's own answer: a Dispatch that holds
   * a fact otherwise than it was written. */
  misread: (key: string, answer: Record<string, unknown>) => Record<string, unknown> = (
    _key,
    answer
  ) => answer;
  asks: FakeAsk[] = [];
  messages: { message: Record<string, unknown>; replies: Record<string, unknown>[] }[] = [];
  comments: Record<string, unknown>[] = [];
  artifacts: FakeArtifact[] = [];
  handoffs: {
    id: string;
    issue_key: string;
    phase: string;
    version: number;
    author: Actor;
    created_at: string;
    body: string;
  }[] = [];
  results: Record<string, unknown>[] = [];
  events: { seq: number; type: string; created_at: string }[] = [];
  /** A seeded spec's ask blocks no settlement has indexed yet, by issue. Dispatch settles a
   * seeded document a moment after the issue is created; here the issue's second read of its asks
   * is the first to find them. */
  unsettled = new Map<string, ReturnType<typeof askBlocks>>();
  asksReads = new Map<string, number>();
  #ids = 0;
  id(prefix: string): string {
    return `${prefix}-${++this.#ids}`;
  }
  /** A fresh id in a uuid's form, as Dispatch gives a handoff. */
  uuid(): string {
    return `00000000-0000-4000-8000-${String(++this.#ids).padStart(12, "0")}`;
  }
  issue(
    key: string,
    title = "A title",
    parent: string | null = null,
    labels: string[] = []
  ): string {
    const spec = this.id("art");
    this.issues.set(key, {
      key,
      title,
      spec_artifact: spec,
      parent,
      status: "triage",
      labels,
      blocked_by: [],
      claim: null,
      rank: 1000 + this.issues.size,
      created_at: this.stamp(),
    });
    this.artifacts.push({
      id: spec,
      issue_key: key,
      name: "spec.md",
      slug: "spec",
      kind: "doc",
      primary: true,
      markdown: "",
      versions: [],
    });
    return spec;
  }

  readonly fetch = async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    expect(url.origin).toBe(origin);
    const method = init?.method ?? "GET";
    if (url.pathname === "/auth/_dev/signin") {
      const login = url.searchParams.get("login") ?? "";
      return new Response(null, {
        status: 302,
        headers: { "Set-Cookie": `dsession=${login}; Path=/; HttpOnly` },
      });
    }
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const who =
      headers.Authorization === "Bearer e2e-token"
        ? "agent"
        : (/dsession=([^;]+)/.exec(headers.Cookie ?? "")?.[1] ?? "nobody");
    const body = init?.body === undefined ? undefined : JSON.parse(String(init.body));
    this.calls.push({ method, path: url.pathname, who, body });
    const route = `${method} ${url.pathname}`;
    const json = (value: unknown, status = 200) => Response.json(value, { status });
    const actor = (): Actor =>
      who === "agent" ? (body?.actor as Actor) : { kind: "user", id: `${who}@example.invalid` };
    const strip = <T extends { markdown: string }>({ markdown: _markdown, ...rest }: T) => rest;
    // Each route: its method and path pattern, and what it answers for the path's captures.
    const routes: [RegExp, (id: string, second: string) => Response][] = [
      [/^POST \/api\/v1\/projects$/, () => json(body, 201)],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/status-change$/,
        (key) => {
          const issue = this.issues.get(key);
          if (issue !== undefined && typeof body?.status === "string") issue.status = body.status;
          return json({ changed: [] });
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/claim$/,
        (key) => {
          const issue = this.issues.get(key);
          if (issue === undefined) return json({ code: "ISSUE_NOT_FOUND" }, 404);
          issue.claim = { actor: actor() };
          return json({ ...issue, legion_managed_status: false });
        },
      ],
      [
        /^PUT \/api\/v1\/issues\/([^/]+)\/steps\/([^/]+)\/rating$/,
        (_key, id) => {
          const run = this.runs.find((each) => each.id === id);
          if (run === undefined) return json({ code: "STEP_INPUT" }, 400);
          run.rating = body?.rating;
          run.rating_reason = body?.rating_reason;
          return json(run);
        },
      ],
      [
        /^PUT \/api\/v1\/issues\/([^/]+)\/steps\/([^/]+)$/,
        (key, id) => {
          const run = { ...body, id, issue: key, rating: null, rating_reason: null };
          this.runs.push(run);
          return json(run);
        },
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/steps$/,
        (key) => {
          const tasks: { task: string; needs: null; steps: Record<string, unknown>[] }[] = [];
          for (const run of this.runs.filter((each) => each.issue === key)) {
            let task = tasks.find((each) => each.task === run.task);
            if (task === undefined) {
              task = { task: String(run.task), needs: null, steps: [] };
              tasks.push(task);
            }
            task.steps.push(this.misreadRun({ result: "", ended_at: null, ...run }));
          }
          return json({ issue: key, tasks });
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/comments$/,
        (issue) => {
          // An anchor names a document of the issue by its id, and words its text holds once, as
          // Dispatch places a quote with no occurrence (pmdoc/find.go choose); a reply is filed
          // under the head of the thread of the comment it names, as Dispatch files it.
          const anchor = body?.anchor as { artifact?: string; quote?: string } | null | undefined;
          const document = this.artifacts.find(
            (artifact) => artifact.id === anchor?.artifact && artifact.issue_key === issue
          );
          if (anchor && document === undefined) return json({ code: "ARTIFACT_NOT_FOUND" }, 404);
          const quote = anchor?.quote;
          if (anchor && !(quote !== undefined && document?.markdown.includes(quote)))
            return json({ code: "TARGET_NOT_FOUND" }, 404);
          if (
            quote !== undefined &&
            document !== undefined &&
            document.markdown.indexOf(quote) !== document.markdown.lastIndexOf(quote)
          )
            return json({ code: "TARGET_AMBIGUOUS" }, 409);
          const parent =
            body?.reply_to === undefined
              ? undefined
              : this.comments.find((comment) => comment.id === body.reply_to);
          if (body?.reply_to !== undefined && parent === undefined)
            return json({ code: "COMMENT_NOT_FOUND" }, 404);
          const row = {
            id: this.id("comment"),
            issue_key: issue,
            // A comment's own artifact_id is its owner's, and an issue comment's is null: the
            // document an anchor is on is named in the anchor alone.
            artifact_id: null,
            anchor: document === undefined ? null : { artifact_id: document.id, quote },
            reply_to: parent === undefined ? null : (parent.reply_to ?? parent.id),
            body: body?.body,
            author: actor(),
            created_at: this.stamp(),
          };
          this.comments.push(row);
          return json(row, 201);
        },
      ],
      [
        /^POST \/api\/v1\/asks\/([^/]+)\/resolve$/,
        (id) => {
          const ask = this.asks.find((a) => a.id === id);
          if (!ask) return json({ error: "not found" }, 404);
          ask.state = "resolved";
          return json(ask);
        },
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/events$/,
        () =>
          json(
            this.events
              .filter(({ seq }) => seq > Number(url.searchParams.get("after")))
              .slice(0, Number(url.searchParams.get("limit")))
          ),
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/results$/,
        (issue) =>
          json({ checks: [{ results: this.results.filter((row) => row.issue_key === issue) }] }),
      ],
      [
        /^POST \/api\/v1\/issues$/,
        () => {
          const project = String(body?.project);
          const count = [...this.issues].filter(([key]) => key.startsWith(`${project}-`)).length;
          const key = `${project}-${count + (project === "LWEVAL" ? 2 : 1)}`;
          const spec = this.issue(key, body?.title, body?.parent ?? null, body?.labels ?? []);
          const artifact = this.artifacts.find((a) => a.id === spec);
          if (artifact !== undefined && typeof body?.spec === "string") {
            artifact.markdown = body.spec;
            // A spec given at the create is its version 1, which approval-requests reads.
            artifact.versions = [
              {
                number: 1,
                created_at: new Date().toISOString(),
                authors: [actor()],
                markdown: body.spec,
              },
            ];
            const blocks = askBlocks(body.spec);
            artifact.askBlocks = blocks.map((block) => block.block_id);
            if (blocks.length > 0) this.unsettled.set(key, blocks);
          }
          return json(
            { key, primary_artifact_id: spec, created_at: this.issues.get(key)?.created_at },
            201
          );
        },
      ],
      [
        /^PATCH \/api\/v1\/issues\/([^/]+)$/,
        (key) => {
          const issue = this.issues.get(key);
          if (issue === undefined) return json({ code: "ISSUE_NOT_FOUND" }, 404);
          if (typeof body?.status === "string") issue.status = body.status;
          if (Array.isArray(body?.labels)) issue.labels = body.labels;
          if (Array.isArray(body?.blocked_by)) issue.blocked_by = body.blocked_by;
          const neighbour = (other: unknown) => this.issues.get(String(other))?.rank ?? 0;
          if (body?.rank?.before !== undefined) issue.rank = neighbour(body.rank.before) - 0.5;
          if (body?.rank?.after !== undefined) issue.rank = neighbour(body.rank.after) + 0.25;
          return json(issue);
        },
      ],
      [
        /^GET \/api\/v1\/issues$/,
        () =>
          json(
            [...this.issues.values()]
              .filter(({ key }) => key.startsWith(`${url.searchParams.get("project")}-`))
              .map((issue) => ({
                key: issue.key,
                status: issue.status,
                rank: rankText(issue.rank),
              }))
          ),
      ],
      [
        /^POST \/api\/v1\/artifacts\/([^/]+)\/approval-requests$/,
        (id) => {
          const artifact = this.artifacts.find((a) => a.id === id);
          if (artifact === undefined) return json({ code: "ARTIFACT_NOT_FOUND" }, 404);
          const version = artifact.versions.length;
          if (version === 0) return json({ code: "NO_VERSION" }, 409);
          const ask: FakeAsk = {
            id: this.id("ask"),
            issue_key: artifact.issue_key,
            author: actor(),
            kind: "approval",
            question: `Approve ${artifact.name} version ${version}?`,
            options: [{ label: "Approve" }, { label: "Request changes" }],
            multiple: false,
            state: "open",
            answer: null,
            edited_at: null,
            created_at: new Date().toISOString(),
            approval: {
              artifact_id: id,
              name: artifact.name,
              version,
              requested_version: version,
            },
          };
          this.asks.push(ask);
          return json({ artifact_id: id, version, ask }, 201);
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/messages$/,
        (issue) => {
          const message = {
            id: this.id("msg"),
            issue_key: issue,
            author: actor(),
            body: body?.body,
            created_at: new Date().toISOString(),
          };
          this.messages.push({ message, replies: [] });
          return json(message, 201);
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/asks$/,
        (issue) => {
          const ask: FakeAsk = {
            id: this.id("ask"),
            issue_key: issue,
            author: actor(),
            kind: "question",
            question: body?.question,
            options: body?.options ?? [],
            multiple: body?.multiple ?? false,
            state: "open",
            answer: null,
            edited_at: null,
            created_at: this.stamp(),
          };
          this.asks.push(ask);
          return json(ask, 201);
        },
      ],
      [
        /^POST \/api\/v1\/asks\/([^/]+)\/answer$/,
        (id) => {
          const ask = this.asks.find((a) => a.id === id);
          if (ask === undefined) return json({ error: "not found" }, 404);
          if (body?.expected_edited_at !== ask.edited_at) return json({ code: "ASK_EDITED" }, 409);
          ask.state = "answered";
          ask.answer = {
            user: `${who}@example.invalid`,
            selected: body?.selected,
            text: body?.text ?? null,
            at: new Date().toISOString(),
          };
          return json(ask);
        },
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/asks$/,
        (issue) => {
          const reads = (this.asksReads.get(issue) ?? 0) + 1;
          this.asksReads.set(issue, reads);
          const pending = this.unsettled.get(issue);
          if (pending !== undefined && reads >= 2) {
            this.unsettled.delete(issue);
            for (const block of pending)
              this.asks.push({
                id: this.id("ask"),
                issue_key: issue,
                author: { kind: "user", id: "alice@example.invalid" },
                kind: "question",
                question: block.question,
                options: block.options,
                multiple: false,
                state: "open",
                answer: null,
                edited_at: null,
                created_at: new Date().toISOString(),
                block_id: block.block_id,
              });
          }
          return json(this.asks.filter((a) => a.issue_key === issue));
        },
      ],
      [
        /^GET \/api\/v1\/asks\/([^/]+)$/,
        (id) => json({ ask: this.asks.find((a) => a.id === id), replies: [] }),
      ],
      [
        /^GET \/api\/v1\/artifacts\/([^/]+)\/blocks$/,
        (id) => {
          const artifact = this.artifacts.find((a) => a.id === id);
          return json([
            { id: "p-1", type: "paragraph" },
            ...(artifact?.askBlocks ?? []).map((block) => ({ id: block, type: "ask" })),
          ]);
        },
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/messages$/,
        (issue) => json(this.messages.filter((m) => m.message.issue_key === issue)),
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/comments$/,
        (issue) => json(this.comments.filter((c) => c.issue_key === issue)),
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)$/,
        (issue) => {
          const held = this.issues.get(issue);
          return json(
            this.misread(issue, {
              key: issue,
              title: held?.title ?? "A title",
              status: held?.status ?? "triage",
              parent: held?.parent ?? null,
              labels: held?.labels ?? [],
              blocked_by: held?.blocked_by ?? [],
              claim: held?.claim ?? null,
              rank: rankText(held?.rank ?? 0),
              artifacts: this.artifacts.filter((a) => a.issue_key === issue).map(strip),
            })
          );
        },
      ],
      [
        /^GET \/api\/v1\/artifacts\/([^/]+)\/text$/,
        (id) => {
          const artifact = this.artifacts.find((a) => a.id === id);
          return json({ markdown: artifact?.markdown, version: artifact?.versions.length ?? null });
        },
      ],
      [
        /^GET \/api\/v1\/artifacts\/([^/]+)\/versions\/(\d+)$/,
        (id, number) => {
          const artifact = this.artifacts.find((a) => a.id === id);
          return json(artifact?.versions.find((v) => v.number === Number(number)));
        },
      ],
      [
        /^GET \/api\/v1\/artifacts\/([^/]+)$/,
        (id) => {
          const artifact = this.artifacts.find((a) => a.id === id);
          return json({ ...artifact, versions: artifact?.versions.map(strip) });
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/handoffs\/([^/]+)$/,
        (issue, phase) => {
          const record = {
            id: this.uuid(),
            issue_key: issue,
            phase,
            version:
              this.handoffs.filter((h) => h.issue_key === issue && h.phase === phase).length + 1,
            author: actor(),
            created_at: this.stamp(),
            body: body?.body,
          };
          this.handoffs.push(record);
          const { body: _body, ...summary } = record;
          return json(summary, 201);
        },
      ],
      [
        /^GET \/api\/v1\/issues\/([^/]+)\/handoffs$/,
        (issue) =>
          json(
            this.handoffs
              .filter((h) => h.issue_key === issue)
              .reverse()
              .map(({ body: _body, ...summary }) => summary)
          ),
      ],
      [
        /^GET \/api\/v1\/handoffs\/([^/]+)$/,
        (id) => {
          const record = this.handoffs.find((h) => h.id === id);
          return record === undefined ? json({ code: "HANDOFF_NOT_FOUND" }, 404) : json(record);
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/artifacts$/,
        (issue) => {
          const name = String(body?.name ?? "");
          const artifact: FakeArtifact = {
            id: this.id("art"),
            issue_key: issue,
            name,
            slug: name.replace(/\.[^.]+$/, "").replace(/[^a-z0-9]+/gi, "-"),
            kind: "doc",
            primary: false,
            markdown: String(body?.content ?? ""),
            versions: [],
          };
          this.artifacts.push(artifact);
          return json({ artifact: strip(artifact), version: { number: 1 } }, 201);
        },
      ],
      [
        /^POST \/api\/v1\/issues\/([^/]+)\/results$/,
        (issue) => {
          const result = {
            id: this.id("result"),
            issue_key: issue,
            check: body?.check,
            role: body?.role,
            verdict: body?.verdict,
            commit_sha: body?.commit,
            actor: actor(),
            created_at: this.stamp(),
          };
          this.results.push(result);
          return json(
            { result, line: `- Check ${body?.check} (before_merge): ${body?.verdict}` },
            201
          );
        },
      ],
    ];
    for (const [pattern, answer] of routes) {
      const match = pattern.exec(route);
      if (match !== null) return answer(match[1] ?? "", match[2] ?? "");
    }
    return json({ error: `the fake serves no ${route}` }, 500);
  };
}

let dispatch: FakeDispatch;
let fetchSpy: ReturnType<typeof spyOn<typeof globalThis, "fetch">>;
let logSpy: ReturnType<typeof spyOn<typeof console, "log">>;
beforeEach(() => {
  dispatch = new FakeDispatch();
  fetchSpy = spyOn(globalThis, "fetch").mockImplementation(dispatch.fetch as typeof fetch);
  logSpy = spyOn(console, "log").mockImplementation(() => {});
});
afterEach(() => {
  fetchSpy.mockRestore();
  logSpy.mockRestore();
});

const runSession = { kind: "session", id: "ses-run", origin: { session_title: "Run" } } as const;
/** A run directory whose transcript header names the run's session, `ses-run`. */
function runDir(): string {
  const dir = freshDir("run");
  mkdirSync(path.join(dir, "sessions"));
  writeFileSync(
    path.join(dir, "sessions", "2026-10-24_ses-run.jsonl"),
    `${JSON.stringify({ type: "session", id: "ses-run", cwd: "/run/cwd" })}\n`
  );
  return dir;
}
/** A fixture file for issue LWEVAL-2, with `seeded` written beside it as seeded.json. */
function fixture(dir: string, spec: string, seeded: string[] = []): string {
  const file = path.join(dir, "fixture.json");
  writeFileSync(file, `${JSON.stringify({ issue: "LWEVAL-2", spec_artifact: spec })}\n`);
  writeFileSync(path.join(dir, "seeded.json"), `${JSON.stringify(seeded)}\n`);
  return file;
}
function ask(fields: Partial<FakeAsk> & Pick<FakeAsk, "id">): FakeAsk {
  return {
    issue_key: "LWEVAL-2",
    author: runSession,
    kind: "question",
    question: `Question of ${fields.id}?`,
    options: [{ label: "Yes", description: "Do it." }, { label: "No" }],
    multiple: false,
    state: "open",
    answer: null,
    edited_at: null,
    created_at: "2026-10-24T10:00:00Z",
    ...fields,
  };
}

/** A source directory as fetch-spec-sources.ts writes it: two messages, an anchored comment and an
 * unanchored comment whose body starts with a block quote, all by people, and three asks of which
 * only the answered question is input. */
function source(oldSpec?: string): string {
  const dir = freshDir("source");
  writeFileSync(path.join(dir, "title.txt"), "Make the widget faster\n");
  writeFileSync(
    path.join(dir, "messages.md"),
    [
      "<!-- message 2026-10-16T17:53:32.037069Z -->",
      "Please make the widget faster.",
      "",
      "<!-- comment 2026-10-17T09:00:00Z anchored -->",
      "> The widget waits 5 s.",
      "",
      "Why 5 s?",
      "",
      "<!-- message 2026-10-18T12:00:00Z -->",
      "One second is enough.",
      "",
      "<!-- comment 2026-10-18T13:00:00Z -->",
      "> The ticket says 5 s.",
      "",
      "It is wrong.",
      "",
    ].join("\n")
  );
  writeFileSync(
    path.join(dir, "asks.json"),
    JSON.stringify([
      {
        id: "src-ask-1",
        kind: "question",
        question: "Cache the widget or shorten its wait?",
        options: [{ label: "Cache", description: "Keep the last result." }, { label: "Shorten" }],
        multiple: false,
        state: "answered",
        created_at: "2026-10-16T18:00:00Z",
        answer: {
          selected: ["Shorten"],
          text: "The wait is the cost.",
          user: "person@example.invalid",
          at: "2026-10-17T10:00:00Z",
        },
      },
      {
        id: "src-ask-2",
        kind: "question",
        question: "An ask nobody answered?",
        options: [],
        multiple: false,
        state: "open",
        created_at: "2026-10-16T19:00:00Z",
        answer: null,
      },
      {
        id: "src-ask-3",
        kind: "approval",
        question: "Approve spec.md version 3?",
        options: [{ label: "Approve" }, { label: "Request changes" }],
        multiple: false,
        state: "answered",
        created_at: "2026-10-16T20:00:00Z",
        answer: {
          selected: ["Approve"],
          text: null,
          user: "p@example.invalid",
          at: "2026-10-16T21:00:00Z",
        },
      },
    ])
  );
  if (oldSpec !== undefined) writeFileSync(path.join(dir, "old-spec.md"), oldSpec);
  return dir;
}
const writes = () =>
  dispatch.calls
    .filter((call) => call.method === "POST")
    .map((call) => `${call.who} ${call.path.replace(/[^/]+-\d+/g, "<id>")}`);

describe("spec-writing", () => {
  // Catches: a seeding that posts the source out of time order, posts a person's words as the
  // agent, opens a seeded ask as the run's session, or seeds the open and the approval asks.
  test("posts the source's input in time order, the messages and answers as alice", async () => {
    const dir = freshDir("seed");
    const file = path.join(dir, "fixture.json");
    await seed.specWriting(file, source());
    expect(writes()).toEqual([
      "alice /api/v1/issues",
      "alice /api/v1/issues/<id>/messages",
      "agent /api/v1/issues/<id>/asks",
      "alice /api/v1/issues/<id>/messages",
      "alice /api/v1/asks/<id>/answer",
      "alice /api/v1/issues/<id>/messages",
      "alice /api/v1/issues/<id>/messages",
    ]);
    const posts = dispatch.calls.filter((call) => call.method === "POST");
    expect(posts[0]?.body).toEqual({
      project: "LWEVAL",
      force: true,
      title: "Make the widget faster",
    });
    expect(posts.map((call) => call.body?.body).filter((body) => body !== undefined)).toEqual([
      "Please make the widget faster.",
      "> The widget waits 5 s.\n\nWhy 5 s?",
      "One second is enough.",
      "> The ticket says 5 s.\n\nIt is wrong.",
    ]);
    expect(posts[2]?.body).toEqual({
      question: "Cache the widget or shorten its wait?",
      options: [{ label: "Cache", description: "Keep the last result." }, { label: "Shorten" }],
      multiple: false,
      actor: { kind: "session", id: seed.SEED_ASKER.id },
    });
    expect(seed.SEED_ASKER.id).not.toBe(runSession.id);
    expect(posts[4]?.body).toEqual({
      selected: ["Shorten"],
      text: "The wait is the cost.",
      expected_edited_at: null,
    });
  });

  // Catches: a seeding that leaves seeded.json out, or writes it somewhere a run's readers miss.
  test("writes the fixture and, beside it, seeded.json with each seeded ask's id", async () => {
    const dir = freshDir("seed");
    const file = path.join(dir, "fixture.json");
    await seed.specWriting(file, source());
    const issue = [...dispatch.issues.values()][0];
    expect(JSON.parse(readFileSync(file, "utf8"))).toEqual({
      issue: issue?.key,
      spec_artifact: issue?.spec_artifact,
    });
    expect(JSON.parse(readFileSync(path.join(dir, "seeded.json"), "utf8"))).toEqual(
      dispatch.asks.map((a) => a.id)
    );
    expect(dispatch.asks).toHaveLength(1);
  });

  // Catches: a seeding that cuts a source question of its own. Dispatch owns the bound on a
  // question, so the question is posted as the source holds it and the server answers.
  test("posts a long source question whole", async () => {
    const src = source();
    const asks = JSON.parse(readFileSync(path.join(src, "asks.json"), "utf8"));
    const long = `${"Which way? ".repeat(400)}Recommendation: the second way, since it costs less.`;
    asks[0].question = long;
    writeFileSync(path.join(src, "asks.json"), JSON.stringify(asks));

    await seed.specWriting(path.join(freshDir("seed"), "fixture.json"), src);

    const posted = dispatch.calls.find((call) => call.path.endsWith("/asks"))?.body?.question;
    expect(posted).toBe(long);
  });

  // Catches: a seeding that orders the input by a millisecond time, which ties every time of one
  // millisecond and so posts a later message before an earlier ask.
  test("orders the input at full precision, microseconds and nanoseconds, ties in source order", async () => {
    for (const [ask, message, answer] of [
      ["000001", "000002", "000003"],
      ["000000001", "000000002", "000000003"],
    ]) {
      const src = freshDir("source");
      writeFileSync(path.join(src, "title.txt"), "Greet\n");
      writeFileSync(
        path.join(src, "messages.md"),
        [
          `<!-- message 2026-10-24T10:00:00.${message}Z -->`,
          "Changed my mind after the ask.",
          "",
          // At one time as the answer: the answer's ask opened first, then the message comes in
          // source order, before the answer.
          `<!-- message 2026-10-24T10:00:00.${answer}Z -->`,
          "Same time as the answer.",
          "",
        ].join("\n")
      );
      writeFileSync(
        path.join(src, "asks.json"),
        JSON.stringify([
          {
            id: "src-ask",
            kind: "question",
            question: "First question?",
            options: [{ label: "Yes" }],
            multiple: false,
            state: "answered",
            created_at: `2026-10-24T10:00:00.${ask}Z`,
            answer: {
              selected: ["Yes"],
              text: "After the message.",
              user: "p@example.invalid",
              at: `2026-10-24T10:00:00.${answer}Z`,
            },
          },
        ])
      );
      dispatch.calls.length = 0;
      await seed.specWriting(path.join(freshDir("seed"), "fixture.json"), src);
      expect(writes(), ask).toEqual([
        "alice /api/v1/issues",
        "agent /api/v1/issues/<id>/asks",
        "alice /api/v1/issues/<id>/messages",
        "alice /api/v1/issues/<id>/messages",
        "alice /api/v1/asks/<id>/answer",
      ]);
    }
  });

  // Catches: a source time read loosely: Date.parse takes a time with no seconds, which RFC 3339
  // does not allow and which Dispatch never writes.
  test("refuses a source time that is no RFC 3339 time, naming the file", async () => {
    const src = freshDir("source");
    writeFileSync(path.join(src, "title.txt"), "Greet\n");
    writeFileSync(path.join(src, "messages.md"), "<!-- message 2026-10-24T10:00Z -->\nHello.\n");
    writeFileSync(path.join(src, "asks.json"), "[]");
    await expect(
      seed.specWriting(path.join(freshDir("seed"), "fixture.json"), src)
    ).rejects.toThrow(
      `${path.join(src, "messages.md")}: "2026-10-24T10:00Z" is not an RFC 3339 time`
    );
  });
});

describe("spec-rewrite", () => {
  // Catches: a rewrite that starts from an empty spec instead of the source's old one.
  test("creates the issue with the source's old-spec.md as its spec", async () => {
    const dir = freshDir("seed");
    await seed.specRewrite(path.join(dir, "fixture.json"), source("## Summary\n\nThe old spec.\n"));
    const created = dispatch.calls.find((call) => call.path === "/api/v1/issues");
    expect(created?.body?.spec).toBe("## Summary\n\nThe old spec.\n");
  });

  // Catches: a rewrite of a source with no old spec, which would be a spec-writing run mislabelled.
  test("refuses a source with no old-spec.md, naming it, and writes nothing", async () => {
    const dir = freshDir("seed");
    const src = source();
    await expect(seed.specRewrite(path.join(dir, "fixture.json"), src)).rejects.toThrow(
      path.join(src, "old-spec.md")
    );
    expect(writes()).toEqual([]);
  });

  /** An old spec whose one ask block the source's src-ask-1 answered: Dispatch reopens it on upload. */
  const oldSpecWithAsk = (question = "Cache the widget or shorten its wait?") =>
    [
      "## Summary",
      "",
      "The widget waits 5 s.",
      "",
      ':::ask{#wait urgency="med" multiple="false" state="answered" answered_by="person" selected="[&#x22;Shorten&#x22;]" answer="The wait is the cost."}',
      question,
      "",
      "- Cache: Keep the last result.",
      "- Shorten",
      ":::",
      "",
    ].join("\n");

  // Catches: a rewrite that leaves the old spec's asks open, for the stand-in to answer and the
  // judges to count as the agent's (an upload resets every ask block to open); one that answers
  // such an ask as someone other than alice, or otherwise than the source did; and one that seeds
  // the same question a second time, as an ask of its own.
  test("answers each ask of the old spec as alice with the source's answer, and seeds it", async () => {
    const dir = freshDir("seed");
    const file = path.join(dir, "fixture.json");
    await seed.specRewrite(file, source(oldSpecWithAsk()));
    const block = dispatch.asks.find((ask) => ask.block_id === "wait");
    expect(block).toMatchObject({
      state: "answered",
      answer: {
        user: "alice@example.invalid",
        selected: ["Shorten"],
        text: "The wait is the cost.",
      },
    });
    expect(JSON.parse(readFileSync(path.join(dir, "seeded.json"), "utf8"))).toEqual([block?.id]);
    expect(dispatch.asks).toHaveLength(1);
    const run = runDir();
    await seed.state(file, 1, run);
    expect(JSON.parse(readFileSync(path.join(run, "state-1.json"), "utf8")).open_asks).toEqual([]);
  });

  // Catches: a rewrite that makes up an answer, or leaves the ask open, when the source answers no
  // question the old spec's ask asks.
  test("fails, naming the ask and the source, when the source holds no answer to an old-spec ask", async () => {
    const dir = freshDir("seed");
    const src = source(oldSpecWithAsk("Which colour?"));
    const error = await seed.specRewrite(path.join(dir, "fixture.json"), src).then(
      () => undefined,
      (caught: unknown) => caught
    );
    expect(error).toBeInstanceOf(Error);
    const message = (error as Error).message;
    expect(message).toContain(path.join(src, "asks.json"));
    expect(message).toContain('"Which colour?"');
    expect(message).toContain("block wait");
    expect(writes().filter((write) => write.endsWith("/answer"))).toEqual([]);
  });
});

describe("state", () => {
  function world(spec: string) {
    const plan = dispatch.id("art");
    dispatch.asks.push(
      ask({ id: "seeded-open" }),
      ask({ id: "agent-open" }),
      ask({ id: "agent-answered", state: "answered" }),
      ask({
        id: "approval-spec",
        kind: "approval",
        options: [{ label: "Approve" }, { label: "Request changes" }],
        approval: { artifact_id: spec, name: "spec.md", version: 4, requested_version: 4 },
      }),
      ask({
        id: "approval-plan",
        kind: "approval",
        approval: { artifact_id: plan, name: "plan.md", version: 1, requested_version: 1 },
      })
    );
  }

  // Catches: a state that lists a seeded ask, an answered ask or an approval request as an open
  // question, misses the spec's approval request, or takes another document's for it.
  test("writes and prints each open question and the spec's open approval request", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    world(spec);
    const dir = runDir();
    const result = await seed.state(fixture(dir, spec, ["seeded-open"]), 3, dir);
    const expected = {
      turn: 3,
      open_asks: [
        {
          id: "agent-open",
          question: "Question of agent-open?",
          options: [
            { label: "Yes", description: "Do it." },
            { label: "No", description: "" },
          ],
          multiple: false,
        },
      ],
      approval: { ask_id: "approval-spec", version: 4 },
    };
    expect(result).toEqual(expected);
    expect(JSON.parse(readFileSync(path.join(dir, "state-3.json"), "utf8"))).toEqual(expected);
    expect(logSpy.mock.calls.map((call) => JSON.parse(String(call[0])))).toEqual([expected]);
  });

  // Catches: a state that reports an approval when the spec has none open.
  test("approval is null when no approval request on the spec is open", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    dispatch.asks.push(ask({ id: "agent-open" }));
    const dir = runDir();
    expect((await seed.state(fixture(dir, spec), 1, dir)).approval).toBeNull();
  });
});

describe("answer", () => {
  // Catches: an answer posted as the agent, without the ask's current edited_at, or without the
  // stand-in's text.
  test("answers each line's ask as alice with its selection and text", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    dispatch.asks.push(ask({ id: "agent-open", edited_at: "2026-10-24T10:05:00Z" }));
    const dir = runDir();
    const answers = path.join(dir, "answers-1.jsonl");
    writeFileSync(
      answers,
      `${JSON.stringify({ ask: "agent-open", selected: ["Yes"], text: "Yes, do it." })}\n`
    );
    await seed.answer(fixture(dir, spec), answers);
    expect(dispatch.calls.filter((call) => call.method === "POST")).toEqual([
      {
        method: "POST",
        path: "/api/v1/asks/agent-open/answer",
        who: "alice",
        body: {
          selected: ["Yes"],
          text: "Yes, do it.",
          expected_edited_at: "2026-10-24T10:05:00Z",
        },
      },
    ]);
  });

  // Catches: a stand-in answer to a seeded ask, which would change the person's recorded input.
  test("refuses a line that answers a seeded ask, and posts nothing", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    dispatch.asks.push(ask({ id: "seeded-open" }));
    const dir = runDir();
    const answers = path.join(dir, "answers-1.jsonl");
    writeFileSync(answers, `${JSON.stringify({ ask: "seeded-open", selected: [], text: "x" })}\n`);
    await expect(seed.answer(fixture(dir, spec, ["seeded-open"]), answers)).rejects.toThrow(
      "seeded-open"
    );
    expect(writes()).toEqual([]);
  });
});

describe("approve", () => {
  // Catches: an approval sent as the agent, with another label than Dispatch's Approve, or to
  // another document's request.
  test("answers the spec's open approval request as alice with Approve", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    dispatch.asks.push(
      ask({
        id: "approval-plan",
        kind: "approval",
        approval: { artifact_id: "other", name: "plan.md", version: 1, requested_version: 1 },
      }),
      ask({
        id: "approval-spec",
        kind: "approval",
        edited_at: "2026-10-24T11:00:00Z",
        approval: { artifact_id: spec, name: "spec.md", version: 2, requested_version: 2 },
      })
    );
    const dir = runDir();
    await seed.approve(fixture(dir, spec));
    expect(dispatch.calls.filter((call) => call.method === "POST")).toEqual([
      {
        method: "POST",
        path: "/api/v1/asks/approval-spec/answer",
        who: "alice",
        body: { selected: ["Approve"], expected_edited_at: "2026-10-24T11:00:00Z" },
      },
    ]);
  });

  // Catches: an approve that succeeds with nothing to approve.
  test("fails when the spec has no open approval request", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    const dir = runDir();
    await expect(seed.approve(fixture(dir, spec))).rejects.toThrow("no open approval request");
  });
});

describe("capture-spec", () => {
  function world() {
    const spec = dispatch.issue("LWEVAL-2");
    const artifact = dispatch.artifacts.find((a) => a.id === spec);
    if (artifact === undefined) throw new Error("no spec");
    artifact.markdown = "## Summary\n\nVersion two.";
    artifact.versions = [
      { number: 1, created_at: "2026-10-24T10:00:00Z", authors: [runSession], markdown: "v1" },
      {
        number: 2,
        created_at: "2026-10-24T10:30:00Z",
        authors: [{ kind: "user", id: "alice@example.invalid" }, runSession],
        markdown: "## Summary\n\nVersion two.",
      },
    ];
    for (const [name, slug, kind, markdown] of [
      ["plan.md", "plan-md", "doc", "## Plan"],
      ["notes.md", "notes-md", "doc", "## Notes"],
      ["shot.png", "shot-png", "image", ""],
    ] as const) {
      dispatch.artifacts.push({
        id: dispatch.id("art"),
        issue_key: "LWEVAL-2",
        name,
        slug,
        kind,
        primary: false,
        markdown,
        versions: [],
      });
    }
    const alice = { kind: "user", id: "alice@example.invalid" };
    const message = (id: string, author: unknown, body: string, at: string) => ({
      id,
      issue_key: "LWEVAL-2",
      author,
      body,
      created_at: at,
    });
    dispatch.messages.push(
      {
        message: message("m-alice", alice, "Make it faster.", "2026-10-24T09:00:00Z"),
        replies: [],
      },
      {
        message: message("m-run", runSession, "I wrote a spec.", "2026-10-24T10:40:00Z"),
        replies: [message("m-reply", runSession, "And a plan.", "2026-10-24T10:50:00Z")],
      }
    );
    dispatch.comments.push(
      { ...message("c-run", runSession, "A note on R2.", "2026-10-24T10:45:00Z"), ask_id: null },
      {
        ...message("c-other", { kind: "session", id: "ses-other" }, "Hi.", "2026-10-24T10:46:00Z"),
        ask_id: null,
      }
    );
    dispatch.asks.push(
      // The rig opens a seeded ask with the run's own bearer; its session id is the rig's, but the
      // filter must not rest on that alone.
      ask({
        id: "seeded",
        author: runSession,
        state: "answered",
        created_at: "2026-10-24T08:00:00Z",
      }),
      ask({ id: "agent-ask", created_at: "2026-10-24T10:35:00Z" })
    );
    return spec;
  }

  // Catches: a capture that hands a seeded ask to the readers, misses the run's session's texts,
  // takes another session's, renders an ask body another way than fetch-spec-sources.ts, or files
  // plan.md or a picture among documents/.
  test("writes the run's capture files, the seeded asks left out", async () => {
    const spec = world();
    const dir = runDir();
    await seed.captureSpec(fixture(dir, spec, ["seeded"]), dir);
    const read = (name: string) => readFileSync(path.join(dir, name), "utf8");
    expect(read("spec.md")).toBe("## Summary\n\nVersion two.");
    expect(JSON.parse(read("spec-versions.json"))).toEqual([
      { version: 1, created_at: "2026-10-24T10:00:00Z", authors: [runSession], actor: runSession },
      {
        version: 2,
        created_at: "2026-10-24T10:30:00Z",
        authors: [{ kind: "user", id: "alice@example.invalid" }, runSession],
        actor: { kind: "user", id: "alice@example.invalid" },
      },
    ]);
    expect(read("spec-versions/1.md")).toBe("v1");
    expect(read("spec-versions/2.md")).toBe("## Summary\n\nVersion two.");
    expect(JSON.parse(read("agent-asks.json"))).toEqual([
      {
        id: "agent-ask",
        kind: "question",
        question: "Question of agent-ask?",
        options: [{ label: "Yes", description: "Do it." }, { label: "No" }],
        state: "open",
        answer: null,
        created_at: "2026-10-24T10:35:00Z",
      },
    ]);
    expect(JSON.parse(read("agent-texts.json"))).toEqual([
      {
        id: "agent-ask",
        kind: "ask",
        body: "Question of agent-ask?\nYes\nDo it.\nNo",
        origin_session_title: "Run",
        created_at: "2026-10-24T10:35:00Z",
      },
      {
        id: "m-run",
        kind: "message",
        body: "I wrote a spec.",
        origin_session_title: "Run",
        created_at: "2026-10-24T10:40:00Z",
      },
      {
        id: "c-run",
        kind: "comment",
        body: "A note on R2.",
        origin_session_title: "Run",
        created_at: "2026-10-24T10:45:00Z",
      },
      {
        id: "m-reply",
        kind: "message",
        body: "And a plan.",
        origin_session_title: "Run",
        created_at: "2026-10-24T10:50:00Z",
      },
    ]);
    expect(read("plan.md")).toBe("## Plan");
    expect(read("documents/notes-md.md")).toBe("## Notes");
    expect(existsSync(path.join(dir, "documents", "plan-md.md"))).toBe(false);
    expect(existsSync(path.join(dir, "documents", "shot-png.md"))).toBe(false);
    expect(existsSync(path.join(dir, "documents", "spec.md"))).toBe(false);
  });

  // Catches: captured texts sorted by Date.parse, which drops Dispatch's microseconds and keeps two
  // texts of one millisecond in the order the capture read them (asks, messages, comments).
  test("orders the run's texts at full precision", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    const at = (id: string, created_at: string) => ({
      id,
      issue_key: "LWEVAL-2",
      author: runSession,
      body: id,
      created_at,
    });
    dispatch.messages.push({
      message: at("m-later", "2026-10-24T10:40:00.000002Z"),
      replies: [],
    });
    dispatch.comments.push({ ...at("c-earlier", "2026-10-24T10:40:00.000001Z"), ask_id: null });
    const dir = runDir();
    await seed.captureSpec(fixture(dir, spec), dir);
    const texts = JSON.parse(readFileSync(path.join(dir, "agent-texts.json"), "utf8"));
    expect(texts.map((text: { id: string }) => text.id)).toEqual(["c-earlier", "m-later"]);
  });

  // Catches: a plan.md left from an earlier capture when the issue holds none now.
  test("writes no plan.md when the issue holds none", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    const dir = runDir();
    await seed.captureSpec(fixture(dir, spec), dir);
    expect(existsSync(path.join(dir, "plan.md"))).toBe(false);
    expect(JSON.parse(readFileSync(path.join(dir, "agent-texts.json"), "utf8"))).toEqual([]);
  });

  // Catches (dispatch://INFRA-1523, census row X.3, the architect's decision of 2026-11-21 10:44Z):
  // a post the agent under test deleted reaches agent-texts.json as an empty, unmarked text, which
  // the judges score as something the agent said. Dispatch reads a deleted post with its body empty
  // and its deleted_at set; the capture must leave it out or mark it deleted.
  test("a post the run deleted reaches the judges neither as an empty text nor unmarked", async () => {
    const spec = dispatch.issue("LWEVAL-2");
    const post = (id: string, body: string, at: string, deleted: boolean) => ({
      id,
      issue_key: "LWEVAL-2",
      author: runSession,
      body,
      created_at: at,
      deleted_at: deleted ? "2026-10-24T11:00:00Z" : null,
      deleted_by: deleted ? runSession : null,
    });
    dispatch.messages.push(
      { message: post("m-gone", "", "2026-10-24T10:40:00Z", true), replies: [] },
      { message: post("m-kept", "Still here.", "2026-10-24T10:41:00Z", false), replies: [] }
    );
    dispatch.comments.push({ ...post("c-gone", "", "2026-10-24T10:42:00Z", true), ask_id: null });
    const dir = runDir();
    await seed.captureSpec(fixture(dir, spec), dir);
    const texts: Record<string, unknown>[] = JSON.parse(
      readFileSync(path.join(dir, "agent-texts.json"), "utf8")
    );
    // The control: the run's standing message is captured as it reads.
    expect(texts.find((text) => text.id === "m-kept")?.body).toBe("Still here.");
    const unmarked = texts.filter(
      (text) =>
        (text.id === "m-gone" || text.id === "c-gone") &&
        !text.deleted &&
        (text.deleted_at === undefined || text.deleted_at === null)
    );
    expect(unmarked).toEqual([]);
  });
});

describe("tester-proof", () => {
  // Catches: a seeding that shares one issue between a batch's runs, or that the duplicate check
  // refuses on the second run.
  test("files the run's own greet() issue, forced past the duplicate check", async () => {
    const file = path.join(freshDir("seed"), "fixture.json");
    await seed.testerProofIssue(file);
    const posts = dispatch.calls.filter((call) => call.method === "POST");
    expect(`${posts[0]?.who} ${posts[0]?.path}`).toBe("agent /api/v1/issues");
    expect(posts[0]?.body).toMatchObject({ project: "LWEVAL", force: true });
    expect(String(posts[0]?.body?.spec)).toContain(":::requirement{#r1}");
    expect(String(posts[0]?.body?.spec)).not.toContain("## Acceptance");
    expect(`${posts[1]?.who} ${posts[1]?.path}`).toBe("agent /api/v1/issues/LWEVAL-2/artifacts");
    expect(String(posts[1]?.body?.content)).toContain(':::check{#k1 surface="cli"');
    expect(JSON.parse(readFileSync(file, "utf8"))).toEqual({ issue: "LWEVAL-2" });
  });

  // Catches: an implement record under the run's session (which the daemon would take as the
  // tester's own), a body re-encoded on the way, a fixture that forgets the record's version, or a
  // seed that leaves the tester no implementer result to judge.
  test("records the implement handoff and the implementer's result, under its own session", async () => {
    const dir = freshDir("seed");
    const file = path.join(dir, "fixture.json");
    await seed.testerProofIssue(file);
    const commit = "1111111111111111111111111111111111111111";
    const body = { head: commit };
    writeFileSync(path.join(dir, "implement.json"), JSON.stringify(body));
    await seed.implementHandoff(file, path.join(dir, "implement.json"), commit);
    expect(dispatch.handoffs).toEqual([
      expect.objectContaining({
        issue_key: "LWEVAL-2",
        phase: "implement",
        version: 1,
        author: seed.IMPLEMENTER,
        body: JSON.stringify(body),
      }),
    ]);
    expect(dispatch.results).toEqual([
      expect.objectContaining({
        issue_key: "LWEVAL-2",
        check: "k1",
        role: "implementer",
        verdict: "pass",
        commit_sha: commit,
        actor: seed.IMPLEMENTER,
      }),
    ]);
    expect(seed.IMPLEMENTER.id).not.toBe(runSession.id);
    expect(JSON.parse(readFileSync(file, "utf8"))).toEqual({
      issue: "LWEVAL-2",
      implement: { id: dispatch.handoffs[0]?.id, version: 1 },
    });
  });

  // Catches: a fixture whose body is no JSON object reaching Dispatch.
  test("refuses a body that is no JSON object, and writes nothing", async () => {
    const dir = freshDir("seed");
    const file = path.join(dir, "fixture.json");
    await seed.testerProofIssue(file);
    writeFileSync(path.join(dir, "implement.json"), "[1]");
    await expect(
      seed.implementHandoff(file, path.join(dir, "implement.json"), "1".repeat(40))
    ).rejects.toThrow("holds no JSON object");
    expect(dispatch.handoffs).toEqual([]);
    expect(dispatch.results).toEqual([]);
  });

  // Catches: a capture that keeps only the metadata Dispatch lists, or loses the newest-first order
  // the score reads the run's own newest test record by.
  test("captures every record of the issue, newest first, each with its body", async () => {
    const dir = runDir();
    const file = path.join(dir, "fixture.json");
    await seed.testerProofIssue(file);
    writeFileSync(path.join(dir, "implement.json"), JSON.stringify({ phase: "implement" }));
    await seed.implementHandoff(file, path.join(dir, "implement.json"), "2".repeat(40));
    dispatch.handoffs.push({
      id: "h-test",
      issue_key: "LWEVAL-2",
      phase: "test",
      version: 1,
      author: runSession,
      created_at: "2026-10-24T10:50:00.000001Z",
      body: '{"phase":"test"}',
    });
    await seed.captureHandoffs(file, dir);
    const captured = JSON.parse(readFileSync(path.join(dir, "handoffs.json"), "utf8"));
    expect(
      captured.map((record: { id: string; body: string }) => [record.id, record.body])
    ).toEqual([
      ["h-test", '{"phase":"test"}'],
      [dispatch.handoffs[0]?.id, '{"phase":"implement"}'],
    ]);
  });
});

describe("case-world Dispatch", () => {
  const example = (name: string) =>
    readFileSync(path.join(import.meta.dir, "testdata", `${name}-case-example.json`), "utf8");
  /** P05's world as the example held-out file holds it, parsed afresh on each call. */
  const p05World = (): World => {
    const p05 = parseHeldoutFile(example("heldout")).cases.find(({ id }) => id === "P05");
    if (!p05) throw new Error("missing P05");
    return p05.world;
  };

  test("allocates P05's issue and parent and writes its predecessor's handoff under that session", async () => {
    const world = p05World();
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    await seed.seedCaseDispatch(dir);
    const creates = dispatch.calls.filter(
      ({ method, path }) => method === "POST" && path === "/api/v1/issues"
    );
    expect(creates).toHaveLength(12);
    expect(creates[9]?.body?.title).toBe("Parent of ACME-12");
    const { status, key, ...issue } = world.dispatch.issue;
    expect(creates.at(-1)?.body).toMatchObject(issue);
    expect(dispatch.calls).toContainEqual(
      expect.objectContaining({
        path: `/api/v1/issues/${key}/status-change`,
        body: expect.objectContaining({ status }),
      })
    );
    expect(dispatch.handoffs[0]).toMatchObject({
      issue_key: key,
      phase: "plan",
      version: 1,
      author: { kind: "session", id: "ses-acme-12-planner" },
      body: JSON.stringify(world.dispatch.handoffs[0]?.body),
    });
  });

  test("seeds P09's results, comments and asks and reads every page with its time", async () => {
    const p09 = parseSkillUseFile(example("skilluse")).cases.find(({ id }) => id === "P09");
    if (!p09) throw new Error("missing P09");
    const world = structuredClone(p09.world);
    const at = "2030-01-09T09:15:00.123456Z";
    world.dispatch.comments.push({ id: "c1", author: "alice", body: "Keep the plan.", at });
    world.dispatch.asks.push(
      {
        id: "a1",
        author: "ses-qa",
        question: "Keep it?",
        options: ["Yes", "No"],
        state: "answered",
        answer: "Yes",
        at,
      },
      {
        id: "a2",
        author: "ses-qa",
        question: "Still needed?",
        options: [],
        state: "dismissed",
        answer: null,
        at,
      }
    );
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    dispatch.events.push({ seq: 1, type: "issue.created", created_at: at });
    await seed.seedCaseDispatch(dir);
    expect(dispatch.comments).toMatchObject([{ body: "Keep the plan." }]);
    expect(dispatch.asks.map(({ state }) => state)).toEqual(["answered", "resolved"]);
    expect(dispatch.asks[0]?.answer).toMatchObject({ selected: ["Yes"], text: "Yes" });
    expect(dispatch.results).toHaveLength(world.dispatch.results.length);
    for (let seq = 2; seq <= 201; seq += 1)
      dispatch.events.push({ seq, type: "comment.created", created_at: at });
    await seed.captureCaseDispatch(dir);
    const data = JSON.parse(readFileSync(path.join(dir, "dispatch-readback.json"), "utf8"));
    expect(data.seedThrough).toBe(1);
    expect(data.events).toEqual(dispatch.events);
    expect(data.documents).toContainEqual(
      expect.objectContaining({
        name: "verification.md",
        text: world.dispatch.documents[0]?.body,
      })
    );
    expect(data.results.checks[0].results).toEqual(dispatch.results);
    expect(data.handoffs).toHaveLength(world.dispatch.handoffs.length);
  });

  // Catches: a case's check that serves a requirement by its bare id (`#r1`), which the real
  // Dispatch refuses (INVALID_MARKDOWN) and so seeded no world at all in the first lane proof.
  test("writes a case's block references as Dispatch writes them", async () => {
    expect(
      seed.dispatchBlockRefs(
        'serves="dispatch://ACME-12/spec#r1" dispatch://ACME-12/spec#b-r2 dispatch://ACME-12/ask/a1',
        "ACME-12"
      )
    ).toBe(
      'serves="dispatch://ACME-12/spec#b-r1" dispatch://ACME-12/spec#b-r2 dispatch://ACME-12/ask/a1'
    );
    const world = p05World();
    const verification = world.dispatch.documents[0];
    if (!verification) throw new Error("P05 has no Verification document");
    verification.body = verification.body.replace(
      "{#k1 ",
      '{#k1 serves="dispatch://ACME-12/spec#r1" '
    );
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    await seed.seedCaseDispatch(dir);
    const uploaded = dispatch.calls.find(({ path }) => path === "/api/v1/issues/ACME-12/artifacts");
    expect(String(uploaded?.body?.content)).toContain('serves="dispatch://ACME-12/spec#b-r1"');
  });

  // Catches: a check or a plan task that names a block by its bare id (`serves="r1"`,
  // `needs="t1"`), which the real Dispatch refuses (400 INVALID_MARKDOWN: a pointer is a canonical
  // block reference), so the skill-use and real-daemon Jobs seeded no world; and a rewrite that
  // reaches a quoted value, another attribute or block, prose or fenced code, or a reference
  // already canonical, or that leaves a pin before the fragment (`plan-md@v3#b-step-1`).
  test("writes each bare id of a check's or a plan task's pointers as the reference Dispatch writes", async () => {
    const authored = [
      ":::requirement{#r1}",
      "The reader retries.",
      ":::",
      "",
      ':::check{#k1 serves="r1 dispatch://ACME-12/spec#r2 dispatch://ACME-12/spec#b-r3 dispatch://ACME-12/artifact/decisions-md@v3#d1" surface="cli" run="go test ./... -run r1" negative_control="A check that serves r1 alone fails."}',
      "It serves r1.",
      ":::",
      "",
      ':::plan_task{#t2 serves="r1" needs="t1 dispatch://ACME-12/artifact/plan-md@v3#step-1"}',
      "After t1.",
      ":::",
      "",
      ':::callout{#c1 serves="r1"}',
      "Not a pointer of a check or a plan task.",
      ":::",
      "",
      "```md",
      ':::check{#k9 serves="r9"}',
      "```",
      "",
      'Prose with serves="r1" needs="t1".',
    ].join("\n");
    const written = (needs: string) =>
      authored
        .replace(
          'serves="r1 dispatch://ACME-12/spec#r2 dispatch://ACME-12/spec#b-r3 dispatch://ACME-12/artifact/decisions-md@v3#d1"',
          'serves="dispatch://ACME-12/spec#b-r1 dispatch://ACME-12/spec#b-r2 dispatch://ACME-12/spec#b-r3 dispatch://ACME-12/artifact/decisions-md#b-d1@v3"'
        )
        .replace(
          ':::plan_task{#t2 serves="r1" needs="t1 dispatch://ACME-12/artifact/plan-md@v3#step-1"}',
          `:::plan_task{#t2 serves="dispatch://ACME-12/spec#b-r1" needs="${needs} dispatch://ACME-12/artifact/plan-md#b-step-1@v3"}`
        );
    expect(seed.dispatchBlockRefs(authored, "ACME-12", "plan-md")).toBe(
      written("dispatch://ACME-12/artifact/plan-md#b-t1")
    );
    expect(seed.dispatchBlockRefs(authored, "ACME-12", "spec")).toBe(
      written("dispatch://ACME-12/spec#b-t1")
    );
    // A comment or an ask is no document, so a bare `needs` names no block of one.
    expect(seed.dispatchBlockRefs(authored, "ACME-12")).toBe(written("t1"));

    // The refused input as the seed uploads it: each document under its own slug.
    const world = p05World();
    const verification = world.dispatch.documents[0];
    if (!verification) throw new Error("P05 has no Verification document");
    const check = verification.body.replace("{#k1 ", '{#k1 serves="r1" ');
    const plan = [
      ':::plan_task{#t1 serves="r1"}',
      "First.",
      ":::",
      "",
      ':::plan_task{#t2 serves="r1" needs="t1"}',
      "After t1.",
      ":::",
      "",
    ].join("\n");
    verification.body = check;
    world.dispatch.documents.push({ name: "plan.md", body: plan });
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    await seed.seedCaseDispatch(dir);
    const uploads = dispatch.calls
      .filter(({ path }) => path === "/api/v1/issues/ACME-12/artifacts")
      .map(({ body }) => [body?.name, body?.content]);
    expect(uploads).toEqual([
      ["verification.md", check.replace('serves="r1"', 'serves="dispatch://ACME-12/spec#b-r1"')],
      [
        "plan.md",
        plan
          .replaceAll('serves="r1"', 'serves="dispatch://ACME-12/spec#b-r1"')
          .replace('needs="t1"', 'needs="dispatch://ACME-12/artifact/plan-md#b-t1"'),
      ],
    ]);
  });

  // Catches: P02's task names its tester's comment as `dispatch://ACME-41/comment/c-1`, the case's
  // own id, which Dispatch refuses (a comment id is a UUID), so the role could not read the comment
  // its task points to.
  test("names in the run's prompt the id Dispatch gave each comment and ask the case names", async () => {
    const p09 = parseSkillUseFile(example("skilluse")).cases.find(({ id }) => id === "P09");
    if (!p09) throw new Error("missing P09");
    const world = structuredClone(p09.world);
    const key = world.dispatch.issue.key;
    const at = "2030-01-09T09:15:00Z";
    world.dispatch.comments.push({ id: "c-1", author: "alice", body: "Which policy?", at });
    world.dispatch.asks.push({
      id: "a1",
      author: "ses-qa",
      question: "Keep it?",
      options: ["Yes"],
      state: "open",
      answer: null,
      at,
    });
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    writeFileSync(
      path.join(dir, "prompt.txt"),
      `From the tester: dispatch://${key}/comment/c-1. See dispatch://${key}/ask/a1 and dispatch://ACME-1/comment/c-1.\n`
    );
    await seed.seedCaseDispatch(dir);
    const comment = dispatch.comments.find(({ body }) => body === "Which policy?");
    const ask = dispatch.asks.find(({ question }) => question === "Keep it?");
    expect(readFileSync(path.join(dir, "prompt.txt"), "utf8")).toBe(
      `From the tester: dispatch://${key}/comment/${comment?.id}. See dispatch://${key}/ask/${ask?.id} and dispatch://ACME-1/comment/c-1.\n`
    );
    // The fixture keeps each id it gave, under the world's, for outcome.ts to write back.
    expect(
      JSON.parse(readFileSync(path.join(dir, "dispatch-fixture.json"), "utf8")).ids
    ).toMatchObject({ "comment/c-1": comment?.id, "ask/a1": ask?.id });
  });

  // Catches: a document version whose text no readback holds, so the outcome could carry no body
  // for a document the run wrote, and lane D's replay could not make the document again at a
  // version an event wrote, the seed's first version of a document the run then edited among them.
  // The readback reads the text of each version of a document of the issue that an event wrote,
  // the seed's events included, once each, through Dispatch's own version route, and of no other.
  test("the readback holds the text of each document version an event wrote", async () => {
    const p09 = parseSkillUseFile(example("skilluse")).cases.find(({ id }) => id === "P09");
    if (!p09) throw new Error("missing P09");
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(p09.world));
    const at = "2030-01-09T09:15:00Z";
    dispatch.events.push({ seq: 1, type: "issue.created", created_at: at });
    await seed.seedCaseDispatch(dir);
    const seeded = dispatch.artifacts.find(({ name }) => name === "verification.md");
    if (seeded === undefined) throw new Error("P09's verification.md was not seeded");
    const version = (number: number, markdown: string) => ({
      number,
      created_at: at,
      authors: [runSession],
      markdown,
    });
    seeded.versions = [version(1, "seeded"), version(2, "the run's second"), version(3, "third")];
    // The seed's own write of the document, at its first version, comes before the seed cursor.
    const fixture = path.join(dir, "dispatch-fixture.json");
    writeFileSync(
      fixture,
      JSON.stringify({ ...JSON.parse(readFileSync(fixture, "utf8")), seedThrough: 2 })
    );
    dispatch.events.push(
      {
        seq: 2,
        type: "artifact.created",
        created_at: at,
        ...{
          payload: { artifact: { id: seeded.id, name: seeded.name, versions: [{ number: 1 }] } },
        },
      },
      {
        seq: 3,
        type: "artifact.version",
        created_at: at,
        ...{ payload: { artifact_id: seeded.id, name: seeded.name, version: { number: 2 } } },
      },
      {
        seq: 4,
        type: "artifact.version",
        created_at: at,
        ...{ payload: { artifact_id: seeded.id, name: seeded.name, version: { number: 3 } } },
      },
      {
        seq: 5,
        type: "artifact.version",
        created_at: at,
        ...{ payload: { artifact_id: seeded.id, name: seeded.name, version: { number: 3 } } },
      },
      { seq: 6, type: "comment.created", created_at: at }
    );
    await seed.captureCaseDispatch(dir);
    const data = JSON.parse(readFileSync(path.join(dir, "dispatch-readback.json"), "utf8"));
    expect(data.seedThrough).toBe(2);
    expect(data.versions).toEqual([
      { artifact_id: seeded.id, number: 1, markdown: "seeded" },
      { artifact_id: seeded.id, number: 2, markdown: "the run's second" },
      { artifact_id: seeded.id, number: 3, markdown: "third" },
    ]);
    expect(
      dispatch.calls.filter(({ path }) =>
        path.startsWith(`/api/v1/artifacts/${seeded.id}/versions/`)
      )
    ).toHaveLength(3);
  });

  const at = (time: string) => `2030-01-07T${time}Z`;
  /** P05's world with the runs of its task's steps around its records, as a reviewer's world
   * holds them: the planner's, the implementer's and the tester's runs ended and the reviewer's
   * running, each holding the records of its time. The implementer's comment is on words of
   * plan.md and the architect's a reply in its thread; the tester wrote two versions of its handoff
   * 20 s apart; and the implementer asks at the very moment its run starts, when the planner's
   * ends. */
  function steppedWorld(): World {
    const world = p05World();
    const handoff = (phase: "implement" | "test", version: number, role: string, time: string) => ({
      id: `hnd-acme-12-${phase}-${version}`,
      phase,
      version,
      author: `ses-acme-12-${role}`,
      at: at(time),
      body: { summary: `The ${phase} handoff, version ${version}.` },
    });
    const ended = (step: number, role: string, from: string, to: string): StepRun => ({
      task: "t1",
      step,
      kind: "step",
      role,
      result: "passed",
      startedAt: at(from),
      endedAt: at(to),
    });
    world.dispatch.documents.push({
      name: "plan.md",
      body: "# Plan of ACME-12\n\nCarry both fields. The flag's default is 3.\n",
    });
    world.dispatch.handoffs.push(
      handoff("implement", 1, "implementer", "13:50:00"),
      handoff("test", 1, "tester", "14:20:00"),
      handoff("test", 2, "tester", "14:20:20")
    );
    world.dispatch.comments.push(
      {
        id: "cmt-acme-12-departure",
        author: "ses-acme-12-implementer",
        at: at("13:35:00"),
        body: "For the architect: the default is 1, not the 3 the plan asks for.",
        anchor: { document: "plan.md", quote: "The flag's default is 3." },
      },
      {
        id: "cmt-acme-12-decision",
        author: "ses-acme-12-architect",
        at: at("13:37:00"),
        body: "Take it.",
        replyTo: "cmt-acme-12-departure",
      }
    );
    world.dispatch.asks.push({
      id: "ask-acme-12-default",
      author: "ses-acme-12-implementer",
      at: at("13:10:30"),
      question: "Which default?",
      options: ["3", "1"],
      state: "open",
      answer: null,
    });
    world.dispatch.results.push({
      check: "k1",
      role: "implementer",
      verdict: "pass",
      commit: "1f3a5c7e9b2d4f6a8c0e1b3d5f7a9c2e4b6d8f0a",
      at: at("13:48:00"),
    });
    world.dispatch.steps = [
      ended(1, "planner", "12:40:00", "13:10:30"),
      ended(2, "implementer", "13:10:30", "13:50:30"),
      ended(3, "tester", "13:50:30", "14:21:00"),
      {
        task: "t1",
        step: 4,
        kind: "step",
        role: "reviewer",
        result: null,
        startedAt: at("14:21:00"),
        endedAt: null,
      },
    ];
    return World.parse(world);
  }
  /** A run directory whose launch-world.json is `world`, seeded on `clock`: by default one that
   * moves only when the seed sleeps (heldClock). */
  async function seeded(world: World, clock = heldClock()): Promise<string> {
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    await seed.seedCaseDispatch(dir, clock);
    return dir;
  }
  /** heldClock, with each wait the seed asks of it, in milliseconds. */
  const sleptClock = () => {
    const held = heldClock();
    const sleeps: number[] = [];
    const clock: seedModule.SeedClock = {
      now: held.now,
      sleep: async (ms) => {
        sleeps.push(ms);
        await held.sleep(ms);
      },
    };
    return { clock, sleeps };
  };
  const handoffId = (phase: string, version: number) =>
    dispatch.handoffs.find((record) => record.phase === phase && record.version === version)?.id;

  // Catches: a step run stamped when the seed wrote it, after every record, so that the reviewer
  // finds no handoff inside any run; and a record at the moment one run ends and the next starts
  // that lands in both runs, or in neither.
  test("places every seeded record whose case time falls inside a step run inside the run the read-back captures, and every other one outside it", async () => {
    const world = steppedWorld();
    const dir = await seeded(world);
    await seed.captureCaseDispatch(dir);
    const readback = JSON.parse(readFileSync(path.join(dir, "dispatch-readback.json"), "utf8"));
    expect(readback.steps.issue).toBe("ACME-12");
    expect(readback.steps.tasks.map(({ task }: { task: string }) => task)).toEqual(["t1"]);
    // Each run as the read-back captures it, in the order Dispatch lists it, the world's order.
    const listed: seedModule.RunTimes[] = readback.steps.tasks[0].steps.map(
      (run: { started_at: string; ended_at: string | null }) => ({
        startedAt: run.started_at,
        endedAt: run.ended_at,
      })
    );
    // Each record with the stamp the read-back captures of it, found by the id the fixture maps
    // its own to, and a result by its commit.
    const { ids } = JSON.parse(readFileSync(path.join(dir, "dispatch-fixture.json"), "utf8"));
    const rows: { id: string; created_at: string }[] = [
      ...readback.handoffs,
      ...readback.comments,
      ...readback.asks,
    ];
    const checked: { commit_sha: string; created_at: string }[] =
      readback.results.checks[0].results;
    const records = caseRecords(world).map((record) => {
      const result = world.dispatch.results.find(
        (_, index) => record.id === `world.dispatch.results[${index}]`
      );
      const given = ["handoff", "comment", "ask"].map((kind) => ids[`${kind}/${record.id}`]);
      const row =
        result === undefined
          ? rows.find(({ id }) => given.includes(id))
          : checked.find(({ commit_sha }) => commit_sha === result.commit);
      return { ...record, stamp: ns(String(row?.created_at)) };
    });
    const runs = world.dispatch.steps ?? [];
    // On the case's clock the ask at the planner's end, the implementer's start, is the
    // implementer's alone.
    expect(holdings(runs, records)).toEqual([
      ["hnd-acme-12-plan-1"],
      [
        "ask-acme-12-default",
        "cmt-acme-12-departure",
        "cmt-acme-12-decision",
        "world.dispatch.results[0]",
        "hnd-acme-12-implement-1",
      ],
      ["hnd-acme-12-test-1", "hnd-acme-12-test-2"],
      [],
    ]);
    // In the run's Dispatch, each run the read-back captures holds the same records.
    expect(holdings(runs, records, listed)).toEqual(holdings(runs, records));
  });

  // Catches: a read-back that takes any time Dispatch lists once the seed placed the times, so a
  // run Dispatch holds at the world's own instant, where the records of its time do not lie inside
  // it, goes unseen.
  test("a run Dispatch lists at another instant than the seed wrote fails the seed, naming it", async () => {
    const world = steppedWorld();
    const implementer = world.dispatch.steps?.[1];
    if (implementer === undefined) throw new Error("steppedWorld lost the implementer's run");
    dispatch.misreadRun = (run) =>
      run.role === "implementer" ? { ...run, started_at: implementer.startedAt } : run;
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    const error = await seed.seedCaseDispatch(dir, heldClock()).then(
      () => undefined,
      (thrown: Error) => thrown
    );
    const placed = dispatch.runs[1]?.started_at;
    expect(placed).toBeString();
    expect(error?.message).toContain(
      `world.dispatch.steps[1]: Dispatch lists {"task":"t1","step":2,"kind":"step","role":"implementer","result":"passed","rating":null,"reason":null,"startedAt":"${implementer.startedAt}"`
    );
    expect(error?.message).toContain(`, and the seed wrote {"task":"t1","step":2,`);
    expect(error?.message).toContain(`"startedAt":"${placed}"`);
    expect(error?.message).toContain(
      "the world's run at the instants it placed among the records' stamps"
    );
    expect(error?.message).not.toContain("world.dispatch.steps[0]");
    expect(existsSync(path.join(dir, "dispatch-fixture.json"))).toBe(false);
  });

  /** P05's world with four more versions of its plan handoff after its version 1 at 13:10:00:
   * 20 s, 60 s and 61 s apart, then 20 s with a comment between. */
  const gappedWorld = (): World => {
    const world = p05World();
    const plan = (version: number, time: string) => ({
      id: `hnd-acme-12-plan-${version}`,
      phase: "plan" as const,
      version,
      author: "ses-acme-12-planner",
      at: at(time),
      body: { summary: `Version ${version}.` },
    });
    world.dispatch.handoffs.push(
      plan(2, "13:10:20"),
      plan(3, "13:11:20"),
      plan(4, "13:12:21"),
      plan(5, "13:12:41")
    );
    world.dispatch.comments.push({
      id: "cmt-acme-12-between",
      author: "alice",
      at: at("13:12:31"),
      body: "Between two versions.",
    });
    return world;
  };
  /** The whole seconds between Dispatch's stamps of each two handoffs seeded one after another. */
  const handoffGaps = () => {
    const stamps = dispatch.handoffs.map(({ created_at }) => ns(created_at));
    return stamps.slice(1).map((stamp, index) => (stamp - (stamps[index] ?? 0n)) / 1_000_000_000n);
  };

  // Catches: a seed that writes two versions of a handoff a moment apart where the case holds
  // them 20 s apart, so the reviewer reads no double write; and one that keeps every gap of the
  // case, which would take a run hours.
  test("keeps a gap of 60 s or less between two handoffs and no longer gap", async () => {
    const { clock, sleeps } = sleptClock();
    await seeded(gappedWorld(), clock);
    expect(sleeps).toEqual([20_000, 60_000]);
    expect(handoffGaps()).toEqual([20n, 60n, 0n, 0n]);
  });

  // Catches: a seed that waits both a kept gap and the millisecond before a later record in a world
  // with step runs, so each kept gap grows by that millisecond; and one that keeps no gap there.
  test("where the world has step runs, waits a kept gap in place of the millisecond before a later record, not both", async () => {
    const world = gappedWorld();
    // The run ends between the comment and the last version, so no placed time is in the future.
    world.dispatch.steps = [
      stepRun(1, "step", "planner", "2030-01-07T13:00:00Z", "2030-01-07T13:12:35Z"),
    ];
    const { clock, sleeps } = sleptClock();
    await seeded(world, clock);
    expect(sleeps).toEqual([1, 20_000, 60_000, 1, 1, 1]);
    expect(handoffGaps()).toEqual([20n, 60n, 0n, 0n]);
  });

  // Catches: the departure posted as a plain comment on the issue and the decision as another,
  // so the thread on plan.md's words the reviewer reads holds neither.
  test("seeds an anchored comment on its document's quote and a reply in its thread", async () => {
    await seeded(steppedWorld());
    const plan = dispatch.artifacts.find(({ name }) => name === "plan.md")?.id;
    const posts = dispatch.calls
      .filter(({ method, path }) => method === "POST" && path === "/api/v1/issues/ACME-12/comments")
      .map(({ body }) => body);
    const quote = "The flag's default is 3.";
    expect(posts).toEqual([
      expect.objectContaining({
        body: "For the architect: the default is 1, not the 3 the plan asks for.",
        anchor: { artifact: plan, quote },
      }),
      expect.objectContaining({ body: "Take it.", reply_to: dispatch.comments[0]?.id }),
    ]);
    expect(posts[1]).not.toHaveProperty("anchor");
    expect(dispatch.comments).toMatchObject([
      { artifact_id: null, anchor: { artifact_id: plan, quote }, reply_to: null },
      { artifact_id: null, reply_to: dispatch.comments[0]?.id },
    ]);
  });

  // Catches: a seed that keeps no handoff's id, so that the outcome names a handoff by the id
  // Dispatch gave it where the rubric names it by the world's.
  test("writes the id of every seeded handoff, comment and ask to the fixture's ids", async () => {
    const dir = await seeded(steppedWorld());
    const comment = (body: string) => dispatch.comments.find((row) => row.body === body)?.id;
    const ask = dispatch.asks.find(({ question }) => question === "Which default?")?.id;
    expect(JSON.parse(readFileSync(path.join(dir, "dispatch-fixture.json"), "utf8")).ids).toEqual({
      "handoff/hnd-acme-12-plan-1": handoffId("plan", 1),
      "handoff/hnd-acme-12-implement-1": handoffId("implement", 1),
      "handoff/hnd-acme-12-test-1": handoffId("test", 1),
      "handoff/hnd-acme-12-test-2": handoffId("test", 2),
      "comment/cmt-acme-12-departure": comment(
        "For the architect: the default is 1, not the 3 the plan asks for."
      ),
      "comment/cmt-acme-12-decision": comment("Take it."),
      "ask/ask-acme-12-default": ask,
    });
  });

  /** P05's world with its Dispatch parts: the issue's labels, two other issues (ACME-13 under the
   * case's parent, waiting on ACME-14, claimed and second on the board, and ACME-14, first), and
   * three step runs (a passed step, a rated retro, a merge that runs). */
  const partsWorld = () => {
    const p05 = parseHeldoutFile(example("heldout")).cases.find(({ id }) => id === "P05");
    if (!p05) throw new Error("missing P05");
    const world = structuredClone(p05.world);
    world.dispatch.issue.labels = ["legion"];
    world.dispatch.issues = [
      {
        key: "ACME-13",
        title: "A sibling",
        status: "todo",
        parent: "ACME-10",
        rank: 2,
        labels: ["ux"],
        blockedBy: ["ACME-14"],
        claimedBy: "ses-acme-13",
      },
      {
        key: "ACME-14",
        status: "in_progress",
        parent: null,
        rank: 1,
        labels: [],
        blockedBy: [],
        claimedBy: null,
      },
    ];
    const at = (minute: number) => `2030-01-07T14:${String(minute).padStart(2, "0")}:00Z`;
    world.dispatch.steps = [
      {
        task: "default",
        step: 1,
        kind: "step",
        role: "planner",
        result: "passed",
        startedAt: at(0),
        endedAt: at(10),
      },
      {
        task: "default",
        step: 0,
        kind: "retro",
        role: "implementer",
        result: "passed",
        startedAt: at(20),
        endedAt: at(25),
        planRating: 4,
        planReason: "The plan fit.",
      },
      {
        task: "default",
        step: 0,
        kind: "merge",
        role: "merger",
        result: null,
        startedAt: at(30),
        endedAt: null,
      },
    ];
    return world;
  };

  /** Nanoseconds since the epoch of the RFC 3339 time `text`. */
  const ns = (text: string): bigint => {
    const at = parseInstant(text);
    if (at === undefined) throw new Error(`${text} is no RFC 3339 time`);
    return at;
  };
  /** A step run of task `default`, passed where it ended unless `result` says otherwise; one with
   * no end still runs. */
  const stepRun = (
    step: number,
    kind: StepRun["kind"],
    role: string,
    startedAt: string,
    endedAt: string | null,
    result: StepRun["result"] = endedAt === null ? null : "passed"
  ): StepRun => ({ task: "default", step, kind, role, result, startedAt, endedAt });

  /** P05's world (T0's public example) reshaped as H08 is: a planner, an implementer, a tester
   * whose run failed, a second implementer run, a second tester run, a reviewer, and a retro that
   * still runs, each run holding its handoffs and results. Comment c-1 comes at the failed tester
   * run's very end, so between two runs and in neither; comment c-2 at the second implementer
   * run's very start, so inside it; and ask a1 inside the retro. */
  const h08World = (): World => {
    const p05 = parseHeldoutFile(example("heldout")).cases.find(({ id }) => id === "P05");
    if (!p05) throw new Error("missing P05");
    const world = structuredClone(p05.world);
    const at = (time: string) => `2030-01-09T${time}Z`;
    const handoff = (
      phase: "plan" | "implement" | "test" | "review",
      version: number,
      author: string,
      time: string
    ) => ({ id: `hnd-${phase}-${version}`, phase, version, author, at: at(time), body: { phase } });
    world.dispatch.handoffs = [
      handoff("plan", 1, "ses-planner", "10:15:00"),
      handoff("implement", 1, "ses-implementer", "10:55:00"),
      handoff("test", 1, "ses-tester", "11:28:00"),
      handoff("implement", 2, "ses-implementer-2", "11:58:00"),
      handoff("test", 2, "ses-tester-2", "12:28:00"),
      handoff("test", 3, "ses-tester-2", "12:28:20"),
      handoff("review", 1, "ses-reviewer", "12:45:00"),
    ];
    world.dispatch.results = [
      { check: "k1", role: "tester", verdict: "fail", commit: "a".repeat(40), at: at("11:20:00") },
      { check: "k1", role: "tester", verdict: "pass", commit: "b".repeat(40), at: at("12:20:00") },
    ];
    world.dispatch.comments = [
      { id: "c-1", author: "alice", body: "Between the runs.", at: at("11:30:00") },
      { id: "c-2", author: "ses-implementer-2", body: "Starting again.", at: at("11:35:00") },
    ];
    world.dispatch.asks = [
      {
        id: "a1",
        author: "ses-retro",
        question: "Rate the plan?",
        options: ["Yes"],
        state: "open",
        answer: null,
        at: at("12:58:00"),
      },
    ];
    world.dispatch.steps = [
      stepRun(1, "step", "planner", at("10:00:00"), at("10:20:00")),
      stepRun(2, "step", "implementer", at("10:25:00"), at("11:00:00")),
      stepRun(3, "step", "tester", at("11:05:00"), at("11:30:00"), "failed"),
      stepRun(2, "step", "implementer", at("11:35:00"), at("12:00:00")),
      stepRun(3, "step", "tester", at("12:05:00"), at("12:30:00")),
      stepRun(4, "step", "reviewer", at("12:35:00"), at("12:50:00")),
      stepRun(0, "retro", "implementer", at("12:55:00"), null),
    ];
    return world;
  };
  /** The records each run of h08World holds on the case's clock, as the seed names them. */
  const h08Holds = [
    ["hnd-plan-1"],
    ["hnd-implement-1"],
    ["world.dispatch.results[0]", "hnd-test-1"],
    ["c-2", "hnd-implement-2"],
    ["world.dispatch.results[1]", "hnd-test-2", "hnd-test-3"],
    ["hnd-review-1"],
    ["a1"],
  ];

  /** The world's records as the seed names them, in case-time order: each handoff, comment and
   * ask by its id, and each result as `world.dispatch.results[<i>]`. */
  const caseRecords = (world: World) =>
    [
      ...world.dispatch.handoffs,
      ...world.dispatch.comments,
      ...world.dispatch.asks,
      ...world.dispatch.results.map(({ at }, index) => ({
        id: `world.dispatch.results[${index}]`,
        at,
      })),
    ]
      .map(({ id, at }) => ({ id, at: ns(at) }))
      .sort((a, b) => (a.at < b.at ? -1 : a.at > b.at ? 1 : 0));
  /** `records`, each stamped `gap` nanoseconds after the one before it, the first `gap` after
   * `created`. */
  const stampedAfter = (
    records: readonly { id: string; at: bigint }[],
    created: bigint,
    gap = 1_000_000n
  ): seedModule.Stamped[] =>
    records.map((record, index) => ({ ...record, stamp: created + BigInt(index + 1) * gap }));
  /** The world's records with the stamps the fake gave them, as the seed names them. */
  const fakeStamps = (world: World): seedModule.Stamped[] =>
    caseRecords(world).map((record) => {
      const handoff = world.dispatch.handoffs.find(({ id }) => id === record.id);
      const comment = world.dispatch.comments.find(({ id }) => id === record.id);
      const ask = world.dispatch.asks.find(({ id }) => id === record.id);
      const result = world.dispatch.results.find(
        (_, index) => record.id === `world.dispatch.results[${index}]`
      );
      const row = handoff
        ? dispatch.handoffs.find(
            ({ phase, version }) => phase === handoff.phase && version === handoff.version
          )
        : comment
          ? dispatch.comments.find(({ body }) => body === comment.body)
          : ask
            ? dispatch.asks.find(({ question }) => question === ask.question)
            : dispatch.results.find(({ commit_sha }) => commit_sha === result?.commit);
      return { ...record, stamp: ns(String(row?.created_at)) };
    });
  /** The start and end of each run the fake holds, as the seed wrote them. */
  const fakeRuns = (): seedModule.RunTimes[] =>
    dispatch.runs.map(({ started_at, ended_at }) => ({
      startedAt: String(started_at),
      endedAt: ended_at === undefined ? null : String(ended_at),
    }));
  /** The names of the records each of `runs` holds: those whose case time is inside the run's
   * window in the world or, given `written` (runTimes), those whose stamp is inside the window the
   * seed wrote. A window runs from its start up to its end, with no end for a run that has not
   * ended. */
  const holdings = (
    runs: readonly StepRun[],
    records: readonly seedModule.Stamped[],
    written?: readonly seedModule.RunTimes[]
  ) =>
    runs.map((run, index) => {
      const { startedAt, endedAt } = written?.[index] ?? run;
      const from = ns(startedAt);
      const to = endedAt === null ? undefined : ns(endedAt);
      return records
        .filter(({ at, stamp }) => {
          const when = written === undefined ? at : stamp;
          return from <= when && (to === undefined || when < to);
        })
        .map(({ id }) => id);
    });
  /** Each start and end of `written`, in nanoseconds, in world order. */
  const instants = (written: readonly seedModule.RunTimes[]) =>
    written
      .flatMap(({ startedAt, endedAt }) => (endedAt === null ? [startedAt] : [startedAt, endedAt]))
      .map(ns);
  /** Whether each of `values` is after the one before it. */
  const rising = (values: readonly bigint[]) =>
    values.every((value, index) => index === 0 || (values[index - 1] ?? value) < value);
  /** A clock that moves only when the seed sleeps, which the fake stamps from and the seed reads
   * and sleeps on: two records the seed does not wait between share a stamp. */
  const heldClock = (): seedModule.SeedClock => {
    let at = ns("2026-11-27T16:36:00Z");
    dispatch.now = () => at;
    return {
      now: () => at,
      sleep: async (ms) => {
        at += BigInt(ms) * 1_000_000n;
      },
    };
  };

  test("files the world's other issues and step runs as the daemon writes them, and reads each back", async () => {
    const world = partsWorld();
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    await seed.seedCaseDispatch(dir);
    const created = (key: string) =>
      dispatch.calls.filter(({ method, path }) => method === "POST" && path === "/api/v1/issues")[
        Number(key.split("-")[1]) - 1
      ]?.body;
    expect(created("ACME-12")).toMatchObject({ labels: ["legion"], parent: "ACME-10" });
    expect(created("ACME-13")).toMatchObject({
      title: "A sibling",
      labels: ["ux"],
      parent: "ACME-10",
    });
    expect(created("ACME-14")).toMatchObject({ title: "ACME-14", labels: [] });
    expect(dispatch.issues.get("ACME-13")).toMatchObject({
      status: "todo",
      blocked_by: ["ACME-14"],
      claim: { actor: { kind: "session", id: "ses-acme-13" } },
    });
    const rank = (key: string) => dispatch.issues.get(key)?.rank ?? 0;
    expect(rank("ACME-14")).toBeLessThan(
      Math.min(...[...dispatch.issues.keys()].filter((key) => key !== "ACME-14").map(rank))
    );
    expect(rank("ACME-13")).toBeGreaterThan(rank("ACME-14"));
    const daemon = { kind: "session", id: "legion-daemon:ACME" };
    expect(dispatch.runs).toEqual([
      expect.objectContaining({
        actor: daemon,
        task: "default",
        step: 1,
        kind: "step",
        role: "planner",
        agent: "rig-seed",
        model: "rig-seed",
        generation: 1,
        result: "passed",
        ended_at: expect.any(String),
      }),
      expect.objectContaining({
        kind: "retro",
        step: 0,
        rating: 4,
        rating_reason: "The plan fit.",
      }),
      expect.objectContaining({
        kind: "merge",
        role: "merger",
        started_at: expect.any(String),
      }),
    ]);
    expect(dispatch.runs[2]).not.toHaveProperty("result");
    // Each run of the world is after its one record, P05's plan handoff, so each start and end the
    // seed wrote follows Dispatch's stamp of that handoff a nanosecond apart, in world order.
    const stamp = ns(dispatch.handoffs[0]?.created_at ?? "");
    expect(instants(fakeRuns())).toEqual([1n, 2n, 3n, 4n, 5n].map((n) => stamp + n));
    expect(JSON.parse(readFileSync(path.join(dir, "dispatch-fixture.json"), "utf8"))).toEqual({
      issue: "ACME-12",
      seedThrough: 0,
      ids: { "handoff/hnd-acme-12-plan-1": handoffId("plan", 1) },
    });
  });

  // Catches: a seed that files a world Dispatch then holds otherwise, which the run would read as
  // the world, where it is a setup failure that names the part.
  test("a world Dispatch does not answer as it states fails the seed, naming each part", async () => {
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(partsWorld()));
    dispatch.misread = (key, answer) =>
      key === "ACME-13" ? { ...answer, labels: [], status: "backlog" } : answer;
    const seeded = seed.seedCaseDispatch(dir);
    await expect(seeded).rejects.toThrow(
      "world.dispatch.issues[0].status: Dispatch answers backlog, and the world states todo"
    );
    await expect(seeded).rejects.toThrow(
      "world.dispatch.issues[0].labels: Dispatch answers [], and the world states [ux]"
    );
    expect(existsSync(path.join(dir, "dispatch-fixture.json"))).toBe(false);

    const nameless = partsWorld();
    const merge = nameless.dispatch.steps?.[2];
    if (merge === undefined) throw new Error("the world lost its merge");
    merge.role = null;
    const other = runDir();
    writeFileSync(path.join(other, "launch-world.json"), JSON.stringify(nameless));
    await expect(seed.seedCaseDispatch(other)).rejects.toThrow(
      "world.dispatch.steps[2].role is null"
    );
  });

  test("names each gate step's document by the id the scratch Dispatch gives it, in the fixture and the prompt", async () => {
    const p05 = parseHeldoutFile(example("heldout")).cases.find(({ id }) => id === "P05");
    if (!p05?.world.daemon) throw new Error("missing P05's daemon");
    const world = structuredClone(p05.world);
    const stated = "6c174565-c501-58c9-b35a-149c49ff8ffa";
    if (world.daemon === null) throw new Error("missing P05's daemon");
    world.daemon.gate = [
      { step: "spec", document: "spec.md", current: 1, approved: 1, artifactId: stated },
    ];
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    writeFileSync(path.join(dir, "prompt.txt"), `The spec is ${stated}.\n`);
    await seed.seedCaseDispatch(dir);
    // The gate is the claim's tree root's, ACME-10 (P05's parent), whose spec Dispatch names spec.md.
    const given = dispatch.issues.get("ACME-10")?.spec_artifact;
    expect(JSON.parse(readFileSync(path.join(dir, "dispatch-fixture.json"), "utf8")).gate).toEqual({
      spec: given,
    });
    expect(readFileSync(path.join(dir, "prompt.txt"), "utf8")).toBe(`The spec is ${given}.\n`);
    expect(JSON.parse(readFileSync(path.join(dir, "dispatch-fixture.json"), "utf8")).ids).toEqual({
      [`artifact/${stated}`]: given,
      "handoff/hnd-acme-12-plan-1": handoffId("plan", 1),
    });
  });

  test("a gate step whose document the claim tree's root does not hold fails the seed, naming it", async () => {
    const p05 = parseHeldoutFile(example("heldout")).cases.find(({ id }) => id === "P05");
    const world = structuredClone(p05?.world);
    if (world?.daemon === null || world?.daemon === undefined)
      throw new Error("missing P05's daemon");
    world.daemon.gate = [{ step: "criteria", document: "criteria.md", current: 1, approved: null }];
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    await expect(seed.seedCaseDispatch(dir)).rejects.toThrow(
      "world.daemon.gate[0].document criteria.md: the scratch Dispatch holds no document of ACME-10"
    );
  });

  // Catches: a step run written at the world's own instants, on the case's clock, beside records
  // Dispatch stamped when the seed wrote them, so that no handoff lies inside any run
  // (dispatch://INFRA-3223/comment/a21351d3-d0bd-4f9d-9672-6f8707ac8e07).
  test("places the runs of T0's example among its records' stamps, each run holding the records it holds in the world", () => {
    const file = parseHeldoutFile(example("heldout"));
    const p05 = file.cases.find(({ id }) => id === "P05")?.world;
    const p10 = file.cases.find(({ id }) => id === "P10")?.world;
    if (p05 === undefined || p10 === undefined) throw new Error("missing P05 or P10");
    const created = ns("2026-11-27T16:36:00.000100Z");
    // P05's planner run holds its plan handoff, as typed-parts-cases.json writes the run.
    const planner = [stepRun(1, "step", "planner", "2030-01-07T13:00:00Z", "2030-01-07T13:30:00Z")];
    const p05Records = stampedAfter(caseRecords(p05), created);
    const p05Written = seed.runTimes("ACME-12", planner, p05Records, created);
    expect(holdings(planner, p05Records)).toEqual([["hnd-acme-12-plan-1"]]);
    expect(holdings(planner, p05Records, p05Written)).toEqual([["hnd-acme-12-plan-1"]]);
    // P10's tester run holds its passing result, and its reviewer run, which still runs, the review.
    const p10Runs = [
      stepRun(3, "step", "tester", "2030-01-08T09:00:00Z", "2030-01-08T09:10:00Z"),
      stepRun(4, "step", "reviewer", "2030-01-08T09:30:00Z", null),
    ];
    const p10Records = stampedAfter(caseRecords(p10), created);
    const p10Written = seed.runTimes("ACME-21", p10Runs, p10Records, created);
    expect(holdings(p10Runs, p10Records)).toEqual([
      ["world.dispatch.results[0]"],
      ["hnd-acme-21-review-1"],
    ]);
    expect(holdings(p10Runs, p10Records, p10Written)).toEqual(holdings(p10Runs, p10Records));
    expect(p10Written[1]?.endedAt).toBeNull();
    // Each instant is after the issue's creation, in the world's order.
    expect(rising([created, ...instants(p05Written)])).toBe(true);
    expect(rising([created, ...instants(p10Written)])).toBe(true);
  });

  test("places an H08-shaped world's runs in its order, with the record between two runs in neither and the running retro left open", () => {
    const world = h08World();
    const runs = world.dispatch.steps ?? [];
    const created = ns("2026-11-27T16:36:00.000100Z");
    expect(holdings(runs, stampedAfter(caseRecords(world), created))).toEqual(h08Holds);
    // Stamps a millisecond apart, and a microsecond apart, as finely as Dispatch stamps.
    for (const gap of [1_000_000n, 1_000n]) {
      const records = stampedAfter(caseRecords(world), created, gap);
      const written = seed.runTimes("ACME-12", runs, records, created);
      expect(holdings(runs, records, written)).toEqual(h08Holds);
      expect(holdings(runs, records, written).flat()).not.toContain("c-1");
      // The world's runs follow each other, so each start and end comes after the one before it.
      expect(rising([created, ...instants(written)])).toBe(true);
      expect(written.at(-1)?.endedAt).toBeNull();
    }
  });

  // Catches: a placement that counts the records before a case time and takes the stamp at that
  // index, which places a time wrongly where Dispatch's stamps do not rise in the world's order.
  test("refuses, naming the step times and the records, stamps that do not keep the world's order", () => {
    const created = ns("2026-11-27T16:36:00Z");
    const hour = (h: number) => ns(`2030-01-09T0${h}:00:00Z`);
    const times = [
      { at: hour(2), field: "world.dispatch.steps[0].startedAt" },
      { at: hour(4), field: "world.dispatch.steps[0].endedAt" },
    ];
    const a = { id: "a", at: hour(1), stamp: created + 1_000_000n };
    const place = (records: seedModule.Stamped[]) => () =>
      seed.placeStepTimes("ACME-12", records, times, created);
    expect(place([a, { id: "b", at: hour(3), stamp: a.stamp }])).toThrow(
      "world.dispatch.steps[0].startedAt, world.dispatch.steps[0].endedAt: Dispatch stamped b 2026-11-27T16:36:00.001000000Z, and a, which the seed wrote before it, 2026-11-27T16:36:00.001000000Z: the stamps do not keep the world's order, so no step time after a can be placed among them"
    );
    expect(place([a, { id: "b", at: hour(3), stamp: created + 500_000n }])).toThrow(
      "Dispatch stamped b 2026-11-27T16:36:00.000500000Z, and a, which the seed wrote before it"
    );
    expect(place([{ ...a, stamp: created }])).toThrow(
      "world.dispatch.steps[0].startedAt, world.dispatch.steps[0].endedAt: Dispatch stamped a 2026-11-27T16:36:00.000000000Z, and ACME-12's creation, which the seed wrote before it, 2026-11-27T16:36:00.000000000Z"
    );
    expect(
      place([
        { id: "b", at: hour(3), stamp: a.stamp },
        { ...a, stamp: a.stamp + 1n },
      ])
    ).toThrow(
      "ACME-12: the seed wrote a (case time 2030-01-09T01:00:00.000000000Z) after b (case time 2030-01-09T03:00:00.000000000Z), out of the world's order"
    );
    // Two records of one case time may share a stamp: no step time falls between them.
    expect(place([a, { ...a, id: "a2" }])()).toEqual(
      new Map([
        [hour(2), a.stamp + 1n],
        [hour(4), a.stamp + 2n],
      ])
    );
  });

  test("refuses, naming the step times, two stamps that leave no room for the times between them", () => {
    const created = ns("2026-11-27T16:36:00Z");
    const times = [
      { at: ns("2030-01-09T02:00:00Z"), field: "world.dispatch.steps[0].startedAt" },
      { at: ns("2030-01-09T02:30:00Z"), field: "world.dispatch.steps[0].endedAt" },
    ];
    const a = { id: "a", at: ns("2030-01-09T01:00:00Z"), stamp: created + 1_000n };
    const b = (stamp: bigint) => ({ id: "b", at: ns("2030-01-09T03:00:00Z"), stamp });
    expect(() => seed.placeStepTimes("ACME-12", [a, b(a.stamp + 2n)], times, created)).toThrow(
      "world.dispatch.steps[0].startedAt, world.dispatch.steps[0].endedAt: Dispatch stamped a 2026-11-27T16:36:00.000001000Z and b, which the seed wrote after it, 2026-11-27T16:36:00.000001002Z, which leaves no room between them for 2 step times"
    );
    expect([
      ...seed.placeStepTimes("ACME-12", [a, b(a.stamp + 3n)], times, created).values(),
    ]).toEqual([a.stamp + 1n, a.stamp + 2n]);
  });

  // Catches: a seed that writes the records one right after the other, so that two records around
  // a run's start or end share a stamp on a fast Dispatch and the seed refuses the world by chance.
  test("waits before each later record, so that each run Dispatch lists holds its records even on a clock that moves only when the seed sleeps", async () => {
    const clock = heldClock();
    const world = h08World();
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    await seed.seedCaseDispatch(dir, clock);
    const records = fakeStamps(world);
    expect(rising(records.map(({ stamp }) => stamp))).toBe(true);
    expect(holdings(world.dispatch.steps ?? [], records, fakeRuns())).toEqual(h08Holds);
    expect(fakeRuns().at(-1)?.endedAt).toBeNull();
    expect(instants(fakeRuns()).every((at) => at <= clock.now())).toBe(true);
  });

  // Catches: a seed that waits, and checks its stamps, in a world with no step run to place.
  test("a world with no step run places nothing, so the seed waits for nothing", async () => {
    const p09 = parseSkillUseFile(example("skilluse")).cases.find(({ id }) => id === "P09");
    if (!p09) throw new Error("missing P09");
    const clock = heldClock();
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(p09.world));
    await seed.seedCaseDispatch(dir, clock);
    expect(clock.now()).toBe(ns("2026-11-27T16:36:00Z"));
    expect(dispatch.handoffs).toHaveLength(p09.world.dispatch.handoffs.length);
  });

  test("a world whose stamps leave a step time no place fails the seed, naming the run, before it writes one", async () => {
    const at = ns("2026-11-27T16:36:00Z");
    dispatch.now = () => at;
    const world = partsWorld();
    world.dispatch.steps = [
      stepRun(1, "step", "planner", "2030-01-07T13:00:00Z", "2030-01-07T13:30:00Z"),
    ];
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    // A clock that never moves: the plan handoff takes the stamp of the issue's creation.
    const seeded = seed.seedCaseDispatch(dir, { now: () => at, sleep: () => Promise.resolve() });
    await expect(seeded).rejects.toThrow(
      "world.dispatch.steps[0].startedAt, world.dispatch.steps[0].endedAt: Dispatch stamped hnd-acme-12-plan-1 2026-11-27T16:36:00.000000000Z, and ACME-12's creation, which the seed wrote before it, 2026-11-27T16:36:00.000000000Z: the stamps do not keep the world's order"
    );
    expect(dispatch.runs).toEqual([]);
  });

  // Catches: step times placed a second apart after the last record, so that the seed returns
  // with runs that start or end in the future and a role reads a run that ended after now.
  test("waits, and logs it, until its clock has passed the last step time it placed", async () => {
    const clock = heldClock();
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(partsWorld()));
    await seed.seedCaseDispatch(dir, clock);
    const written = instants(fakeRuns());
    expect(written).toHaveLength(5);
    expect(written.every((at) => at <= clock.now())).toBe(true);
    expect(logSpy).toHaveBeenCalledWith(
      expect.stringMatching(
        /^waiting 1 ms for the clock to pass \S+, world\.dispatch\.steps\[2\]\.startedAt as the seed placed it$/
      )
    );
  });

  test("a Dispatch whose clock is ahead of the seed's by more than the wait's bound fails the seed, naming the run, before it writes one", async () => {
    const clock = heldClock();
    dispatch.now = () => clock.now() + 3_600_000_000_000n;
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(partsWorld()));
    await expect(seed.seedCaseDispatch(dir, clock)).rejects.toThrow(
      /^world\.dispatch\.steps\[2\]\.startedAt: the seed placed it at \S+, and its clock reads \S+ after a wait of 0 ms: it waits at most 5000 ms for its clock to pass the step times it placed$/
    );
    expect(dispatch.runs).toEqual([]);
  });

  test("places the runs of a world with no record after the issue's creation, in the world's order", async () => {
    const runs = partsWorld().dispatch.steps ?? [];
    const created = ns("2026-11-27T16:36:00.000100Z");
    const placed = seed.runTimes("ACME-12", runs, [], created);
    expect(instants(placed)).toEqual([1n, 2n, 3n, 4n, 5n].map((n) => created + n));
    expect(placed[2]?.endedAt).toBeNull();

    const clock = heldClock();
    const world = partsWorld();
    world.dispatch.handoffs = [];
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    await seed.seedCaseDispatch(dir, clock);
    const issued = ns(dispatch.issues.get("ACME-12")?.created_at ?? "");
    expect(instants(fakeRuns())).toEqual([1n, 2n, 3n, 4n, 5n].map((n) => issued + n));
    expect(instants(fakeRuns()).every((at) => at <= clock.now())).toBe(true);
  });

  // Catches: a read-back that holds only the listed times to the written ones, and so passes a
  // Dispatch whose runs do not hold the records the world's runs hold.
  test("a run Dispatch lists without a record the world's run holds fails the seed, naming the run and the record", async () => {
    const world = partsWorld();
    world.dispatch.steps = [
      stepRun(1, "step", "planner", "2030-01-07T13:00:00Z", "2030-01-07T13:30:00Z"),
    ];
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    // The run as Dispatch lists it starts where it ends, after Dispatch stamped the plan handoff.
    dispatch.misreadRun = (run) => ({ ...run, started_at: run.ended_at });
    const seeded = seed.seedCaseDispatch(dir);
    await expect(seeded).rejects.toThrow(
      "world.dispatch.steps[0]: hnd-acme-12-plan-1 is inside the world's run (case time 2030-01-07T13:10:00.000000000Z), and Dispatch stamped it "
    );
    await expect(seeded).rejects.toThrow("outside the run as Dispatch lists it");
  });

  test("a run Dispatch lists with a record the world's runs do not hold fails the seed, naming the run and the record", async () => {
    const clock = heldClock();
    const world = h08World();
    const dir = runDir();
    writeFileSync(path.join(dir, "launch-world.json"), JSON.stringify(world));
    // The failed tester run as Dispatch lists it ends at c-2's stamp, after c-1's.
    dispatch.misreadRun = (run) =>
      run.result === "failed"
        ? {
            ...run,
            ended_at: dispatch.comments.find(({ body }) => body === "Starting again.")?.created_at,
          }
        : run;
    const seeded = seed.seedCaseDispatch(dir, clock);
    await expect(seeded).rejects.toThrow(
      "world.dispatch.steps[2]: c-1 is outside the world's run (case time 2030-01-09T11:30:00.000000000Z), and Dispatch stamped it "
    );
    await expect(seeded).rejects.toThrow("inside the run as Dispatch lists it");
  });
});

describe("owner-walk", () => {
  /** The recorded plans answer: two finished issues of another project, newest first. */
  const recorded = {
    project: "SRX1",
    issues: [
      { issue: "SRX1-2", title: "A second change rates its plan", ratings: [] },
      { issue: "SRX1-1", title: "One issue walks to done", ratings: [] },
    ],
  };
  // The fake numbers LWEVAL's issues from 2, as the run's Dispatch numbers them from 1: the map
  // names the keys the seed's first creates get.
  const keyMap = { SRX1: "LWEVAL", "SRX1-1": "LWEVAL-2", "SRX1-2": "LWEVAL-3" };
  /** plans-answer.json and plans-answer.source in `dir`, as the rig writes them; the path of the
   * first. `sha256` stands in for the body's own sum. */
  function plansAnswer(dir: string, map: Record<string, string> = keyMap, sha256?: string) {
    const body = `${JSON.stringify(recorded)}\n`;
    writeFileSync(path.join(dir, "plans-answer.json"), body);
    const source = {
      commit: "a".repeat(40),
      sha256: sha256 ?? createHash("sha256").update(body).digest("hex"),
      keyMap: map,
    };
    writeFileSync(path.join(dir, "plans-answer.source"), `${JSON.stringify(source)}\n`);
    return path.join(dir, "plans-answer.json");
  }
  const fixtureOf = (file: string) => JSON.parse(readFileSync(file, "utf8"));
  const keyOf = (fixture: { issues: { part: string; key: string }[] }, part: string) => {
    const found = fixture.issues.find((issue) => issue.part === part);
    if (found === undefined) throw new Error(`the fixture holds no ${part} issue`);
    return found.key;
  };
  /** Whether the person approved the spec of `key` through an approval request. */
  const approved = (key: string) =>
    dispatch.asks.some(
      (ask) =>
        ask.kind === "approval" &&
        ask.approval?.artifact_id === dispatch.issues.get(key)?.spec_artifact &&
        ask.state === "answered" &&
        ask.answer?.user === "alice@example.invalid" &&
        ask.answer.selected.join() === "Approve"
    );

  // Catches: keys the owner reads in the plans answer that do not exist in the run's Dispatch, a
  // root whose gate is not open, a G the release cannot release, an E1 that is still open or
  // whose record is the run's own, and a G spec that names no E1.
  test("owner-walk seeds the root's spec approved, G backlog, and E1 and the recorded plans' issues closed with their titles", async () => {
    const dir = freshDir("owner-walk");
    const file = path.join(dir, "fixture.json");
    await ownerWalkSeed.ownerWalkIssues(file, "root", plansAnswer(dir));
    expect(dispatch.issues.get("LWEVAL-2")).toMatchObject({
      title: "One issue walks to done",
      status: "done",
    });
    expect(dispatch.issues.get("LWEVAL-3")).toMatchObject({
      title: "A second change rates its plan",
      status: "done",
    });
    const fixture = fixtureOf(file);
    const root = keyOf(fixture, "root");
    const earlier = keyOf(fixture, "earlier");
    const planned = keyOf(fixture, "planned");
    expect(fixture.issues.map((issue: { part: string }) => issue.part).sort()).toEqual([
      "earlier",
      "planned",
      "root",
    ]);
    expect(dispatch.issues.get(root)).toMatchObject({ parent: null, status: "todo" });
    expect(approved(root)).toBe(true);
    expect(dispatch.issues.get(earlier)).toMatchObject({ status: "done" });
    expect(dispatch.issues.get(planned)).toMatchObject({ parent: root, status: "backlog" });
    const plannedSpec = dispatch.artifacts.find(
      (artifact) => artifact.id === dispatch.issues.get(planned)?.spec_artifact
    )?.markdown;
    expect(plannedSpec).toContain(earlier);
    expect(plannedSpec).not.toContain("{{");
    const records = dispatch.handoffs.filter((handoff) => handoff.issue_key === earlier);
    expect(records).toEqual([
      expect.objectContaining({
        phase: "implement",
        version: 1,
        author: ownerWalkSeed.EARLIER_IMPLEMENTER,
      }),
    ]);
    expect(JSON.parse(records[0]?.body ?? "{}")).toMatchObject({
      schemaVersion: 1,
      phase: "implement",
      issue: earlier,
    });
    // The world holds no plan: the owner writes the first one.
    expect(dispatch.artifacts.filter((artifact) => artifact.name === "plan.md")).toEqual([]);
    expect(fixture).toMatchObject({
      scenario: "owner-walk",
      kind: "root",
      owner: root,
      planned,
      earlier: {
        key: earlier,
        record: { id: records[0]?.id, version: 1 },
        session: ownerWalkSeed.EARLIER_IMPLEMENTER.id,
      },
      plans: [
        { recorded: "SRX1-1", key: "LWEVAL-2", title: "One issue walks to done" },
        { recorded: "SRX1-2", key: "LWEVAL-3", title: "A second change rates its plan" },
      ],
    });
  });

  // Catches: a child owner's world whose parent issues are not in implementing, or whose G hangs
  // under the root rather than under the owner's own issue.
  test("owner-walk seeds, for a child owner, R and C in progress with their specs approved, and G a backlog child of C", async () => {
    const dir = freshDir("owner-walk");
    const file = path.join(dir, "fixture.json");
    await ownerWalkSeed.ownerWalkIssues(file, "child", plansAnswer(dir));
    const fixture = fixtureOf(file);
    const root = keyOf(fixture, "root");
    const child = keyOf(fixture, "child");
    const planned = keyOf(fixture, "planned");
    expect(dispatch.issues.get(root)).toMatchObject({ parent: null, status: "in_progress" });
    expect(dispatch.issues.get(child)).toMatchObject({ parent: root, status: "in_progress" });
    expect(approved(root)).toBe(true);
    expect(approved(child)).toBe(true);
    expect(dispatch.issues.get(planned)).toMatchObject({ parent: child, status: "backlog" });
    expect(fixture).toMatchObject({ kind: "child", owner: child, planned });
  });

  // Catches: a seed that serves the owner an answer other than the one the capture recorded.
  test("owner-walk refuses a plans answer whose sha256 is not its source's, and writes nothing", async () => {
    const dir = freshDir("owner-walk");
    const answer = plansAnswer(dir, keyMap, "c".repeat(64));
    await expect(
      ownerWalkSeed.ownerWalkIssues(path.join(dir, "fixture.json"), "root", answer)
    ).rejects.toThrow("c".repeat(64));
    expect(dispatch.calls.filter((call) => call.method !== "GET")).toEqual([]);
  });

  // Catches: a key the owner reads in the answer that the run's Dispatch does not hold.
  test("owner-walk refuses a key map that leaves an issue of the answer unmapped, and writes nothing", async () => {
    const dir = freshDir("owner-walk");
    const answer = plansAnswer(dir, { SRX1: "LWEVAL", "SRX1-1": "LWEVAL-2" });
    await expect(
      ownerWalkSeed.ownerWalkIssues(path.join(dir, "fixture.json"), "root", answer)
    ).rejects.toThrow("SRX1-2");
    expect(dispatch.calls.filter((call) => call.method !== "GET")).toEqual([]);
  });
});

describe("capture-scored", () => {
  // Catches: a capture that keeps only the newest plan, or drops a version's authors, so the
  // scorer cannot tell which session wrote which version; and one that captures from a fixture
  // that is not the seed's whole fixture, which the scorer then refuses after the run.
  test("capture-scored keeps each plan version with its author", async () => {
    dispatch.issue("LWEVAL-6", "Finish the CSV importer", "LWEVAL-4");
    const other = { kind: "session", id: "ses-other" } as const;
    dispatch.artifacts.push({
      id: "art-plan",
      issue_key: "LWEVAL-6",
      name: "plan.md",
      slug: "plan-md",
      kind: "doc",
      primary: false,
      markdown: "v2",
      versions: [
        {
          number: 1,
          created_at: "2026-11-24T12:00:05.000001Z",
          authors: [runSession],
          markdown: "v1",
        },
        { number: 2, created_at: "2026-11-24T12:10:05.000001Z", authors: [other], markdown: "v2" },
      ],
    });
    dispatch.handoffs.push({
      id: "h-1",
      issue_key: "LWEVAL-6",
      phase: "implement",
      version: 1,
      author: other,
      created_at: "2026-11-24T12:20:00.000001Z",
      body: '{"phase":"implement"}',
    });
    const dir = runDir();
    const file = path.join(dir, "fixture.json");
    const fixture = {
      scenario: "owner-walk",
      kind: "root",
      project: "LWEVAL",
      owner: "LWEVAL-4",
      planned: "LWEVAL-6",
      issues: [
        {
          key: "LWEVAL-6",
          part: "planned",
          title: "Finish the CSV importer",
          parent: "LWEVAL-4",
          status: "backlog",
          spec: { artifact: "art-spec", approvedVersion: null },
        },
      ],
      earlier: {
        key: "LWEVAL-5",
        record: { id: "h-0", version: 1 },
        session: "rig-implementer-e1",
        pr: 7,
        head: "a".repeat(40),
      },
      plans: [],
    };
    const partial = Object.fromEntries(
      Object.entries(fixture).filter(([key]) => key !== "planned")
    );
    writeFileSync(file, `${JSON.stringify(partial)}\n`);
    const reads = dispatch.calls.length;
    await expect(ownerWalkSeed.captureScored(file, dir)).rejects.toThrow("planned");
    expect(dispatch.calls.length).toBe(reads);
    expect(existsSync(path.join(dir, "captured.json"))).toBe(false);
    writeFileSync(file, `${JSON.stringify(fixture)}\n`);
    await ownerWalkSeed.captureScored(file, dir);
    const captured = JSON.parse(readFileSync(path.join(dir, "captured.json"), "utf8"));
    expect(captured.fixture).toEqual(fixture);
    expect(captured.issues).toHaveLength(1);
    expect(captured.issues[0]).toMatchObject({
      key: "LWEVAL-6",
      title: "Finish the CSV importer",
      parent: "LWEVAL-4",
      status: "triage",
    });
    const plan = captured.issues[0].artifacts.find(
      (artifact: { name: string }) => artifact.name === "plan.md"
    );
    expect(plan.versions).toEqual([
      {
        version: 1,
        created_at: "2026-11-24T12:00:05.000001Z",
        authors: [runSession],
        markdown: "v1",
      },
      { version: 2, created_at: "2026-11-24T12:10:05.000001Z", authors: [other], markdown: "v2" },
    ]);
    expect(captured.issues[0].handoffs).toEqual([
      {
        id: "h-1",
        phase: "implement",
        version: 1,
        author: other,
        created_at: "2026-11-24T12:20:00.000001Z",
        body: '{"phase":"implement"}',
      },
    ]);
  });
});
