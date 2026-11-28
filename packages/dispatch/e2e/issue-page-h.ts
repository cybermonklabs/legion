import type { Locator, Page } from "@playwright/test";

import type { Actor } from "../web/src/api/types";
import type { MarginSection } from "../web/src/features/issue-page/page-structure";
import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { PAGE_SIZE } from "../web/src/features/issue-page/show-more-fixture";
import { setLiveSessions } from "./agents";
import {
  claimIssue,
  createAsk,
  createComment,
  createHandoff,
  createIssue,
  createIssueArtifact,
  createPriority,
  createProject,
  getIssueEvents,
  getMyState,
  handAskOn,
  patchIssue,
  putArchitectureSource,
  putIssueState,
  putProjectDefaultAssignee,
  putStep,
  requestApproval,
  reviewArtifact,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { type LegionStateSeed, seedLegionState } from "./legion-state-seed";

/**
 * What the browser rows of `issue-page-h.e2e.ts` read of Details: the locators of its panel and
 * the one seed its rows open. The rows of `issue-page-m.e2e.ts` read its locators too.
 *
 * `e2e/issue-page.ts` opens the new page and Details (`phase`, `gotoIssue`, `openDetails`,
 * `closeDetails`); this file adds what reads the panel inside it.
 *
 * - **The panel** `[data-testid="issue-margin"]`, the host the details slot's panel is appended to
 *   inside the drawer (`IssueMargin.tsx`'s `IssueMarginCards`).
 * - **A section** `[data-margin-section="<name>"]`, whose heading is the one button that closes
 *   and opens it, with `aria-expanded`.
 * - **A row** of a section: the label column and the value, found by the label's words.
 * - **Pinned** `[data-testid="issue-pins"]`.
 * - **Files and documents** `[data-testid="issue-files"]`, the list of the issue's documents
 *   and files inside the panel, one link each.
 * - **The Legion block**, the region named `CONTROL_LABELS.legion.heading` directly above the
 *   history, whose lines are `[data-legion-line="phase|place|read"]` (`LegionBlock.tsx`).
 */

/** The details slot's panel, inside Details. */
export function marginSlot(page: Page): Locator {
  return page.getByTestId("issue-margin");
}

export function marginSection(page: Page, section: MarginSection): Locator {
  return marginSlot(page).locator(`[data-margin-section="${section}"]`);
}

/** The one control that opens and closes a section: its heading. */
export function marginSectionDisclosure(page: Page, section: MarginSection): Locator {
  return marginSection(page, section).getByRole("button").first();
}

/** One row of Details, by the words of its label column: the row's label and its value. */
export function detailsRow(page: Page, label: string): Locator {
  return marginSlot(page)
    .locator("[data-margin-section] > div > div")
    .filter({ has: page.getByText(label, { exact: true }) });
}

/** The one control that adds a property with no value yet, under Development's rows. */
export function addControl(page: Page): Locator {
  return marginSlot(page).getByRole("button", { exact: true, name: CONTROL_LABELS.add });
}

/** The reader's pins, under the sections. */
export function pinsList(page: Page): Locator {
  return page.getByTestId("issue-pins");
}

/** Files and documents' list, inside the panel: one entry, a link, for each. */
export function filesList(page: Page): Locator {
  return marginSlot(page).getByTestId("issue-files");
}

/** The project every issue of the seed is in. */
export const PROJECT = "DET";
/** The repository ONE carries, which the fake GitHub serves with one pull request. Its name is
 *  twenty characters long, as long as a real repository's name that the box of Details'
 *  Repository row was found to cut. */
export const REPOSITORY = "acme/details-of-rows";
const ARCHITECTURE_REPO = "acme/details-architecture";
export const PULL_NUMBER = 11;
/** ONE's link out of Dispatch that is not a pull request. */
export const OTHER_LINK = `https://github.com/${REPOSITORY}/issues/7`;
/** The one component of the project's model, which ONE is attached to. */
export const COMPONENT_TITLE = "Details panel";
/** The agent that claims ONE, under the title the fake Envoy lists it by. */
export const PLANNER = "details-planner";
export const PLANNER_TITLE = "Planner (details)";
const planner: Actor = { id: PLANNER, kind: "session", origin: { session_title: PLANNER_TITLE } };
const asPlanner = { actor: planner, as: "agent" as const };
/** The priority ONE advances, which TWO inherits once ONE is its parent. */
export const PRIORITY_TITLE = "Details hold every property";
/** ONE's twenty labels, Dispatch's limit, in the order the issue holds them. */
export const TWENTY_LABELS = Array.from(
  { length: 20 },
  (_, index) => `label-${String(index + 1).padStart(2, "0")}`
);
/** The words of ONE's comment nobody pinned, which Details never draws. */
export const UNPINNED_COMMENT = "An unpinned comment of ONE, which Details never draws.";
/** ONE's question, waiting on alice, which Details never draws. */
export const QUESTION = "Which width does Details open at?";
/** The first line of ONE's pinned comment, which Pinned draws as its link: the words of the
 *  server's reading of the comment's text, with none of the `**` its body marks them with. */
export const PINNED_FIRST_LINE = "The pinned comment's first line.";
/** The reason bob gave for the changes he asked of THREE's spec. */
export const CHANGES_REASON = "Name the limits";
/** How many issues wait on ONE, and how many documents ONE holds: one more than Details draws
 *  of a list before the Show more under it (`PAGE_SIZE`). */
export const ONE_PAST_PAGE = PAGE_SIZE + 1;

export interface DetailsSeed {
  /** Every key `seedDetailsIssue` names, by its part in the rows. */
  readonly keys: {
    readonly blocks: readonly string[];
    readonly one: string;
    readonly parent: string;
    readonly three: string;
    readonly two: string;
  };
  readonly pinnedCommentId: string;
  readonly priorityId: string;
  /** The titles the rows read, each beside its key: `blocks[i]` is the title of `keys.blocks[i]`. */
  readonly titles: { readonly blocks: readonly string[]; readonly three: string };
}

/**
 * The issues the rows of `issue-page-h.e2e.ts` read, as alice, written through Dispatch with
 * `main`'s helpers.
 *
 * - **ONE** has a parent, the twenty labels, a repository, a pull request and one other link, a
 *   component, the planner's claim, the route `role:architect`, an advanced priority,
 *   twenty-one documents (its spec and twenty more), twenty-one issues it blocks, a spec
 *   approval request handed on to bob and a task list of two items, one checked, an unpinned
 *   comment, a question waiting on alice, a pinned comment whose first line starts with a mark
 *   and a pinned line of its history, and it is blocked by THREE.
 * - **TWO** has no parent, route, label, blocker or priority, and an approval request waiting on
 *   alice, the project's default assignee.
 * - **THREE**'s spec carries bob's request for changes with the reason `CHANGES_REASON`.
 */
export async function seedDetailsIssue(): Promise<DetailsSeed> {
  await createProject({ key: PROJECT, name: "Details" });
  // An approval request or a question waits on its issue's assignee, and on the project's
  // default assignee where the issue has none: TWO has none, and its request waits on alice.
  await putProjectDefaultAssignee(PROJECT, { assignee: "alice" }, { as: "user", login: "alice" });
  await seedFakeGithub({
    [ARCHITECTURE_REPO]: {
      contents: "read",
      files: { "panel.md": `---\ntitle: ${COMPONENT_TITLE}\n---\nThe panel of properties.\n` },
      installation_id: 411,
    },
    [REPOSITORY]: {
      contents: "read",
      installation_id: 412,
      pull_requests: "read",
      pulls: { [PULL_NUMBER]: { head_sha: "a".repeat(40), state: "open", title: "ONE's work" } },
    },
  });
  await putArchitectureSource(PROJECT, { branch: "main", repo: ARCHITECTURE_REPO });
  const source = await syncArchitectureSource(PROJECT, ARCHITECTURE_REPO);
  if (source.last_error !== null) {
    throw new Error(`the component model did not import: ${source.last_error}`);
  }
  await setLiveSessions([{ session_id: PLANNER, title: PLANNER_TITLE }]);

  // `force` past the possible-duplicate guard: these titles are deliberately close.
  const parent = await createIssue({ force: true, project: PROJECT, title: "ONE sits under it" });
  const three = await createIssue({
    force: true,
    project: PROJECT,
    spec: "THREE's spec, which bob sends back.",
    title: "THREE is what ONE waits on",
  });
  await reviewArtifact(
    three.primary_artifact_id ?? "",
    { reason: CHANGES_REASON, state: "changes_requested" },
    { login: "bob" }
  );
  const priority = await createPriority({ owner: "alice", title: PRIORITY_TITLE });

  const one = await createIssue({
    force: true,
    parent: parent.key,
    project: PROJECT,
    spec: "ONE's spec, which waits on bob's approval.\n\n- [x] Read it.\n- [ ] Approve it.\n",
    title: "ONE holds every property Details draws",
  });
  await patchIssue(one.key, {
    advances: [priority.id],
    assignee: "alice",
    blocked_by: [three.key],
    components: { ids: ["panel"], mode: "explicit" },
    external_links: [
      { kind: "github_pr", url: `https://github.com/${REPOSITORY}/pull/${PULL_NUMBER}` },
      { kind: "github_issue", url: OTHER_LINK },
    ],
    labels: [...TWENTY_LABELS],
    repo: REPOSITORY,
    route: "role:architect",
  });
  await claimIssue(one.key, asPlanner);
  // The spec and `PAGE_SIZE` documents more: `ONE_PAST_PAGE`.
  for (let index = 1; index < ONE_PAST_PAGE; index += 1) {
    await createIssueArtifact(one.key, {
      content: `Notes ${index} of ONE.\n`,
      name: `notes-${String(index).padStart(2, "0")}.md`,
    });
  }
  const blocks: string[] = [];
  const blockTitles: string[] = [];
  for (let index = 1; index <= ONE_PAST_PAGE; index += 1) {
    const waiter = await createIssue({
      blocked_by: [one.key],
      force: true,
      project: PROJECT,
      title: `Waits on ONE (${index})`,
    });
    blocks.push(waiter.key);
    blockTitles.push(waiter.title);
  }
  // The spec's request waits on ONE's assignee, alice, until she hands it on to bob, who has
  // signed in (`PUT /api/v1/asks/{id}/asked-of`).
  const requested = await requestApproval(
    one.primary_artifact_id ?? "",
    { summary: "Read it back." },
    asPlanner
  );
  if (requested.ask === null) throw new Error("ONE's approval request opened no ask");
  await handAskOn(requested.ask.id, "bob");
  await createComment(one.key, { body: UNPINNED_COMMENT });
  await createAsk(one.key, { question: QUESTION }, asPlanner);
  const pinned = await createComment(one.key, {
    body: "**The pinned** comment's first line.\nIts second line, which Pinned does not draw.",
  });
  const events = await getIssueEvents(one.key);
  const event = events[0];
  if (event === undefined) throw new Error(`${one.key} has no event to pin`);
  const state = (await getMyState("alice"))[one.key];
  await putIssueState(
    one.key,
    {
      dismissed: [`pinned_items:comment:${pinned.id}`, `pinned_items:event:${event.id}`],
      seq: (state?.seq ?? 0) + 1,
    },
    { login: "alice" }
  );

  const two = await createIssue({
    force: true,
    project: PROJECT,
    spec: "TWO's spec, which waits on alice's approval.",
    title: "TWO holds nothing yet",
  });
  await requestApproval(two.primary_artifact_id ?? "", { summary: "Read it." }, asPlanner);

  return {
    keys: { blocks, one: one.key, parent: parent.key, three: three.key, two: two.key },
    pinnedCommentId: pinned.id,
    priorityId: priority.id,
    titles: { blocks: blockTitles, three: three.title },
  };
}

// The Legion block of Details (dispatch://INFRA-4870/spec#b-r-block)
// ---------------------------------------------------------------------------------------------

/** The project whose Legion daemon the Legion block's rows seed (`e2e/legion-state-seed.ts`), and
 *  one that names no daemon. */
export const LEGION_PROJECT = "LGN";
export const NO_DAEMON_PROJECT = "QUIET";

/** The agent of ONE's one run, which Details' runs draw and the Legion block never does. */
export const RUN_AGENT = "legion-lgn-one-implementer";

/** ONE's Legion state as the block's rows first read it: `implementing`, at work, the architect
 *  and the implementer in a turn, the tester idle and the planner paused, and a good read. */
export const IMPLEMENTING: LegionStateSeed = {
  claims: { architect: "working", implementer: "working", planner: "suspended", tester: "idle" },
  phase: "implementing",
  slot: 0,
};

export interface LegionBlockSeed {
  /** ONE's question, which an ask wait names. */
  readonly askId: string;
  /** ONE's document, which a gate wait names, at its one version. */
  readonly document: { readonly id: string; readonly slug: string };
  readonly keys: {
    /** The issue the daemon records, in `implementing`. */
    readonly one: string;
    /** An issue of the same project the daemon records nothing of. */
    readonly two: string;
    /** An issue of a project that names no daemon. */
    readonly quiet: string;
  };
}

/**
 * The issues the Legion block's rows read, as alice: ONE, of `LEGION_PROJECT`, with a question, a
 * document, one run of its implementation step and one implement handoff, which Details lists
 * and the block does not, and the Legion state `IMPLEMENTING`, stored over `planning` so that
 * Dispatch saw it start; TWO, of the same project, of which the daemon records nothing; and QUIET,
 * of `NO_DAEMON_PROJECT`.
 */
export async function seedLegionBlock(): Promise<LegionBlockSeed> {
  await createProject({ key: LEGION_PROJECT, name: "Legion block" });
  await createProject({ key: NO_DAEMON_PROJECT, name: "No daemon" });
  const one = await createIssue({
    force: true,
    project: LEGION_PROJECT,
    title: "ONE is where Legion stands",
  });
  const two = await createIssue({
    force: true,
    project: LEGION_PROJECT,
    title: "TWO is not in Legion yet",
  });
  const quiet = await createIssue({
    force: true,
    project: NO_DAEMON_PROJECT,
    title: "QUIET's project names no daemon",
  });
  const ask = await createAsk(one.key, { question: "Which gate opens first?" });
  const document = await createIssueArtifact(one.key, {
    content: "The design ONE's gate waits on.\n",
    name: "design.md",
  });
  await putStep(one.key, crypto.randomUUID(), {
    agent: RUN_AGENT,
    generation: 1,
    kind: "step",
    model: "anthropic/claude-opus-5",
    role: "implementer",
    started_at: new Date(Date.now() - 60_000).toISOString(),
    step: 2,
    task: "default",
    type: "implementation",
  });
  await createHandoff(one.key, "implement", { head: "ONE's implement handoff" });
  await seedLegionState(one.key, { claims: { planner: "working" }, phase: "planning" });
  await seedLegionState(one.key, IMPLEMENTING);
  return {
    askId: ask.id,
    document: { id: document.artifact.id, slug: document.artifact.slug },
    keys: { one: one.key, quiet: quiet.key, two: two.key },
  };
}

/** The Legion block, inside Details, by its heading (`LegionBlock.tsx`). */
export function legionBlock(page: Page): Locator {
  return marginSlot(page).getByRole("region", {
    exact: true,
    name: CONTROL_LABELS.legion.heading,
  });
}

/** One line of the Legion block: the phase and since when, the place and the wait, or the read. */
export function legionLine(page: Page, line: "phase" | "place" | "read"): Locator {
  return legionBlock(page).locator(`[data-legion-line="${line}"]`);
}

/** What the page's live socket carried of the `legion_issue` view (`routeLiveLegion`). */
export interface LiveLegion {
  /** The issue each `legion_issue` subscribe frame the page sent names, in order. */
  readonly subscribed: string[];
  /** The issue of each such subscription the page then unsubscribed, in order. */
  readonly unsubscribed: string[];
  /** The issue of each such subscription a snapshot answered, in order. */
  readonly answered: string[];
  /** While true, the page's `legion_issue` subscribe frames reach no Dispatch, and each is
   *  answered with the error frame of a failure of Dispatch's own: a code that is not
   *  `INVALID_FRAME`, which would close the socket rather than the subscription. */
  refuse: boolean;
}

/** Puts the page's live socket through this process, which passes every frame on in both
 *  directions and notes what crossed of the `legion_issue` view. Installed before the page
 *  opens, as every route is. */
export async function routeLiveLegion(page: Page): Promise<LiveLegion> {
  const live: LiveLegion = { answered: [], refuse: false, subscribed: [], unsubscribed: [] };
  const read = (message: string | Buffer) =>
    JSON.parse(String(message)) as {
      readonly sub?: string;
      readonly type?: string;
      readonly view?: { readonly issue?: string; readonly kind?: string };
    };
  await page.routeWebSocket(/\/ws\/live$/u, (route) => {
    const server = route.connectToServer();
    // A socket's subscription ids are its own, so each socket keeps the issue of each of its own.
    const issues = new Map<string, string>();
    route.onMessage((message) => {
      const frame = read(message);
      if (frame.type === "subscribe" && frame.view?.kind === "legion_issue" && frame.sub) {
        const issue = frame.view.issue ?? "";
        live.subscribed.push(issue);
        issues.set(frame.sub, issue);
        if (live.refuse) {
          route.send(
            JSON.stringify({
              code: "INTERNAL",
              message: "the Legion state could not be read",
              sub: frame.sub,
              type: "error",
            })
          );
          return;
        }
      }
      const unsubscribed = frame.type === "unsubscribe" ? issues.get(frame.sub ?? "") : undefined;
      if (unsubscribed !== undefined) live.unsubscribed.push(unsubscribed);
      server.send(message);
    });
    server.onMessage((message) => {
      const frame = read(message);
      const answered = frame.type === "snapshot" ? issues.get(frame.sub ?? "") : undefined;
      if (answered !== undefined) live.answered.push(answered);
      route.send(message);
    });
  });
  return live;
}
