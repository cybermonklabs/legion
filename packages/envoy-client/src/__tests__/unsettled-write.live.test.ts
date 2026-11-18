import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createServer, type IncomingMessage, type Server } from "node:http";
import type { AddressInfo } from "node:net";
import { join } from "node:path";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
} from "../../test/dispatch-stack";
import { resendOf } from "./resend-line";

// A write whose answer is lost, over the whole path an agent's write takes: the real `dispatch`
// CLI of this checkout, run as a child process, against a real Dispatch server of this checkout
// and its keyed layer, on a database of its own. Between the two is a proxy that lets the write
// through to Dispatch and then breaks the connection before any answer comes back, on every
// attempt of the first run. The unit rows (dispatch-cli-unsettled.test.ts) prove the CLI over a
// stand-in of the keyed layer; this proves that the key the CLI names and the command it prints
// are the ones the real layer answers from its first run, so the write is made once.

const { PATH: SHELL_PATH } = process.env;

const PERSON = "alice";
const SESSION = "session-unsettled-write-live";
const PROJECT = "LOST";
const ENTRY = join(import.meta.dir, "..", "..", "bin", "dispatch.ts");
const BODY = "The answer to this message is lost on the way back.";

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const directories = useDispatchCliDirectories();

/** The request's whole body. */
async function bodyOf(request: IncomingMessage): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const chunk of request) chunks.push(chunk as Buffer);
  return Buffer.concat(chunks);
}

/** A message as the issue's event log carries it. */
interface MessageEvent {
  readonly type: string;
  readonly payload: { readonly body?: string };
}

describe.skipIf(!dispatchStackEnabled)(
  "a write whose answer is lost, over the real CLI against a real Dispatch",
  () => {
    let stack: DispatchStack | undefined;
    let proxy: Server | undefined;
    let proxyUrl = "";
    /** The route whose every answer the proxy breaks off while it is set. */
    let losing: string | undefined;
    /** Each request the proxy took for that route: its Idempotency-Key, and whether it reached
     *  Dispatch. */
    const lost: { key: string | undefined; forwarded: boolean }[] = [];

    const { asPerson, createIssue } = dispatchCliLive({
      stack: () => stack,
      session: SESSION,
      person: PERSON,
      spec: "# Design\n\nThe design, as it stands.\n",
      directories,
    });

    /** The real CLI entry run as an agent's shell runs it, through the proxy. */
    async function dispatch(argv: readonly string[]) {
      const child = Bun.spawn([process.execPath, ENTRY, ...argv], {
        env: {
          PATH: SHELL_PATH,
          HOME: directories.state,
          DISPATCH_STATE_DIR: directories.state,
          DISPATCH_HOST: "omp",
          DISPATCH_SESSION_ID: SESSION,
          DISPATCH_URL: proxyUrl,
          DISPATCH_TOKEN: stack?.token ?? "",
        },
        stdout: "pipe",
        stderr: "pipe",
      });
      const [stdout, stderr, code] = await Promise.all([
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
        child.exited,
      ]);
      return { stdout, stderr, code };
    }

    beforeAll(async () => {
      stack = await startDispatchStack({ work: directories.stack });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
      const upstream = stack.url;
      proxy = createServer(async (request, response) => {
        const body = await bodyOf(request);
        const route = `${request.method} ${request.url}`;
        const lose = route === losing;
        const key = request.headers["idempotency-key"];
        // A later attempt of the lost write is broken off before it reaches Dispatch, so the run
        // gets no answer of Dispatch's at all.
        if (lose && lost.length > 0) {
          lost.push({ key: typeof key === "string" ? key : undefined, forwarded: false });
          request.socket.destroy();
          return;
        }
        const headers = new Headers();
        for (const [name, value] of Object.entries(request.headers)) {
          if (typeof value === "string" && !["host", "connection", "content-length"].includes(name))
            headers.set(name, value);
        }
        const answer = await fetch(`${upstream}${request.url}`, {
          method: request.method ?? "GET",
          headers,
          ...(body.length === 0 ? {} : { body: new Uint8Array(body) }),
        });
        const answered = Buffer.from(await answer.arrayBuffer());
        // The first attempt reached Dispatch and was answered; the answer is lost on the way back.
        if (lose) {
          lost.push({ key: typeof key === "string" ? key : undefined, forwarded: true });
          request.socket.destroy();
          return;
        }
        const relayed: Record<string, string> = {};
        answer.headers.forEach((value, name) => {
          if (
            !["content-encoding", "content-length", "transfer-encoding", "connection"].includes(
              name
            )
          )
            relayed[name] = value;
        });
        response.writeHead(answer.status, relayed);
        response.end(answered);
      });
      const listening = Promise.withResolvers<void>();
      proxy.listen(0, "127.0.0.1", () => listening.resolve());
      await listening.promise;
      proxyUrl = `http://127.0.0.1:${(proxy.address() as AddressInfo).port}`;
    }, 600_000);
    afterAll(async () => {
      const closed = Promise.withResolvers<void>();
      if (proxy === undefined) closed.resolve();
      else proxy.close(() => closed.resolve());
      await closed.promise;
      await stack?.stop();
    }, 60_000);

    test("the CLI exits 75 naming its key, and the command it prints makes the write once", async () => {
      const issue = await createIssue(PROJECT, "Send a message whose answer is lost");
      const message = ["message", "--issue", issue.key, "--body", BODY];
      losing = `POST /api/v1/issues/${issue.key}/messages`;

      const first = await dispatch(message);
      expect(first.code, `${first.stdout}${first.stderr}`).toBe(75);
      const { key, words } = resendOf(first.stdout);
      expect(words).toEqual(["dispatch", ...message, "--idempotency-key", key]);
      // Every attempt went under the key the CLI named, and only the first reached Dispatch. How
      // many attempts there were is the write's schedule and its time bound, not this row's.
      expect(lost[0]).toEqual({ key, forwarded: true });
      expect(lost.length).toBeGreaterThan(1);
      expect(lost.slice(1)).toEqual(lost.slice(1).map(() => ({ key, forwarded: false })));

      losing = undefined;
      const again = await dispatch(words.slice(1));
      expect(again.code, `${again.stdout}${again.stderr}`).toBe(0);
      expect(again.stdout).toStartWith("Posted message");

      const events = await asPerson<MessageEvent[]>(
        "GET",
        `/api/v1/issues/${issue.key}/events?limit=200`
      );
      const posted = events.filter(
        (event) => event.type === "message.created" && event.payload.body === BODY
      );
      expect(posted).toHaveLength(1);
    }, 120_000);
  }
);
