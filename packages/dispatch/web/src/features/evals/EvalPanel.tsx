import { useQuery } from "@tanstack/react-query";
import { createContext, type ReactNode, useCallback, useContext, useRef, useState } from "react";

import { type EvalKind, evalDetailQuery } from "../../api/queries";
import type { EvalListItem, EvalListResponse } from "../../api/types";
import { LinkList } from "../../components/LinkList";
import { QueryError } from "../../components/QueryError";
import {
  borderStrong,
  dismissButtonText,
  focusVisibleRing,
  surfaceBg,
  textMutedOnSurface,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { useDialog } from "../shell/useDialog";
import { evalName, primarySeries } from "./chart-model";
import { EvalRows } from "./EvalRows";
import { type EvalsScope, useEvalsViews } from "./useEvalsViews";

/** How many rows the panel lists a page. */
const PANEL_ROWS = 50;

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

/** The panel's body: the eval's method word for word from its detail read, keyed by the version
 *  the evals view answers (plan D11a), so a register write that writes a new version reads it
 *  again; the eval's links and its primary series' links under the method, as the evals view
 *  answers them, and no list where it has none; then the rows behind the number. */
function PanelBody({ item, scope }: { item: EvalListItem; scope: EvalsScope }): ReactNode {
  const { slug, version } = item.definition;
  const detail = useQuery(evalDetailQuery(scope.project, slug, version));
  // A series' links write a version of the series and none of the eval, so they are read from the
  // view, which answers the series' own newest version, and never from the detail of the eval's.
  const links = [...(item.definition.links ?? []), ...(primarySeries(item)?.links ?? [])];
  const heading = `text-xs font-semibold uppercase ${textMutedOnSurface}`;
  return (
    <div className="flex flex-col gap-3">
      <section className="flex flex-col gap-1">
        <h3 className={heading}>Method</h3>
        {detail.isError ? (
          <QueryError
            error={detail.error}
            message="Could not read the method."
            onRetry={() => void detail.refetch()}
          />
        ) : detail.data === undefined ? (
          <p className={`text-sm ${textMutedOnSurface}`} role="status">
            Reading the method…
          </p>
        ) : detail.data.doc_text === null ? (
          <p className={`text-sm ${textMutedOnSurface}`}>No method is written for this eval.</p>
        ) : (
          <p className={`whitespace-pre-wrap text-sm ${textPrimaryOnSurface}`} data-method-text>
            {detail.data.doc_text}
          </p>
        )}
        {links.length === 0 ? null : <LinkList label="Links" links={links} />}
      </section>
      <section className="flex flex-col gap-1">
        <h3 className={heading}>Rows</h3>
        <EvalRows
          label={`Rows behind ${evalName(item)}`}
          limit={PANEL_ROWS}
          scope={scope}
          slug={slug}
        />
      </section>
    </div>
  );
}

/**
 * The number's panel (dispatch://INFRA-2918/spec R10, plan D12, D11a), opened on a card's or a
 * chart's title: the eval's method, its links and its primary series' links, and the rows behind
 * it. It reads the eval from the evals views the tab holds (sharing their subscriptions) and keeps
 * that one eval of each page, so it renders again only when a delta moves that eval. It is a
 * dialog at the right edge (`useDialog`): focus starts on its close button and stays inside, and
 * Escape closes it.
 */
function EvalPanel({
  kinds,
  onClose,
  scope,
  slug,
}: {
  kinds: readonly EvalKind[];
  onClose: () => void;
  scope: EvalsScope;
  slug: string;
}): ReactNode {
  const closeRef = useRef<HTMLButtonElement>(null);
  const dialog = useDialog<HTMLElement>({ initialFocusRef: closeRef, onClose, open: true });
  const keep = useCallback(
    (answer: EvalListResponse): EvalListResponse => ({
      ...answer,
      items: (answer.items ?? []).filter((item) => item.definition.slug === slug),
    }),
    [slug]
  );
  const views = useEvalsViews(scope, kinds, keep);
  const item = views.flatMap((view) => view.items)[0];
  const name = item === undefined ? slug : evalName(item);
  return (
    <aside
      aria-label={name}
      aria-modal="true"
      className={`fixed top-0 right-0 z-30 flex h-full w-[28rem] max-w-full flex-col gap-3 overflow-y-auto border-l p-4 shadow-xl ${borderStrong} ${surfaceBg}`}
      data-eval-panel={slug}
      ref={dialog.containerRef}
      role="dialog"
    >
      <div className="flex items-start justify-between gap-2">
        <h2 className={`text-[15px] font-semibold ${textPrimaryOnSurface}`}>{name}</h2>
        <button
          aria-label="Close the panel"
          className={dismissButtonText}
          onClick={onClose}
          ref={closeRef}
          type="button"
        >
          ✕
        </button>
      </div>
      {item !== undefined ? (
        <PanelBody item={item} scope={scope} />
      ) : views.some((view) => view.status === "subscribed") ? (
        <p className={`text-sm ${textMutedOnSurface}`} role="status">
          Reading the eval…
        </p>
      ) : (
        <p className={`text-sm ${textMutedOnSurface}`}>
          This eval is not in the tab's reads any more.
        </p>
      )}
    </aside>
  );
}

/**
 * Where a page draws the number's panel: every card's and chart's title inside it opens the panel
 * of its eval (`EvalTitle`), which reads the evals of `kinds` over `scope`. Opening and closing
 * the panel renders none of the page under it.
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
        <EvalPanel kinds={kinds} onClose={() => setOpen(null)} scope={scope} slug={open} />
      )}
    </OpenEvalPanel.Provider>
  );
}
