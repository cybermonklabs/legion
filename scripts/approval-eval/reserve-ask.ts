import { createHash } from "node:crypto";
import { parseArgs } from "node:util";
import { CannotRunError, recordOf, stringOf, UsageError } from "./corpus-io";
import { type DispatchConfig, dispatchConfig, readJson, readText } from "./dispatch-read";
import {
  RESERVE_DOCUMENT,
  RESERVE_LIST_FILE,
  RESERVE_LIST_SHA256,
  RESERVE_RULE_VERSION,
  reserveRuleOf,
  ruleVersionsOf,
} from "./reserve";

/**
 * Decides one ask now under the reserve of INFRA-2897's next final test, for a scorer that meets
 * the ask live: the shadow run, the tester, the first reader. Each of them calls this command
 * rather than keep its own copy of the rule, so the rule a live scorer applies is the rule every
 * build of this directory applies (`reserveRuleOf` in `reserve.ts`).
 *
 * A build reads an ask under two roots, its tree's root at the ask's first opening and at the
 * build's event. A live scorer has only the root it walks to now, which is the root at the
 * opening while no parent has changed since. A tree that moves after a scorer read it is one the
 * claim's draw checks against the scorers' own records (the registration's item 6 (c)), so the
 * scorer records each ask it read.
 *
 * The command reads, in order: the registration document's `## Version N` headings, and refuses
 * while one names a version above `RESERVE_RULE_VERSION`; the ask, for its issue and its first
 * opening; each issue's parent up to the root; and version 3's list, refused unless its sha256 is
 * the registered one. It prints one JSON line, which holds no text of the ask and no answer, and
 * exits 0 when the scorer may read the ask, 1 when the reserve holds it, and 2 when it decided
 * nothing, which the scorer takes as held. The reserve holds asks that ask the registration's
 * person or that the person answers; the caller asks only of those.
 */

const USAGE = `Usage: bun scripts/approval-eval/reserve-ask.ts --ask <id | dispatch://KEY/ask/<id>>

Decides one ask now under the reserve of INFRA-2897's next final test, by the rule every command
here reads (reserve.ts), and prints one JSON line. Exit 0: the scorer may read it. Exit 1: the
reserve holds it. Exit 2: nothing was decided, which the scorer takes as held.
Reads DISPATCH_URL and DISPATCH_TOKEN_FILE.`;

/** How many parents a walk follows before it takes the chain for a cycle (`treeRootOf`'s bound). */
const TREE_DEPTH = 64;

/** The answer to one ask: what the scorer does, and the facts the rule read. */
export interface AskDecision {
  ask: string;
  issue: string | null;
  opened_at: string;
  root: string | null;
  listed: boolean | null;
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
    if (issue === null || !Object.hasOwn(issue, "parent")) {
      throw new UndecidedError(`Dispatch answered ${at} with no parent field`);
    }
    const parent = stringOf(issue.parent);
    if (parent === null || parent === "") return at;
    at = parent;
  }
  throw new UndecidedError(`the parents of ${issueKey} run past ${TREE_DEPTH} issues`);
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
  if (ask === null || openedAt === null || Number.isNaN(askTime)) {
    throw new UndecidedError(`Dispatch answered ask ${askId} with no first opening`);
  }
  const issue = stringOf(ask.issue_key);
  const root = issue === null ? null : await rootNowOf(issue, config);
  const list = listOf(
    await readText(
      `/api/v1/projects/${RESERVE_LIST_FILE.project}/artifacts/${RESERVE_LIST_FILE.slug}` +
        `/versions/${RESERVE_LIST_FILE.version}`,
      config
    ),
    listSha256
  );
  const listed = root === null ? null : list.has(root);
  const decision = reserveRuleOf({
    askTime,
    onIssue: issue !== null,
    roots: [root],
    openingRootListed: listed === true,
    everyRootListed: listed === true,
  });
  return {
    ask: askId,
    issue,
    opened_at: openedAt,
    root,
    listed,
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
