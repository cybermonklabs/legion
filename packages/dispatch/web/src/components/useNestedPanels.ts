import { useEffect, useState } from "react";

/** What a trigger inside a panel is handed: whether its own panel is the one open, and how it
 *  asks to open or close it. */
export interface NestedPanel {
  readonly onOpenChange: (open: boolean) => void;
  readonly open: boolean;
}

/**
 * The panels a page's filter panel opens inside itself - its multi-selects, the search bar's
 * Project and Status lists - kept one open at a time, since each is portalled over the page and
 * two would cover each other. `open` is the filter panel's own: no inner panel stands open while
 * it is shut, and one that stood open when it shut is shut too, so the next open of the filter
 * panel starts with none.
 *
 * Nothing here refuses the filter panel's own close. `AnchoredPanel` counts a press in an inner
 * panel as a press in the filter panel, so no page has an outside press to refuse. Escape, or Tab
 * or Shift+Tab past either end of the filter panel itself, closes it with any inner panel; pressed
 * in an inner list, they close that list alone and put the reader back on its trigger in the
 * filter panel. So a reader who has left an inner list is never held in the panel behind it.
 *
 * Answers, for each inner panel's name, the props its trigger takes. Opening one shuts whichever
 * was open; a close that reaches one already replaced leaves the replacement alone.
 */
export function useNestedPanels<Name extends string>(open: boolean): (name: Name) => NestedPanel {
  const [current, setCurrent] = useState<Name | undefined>(undefined);
  useEffect(() => {
    if (!open) setCurrent(undefined);
  }, [open]);
  return (name) => ({
    onOpenChange: (next) => setCurrent((was) => (next ? name : was === name ? undefined : was)),
    open: open && current === name,
  });
}
