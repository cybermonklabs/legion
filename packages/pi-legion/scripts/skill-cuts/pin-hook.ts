/**
 * The hook the pinned Oh My Pi loads where checkout.ts answers a call with the pin's own tool
 * (`omp -p --hook <this file> --model stub/call x`, checkout.ts's `pinAnswer`), with no model:
 *
 * - It registers a provider `stub` of one model, `call`, whose stream (built with pi-ai's
 *   `createAssistantMessageEventStream`, the host's own) asks for no network and holds no key (its
 *   `apiKey` a placeholder, its `baseUrl` a port nothing listens on, in a sandbox with no
 *   network): its first turn is one tool call, the call PIN_CALL_FILE beside this file names (its
 *   tool and its arguments as the session's model gave them), ended for `toolUse`; its next, once
 *   the tool's result is in, one text, ended for `stop`: PIN_RESULT_MARKER, then the JSON of a
 *   PinResult, the result's content as the pin's tool gave it and the artifacts the call made,
 *   which `omp -p` prints, alone, as its answer.
 * - Its `session_start` handler writes into the session's artifacts directory, before any tool
 *   runs, the files the call names (`seed`): Oh My Pi numbers each artifact it saves one past the
 *   highest number a file there holds, so a seed `<n>.seed.log` makes the next ids n+1 and on.
 *
 * It imports Node's own modules alone at its top, since Oh My Pi loads it from a directory of its
 * own; pi-ai is the host's, imported when Oh My Pi calls the hook, so checkout.ts reads this
 * module's names without it.
 */
import { mkdirSync, readdirSync, readFileSync, writeFileSync } from "node:fs";
import path from "node:path";

/** The file beside this hook that names its call (a PinCall's JSON). */
export const PIN_CALL_FILE = "call.json";

/** What the stub's last text starts with, before the JSON of a PinResult. */
export const PIN_RESULT_MARKER = "skill-cuts pin result:";

/** The call the stub makes: its tool, its arguments, and the files written into the session's
 *  artifacts directory first (each name to its text). */
export interface PinCall {
  readonly tool: string;
  readonly arguments: Readonly<Record<string, unknown>>;
  readonly seed: Readonly<Record<string, string>>;
}

/** What the call gave: the tool's result content and whether it is an error, as the session's
 *  model would read them, and each file of the artifacts directory the call did not seed (each
 *  name to its text). */
export interface PinResult {
  readonly content: readonly { readonly type: string; readonly text?: string }[];
  readonly isError: boolean;
  readonly artifacts: Readonly<Record<string, string>>;
}

/** A message of the context the stub's stream is given. */
interface ContextMessage {
  readonly role: string;
  readonly content?: PinResult["content"];
  readonly isError?: boolean;
}

/** The event stream pi-ai builds for a provider of an extension's own. */
interface AssistantStream {
  push(event: Readonly<Record<string, unknown>>): void;
}

declare module "@oh-my-pi/pi-ai" {
  export function createAssistantMessageEventStream(): AssistantStream;
}

/** The part of Oh My Pi's extension API this hook uses. */
interface StubApi {
  on(
    event: "session_start",
    handler: (
      event: unknown,
      ctx: { readonly sessionManager: { getArtifactsDir(): string | null } }
    ) => Promise<void>
  ): void;
  registerProvider(
    name: string,
    config: {
      readonly baseUrl: string;
      readonly apiKey: string;
      readonly api: string;
      readonly models: readonly Readonly<Record<string, unknown>>[];
      readonly streamSimple: (
        model: { readonly id: string; readonly api: string; readonly provider: string },
        context: { readonly messages: readonly ContextMessage[] }
      ) => AssistantStream;
    }
  ): void;
}

/** A turn's usage: none. */
const NO_USAGE = {
  input: 0,
  output: 0,
  cacheRead: 0,
  cacheWrite: 0,
  totalTokens: 0,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};

export default async function pinHook(pi: StubApi): Promise<void> {
  // The host's own pi-ai, which only the pin that loads this hook holds: a static import would fail
  // in the runner, which imports this module's names and has no pi-ai.
  const { createAssistantMessageEventStream } = await import("@oh-my-pi/pi-ai");
  const call = JSON.parse(
    readFileSync(path.join(import.meta.dir, PIN_CALL_FILE), "utf8")
  ) as PinCall;
  let artifacts: string | undefined;
  pi.on("session_start", async (_event, ctx) => {
    artifacts = ctx.sessionManager.getArtifactsDir() ?? undefined;
    if (artifacts === undefined) return;
    mkdirSync(artifacts, { recursive: true });
    for (const [name, text] of Object.entries(call.seed)) {
      writeFileSync(path.join(artifacts, name), text);
    }
  });
  let called = false;
  pi.registerProvider("stub", {
    baseUrl: "http://127.0.0.1:9",
    // fixture-secret-next-line: a placeholder no provider reads, in a sandbox with no network
    apiKey: "STUB_PLACEHOLDER_NOT_A_KEY",
    api: "stub-api",
    models: [
      {
        id: "call",
        name: "call",
        reasoning: false,
        input: ["text"],
        cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
        contextWindow: 1_000_000,
        maxTokens: 1_000,
      },
    ],
    streamSimple: (model, context) => {
      const stream = createAssistantMessageEventStream();
      const base = {
        role: "assistant",
        api: model.api,
        provider: model.provider,
        model: model.id,
        usage: NO_USAGE,
        timestamp: Date.now(),
      };
      const turn = (stopReason: string, content: readonly Readonly<Record<string, unknown>>[]) => ({
        ...base,
        content,
        stopReason,
      });
      queueMicrotask(() => {
        if (!called) {
          called = true;
          const toolCall = {
            type: "toolCall",
            id: "call_stub_1",
            name: call.tool,
            arguments: call.arguments,
          };
          stream.push({ type: "start", partial: turn("toolUse", []) });
          stream.push({
            type: "toolcall_start",
            contentIndex: 0,
            partial: turn("toolUse", [toolCall]),
          });
          stream.push({
            type: "toolcall_end",
            contentIndex: 0,
            toolCall,
            partial: turn("toolUse", [toolCall]),
          });
          stream.push({ type: "done", reason: "toolUse", message: turn("toolUse", [toolCall]) });
          return;
        }
        const last = context.messages.findLast((message) => message.role === "toolResult");
        const made =
          artifacts === undefined
            ? []
            : readdirSync(artifacts, { withFileTypes: true }).filter((entry) => entry.isFile());
        const result: PinResult = {
          content: last?.content ?? [],
          isError: last?.isError === true,
          artifacts: Object.fromEntries(
            made
              .filter(({ name }) => !Object.hasOwn(call.seed, name))
              .map(({ name }) => [name, readFileSync(path.join(artifacts ?? "", name), "utf8")])
          ),
        };
        const text = `${PIN_RESULT_MARKER}${JSON.stringify(result)}`;
        stream.push({ type: "start", partial: turn("stop", []) });
        stream.push({
          type: "text_start",
          contentIndex: 0,
          partial: turn("stop", [{ type: "text", text: "" }]),
        });
        stream.push({
          type: "text_end",
          contentIndex: 0,
          content: text,
          partial: turn("stop", [{ type: "text", text }]),
        });
        stream.push({
          type: "done",
          reason: "stop",
          message: turn("stop", [{ type: "text", text }]),
        });
      });
      return stream;
    },
  });
}
