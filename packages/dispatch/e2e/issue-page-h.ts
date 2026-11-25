import type { Locator, Page } from "@playwright/test";

import type { Actor } from "../web/src/api/types";
import { FOLD_AFTER } from "../web/src/features/issue-page/details-leaves";
import type { MarginSection } from "../web/src/features/issue-page/page-structure";
import { CONTROL_LABELS } from "../web/src/features/issue-page/page-words";
import { setLiveSessions } from "./agents";
import {
  claimIssue,
  createAsk,
  createComment,
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
  type RowKey,
  requestApproval,
  reviewArtifact,
  syncArchitectureSource,
} from "./api";
import { seedFakeGithub } from "./fake-github-helpers";
import { rowKey, rowSessionId } from "./test";

/**
 * What the browser rows of `issue-page-h.e2e.ts` read of Details: the locators of its panel and
 * the one seed its rows open. No other row reads this file.
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

/** The logical key of the project every issue of the seed is in, which `seedDetailsIssue` makes
 *  under the running row's own key for it (`rowKey`, e2e/test.ts), since the database is emptied
 *  before the worker's first row and before the review corpus's seed, not before each row. */
const PROJECT = "DET";
/** The repository ONE carries, which the fake GitHub serves with one pull request: the row's own,
 *  `acme/detail-` and the project's key in lower case, since Dispatch links one address from one
 *  issue alone (`EXTERNAL_LINK_TAKEN`) and ONE links its pull request and one of its issues. Its
 *  name is twenty characters long (`rowKey` answers eight for `DET`), as long as a real
 *  repository's name that the box of Details' Repository row was found to cut. */
function detailsRepository(project: RowKey): string {
  return `acme/detail-${project.toLowerCase()}`;
}
const ARCHITECTURE_REPO = "acme/details-architecture";
export const PULL_NUMBER = 11;
/** The one component of the project's model, which ONE is attached to. */
export const COMPONENT_TITLE = "Details panel";
/** The logical name of the agent that claims ONE, whose session is the running row's own
 *  (`rowSessionId`, e2e/test.ts): Dispatch keeps what a session said, and what it claimed, by its
 *  id for every row of the worker at once. */
const PLANNER = "planner";
/** The title the fake Envoy lists the planner's session by. */
export const PLANNER_TITLE = "Planner (details)";
/** The words of the priority ONE advances, which TWO inherits once ONE is its parent. The seed
 *  writes them followed by the project's key (`DetailsSeed.priorityTitle`): a priority belongs to
 *  no project, and the picker lists every priority the rows of the worker wrote. */
const PRIORITY_TITLE = "Details hold every property";
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
export { FOLD_AFTER };
/** How many issues wait on ONE, and how many documents ONE holds: one more than Details draws
 *  before it folds a list behind Show all. */
export const ONE_PAST_FOLD = FOLD_AFTER + 1;

export interface DetailsSeed {
  /** Every key `seedDetailsIssue` names, by its part in the rows. */
  readonly keys: {
    readonly blocks: readonly string[];
    readonly one: string;
    readonly parent: string;
    readonly three: string;
    readonly two: string;
  };
  /** ONE's link out of Dispatch that is not a pull request: an issue of `repository`. */
  readonly otherLink: string;
  readonly pinnedCommentId: string;
  /** The session id of the agent that claims ONE, which the fake Envoy lists by `PLANNER_TITLE`. */
  readonly planner: string;
  readonly priorityId: string;
  /** The priority's title: `PRIORITY_TITLE` and the project's key. */
  readonly priorityTitle: string;
  /** The project every issue of the seed is in, the row's own key for `DET`. */
  readonly project: RowKey;
  /** The repository ONE carries (`detailsRepository`). */
  readonly repository: string;
  /** The titles the rows read, each beside its key: `blocks[i]` is the title of `keys.blocks[i]`. */
  readonly titles: { readonly blocks: readonly string[]; readonly three: string };
}

/**
 * The issues the rows of `issue-page-h.e2e.ts` read, as alice, written through Dispatch with
 * `main`'s helpers, in a project, a repository, a session and a priority of the row's own, beside
 * what the rows before it wrote: the suite's `test` empties the database before the worker's
 * first row and before the review corpus's seed, not before each row.
 *
 * - **ONE** has a parent, the twenty labels, a repository, a pull request and one other link, a
 *   component, the planner's claim, the route `role:architect`, an advanced priority, eleven
 *   documents, eleven issues it blocks, a spec approval request handed on to bob and a task list
 *   of two items, one checked, an unpinned comment, a question waiting on alice, a pinned
 *   comment whose first line starts with a mark and a pinned line of its history, and it is
 *   blocked by THREE.
 * - **TWO** has no parent, route, label, blocker or priority, and an approval request waiting on
 *   alice, the project's default assignee.
 * - **THREE**'s spec carries bob's request for changes with the reason `CHANGES_REASON`.
 */
export async function seedDetailsIssue(): Promise<DetailsSeed> {
  const project = rowKey(PROJECT);
  const repository = detailsRepository(project);
  const otherLink = `https://github.com/${repository}/issues/7`;
  const planner = rowSessionId(PLANNER);
  const plannerActor: Actor = {
    id: planner,
    kind: "session",
    origin: { session_title: PLANNER_TITLE },
  };
  const asPlanner = { actor: plannerActor, as: "agent" as const };
  await createProject({ key: project, name: "Details" });
  // An approval request or a question waits on its issue's assignee, and on the project's
  // default assignee where the issue has none: TWO has none, and its request waits on alice.
  await putProjectDefaultAssignee(project, { assignee: "alice" }, { as: "user", login: "alice" });
  await seedFakeGithub({
    [ARCHITECTURE_REPO]: {
      contents: "read",
      files: { "panel.md": `---\ntitle: ${COMPONENT_TITLE}\n---\nThe panel of properties.\n` },
      installation_id: 411,
    },
    [repository]: {
      contents: "read",
      installation_id: 412,
      pull_requests: "read",
      pulls: { [PULL_NUMBER]: { head_sha: "a".repeat(40), state: "open", title: "ONE's work" } },
    },
  });
  await putArchitectureSource(project, { branch: "main", repo: ARCHITECTURE_REPO });
  const source = await syncArchitectureSource(project, ARCHITECTURE_REPO);
  if (source.last_error !== null) {
    throw new Error(`the component model did not import: ${source.last_error}`);
  }
  await setLiveSessions([{ session_id: planner, title: PLANNER_TITLE }]);

  // `force` past the possible-duplicate guard: these titles are deliberately close.
  const parent = await createIssue({ force: true, project, title: "ONE sits under it" });
  const three = await createIssue({
    force: true,
    project,
    spec: "THREE's spec, which bob sends back.",
    title: "THREE is what ONE waits on",
  });
  await reviewArtifact(
    three.primary_artifact_id ?? "",
    { reason: CHANGES_REASON, state: "changes_requested" },
    { login: "bob" }
  );
  const priority = await createPriority({
    owner: "alice",
    title: `${PRIORITY_TITLE} (${project})`,
  });

  const one = await createIssue({
    force: true,
    parent: parent.key,
    project,
    spec: "ONE's spec, which waits on bob's approval.\n\n- [x] Read it.\n- [ ] Approve it.\n",
    title: "ONE holds every property Details draws",
  });
  await patchIssue(one.key, {
    advances: [priority.id],
    assignee: "alice",
    blocked_by: [three.key],
    components: { ids: ["panel"], mode: "explicit" },
    external_links: [
      { kind: "github_pr", url: `https://github.com/${repository}/pull/${PULL_NUMBER}` },
      { kind: "github_issue", url: otherLink },
    ],
    labels: [...TWENTY_LABELS],
    repo: repository,
    route: "role:architect",
  });
  await claimIssue(one.key, asPlanner);
  // The spec and ten documents more: eleven.
  for (let index = 1; index < ONE_PAST_FOLD; index += 1) {
    await createIssueArtifact(one.key, {
      content: `Notes ${index} of ONE.\n`,
      name: `notes-${String(index).padStart(2, "0")}.md`,
    });
  }
  const blocks: string[] = [];
  const blockTitles: string[] = [];
  for (let index = 1; index <= ONE_PAST_FOLD; index += 1) {
    const waiter = await createIssue({
      force: true,
      project,
      title: `Waits on ONE (${index})`,
    });
    await patchIssue(waiter.key, { blocked_by: [one.key] });
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
    project,
    spec: "TWO's spec, which waits on alice's approval.",
    title: "TWO holds nothing yet",
  });
  await requestApproval(two.primary_artifact_id ?? "", { summary: "Read it." }, asPlanner);

  return {
    keys: { blocks, one: one.key, parent: parent.key, three: three.key, two: two.key },
    otherLink,
    pinnedCommentId: pinned.id,
    planner,
    priorityId: priority.id,
    priorityTitle: priority.title,
    project,
    repository,
    titles: { blocks: blockTitles, three: three.title },
  };
}
