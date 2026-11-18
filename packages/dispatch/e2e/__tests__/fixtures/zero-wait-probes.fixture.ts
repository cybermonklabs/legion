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

/** One probe per decision made where the scan does not read, each a pass, in this order: an
 *  answer passed into a function that branches on it, an answer handed to `expect`, a callback
 *  `forEach` runs once per element, an answer kept in a property, in an array literal's element
 *  and stored into an element, a name copied from a bound name and one computed from it, an
 *  answer `??` only replaces, a call's result returned and an answer returned. */
export async function plantedLimits(
  menu: Locator,
  rows: Locator,
  state: { shown: boolean },
  pick: boolean
): Promise<boolean> {
  await clickWhen(menu, await menu.isVisible());
  expect(await rows.count()).toBe(3);
  (await rows.all()).forEach((row) => void row.click());
  state.shown = await menu.isChecked();
  if (state.shown) await menu.uncheck();
  const answers = [await menu.isEnabled()];
  if (answers[0]) await menu.click();
  const stored: boolean[] = [];
  stored[0] = await menu.isDisabled();
  if (stored[0]) await menu.focus();
  const seen = await menu.isHidden();
  const copy = seen;
  if (copy) await menu.hover();
  const value = await menu.inputValue();
  const trimmed = value.trim();
  if (trimmed) await menu.fill(trimmed);
  const href = (await menu.getAttribute("href")) ?? "";
  expect(href).toBe("/");
  if (pick) return Boolean(await menu.isEditable());
  return (await rows.count()) > 1;
}

/** Clicks `menu` where `shown` holds: the function a probe's answer is passed into. */
async function clickWhen(menu: Locator, shown: boolean): Promise<void> {
  if (shown) await menu.click();
}

// biome-ignore-start lint/suspicious/noVar: a `var` its loop's condition reads is planted here
// biome-ignore-start lint/correctness/noInnerDeclarations: the planted `var` is a loop body's
// biome-ignore-start lint/correctness/noInvalidUseBeforeDeclaration: the condition reads it first
/** A name a loop's condition reads and a function declared below the loop assigns, which the loop
 *  calls; and a `var` declared in a loop's body that the loop's condition reads on the next turn.
 *  Both refused: a read anywhere in the name's scope counts, whatever the text writes first. */
export async function plantedReadsAnywhere(menu: Locator): Promise<void> {
  let seen = false;
  while (!seen) await look();
  async function look(): Promise<void> {
    seen = await menu.isVisible();
  }
  while (!done) {
    await menu.click();
    var done: boolean | undefined = await menu.isChecked();
  }
}
// biome-ignore-end lint/correctness/noInvalidUseBeforeDeclaration: the condition reads it first
// biome-ignore-end lint/correctness/noInnerDeclarations: the planted `var` is a loop body's
// biome-ignore-end lint/suspicious/noVar: a `var` its loop's condition reads is planted here

/** One name per way a position reads a name bound to an answer, in this order: walked through a
 *  method called on it, beside the same walk over the answer in place, the `.length` an `if`
 *  compares and the left of an `&&` an `if` reads. Each is refused as the same answer written in
 *  its place is: a call on the answer whose result decides reads it as an operator on it does. */
export async function plantedBoundReads(
  menu: Locator,
  rows: Locator,
  open: boolean
): Promise<void> {
  const items = await rows.all();
  for (const item of items.slice(1)) await item.click();
  for (const text of (await rows.allTextContents()).slice(1)) expect(text).not.toBe("");
  const texts = await rows.allTextContents();
  if (texts.length > 0) await menu.hover();
  const checked = await menu.isChecked();
  if (checked && open) await menu.uncheck();
}

// biome-ignore-start lint/complexity/noExtraBooleanCast: an answer passed into `Boolean` is planted
// biome-ignore-start lint/complexity/noUselessStringRaw: a tagged template on the answer is planted
/** One call on a probe's answer whose result decides an `if`, per form a call takes, each written
 *  twice and refused both times: on a name bound to the answer, then on the answer in place. In
 *  this order: a method called on it, an optional call on it, `Boolean` of it, an argument of a
 *  call, of a `new` and of a spread, a tagged template it is written into, and a key. */
export async function plantedCallRefusals(
  menu: Locator,
  rows: Locator,
  handlers: Record<string, boolean>
): Promise<void> {
  const value = await menu.inputValue();
  if (value.includes("B")) await menu.click();
  if ((await menu.inputValue()).includes("B")) await menu.click();
  const text = await menu.textContent();
  if (text?.includes("B")) await menu.click();
  if ((await menu.textContent())?.includes("B")) await menu.click();
  const shown = await menu.isVisible();
  if (Boolean(shown)) await menu.click();
  if (Boolean(await menu.isVisible())) await menu.click();
  const entered = await menu.inputValue();
  if (/B/.test(entered)) await menu.click();
  if (/B/.test(await menu.inputValue())) await menu.click();
  const texts = await rows.allTextContents();
  if (new Set(texts).has("B")) await menu.click();
  if (new Set(await rows.allTextContents()).has("B")) await menu.click();
  const labels = await rows.allTextContents();
  if ("".concat(...labels) === "B") await menu.click();
  if ("".concat(...(await rows.allTextContents())) === "B") await menu.click();
  const title = await menu.textContent();
  if (String.raw`${title}` === "B") await menu.click();
  if (String.raw`${await menu.textContent()}` === "B") await menu.click();
  const kind = await menu.inputValue();
  if (handlers[kind]) await menu.click();
  if (handlers[await menu.inputValue()]) await menu.click();
}
// biome-ignore-end lint/complexity/noUselessStringRaw: a tagged template on the answer is planted
// biome-ignore-end lint/complexity/noExtraBooleanCast: an answer passed into `Boolean` is planted

/** One read of the page's state per Locator read the rule names beside the ones above, each
 *  refused with no wait before it, in this order: a `for...of` over the answer of `evaluateAll`,
 *  of `evaluate` and of `boundingBox`, then an `if` on each of the three. */
export async function plantedEvaluateReads(
  page: Page,
  menu: Locator,
  rows: Locator
): Promise<void> {
  for (const id of await rows.evaluateAll((nodes) => nodes.map((node) => node.id))) {
    await page.locator(`#${id}`).click();
  }
  for (const title of await page.evaluate(() => [document.title, document.URL])) {
    expect(title).not.toBe("");
  }
  for (const edge of Object.values((await menu.boundingBox()) ?? {})) {
    expect(edge).toBeGreaterThanOrEqual(0);
  }
  if ((await rows.evaluateAll((nodes) => nodes.length)) > 1) await rows.first().click();
  if (await page.evaluate(() => document.hasFocus())) await menu.click();
  if ((await menu.boundingBox()) === null) throw new Error("the planted menu has no box");
}

/** A function literal passed to `evaluate` whose body branches on a read: it runs in the page,
 *  where the read is the DOM's own, so it is skipped, and the call's answer decides nothing. */
export async function plantedPageSideBranch(page: Page): Promise<void> {
  await page.evaluate(() => {
    if (document.body.getAttribute("data-ready") === "yes") document.body.dataset.seen = "yes";
  });
}

// biome-ignore-start lint/suspicious/noAssignInExpressions: the compound assignment is planted
/** A name bound to an answer that a compound assignment reads before it writes it: `-=` reads
 *  `n`, so the `while` its value stands in reads the answer and is refused. */
export async function plantedCompoundRead(rows: Locator): Promise<void> {
  let n = await rows.count();
  while ((n -= 1) >= 0) await rows.nth(n).click();
}
// biome-ignore-end lint/suspicious/noAssignInExpressions: the compound assignment is planted
