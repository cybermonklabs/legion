/** The rows write an issue's status, priority, people, labels, parent, route, repository,
 *  claim, subscribers, approval, decisions, uploads and documents through the page and read each
 *  write back from the server, and a closed issue's rows wait for the settlement version the
 *  server writes of its spec. */

/*
 * Part N of dispatch://INFRA-1196 (dispatch://INFRA-3940), R7 and c-inventory: the new page
 * carries every element of the old one, read against the fixture `e2e/record-page-inventory.e2e.ts`
 * wrote, one row per recorded state, and then the writes of that state (c-nothing-lost). The rows
 * were `e2e/issue-page-n.e2e.ts`'s, which keeps part N's other rows.
 */

import { readFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { presetPath } from "../web/src/features/issue-page/issue-address";
import {
  CONTROL_LABELS,
  PRESET_NAMES,
  PRESET_ORDER,
} from "../web/src/features/issue-page/page-words";
import { blockNodeId } from "../web/src/features/issue-page/statement-rules";
import { getUnsubscribeCalls } from "./agents";
import {
  getArtifact,
  getAsk,
  getIssue,
  getIssueArtifactText,
  getIssueDecisions,
  getMyState,
  listComments,
  patchIssue,
  requestApproval,
} from "./api";
import { placeCaret } from "./editor";
import { type AdvancingIssueSeed, seedWholeIssueAdvancing } from "./issue-layout-seed";
import { asPlanner, PLANNER_TITLE } from "./issue-layout-seed-support";
import { requirementBlockId, requirementText } from "./issue-layout-seed-text";
import {
  closeDetails,
  gotoIssue,
  marginShell,
  openDetails,
  openMenu,
  phase,
  presetChooser,
} from "./issue-page";
import {
  DESKTOP,
  expectIssuePageDrawn,
  nodeRow,
  nodeRows,
  PHONE,
  unfoldEveryBranch,
} from "./issue-page-g";
import { caretInNode, openPreset, pickPreset } from "./issue-page-i";
import {
  AWAITING_THE_BOX,
  AWAITING_THREADS,
  assertInventoryCarried,
  documentLink,
  documentsList,
  INVENTORY_PARENT_ROWS,
  INVENTORY_RENAMES,
  INVENTORY_ROWS,
  identityBlock,
  offered,
  openDocuments,
  openedEditor,
  openIssueActions,
  openVersionControl,
  type ParentRowMapping,
  uploadInDocuments,
  versionControl,
  zoomCrumbs,
} from "./issue-page-n";
import type { InventoryEntry } from "./page-inventory";
import { type Browser, expect, type Locator, type Page, test } from "./test";
import { expectLoadSettled, watchApiRequests } from "./traffic";

/** The fixture `e2e/record-page-inventory.e2e.ts` wrote: what today's page offered, by state.
 *  What the new page offers in its place is `e2e/issue-page-n.ts`'s rename table. */
interface PageInventoryFixture {
  readonly commit: string;
  readonly "open-1440": readonly InventoryEntry[];
  readonly "open-390": readonly InventoryEntry[];
  readonly "closed-1440": readonly InventoryEntry[];
  readonly "closed-390": readonly InventoryEntry[];
}

/** One of the fixture's four states: the issue open or closed, at 1440 or at 390 px. */
type InventoryState = Exclude<keyof PageInventoryFixture, "commit">;

/** The priorities' picker, which "Edit priorities" opens over the page in a portal
 *  (`IssuePriorityLine.tsx`, `MultiSelect`): its search box, named as the page names it. */
function prioritiesPicker(page: Page): Locator {
  return page.getByRole("combobox", { exact: true, name: CONTROL_LABELS.priority_to_advance });
}

/** The control in Details that opens the priorities' picker, which a closed issue draws refused. */
function editPriorities(page: Page): Locator {
  return marginShell(page).getByRole("button", {
    exact: true,
    name: CONTROL_LABELS.edit_priorities,
  });
}

/** Closes whatever the view before this one opened, so the next control is reached rather than
 *  dismissed: a press outside an open menu, or outside the identity block's actions group,
 *  closes it instead of reaching the control under it, and Escape closes each of them
 *  (`IssueActionsMenu.tsx`). The priorities' picker closes by Escape too, as a reader closes it,
 *  and Details by its own Close, since on a phone it covers the page. */
async function closeOpenControls(page: Page): Promise<void> {
  await page.keyboard.press("Escape");
  await expect(openMenu(page)).toHaveCount(0);
  const actions = offered(page, "button", CONTROL_LABELS.issue_actions);
  // The header drew this control with the page an earlier view loaded, and Escape draws no
  // control, so one count says whether this page shows it.
  if ((await actions.count()) === 1) {
    await expect(actions).not.toHaveAttribute("aria-expanded", "true");
  }
  await expect(prioritiesPicker(page), "Escape left the priorities' picker open").toHaveCount(0);
  await closeDetails(page);
}

/**
 * Puts the page where the outline draws both rows a parent-row mapping names (`ParentRowMapping`):
 * every branch open where the outline draws the whole tree, and below 1024 px, where a branch is
 * zoomed by its row's own key, the target's own zoom, whose nodes follow it. It is read off a page that has
 * stopped reading, as the zoom walk of `inventoryViews` is, because the tree fills as its reads
 * land. A target the tree does not draw has no zoom to press, and the mapping is then lost, which
 * `parentRowMissing` names: a press waiting for that twisty times out and ends the row there,
 * leaving every state after it unread.
 */
function inventoryOutline(
  page: Page,
  key: string,
  width: number
): (mapping: ParentRowMapping) => Promise<void> {
  return async (mapping) => {
    const settling = watchApiRequests(page);
    await gotoIssue(page, `/issues/${key}/conversation`);
    await expectLoadSettled(settling);
    if (width >= 1024) {
      await unfoldEveryBranch(page);
      return;
    }
    const parent = nodeRows(page).filter({ hasText: mapping.parent }).first();
    // The page's reads went quiet above, so the tree is drawn whole and one count says whether
    // the parent's row draws a twisty, which is what a branch has.
    if ((await parent.getByRole("button", { name: CONTROL_LABELS.unfold }).count()) > 0) {
      // A twisty opens a branch in place at every width; the zoom is the row's own key.
      await parent.focus();
      await page.keyboard.press(".");
      // The zoom is drawn once its crumbs are, and the rows are read the moment this returns.
      await expect(
        zoomCrumbs(page),
        `the zoom key of "${mapping.parent}" zoomed nowhere`
      ).not.toHaveCount(0);
    }
  };
}

/** The presets that filter the outline (R2), whose rows below 1024 px are read one at a time as
 *  the outline's are: Dashboard and Documents draw no row, and Conversation's rows are the
 *  outline's own, which the walk over the whole tree reads. */
const OUTLINE_FILTERS: readonly string[] = ["decisions", "plan", "spec", "verification"];

/** How many rows of each preset that filters the outline are read below 1024 px. */
const FILTER_ROWS = 8;

/**
 * The views the new page is read through for the inventory (R7), each a call that puts the page
 * in one view and answers whether that view is there. The fixture is the merge of the old page's
 * ten tab panels and its priority picker, one view at a time, so the new page is read the same
 * way: its seven presets, Documents' list among them, and the addresses beside them - the
 * conversation, the activity, the collaborators and the artifacts (R3, R12, R19). Some views are
 * the page's own, because what they hold sits behind a control rather than behind an address:
 * the spec opened from Documents' list, where every version, download and reference of a
 * document lives (R2, R3); the identity block's actions menu, where Close and the parent editor
 * live (R11); the priority row's picker, which Details' control opens (R13); and each row of the
 * tree, whose body is what a node opens (R6).
 *
 * A view over a row the tree does not draw answers that it is not there and is not read
 * (`e2e/issue-page-n.ts`), so the walk's depth costs a count and nothing else where the rows run
 * out. Each walk loads the page once and moves through it from there, which is how a reader
 * moves through it: the tree is drawn empty and fills as the reads land, so a walk reads it
 * only once the page has stopped reading, and a view that only moved the focus to a row of it
 * answers that row, which is read alone (`assertInventoryCarried`).
 */
function inventoryViews(
  page: Page,
  key: string,
  documents: { readonly plan: string; readonly spec: string; readonly verification: string },
  width: number
): (() => Promise<boolean | Locator>)[] {
  // How far a walk over the tree reaches. The seeded issue draws up to about 70 rows at 1440 px
  // once every branch is open, and a view over a row the tree does not draw answers that it is
  // not there rather than reading the page the view before it left (`assertInventoryCarried`), so
  // the bound costs a count and nothing else where the rows run out.
  const walkDepth = 80;
  // Which rows of the tree hold a zoom. The zoom walk's first view reads it off the tree it loads
  // and every view of that walk after it reads it back.
  const zoomsInto = new Set<number>();
  // Whether the zoom walk has a zoom open, which the next zoom it opens leaves first.
  let zoomOpen = false;
  /** The view that moves the focus to row `index` of the tree a view before it drew and waited
   *  out, and answers that row, which is read alone; `false` where the tree draws no such row.
   *  With `whole` it answers `true`, and the page is read whole. */
  const rowView =
    (index: number, whole = false) =>
    async (): Promise<boolean | Locator> => {
      const rows = nodeRows(page);
      // The walk's first view waited for a quiet load, and each view since moved the focus
      // alone, which draws no row, so one count holds the rows.
      if ((await rows.count()) <= index) return false;
      await rows.nth(index).focus();
      return whole ? true : rows.nth(index);
    };
  /** Leaves the zoom the walk is in for the whole outline, as a reader does: the issue's crumb,
   *  the first of the line above the zoom's rows, pressed with the focus on the zoom's own row,
   *  its first. The press unfolds the path down to the row that holds the focus before it zooms
   *  out (`IssueTree.tsx`'s crumb), and the path down to the zoom's own row is the one its zoom
   *  key unfolded already, so the outline is drawn again as its load drew it, each row where the
   *  walk's first view counted it. */
  const leaveZoom = async (): Promise<void> => {
    await nodeRows(page).first().focus();
    await zoomCrumbs(page).first().click();
    await expect(zoomCrumbs(page), "the issue's crumb did not leave the zoom").toHaveCount(0);
    zoomOpen = false;
  };
  return [
    // Each preset at its own address. Below 1024 px the tree is read one node at a time (R16),
    // so a preset that filters the outline is read row by row as well: a decision, a check and a
    // plan task sit under the issue in the preset that draws them, and a node's own text, its
    // facts and its controls are in the body its focus opens. There the preset is read off a
    // load whose reads went quiet, since the tree is drawn empty and fills as the reads land,
    // and its first rows are read in turn on that one load. A row of the Verification view
    // keeps the tree's tab stop where the tree's keys left it, not where the focus is
    // (`VerificationFilterBody.tsx`'s `rovingId`), so no stop says its focus is drawn, and that
    // row is read with the whole page, under the page's own settle.
    ...PRESET_ORDER.flatMap((preset) => [
      async () => {
        const settling = width < 1024 ? watchApiRequests(page) : undefined;
        await gotoIssue(page, presetPath(key, preset));
        if (settling !== undefined) await expectLoadSettled(settling);
        return true;
      },
      ...(width >= 1024 || !OUTLINE_FILTERS.includes(preset)
        ? []
        : Array.from({ length: FILTER_ROWS }, (_unused, index) =>
            rowView(index, preset === "verification")
          )),
    ]),
    // The conversation's address mounts the panel's list (`IssuePage.tsx`'s `activatedTabs`),
    // which is in Details, so the view opens it as a reader does (R14).
    async () => {
      await gotoIssue(page, `/issues/${key}/conversation`);
      await openDetails(page);
      return true;
    },
    // The shell's navigation: on the issue page the rail's foot control, or the header's `☰` on a
    // phone, opens the whole of it over the page, which holds every entry the old page's sidebar
    // offered (R14).
    async () => {
      await gotoIssue(page, `/issues/${key}`);
      await page.getByRole("button", { exact: true, name: "Open navigation" }).click();
      await expect(page.getByRole("dialog", { exact: true, name: "Navigation" })).toBeVisible();
      return true;
    },
    // Peek, which the old sidebar's Inbox entry carried and the navigation drawer carries on the
    // issue page, opens the Inbox drawer over the page without changing the address; the view
    // reads the page with that drawer open, and the next view loads the issue again.
    async () => {
      await expect(
        page.getByRole("dialog", { exact: true, name: "Navigation" }),
        "the navigation drawer is not open for Peek"
      ).toBeVisible();
      const address = page.url();
      await page
        .getByRole("button", { exact: true, name: "Peek at the inbox without leaving this page" })
        .click();
      await expect(
        page.getByRole("dialog", { exact: true, name: "Inbox" }),
        "Peek opened no Inbox drawer"
      ).toBeVisible();
      expect(page.url(), "Peek changed the issue's address").toBe(address);
      return true;
    },
    async () => {
      await gotoIssue(page, `/issues/${key}/activity`);
      return true;
    },
    // `/issues/<key>/collaborators`, the address the Collaborators tab answered: it is the one
    // address that opens the panel's list of everyone who worked the issue (R19, `IssuePage.tsx`'s
    // `opensCollaborators`), and the list's own People and Other agents sections
    // (`IssueCollaboratorList.tsx`) are drawn at no other address, so a walk without this view
    // reads that tab's landmarks as lost.
    async () => {
      await gotoIssue(page, `/issues/${key}/collaborators`);
      return true;
    },
    // `/issues/<key>/artifacts`, the address the Artifacts tab answered, opens Documents' list,
    // which carries that tab's list (R2, the Design's "Where each thing goes"): each file with
    // its download and, for a document the issue can drop, Remove, the list's filter, the upload
    // and New document, which a closed issue draws refused.
    async () => {
      await gotoIssue(page, `/issues/${key}/artifacts`);
      await expect(documentsList(page), "/artifacts does not open Documents").toBeVisible();
      return true;
    },
    // The spec opened from that list, in place of the outline (R3): its editor, with its
    // collaborative cursors and its comment chord, its versions with their downloads, what
    // references it, and the version control beside the filters reading it, with the old spec
    // toolbar's link to the block the caret is in and its live connection, which belong to an
    // open editor. The version control opens from its own small control (ruling 5), or from the
    // Outline sheet at 760 px and below, and this view reads the page with it open.
    async () => {
      await documentLink(page, documents.spec).click();
      await expect(
        openedEditor(page),
        "the spec opened from Documents' list draws no editor"
      ).toBeVisible();
      await openVersionControl(page);
      await expect(
        versionControl(page).getByRole("button", { name: "Copy link to block" })
      ).toHaveCount(1);
      return true;
    },
    // Escape shuts the version control again and leaves the document open; the margin's cards
    // beside the opened spec - its decisions, its comments - are in Details, which a reader opens
    // to read them beside the document (R14).
    async () => {
      await page.keyboard.press("Escape");
      await expect(versionControl(page)).toBeHidden();
      await expect(
        openedEditor(page),
        "Escape on the version control shut the document"
      ).toBeVisible();
      await openDetails(page);
      return true;
    },
    // The plan and the Verification document opened from Documents' list, each with its editor
    // and the block references beside it, which the old Plan and Verification tabs drew open: a
    // reference's source is named by its issue and its document, and lists in the aside only
    // where the document is opened. The list is reached from the document before it by that
    // document's own Back to Documents (R2), with Details closed first, since on a phone it
    // covers the page.
    ...[documents.plan, documents.verification].map((name) => async () => {
      await closeDetails(page);
      await offered(page, "button", CONTROL_LABELS.back_to_documents).click();
      await expect(
        documentsList(page),
        `${CONTROL_LABELS.back_to_documents} did not return to Documents' list`
      ).toBeVisible();
      await documentLink(page, name).click();
      await expect(
        openedEditor(page),
        `${name} opened from Documents' list draws no editor`
      ).toBeVisible();
      return true;
    }),
    // The identity block's actions control opens its menu on the issue's own address. Its first
    // check says the control is there before it reaches for it: a click waits for its element
    // with no deadline of its own (Playwright's `actionTimeout` is unset here), so a page
    // drawing no control would spend this row's whole deadline inside one click and report a
    // timeout; asserted first, such a page says which control is missing.
    async () => {
      await gotoIssue(page, `/issues/${key}`);
      const actions = offered(page, "button", CONTROL_LABELS.issue_actions);
      await expect(
        actions,
        `the page draws no ${CONTROL_LABELS.issue_actions} control: its view is unreadable`
      ).toHaveCount(1);
      await actions.click();
      return true;
    },
    // The priority row's picker (dispatch://INFRA-2947 R3), which Details' control opens and no
    // address does: the old page drew that row in its header's rail and its recording opened the
    // picker there (`record-page-inventory.e2e.ts`), and this page draws it in the Details
    // panel's Development section (R13), which a reader opens first (R14). A closed issue's
    // control is refused, so there the view is Details with the menus closed, as the views below
    // are where what they open is already open.
    async () => {
      await closeOpenControls(page);
      await openDetails(page);
      await expect(
        editPriorities(page),
        `the page draws no ${CONTROL_LABELS.edit_priorities} control: its view is unreadable`
      ).toHaveCount(1);
      // No write of the row is out, so only the issue's closed state refuses the control, and the
      // page read that state before Details opened: one read says whether it is refused.
      if (await editPriorities(page).isEnabled()) {
        await editPriorities(page).click();
        await expect(prioritiesPicker(page)).toBeVisible();
      }
      return true;
    },
    // The repository's box, which Details draws in place of the repository's text once the text's
    // button is pressed, with its clear control (`RepoControl.tsx`'s `inRow`). A closed issue's
    // button is refused, so there the view is Details as the views around it leave it, which
    // draws the clear control disabled beside the wrapped text (`clearWhenClosed`).
    async () => {
      await closeOpenControls(page);
      await openDetails(page);
      const text = marginShell(page).getByRole("button", {
        name: new RegExp(`, Repository of ${key}$`),
      });
      await expect(text, "Details draws no button for the repository's text").toHaveCount(1);
      // The page read the issue's state before Details opened, so one read says whether the
      // button is refused, as it is on a closed issue.
      if (await text.isEnabled()) {
        await text.click();
        await expect(offered(page, "combobox", `Repository of ${key}`)).toBeVisible();
      }
      return true;
    },
    // The preset row below 1280 px is one chooser rather than a row of tabs, and the panel is
    // Details, which a reader opens from the header at every width (R14): each is a view of the
    // page, and at a width where it is already open the view is the page with the menus closed,
    // which is the page a reader is left on there.
    async () => {
      await closeOpenControls(page);
      // The view row is drawn with the page an earlier view loaded, in its shape for a width
      // query read as it renders, so one count says whether this width draws the chooser.
      if ((await presetChooser(page).count()) === 1) await presetChooser(page).click();
      return true;
    },
    async () => {
      await closeOpenControls(page);
      await openDetails(page);
      return true;
    },
    // One view per row of the tree, because a tree opens one body at a time and a node's own
    // text, its facts and its controls are in that body (R2). The walk loads the page once and
    // then moves through the rows, which is how a reader moves through them: each view is still
    // read, and still fitted, on its own, and a row after the first is read alone, since the
    // focus draws what it opens inside the row.
    //
    // At a width that draws the whole tree the walk opens the branches the page opened folded
    // first (R1): a node behind a fold - the check under a requirement - is a twisty away for a
    // reader, and a walk that never pressed one would read it as lost. Below 1024 px the zoom walk
    // below goes into a branch with the row's own key instead.
    ...Array.from({ length: walkDepth }, (_unused, index) =>
      index === 0
        ? async () => {
            // The tree is drawn empty and fills as the reads land, so it is read once the page
            // has stopped reading, as `unfoldEveryBranch` asks of its caller.
            const settling = watchApiRequests(page);
            await gotoIssue(page, `/issues/${key}/conversation`);
            await expectLoadSettled(settling);
            if (width >= 1024) await unfoldEveryBranch(page);
            const first = nodeRows(page);
            // The page's reads went quiet above, so the tree is drawn whole and one count says
            // whether it draws a row.
            if ((await first.count()) > 0) await first.first().focus();
            return true;
          }
        : rowView(index)
    ),
    // Below 1024 px the tree is read one node at a time (R16), so what a reader reaches by
    // zooming the walk reaches by zooming too, after the presets' own rows above: one zoom per
    // row of the issue's own address, because a node that hangs off a row - the document a
    // requirement cites - is behind that row's own twisty and nowhere else. At a width that
    // draws the whole tree the views above already open every row, so the walk does not run
    // there. It loads its page once, as the walk above does, and leaves each zoom by the issue's
    // crumb, as a reader does.
    ...(width >= 1024
      ? []
      : Array.from({ length: walkDepth }, (_unused, into) => [
          async () => {
            if (into === 0) {
              // Read off a page that has stopped reading: the tree is drawn empty and fills as
              // the reads land (`expectLoadSettled`), so a set taken the moment the navigation
              // returns holds the twisties of a half-filled tree, and a row missed here is a
              // node this state never zooms into. Measured on a loaded box at 390 px: a
              // document's own copy control read as lost on the attempt and was found on the
              // retry. A row with no branch of its own has no zoom to walk into, and this walk
              // does not read the page for it: which rows hold one is read once, here, and a row
              // the tree does not draw is not in it either.
              const settling = watchApiRequests(page);
              await gotoIssue(page, `/issues/${key}/conversation`);
              await expectLoadSettled(settling);
              zoomOpen = false;
              const rows = nodeRows(page);
              // The page's reads went quiet above, so the tree is drawn whole and one count
              // holds its rows.
              const drawn = Math.min(await rows.count(), walkDepth);
              for (let at = 0; at < drawn; at += 1) {
                const fold = rows.nth(at).getByRole("button", { name: CONTROL_LABELS.unfold });
                // The tree was drawn whole above, and a row draws its twisty with it, so one
                // count says whether this row has one.
                if ((await fold.count()) > 0) zoomsInto.add(at);
              }
              // The walk's first view loaded the page, which is read as it stands where its row
              // holds no zoom.
              if (!zoomsInto.has(into)) return true;
            } else if (!zoomsInto.has(into)) {
              return false;
            } else if (zoomOpen) {
              await leaveZoom();
            }
            // A twisty opens a branch in place at every width, and zooming is the row's own key
            // (`.`, `IssueTree.tsx`), pressed with the focus on the row.
            await nodeRows(page).nth(into).focus();
            await page.keyboard.press(".");
            await expect(zoomCrumbs(page), `row ${into}'s zoom key zoomed nowhere`).not.toHaveCount(
              0
            );
            zoomOpen = true;
            // The zoomed node is the first row; its own nodes follow it, and the first of them -
            // a check under the row's requirement - is read by the view that opened the zoom.
            const under = nodeRows(page);
            // The zoom was waited for above, and the focus alone moved since, which draws no
            // row, so one count holds the zoom's rows.
            if ((await under.count()) > 1) await under.nth(1).focus();
            return true;
          },
          // The second of the zoom's own nodes, read alone, where there was a zoom to open.
          async () => (zoomsInto.has(into) ? rowView(2)() : false),
        ]).flat()),
  ];
}

/** Opens Documents' list from `path`, the way a reader does at any width: the issue's page, then
 *  the view row's Documents. */
async function openDocumentsFrom(page: Page, path: string): Promise<void> {
  await gotoIssue(page, path);
  await openDocuments(page);
}

/**
 * One row of "the new page carries every element of the old one" (R7, c-inventory): the fixture's
 * `state` read against the new page at that state's width, and then the writes of that state
 * (`writes`), each made through the page and read back from Dispatch (c-nothing-lost). The
 * fixture was recorded in a desktop browser, so the 1440 px rows run in the desktop project alone
 * - an iPhone profile at 1440 px draws a different shell around the same page - and the 390 px
 * rows, which carry `@phone`, run in both.
 */
async function carryEveryElement(
  browser: Browser,
  state: InventoryState,
  writes: (page: Page, seeded: AdvancingIssueSeed, specName: string) => Promise<void>
): Promise<void> {
  const fixture = JSON.parse(
    await readFile(
      fileURLToPath(new URL("./fixtures/issue-page-inventory.json", import.meta.url)),
      "utf8"
    )
  ) as PageInventoryFixture;
  test.info().annotations.push({ description: fixture.commit, type: "fixture recorded at" });
  const size = state.endsWith("-1440") ? DESKTOP : PHONE;
  await phase(browser, seedWholeIssueAdvancing, async (page, seeded) => {
    // The documents' names, which their rows in Documents' list carry.
    const documents = {
      plan: (await getArtifact(seeded.planArtifactId)).name,
      spec: (await getArtifact(seeded.specArtifactId)).name,
      verification: (await getArtifact(seeded.verificationArtifactId)).name,
    };
    const specName = documents.spec;
    await page.setViewportSize({ height: size.height, width: size.width });
    if (state.startsWith("closed")) {
      await patchIssue(seeded.key, { status: "done" });
      // Closing settles the issue's documents: Dispatch saves the spec's open question resolved,
      // as the spec's next version (`docs/settlement.go`), and the old page was recorded after it,
      // so the fixture's closed states hold "Version 3" and its open states do not. The settlement
      // the seed's last write armed runs about 17 s after it where the spec's room is open, as a
      // page that shows the spec opens it; where no one has the room open it waits for the
      // resumption pass, a minute or two on (`docs/resume.go`), which made the row's wait
      // unsteady. So the page opens the spec first, and the row reads once the version exists, as
      // the recording did, so the page's own read of the versions finds the same three.
      await gotoIssue(page, presetPath(seeded.key, "spec"));
      await expect
        .poll(async () => ((await getArtifact(seeded.specArtifactId)).versions ?? []).length, {
          message: "Dispatch wrote no settlement version of the closed issue's spec",
          timeout: 180_000,
        })
        .toBe(3);
    }
    await assertInventoryCarried(page, fixture[state], {
      awaiting: [...AWAITING_THE_BOX, ...AWAITING_THREADS],
      outline: inventoryOutline(page, seeded.key, size.width),
      parentRows: INVENTORY_PARENT_ROWS,
      renames: INVENTORY_RENAMES,
      rows: INVENTORY_ROWS,
      state,
      views: inventoryViews(page, seeded.key, documents, size.width),
    });
    await gotoIssue(page, `/issues/${seeded.key}/conversation`);
    await expectIssuePageDrawn(page);
    if (state.startsWith("closed")) await expectClosedClear(page, seeded.key);
    await writes(page, seeded, specName);
  });
}

/**
 * A closed issue's own repository, read in Details (R7): today's page draws its Clear control
 * disabled, and Details draws the same one beside the wrapped value, under the recorded name. The
 * name is read exactly, there is one, and it is disabled at the width the state is read at, so a
 * Details that draws none, draws two, or draws it enabled fails here, where the inventory's own
 * read finds a name and not its state.
 */
async function expectClosedClear(page: Page, key: string): Promise<void> {
  await openDetails(page);
  const clear = page.getByRole("button", { exact: true, name: `Clear the repository of ${key}` });
  await expect(clear, `Details draws one Clear of ${key}'s repository`).toHaveCount(1);
  await expect(clear, "the Clear of a closed issue is disabled").toBeDisabled();
  await closeDetails(page);
}

/**
 * The writes of a closed issue (c-nothing-lost): what its page still takes - the reader's own pin
 * and their subscribers' Unsubscribe, which are no write of the issue - and Reopen, which only a
 * closed issue draws. Close, Reopen and the parent editor are behind the identity block's one
 * actions control now (R11), so each is reached through it.
 */
async function writeFromAClosedIssue(page: Page, seeded: AdvancingIssueSeed): Promise<void> {
  // The pin is the overflow menu's.
  await openIssueActions(page);
  await offered(page, "button", "Unpin issue").click();
  await expect.poll(async () => (await getMyState("alice"))[seeded.key]?.pinned).toBe(false);

  await openDetails(page);
  await offered(page, "button", "Subscribers: 3").click();
  await page
    .getByRole("button", { name: /Unsubscribe/ })
    .first()
    .click();
  // Unsubscribing asks first: Confirm is the only path that removes a subscriber
  // (`UnsubscribeDialog.tsx`).
  await page
    .getByRole("dialog", { name: "Unsubscribe" })
    .getByRole("button", { name: "Confirm" })
    .click();
  await expect.poll(async () => (await getUnsubscribeCalls()).length).toBeGreaterThan(0);
  await closeDetails(page);

  await openIssueActions(page);
  await offered(page, "button", CONTROL_LABELS.reopen_into_backlog).click();
  await expect.poll(async () => (await getIssue(seeded.key)).status).not.toBe("done");
}

/**
 * The writes of an open issue (c-nothing-lost): each recorded name that writes today still writes,
 * made through the page and the value read back from Dispatch. Close, Reopen and the parent
 * editor are behind the identity block's one actions control now (R11), so each is reached
 * through it; the upload and New document are Documents' list's (R2), and a document's text is
 * edited in the document opened from it (R3).
 */
async function writeFromAnOpenIssue(
  page: Page,
  seeded: AdvancingIssueSeed,
  specName: string
): Promise<void> {
  // Backlog and not Todo: the issue is in progress under a parent in Triage, and a move to
  // another status of work stops at the dialog that asks for the parents to be moved too
  // (`ParentStatusDialog.tsx`), which no write of c-nothing-lost goes through.
  await identityBlock(page).getByRole("combobox", { name: "Status" }).selectOption("backlog");
  await expect.poll(async () => (await getIssue(seeded.key)).status).toBe("backlog");

  await offered(page, "combobox", `Priority of ${seeded.key}`).selectOption("3");
  await expect.poll(async () => (await getIssue(seeded.key)).priority).toBe(3);

  // A picker whose list is read when the reader reaches for it - the people and the
  // repositories (`AssigneeControl.tsx`, `RepoControl.tsx`) - is reached for first, as a reader
  // reaches for it; without that it offers the value it already holds and nothing else.
  const assignee = offered(page, "combobox", `Assignee of ${seeded.key}`);
  await assignee.focus();
  await expect(assignee.locator('option[value="bob"]')).toHaveCount(1);
  await assignee.selectOption("bob");
  await expect.poll(async () => (await getIssue(seeded.key)).assignee).toBe("bob");
  // The issue goes back to the reader: an approval request waits on the issue's assignee, and the
  // header draws the chip's two actions only for the reader it waits on (`waitingOnReader`).
  await assignee.selectOption("alice");
  await expect.poll(async () => (await getIssue(seeded.key)).assignee).toBe("alice");

  // The labels' control is Details' Labels row; the header draws the labels that fit (R13). The
  // assignee's select still holds the focus and takes Shift+M as the letter it types, so Details
  // opens from the header's own control, as a reader's press opens it.
  await offered(page, "button", CONTROL_LABELS.details).click();
  await expect(marginShell(page), "Details did not open from its control").toBeVisible();
  await offered(page, "button", "Edit labels").click();
  await page.getByRole("combobox", { name: "Search or create label" }).fill("carried");
  await page
    .getByRole("option", { name: /carried/ })
    .first()
    .click();
  await page.keyboard.press("Escape");
  await expect.poll(async () => (await getIssue(seeded.key)).labels).toContain("carried");
  await closeDetails(page);

  // The parent's editor opens in Details' Parent row, from the header's overflow menu.
  await openIssueActions(page);
  await offered(page, "button", "Edit parent issue").click();
  await page.getByRole("textbox", { name: "Parent" }).fill("");
  await offered(page, "button", CONTROL_LABELS.save_parent).click();
  await expect.poll(async () => (await getIssue(seeded.key)).parent).toBeNull();

  // The route and its editor are Details' Route row (R13), which the parent left open.
  await offered(page, "button", `Messages default to ${PLANNER_TITLE}`).click();
  const route = page.getByRole("textbox", { name: "Messages go to" });
  await route.fill("role:reviewer");
  await offered(page, "button", CONTROL_LABELS.save_route).click();
  await expect.poll(async () => (await getIssue(seeded.key)).route).toBe("role:reviewer");

  // The repository and every property under it are in Details (R13, R14). The repository is the
  // row's text, whose button draws the box in its place, and what is typed in the box is written
  // when Enter is pressed (`RepoControl.tsx`).
  await openDetails(page);
  const repoBefore = (await getIssue(seeded.key)).repo;
  await offered(page, "button", `${repoBefore}, Repository of ${seeded.key}`).click();
  const repository = offered(page, "combobox", `Repository of ${seeded.key}`);
  await repository.fill("acme/carried");
  await repository.press("Enter");
  await expect.poll(async () => (await getIssue(seeded.key)).repo).toBe("acme/carried");

  await offered(page, "button", `Edit components of ${seeded.key}`).click();
  await page.getByRole("option", { name: "Issue panel" }).first().click();
  await page.keyboard.press("Escape");
  await expect.poll(async () => (await getIssue(seeded.key)).components.ids).not.toContain("panel");
  // The header's own controls are written with Details closed, which on a phone covers them.
  await closeDetails(page);

  // The pin is the overflow menu's.
  await openIssueActions(page);
  await offered(page, "button", "Unpin issue").click();
  await expect.poll(async () => (await getMyState("alice"))[seeded.key]?.pinned).toBe(false);

  // The claim and its Release are Details' (R13).
  await openDetails(page);
  await expect(offered(page, "button", `Release the claim on ${seeded.key}`)).toBeVisible();
  await offered(page, "button", `Release the claim on ${seeded.key}`).click();
  await expect.poll(async () => (await getIssue(seeded.key)).claim).toBeNull();
  await closeDetails(page);

  await openIssueActions(page);
  await offered(page, "button", "Close issue").click();
  // The issue holds open children, so closing it asks first (`ParentStatusDialog.tsx`): Move them
  // with it closes the children too, and the issue's own status is read back from Dispatch.
  await page
    .getByRole("dialog", { name: `Move ${seeded.key} to Done?` })
    .getByRole("button", { name: "Move them with it" })
    .click();
  await expect.poll(async () => (await getIssue(seeded.key)).status).toBe("done");
  await openIssueActions(page);
  await offered(page, "button", CONTROL_LABELS.reopen_into_backlog).click();
  await expect.poll(async () => (await getIssue(seeded.key)).status).not.toBe("done");

  await openDetails(page);
  await offered(page, "button", "Subscribers: 3").click();
  await page
    .getByRole("button", { name: /Unsubscribe/ })
    .first()
    .click();
  // Unsubscribing asks first: Confirm is the only path that removes a subscriber
  // (`UnsubscribeDialog.tsx`).
  await page
    .getByRole("dialog", { name: "Unsubscribe" })
    .getByRole("button", { name: "Confirm" })
    .click();
  await expect.poll(async () => (await getUnsubscribeCalls()).length).toBeGreaterThan(0);

  // The approval: the seed requests none, so the planner requests it here, and it waits on the
  // reader, who approves it from the page.
  await requestApproval(seeded.specArtifactId, { summary: "Read it back." }, asPlanner);
  await openPreset(page, seeded.key, "spec");
  // Approve is the identity block's own chip, drawn with its two actions where the request waits
  // on the reader (`IssuePropertiesRow.tsx`); the preset's toolbar draws how the document stands
  // and no action (`features/doc/ApprovalChip.tsx`'s `showActions`).
  await page
    .getByTestId("issue-state-actions")
    .getByRole("button", { name: /^Approve( v\d+)?$/ })
    .click();
  await expect
    .poll(async () => (await getArtifact(seeded.specArtifactId)).approval?.state)
    .toBe("approved");

  // Keep and Change, on the decision node they moved to. Both are named by the inventory's own
  // words, so they are still read by role and name - but scoped to one decision row. `Keep` is
  // drawn only while a decision waits for review and `Change` wherever the decision's quote has
  // resolved (`DecisionActions.tsx`), so a page whose second decision has its quote offers two
  // `Change` controls and one `Keep`, and an unscoped read of `Change` is ambiguous rather than
  // wrong. The row that offers `Keep` is the decision this phase is about.
  await openPreset(page, seeded.key, "decisions");
  const deciding = page
    .locator('[data-node-row][data-node-type="decision"]')
    .filter({ has: offered(page, "button", CONTROL_LABELS.keep_decision) });
  // `getAttribute` waits for a decision row that draws Keep, and the seed leaves a decision
  // waiting for review, so the row it finds is a decision that waits.
  const decidingId = await deciding.first().getAttribute("data-node-id");
  expect(decidingId, "the decision row that draws Keep carries no node id").not.toBeNull();
  const decidingRow = page.locator(`[data-node-row][data-node-id="${decidingId}"]`);
  const decidingBlock = decidingId?.split("#b-")[1];
  await decidingRow
    .getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision })
    .click();
  // Keep's own write, read back from Dispatch, as every write of this walk is: the press is what
  // this step proves, so a press the page lost fails here rather than leaving the decision
  // waiting while the step goes on to Change (`NodeRow.tsx`, the press that scrolled).
  await expect
    .poll(
      async () =>
        ((await getIssueDecisions(seeded.key)).decisions ?? []).find(
          (decision) => decision.block_id === decidingBlock
        )?.kept ?? null,
      { message: "Keep was pressed and Dispatch recorded no Keep of that decision" }
    )
    .not.toBeNull();
  // The page has taken the write once Keep is gone from the row. The row Keep settled draws no
  // Change on this page, and other decisions wait for review, so Change is pressed on another
  // decision row that draws it: the first row of the page with a Change in it.
  await expect(
    decidingRow.getByRole("button", { exact: true, name: CONTROL_LABELS.keep_decision })
  ).toHaveCount(0);
  await page
    .locator('[data-node-row][data-node-type="decision"]')
    .filter({
      has: page.getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision }),
    })
    .first()
    .getByRole("button", { exact: true, name: CONTROL_LABELS.change_decision })
    .click();
  // The change is written in the decision's own composer, which names its box and its send apart
  // from the conversation's (`DecisionActions.tsx`, R7).
  await page
    .getByRole("textbox", { name: CONTROL_LABELS.your_change })
    .fill("Change this decision.");
  await offered(page, "button", CONTROL_LABELS.send_change).click();
  await expect
    .poll(async () => (await listComments(seeded.key)).map((comment) => comment.body))
    .toContain("Change this decision.");

  // The upload and New document are Documents' list's (R2). A file picked is staged behind its
  // summary field until its own Upload confirms it (`ArtifactUploadRow` in
  // `features/artifacts/ArtifactUpload.tsx`), as on every page that uploads an issue's file.
  await openDocumentsFrom(page, `/issues/${seeded.key}`);
  await uploadInDocuments(page, {
    buffer: Buffer.from("# Carried\n"),
    mimeType: "text/markdown",
    name: "carried.md",
  });
  await expect
    .poll(async () => ((await getIssue(seeded.key)).artifacts ?? []).map((one) => one.name))
    .toContain("carried.md");
  await documentsList(page)
    .getByRole("button", { exact: true, name: CONTROL_LABELS.new_document })
    .click();
  await documentsList(page)
    .getByRole("textbox", { exact: true, name: "Title" })
    .fill("Carried new");
  await documentsList(page).getByRole("button", { exact: true, name: "Create" }).click();
  await expect
    .poll(async () =>
      ((await getIssue(seeded.key)).artifacts ?? []).some((one) => one.name.includes("Carried new"))
    )
    .toBe(true);

  // No write through the composer: on the new page it is part L's box, which follows this part
  // (part N's R11) and is in `AWAITING_THE_BOX`. Part L's pull request writes a comment in the
  // box here and reads it back. Nor is a message to an agent written: today's way is the
  // composer with an @ mention, and the new page's is Threads' New thread, which follows this part
  // too (`AWAITING_THREADS`).

  // Back to Conversation's address, where the tree opens on the spec's nodes.
  await gotoIssue(page, `/issues/${seeded.key}/conversation`);
  const specSlug = (await getArtifact(seeded.specArtifactId)).slug;

  await nodeRow(page, blockNodeId(specSlug, requirementBlockId(3)))
    .first()
    .focus();
  await page.keyboard.press("Enter");
  // The words go where the caret is, so they wait for it to land in the row's own text: typed
  // before it does, the space and the capital reach the page's own keys and the rest of the words
  // land in whatever those keys opened.
  await expect
    .poll(() => caretInNode(page), { message: "Enter did not put the caret in requirement 3" })
    .toBe(true);
  await page.keyboard.type(" Edited in place.");
  await expect
    .poll(async () => (await getIssueArtifactText(seeded.key, specSlug)).markdown)
    .toContain("Edited in place.");

  // Naming a version, from the version control beside the filters, which the Spec filter draws
  // reading the spec; with no filter on none is drawn (R2).
  await page.keyboard.press("Escape");
  await pickPreset(page, PRESET_NAMES.spec);
  await openVersionControl(page);
  await versionControl(page).getByRole("button", { name: "Name version" }).click();
  const naming = page.getByRole("dialog", { name: "Name this version" });
  await naming.getByRole("textbox", { name: "What changed?" }).fill("Carried over");
  await naming.getByRole("button", { name: /^Save/ }).click();
  await expect
    .poll(async () =>
      ((await getArtifact(seeded.specArtifactId)).versions ?? []).map((one) => one.summary)
    )
    .toContain("Carried over");

  // Editing a document's text in the document opened from Documents' list, in place of the
  // outline, with its own editor.
  await openDocumentsFrom(page, `/issues/${seeded.key}`);
  await documentLink(page, specName).click();
  // The caret goes where a reader's click after a requirement's own words puts it: text the
  // document holds as editable, inside the requirement's block.
  await placeCaret(page, "after", requirementText(2), openedEditor(page));
  await page.keyboard.type(" Edited where it was opened.");
  await expect
    .poll(async () => (await getIssueArtifactText(seeded.key, specSlug)).markdown)
    .toContain("Edited where it was opened.");

  // The open question is answered where the new page offers an answer today: Peek, which the
  // navigation drawer carries (R7), opens the Inbox drawer over the issue, with the same ask's own
  // card, whose option and Answer button write it. The outline's row writes nothing yet: its
  // controls set the line at the foot of the outline (`slots/foot.tsx`, part L's box), which
  // draws nothing while L follows, so the answer is not a write of the outline and this step does
  // not read it as one. What it reads is Dispatch's own record: the ask answered with the option
  // picked, and the issue's address as Peek found it.
  await gotoIssue(page, `/issues/${seeded.key}`);
  const address = page.url();
  await page.getByRole("button", { exact: true, name: "Open navigation" }).click();
  await page
    .getByRole("dialog", { exact: true, name: "Navigation" })
    .getByRole("button", { exact: true, name: "Peek at the inbox without leaving this page" })
    .click();
  const card = page
    .getByRole("dialog", { exact: true, name: "Inbox" })
    .getByTestId(`ask-${seeded.openQuestion.askId}`);
  const option = card.getByRole("radio").first();
  await option.check();
  await card.getByRole("button", { name: /^Answer/ }).click();
  await expect
    .poll(async () => (await getAsk(seeded.openQuestion.askId)).ask.state)
    .toBe("answered");
  const answered = (await getAsk(seeded.openQuestion.askId)).ask;
  expect(answered.answer?.selected, "the answer stores the option that was picked").toEqual([
    answered.options?.[0]?.label,
  ]);
  expect(page.url(), "answering in the Inbox drawer changed the issue's address").toBe(address);
}

// c-nothing-lost, row by row of the fixture's four states (R7). Each row loads the page once for
// each address it reads and walks each tree on one load, reading a view whole, settled over two
// readings of its accessibility tree, and a row the walk only moved the focus to alone, read
// once; then it makes its state's writes. The old page's one row held all four states and
// measured 8.2 minutes, which outlasts the browser step's budget in CI: each row of the four ends
// well inside it.
test("the new page carries every element of the old one: open at 1440 px", async ({ browser }) => {
  // Measured in an e2e-rows Job, one worker, alone: 1.5 min on chromium, which loads the page
  // 12 times and makes the open writes; the bound is four times that, for a slow leg.
  test.setTimeout(360_000);
  await carryEveryElement(browser, "open-1440", writeFromAnOpenIssue);
});

test("the new page carries every element of the old one: closed at 1440 px", async ({
  browser,
}) => {
  // Measured in an e2e-rows Job, one worker, alone: 2.5 min on chromium, which includes the wait
  // of about 17 s for the settlement version a close brings; the bound is four times that, for a
  // slow leg.
  test.setTimeout(600_000);
  await carryEveryElement(browser, "closed-1440", writeFromAClosedIssue);
});

test("the new page carries every element of the old one: open at 390 px @phone", async ({
  browser,
}) => {
  // Measured in an e2e-rows Job, one worker, alone: 2.6 min on chromium and 1.9 min on iphone,
  // which load the page about 18 times at 390 px and make the open writes; the bound is about
  // four times the longer, for a slow leg.
  test.setTimeout(600_000);
  await carryEveryElement(browser, "open-390", writeFromAnOpenIssue);
});

test("the new page carries every element of the old one: closed at 390 px @phone", async ({
  browser,
}) => {
  // Measured in an e2e-rows Job, one worker, alone: 3.0 min on chromium and 3.1 min on iphone,
  // which include the wait of about 17 s for the settlement version a close brings; the bound is
  // about four times the longer, for a slow leg.
  test.setTimeout(720_000);
  await carryEveryElement(browser, "closed-390", writeFromAClosedIssue);
});
