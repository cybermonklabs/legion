import type { WhoamiResponse } from "@legion/contracts";
import type { AsOfRead } from "../dispatch-http";
import { ToolInputError } from "../tool-input-errors";
import { type DispatchToolResult, optionalString, type SessionContext } from "./context";

/**
 * The parts a read command does not read at a cut, in the words its `Left out at` line names
 * them. Each is read from a route that holds no history (the plan's kind "no history",
 * dispatch://INFRA-4620), which answers only the present and refuses `as_of`.
 */
export const LEFT_OUT = {
  /** `GET /api/v1/references`: the node line, `Referenced by:` and `Links:`. */
  graph: "the reference graph now",
  /** `GET /api/v1/issues/{key}/references`: an issue read's `References:`. */
  issueReferences: "the issue's references now",
  /** `GET /api/v1/issues/{key}/coverage`: an issue read's `Coverage:`. */
  coverage: "the spec's coverage now",
  /** `GET /api/v1/agents`: whether the session that holds a claim or a route runs. */
  liveness: "whether a session runs now",
  /** `GET /api/v1/me/replies`: the Inbox's `Unread threads`. */
  unreadThreads: "unread threads",
  /** `GET /api/v1/credential-requests`: the Inbox's `Credential requests`. */
  credentialRequests: "credential requests",
  /** A document block's last change, which only the reference graph records. */
  blockChanged: "the version in which the block last changed",
  /** A pinned block's state against its pin, which only the reference graph records. */
  blockPin: "the block at its pinned version",
  /** A comment's or message's blocks, which `GET /api/v1/comments/{id}/blocks` holds now only. */
  postBlock: "where the block starts and ends in its post",
  /** The Inbox's `Later` band: the asks the person snoozed, which no event records. */
  snoozes: "snoozes",
} as const;

/** A part a read command left out at a cut: one of `LEFT_OUT`'s words. */
export type LeftOutPart = (typeof LEFT_OUT)[keyof typeof LEFT_OUT];

/** What a read prints in place of a part or a field it left out at `asOf`, the cut. */
export function leftOutAt(asOf: string): string {
  return `left out at ${asOf}`;
}

/**
 * What a read prints in place of a member Dispatch left out of an answer at a cut
 * (`Dispatch-As-Of-Absent`): `left out at <cut>` where the read knows the cut, from `--as-of`, a
 * token's cut, or the `Dispatch-As-Of` its answer named (`AnswerCut`), and `left out by Dispatch`
 * where it does not. A member every answer of the present carries is missing only from an answer
 * at a cut, so a renderer prints these words for it rather than throw, whether or not it knows the
 * cut. A member whose absence means something at the present (no person an ask waits on, no
 * token) keeps that meaning on a read that knows of no cut.
 */
export function leftOutWords(asOf: string | undefined): string {
  return asOf === undefined ? "left out by Dispatch" : leftOutAt(asOf);
}

/** `value`, or the words for it left out (`leftOutWords`) where the answer does not carry it: for
 *  a member every answer of the present carries. */
export function orLeftOut<T>(value: T | undefined, asOf: string | undefined): T | string {
  return value === undefined ? leftOutWords(asOf) : value;
}

/** The time a read counts its ages and windows from: the present on this machine's clock
 *  (`now`, in epoch milliseconds), or the cut it read Dispatch at, as the read names it (`asOf`)
 *  and as the instant its answers named in `Dispatch-As-Of` (`at`, in epoch milliseconds). */
export type ReadTime = { readonly now: number } | { readonly asOf: string; readonly at: number };

/** The time a read at `cut` counts from: the cut, or the present where it names none. At a cut
 *  it is read after an answer at the cut, which named the cut (`ReadCut.instant`). */
export function readTime(cut: ReadCut | undefined): ReadTime {
  return cut === undefined ? { now: Date.now() } : { asOf: cut.asOf, at: cut.instant() };
}

/** Why `dispatch results` refuses `--at` with `--as-of`: `--at` compares each check with one
 *  commit through GitHub now, and a read at a cut reads nothing from GitHub. */
export const RESULTS_AT_AS_OF_PROBLEM =
  "--at compares each check with one commit, and --as-of reads Dispatch as it stood at a past time, which reads nothing from GitHub: name one of them, not both";

/** Why `dispatch results` refuses `--at` under a token that carries a cut, which replaces any
 *  `--as-of` (`readAtCut`). */
export function resultsAtACutProblem(cut: string): string {
  return `this token reads Dispatch as of ${cut}, which reads nothing from GitHub, and --at compares each check with one commit: drop --at`;
}

/**
 * The words for a field an answer named in `Dispatch-As-Of-Absent`, by the end of its JSON
 * Pointer: a key starting with `/` is the whole pointer, any other the pointer's last segments
 * (`/open_asks/*\/referenced_by_count` ends in `referenced_by_count`). The longest match wins, and
 * a pointer that matches none is named as the pointer itself. The fields are each pointer that
 * `AbsentFields` in `packages/envoy/internal/dispatch/asof/absent.go` names, which is the only
 * table a route names absent fields from.
 */
export const ABSENT_FIELD_WORDS: Readonly<Record<string, string>> = {
  route_status: "the route's state now",
  route_holder: "the route's holder now",
  route_status_reason: "why the route stands as it does now",
  holder_state: "whether the claim's holder runs now",
  last_seen: "when the claim's holder was last seen",
  takeable_at: "when another session may take the claim",
  design_gates: "the design gates",
  referenced_by_count: "how many nodes reference each item now",
  "components/ids": "the component ids",
  "components/unknown": "the unknown components",
  revision: "the revision of each ask's parts",
  "options/*/id": "the ids of ask options",
  "answer/id": "the ids of ask answers",
  waiting_on_person: "the person an ask waits on by its project's default",
  "approval/documents": "the documents of each approval request",
  question: "the question of each ask whose wording a restore changed",
  question_tree: "the question tree of each ask whose wording a restore changed",
  options: "the options of each ask whose wording a restore changed",
  multiple: "whether each ask whose wording a restore changed takes several options",
  urgency: "the urgency of each ask whose wording a restore changed",
  body: "the bodies of posts deleted or edited since",
  "reply_block/text": "the text of the block a reply answers",
  "reply_block/removed": "whether the block a reply answers was removed",
  anchor_block: "where each anchored block stands in its document",
  "anchor/orphaned": "whether each anchor's mark was gone",
  "deliveries/*/state": "the state of each delivery",
  "deliveries/*/reply_id": "the reply of each delivery",
  "deliveries/*/envelope_id": "the envelope of each delivery",
  "deliveries/*/resolve_error": "the error of each delivery",
  "deliveries/*/created_at": "the time of each delivery",
  "deliveries/*/requested_by": "who asked for each delivery",
  "deliveries/*/delivery": "the delivery mode of each delivery",
  "deliveries/*/session_id": "the session of each delivery",
  snoozed_until: "snoozes",
  "/token": "the document token",
  token: "the block tokens",
  embedding: "the block embeddings",
  references: "the references of each block",
  id: "the ids of blocks that no index holds",
  "table/rows/*/id": "the ids of table rows",
  "table/rows/*/cells/*/id": "the ids of table cells",
  compared_with: "what each check is compared with on GitHub now",
  compared_nothing: "why a check is compared with nothing",
  pass_stands: "whether a pass stands now",
  ci: "the CI result of each check",
  commit_stands: "where the commit of each result stands now",
  currency: "the GitHub currency of each result",
  not_counted: "why a result counts for nothing against the program's deploy now",
  "checks/*/version": "the document version of each check",
  sha256: "the digest of the words of each check",
  check_words_moved: "whether the words of a check moved after each result",
  "query/broadened": "the broadened query",
  href: "the link of each ask hit whose document changed since",
  snippet: "the snippet of each ask hit whose wording a restore changed",
  snippet_tree: "the snippet tree of each ask hit whose wording a restore changed",
};

/** A field an answer named absent, in words (`ABSENT_FIELD_WORDS`). */
export function absentFieldWords(pointer: string): string {
  const whole = ABSENT_FIELD_WORDS[pointer];
  if (whole !== undefined) return whole;
  let words: string | undefined;
  let longest = 0;
  for (const [end, named] of Object.entries(ABSENT_FIELD_WORDS)) {
    if (!end.startsWith("/") && pointer.endsWith(`/${end}`) && end.length > longest) {
      words = named;
      longest = end.length;
    }
  }
  return words ?? pointer;
}

/**
 * The cut one run of a read command reads Dispatch at, and what it left out there. It is the
 * `AsOfRead` the command hands each read of a route that answers `as_of`, so each such read sends
 * the cut, tells it the cut its answer names, and reports the fields its answer names absent.
 * `leave` records a part the command did not read, a route that holds no history, and answers the
 * text the read prints in its place.
 */
export class ReadCut implements AsOfRead {
  readonly #parts = new Set<string>();
  readonly #absent = new Set<string>();
  #answeredAt: string | undefined;

  constructor(readonly asOf: string) {}

  answeredAt(cut: string): void {
    this.#answeredAt = cut;
  }

  absent(pointers: readonly string[]): void {
    for (const pointer of pointers) this.#absent.add(pointer);
  }

  /** The cut's instant in epoch milliseconds, read from the cut an answer named in
   *  `Dispatch-As-Of` and never from `asOf`: Dispatch reads RFC 3339 spellings that `Date.parse`
   *  does not, such as a comma before the fraction of a second, and names the cut in one form. */
  instant(): number {
    const answered = this.#answeredAt;
    if (answered === undefined) {
      throw new Error(`no answer read at ${this.asOf} has named its cut yet`);
    }
    return Date.parse(answered);
  }

  /** Records `part` as left out, and answers the row a section prints for it. */
  leave(part: LeftOutPart): string {
    this.#parts.add(part);
    return leftOutAt(this.asOf);
  }

  /** `Left out at <cut>: <part>, …`: each part left out, then each absent field in words, each
   *  once, or `nothing`. A read at a cut always prints it, so its output names the cut it was
   *  read at, a token's cut that replaced `--as-of` included. */
  line(): string {
    const named = new Set([...this.#parts, ...[...this.#absent].map(absentFieldWords)]);
    return `Left out at ${this.asOf}: ${named.size === 0 ? "nothing" : [...named].join(", ")}`;
  }
}

/** What a read command knows before its first read: whoami's answer, and the cut it reads at. */
export interface Reading {
  readonly identity: WhoamiResponse;
  /** The token's cut where whoami names one, else `--as-of`; undefined for a read of the
   *  present. */
  readonly cut: ReadCut | undefined;
}

/**
 * Reads `GET /api/v1/whoami` before any other request of a read command. A token that carries a
 * cut reads at that cut, whatever `--as-of` the call names (decision 6 of dispatch://INFRA-4620),
 * so its cut replaces the call's own. A Dispatch older than the cut names none. A token's cut
 * refuses `dispatch results --at` here, before any other request, as `check.ts` refuses `--at`
 * beside `--as-of` before any request at all.
 */
export async function readAtCut(session: SessionContext): Promise<Reading> {
  const identity = await session.newClient().whoami();
  const tokenCut = identity.kind === "agent" ? (identity.cut ?? null) : null;
  const { tool } = session.input;
  if (
    tokenCut !== null &&
    tool === "dispatch_results" &&
    optionalString(session.args, "at") !== undefined
  ) {
    throw new ToolInputError(tool, [resultsAtACutProblem(tokenCut)]);
  }
  const asOf = tokenCut ?? optionalString(session.args, "as_of");
  return { identity, cut: asOf === undefined ? undefined : new ReadCut(asOf) };
}

/** `result` with the cut's `Left out at` line before its text, at a cut. */
export function withLeftOut(
  result: DispatchToolResult,
  cut: ReadCut | undefined
): DispatchToolResult {
  return cut === undefined ? result : { ...result, text: `${cut.line()}\n${result.text}` };
}
