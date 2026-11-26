import type { Page } from "@playwright/test";

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
