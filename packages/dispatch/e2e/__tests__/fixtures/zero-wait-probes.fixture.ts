/** The planted controls of e2e/__tests__/zero-wait-probes.test.ts: one probe of every shape the
 *  rule has an answer for, so proving the check looks needs no edit of a real spec. Without them
 *  a check that refuses nothing in the suite would read the same whether it looked or not.
 *
 * Its name ends `.fixture.ts` and not `.e2e.ts` because e2e/playwright.config.ts says
 * `testDir: "."` and `testMatch: /.*\.e2e\.ts/`: Playwright walks the `e2e` directory
 * recursively, so a `.e2e.ts` file here would join the real suite and run. `bun test` collects
 * only `*.test.ts` and its kin, so this file runs nowhere; the test reads its text and parses it.
 * `suiteFiles` skips this whole directory for the same reason: half the probes below are written
 * to be refused, and the rule is about the suite.
 *
 * No comment stands directly above a planted probe but the two whose length is what they plant,
 * because a comment there is what the rule reads as a reason.
 *
 * The lines are asserted in the test, so a probe added here goes at the END of the file.
 */

import { expect, type Locator, type Page, test } from "@playwright/test";

/** A wrapper: its own `return` hands on a probe, so a call of it is a probe where it is called.
 *  The probe it returns decides nothing here. */
async function menuShows(menu: Locator): Promise<boolean> {
  return menu.isVisible();
}

/** One probe per shape the rule refuses, in this order: an `if` on `isVisible`, a `const` read
 *  later in a ternary, the condition of `test.skip`, a `for` loop's bound, a wrapper called in an
 *  `if`, and an `if` under a comment of seven words. */
export async function plantedRefusals(page: Page, menu: Locator, rows: Locator): Promise<string> {
  if (!(await menu.isVisible())) return "closed";

  const shown = await menu.textContent();
  const label = shown === "Menu" ? "menu" : "other";

  test.skip((await rows.count()) === 0, "a planted skip decided by one count");

  for (let index = 0; index < (await rows.count()); index += 1) {
    await rows.nth(index).click();
  }

  if (await menuShows(menu)) await menu.click();

  // One read of this instant is enough.
  if ((await rows.count()) > 1) return label;

  return page.url();
}

/** One probe per shape, in this order: a ternary inside `expect.poll`, inside the subject of
 *  `expect(...).toPass` and inside `page.evaluate`, which pass; a `getAttribute` bound to a
 *  `const` whose `if` only throws and one in an `if` that only throws, refused as any `if` is; a
 *  probe handed to `expect(...)`, which passes; and an `if` under a reason of eight words, kept. */
export async function plantedMixed(page: Page, menu: Locator, rows: Locator): Promise<void> {
  await expect.poll(async () => ((await rows.count()) > 0 ? "some" : "none")).toBe("some");

  await expect(async () => {
    expect((await menu.inputValue()) === "" ? "empty" : "filled").toBe("filled");
  }).toPass();

  await page.evaluate(() => (document.body.getAttribute("data-ready") === "yes" ? 1 : 0));

  const state = await menu.getAttribute("data-state");
  if (state === null) throw new Error("the planted menu names no state");

  if ((await menu.getAttribute("aria-expanded")) !== "true") {
    throw new Error("the planted menu is not expanded");
  }

  expect(await rows.count()).toBe(2);

  // One read of this instant is enough here.
  if (await menu.isChecked()) await menu.uncheck();
}

/** Two loops over `Promise.all`, which is no probe: it waits for the promises it is handed and
 *  reads no page, though its member is named like `locator.all()`. One walks it in place, and one
 *  walks a `const` bound to it, as e2e/keyboard-inbox.e2e.ts walks the requests it awaits. */
export async function plantedPromiseAll(page: Page, rows: Locator): Promise<void> {
  for (const box of await Promise.all([rows.first().boundingBox(), rows.last().boundingBox()])) {
    expect(box).not.toBeNull();
  }

  const writes = Promise.all([page.waitForRequest("**/issues"), page.waitForRequest("**/asks")]);
  await rows.first().click();
  for (const request of await writes) expect(request.method()).toBe("POST");
}

/** One probe per shape of a branch that is no `if`, each refused, in this order: a `switch` on a
 *  `getAttribute`, a `switch` on a `const` bound to `textContent`, a statement whose `&&` runs a
 *  click on an `isVisible`, a chain of `&&` whose two operands before the last are probes, a name
 *  assigned an `isVisible` that a later `if` reads, and a name assigned in a loop's body that the
 *  loop's `while` above it reads. */
export async function plantedBranchRefusals(menu: Locator, rows: Locator): Promise<string> {
  switch (await menu.getAttribute("aria-expanded")) {
    case "true":
      return "open";
    default:
      break;
  }

  const label = await menu.textContent();
  switch (label) {
    case "Menu":
      return "menu";
    default:
      break;
  }

  (await menu.isVisible()) && (await menu.click());

  (await rows.count()) > 1 && (await menu.isChecked()) && (await menu.click());

  let shown = false;
  shown = await menu.isVisible();
  if (shown) await menu.click();

  let open = false;
  while (!open) {
    await menu.click();
    open = await menu.isVisible();
  }

  return "closed";
}

/** A probe that is only the last operand of a statement's `&&`: its answer is thrown away, so it
 *  decides nothing and is no instance. */
export async function plantedLastOperand(menu: Locator, open: boolean): Promise<void> {
  open && (await menu.isVisible());
}

/** One probe per place the grammar lets a value decide what runs next, each refused, in this
 *  order: an `if`, a `while`, a `do...while`, a `for`, a ternary, a `switch`, a `case`, a
 *  `for...of` over `all()`, a `for...in`, the left of a statement's `&&`, `||` and `??` whose right
 *  operand runs code, the target of an `||=` that reads a bound name, a `?.` followed by a call,
 *  and a destructuring whose default runs code. */
export async function plantedPositions(page: Page, menu: Locator, rows: Locator): Promise<string> {
  if (await menu.isVisible()) await menu.click();
  while (await menu.isHidden()) await page.keyboard.press("Escape");
  do {
    await menu.click();
  } while (!(await menu.isChecked()));
  for (let turn = 0; await menu.isEnabled(); turn += 1) await rows.nth(turn).click();
  const word = (await menu.isEditable()) ? "editable" : "fixed";
  switch (await rows.count()) {
    case 0:
      return word;
    default:
      break;
  }
  switch (word) {
    case await menu.textContent():
      return "named";
    default:
      break;
  }
  for (const row of await rows.all()) await row.click();
  for (const index in await rows.all()) await rows.nth(Number(index)).click();
  (await menu.isVisible()) && (await menu.click());
  (await menu.isChecked()) || (await menu.check());
  (await menu.getAttribute("href")) ?? (await menu.click());
  let opened = await menu.isVisible();
  opened ||= await menu.click().then(() => true);
  const relative = (await menu.getAttribute("href"))?.startsWith("/");
  const [first = rows.first()] = await rows.all();
  await first.click();
  return `${opened} ${relative}`;
}

/** The same three operators whose value a `return` hands on, each refused: the left operand still
 *  decides whether the right one runs. */
export async function plantedLogicalReturns(menu: Locator, pick: number): Promise<unknown> {
  if (pick === 0) return (await menu.isVisible()) && (await menu.click());
  if (pick === 1) return (await menu.isChecked()) || (await menu.check());
  return (await menu.textContent()) ?? (await menu.click());
}

/** One probe per way of binding its answer to a name, each read by a later `if` and refused, in
 *  this order: `const`, `let`, `=`, `+=`, `&&=`, `||=`, `??=`, an array and an object pattern
 *  declared, and an array and an object pattern assigned. */
export async function plantedBindings(menu: Locator, rows: Locator): Promise<void> {
  const visible = await menu.isVisible();
  if (visible) await menu.click();
  let enabled = await menu.isEnabled();
  if (enabled) enabled = false;
  let checked = false;
  checked = await menu.isChecked();
  if (checked) await menu.uncheck();
  let total = 0;
  total += await rows.count();
  if (total > 2) await rows.first().click();
  let ready = true;
  ready &&= await menu.isEditable();
  if (ready) await menu.fill("ready");
  let anyHidden = false;
  anyHidden ||= await menu.isHidden();
  if (anyHidden) await menu.hover();
  let text: string | null = null;
  text ??= await menu.textContent();
  if (text) await menu.focus();
  const [firstText] = await rows.allTextContents();
  if (firstText === "Menu") await menu.click();
  const { length } = await rows.all();
  if (length > 1) await rows.last().click();
  let head = rows.first();
  [head] = await rows.all();
  if (head) await head.click();
  let size = 0;
  ({ length: size } = await rows.all());
  if (size > 1) await rows.first().hover();
}

// biome-ignore-start lint/suspicious/noVar: a `var` binding is one of the shapes planted here
/** A probe's answer bound by `var`, by a parameter's default and by a pattern element's default,
 *  each read by a later `if` and refused. */
export async function plantedDeclarations(
  menu: Locator,
  options: { open?: boolean },
  shown = menu.isVisible()
): Promise<void> {
  var editable = await menu.isEditable();
  if (editable) await menu.fill("editable");
  if (await shown) await menu.click();
  const { open = await menu.isChecked() } = options;
  if (open) await menu.uncheck();
}
// biome-ignore-end lint/suspicious/noVar: a `var` binding is one of the shapes planted here

/** A name assigned in a loop's body and read by an `if` above it in that body, which a later turn
 *  runs after the assignment; and a name read by an `if` of a function nested in its own, written
 *  above the binding, which runs after it when called. Both refused. */
export async function plantedLaterReads(menu: Locator): Promise<void> {
  let seen = false;
  for (let turn = 0; turn < 3; turn += 1) {
    if (seen) break;
    seen = await menu.isVisible();
  }
  const act = async (): Promise<void> => {
    if (present) await menu.click();
  };
  const present = await menu.isEnabled();
  await act();
}

// biome-ignore-start lint/style/noNonNullAssertion: a non-null `!` is one of the planted climbs
/** One probe per step the climb takes on the way to its `if`, each refused, in this order:
 *  `.length`, `[0]`, arithmetic, a template literal, `as` and a non-null `!`. */
export async function plantedClimbs(menu: Locator, rows: Locator): Promise<void> {
  if ((await rows.all()).length > 1) await menu.click();
  if ((await rows.allTextContents())[0] === "Menu") await menu.click();
  if ((await rows.count()) - 1 > 0) await menu.click();
  if (`${await menu.textContent()}` === "Menu") await menu.click();
  if (((await menu.getAttribute("aria-expanded")) as string) === "true") await menu.click();
  if ((await menu.textContent())!.length > 0) await menu.click();
}
// biome-ignore-end lint/style/noNonNullAssertion: a non-null `!` is one of the planted climbs

// biome-ignore-start lint/suspicious/noAssignInExpressions: the assignment's value is planted
/** An assignment whose value is the condition of an `if`, and a closure that assigns a name its
 *  outer function declares and reads in a later `if`. Both refused. */
export async function plantedAssignedValues(menu: Locator): Promise<void> {
  let found = false;
  if ((found = await menu.isVisible())) await menu.click();
  let outer = false;
  const look = async (): Promise<void> => {
    outer = await menu.isChecked();
  };
  await look();
  if (outer || found) await menu.uncheck();
}
// biome-ignore-end lint/suspicious/noAssignInExpressions: the assignment's value is planted

/** One probe per kind of data flow the scan does not follow, each a pass, in this order: an answer
 *  passed into a function that branches on it, a method called on the answer, an answer kept in a
 *  property and in an array element, a name copied into another name, an answer that `??` only
 *  replaces with a literal, and an answer returned. */
export async function plantedLimits(
  menu: Locator,
  rows: Locator,
  state: { shown: boolean }
): Promise<boolean> {
  await clickWhen(menu, await menu.isVisible());
  for (const text of (await rows.allTextContents()).slice(1)) expect(text).not.toBe("");
  state.shown = await menu.isChecked();
  if (state.shown) await menu.uncheck();
  const answers = [await menu.isEnabled()];
  if (answers[0]) await menu.click();
  const seen = await menu.isHidden();
  const copy = seen;
  if (copy) await menu.hover();
  const href = (await menu.getAttribute("href")) ?? "";
  expect(href).toBe("/");
  return (await rows.count()) > 1;
}

/** Clicks `menu` where `shown` holds: the function a probe's answer is passed into. */
async function clickWhen(menu: Locator, shown: boolean): Promise<void> {
  if (shown) await menu.click();
}
