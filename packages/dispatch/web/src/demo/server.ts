// The ask card demo's stubbed Dispatch (packages/dispatch/e2e/ask-card-demo/README.md): every
// `/api/` and `/auth/` request the page makes, answered in the browser from the invented
// fixtures (`fixtures.ts`). A write changes this in-memory state, which the tab keeps in
// sessionStorage across a reload, and goes out on the event feed as the server's own event would.
//
// Rewrite and Define, which no Dispatch route serves yet, are answered here as the design says
// the server answers them, with two differences the README lists: the rewrite's text and each
// definition are the fixtures' own rather than a model's, and a rewrite answers its POST when it
// ends instead of running behind the write and writing an `ask.edited` event.

// The server's own block schema, which `GET /api/v1/schema/blocks` answers with.
import blockSchema from "../../../../envoy/internal/dispatch/pmdoc/schema/blocks.json";
import type { Ask, AskRead, Comment, DispatchEvent, InboxRow } from "../api/types";
import type { AskRewrite, Definition } from "../features/inbox/ask-rewrite";
import { DOCUMENT_ROUTES } from "./documents";
import {
  AGENTS,
  ASKS,
  type DemoAsk,
  definitionOf,
  ISSUES,
  issueSummary,
  PROJECT,
  PROJECTS,
  READER,
  USERS,
} from "./fixtures";
import { type DemoAnswer, type DemoRoute, goTime, notFound, ok } from "./http";

/** How long a rewrite runs before it answers: long enough to see Rewriting…. */
const REWRITE_MS = 2_400;
/** How long the record's half of a definition takes, and the agent's. */
const DEFINE_MS = 700;
const AGENT_ANSWER_MS = 3_500;
const KEPT_STATE = "dispatch-ask-card-demo.state";

type DemoRow = InboxRow & { rewrite?: AskRewrite };

interface KeptState {
  asks: Record<string, DemoRow>;
  replies: Record<string, Comment[]>;
  eventId: number;
}

interface HeldDefinition {
  definition: Definition;
  createdAt: number;
  agentAnswer?: string;
}

export class DemoServer {
  private readonly asks = new Map<string, DemoRow>();
  private readonly plans = new Map<string, DemoAsk>();
  private readonly replies = new Map<string, Comment[]>();
  private readonly definitions = new Map<string, HeldDefinition>();
  private readonly failedOnce = new Set<string>();
  private eventId = 1_000;
  private readonly eventListeners = new Set<(id: string, event: DispatchEvent) => void>();
  /** Every request no route of the stub answered, for the screenshot run to read. */
  readonly unanswered: string[] = [];

  constructor() {
    for (const plan of ASKS) {
      this.asks.set(plan.row.id, structuredClone(plan.row));
      this.plans.set(plan.row.id, plan);
    }
    const kept = readKept();
    if (kept !== null) {
      for (const [id, row] of Object.entries(kept.asks)) this.asks.set(id, row);
      for (const [id, list] of Object.entries(kept.replies)) this.replies.set(id, list);
      this.eventId = kept.eventId;
    }
  }

  eventHead(): string {
    return String(this.eventId);
  }

  onEvent(listener: (id: string, event: DispatchEvent) => void): () => void {
    this.eventListeners.add(listener);
    return () => this.eventListeners.delete(listener);
  }

  /** The answer to one request, or `undefined` where no route of the stub answers it. A
   *  promise where the route takes time, as a rewrite does. */
  handle(method: string, url: URL, body: unknown): DemoAnswer | Promise<DemoAnswer> | undefined {
    for (const route of [...this.routes, ...DOCUMENT_ROUTES]) {
      if (route.method !== method) continue;
      const match = route.pattern.exec(url.pathname);
      if (match !== null)
        return route.answer(match.slice(1).map(decodeURIComponent), url.searchParams, body);
    }
    return undefined;
  }

  /** Records a request no route answered. */
  missed(method: string, url: URL): void {
    const entry = `${method} ${url.pathname}${url.search}`;
    this.unanswered.push(entry);
    console.warn(`[ask card demo] no stub route answers ${entry}`);
  }

  private readonly routes: DemoRoute[] = [
    {
      method: "GET",
      pattern: /^\/auth\/whoami$/,
      answer: () => ok({ kind: "user", login: READER, event_head: this.eventHead() }),
    },
    {
      method: "GET",
      pattern: /^\/api\/v1\/whoami$/,
      answer: () =>
        ok({
          kind: "user",
          login: READER,
          scopes: ["person", "agent"],
          session: null,
          project: null,
        }),
    },
    { method: "GET", pattern: /^\/api\/v1\/inbox$/, answer: () => ok(this.inbox()) },
    {
      method: "GET",
      pattern: /^\/api\/v1\/asks\/([^/]+)$/,
      answer: ([id]) => {
        const row = this.asks.get(id);
        return row === undefined ? notFound("ASK_NOT_FOUND") : ok(this.askRead(row));
      },
    },
    {
      method: "POST",
      pattern: /^\/api\/v1\/asks\/([^/]+)\/answer$/,
      answer: ([id], _, body) => this.answer(id, body as { selected?: string[]; text?: string }),
    },
    {
      method: "POST",
      pattern: /^\/api\/v1\/asks\/([^/]+)\/decline$/,
      answer: ([id], _, body) => this.decline(id, (body ?? {}) as { note?: string }),
    },
    {
      method: "PUT",
      pattern: /^\/api\/v1\/asks\/([^/]+)\/asked-of$/,
      answer: ([id], _, body) => this.handOn(id, (body as { asked_of: string }).asked_of),
    },
    {
      method: "POST",
      pattern: /^\/api\/v1\/asks\/([^/]+)\/rewrite$/,
      answer: ([id]) => this.rewrite(id),
    },
    {
      method: "POST",
      pattern: /^\/api\/v1\/definitions$/,
      answer: (_, __, body) => this.define(body as { words: string; ask_id?: string }),
    },
    {
      method: "GET",
      pattern: /^\/api\/v1\/definitions\/([^/]+)$/,
      answer: ([id]) => {
        const held = this.definitions.get(id);
        return held === undefined ? notFound("DEFINITION_NOT_FOUND") : ok(this.definitionNow(held));
      },
    },
    {
      method: "POST",
      pattern: /^\/api\/v1\/issues\/([^/]+)\/comments$/,
      answer: ([key], _, body) => this.comment(key, body as { body: string; ask_id?: string }),
    },
    {
      method: "GET",
      pattern: /^\/api\/v1\/users$/,
      answer: () => ok({ users: USERS.map((login) => ({ login })) }),
    },
    { method: "GET", pattern: /^\/api\/v1\/projects$/, answer: () => ok(PROJECTS) },
    {
      method: "GET",
      pattern: /^\/api\/v1\/projects\/([^/]+)$/,
      answer: ([key]) => (key === PROJECT ? ok(PROJECTS[0]) : notFound("PROJECT_NOT_FOUND")),
    },
    {
      method: "GET",
      pattern: /^\/api\/v1\/issues$/,
      // The reader pins nothing, so the sidebar's pinned list is empty.
      answer: (_, q) =>
        ok(
          q.get("pinned") === "true"
            ? []
            : ISSUES.map((issue) => issueSummary(issue, this.openAsksOf(issue.key)))
        ),
    },
    { method: "GET", pattern: /^\/api\/v1\/agents$/, answer: () => ok(AGENTS) },
    { method: "GET", pattern: /^\/api\/v1\/me\/state$/, answer: () => ok({}) },
    { method: "GET", pattern: /^\/api\/v1\/me\/agents\/state$/, answer: () => ok({}) },
    {
      method: "GET",
      pattern: /^\/api\/v1\/me\/replies$/,
      answer: (_, q) =>
        ok({
          rows: [],
          total: 0,
          limit: Number(q.get("limit") ?? 50),
          offset: 0,
          as_of: goTime(new Date()),
        }),
    },
    { method: "GET", pattern: /^\/api\/v1\/schema\/blocks$/, answer: () => ok(blockSchema) },
    { method: "GET", pattern: /^\/api\/v1\/credential-requests$/, answer: () => ok(null) },
    { method: "GET", pattern: /^\/api\/v1\/settings\/legion$/, answer: () => ok([]) },
    { method: "GET", pattern: /^\/api\/v1\/issues\/[^/]+\/subscribers$/, answer: () => ok([]) },
    { method: "POST", pattern: /^\/auth\/logout$/, answer: () => ({ status: 204, body: null }) },
  ];

  private openAsksOf(key: string): number {
    return [...this.asks.values()].filter((row) => row.issue_key === key && row.state === "open")
      .length;
  }

  private inbox(): DemoRow[] {
    return [...this.asks.values()]
      .filter((row) => row.state === "open")
      .map((row) => ({
        ...row,
        thread: { ...row.thread, replies: this.replies.get(row.id) ?? [] },
      }));
  }

  private askRead(row: DemoRow): AskRead {
    const { thread, issue: _issue, priority: _priority, snoozed_until: _snoozed, ...ask } = row;
    return {
      ...thread,
      ask: ask as unknown as AskRead["ask"],
      replies: (this.replies.get(row.id) ?? []) as unknown as AskRead["replies"],
    };
  }

  private update(id: string, change: (row: DemoRow) => DemoRow, type: string): DemoRow | undefined {
    const held = this.asks.get(id);
    if (held === undefined) return undefined;
    const next = change(held);
    this.asks.set(id, next);
    this.keep();
    this.publish(next, type);
    return next;
  }

  private answer(id: string, input: { selected?: string[]; text?: string }): DemoAnswer {
    const next = this.update(
      id,
      (row) => ({
        ...row,
        state: "answered",
        waiting_on: undefined,
        answer: {
          at: goTime(new Date()),
          selected: input.selected ?? [],
          text: input.text ?? null,
          user: READER,
        },
      }),
      "ask.answered"
    );
    return next === undefined ? notFound("ASK_NOT_FOUND") : ok(this.askRead(next).ask);
  }

  private decline(id: string, input: { note?: string }): DemoAnswer {
    const next = this.update(
      id,
      (row) => ({
        ...row,
        state: "resolved",
        waiting_on: undefined,
        resolution: {
          actor: { id: READER, kind: "user" },
          at: goTime(new Date()),
          kind: "declined",
          reason: input.note ?? "",
        },
      }),
      "ask.resolved"
    );
    return next === undefined ? notFound("ASK_NOT_FOUND") : ok(this.askRead(next).ask);
  }

  private handOn(id: string, person: string): DemoAnswer {
    const next = this.update(
      id,
      (row) => ({
        ...row,
        asked_of: person === row.issue?.assignee ? null : person,
        asked_of_at: goTime(new Date()),
        asked_of_by: READER,
        waiting_on_person: person,
      }),
      "ask.asked_of"
    );
    return next === undefined ? notFound("ASK_NOT_FOUND") : ok(this.askRead(next).ask);
  }

  /** A rewrite: running at once, then the fixtures' rewrite in place after `REWRITE_MS`, or a
   *  refusal the first time for the ask whose plan says so. */
  private async rewrite(id: string): Promise<DemoAnswer> {
    const held = this.asks.get(id);
    const plan = this.plans.get(id);
    if (held === undefined || plan === undefined) return notFound("ASK_NOT_FOUND");
    if (held.kind !== "question") {
      return {
        status: 409,
        body: { code: "ASK_NOT_QUESTION", error: "an approval request is not rewritten" },
      };
    }
    const original = held.rewrite?.original ?? {
      question: held.question,
      options: (held.options ?? []).map(({ label, description }) =>
        description === undefined ? { label } : { label, description }
      ),
    };
    const at = () => goTime(new Date());
    this.update(
      id,
      (row) => ({ ...row, rewrite: { by: "person", state: "running", at: at(), original } }),
      "ask.rewriting"
    );
    const done = Promise.withResolvers<void>();
    setTimeout(done.resolve, REWRITE_MS);
    await done.promise;
    const fail = plan.failFirstRewrite === true && !this.failedOnce.has(id);
    if (fail || plan.rewriteTo === undefined) {
      this.failedOnce.add(id);
      const next = this.update(
        id,
        (row) => ({
          ...row,
          rewrite: {
            by: "person",
            state: "failed",
            at: at(),
            original,
            reason: "The rewrite changed an option's label, which agents match on.",
          },
        }),
        "ask.rewrite_failed"
      );
      return ok(next === undefined ? null : this.askRead(next).ask);
    }
    const to = plan.rewriteTo;
    const next = this.update(
      id,
      (row) => ({
        ...row,
        question: to.question,
        options: to.options,
        edited_at: at(),
        rewrite: { by: "person", state: "done", at: at(), original },
      }),
      "ask.edited"
    );
    return ok(next === undefined ? null : this.askRead(next).ask);
  }

  private async define(input: { words: string; ask_id?: string }): Promise<DemoAnswer> {
    const known = definitionOf(input.words);
    const fromAsk = input.ask_id !== undefined;
    const id = `def-${this.definitions.size + 1}`;
    const held: HeldDefinition = {
      createdAt: Date.now(),
      agentAnswer: fromAsk ? known.agent?.answer : undefined,
      definition: {
        id,
        words: input.words.trim(),
        meaning: known.meaning,
        ...(known.link === undefined ? {} : { link: known.link }),
        agent:
          fromAsk && known.agent !== undefined
            ? { state: "asking", actor: known.agent.actor }
            : { state: "none" },
      },
    };
    this.definitions.set(id, held);
    const done = Promise.withResolvers<void>();
    setTimeout(done.resolve, DEFINE_MS);
    await done.promise;
    return ok(this.definitionNow(held));
  }

  /** A definition as it stands now: the agent's answer joins it `AGENT_ANSWER_MS` after the
   *  request, as a side question to a running session's answer would. */
  private definitionNow(held: HeldDefinition): Definition {
    const { definition } = held;
    if (
      definition.agent.state === "asking" &&
      held.agentAnswer !== undefined &&
      Date.now() - held.createdAt >= AGENT_ANSWER_MS
    ) {
      return {
        ...definition,
        agent: { ...definition.agent, state: "answered", answer: held.agentAnswer },
      };
    }
    return definition;
  }

  private comment(key: string, input: { body: string; ask_id?: string }): DemoAnswer {
    const askId = input.ask_id ?? null;
    const comment = {
      anchor: null,
      answers: null,
      artifact_id: null,
      ask_id: askId,
      author: { id: READER, kind: "user" },
      body: input.body,
      created_at: goTime(new Date()),
      deliveries: [],
      edited_at: null,
      id: `reply-${Date.now()}`,
      issue_key: key,
      mentions: [],
      reply_block: null,
      reply_to: null,
      resolved: false,
      resolved_at: null,
      resolved_by: null,
      suggestion: null,
      turn: askId === null ? null : "agent",
    } as unknown as Comment;
    if (askId !== null) {
      this.replies.set(askId, [...(this.replies.get(askId) ?? []), comment]);
      this.update(
        askId,
        (row) => ({
          ...row,
          waiting_on: "agent",
          last_reply: { author: comment.author, created_at: comment.created_at },
        }),
        "comment.created"
      );
    }
    return { status: 201, body: comment };
  }

  private publish(row: DemoRow, type: string): void {
    this.eventId += 1;
    const event = {
      id: this.eventId,
      issue_key: row.issue_key,
      project: PROJECT,
      seq: this.eventId,
      type,
      actor: { kind: "user", id: READER },
      notify: false,
      created_at: goTime(new Date()),
      payload: this.askRead(row).ask as Ask,
    } as unknown as DispatchEvent;
    for (const listener of this.eventListeners) listener(String(this.eventId), event);
  }

  private keep(): void {
    const kept: KeptState = {
      asks: Object.fromEntries(this.asks),
      replies: Object.fromEntries(this.replies),
      eventId: this.eventId,
    };
    try {
      sessionStorage.setItem(KEPT_STATE, JSON.stringify(kept));
    } catch {
      // A tab that cannot keep it starts from the fixtures again on a reload.
    }
  }
}

function readKept(): KeptState | null {
  try {
    const raw = sessionStorage.getItem(KEPT_STATE);
    return raw === null ? null : (JSON.parse(raw) as KeptState);
  } catch {
    return null;
  }
}
