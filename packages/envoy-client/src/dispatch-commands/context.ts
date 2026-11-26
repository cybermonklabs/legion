import type {
  Actor,
  Artifact,
  ArtifactUploadResponse,
  CreateArtifactInput,
  IssueComponentsInput,
  IssueComponentsMode,
  IssueDetails,
  IssuePriority,
  Sleep,
} from "@legion/contracts";
import { goTrimSpace } from "@legion/contracts/go-blank";
import { urlForText } from "@legion/contracts/log-text";
import type { ActiveDispatchConfig } from "../dispatch-config";
import type { DispatchHost, ExecFn } from "../dispatch-cwd";
import {
  DISPATCH_WRITE_RETRY,
  DispatchClient,
  type DispatchClientOptions,
  type DispatchWriteRetry,
} from "../dispatch-http";
import type { Owner, ParsedDispatchRef } from "../dispatch-owner";
import type { AgentArtifactRef, LocalPicture } from "../dispatch-picture-tools";
import type { ToolImage } from "../dispatch-pictures";
import { linkBaseOf } from "./links";

/**
 * Tool arguments as the model supplied them. The tool's Zod schema validates
 * them before use; the named keys are the ones the executor inspects itself,
 * everything else is forwarded to the API unchanged.
 */
export type ToolArguments = {
  readonly urgency?: unknown;
  readonly ref?: unknown;
  readonly issue?: unknown;
  readonly project?: unknown;
  readonly artifact?: unknown;
  readonly version?: unknown;
  readonly anchor?: unknown;
  readonly options?: unknown;
  readonly labels?: unknown;
  readonly blocked_by?: unknown;
  readonly external_links?: unknown;
  readonly ops?: unknown;
  readonly in_reply_to?: unknown;
  readonly message?: unknown;
  readonly images?: unknown;
} & Record<string, unknown>;

export type ExecutorEnvironment = {
  readonly LEGION_ISSUE?: string;
} & Record<string, string | undefined>;

export interface ExecuteDispatchToolInput {
  readonly tool: string;
  readonly args: ToolArguments;
  readonly cwd: string;
  /**
   * The bytes of an upload's `--path` file the CLI already read, sent in place of a read: those
   * AskGate's check digested where it read them, so the upload sends exactly those bytes, and
   * otherwise those of a Markdown file as the CLI read and checked them once the flags were read
   * (`documentFileRead`), so the call reads the file no second time.
   */
  readonly uploadBytes?: Uint8Array;
  readonly host: DispatchHost;
  readonly sessionId?: string;
  readonly sessionTitle?: string;
  /** The resolved Dispatch (`activeDispatchSettings`): the server, the credential every request
   *  presents, and the public dashboard its printed links point at when there is one. */
  readonly config: Pick<ActiveDispatchConfig, "url" | "credential" | "publicUrl">;
  /** Host cancellation for the currently executing tool call. */
  readonly signal?: AbortSignal;
  readonly env?: ExecutorEnvironment;
  readonly fetchImpl?: typeof fetch;
  /** How each write asks Dispatch again under its key (`DISPATCH_WRITE_RETRY` unless set). */
  readonly writeRetry?: DispatchWriteRetry;
  /** The wait before each new attempt of a request Dispatch answered with no route
   *  (`DispatchClient`); a test passes one that returns at once. */
  readonly sleep?: Sleep;
  /** The `User-Agent` the call's requests send (`DispatchClient`): the `dispatch` command names
   *  itself and the host plugin that ships it (`dispatchCommandUserAgent`). */
  readonly userAgent?: string;
  readonly exec?: ExecFn;
  /** The topics the session's Envoy subscriptions hold, or null when they cannot be read (Envoy
   *  off, or no answer from its listener); called only by a write that would name its owner as
   *  unsubscribed. A write names the owner the session is not subscribed to only from this. */
  readonly subscriptions?: () => Promise<readonly string[] | null>;
  /** The CLI's `--dry-run` of a call whose dry run Dispatch answers (`dryRunAsksDispatch`): the
   *  command asks Dispatch what its write would answer and writes nothing. No other call is run
   *  with it (`executeDispatchTool`). */
  readonly dryRun?: boolean;
}

export interface DispatchToolResult {
  readonly text: string;
  readonly details: Record<string, unknown>;
  /** A body the dispatch CLI writes to standard output as it came, byte for byte, in place of
   *  `text`, with no line of its own beside it there: `dispatch api`'s answer to a 2xx that is not
   *  Dispatch's JSON: stored content (an attachment) whatever its type, a body that is not JSON,
   *  or a bodiless 204's none. `text` then says what it is, and `details` records it without its
   *  bytes. */
  readonly body?: Uint8Array;
  /** Pictures the model is shown after the text, in the order the text names them. */
  readonly images?: readonly ToolImage[];
  /** What the caller should hear beside the result rather than in it: the dispatch CLI writes
   *  each line to stderr. The text already says what the reader needs. */
  readonly warnings?: readonly string[];
  /** The call was answered and did nothing: Dispatch refused the write and the text says so.
   *  A tool reads the text, so this rides beside it; the dispatch CLI exits 1 on it, which is
   *  what `skill://dispatch` promises a refusal is, so a script that reads the status is not
   *  told a create happened. */
  readonly refused?: boolean;
}

/**
 * What a command's dry-run step (`DispatchCommand.dryRun`, table.ts) is handed once the call's own
 * argument check has passed: what the step needs to ask Dispatch what the call asks before it
 * writes, under the dry run's deadline.
 */
export interface DryRunContext {
  /** The command's tool name, which its refusals name. */
  readonly tool: string;
  /** The arguments as the call's check parsed them. */
  readonly args: ToolArguments;
  /** The directory the call resolves a relative file path against, as the call does. */
  readonly cwd: string;
  /** The bytes of the upload's Markdown `--path` file as the CLI read and checked them once the
   *  flags were read (`documentFileRead`), which the step checks in place of a read. */
  readonly uploadBytes?: Uint8Array<ArrayBuffer>;
  /**
   * The client the call builds, from the session's Dispatch as the call resolves it. It stops at
   * `deadline`, and its own deadline is the same budget (`deadlineMs`), so a failure after the
   * budget is the deadline's and a credential is given that budget less the request's reserve. It
   * asks no request again that Dispatch answered with no route. It prints each warning of the
   * session's configuration on stderr, and throws, naming why, when the session has no Dispatch.
   * A step calls it only for arguments it asks Dispatch about, so a dry run of any other arguments
   * builds no client here and prints no configuration warning. The one other dry run that sends a
   * request is a `dispatch result` record's, which runs as the record does (`dryRunAsksDispatch`).
   */
  readonly client: () => DispatchClient;
  /** Aborts once `deadlineMs` have passed, so a failure after it is the deadline's. */
  readonly deadline: AbortSignal;
  readonly deadlineMs: number;
}

/** How long a dry run waits on Dispatch for its step's answer (`DispatchCommand.dryRun`): well
 *  inside the 20 s an AskGate older than DISPATCH_DRY_RUN_LOCAL gives a dry run, so a slow Dispatch
 *  passes that gate's dry run with a warning rather than failing it. AskGate now sets the
 *  variable, under which no step runs. The call of `dispatch artifact` asks its check of a
 *  Markdown file within the same bound, through the same client options as its dry run
 *  (`askOnceWithinDryRun`). */
export const DRY_RUN_DEADLINE_MS = 5_000;

/**
 * The options of a client that asks Dispatch once within the dry run's bound, `deadlineMs`
 * (DRY_RUN_DEADLINE_MS, or a shorter one a test sets): its own deadline is that bound, it asks no
 * request again that Dispatch answered with no route, and it sends no second attempt after a
 * failed one (no pauses), so no wait outlasts the bound. A fresh token is still presented once
 * where Dispatch says the token is no longer live. The dry run's client and the client of the
 * call's check of a Markdown file both take these, so the two ask alike. The pacing is
 * DISPATCH_WRITE_RETRY's, its pauses removed: the dry run has no session pacing of its own, and the
 * call's is DISPATCH_WRITE_RETRY wherever a test does not replace it.
 */
export function askOnceWithinDryRun(deadlineMs = DRY_RUN_DEADLINE_MS): DispatchClientOptions {
  return { deadlineMs, retryNoRoute: false, writeRetry: { ...DISPATCH_WRITE_RETRY, pausesMs: [] } };
}

/**
 * What a dry-run step found, which the dry run answers with the call's own output and exit code:
 * - `refused`: the error the call throws for these arguments, which the dry run prints as
 *   `messageFor` prints it, and exits 2 on where it is a `ToolInputError` and 1 on any other, as
 *   the call does;
 * - `passed`: a pass, with a warning for stderr when the step could not get its answer.
 */
export type DryRunVerdict =
  | { readonly kind: "refused"; readonly refusal: Error }
  | { readonly kind: "passed"; readonly warning?: string };

export interface ResolvedArtifact {
  readonly owner: Owner;
  readonly issue?: IssueDetails;
  readonly artifact: Artifact;
}
export interface OwnerResolution {
  readonly args: ToolArguments;
  readonly ref: ParsedDispatchRef | null;
  readonly owner: Owner | null;
  /** The conversation upload a `dispatch://agent/...` ref names; it has no issue or project. */
  readonly agentArtifact?: AgentArtifactRef;
  /** Set when the call needed an owner and the resolution pushed that it found none: no issue and
   *  no LEGION_ISSUE, a LEGION_ISSUE that is not an issue, or a bare number with no GitHub
   *  repository in cwd. Unset whenever the call names its owner or rightly has none. */
  readonly ownerMissing?: true;
}
export interface CheckedCall {
  /** Every problem the check found; the call is refused once, listing them all. */
  readonly problems: string[];
  readonly owner: OwnerResolution;
  /** The arguments as the schema parsed them: the owner-resolved ones when it refused them, or
   *  when it parsed them with the stand-in issue. */
  readonly args: ToolArguments;
  readonly pictures: LocalPicture[];
}

/** What the pre-send check reads: the call's arguments, its checkout and environment, and the
 *  Dispatch server's addresses, whose dashboard URLs a `ref` may be (none when the session has no
 *  Dispatch configured, which only a dry run checks with). */
export interface DispatchCallCheckInput
  extends Pick<ExecuteDispatchToolInput, "tool" | "args" | "cwd" | "env" | "exec"> {
  readonly serverUrls: readonly string[];
}
export function stringArg(args: Record<string, unknown>, name: string): string {
  const value = args[name];
  if (typeof value !== "string") throw new Error(`${name} is required`);
  return value;
}

export function numberArg(args: Record<string, unknown>, name: string): number {
  const value = args[name];
  if (typeof value !== "number") throw new Error(`${name} is required`);
  return value;
}

export function optionalString(args: Record<string, unknown>, name: string): string | undefined {
  const value = args[name];
  return typeof value === "string" ? value : undefined;
}

/** A field the server reads as a tri-state: absent leaves it, null clears it, a string sets it.
 *  `assignee` is one, and `--clear-assignee` is how the command sends its null. */
export function optionalNullableString(
  args: Record<string, unknown>,
  name: string
): string | null | undefined {
  const value = args[name];
  if (value === null) return null;
  return typeof value === "string" ? value : undefined;
}

export function optionalBoolean(args: Record<string, unknown>, name: string): boolean | undefined {
  const value = args[name];
  return typeof value === "boolean" ? value : undefined;
}

export function optionalNumber(args: Record<string, unknown>, name: string): number | undefined {
  const value = args[name];
  return typeof value === "number" ? value : undefined;
}

/** The optimistic-concurrency guard a call carries, under whichever shape its tool takes: an
 *  edit's `EditPrecondition`, an upload's `UploadPrecondition`. The tool's own schema already
 *  held the value to that shape, so this only reads it off `args`, which carries an index
 *  signature a package built with `noPropertyAccessFromIndexSignature` denies a dotted read of. */
export function optionalPrecondition<Guard>(args: Record<string, unknown>): Guard | undefined {
  const { precondition } = args;
  return precondition as Guard | undefined;
}

/** One field of a call's input, or no field at all when the argument was absent. An input whose
 *  optional properties are exact (`exactOptionalPropertyTypes`, which claude-envoy compiles this
 *  file under) takes an omitted key, never one whose value is `undefined`. */
export function defined<K extends string, V>(name: K, value: V | undefined): { [P in K]?: V } {
  return (value === undefined ? {} : { [name]: value }) as { [P in K]?: V };
}

/** A list of strings an argument carries, or nothing when it carries none. */
export function optionalStringArray(
  args: Record<string, unknown>,
  name: string
): string[] | undefined {
  const value = args[name];
  return Array.isArray(value) ? (value as string[]) : undefined;
}

/** `priority` as the server's tri-state: absent leaves it, null clears it, 0–3 set it. */
export function optionalPriority(
  args: Record<string, unknown>,
  name: string
): IssuePriority | null | undefined {
  const value = args[name];
  if (value === null) return null;
  // The zod spec already refused anything but an integer 0–3.
  return typeof value === "number" ? (value as IssuePriority) : undefined;
}

/** A `priority` filter list as the issue list takes it: null, meaning no priority, is `none`. */
export function optionalPriorityFilter(
  args: Record<string, unknown>,
  name: string
): (IssuePriority | "none")[] | undefined {
  const value = args[name];
  // The zod spec already refused anything but a list of integers 0–3 and null.
  return Array.isArray(value)
    ? (value as (IssuePriority | null)[]).map((item) => item ?? "none")
    : undefined;
}

/** The `components` argument as the server takes it; the zod spec already checked its shape. */
export function optionalComponents(
  args: Record<string, unknown>,
  name: string
): IssueComponentsInput | undefined {
  const value = args[name];
  if (typeof value !== "object" || value === null) return undefined;
  const input = value as { mode: IssueComponentsMode; ids?: string[]; reason?: string };
  return {
    mode: input.mode,
    ...(input.ids === undefined ? {} : { ids: input.ids }),
    ...(input.reason === undefined ? {} : { reason: input.reason }),
  };
}
/** `value` as a JSON object (not null, not an array), or null for anything else. */
export function asObject(value: unknown): Record<string, unknown> | null {
  return typeof value === "object" && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : null;
}

/**
 * What every spec command is handed: the call as `checkDispatchCall` left it, and the Dispatch
 * this session speaks to. A command that answers before the origin is resolved
 * (`dispatch_open_asks`, `dispatch_answers`, `dispatch_whoami`) takes this alone, so it starts no
 * subprocess.
 */
export interface SessionContext {
  readonly input: ExecuteDispatchToolInput;
  /** The arguments as the tool's schema parsed them. */
  readonly args: ToolArguments;
  /** The owner the check resolved, and the arguments its ref filled. */
  readonly ownerArguments: OwnerResolution;
  /** The pictures the call's `images` argument names, read from disk by the check. */
  readonly pictures: LocalPicture[];
  /** The API's own address, which an unreachable server is named by: the configured URL where it
   *  is a plain http(s) URL, and otherwise its fixed phrase (`urlForText`). */
  readonly configUrl: string;
  /**
   * The base of every link a command prints, to Dispatch's dashboard as people open it: the public
   * URL when the session has one (a pod reaches Dispatch at an in-cluster address nobody else can
   * open), and the API's URL otherwise; undefined where that URL is not a plain http(s) URL, and
   * then every link prints as its `dispatch://` reference (`linkBaseOf`, `linkFor`).
   */
  readonly linkBase: string | undefined;
  /** This host's own session id with Go's `strings.TrimSpace` cut from both ends (`goTrimSpace`):
   *  Dispatch trims it on the routes that read it so (open-asks, follow, read --message) and
   *  refuses an id holding whitespace on those that read it with `requestSessionID` (answers, a
   *  picture upload), so an id blank to Go is none. Every command that sends the id reads it here,
   *  in a path, a query or the actor every write carries (`toolActor`), and one that needs one
   *  refuses an empty one itself. */
  readonly hostSession: string | undefined;
  /**
   * A factory, not one instance: the constructor starts the request deadline, and the main path
   * resolves the origin (a subprocess) before it needs a client. `options` are laid over the
   * session's own (its signal, its write pacing and its sleep), for a request that runs under a
   * budget of its own: the call of `dispatch artifact` asks its check of a Markdown file once,
   * within DRY_RUN_DEADLINE_MS, as its dry run asks it (`askOnceWithinDryRun`).
   */
  newClient(options?: DispatchClientOptions): DispatchClient;
}

/**
 * What a command that acts for this session is handed: the session's own context, the client and
 * actor its writes carry, the owner its arguments name, and the pictures helpers a write posts
 * with. `commandContext` builds it once per call, after the origin is resolved.
 */
export interface CommandContext extends SessionContext {
  readonly client: DispatchClient;
  readonly actor: Actor;
  /** The call's owner, an issue reference resolved to the issue's key. */
  readonly owner: Owner | null;
  /** The owner, refused when the call names none. */
  documentOwner(): Owner;
  /** The document `reference` names under `owner`, read as the call's own reference reads it. */
  resolveDocument(owner: Owner, reference: string | undefined): Promise<ResolvedArtifact>;
  /** The issue the call acts on, refused when its owner is not an issue. */
  issue(): string;
  /**
   * The text a call posts with its pictures, each uploaded first to the owner the address names.
   * An upload is a step before the call's own write, so one that landed without its answer leaves
   * the call undone: it fails, rather than reading as the call's landing.
   */
  postWithPictures(
    text: string,
    owner: string,
    upload: (upload: CreateArtifactInput) => Promise<ArtifactUploadResponse>
  ): Promise<string>;
  /** An ask or comment uploads to the project of the project document it is on, else to its issue. */
  writePictures(resolved: ResolvedArtifact | undefined, text: string): Promise<string>;
  /**
   * The pictures this host session was already shown: `dispatch_read` names them instead of
   * sending them again, and it and `dispatch_doc_read` add the ones their result shows.
   */
  readonly shown: Set<string> | undefined;
}

/** One Dispatch command: the whole of what `dispatch <command>` does, after the check passed. */
export type DispatchCommand = (context: CommandContext) => Promise<DispatchToolResult>;

/**
 * The context every spec command starts from, built from the checked call. The client names the
 * configured server in a transport error (`DispatchClient`: its URL by `urlForText`, the failure
 * by `errorClass`), since a session whose Dispatch moved reads that message and nothing else; a
 * write the failure leaves unsettled names its request and quotes that message whole. Every link
 * is built on a clean base or printed as its reference (`linkBaseOf`). The client's requests go to
 * the URL as configured.
 */
export function sessionContext(
  input: ExecuteDispatchToolInput,
  checked: CheckedCall
): SessionContext {
  const { url, credential } = input.config;
  const configUrl = urlForText(url, "the configured Dispatch URL");
  const fetchImpl = input.fetchImpl ?? fetch;
  return {
    input,
    args: checked.args,
    ownerArguments: checked.owner,
    pictures: checked.pictures,
    configUrl,
    linkBase: linkBaseOf(input.config.publicUrl ?? url),
    hostSession: input.sessionId === undefined ? undefined : goTrimSpace(input.sessionId),
    newClient: (options = {}) =>
      new DispatchClient(url, credential, fetchImpl, {
        signal: input.signal,
        writeRetry: input.writeRetry,
        sleep: input.sleep,
        userAgent: input.userAgent,
        ...options,
      }),
  };
}
