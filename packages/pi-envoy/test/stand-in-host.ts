// The stand-in Oh My Pi host `extensions/envoy.ts` is driven against off a real session: the
// surface the extension sees (`TestPi`) and a recorder of everything it registers, sends and
// appends. Five callers share it, which is why it is a module of its own: the package's own suite
// (`extensions/envoy.test.ts` and `extensions/consumer-delivery.test.ts`, through `createPi` in
// `test/extension-harness.ts`, which adds the clipboard the suite mocks),
// `scripts/smoke-role-takeover.sh`'s client process (`scripts/role-takeover/omp-client.ts`), one
// client instance per OS process, `test/session-pod-process.ts`, one session of a session pod in
// an OS process of its own, `test/relay-session-process.ts`, one session whose inbound is the
// listener's relay socket in an OS process of its own, and `test/stopping-session.ts`, the
// instance `extensions/stopping-session.test.ts` sends a SIGTERM to in an OS process of its own.
// A host method the extension starts to use is added here once, so none of those processes can
// drift behind the suite.
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer";
import type { MessageRenderer, PiApi } from "@legion/pi-shared/pi-types";
import { z } from "zod";

export type ToolResult = {
  readonly content: readonly { readonly type: "text"; readonly text: string }[];
  readonly details: Readonly<Record<string, unknown>>;
  readonly isError?: boolean;
};

export type RegisteredTool = {
  readonly name: string;
  readonly description: string;
  readonly parameters: unknown;
  readonly execute: (
    id: string,
    params: Record<string, unknown>,
    signal?: AbortSignal,
    onUpdate?: unknown,
    context?: SessionContext
  ) => Promise<ToolResult>;
};

export type RegisteredCommand = {
  readonly name: string;
  readonly description: string | undefined;
  readonly handler: (args: string, context: CommandContext) => Promise<void>;
};

export type SessionContext = {
  readonly cwd: string;
  /** Oh My Pi's agent identity: the top-level session's runner is `main`, a `task` subagent's `sub`. */
  readonly agent?: { readonly kind: "main" | "sub" };
  readonly sessionManager: {
    readonly getSessionId: () => string;
    readonly getSessionName?: () => string | undefined;
    readonly getBranch?: () => readonly unknown[];
    readonly getEntries: () => readonly unknown[];
    readonly getSessionFile: () => string | undefined;
    readonly ensureOnDisk: () => Promise<void>;
  };
  readonly setInterval: (callback: () => void, intervalMs: number) => void;
  /** The host's managed `ctx.setTimeout`: the one-shot timer the extension arms through the
   *  session (the heartbeat's extra call after a failed pass), cleared with it. */
  readonly setTimeout: (callback: () => void | Promise<void>, delayMs: number) => void;
  /** The host's `ctx.isIdle()`: false while a turn runs. */
  readonly isIdle?: () => boolean;
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning") => void;
    readonly onTerminalInput: (handler: (input: unknown) => void) => () => void;
    readonly getEditorText: () => string;
  };
};

export type CommandContext = {
  readonly ui: {
    readonly notify: (message: string, level: "info" | "warning" | "error") => void;
  };
};

export type TestPi = {
  readonly zod: typeof z;
  readonly registerTool: (tool: RegisteredTool) => void;
  readonly registerCommand: (name: string, command: Omit<RegisteredCommand, "name">) => void;
  readonly registerMessageRenderer: PiApi["registerMessageRenderer"];
  readonly on: (
    event:
      | "resources_discover"
      | "session_start"
      | "session_switch"
      | "session_branch"
      | "session_tree"
      | "session_shutdown"
      | "before_agent_start"
      | "agent_end"
      | "message_start"
      | "session_stop"
      | "input"
      | "tool_call"
      | "tool_result",
    handler: (event: unknown, context: SessionContext) => Promise<unknown>
  ) => void;
  readonly sendMessage: (
    message: { readonly content: string; readonly customType?: string; readonly display?: boolean },
    options: unknown
  ) => void;
  readonly sendUserMessage: (content: string, options?: unknown) => void;
  readonly appendEntry: PiApi["appendEntry"];
};

/** A message the extension sent through `pi.sendMessage`, as the host records it. */
export type Delivery = {
  readonly content: string;
  readonly customType?: string;
  readonly details?: unknown;
  readonly display?: boolean;
  readonly options: unknown;
};

/** An event handler the extension registered through `pi.on`. */
export type EventHandler = (event: unknown, context: SessionContext) => Promise<unknown>;

/** A custom entry the extension persisted, in the shape `getBranch()` and `getEntries()` return. */
export type BranchEntry = {
  readonly type: "custom";
  readonly customType: string;
  readonly data: unknown;
};

/**
 * One stand-in host: `pi`, to hand the extension, and the arrays and maps it records into. The
 * caller reads `tools` and `handlers` to drive the extension, and `entries` is also the branch a
 * session manager hands back (`omp-client.ts`'s does).
 *
 * `zod` replaces the `z` the extension validates with, for the one case that injects another.
 */
export function createStandInHost(options: { readonly zod?: typeof z } = {}) {
  const commands: RegisteredCommand[] = [];
  const tools: RegisteredTool[] = [];
  const handlers = new Map<string, EventHandler>();
  const renderers = new Map<string, MessageRenderer>();
  const messages: string[] = [];
  const deliveries: Delivery[] = [];
  // User turns the extension started, in the order it sent them.
  const userMessages: { readonly content: string; readonly options: unknown }[] = [];
  const entries: BranchEntry[] = [];
  const pi: TestPi = {
    zod: options.zod ?? z,
    registerTool: (tool) => tools.push(tool),
    registerCommand: (name, command) => commands.push({ name, ...command }),
    registerMessageRenderer: (customType, renderer) => renderers.set(customType, renderer),
    on: (event, handler) => handlers.set(event, handler),
    sendMessage: (message, options) => {
      messages.push(message.content);
      deliveries.push({
        content: message.content,
        customType: message.customType,
        details: "details" in message ? message.details : undefined,
        display: message.display,
        options,
      });
    },
    sendUserMessage: (content, options) => {
      userMessages.push({ content, options });
    },
    appendEntry: (customType, data) => {
      entries.push({ type: "custom", customType, data });
    },
  };
  return {
    commands,
    deliveries,
    entries,
    handlers,
    messages,
    pi,
    renderers,
    tools,
    userMessages,
  };
}

/**
 * The context a script that runs one session in an OS process of its own hands `session_start`
 * (`test/session-pod-process.ts`, `test/relay-session-process.ts`): session `sessionID`, in `cwd`,
 * whose branch is `entries`, idle and holding no message (so the extension hands its transcript
 * notices over, as to a real idle host), with no timers of the host's, and the UI's notices handed
 * to `notify`.
 */
export function processSessionContext(options: {
  readonly sessionID: string;
  readonly cwd: string;
  readonly entries: readonly unknown[];
  readonly notify?: (message: string) => void;
}) {
  return {
    cwd: options.cwd,
    hasUI: true,
    agent: { kind: "main" as const },
    sessionManager: {
      getSessionId: () => options.sessionID,
      getSessionName: () => undefined,
      getBranch: (): readonly unknown[] => options.entries,
      getEntries: (): readonly unknown[] => options.entries,
      getSessionFile: () => undefined,
      ensureOnDisk: async (): Promise<void> => undefined,
    },
    isIdle: () => true,
    hasPendingMessages: () => false,
    setInterval: () => undefined,
    setTimeout: () => undefined,
    ui: {
      notify: (message: string) => options.notify?.(message),
      onTerminalInput: () => () => undefined,
      getEditorText: () => "",
    },
  };
}

/**
 * A listener in `globalThis.fetch` for such a script: `route` answers first where it answers at
 * all; then every registration is accepted with the session's own consumer, a role read answers
 * that no session holds it, and anything else answers `{}`. `route` is handed the path and the
 * request's JSON body.
 */
export function standInListenerFetch(
  route: (
    pathname: string,
    body: Record<string, unknown>,
    input: Parameters<typeof fetch>[0],
    init: RequestInit | undefined
  ) => Response | Promise<Response> | undefined = () => undefined
): typeof fetch {
  return (async (input: Parameters<typeof fetch>[0], init?: RequestInit) => {
    const { pathname } = new URL(input instanceof Request ? input.url : input.toString());
    const body: Record<string, unknown> =
      typeof init?.body === "string" ? JSON.parse(init.body) : {};
    const routed = await route(pathname, body, input, init);
    if (routed !== undefined) return routed;
    if (pathname === "/v1/interests/subscribe") {
      return Response.json({
        session_id: body.session_id,
        machine_id: "test",
        dir: body.dir,
        topics: body.topics,
        delivery: consumerDelivery(String(body.session_id)),
      });
    }
    if (pathname.startsWith("/v1/roles/")) {
      return Response.json({ error: "no holder" }, { status: 404 });
    }
    return Response.json({});
  }) as typeof fetch;
}
