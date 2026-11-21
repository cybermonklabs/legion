// The recorder approval-context-capture.omp.test.ts and approval-message-capture.ts load after
// pi-envoy and pi-legion: it writes what the pinned Oh My Pi hands an extension on each model
// request, one JSON line per event, to the file LEGION_CAPTURE_RECORD names. Loaded last, its
// `context` handler sees the messages both plugins return, and its `before_provider_request`
// handler sees the wire body they leave. JSON drops the host's per-call Symbol
// (`agent.message.perCallContext`, a `Symbol()` and not a `Symbol.for`), so the recorder writes it
// beside each wire message as a flag of its own.
//
// While the file LEGION_CAPTURE_PLACEMENT names exists, the recorder also stands in for the
// approval message, for the F1 capture of a build of pi-legion that placed none: on the first
// main-loop request it adds a custom message of APPROVAL_MESSAGE_TYPE with that file's text, last,
// and on every later request it adds the same message right before its answer, the first
// assistant message stamped at or after it. approval-message-capture.ts never sets
// LEGION_CAPTURE_PLACEMENT, so every approval message it records is pi-legion's own.
// Without LEGION_CAPTURE_RECORD the recorder does nothing.
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import {
  APPROVAL_MESSAGE_TYPE,
  isHandoffPrompt,
  isSideTurn,
  liveAgentSession,
  messageKey,
} from "@legion/pi-shared/approval-message";
import * as host from "@oh-my-pi/pi-coding-agent";
import { perCall, roleOf, timestampOf } from "../src/approval-message";

/** What the recorder reads of a handler's context. */
interface RecorderContext {
  readonly agent?: unknown;
  readonly sessionManager: {
    getSessionId(): string;
    getBranch(): readonly unknown[];
  };
  readonly model?: {
    readonly provider?: string;
    readonly id?: string;
    readonly input?: readonly string[];
    readonly compat?: { readonly supportsMidConversationSystem?: boolean };
  };
}

/** What the recorder uses of the injected API: `on`, for events the shared `PiApi` leaves out. */
interface RecorderApi {
  on(
    event: string,
    handler: (event: Record<string, unknown>, context: RecorderContext) => unknown
  ): void;
}

function record(file: string, event: string, fields: Record<string, unknown>): void {
  appendFileSync(file, `${JSON.stringify({ at: Date.now(), event, ...fields })}\n`);
}

/** The session's live compaction state, read as pi-envoy reads it (`sessionCompacting`). */
function liveState(session: string): Record<string, unknown> {
  const live = liveAgentSession(host.AgentRegistry, session);
  if (live === undefined) return { listed: false };
  return {
    listed: true,
    isCompacting: "isCompacting" in live ? live.isCompacting : undefined,
    compactionSpeculation: "compactionSpeculation" in live ? live.compactionSpeculation : undefined,
  };
}

export default function recorder(pi: RecorderApi): void {
  const file = process.env.LEGION_CAPTURE_RECORD;
  if (file === undefined || file === "") return;
  const placementFile = process.env.LEGION_CAPTURE_PLACEMENT;
  /** The stand-in approval message this session placed, once it has placed one. */
  let placed: Record<string, unknown> | undefined;

  pi.on("session_start", async (_event, context) => {
    record(file, "session_start", {
      agent: context.agent,
      session: context.sessionManager.getSessionId(),
      branchEntries: context.sessionManager.getBranch().length,
    });
  });

  pi.on("context", async (event, context) => {
    const session = context.sessionManager.getSessionId();
    const messages = Array.isArray(event.messages) ? (event.messages as unknown[]) : [];
    const live = liveState(session);
    const returned = standInPlacement(messages, context, live);
    record(file, "context", {
      agent: context.agent,
      session,
      live,
      messages,
      ...(returned === undefined ? {} : { returned }),
      branch: context.sessionManager.getBranch(),
    });
    return returned === undefined ? undefined : { messages: returned };
  });

  pi.on("before_provider_request", async (event, context) => {
    const payload = event.payload;
    const wire =
      typeof payload === "object" && payload !== null && "messages" in payload
        ? payload.messages
        : undefined;
    record(file, "before_provider_request", {
      agent: context.agent,
      session: context.sessionManager.getSessionId(),
      model: {
        provider: context.model?.provider,
        id: context.model?.id,
        input: context.model?.input,
        supportsMidConversationSystem: context.model?.compat?.supportsMidConversationSystem,
      },
      perCall: Array.isArray(wire) ? wire.map(perCall) : undefined,
      payload,
    });
    return undefined;
  });

  pi.on("message_end", async (event, context) => {
    const { message } = event;
    record(file, "message_end", {
      agent: context.agent,
      session: context.sessionManager.getSessionId(),
      key: messageKey(message),
      message,
    });
  });

  pi.on("session_compact", async (event, context) => {
    record(file, "session_compact", {
      agent: context.agent,
      session: context.sessionManager.getSessionId(),
      compactionEntry: event.compactionEntry,
      fromExtension: event.fromExtension,
    });
  });

  /**
   * The request's messages with the stand-in approval message placed, or undefined when this
   * request gets none: no placement file, a subagent, a side turn, the handoff prompt, a
   * compaction's own request, or a later request that holds no answer to the placed message.
   */
  function standInPlacement(
    messages: readonly unknown[],
    context: RecorderContext,
    live: Record<string, unknown>
  ): unknown[] | undefined {
    if (placementFile === undefined || !existsSync(placementFile)) return undefined;
    const agent = context.agent;
    if (typeof agent === "object" && agent !== null && "kind" in agent && agent.kind !== "main") {
      return undefined;
    }
    if (isSideTurn(messages) || isHandoffPrompt(messages)) return undefined;
    if (live.isCompacting === true || live.compactionSpeculation === "running") return undefined;
    if (placed === undefined) {
      placed = {
        role: "custom",
        customType: APPROVAL_MESSAGE_TYPE,
        content: [{ type: "text", text: readFileSync(placementFile, "utf8") }],
        display: false,
        attribution: "agent",
        timestamp: Date.now(),
      };
      return [...messages, placed];
    }
    const at = timestampOf(placed) ?? 0;
    const answer = messages.findIndex(
      (message) => roleOf(message) === "assistant" && (timestampOf(message) ?? -1) >= at
    );
    if (answer === -1) return undefined;
    return [...messages.slice(0, answer), placed, ...messages.slice(answer)];
  }
}
