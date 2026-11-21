import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import type { Server } from "bun";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
} from "../../test/dispatch-stack";

// An agent's ask written through the real `dispatch` CLI of this checkout, run as a child process
// as an agent's shell runs it, against a real Dispatch server of this checkout whose reviewer holds
// asks (INFRA-3231's R5, check 47b5e250). The server's own Go rows (reviewer_test.go) hold an ask
// written over the route; this row proves that the request the CLI sends is one the reviewer
// holds, and that the line the CLI prints names the held ask's id. A pi-envoy session, a task
// subagent of one and an eval cell each reach Dispatch through this same CLI.
//
// The reviewer's model is a stand-in that holds its answer to each review until the row releases
// it, so the row reads the ask while it is held, then lets the review pass and reads it posted.

/** The person the issue is assigned to, who reads the ask once it posts. */
const PERSON = "alice";
/** The session the CLI writes as. */
const SESSION = "session-held-ask-live";
/** The project the row's issue is made in. */
const PROJECT = "HELDASK";
/** The deployment the reviewer's model group names, which each of its calls to the stand-in
 *  carries. */
const REVIEWER_MODEL = "stand-in-reviewer";
/** The question the CLI asks. */
const QUESTION = "Ship the reviewer today?";

/** The held text an agent reads through GET /api/v1/held/{id}, as far as the row reads it. */
interface HeldRead {
  readonly id: string;
  readonly kind: string;
  readonly state: string;
  readonly reader: string;
  readonly outcome?: string;
}

/** The body of a call to the stand-in, as far as it reads it. */
interface ModelCall {
  readonly model?: string;
  readonly stream?: boolean;
  readonly messages?: readonly unknown[];
}

/** One server-sent event of an Anthropic Messages stream. */
function event(name: string, data: unknown): string {
  return `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
}

/** The stand-in's Anthropic Messages answer of `text` to `call`, streamed where the call asks
 *  for a stream, as the reviewer's calls do. */
function anthropicAnswer(call: ModelCall, text: string): Response {
  const usage = { input_tokens: 1, output_tokens: 1 };
  const message = {
    id: "msg_standin",
    model: call.model ?? REVIEWER_MODEL,
    role: "assistant",
    stop_sequence: null,
    type: "message",
  };
  if (call.stream !== true) {
    return Response.json({
      ...message,
      content: [{ text, type: "text" }],
      stop_reason: "end_turn",
      usage,
    });
  }
  return new Response(
    event("message_start", {
      message: { ...message, content: [], stop_reason: null, usage },
      type: "message_start",
    }) +
      event("content_block_start", {
        content_block: { text: "", type: "text" },
        index: 0,
        type: "content_block_start",
      }) +
      event("content_block_delta", {
        delta: { text, type: "text_delta" },
        index: 0,
        type: "content_block_delta",
      }) +
      event("content_block_stop", { index: 0, type: "content_block_stop" }) +
      event("message_delta", {
        delta: { stop_reason: "end_turn", stop_sequence: null },
        type: "message_delta",
        usage,
      }) +
      event("message_stop", { type: "message_stop" }),
    { headers: { "Cache-Control": "no-cache", "Content-Type": "text/event-stream" } }
  );
}

/** Reads `read` every 100 ms until it answers a value, for at most `ms`. It polls because the
 *  server it reads is another process, which tells this one nothing when what it serves changes. */
async function within<T>(what: string, ms: number, read: () => Promise<T | undefined>): Promise<T> {
  const deadline = Date.now() + ms;
  for (;;) {
    const value = await read();
    if (value !== undefined) return value;
    if (Date.now() > deadline) throw new Error(`${what}: not within ${ms} ms`);
    await Bun.sleep(100);
  }
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const directories = useDispatchCliDirectories();

describe.skipIf(!dispatchStackEnabled)(
  "an ask the real CLI writes against a real Dispatch whose reviewer holds asks",
  () => {
    let stack: DispatchStack | undefined;
    let model: Server<undefined> | undefined;
    /** The text of each review the stand-in was asked for. */
    const reviews: string[] = [];
    /** Settles when the stand-in is first asked for a review. */
    const reviewed = Promise.withResolvers<void>();
    /** The row settles it to let the stand-in answer the reviews it holds. */
    const answer = Promise.withResolvers<void>();

    const { asPerson, asMachine, createIssue, ask } = dispatchCliLive({
      stack: () => stack,
      session: SESSION,
      person: PERSON,
      spec: "# Reviewer\n\nThe reviewer holds an agent's ask until its review ends.\n",
      directories,
    });

    /** The status of the person's read of `route`, and its body. */
    async function personRead(route: string): Promise<{ status: number; body: string }> {
      if (stack === undefined) throw new Error("the Dispatch stack did not start");
      const response = await fetch(`${stack.url}${route}`, {
        headers: { "X-Dispatch-User": PERSON },
      });
      return { status: response.status, body: await response.text() };
    }

    beforeAll(async () => {
      // The stand-in reviewer: an Anthropic Messages service on a port the kernel chose, which
      // answers each review with no finding once the row lets it. Its idle timeout outlasts the
      // row, so the call waits on the row and not on the listener.
      model = Bun.serve({
        hostname: "127.0.0.1",
        port: 0,
        idleTimeout: 120,
        async fetch(request) {
          if (request.method !== "POST" || new URL(request.url).pathname !== "/v1/messages") {
            return Response.json({ message: "not found" }, { status: 404 });
          }
          const call = (await request.json()) as ModelCall;
          reviews.push(JSON.stringify(call.messages ?? []));
          reviewed.resolve();
          await answer.promise;
          return anthropicAnswer(call, JSON.stringify({ findings: [] }));
        },
      });
      stack = await startDispatchStack({
        work: directories.stack,
        env: {
          DISPATCH_TEST_HOOKS: "1",
          DISPATCH_TEST_MODEL_URL: `http://127.0.0.1:${model.port}`,
          DISPATCH_REVIEWER_ASKS: "hold",
          DISPATCH_REVIEWER_MODEL_KIND: "anthropic",
          DISPATCH_REVIEWER_MODEL_DEPLOYMENT: REVIEWER_MODEL,
          DISPATCH_REVIEWER_MODEL_TOKENS_PER_MINUTE: "1000000",
        },
      });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    }, 600_000);
    afterAll(async () => {
      answer.resolve();
      await stack?.stop();
      await model?.stop(true);
    }, 60_000);

    test("dispatch ask prints the held ask's id, and no person reads the ask until its review posts it", async () => {
      const issue = await createIssue(PROJECT, "Ship the reviewer");

      const printed = await ask(issue.key, QUESTION);

      // The CLI says the ask is held, not that it was asked, and names the id it reserved.
      const id = /^Held for review \(reviewing\): ask (\S+) on /m.exec(printed)?.[1];
      if (id === undefined) throw new Error(`dispatch ask printed no held ask id: ${printed}`);
      expect(printed).not.toMatch(/^Asked /m);
      // The id the CLI printed is the held text's: the write was held, for the person who reads
      // the issue, and its review is running.
      const held = await asMachine<HeldRead>("GET", `/api/v1/held/${id}`);
      expect(held).toMatchObject({ id, kind: "ask", state: "reviewing", reader: PERSON });
      await reviewed.promise;
      expect(reviews).toHaveLength(1);
      expect(reviews[0]).toContain(QUESTION);
      // While it is held no person reads it: not by its id, and not in their Inbox.
      expect((await personRead(`/api/v1/asks/${id}`)).status).toBe(404);
      const inbox = await personRead("/api/v1/inbox");
      expect(inbox.status).toBe(200);
      expect(inbox.body).not.toContain(id);

      answer.resolve();

      // The review found nothing, so the ask posts as written, under the id the CLI printed.
      const posted = await within("the held ask's post", 30_000, async () => {
        const read = await personRead(`/api/v1/asks/${id}`);
        return read.status === 200
          ? (JSON.parse(read.body) as { ask: { id: string; question: string } })
          : undefined;
      });
      expect(posted.ask).toMatchObject({ id, question: QUESTION });
      expect(await asMachine<HeldRead>("GET", `/api/v1/held/${id}`)).toMatchObject({
        state: "posted",
        outcome: "passed",
      });
      expect((await personRead("/api/v1/inbox")).body).toContain(id);
    }, 120_000);
  }
);
