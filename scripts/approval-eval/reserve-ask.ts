import { createHash } from "node:crypto";
import { parseArgs } from "node:util";
import { CannotRunError, recordOf, stringOf, UsageError } from "./corpus-io";
import {
  type DispatchConfig,
  type DispatchEvent,
  dispatchConfig,
  readJson,
  readText,
} from "./dispatch-read";
import {
  type IssueParents,
  noteIssueParent,
  RESERVE_DOCUMENT,
  RESERVE_LIST_FILE,
  RESERVE_LIST_SHA256,
  RESERVE_RULE_VERSION,
  reserveRuleOf,
  ruleVersionsOf,
  treeRootOf,
} from "./reserve";

/**
 * Decides one ask now under the reserve of INFRA-2897's next final test, for a scorer that meets
 * the ask live: the shadow run, the tester, the first reader. Each of them calls this command
 * rather than keep its own copy of the rule, so the rule a live scorer applies is the rule every
 * build of this directory applies (`reserveRuleOf` in `reserve.ts`).
 *
 * A build reads an ask under two roots: its tree's root at the ask's first opening, from the
 * parents the log recorded before the ask's `ask.opened` event, and the root at the build's
 * event. This command reads the same two: the root at the opening from each walked issue's own
 * log (`GET /api/v1/issues/{key}/events`), by the same `noteIssueParent` and `treeRootOf` the
 * build uses, and the root now, which stands for the build's. A tree that moves after a scorer
 * read the ask is one the claim's draw checks against the scorers' own records (the
 * registration's item 6 (c)), so the scorer records each ask it read.
 *
 * The command reads, in order: the registration document's `## Version N` headings, and refuses
 * while one names a version above `RESERVE_RULE_VERSION`; the ask, for its issue, its first
 * opening and its opening event; the two roots; and version 3's list, refused unless its sha256
 * is the registered one. A field Dispatch leaves out or answers in another type decides nothing.
 * It prints one JSON line, which holds no text of the ask and no answer, and exits 0 when the
 * scorer may read the ask, 1 when the reserve holds it, and 2 when it decided nothing, which the
 * scorer takes as held. The reserve holds asks that ask the registration's person or that the
 * person answers; the caller asks only of those.
 */

const USAGE = `Usage: bun scripts/approval-eval/reserve-ask.ts --ask <id | dispatch://KEY/ask/<id>>

Decides one ask now under the reserve of INFRA-2897's next final test, by the rule every command
here reads (reserve.ts), and prints one JSON line. Exit 0: the scorer may read it. Exit 1: the
reserve holds it. Exit 2: nothing was decided, which the scorer takes as held.
Reads DISPATCH_URL and DISPATCH_TOKEN_FILE.`;

/** How many parents a walk follows before it takes the chain for a cycle (`treeRootOf`'s bound). */
const TREE_DEPTH = 64;

/** The page an issue's own log is read in: that route's own maximum. */
const ISSUE_LOG_PAGE = 200;

/** The answer to one ask: what the scorer does, and the facts the rule read. */
export interface AskDecision {
  ask: string;
  issue: string | null;
  opened_at: string;
  /** The tree's root at the ask's first opening; null where the log names none. */
  opening_root: string | null;
  /** The tree's root now. */
  root_now: string | null;
  opening_listed: boolean | null;
  now_listed: boolean | null;
  rule_version: number;
  decision: "held" | "kept";
}

/** The command decided nothing: the scorer holds the ask, and the message says why. */
export class UndecidedError extends CannotRunError {
  override readonly name = "UndecidedError";
}

/** The ask's id, from a bare id or from a `dispatch://KEY/ask/<id>` reference. */
export function askIdOf(value: string): string | null {
  const trimmed = value.trim();
  const reference = /^dispatch:\/\/[A-Z][A-Z0-9]*-\d+\/ask\/([0-9a-f-]{36})$/.exec(trimmed);
  if (reference?.[1] !== undefined) return reference[1];
  return /^[0-9a-f-]{36}$/.test(trimmed) ? trimmed : null;
}

/** Refuses a registration whose headings name no version, or one this code does not implement. */
export function checkRuleVersion(markdown: string): void {
  const versions = ruleVersionsOf(markdown);
  if (versions.length === 0) {
    throw new UndecidedError(
      `the registration document ${RESERVE_DOCUMENT.slug} names no "## Version N" heading`
    );
  }
  const newest = Math.max(...versions);
  if (newest > RESERVE_RULE_VERSION) {
    throw new UndecidedError(
      `the registration names rule version ${newest}, and this code implements version ` +
        `${RESERVE_RULE_VERSION} at most: run a build that implements version ${newest}`
    );
  }
}

/**
 * Version 3's list, from the registered file's bytes, refused unless their sha256 is `expected`:
 * the registered digest, which only a test replaces.
 */
export function listOf(text: string, expected: string = RESERVE_LIST_SHA256): Set<string> {
  const digest = createHash("sha256").update(text, "utf8").digest("hex");
  if (digest !== expected) {
    throw new UndecidedError(
      `version 3's list read with sha256 ${digest}, and the registration names ${expected}`
    );
  }
  return new Set(text.split("\n").filter((line) => line !== ""));
}

/** A field that must be a string or null: anything else decides nothing. */
function keyField(record: Record<string, unknown>, field: string, owner: string): string | null {
  const value = record[field];
  if (!Object.hasOwn(record, field) || (value !== null && typeof value !== "string")) {
    throw new UndecidedError(`Dispatch answered ${owner} with no ${field} that is a key or null`);
  }
  return value === null || value === "" ? null : value;
}

/** The root of an issue's tree now: each parent Dispatch records, followed until one has none. */
async function rootNowOf(issueKey: string, config: DispatchConfig): Promise<string> {
  const passed = new Set<string>();
  let at = issueKey;
  while (passed.size <= TREE_DEPTH) {
    if (passed.has(at)) throw new UndecidedError(`the parents of ${issueKey} come back to ${at}`);
    passed.add(at);
    const issue = recordOf(
      await readJson<unknown>(`/api/v1/issues/${encodeURIComponent(at)}`, config)
    );
    if (issue === null) throw new UndecidedError(`Dispatch answered ${at} with no issue`);
    const parent = keyField(issue, "parent", at);
    if (parent === null) return at;
    at = parent;
  }
  throw new UndecidedError(`the parents of ${issueKey} run past ${TREE_DEPTH} issues`);
}

/** The rows of one issue's own log written before the event `before`, oldest first. */
async function issueLogBefore(
  issueKey: string,
  before: number,
  config: DispatchConfig
): Promise<DispatchEvent[]> {
  const rows: DispatchEvent[] = [];
  for (let after = 0; ; ) {
    const page = await readJson<unknown>(
      `/api/v1/issues/${encodeURIComponent(issueKey)}/events?after=${after}&limit=${ISSUE_LOG_PAGE}`,
      config
    );
    if (!Array.isArray(page)) throw new UndecidedError(`the log of ${issueKey} is not a list`);
    for (const value of page) {
      const row = recordOf(value);
      const payload = recordOf(row?.payload);
      if (row === null || payload === null || typeof row.id !== "number") {
        throw new UndecidedError(`the log of ${issueKey} holds a row with no id or payload`);
      }
      if (typeof row.seq !== "number" || typeof row.type !== "string") {
        throw new UndecidedError(`the log of ${issueKey} holds a row with no seq or type`);
      }
      if (row.id >= before) return rows;
      rows.push({
        id: row.id,
        issue_key: stringOf(row.issue_key),
        project: stringOf(row.project),
        seq: row.seq,
        type: row.type,
        actor: null,
        created_at: stringOf(row.created_at) ?? "",
        payload,
      });
      after = row.seq;
    }
    if (page.length < ISSUE_LOG_PAGE) return rows;
  }
}

/**
 * The root of an issue's tree at the ask's opening, as a build's walk reads it: each issue's
 * parent from the newest `issue.` row of its own log before the ask's `ask.opened` event
 * (`noteIssueParent`), followed by `treeRootOf`, which names no root where the chain reaches an
 * issue the log had not recorded by then, comes back to an issue, or runs past `TREE_DEPTH`.
 */
async function openingRootOf(
  issueKey: string,
  openedEvent: number,
  config: DispatchConfig
): Promise<string | null> {
  const parents: IssueParents = new Map();
  let at: string | null = issueKey;
  for (let depth = 0; at !== null && !parents.has(at) && depth <= TREE_DEPTH; depth += 1) {
    for (const row of await issueLogBefore(at, openedEvent, config)) noteIssueParent(parents, row);
    at = parents.get(at) ?? null;
  }
  return treeRootOf(issueKey, parents);
}

/**
 * Decides one ask by the registered rule, reading Dispatch as the module's comment says.
 * `listSha256` is the registered digest of version 3's list, which only a test replaces.
 */
export async function decideAsk(
  askId: string,
  config: DispatchConfig,
  listSha256: string = RESERVE_LIST_SHA256
): Promise<AskDecision> {
  const document = recordOf(
    await readJson<unknown>(
      `/api/v1/projects/${RESERVE_DOCUMENT.project}/artifacts/${RESERVE_DOCUMENT.slug}/text`,
      config
    )
  );
  const markdown = stringOf(document?.markdown);
  if (markdown === null)
    throw new UndecidedError("the registration document came back with no text");
  checkRuleVersion(markdown);

  const read = recordOf(
    await readJson<unknown>(`/api/v1/asks/${encodeURIComponent(askId)}`, config)
  );
  const ask = recordOf(read?.ask);
  const openedAt = stringOf(ask?.created_at);
  const askTime = Date.parse(openedAt ?? "");
  const openedEvent = ask?.opened_event_id;
  if (ask === null || openedAt === null || Number.isNaN(askTime)) {
    throw new UndecidedError(`Dispatch answered ask ${askId} with no first opening`);
  }
  if (typeof openedEvent !== "number" || !Number.isSafeInteger(openedEvent)) {
    throw new UndecidedError(`Dispatch answered ask ${askId} with no opening event`);
  }
  const issue = keyField(ask, "issue_key", `ask ${askId}`);
  const rootNow = issue === null ? null : await rootNowOf(issue, config);
  const openingRoot = issue === null ? null : await openingRootOf(issue, openedEvent, config);
  const list = listOf(
    await readText(
      `/api/v1/projects/${RESERVE_LIST_FILE.project}/artifacts/${RESERVE_LIST_FILE.slug}` +
        `/versions/${RESERVE_LIST_FILE.version}`,
      config
    ),
    listSha256
  );
  const openingListed = issue === null ? null : openingRoot !== null && list.has(openingRoot);
  const nowListed = rootNow === null ? null : list.has(rootNow);
  const decision = reserveRuleOf({
    askTime,
    onIssue: issue !== null,
    roots: [openingRoot, rootNow],
    openingRootListed: openingListed === true,
    everyRootListed: openingListed === true && nowListed === true,
  });
  return {
    ask: askId,
    issue,
    opened_at: openedAt,
    opening_root: openingRoot,
    root_now: rootNow,
    opening_listed: openingListed,
    now_listed: nowListed,
    rule_version: RESERVE_RULE_VERSION,
    decision,
  };
}

function readAsk(argv: readonly string[]): string {
  let values: { ask?: string };
  try {
    ({ values } = parseArgs({
      args: [...argv],
      options: { ask: { type: "string" } },
      strict: true,
      allowPositionals: false,
    }));
  } catch {
    // parseArgs names what it refused, so its message is never repeated.
    throw new UsageError(USAGE);
  }
  const askId = askIdOf(values.ask ?? "");
  if (askId === null) throw new UsageError(USAGE);
  return askId;
}

export async function main(argv: readonly string[]): Promise<number> {
  try {
    const decision = await decideAsk(readAsk(argv), await dispatchConfig());
    console.log(JSON.stringify(decision));
    return decision.decision === "kept" ? 0 : 1;
  } catch (error) {
    if (error instanceof CannotRunError) {
      console.error(`${error.name}: ${error.message}`);
      return 2;
    }
    throw error;
  }
}

// Only when run: a test that imports `decideAsk` must not decide an ask.
if (import.meta.main) process.exit(await main(Bun.argv.slice(2)));
