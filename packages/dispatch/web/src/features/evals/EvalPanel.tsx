import { createContext, lazy, type ReactNode, Suspense, useContext, useState } from "react";

import type { EvalKind } from "../../api/queries";
import { focusVisibleRing } from "../../theme/classes";
import type { EvalsScope } from "./useEvalsViews";

/** The number's panel, loaded once a reader opens one (`EvalPanelDialog`). */
const EvalPanel = lazy(() =>
  import("./EvalPanelDialog").then((module) => ({ default: module.EvalPanel }))
);

/** Opens the panel of the eval a slug names, where a page draws one (the Evals tab); null on a
 *  page that draws none. */
const OpenEvalPanel = createContext<((slug: string) => void) | null>(null);

/** A card's or a chart's title: inside a page that draws the number's panel, a button that opens
 *  the panel of the eval `slug` names; elsewhere, or with no eval, the title alone. */
export function EvalTitle({ slug, title }: { slug?: string; title: string }): ReactNode {
  const open = useContext(OpenEvalPanel);
  if (open === null || slug === undefined) return title;
  return (
    <button
      aria-haspopup="dialog"
      className={`rounded-sm text-left hover:underline focus-visible:outline-none focus-visible:ring-2 ${focusVisibleRing}`}
      onClick={() => open(slug)}
      type="button"
    >
      {title}
    </button>
  );
}

/**
 * Where a page draws the number's panel: every card's and chart's title inside it opens the panel
 * of its eval (`EvalTitle`), which reads the evals of `kinds` over `scope`. Opening and closing
 * the panel renders none of the page under it. The panel's code loads as the first one opens
 * (`EvalPanelDialog`), so a page's first paint loads none of it (dispatch://INFRA-2918, T33).
 */
export function EvalPanelHost({
  children,
  kinds,
  scope,
}: {
  children: ReactNode;
  kinds: readonly EvalKind[];
  scope: EvalsScope;
}): ReactNode {
  const [open, setOpen] = useState<string | null>(null);
  return (
    <OpenEvalPanel.Provider value={setOpen}>
      {children}
      {open === null ? null : (
        <Suspense fallback={null}>
          <EvalPanel kinds={kinds} onClose={() => setOpen(null)} scope={scope} slug={open} />
        </Suspense>
      )}
    </OpenEvalPanel.Provider>
  );
}
