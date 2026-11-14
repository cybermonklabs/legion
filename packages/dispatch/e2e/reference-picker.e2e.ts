import {
  kindOrder,
  REF_PICKER_HEADINGS,
  REF_PICKER_INSTRUCTION,
  type RefPickerDoor,
} from "../web/src/features/refs/ref-picker";
import {
  commentField,
  highlightedRow,
  openRefPicker,
  pickerHeadings,
  pickerRows,
  refPicker,
} from "./ref-picker";
import {
  COMPONENT_TITLE,
  PERSON_LOGIN,
  ROLE_REFERENCE,
  ROLE_TOKEN,
  SESSION_REFERENCE,
  SESSION_TITLE,
  seedReferenceWorld,
} from "./reference-surfaces";
import { expect, test } from "./test";
import { asUser } from "./users";

/**
 * INFRA-1702's `c-picker-three-doors`: one picker behind `[[`, `@` and `#`.
 *
 * All three doors open the same list over the same rows, grouped by kind, each ranking the
 * kinds its own door names first (`kindOrder`); the arrows move, Enter and Tab take the row,
 * and what a pick writes lands at the caret with the words after it untouched. The rows the
 * server searches for are reached through `GET /api/v1/search`, which finds a node of another
 * project and refuses a query under two characters - so at no characters at all the picker
 * lists the kinds whose sources need no query, the parties and the components, with one row
 * saying what the next two letters will add.
 *
 * One box carries all of it: the issue Conversation's comment composer, which offers every
 * kind. That each of the other boxes opens the same doors over the same rows is
 * `writing-surfaces.e2e.ts`.
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
  const world = await seedReferenceWorld();
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
      PERSON_LOGIN,
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
      const list = await openRefPicker(field, `${door}cache`);
      await expect
        .poll(() => pickerRows(list), { message: `the ${name} door found no issue for "cache"` })
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
    const other = await openRefPicker(field, "#Cross-project");
    await other.getByRole("option", { name: world.other.title }).click();
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
  const world = await seedReferenceWorld();
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
    await expect(field).toHaveValue(`Ship after [${ROLE_TOKEN}](${ROLE_REFERENCE}) then deploy`);

    // Tab takes the highlighted row as Enter does, and moves no focus out of the box.
    await field.fill("");
    await field.pressSequentially("@Reference");
    await expect
      .poll(async () => (await pickerRows(list)).slice(0, 1))
      .toEqual([`Agents: ${SESSION_TITLE}`]);
    await field.press("Tab");
    await expect(field).toHaveValue(`[${SESSION_TITLE}](${SESSION_REFERENCE})`);
    await expect(field).toBeFocused();
  } finally {
    await alice.close();
  }
});
