import {
  kindOrder,
  REF_PICKER_HEADINGS,
  REF_PICKER_INSTRUCTION,
  REF_PICKER_WORDS,
  type RefPickerDoor,
} from "../web/src/features/refs/ref-picker";
import { setLiveSessions } from "./agents";
import { getArtifactText } from "./api";
import { documentEditor, openSpec, placeCaret } from "./editor";
import {
  commentField,
  highlightedRow,
  openRefPicker,
  pickerHeadings,
  pickerRows,
  refPicker,
  within,
} from "./ref-picker";
import {
  ANCHOR,
  COMPONENT_TITLE,
  ROLE_REFERENCE,
  ROLE_TOKEN,
  referencePerson,
  referenceSearchWord,
  SESSION_TITLE,
  seedReferenceWorld,
  sessionReference,
} from "./reference-surfaces";
import { expect, rowSessionId, test } from "./test";
import { asUser } from "./users";
import { noMatch } from "./writing-boxes";

/**
 * The picker's three doors: one picker behind `[[`, `@` and `#`.
 *
 * All three doors open the same list over the same rows, grouped by kind, each ranking the
 * kinds its own door names first (`kindOrder`); the arrows move, Enter and Tab take the row,
 * and what a pick writes lands at the caret with the words after it untouched - in a link's
 * address too, where a door opens on a trigger typed there and a pick writes the address alone.
 * The rows the server searches for are reached through `GET /api/v1/search`, which finds a node
 * of another project and refuses a query under two characters - so at no characters at all the
 * picker lists the kinds whose sources need no query, the parties and the components, with one
 * row saying what the next two letters will add.
 *
 * One box carries every row but the last: the issue Conversation's comment composer, which
 * offers every kind. That each of the other boxes opens the same doors over the same rows is
 * `writing-surfaces.e2e.ts`.
 *
 * The check's negative controls are rows here too. In the same comment composer, a space typed
 * before any row is taken closes the list and leaves the typed words, Escape closes it and
 * leaves the trigger, a query nothing answers shows the empty state and inserts nothing, and
 * Ctrl+K opens the search palette and no reference dialog. The last row types a heading as `#`
 * and a space in the document body, the one box whose editor makes those two characters a
 * heading (the commonmark preset's heading input rule, which the live editor installs), and
 * holds that it stays one.
 */

/** The headings this door ranks `present` in: the door's own kind order, restricted to the
 *  kinds the picker found rows for. */
function headingsFor(door: RefPickerDoor, present: readonly string[]): string[] {
  return kindOrder(door)
    .map((kind) => REF_PICKER_HEADINGS[kind])
    .filter((heading) => present.includes(heading));
}

// SLOW(>=2s): the one reference world - three projects, fifteen issues, the host's question
// blocks settling into asks, the architecture source sync and the live session - and then the
// picker opened at five queries, each open reading the registry, the issue's collaborators, the
// project's components and the search route.
test("the three doors open one list over the same rows, each ranking the kinds it names first", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const world = await seedReferenceWorld({ tagPickerSearch: true });
  const query = referenceSearchWord(world.projects.core);
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    // Every search the picker makes, whichever of the two paths the client picks for it: a
    // request carrying a member a released build does not serve goes to the results route
    // (`everyBuildReadsSearch`), and the picker's always names its door's kinds, so today
    // every one of them lands there. The row's claim is one search per query, not the path.
    const searched: string[] = [];
    page.on("request", (request) => {
      const url = new URL(request.url());
      if (url.pathname === "/api/v1/search" || url.pathname === "/api/v1/search/results") {
        searched.push(url.searchParams.get("q") ?? "");
      }
    });
    await page.goto(`/issues/${world.host.key}/conversation`);
    const field = commentField(page);

    // At no characters at all: the kinds whose sources take no query - the parties and the
    // project's components - and one row for the kinds the search route answers for.
    const parties = await openRefPicker(field, "@");
    await expect
      .poll(() => pickerHeadings(parties))
      .toEqual(["People", "Agents", "Roles", "Components"]);
    await expect(parties.getByTestId("ref-picker-instruction")).toHaveText(REF_PICKER_INSTRUCTION);
    // Each row is found by its own words: a role's line names the session holding it, so the
    // session's name alone would match that row too.
    for (const row of [
      referencePerson(),
      new RegExp(`^${SESSION_TITLE}\\b`),
      new RegExp(`^${ROLE_TOKEN}\\b`),
      new RegExp(`^${COMPONENT_TITLE}\\b`),
    ]) {
      await expect(parties.getByRole("option", { name: row })).toBeVisible();
    }
    // `#` and `[[` rank the kinds they have rows for in the base order, which puts the
    // components before the parties: `@` is the door that lifts the parties above them.
    for (const door of ["#", "[["]) {
      await field.fill("");
      const list = await openRefPicker(field, door);
      await expect
        .poll(() => pickerHeadings(list))
        .toEqual(["Components", "Agents", "Roles", "People"]);
    }

    // The search route refuses a query under two characters, so the picker asks it for none:
    // one character after the door sends nothing and leaves the instruction row standing.
    await field.fill("@c");
    await expect(parties.getByTestId("ref-picker-instruction")).toBeVisible();
    await field.fill("@ca");
    await expect.poll(() => searched).toEqual(["ca"]);

    // With a query every door lists the same rows, each in its own order: `#` lifts the issues
    // above everything, and the other two keep the base order.
    const opened: Record<string, { headings: string[]; rows: string[] }> = {};
    for (const [door, name] of [
      ["@", "parties"],
      ["#", "issues"],
      ["[[", "all"],
    ] as const) {
      await field.fill("");
      const list = await openRefPicker(field, `${door}${query}`);
      await expect
        .poll(() => pickerRows(list), { message: `the ${name} door found no issue for ${query}` })
        .toContain(`Issues: ${world.target.title}`);
      const headings = await pickerHeadings(list);
      opened[name] = { headings, rows: await pickerRows(list) };
      expect(headings, `the ${name} door's own ranking`).toEqual(headingsFor(name, headings));
    }
    expect(new Set(opened.issues?.rows)).toEqual(new Set(opened.parties?.rows));
    expect(new Set(opened.all?.rows)).toEqual(new Set(opened.parties?.rows));
    expect(opened.issues?.headings[0]).toBe("Issues");

    // A node of another project, found through the search route and written as its address.
    await field.fill("");
    const other = await openRefPicker(field, `#${query}`);
    // The query finds both projects of this row. The option detail includes the issue key.
    const otherRow = within(new RegExp(`^${world.other.title}`), world.other.key);
    await other.getByRole("option", { name: otherRow }).click();
    await expect(field).toHaveValue(`dispatch://${world.other.key}`);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the one reference world again, and the picker opened twice over a draft the row
// types a character at a time so the caret is where a reader's would be.
test("the arrows move, Enter and Tab take the row, and the pick lands at the caret", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const world = await seedReferenceWorld({ tagPickerSearch: true });
  const alice = await asUser(browser, "alice");

  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${world.host.key}/conversation`);
    const field = commentField(page);

    // The words after the caret are already written: the reader goes back to the front and
    // types the mention in the middle of the sentence.
    await field.fill(" then deploy");
    await field.press("Home");
    await field.pressSequentially("Ship after @Reference");
    const list = refPicker(page);
    await expect
      .poll(async () => (await pickerRows(list)).slice(0, 2))
      .toEqual([`Agents: ${SESSION_TITLE}`, `Roles: ${ROLE_TOKEN}`]);

    // The first row is the one Enter would take; the arrow moves to the next.
    await expect(highlightedRow(list)).toContainText(SESSION_TITLE);
    await field.press("ArrowDown");
    await expect(highlightedRow(list)).toContainText(ROLE_TOKEN);
    await field.press("Enter");
    await expect(field).toHaveValue(`Ship after [@${ROLE_TOKEN}](${ROLE_REFERENCE}) then deploy`);

    // Tab takes the highlighted row as Enter does, and moves no focus out of the box.
    await field.fill("");
    await field.pressSequentially("@Reference");
    await expect
      .poll(async () => (await pickerRows(list)).slice(0, 1))
      .toEqual([`Agents: ${SESSION_TITLE}`]);
    await field.press("Tab");
    await expect(field).toHaveValue(`[@${SESSION_TITLE}](${sessionReference()})`);
    await expect(field).toBeFocused();

    // A door opens in the address of a link the reader is writing, and the pick there is the
    // address alone, which completes that link. The query is a whole word: the search route
    // matches words, not the first letters of one.
    await field.fill("");
    await field.pressSequentially(`[the warm one](#${referenceSearchWord(world.projects.core)}`);
    // The option detail identifies the target's issue key among this row's matches.
    const targetRow = within(new RegExp(`^${world.target.title}`), world.target.key);
    await list.getByRole("option", { name: targetRow }).click();
    await expect(field).toHaveValue(`[the warm one](dispatch://${world.target.key}`);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the one reference world again, and the picker opened four times in the comment
// composer, three of them over a query the search route answers before the row goes on.
test("a space or Escape closes the picker over the typed words, a query nothing answers inserts nothing, and Ctrl+K opens the search palette", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const world = await seedReferenceWorld({ tagPickerSearch: true });
  const alice = await asUser(browser, "alice");

  // The four controls share one box, the Conversation's comment composer that the two rows
  // above drive, because what each one asserts is the shared field's: the list it closes, the
  // draft it leaves, and the key it hands on to the page.
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${world.host.key}/conversation`);
    const field = commentField(page);
    const list = refPicker(page);

    // A space typed before any row is taken closes the list, and every word typed stays: the
    // door, its query and the words after the space. The list has a row to take when the space
    // is typed, so the space is what declines it. The unit row is useRefPicker.test.tsx's "a
    // space typed before a row is taken closes the picker, leaving a heading a heading".
    const query = referenceSearchWord(world.projects.core);
    const answered = await openRefPicker(field, `@${query}`);
    await expect.poll(() => pickerRows(answered)).toContain(`Issues: ${world.target.title}`);
    await field.pressSequentially(" is warm");
    await expect(list).toHaveCount(0);
    await expect(field).toHaveValue(`@${query} is warm`);

    // Escape closes the list and leaves the trigger the reader typed, and the list stays shut
    // while the reader writes on after it. The unit rows are useRefPicker.test.tsx's "Escape
    // closes the picker and leaves the character the reader typed" and MentionField.test.tsx's
    // "an open picker takes the arrows, Enter, Tab and Escape ahead of the host".
    await field.fill("");
    await openRefPicker(field, "@ca");
    await field.press("Escape");
    await expect(list).toHaveCount(0);
    await expect(field).toHaveValue("@ca");
    await field.pressSequentially("che");
    await expect(field).toHaveValue("@cache");
    await expect(list).toHaveCount(0);

    // A query nothing answers shows the empty state and no row, so Enter takes nothing: the box
    // types its own line break, as Enter does in this composer, and no address is written. The
    // unit row is useRefPicker.test.tsx's "the list says it is loading, that nothing matches,
    // and that Dispatch is unreachable".
    await field.fill("");
    const empty = await openRefPicker(field, `[[${noMatch()}`);
    await expect(empty.getByTestId("ref-picker-empty")).toHaveText(REF_PICKER_WORDS.empty);
    await expect(empty.getByRole("option")).toHaveCount(0);
    await field.press("Enter");
    await expect(field).toHaveValue(`[[${noMatch()}\n`);
    await expect(list).toHaveCount(0);

    // Ctrl+K in the composer, with the list open, opens the search palette and no other dialog:
    // the build before this change opened a reference dialog of the composer's own on that key
    // instead. The key reaches the palette and types nothing into the draft, and the list
    // closes with the box the palette takes the focus from. The unit row is
    // surface-lists.test.ts's "no file imports a reader this change deleted", whose row "the
    // Ctrl+K reference dialog is imported nowhere" holds that the dialog's module is gone.
    await field.fill("");
    await openRefPicker(field, "@ca");
    await field.press("Control+k");
    await expect(page.getByRole("dialog", { name: "Search" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "Search" })).toBeFocused();
    await expect(page.getByRole("dialog")).toHaveCount(1);
    await expect(list).toHaveCount(0);
    await expect(field).toHaveValue("@ca");
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): the one reference world, and the host's spec opened in the live editor and
// connected before the row types a heading into it and reads it back from the stored text.
test("in the document body `#` and a space make a heading, and the picker the `#` opened closes", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const world = await seedReferenceWorld();
  const alice = await asUser(browser, "alice");

  // The document body is the box where `#` and a space make a heading: its editor converts a
  // block that starts with them into one. A textarea keeps the characters as they are typed,
  // which the row above already holds for a space. The unit row is useRefPicker.test.tsx's "a
  // space typed before a row is taken closes the picker, leaving a heading a heading", which
  // reads the characters in a textarea; no unit row types a heading into the editor.
  try {
    const page = await alice.newPage();
    await openSpec(page, world.host.key, ANCHOR);
    const editor = documentEditor(page);
    const list = refPicker(page);

    // A block of its own after the anchor paragraph, where `#` is both a door and the start of
    // a heading.
    await placeCaret(page, "after", ANCHOR);
    await page.keyboard.press("Enter");
    await page.keyboard.type("#");
    await expect(list).toBeVisible();

    // The space closes the list and reaches the editor, which makes the block a heading.
    await page.keyboard.type(" Release notes");
    await expect(list).toHaveCount(0);
    await expect(editor.getByRole("heading", { level: 1, name: "Release notes" })).toBeVisible();
    // The stored text holds the heading, and not a paragraph holding `\# Release notes`.
    await expect
      .poll(async () => (await getArtifactText(world.host.primary_artifact_id)).markdown, {
        timeout: 30_000,
      })
      .toContain(`${ANCHOR}\n\n# Release notes\n\n`);
  } finally {
    await alice.close();
  }
});

// SLOW(>=2s): seed the reference world, then open the issue composer and its live document.
test("a delivering comment picker omits non-routable sessions that the document can cite", async ({
  browser,
}) => {
  test.setTimeout(120_000);
  const world = await seedReferenceWorld();
  // The sessions are the row's own (`rowSessionId`). A `.` and a `:` are characters an address
  // may hold (`SESSION_ID_PATTERN`) and a delivery route may not (`sessionRouteIDPattern`,
  // `features/refs/routes.ts`), so the two ids no route reaches are the row's own id with one of
  // them in it.
  const dottedSession = `${rowSessionId("dotted")}.session`;
  const colonSession = `${rowSessionId("colon")}:session`;
  await setLiveSessions([
    { session_id: rowSessionId("routable"), title: "Route reachable" },
    { session_id: dottedSession, title: "Route dotted" },
    { session_id: colonSession, title: "Route colon" },
  ]);
  const alice = await asUser(browser, "alice");
  try {
    const page = await alice.newPage();
    await page.goto(`/issues/${world.host.key}/conversation`);
    const list = await openRefPicker(commentField(page), "@Route");
    await expect(list.getByRole("option", { name: /^Route reachable\b/u })).toBeVisible();
    await expect(list.getByRole("option", { name: /^Route (dotted|colon)\b/u })).toHaveCount(0);

    await openSpec(page, world.host.key, ANCHOR);
    await placeCaret(page, "after", ANCHOR);
    await page.keyboard.press("Enter");
    await page.keyboard.type("@Route");
    const documentList = refPicker(page);
    const dotted = documentList.getByRole("option", { name: /^Route dotted\b/u });
    await expect(dotted).toBeVisible();
    await expect(documentList.getByRole("option", { name: /^Route colon\b/u })).toBeVisible();
    await dotted.click();
    await expect
      .poll(async () => (await getArtifactText(world.host.primary_artifact_id)).markdown)
      .toContain(`dispatch://agent/${dottedSession}`);
  } finally {
    await alice.close();
  }
});
