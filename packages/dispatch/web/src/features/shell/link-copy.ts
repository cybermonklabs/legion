import { copyText } from "../../lib/clipboard";
import { linkAddress } from "../refs/link-address";
import { useKeymap } from "./keymap";

/** The address of the link the keyboard's focus is on, where it is one `y` copies: a link that
 *  is not editable - a link inside a document the reader edits takes `y` as a letter typed - and
 *  that has an address (`linkAddress`). Undefined anywhere else. */
function focusedLinkAddress(): string | undefined {
  const active = document.activeElement;
  if (
    !(active instanceof HTMLElement) ||
    active.isContentEditable ||
    !active.matches("a[href], [role='link']")
  ) {
    return undefined;
  }
  return linkAddress(active);
}

/** The key that copies a focused link's address, and its label. The refusal check reads both
 *  (`keymap-refusal.ts`): no action may take a key whose first press is this one, since the
 *  binding below is registered on every page. */
export const LINK_COPY_KEY = "y";
export const LINK_COPY_LABEL = "Copy the focused link's address";

/**
 * `y` while a link has the keyboard's focus copies the link's address (dispatch://INFRA-2160, R23):
 * the `dispatch://` reference of a link to a Dispatch item, the URL of any other link
 * (`linkAddress`). One `global` binding tried before every scope (`KeyBinding.beforeScopes`), so
 * it works on every page and in every dialog, the drawer and the margin's sheet included, and
 * comes before a page's own `y` (the Inbox's copy of the item's reference). `KeymapProvider`
 * registers it. It names no action of the catalogue: the key works only while its link has the
 * focus, which WCAG 2.1.4 accepts with no remap, so Settings lists nothing to change, and the `?`
 * help lists it under Global.
 */
export function useLinkCopy(): void {
  useKeymap("global", [
    {
      beforeScopes: true,
      id: "copy-link-address",
      keys: LINK_COPY_KEY,
      label: LINK_COPY_LABEL,
      run: () => {
        const address = focusedLinkAddress();
        if (address !== undefined) {
          void copyText(address);
        }
      },
      when: () => focusedLinkAddress() !== undefined,
    },
  ]);
}
