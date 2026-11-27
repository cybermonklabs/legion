import { expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { holdSecret } from "@legion/contracts/held-secrets";
import type { PiApi, RegisteredTool, SessionContext } from "@legion/pi-shared/pi-types";
import { scrubbedHost, scrubbedLogger } from "./scrubbed-host";

/** A host logger stand-in that records the arguments of each warning it is handed. */
function recordingLogger() {
  const written: unknown[][] = [];
  return { written, logger: scrubbedLogger({ warn: (...args: unknown[]) => written.push(args) }) };
}

// The extension's log lines (`envoy: failed to deliver envelope …`, with the error that failed it)
// go through the scrub of the secrets this process holds: a held value in a line's message or in a
// field beside it is written as `<withheld>`.
test("a held value in a log line's message and in its fields is written as <withheld>", () => {
  const held = holdSecret(`dsp_${randomBytes(24).toString("base64url")}`);
  const { written, logger } = recordingLogger();
  logger.warn(`envoy: failed to deliver envelope ev-${held.slice(4, 18)}`, {
    error: `the listener answered ${held}`,
    cause: new Error(`no session holds ${held}`),
  });
  const [[message, fields]] = written as [[string, { error: string; cause: Error }]];
  expect(message).toBe("envoy: failed to deliver envelope ev-<withheld>");
  expect(fields.error).toBe("the listener answered <withheld>");
  expect(fields.cause.message).toBe("no session holds <withheld>");
  expect(JSON.stringify(written)).not.toContain(held.slice(4, 18));
});

// The control of the row above: a line that holds no held value is written as it was.
test("a log line that holds no held value is written as it was", () => {
  holdSecret(`dsp_${randomBytes(24).toString("base64url")}`);
  const { written, logger } = recordingLogger();
  logger.warn("envoy: failed to deliver envelope ev_1", {
    error: "the listener did not answer",
  });
  expect(written).toEqual([
    ["envoy: failed to deliver envelope ev_1", { error: "the listener did not answer" }],
  ]);
});

// The host API's wrapper (TNQuality4140's P2 on 2fa6fbf7cf): an event handler's context and a
// tool's `execute` context are the named parameters of `PiApi`'s `on` and `RegisteredTool`'s
// `execute`, each scrubbing its notices, and `execute`'s other arguments reach the tool unchanged
// in their own places. A tool's text parts and its thrown error are scrubbed; an image part is not.
function host() {
  const notices: string[] = [];
  const handlers: ((event: unknown, context: SessionContext) => unknown)[] = [];
  const tools: RegisteredTool[] = [];
  const context = {
    cwd: "/work",
    ui: { notify: (text: string) => notices.push(text), setStatus: () => {} },
  } as unknown as SessionContext;
  const pi = {
    on: (_event: string, handler: (event: unknown, context: SessionContext) => unknown) => {
      handlers.push(handler);
    },
    registerTool: (tool: RegisteredTool) => {
      tools.push(tool);
    },
    getActiveTools: () => ["one"],
  } as unknown as PiApi;
  return { pi, notices, handlers, tools, context };
}

test("an event handler's context scrubs its notices, and the event reaches it as it was", async () => {
  const held = holdSecret(`dsp_${randomBytes(24).toString("base64url")}`);
  const { pi, notices, handlers, context } = host();
  const seen: unknown[] = [];
  scrubbedHost(pi).on("session_start", async (event, ctx) => {
    seen.push(event);
    ctx.ui.notify(`the listener answered ${held}`, "warning");
  });
  const event = { reason: "start" };
  await handlers[0]?.(event, context);
  expect(seen).toEqual([event]);
  expect(notices).toEqual(["the listener answered <withheld>"]);
});

test("a tool's execute gets its id, parameters, signal and update as given, and a scrubbing context", async () => {
  const held = holdSecret(`dsp_${randomBytes(24).toString("base64url")}`);
  const { pi, notices, tools, context } = host();
  const calls: unknown[][] = [];
  scrubbedHost(pi).registerTool({
    name: "t",
    label: "t",
    description: "t",
    parameters: {},
    execute: async (id, parameters, signal, onUpdate, ctx) => {
      calls.push([id, parameters, signal, onUpdate]);
      ctx.ui.notify(`notice ${held}`, "info");
      return {
        content: [
          { type: "text", text: `answer ${held}` },
          { type: "image", data: held, mimeType: "image/png" },
        ],
        details: {},
      };
    },
  });
  const signal = new AbortController().signal;
  const onUpdate = () => {};
  const result = await tools[0]?.execute("call-1", { a: 1 }, signal, onUpdate, context);
  expect(calls).toEqual([["call-1", { a: 1 }, signal, onUpdate]]);
  expect(notices).toEqual(["notice <withheld>"]);
  expect(result?.content).toEqual([
    { type: "text", text: "answer <withheld>" },
    { type: "image", data: held, mimeType: "image/png" },
  ]);
});

test("a tool's thrown error is scrubbed, and the host's other members are its own", async () => {
  const held = holdSecret(`dsp_${randomBytes(24).toString("base64url")}`);
  const { pi, tools, context } = host();
  const scrubbed = scrubbedHost(pi);
  scrubbed.registerTool({
    name: "t",
    label: "t",
    description: "t",
    parameters: {},
    execute: async () => {
      throw new Error(`the broker answered ${held}`);
    },
  });
  const error = await tools[0]
    ?.execute("call-1", {}, undefined, undefined, context)
    .catch((caught: unknown) => caught);
  expect((error as Error).message).toBe("the broker answered <withheld>");
  expect(scrubbed.getActiveTools()).toEqual(["one"]);
});
