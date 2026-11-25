import { createIssue, createProject, search as searchByApi } from "./api";
import { embeddedQueries, forgetEmbedCalls, setEmbedMarkers } from "./fake-embedder-helpers";
import { keyedSearchWord } from "./search";
import { expect, type Page, rowKey, test } from "./test";
import { asUser } from "./users";

// The bar's two tiers, read from the embeddings each costs (dispatch://INFRA-1042): a keystroke
// searches the words alone and embeds nothing, and only a pause pays for an embedding and gets
// the search by meaning. The fake embedder counts the calls the server makes
// (`e2e/fake-embedder.ts`), so each claim here is a number the row reads back rather than a
// stretch of time it waited out. A run whose server embeds nowhere starts no fake embedder, and
// `setEmbedMarkers` skips the row (`e2e/harness-embedder.ts`).
//
// Every word a row writes and types holds the row's own project key (`keyedSearchWord`,
// e2e/search.ts). The bar searches every project of the stack, and the worker empties its
// database before its first row and before the review corpus's seed, not before each row, so the
// issues an earlier row wrote - the other row of this file, or another repeat of this one - hold
// the same bare words and would be listed beside the row's own. The search by meaning is the
// row's own for the same reason: the fake embedder draws a group's vector from the group's words
// (`keyOf`, e2e/fake-embedder.ts), so the vectors stored for an earlier row's issues are nowhere
// near this row's query.

interface TiersFixture {
  /** The word typed, the row's own: two seeded issues hold it and the third does not. */
  readonly query: string;
  /** The issue the words of either query find. */
  readonly sightings: string;
  /** The issue the one-word query finds as well. */
  readonly brightness: string;
  /** The issue only the search by meaning finds. */
  readonly octant: string;
}

async function seedTiersFixture(): Promise<TiersFixture> {
  const core = rowKey("CORE");
  const query = keyedSearchWord("quasar", core);
  const octantWord = keyedSearchWord("octant", core);
  // What the fake embedder embeds as one vector: the word the query is and the word of the one
  // issue the query's words never match. The meaning leg then finds that issue and the keyword
  // leg never does, so the list a pause paints is visibly the fused one. Seeded before the
  // issues are written, since the queue embeds each as it picks it up.
  await setEmbedMarkers([[query, octantWord]]);
  await createProject({ key: core, name: "Core" });
  const titleWord = keyedSearchWord("Quasar", core);
  const sightings = await createIssue({ project: core, title: `${titleWord} sightings log` });
  const brightness = await createIssue({ project: core, title: `${titleWord} brightness notes` });
  const octant = await createIssue({
    project: core,
    title: `${keyedSearchWord("Octant", core)} calibration`,
  });
  return { brightness: brightness.key, octant: octant.key, query, sightings: sightings.key };
}

/** Opens the bar over the home page with the embedder's counter at zero. The seeded issues are
 *  embedded as the queue picks them up, so the row first waits for a search by meaning to find
 *  the issue no words of the query match - the one read that says those vectors are in place -
 *  and then forgets the calls that wait itself made. */
async function openBarOnFixture(page: Page, fixture: TiersFixture): Promise<void> {
  await expect
    .poll(
      async () =>
        ((await searchByApi(fixture.query)).results ?? []).map((hit) =>
          hit.owner.kind === "issue" ? hit.owner.key : ""
        ),
      { timeout: 60_000 }
    )
    .toContain(fixture.octant);
  await forgetEmbedCalls();
  await page.goto("/");
  await expect(page.getByRole("button", { name: /^search/i })).toBeVisible();
  await page.locator("body").focus();
  await page.keyboard.press("Control+k");
  await expect(page.getByRole("combobox", { name: "Search" })).toBeFocused();
}

// SLOW(>=2s): three seeded issues, the wait for the queue to embed them, and the two searches
// one query makes.
test("keystrokes embed nothing, and a pause embeds once and lists the hit words alone never find", async ({
  browser,
}) => {
  const fixture = await seedTiersFixture();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openBarOnFixture(page, fixture);
    const dialog = page.getByRole("dialog", { name: "Search" });
    const box = page.getByRole("combobox", { name: "Search" });
    const options = dialog.getByRole("option");

    // The burst: every keystroke lands inside the pause, so the only searches made are the
    // keystroke tier's, which ask the route for the keyword leg alone.
    await box.pressSequentially(fixture.query, { delay: 25 });
    expect(await embeddedQueries()).toEqual([]);
    await expect(options).toHaveCount(2);
    const highlighted = await box.getAttribute("aria-activedescendant");
    expect(highlighted).toMatch(/^search-option-issue-/);

    // The pause: one embedding, counted here rather than waited out, and the fused order it
    // answers replaces the list - the issue the query's words match nowhere is in it now.
    await expect.poll(embeddedQueries).toEqual([fixture.query]);
    await expect(options).toHaveCount(3);
    await expect(page.locator(`#search-option-issue-${fixture.octant}`)).toBeVisible();

    // The row the reader was looking at is still in the list, so it still holds the highlight,
    // and Enter opens that row and nothing else. Opening embeds nothing of its own.
    expect(await box.getAttribute("aria-activedescendant")).toBe(highlighted);
    await box.press("Enter");
    const opened = (highlighted ?? "").replace("search-option-issue-", "");
    await expect(page).toHaveURL(new RegExp(`/issues/${opened}$`));
    expect([fixture.sightings, fixture.brightness]).toContain(opened);
    expect(await embeddedQueries()).toEqual([fixture.query]);
  } finally {
    await context.close();
  }
});

// SLOW(>=2s): the same fixture, and a longer query typed a letter at a time.
test("a query typed faster than the pause embeds once however many letters it has", async ({
  browser,
}) => {
  const fixture = await seedTiersFixture();
  const context = await asUser(browser, "alice");

  try {
    const page = await context.newPage();
    await openBarOnFixture(page, fixture);
    const dialog = page.getByRole("dialog", { name: "Search" });
    const box = page.getByRole("combobox", { name: "Search" });

    // A keystroke per letter, each inside the pause: every one of them cancels the search by
    // meaning the keystroke before it was heading for, and only the last query is ever
    // embedded. Two words, so the words themselves find one issue of the three: the row's own
    // `sightings` issue is the one that holds both.
    const typed = `${fixture.query} sightings`;
    await box.pressSequentially(typed, { delay: 25 });
    expect(await embeddedQueries()).toEqual([]);
    await expect(dialog.getByRole("option")).toHaveCount(1);

    // Waiting for that one embedding is what makes the count exact: every keystroke before it
    // had its chance to embed and took none, so this stretch of typing cost one embedding and
    // not one per letter.
    await expect.poll(embeddedQueries).toEqual([typed]);
    await expect(page.locator(`#search-option-issue-${fixture.sightings}`)).toBeVisible();
    expect(await embeddedQueries()).toEqual([typed]);
  } finally {
    await context.close();
  }
});
