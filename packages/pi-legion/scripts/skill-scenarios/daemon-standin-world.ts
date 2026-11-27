/**
 * The typed daemon parts of a case's world (heldout-case.ts World, `world.daemon`) as the daemon
 * stand-in holds them (daemon-standin.ts with `--world`): the role's claim, the daemon's record of
 * each issue the world names, the phase entry of the claim, admission and the claim tree's design
 * gate, and the rules the daemon at this tree judges an architect's request on an issue by. Each
 * rule names the handler or the record function of packages/daemon/internal it follows; where the
 * stand-in's answer and that code's differ, the code is right and this file is wrong.
 *
 * A fact no part holds is answered as the daemon answers a fact it holds no record of: an issue no
 * record names is 404 ISSUE_NOT_FOUND to an architect's request and 404 RECORD_NOT_FOUND to
 * `read_record`. Nothing here invents a record, a slot or a gate row the world does not state.
 */
import { DISPATCH_KEY_PATTERN, GATED_STEPS } from "@legion/contracts";
import { LEGION_CLAIM_STATES } from "@legion/contracts/legion-api";
import { STALE_ENTRY_INSTRUCTION } from "@legion/pi-shared/legion-phase-ops";
import type { World } from "./heldout-case";
import { parseInstant } from "./production-dispatch";

/** The role's claim as the daemon holds it: the issue its grant names, its tree, and the session
 * the world's records name as the role's own. */
export interface CaseClaim {
  readonly issue: string;
  readonly tree: string;
  readonly session?: string;
}

/** The daemon's record of one issue, in the fields a read or a rule here reads (record.Issue). */
export interface CaseRecord {
  readonly key: string;
  tree: string;
  parent: string | null;
  phase: string;
  status: string;
  generation: number;
  lingerUntil: string | null;
  architect: { readonly state: string } | null;
}

/** One step of the claim tree's design gate (record.DesignGate): the document's file name, its
 * newest version, the version a person approved, and the id the run's Dispatch gives it. */
export interface GateRow {
  readonly step: string;
  readonly document: string;
  current: number;
  readonly approved: number | null;
  artifactId: string;
}

/** A refusal in the daemon's `Failure` shape (`writeFailure`): its status, code and message. */
export interface Refusal {
  readonly status: number;
  readonly code: string;
  readonly message: string;
}

/** The claim of `world`: `daemon.claim`, else the case's issue in the tree of its Dispatch parent,
 * or its own, as the stand-in held every claim before the part. */
export function caseClaim(world: World): CaseClaim {
  const { key, parent } = world.dispatch.issue;
  return world.daemon?.claim ?? { issue: key, tree: parent ?? key };
}

/** The daemon's records of `world`, by key: the case's own issue (`daemon.issue` over the
 * default: the tree of its Dispatch parent, or its own, that parent, generation 1, no linger, in
 * `daemon.phase` and its Dispatch status, with `daemon.architect`), then each of `daemon.records`.
 * A world with no daemon has none. */
export function caseRecords(world: World): Map<string, CaseRecord> {
  const records = new Map<string, CaseRecord>();
  const daemon = world.daemon;
  if (daemon === null) return records;
  const { key, parent, status } = world.dispatch.issue;
  const own = daemon.issue;
  records.set(key, {
    key,
    tree: own?.tree ?? parent ?? key,
    parent: own === undefined ? parent : own.parent,
    phase: daemon.phase,
    status,
    generation: own?.generation ?? 1,
    lingerUntil: own?.lingerUntil ?? null,
    architect: daemon.architect ?? null,
  });
  for (const each of daemon.records ?? []) records.set(each.key, { ...each, lingerUntil: null });
  return records;
}

/** The claim tree's design gate of `world`, each step with the id `ids` gives its document
 * (`daemon.gate`, the run's Dispatch id of each step's document, by step). */
export function caseGate(world: World, ids: Readonly<Record<string, string>>): GateRow[] {
  return (world.daemon?.gate ?? []).map((row) => ({
    step: row.step,
    document: row.document,
    current: row.current,
    approved: row.approved,
    artifactId: ids[row.step] ?? "",
  }));
}

const CLAIM_STATES: readonly string[] = LEGION_CLAIM_STATES;
const STEPS: readonly string[] = GATED_STEPS;

/** Each typed daemon part of `world` the stand-in cannot serve, by its path: an architect claim in
 * a state the daemon's claim view names none of (LEGION_CLAIM_STATES), a gate step the daemon
 * gates none of (GATED_STEPS), a gate step whose document the run's Dispatch gives no id
 * (`ids`), and a phase entry of a claim on another issue than the case's, whose phase the world
 * does not state. A world the stand-in serves has none. */
export function worldFaults(world: World, ids: Readonly<Record<string, string>>): string[] {
  const daemon = world.daemon;
  if (daemon === null) return [];
  const faults: string[] = [];
  const state = (where: string, architect: { state: string } | null | undefined) => {
    if (architect && !CLAIM_STATES.includes(architect.state))
      faults.push(
        `${where}.state ${architect.state} is no state of a claim the daemon names (${CLAIM_STATES.join(", ")})`
      );
  };
  state("world.daemon.architect", daemon.architect);
  daemon.records?.forEach((each, index) => {
    state(`world.daemon.records[${index}].architect`, each.architect);
  });
  daemon.gate?.forEach((row, index) => {
    if (!STEPS.includes(row.step))
      faults.push(
        `world.daemon.gate[${index}].step ${row.step} is no step of the design gate the daemon names (${STEPS.join(", ")})`
      );
    else if (!ids[row.step])
      faults.push(
        `world.daemon.gate[${index}].document ${row.document}: the run's Dispatch gives no document of ${caseClaim(world).tree} by that name, so the gate names no document`
      );
  });
  const claim = caseClaim(world);
  if (daemon.entry !== undefined && claim.issue !== world.dispatch.issue.key)
    faults.push(
      `world.daemon.entry is the phase entry of the claim's issue ${claim.issue}, and the world states the phase of ${world.dispatch.issue.key} alone`
    );
  return faults;
}

/** Whether `key` is an issue key (claim.IsIssueKey). */
export function isIssueKey(key: string): boolean {
  return DISPATCH_KEY_PATTERN.test(key);
}

/** Whether an architect of `claim` is an architect of `issue`'s tree now (record.ArchitectOfTree):
 * the architect of the tree's root, or of a member the records hold in that tree. */
export function architectOfTree(
  claim: CaseClaim,
  issue: CaseRecord,
  records: ReadonlyMap<string, CaseRecord>
): boolean {
  if (claim.issue === issue.tree) return true;
  return [...records.values()].some((each) => each.tree === issue.tree && each.key === claim.issue);
}

/** Whether an architect of `claim` owns `issue` for a request only its own architect or one above
 * it makes (record.ArchitectAbove): the claim's issue is `issue` or above it through its recorded
 * parents in its tree, and a chain that leaves the tree ends at the root, whose architect owns it. */
export function architectAbove(
  claim: CaseClaim,
  issue: CaseRecord,
  records: ReadonlyMap<string, CaseRecord>
): boolean {
  const seen = new Set<string>();
  for (let current = issue; ; ) {
    if (current.key === claim.issue) return true;
    if (current.parent === null || current.key === current.tree || seen.has(current.key))
      return claim.issue === issue.tree;
    seen.add(current.key);
    const parent = records.get(current.parent);
    if (parent === undefined || parent.tree !== issue.tree) return claim.issue === issue.tree;
    current = parent;
  }
}

/** The records above `issue` through its recorded parents up to its tree's root, then that root
 * (record.Ancestors). */
function ancestors(issue: CaseRecord, records: ReadonlyMap<string, CaseRecord>): CaseRecord[] {
  if (issue.key === issue.tree) return [];
  const above: CaseRecord[] = [];
  const seen = new Set([issue.key]);
  for (let key = issue.parent; key !== null && !seen.has(key); ) {
    seen.add(key);
    const parent = records.get(key);
    if (parent === undefined) break;
    above.push(parent);
    if (parent.key === issue.tree) return above;
    key = parent.parent;
  }
  const root = records.get(issue.tree);
  if (root !== undefined && !seen.has(issue.tree)) above.push(root);
  return above;
}

/** Whether the architect of `claim` owns `target` for a move into or out of the workflow
 * (record.OwnsUnder, as api's childOfTree asks it): the tree root's architect owns every issue of
 * the tree, and a sub-architect the issues under its own issue alone. */
export function ownsUnder(
  claim: CaseClaim,
  target: CaseRecord,
  records: ReadonlyMap<string, CaseRecord>
): boolean {
  if (claim.issue === target.tree) return true;
  return (
    target.key !== claim.issue &&
    ancestors(target, records).some((above) => above.key === claim.issue)
  );
}

/** Whether `issue` lingers after its tree closed at `now` (record.Lingering): a linger deadline
 * after now. */
export function lingering(issue: CaseRecord, now: bigint): boolean {
  const until = issue.lingerUntil === null ? undefined : parseInstant(issue.lingerUntil);
  return until !== undefined && until > now;
}

/** The refusal of an architect's request whose grant is no architect's (api's architectGrant). */
export const ARCHITECT_REQUIRED: Refusal = {
  status: 403,
  code: "ARCHITECT_REQUIRED",
  message: "architect grant required",
};

/** The record of `key` an architect's request on it reads, or its refusal (api's
 * architectForIssue): 403 ISSUE_OUTSIDE_TREE for a record whose tree's architects do not include
 * the claim's (architectOfRecord), and 404 ISSUE_NOT_FOUND for an issue no record names. */
export function architectsRecord(
  claim: CaseClaim,
  key: string,
  records: ReadonlyMap<string, CaseRecord>
): { readonly record: CaseRecord } | { readonly refusal: Refusal } {
  const record = records.get(key);
  if (record === undefined)
    return { refusal: { status: 404, code: "ISSUE_NOT_FOUND", message: "issue is not recorded" } };
  if (!architectOfTree(claim, record, records))
    return {
      refusal: {
        status: 403,
        code: "ISSUE_OUTSIDE_TREE",
        message: "issue is outside the architect tree",
      },
    };
  return { record };
}

/** The refusal of `op` on `issue` by an architect that does not own it (api's architectOwns), or
 * none. */
export function notOwned(
  claim: CaseClaim,
  issue: CaseRecord,
  records: ReadonlyMap<string, CaseRecord>,
  op: string
): Refusal | undefined {
  if (architectAbove(claim, issue, records)) return undefined;
  return {
    status: 403,
    code: "ISSUE_NOT_OWNED",
    message: `${op} of ${issue.key} belongs to its own architect or one above it in the tree, not the architect of ${claim.issue}`,
  };
}

/** A sub-architect's `op` on `key`, an issue not under its own (api's notUnder). */
export function notUnder(claim: CaseClaim, op: string, key: string): Refusal {
  return {
    status: 403,
    code: "ISSUE_NOT_OWNED",
    message: `${op} of ${key} is refused: the architect of ${claim.issue} takes it only on an issue under ${claim.issue}`,
  };
}

/** A Dispatch issue as release_children and register_plan read it (dispatch.Issue): its status,
 * its parent and the issues its `blocked_by` links name. */
export interface DispatchIssue {
  readonly key: string;
  readonly status: string;
  readonly parent: string | null;
  readonly blockedBy: readonly string[];
}

/** A read of one Dispatch issue: the issue, undefined where Dispatch holds none, or the refusal a
 * failed read is answered with. */
export type IssueRead = (key: string) => Promise<DispatchIssue | undefined | Refusal>;

const isRefusal = (value: unknown): value is Refusal =>
  typeof value === "object" && value !== null && "code" in value;

/** `read` cached for one request, as api's cachedIssueReader caches it, answering 404
 * ISSUE_NOT_FOUND for an issue Dispatch does not hold. */
function cached(read: IssueRead): (key: string) => Promise<DispatchIssue | Refusal> {
  const seen = new Map<string, DispatchIssue | Refusal>();
  return async (key) => {
    const known = seen.get(key);
    if (known !== undefined) return known;
    const read_ = await read(key);
    const answer: DispatchIssue | Refusal =
      read_ === undefined
        ? { status: 404, code: "ISSUE_NOT_FOUND", message: `${key} is not a Dispatch issue` }
        : read_;
    seen.set(key, answer);
    return answer;
  };
}

/** The Dispatch parent chain of `key` up to `tree`, the ancestors it passed below `tree` nearest
 * first, or its refusal (api's treeChain): a chain that ends, or comes round on itself, before
 * `tree` is 403 ISSUE_OUTSIDE_TREE. */
async function treeChain(
  key: string,
  issue: DispatchIssue,
  tree: string,
  read: (key: string) => Promise<DispatchIssue | Refusal>
): Promise<string[] | Refusal> {
  const below: string[] = [];
  for (let parent = issue.parent; parent !== null && parent !== key && !below.includes(parent); ) {
    if (parent === tree) return below;
    below.push(parent);
    const ancestor = await read(parent);
    if (isRefusal(ancestor)) return ancestor;
    parent = ancestor.parent;
  }
  return {
    status: 403,
    code: "ISSUE_OUTSIDE_TREE",
    message: `${key} is outside the architect tree ${tree}`,
  };
}

/** register_plan's owner check (api's planOwner): a recorded issue is its tree's architects'
 * (architectsRecord's ISSUE_OUTSIDE_TREE), and one the daemon has not recorded is the tree's when
 * its Dispatch chain reaches the claim's tree and it is triage, backlog, icebox or todo. */
export async function planOwner(
  claim: CaseClaim,
  key: string,
  records: ReadonlyMap<string, CaseRecord>,
  read: IssueRead
): Promise<Refusal | undefined> {
  const recorded = architectsRecord(claim, key, records);
  if ("record" in recorded) return undefined;
  if (recorded.refusal.code !== "ISSUE_NOT_FOUND") return recorded.refusal;
  const reader = cached(read);
  const unrecorded = await reader(key);
  if (isRefusal(unrecorded)) return unrecorded;
  const chain = await treeChain(key, unrecorded, claim.tree, reader);
  if (isRefusal(chain)) return chain;
  if (["triage", "backlog", "icebox", "todo"].includes(unrecorded.status)) return undefined;
  return {
    status: 409,
    code: "ISSUE_ALREADY_RELEASED",
    message: `${key} is ${unrecorded.status} in Dispatch and the daemon holds no record of it; register_plan takes a plan for an issue the daemon has not recorded only while it is triage, backlog, icebox or todo`,
  };
}

/** What release_children answers (api.WaveReleaseResponse): the children it released, each once,
 * those whose first phase it started, and those it holds on an unfinished blocker. */
export interface Released {
  readonly released: string[];
  readonly started: string[];
  readonly held: { issue: string; blockedBy: string[] }[];
}

/**
 * release_children of `issues` by the architect of `claim` (api's waveRelease), judged over
 * `records` and the run's Dispatch (`read`), in the daemon's order:
 *
 * 1. each child (releasableChild): a Dispatch issue (404 ISSUE_NOT_FOUND); for a sub-architect,
 *    an issue under its own through its Dispatch chain (403 ISSUE_NOT_OWNED); triage, backlog or
 *    icebox, which the release writes todo, or todo already (409 ISSUE_ALREADY_RELEASED); and, for
 *    the root's architect, a chain that reaches the tree (403 ISSUE_OUTSIDE_TREE);
 * 2. the adoption of each child recorded in another tree (adoptReleasedChildren, admit's
 *    adoptChildren, workflow's AdoptionRefusal): a member of another tree joins only from a
 *    closed tree, at admitted and holding no slot (409 CHILD_IN_OTHER_TREE, 409
 *    CHILD_NOT_ADOPTABLE), and a root joins a tree that is live (409 CHILD_NOT_ADOPTABLE). A root
 *    that stands in a phase of its own tree ran as that tree (RanAsRoot: lane D opens the
 *    lifecycle of a tree whose root runs a phase) and is refused CHILD_NOT_ADOPTABLE; a root at
 *    admitted or done never opened one in the world, and joins.
 *
 * Its answer (releaseOutcome) names each child the release started: one it wrote todo, or one
 * the daemon had not recorded, that waits on no unfinished Dispatch issue (blockers.UnfinishedOf,
 * a blocker not done) and that stands in a live tree. A started child is recorded in the claim's
 * tree at planning, in_progress, its architect launching, a parked one at done in a generation of
 * its own (startedByRelease); one held or already todo stays at admitted, todo, and a record at
 * work in the claim's tree stays as it stood. `apply` makes those records; `written` is each child
 * the release writes todo in Dispatch, in the order the daemon writes them, which the caller
 * writes as the daemon's session does (api's setDispatchStatus).
 */
export async function releaseChildren(
  claim: CaseClaim,
  issues: readonly string[],
  records: Map<string, CaseRecord>,
  read: IssueRead,
  options: { readonly now: bigint; readonly slotted: readonly string[] }
): Promise<
  | { readonly refusal: Refusal }
  | { readonly answer: Released; readonly written: readonly string[]; apply(): void }
> {
  const reader = cached(read);
  const named = [...new Set(issues)];
  const byRoot = claim.issue === claim.tree;
  const children: { view: DispatchIssue; write: boolean; before: CaseRecord | undefined }[] = [];
  for (const key of named) {
    if (!isIssueKey(key))
      return {
        refusal: {
          status: 400,
          code: "INVALID_ISSUE",
          message: "issues must contain issue keys",
        },
      };
    const view = await reader(key);
    if (isRefusal(view)) return { refusal: view };
    if (!byRoot) {
      let under = false;
      if (key !== claim.tree) {
        const below = await treeChain(key, view, claim.tree, reader);
        if (isRefusal(below)) return { refusal: below };
        under = below.includes(claim.issue);
      }
      if (!under) return { refusal: notUnder(claim, "release_children", key) };
    }
    let write = true;
    if (view.status === "todo") write = false;
    else if (!["triage", "backlog", "icebox"].includes(view.status))
      return {
        refusal: {
          status: 409,
          code: "ISSUE_ALREADY_RELEASED",
          message: `${key} is ${view.status}; release_children releases only a triage, backlog, or icebox child`,
        },
      };
    if (byRoot) {
      const below = await treeChain(key, view, claim.tree, reader);
      if (isRefusal(below)) return { refusal: below };
    }
    const before = records.get(key);
    children.push({ view, write, before: before === undefined ? undefined : { ...before } });
  }
  const root = records.get(claim.tree);
  const live = root === undefined || (root.phase !== "done" && !lingering(root, options.now));
  const outside = children.filter(
    (child) => child.before !== undefined && child.before.tree !== claim.tree
  );
  const queued = new Set(
    outside.flatMap((child) =>
      child.before !== undefined && child.before.key === child.before.tree ? [child.before.key] : []
    )
  );
  for (const { before } of outside) {
    if (before === undefined) continue;
    const member = before.key !== before.tree;
    if (member && queued.has(before.tree)) continue;
    if (!member && before.phase !== "admitted" && before.phase !== "done")
      return { refusal: notAdoptable(before.key, claim.tree, "it ran as a tree of its own") };
    if (member) {
      const owner = records.get(before.tree);
      const closed =
        before.phase === "admitted" &&
        owner !== undefined &&
        owner.status === "done" &&
        !lingering(owner, options.now);
      if (!closed)
        return {
          refusal: {
            status: 409,
            code: "CHILD_IN_OTHER_TREE",
            message: `${before.key} runs in the tree of ${before.tree}, not ${claim.tree}`,
          },
        };
    }
    if (!live)
      return {
        refusal: notAdoptable(before.key, claim.tree, `the tree of ${claim.tree} is not live`),
      };
    const slot = options.slotted.indexOf(before.key);
    if (member && slot >= 0)
      return {
        refusal: notAdoptable(
          before.key,
          claim.tree,
          `it holds admission slot ${slot} in the tree of ${before.tree}`
        ),
      };
  }
  const unfinished = async (child: DispatchIssue): Promise<string[]> => {
    const waiting: string[] = [];
    for (const blocker of child.blockedBy) {
      const row = await reader(blocker);
      if (isRefusal(row) || row.status !== "done") waiting.push(blocker);
    }
    return waiting;
  };
  const started: string[] = [];
  const held: Released["held"] = [];
  const after: CaseRecord[] = [];
  for (const { view, write, before } of children) {
    const adopted = before !== undefined && before.tree !== claim.tree;
    // A record already at work in the claim's tree runs on as it stood (runsPhase).
    if (!adopted && before !== undefined && !["admitted", "done"].includes(before.phase)) continue;
    const waits = await unfinished(view);
    const parked = !adopted && before?.phase === "done";
    const starts = live && waits.length === 0 && (write || before === undefined);
    if (starts) started.push(view.key);
    else if (waits.length > 0) held.push({ issue: view.key, blockedBy: waits });
    after.push({
      key: view.key,
      tree: claim.tree,
      parent: view.parent,
      phase: starts ? "planning" : "admitted",
      status: starts ? "in_progress" : "todo",
      generation: (before?.generation ?? 1) + (starts && parked ? 1 : 0),
      lingerUntil: null,
      architect: starts ? { state: "launching" } : (before?.architect ?? null),
    });
  }
  // The children the release writes todo in Dispatch, a parent the wave releases before its child
  // (api's releaseOrder); a chain that comes round on itself is written in the wave's order.
  const writing = children.filter((child) => child.write).map((child) => child.view);
  const written: string[] = [];
  while (written.length < writing.length) {
    const ready = writing.filter(
      (view) =>
        !written.includes(view.key) &&
        !writing.some((other) => other.key === view.parent && !written.includes(other.key))
    );
    const next = ready.length > 0 ? ready : writing.filter((view) => !written.includes(view.key));
    written.push(...next.map((view) => view.key));
  }
  return {
    answer: { released: named, started, held },
    written,
    apply: () => {
      for (const each of after) records.set(each.key, each);
    },
  };
}

/** intake.ChildNotAdoptable: `key` cannot join the tree of `tree`, for `reason`. */
function notAdoptable(key: string, tree: string, reason: string): Refusal {
  return {
    status: 409,
    code: "CHILD_NOT_ADOPTABLE",
    message: `${key} cannot join the tree of ${tree}: ${reason}`,
  };
}

/** rerun_child of `key` by the architect of `claim` (api's childOfTree and rerunChild), or its
 * refusal: the record and its tree's ownership (architectsRecord), a sub-architect's issue under
 * its own (ISSUE_NOT_OWNED), no tree root (403 CHILD_REQUIRED), a child out of the workflow (409
 * CHILD_RUNNING, naming the move that takes it out, `shown` being Dispatch's status of it where
 * the run's Dispatch read it), and a tree that does not linger (409 TREE_LINGERING). `apply` moves
 * the child into its next run at admitted, todo (workflow's ReenterChild). */
export function rerunChild(
  claim: CaseClaim,
  key: string,
  records: Map<string, CaseRecord>,
  shown: string | undefined
): { readonly refusal: Refusal } | { apply(): void } {
  const recorded = architectsRecord(claim, key, records);
  if ("refusal" in recorded) return recorded;
  const child = recorded.record;
  if (!ownsUnder(claim, child, records)) return { refusal: notUnder(claim, "rerun_child", key) };
  if (child.key === child.tree)
    return {
      refusal: {
        status: 403,
        code: "CHILD_REQUIRED",
        message: `${key} is the tree root. rerun_child takes a child of it.`,
      },
    };
  if (child.phase !== "done")
    return { refusal: { status: 409, code: "CHILD_RUNNING", message: childRunning(child, shown) } };
  const root = records.get(child.tree);
  if (root === undefined)
    return {
      refusal: {
        status: 500,
        code: "RECORD_UNAVAILABLE",
        message: "could not read the tree root",
      },
    };
  if (root.lingerUntil !== null)
    return {
      refusal: {
        status: 409,
        code: "TREE_LINGERING",
        message: `the tree ${root.key} is lingering after it left the workflow, so ${key} cannot run in it again`,
      },
    };
  return {
    apply: () =>
      records.set(key, {
        ...child,
        phase: "admitted",
        status: "todo",
        generation: child.generation + 1,
      }),
  };
}

/** The statuses that take an issue out of the workflow (record.OutOfWorkflowStatuses). */
const OUT_OF_WORKFLOW = ["triage", "icebox", "backlog", "done"];

/** Why rerun_child refuses `child`, which runs a phase (api's childRunning). */
function childRunning(child: CaseRecord, shown: string | undefined): string {
  const key = child.key;
  if (shown !== undefined && OUT_OF_WORKFLOW.includes(shown))
    return (
      `Dispatch shows ${shown} for ${key}, but the daemon has not taken that move yet. The record still shows ${key} in phase ${child.phase}. ` +
      `Call rerun_child again when read_record shows ${key} at phase done. ` +
      `If dispatch read --issue ${key} no longer shows ${shown}, the daemon set the move back. ` +
      `In that case, write backlog on ${key} first (dispatch issue-update --issue ${key} --status backlog). ` +
      `The daemon sets back a phase worker's write and a write by an architect that does not own ${key}. ` +
      "It also sets back a write of the status that Dispatch already showed."
    );
  return (
    `${key} is in phase ${child.phase}, so rerun_child cannot run it again now. The restart of a running child is backlog, then rerun_child. ` +
    `Write backlog on ${key} first (dispatch issue-update --issue ${key} --status backlog). Then call rerun_child again when read_record shows ${key} at phase done.`
  );
}

/** The phase entry a completion is held to (`daemon.entry`): the entry the role's phases row
 * names, the entry the claim serves, the task queued at the row's entry and whether a move back
 * raised it. */
export interface Entry {
  readonly phase: number;
  readonly turn: number;
  readonly owedTask: string | null;
  readonly movedBack?: boolean;
}

/** The id of the delivery the daemon queued with an entry's owed task, which neither lane learns
 * from the world: one fixed name of the issue, the role and the entry, which lane D gives the
 * delivery it seeds too. */
export function owedDeliveryId(issue: string, role: string, entry: number): string {
  return `owed-${issue.toLowerCase()}-${role}-entry-${entry}`;
}

/**
 * The completion route's entry fence (api's entryFence) for a completion by `role` of `issue` in
 * `phase`, at `commit` (none where it names none): none where the entry the claim serves is the
 * row's; HANDOFF_NOT_CURRENT_PHASE where the role works no phase the issue stands in, or the
 * issue is admitted; HANDOFF_STALE_ENTRY for an entry above the row's (unraisedEntry); and, for one
 * behind it, HANDOFF_STALE_ENTRY unless behindEntryClause takes it: the phase ends with a record
 * this role writes (`recordBacked`), the commit is the head of the pull request the record holds
 * (`head`), and no move back raised the entry.
 */
export function entryFence(
  entry: Entry,
  request: {
    readonly issue: string;
    readonly role: string;
    readonly phase: string;
    readonly worksPhase: boolean;
    readonly recordBacked: boolean;
    readonly commit: string | undefined;
    readonly head: string | undefined;
  }
): Refusal | undefined {
  const { issue, role, phase } = request;
  const current = entry.phase;
  const serving = entry.turn;
  if (serving === current) return undefined;
  if (phase === "admitted" || !request.worksPhase)
    return {
      status: 409,
      code: "HANDOFF_NOT_CURRENT_PHASE",
      message: `the ${role} does not run phase ${phase} of ${issue}; this completion changed nothing`,
    };
  if (serving > current)
    return {
      status: 409,
      code: "HANDOFF_STALE_ENTRY",
      message: `this completion is of entry ${serving} of phase ${phase} of ${issue}, and the ${role}'s phases row is on entry ${current}: the daemon raises that entry in the transaction that composes the entry's task, so no task it hands out carries an entry above it. Nothing was recorded; end this turn without completing and tell the architect`,
    };
  const movedBack = entry.movedBack === true;
  let clause: string;
  if (!request.recordBacked)
    clause =
      "this phase ends with no handoff record this role writes, so nothing this completion names says which entry's work it is";
  else {
    const commit = request.commit ?? "";
    const head = request.head ?? "";
    if (!movedBack && commit !== "" && commit === head) return undefined;
    const gave =
      commit === "" ? "this completion names no commit" : `this completion is at commit ${commit}`;
    const holds =
      head === ""
        ? "the record holds no pull request for the issue"
        : `the pull request the record holds is at head ${head}`;
    clause = `${gave} while ${holds}`;
    if (movedBack)
      clause +=
        ", and the move that handed the phase out again moved the issue back, which asks for this phase's work to be done again, so no verdict on either head answers for it";
  }
  let stale = `${issue} handed the ${role} phase ${phase} again: this completion is of entry ${serving}, which is ${current - serving} behind the entry ${current} the phase is on, and ${clause}; nothing was recorded`;
  if (entry.owedTask !== null)
    stale = `${stale}, and your turn began before the daemon's task for entry ${current} (${owedDeliveryId(issue, role, current)}) reached this session`;
  return {
    status: 409,
    code: "HANDOFF_STALE_ENTRY",
    message: `${stale}. ${STALE_ENTRY_INSTRUCTION}`,
  };
}
