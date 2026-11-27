import { expect, type Locator, type Page } from "@playwright/test";

/** The element holding the keyboard focus in `page`, read once: its tag and its name as a reader
 *  hears it - its own `aria-label`, else its first label, else its placeholder, else the first 40
 *  characters of its text - and whether it takes typed text, as a `<textarea>`, a content-editable
 *  element and an `<input>` of a text-entry type do. */
export function focusHolder(page: Page): Promise<{ name: string; takesText: boolean }> {
  return page.evaluate(() => {
    const active = document.activeElement;
    const takesText =
      active instanceof HTMLTextAreaElement ||
      (active instanceof HTMLElement && active.isContentEditable) ||
      (active instanceof HTMLInputElement &&
        ["", "text", "search", "email", "url", "tel", "number", "password"].includes(
          active.getAttribute("type") ?? ""
        ));
    const field = active as HTMLTextAreaElement | HTMLInputElement | null;
    const named =
      active?.getAttribute("aria-label") ||
      field?.labels?.[0]?.textContent?.trim() ||
      active?.getAttribute("placeholder") ||
      active?.textContent?.trim().slice(0, 40);
    return { name: `${active?.tagName.toLowerCase()} "${named}"`, takesText };
  });
}

/** The element holding the focus in `page`, in words for a failure: the page's body, or its tag,
 *  its role, its accessible name as its label or its text gives it, its test id, and the Inbox
 *  row it is in. */
export function describeFocus(page: Page): Promise<string> {
  return page.evaluate(() => {
    const active = document.activeElement;
    if (!(active instanceof HTMLElement) || active === document.body) return "the page's body";
    const tag = active.tagName.toLowerCase();
    const implicit: Record<string, string> = {
      a: "link",
      button: "button",
      hr: "separator",
      input: active.getAttribute("type") === "radio" ? "radio" : "textbox",
      section: active.hasAttribute("aria-label") ? "region" : "generic",
      textarea: "textbox",
    };
    const role = active.getAttribute("role") ?? implicit[tag] ?? "generic";
    const name = (active.getAttribute("aria-label") ?? active.textContent ?? "")
      .replace(/\s+/g, " ")
      .trim()
      .slice(0, 80);
    const inRow = active.closest("[data-inbox-row]")?.getAttribute("data-inbox-row") ?? "no row";
    return `<${tag}> role=${role} name="${name}" testid=${active.dataset.testid ?? "(none)"} in ${inRow}`;
  });
}

/** Whether `target` holds the focus. */
export function holdsFocus(target: Locator): Promise<boolean> {
  return target.evaluate((node) => node === document.activeElement);
}

/** One frame and one task on, by when the page has drawn what it last read and its components'
 *  hand-offs have placed the focus. */
export function frameAndTask(page: Page): Promise<void> {
  return page.evaluate(() => {
    const done = Promise.withResolvers<void>();
    requestAnimationFrame(() => setTimeout(done.resolve, 0));
    return done.promise;
  });
}

/** Presses Tab from where the focus is, at most `limit` times, until `target` holds it, calling
 *  `atStop` at each stop before it. Fails, naming `what` and where the focus is, unless it gets
 *  there. */
export async function tabTo(
  page: Page,
  target: Locator,
  what: string,
  limit: number,
  atStop?: () => Promise<void>
): Promise<void> {
  // Each read follows a key press whose focus move the browser makes before the press resolves,
  // and the line after the loop reads the focus again and fails with its holder.
  for (let pressed = 0; pressed < limit && !(await holdsFocus(target)); pressed += 1) {
    await page.keyboard.press("Tab");
    // The press has moved the focus before it resolves, so this one read is the stop it reached.
    if (atStop !== undefined && !(await holdsFocus(target))) await atStop();
  }
  expect(
    await holdsFocus(target),
    `Tab reaches ${what}; the focus is on ${await describeFocus(page)}`
  ).toBe(true);
}
