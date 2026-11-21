import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { type AddressInfo, createServer, type Socket } from "node:net";
import * as path from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { bearerCredential } from "../bearer";
import { executeDispatchTool } from "../dispatch-execute";
import { type DispatchWriteRetry, forgetKeyedLayerMemory } from "../dispatch-http";
import { messageFor } from "../errors";

// A `dispatch` write that may have reached Dispatch prints the words the command prints for such an
// ending ("may or may not have taken effect") and says to check before retrying; a refusal Dispatch
// decided prints none of them, and neither does a request that writes nothing (INFRA-4071 R16,
// dispatch://INFRA-4395). The command reads what it prints, so each row runs a command through
// `executeDispatchTool` against a stub on a real socket: a dropped connection is Bun's own
// transport failure, passed through the command's fetch as a session meets it.

const makeDirectory = temporaryDirectories("dispatch-unsettled-write-words");

/** What the stub does with one request it read whole: answer it with Dispatch's JSON or with a
 *  page in its place, or drop the connection. */
type Reply =
  | { readonly status: number; readonly json: unknown }
  | { readonly status: number; readonly html: string }
  | "drop";

/**
 * A Dispatch on a real socket that reads each request whole and replies as `reply` says for its
 * `<METHOD> <path>`. One that runs the keyed layer (`keyed`) carries a write's key back on every
 * answer, a 5xx included, as Dispatch's keyed layer does, and says so on `/healthz`; one that does
 * not echoes no key, and its `/healthz` drops the connection, so a client can learn no layer from it.
 */
async function stubDispatch(options: {
  readonly keyed: boolean;
  readonly reply: (request: string) => Reply;
}) {
  const requests: string[] = [];
  const sockets = new Set<Socket>();
  const server = createServer((socket) => {
    sockets.add(socket);
    // A connection the stub drops, or one the client gives up on, errors on this side too; left
    // unheard, that error would redden the run.
    socket.on("error", () => {});
    socket.on("close", () => sockets.delete(socket));
    let received = Buffer.alloc(0);
    let read = false;
    socket.on("data", (data: Buffer) => {
      if (read) return;
      received = Buffer.concat([received, data]);
      const end = received.indexOf("\r\n\r\n");
      if (end === -1) return;
      const lines = received.subarray(0, end).toString("latin1").split("\r\n");
      const header = (name: string): string | undefined =>
        lines
          .find((line) => line.toLowerCase().startsWith(`${name}:`))
          ?.slice(name.length + 1)
          .trim();
      if (received.length - (end + 4) < Number(header("content-length") ?? 0)) return;
      read = true;
      const [method, target] = (lines[0] ?? "").split(" ");
      const pathname = (target ?? "").split("?")[0];
      if (pathname === "/healthz") {
        if (options.keyed) answer(socket, 200, { status: "ok", idempotency_keys: true });
        else socket.destroy();
        return;
      }
      const request = `${method} ${pathname}`;
      requests.push(request);
      const reply = options.reply(request);
      if (reply === "drop") {
        socket.destroy();
        return;
      }
      const key = options.keyed ? header("idempotency-key") : undefined;
      if ("json" in reply) answer(socket, reply.status, reply.json, key);
      else answer(socket, reply.status, reply.html, key, "text/html");
    });
  });
  const { promise: listening, resolve } = Promise.withResolvers<void>();
  server.listen(0, "127.0.0.1", resolve);
  await listening;
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    requests,
    close(): void {
      for (const socket of sockets) socket.destroy();
      server.close();
    },
  };
}

const REASONS: Readonly<Record<number, string>> = {
  200: "OK",
  201: "Created",
  409: "Conflict",
  500: "Internal Server Error",
  502: "Bad Gateway",
};

/** Answers `socket` with `status` and `body` (JSON unless `type` says otherwise), carrying back
 *  `key`, and closes it. */
function answer(
  socket: Socket,
  status: number,
  body: unknown,
  key?: string,
  type = "application/json"
): void {
  const text = typeof body === "string" ? body : JSON.stringify(body);
  socket.end(
    `HTTP/1.1 ${status} ${REASONS[status] ?? "Status"}\r\n` +
      `Content-Type: ${type}\r\n` +
      (key === undefined ? "" : `Idempotency-Key: ${key}\r\n`) +
      `Content-Length: ${Buffer.byteLength(text)}\r\nConnection: close\r\n\r\n${text}`
  );
}

/** Three attempts a few milliseconds apart, so a row that asks again under its key runs in real
 *  time without waiting out the production pauses. */
const writeRetry: DispatchWriteRetry = {
  pausesMs: [10, 10],
  withinMs: 10_000,
  silenceMs: 10_000,
  inFlightPauseMs: 10,
  inFlightWithinMs: 10_000,
};

/** What the `dispatch` command prints for `tool` with `args` against `url` when the call fails:
 *  the thrown error's message, as the CLI writes it. */
async function printed(
  url: string,
  tool: string,
  args: Record<string, unknown>,
  cwd = "/workspace"
): Promise<string> {
  return executeDispatchTool({
    tool,
    args,
    cwd,
    host: "omp",
    sessionId: "session-1",
    config: { url, credential: bearerCredential("secret") },
    env: {},
    exec: async (file, argv) => {
      throw new Error(`unexpected command: ${file} ${argv.join(" ")}`);
    },
    writeRetry,
  }).then(
    (result) => `the call succeeded: ${result.text}`,
    (error: unknown) => messageFor(error)
  );
}

const UPLOAD = "POST /api/v1/issues/DSP-1/artifacts";
const ROUND_TRIP = "POST /api/v1/markdown/round-trip";
const ASK = "5a660655-04ad-4ce0-8a9b-93dd03c412b7";
const ASK_READ = `GET /api/v1/asks/${ASK}`;
const FOLLOW = `PUT /api/v1/asks/${ASK}/followers/session-1`;

const MOVED_HINT =
  "If the Dispatch URL changed, restart this agent process so it picks up the new configuration.";
const DISPATCH_FAILED =
  "Dispatch answers a 5xx when a write fails, which can be after it committed, so it may or may " +
  "not have taken effect";
const READ_IT_BACK =
  ". Dispatch finishes an attempt it received whatever this command does, so read the write back " +
  "before you send it again: a new command sends it under a new key.";
const CHECK_FIRST = ": check whether it did before retrying it.";

const internal: Reply = { status: 500, json: { code: "INTERNAL", error: "the write failed" } };
const closed: Reply = { status: 409, json: { code: "ISSUE_CLOSED", error: "DSP-1 is closed" } };

describe("the words a dispatch write that may have landed prints", () => {
  const opened: { close(): void }[] = [];
  const dispatchOn = async (keyed: boolean, reply: (request: string) => Reply) => {
    const made = await stubDispatch({ keyed, reply });
    opened.push(made);
    return made;
  };

  // What a process learned of an origin's keyed layer outlives a call, and every stub is a new
  // origin, but a row must not lean on that.
  beforeEach(forgetKeyedLayerMemory);
  afterEach(() => {
    for (const made of opened.splice(0)) made.close();
  });

  /** `dispatch artifact --issue DSP-1 --name notes.txt --path notes.txt --force`: a keyed upload,
   *  which --force sends with no read of the issue's documents first. */
  const upload = (url: string) => {
    const cwd = makeDirectory();
    writeFileSync(path.join(cwd, "notes.txt"), "the notes\n");
    return printed(
      url,
      "dispatch_artifact",
      { issue: "DSP-1", name: "notes.txt", path: "notes.txt", force: true },
      cwd
    );
  };

  /** `dispatch follow --ask <id>`: a read of the ask, then the PUT that follows it, which carries
   *  no key and is sent once. */
  const follow = (url: string) => printed(url, "dispatch_follow", { ask: ASK, action: "follow" });

  const askRead: Reply = {
    status: 200,
    json: { ask: { id: ASK, issue_key: "DSP-1" }, replies: [], edits: [], followers: [] },
  };

  // A dropped connection may follow a write Dispatch read whole, so the upload names its request
  // and quotes the failure in the client's line for it, the advice for a moved URL with it, and
  // its last words are the unsettled-write words and what to do before a retry.
  test("a keyed upload whose every attempt the connection drops may or may not have taken effect", async () => {
    const dispatch = await dispatchOn(true, () => "drop");

    const text = await upload(dispatch.url);

    expect(dispatch.requests).toEqual([UPLOAD, UPLOAD, UPLOAD]);
    expect(text).toStartWith(
      `POST ${dispatch.url}/api/v1/issues/DSP-1/artifacts failed on its last attempt (Dispatch at ` +
        `${dispatch.url} is unreachable: `
    );
    expect(text).toEndWith(
      `. ${MOVED_HINT}) (the last of 3 attempts), and an attempt of the write may have reached ` +
        `Dispatch, so it may or may not have taken effect${READ_IT_BACK}`
    );
  });

  test("a keyed upload Dispatch answers 500 on every attempt may or may not have taken effect", async () => {
    const dispatch = await dispatchOn(true, () => internal);

    const text = await upload(dispatch.url);

    expect(dispatch.requests).toEqual([UPLOAD, UPLOAD, UPLOAD]);
    expect(text).toBe(
      `INTERNAL: the write failed (the last of 3 attempts), and ${DISPATCH_FAILED}${READ_IT_BACK}`
    );
  });

  // No layer was seen, so the write is asked once and nothing says Dispatch finishes it.
  test("a keyed upload a Dispatch without a keyed layer answers 500 is asked once and may or may not have taken effect", async () => {
    const dispatch = await dispatchOn(false, () => internal);

    const text = await upload(dispatch.url);

    expect(dispatch.requests).toEqual([UPLOAD]);
    expect(text).toBe(
      `INTERNAL: the write failed (its only attempt), and ${DISPATCH_FAILED}${CHECK_FIRST}`
    );
  });

  test("a keyed upload Dispatch refuses prints the refusal alone", async () => {
    const dispatch = await dispatchOn(true, () => closed);

    const text = await upload(dispatch.url);

    expect(dispatch.requests).toEqual([UPLOAD]);
    expect(text).toBe("ISSUE_CLOSED: DSP-1 is closed");
  });

  // `dispatch artifact --issue DSP-1 --name notes.md --content … --force` first sends inline
  // Markdown through the round trip, a POST that keeps its key and its attempts but stores
  // nothing, so no ending of it says a write may have landed; the upload it guards is never sent.
  const roundTrip = async (reply: Reply) => {
    const dispatch = await dispatchOn(true, () => reply);
    const text = await printed(dispatch.url, "dispatch_artifact", {
      issue: "DSP-1",
      name: "notes.md",
      content: "# Notes\n",
      force: true,
    });
    expect(dispatch.requests).toEqual([ROUND_TRIP, ROUND_TRIP, ROUND_TRIP]);
    expect(text).not.toContain("may or may not");
    return { url: dispatch.url, text };
  };
  const unchecked =
    "Dispatch could not say how it would store this text, so nothing was uploaded: POST " +
    "/api/v1/markdown/round-trip failed with";

  test("a markdown round trip whose connection drops prints none of the words", async () => {
    const { url, text } = await roundTrip("drop");

    expect(text).toStartWith(`${unchecked} Dispatch at ${url} is unreachable: `);
    expect(text).toEndWith(`${MOVED_HINT} Send the upload again once that call is answered.`);
  });

  test("a markdown round trip Dispatch answers 500 prints none of the words", async () => {
    const { text } = await roundTrip({
      status: 500,
      json: { code: "INTERNAL", error: "the round trip failed" },
    });

    expect(text).toBe(
      `${unchecked} INTERNAL: the round trip failed. Send the upload again once that call is answered.`
    );
  });

  // A gateway's 5xx to a write may have reached Dispatch after it forwarded the write; to a request
  // that writes nothing it is what it is to a read, which a retry may get past.
  test("a markdown round trip a gateway answers 502 prints a read's advice", async () => {
    const { url, text } = await roundTrip({
      status: 502,
      html: "<html><body>502 Bad Gateway</body></html>",
    });

    expect(text).toBe(
      `${unchecked} HTTP_502: POST ${url}/api/v1/markdown/round-trip answered 502 Bad Gateway with ` +
        `a body that is not Dispatch's error JSON ("502 Bad Gateway"), which looks like a proxy or ` +
        "gateway page rather than Dispatch's own answer, so a retry may succeed. Send the upload " +
        "again once that call is answered."
    );
  });

  test("an unkeyed PUT whose connection drops may or may not have taken effect", async () => {
    const dispatch = await dispatchOn(true, (request) => (request === ASK_READ ? askRead : "drop"));

    const text = await follow(dispatch.url);

    expect(dispatch.requests).toEqual([ASK_READ, FOLLOW]);
    expect(text).toStartWith(
      `PUT ${dispatch.url}/api/v1/asks/${ASK}/followers/session-1 failed on its last attempt ` +
        `(Dispatch at ${dispatch.url} is unreachable: `
    );
    expect(text).toEndWith(
      `. ${MOVED_HINT}) (its only attempt), and an attempt of the write may have reached ` +
        `Dispatch, so it may or may not have taken effect${CHECK_FIRST}`
    );
  });

  test("an unkeyed PUT Dispatch answers 500 may or may not have taken effect", async () => {
    const dispatch = await dispatchOn(true, (request) =>
      request === ASK_READ ? askRead : internal
    );

    const text = await follow(dispatch.url);

    expect(dispatch.requests).toEqual([ASK_READ, FOLLOW]);
    expect(text).toBe(
      `INTERNAL: the write failed (its only attempt), and ${DISPATCH_FAILED}${CHECK_FIRST}`
    );
  });

  test("an unkeyed PUT Dispatch refuses prints the refusal alone", async () => {
    const dispatch = await dispatchOn(true, (request) => (request === ASK_READ ? askRead : closed));

    const text = await follow(dispatch.url);

    expect(dispatch.requests).toEqual([ASK_READ, FOLLOW]);
    expect(text).toBe("ISSUE_CLOSED: DSP-1 is closed");
  });

  // A read that gets no answer says the server is unreachable, and nothing of a write.
  test("a read whose connection drops names the server as unreachable", async () => {
    const dispatch = await dispatchOn(true, () => "drop");

    const text = await follow(dispatch.url);

    expect(dispatch.requests).toEqual([ASK_READ]);
    expect(text).toStartWith(`Dispatch at ${dispatch.url} is unreachable: `);
    expect(text).toEndWith(MOVED_HINT);
    expect(text).not.toContain("may or may not");
  });
});
