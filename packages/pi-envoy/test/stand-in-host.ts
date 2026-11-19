// The stand-in Oh My Pi host `extensions/envoy.ts` is driven against off a real session: the
// surface the extension sees (a `PiApi`) and a recorder of everything it registers, sends and
// appends. Three callers share it, which is why it is a module of its own: the package's own suite
// (`extensions/envoy.test.ts`, through its `createPi`, which adds the clipboard the suite mocks),
// `scripts/smoke-role-takeover.sh`'s client process (`scripts/role-takeover/omp-client.ts`), one
// client instance per OS process, and `test/session-pod-process.ts`, one session of a session pod
// in an OS process of its own. A host method the extension starts to use is added here once, so
// neither process can drift behind the suite.
import type {
  CommandContext,
  RegisteredTool as HostRegisteredTool,
  MessageRenderer,
  PiApi,
  PiZod,
  SessionContext,
  ToolResult,
} from "@legion/pi-shared/pi-types";
import { z } from "zod";

// What a row hands a handler, a tool or a command is the context the extension declares it takes.
export type { CommandContext, SessionContext, ToolResult };

/**
 * A tool the extension registered, as a row calls it: the host's `RegisteredTool`, whose signal,
 * progress callback and context a row may leave out, as no host does. The extension's tools are
 * called that way throughout the suite, so the stand-in records each tool as this.
 */
export type RegisteredTool = Omit<HostRegisteredTool, "execute"> & {
  readonly execute: (
    id: string,
    params: Record<string, unknown>,
    signal?: AbortSignal,
    onUpdate?: unknown,
    context?: SessionContext
  ) => Promise<ToolResult>;
};

/** A slash command the extension registered, under its name. */
export type RegisteredCommand = { readonly name: string } & Parameters<PiApi["registerCommand"]>[1];

/** A card the extension sent: the host's `sendMessage` message that carries content. */
export type Card = Extract<Parameters<PiApi["sendMessage"]>[0], { readonly customType: string }>;

/** `message` as the card it is: the extension sends cards alone, so any other message is refused,
 *  naming its type. */
export function sentCard(message: Parameters<PiApi["sendMessage"]>[0]): Card {
  if (!("customType" in message)) {
    throw new Error(`the stand-in host records cards alone, not a ${message.type} message`);
  }
  return message;
}

/** A message the extension sent through `pi.sendMessage`, as the host records it. */
export type Delivery = {
  readonly content: Card["content"];
  readonly customType: string;
  readonly details?: Card["details"];
  readonly display: boolean;
  readonly options: unknown;
};

/**
 * An event handler the extension registered through `pi.on`, as a row calls it: with whatever
 * event the row builds, a partial or malformed one included, and the context of a session.
 */
export type EventHandler = (event: unknown, context: SessionContext) => Promise<unknown>;

/** A custom entry the extension persisted, in the shape `getBranch()` and `getEntries()` return. */
export type BranchEntry = {
  readonly type: "custom";
  readonly customType: string;
  readonly data: unknown;
};

/** A user turn the extension started through `pi.sendUserMessage`, as the host records it. */
export type UserTurn = {
  readonly content: Parameters<PiApi["sendUserMessage"]>[0];
  readonly options: unknown;
};

/**
 * One stand-in host: `pi`, to hand the extension, and the arrays and maps it records into. The
 * caller reads `tools` and `handlers` to drive the extension, and `entries` is also the branch a
 * session manager hands back (`omp-client.ts`'s does).
 */
export type StandInHost = {
  readonly commands: RegisteredCommand[];
  readonly deliveries: Delivery[];
  readonly entries: BranchEntry[];
  readonly handlers: Map<string, EventHandler>;
  /** The content of each card, in the order the extension sent them. */
  readonly messages: Delivery["content"][];
  readonly pi: PiApi;
  readonly renderers: Map<string, MessageRenderer>;
  readonly tools: RegisteredTool[];
  /** The user turns the extension started, in the order it sent them. */
  readonly userMessages: UserTurn[];
};

/** A stand-in host; `zod` replaces the `z` the extension validates with, for the one case that
 *  injects another. */
export function createStandInHost(options: { readonly zod?: typeof z } = {}): StandInHost {
  const commands: RegisteredCommand[] = [];
  const tools: RegisteredTool[] = [];
  const handlers = new Map<string, EventHandler>();
  const renderers = new Map<string, MessageRenderer>();
  const messages: Delivery["content"][] = [];
  const deliveries: Delivery[] = [];
  const userMessages: UserTurn[] = [];
  const entries: BranchEntry[] = [];
  const pi: PiApi = {
    // Real zod where the host hands its own: `PiZod` declares only the calls the extension makes,
    // and each of zod's own takes zod's node types where `PiZod`'s takes `unknown`.
    zod: (options.zod ?? z) as PiZod,
    // A row calls a tool with fewer arguments than a host passes (`RegisteredTool`).
    registerTool: (tool) => tools.push(tool as RegisteredTool),
    registerCommand: (name, command) => commands.push({ name, ...command }),
    registerMessageRenderer: (customType, renderer) => renderers.set(customType, renderer),
    // A row hands a handler the event it builds (`EventHandler`).
    on: (event, handler) => handlers.set(event, handler as EventHandler),
    sendMessage: (message, options) => {
      const card = sentCard(message);
      messages.push(card.content);
      deliveries.push({
        content: card.content,
        customType: card.customType,
        details: card.details,
        display: card.display,
        options,
      });
    },
    sendUserMessage: (content, options) => {
      userMessages.push({ content, options });
    },
    appendEntry: (customType, data) => {
      entries.push({ type: "custom", customType, data });
    },
    // Host members the extension never calls. Each throws, naming itself, as a call to a member
    // the host lacked would fail, so a row that reaches one names what to add here.
    setSessionName: () => {
      throw new Error("the stand-in host has no setSessionName");
    },
    getActiveTools: () => {
      throw new Error("the stand-in host has no getActiveTools");
    },
    setActiveTools: () => {
      throw new Error("the stand-in host has no setActiveTools");
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
