/**
 * What the `issue` scope's keys act on in the header: every target is a control the header
 * already renders, so a key does exactly what a click on it does. Scoping the lookup to
 * `[data-issue-header]` keeps a child issue's row in the Children tab - it carries the same
 * accessible names - out of reach. `IssuePage` reads it for the Status, priority, labels, title
 * and pin keys, and `IssueHeader` for Approve and Request changes; the new page's header reads it
 * for its own keys (`issue-page/useHeaderKeys.ts`), whose `<header>` carries the same mark.
 */
const HEADER = "[data-issue-header]";

/** The header control `selector` names, or `null` when it is absent or refuses input (a closed
 *  issue, a save in flight, which holds a menu's trigger or the labels' trigger with
 *  `aria-disabled`, `IssueLabels`): the one fact both the key's `run` and the `when` that offers
 *  it read, so a shortcut is listed exactly while its control would take a click. */
export function usableControl(selector: string): HTMLElement | null {
  const node = document.querySelector(HEADER)?.querySelector<HTMLElement>(selector);
  if (node == null) {
    return null;
  }
  const disabled =
    ((node instanceof HTMLButtonElement || node instanceof HTMLSelectElement) && node.disabled) ||
    node.getAttribute("aria-disabled") === "true";
  return disabled ? null : node;
}
