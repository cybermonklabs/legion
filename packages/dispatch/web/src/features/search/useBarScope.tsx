import { type KeyboardEvent, type ReactNode, useState } from "react";
import { useLocation } from "react-router-dom";

import { borderDefault, textMutedOnSurface, textSecondaryOnSurface } from "../../theme/classes";
import { useProjectArtifact } from "../document/useProjectArtifact";
import { routeProjectOf } from "../refs/routes";
import { KeyHints } from "../shell/ShortcutHelp";
import {
  type BarScopeLevel,
  type BarTargets,
  barTargets,
  documentRouteOf,
  issueOfPage,
  readSelectedIssues,
  scopeAtCursor,
  scopeOfPage,
  scopeOfProject,
} from "./bar-scope";

/** The chip the bar acts through: the line it draws above the box, the two keys that move it,
 *  and what each offer acts on at the level those keys have left it at. */
export interface BarScope {
  /** `Tab` and `Backspace`, the two keys that move the chip, for the box to offer them before
   *  its own. It answers whether the key moved the chip, which the box reads as the key being
   *  spent: the browser's own move is then the one thing left to cancel. */
  readonly handleKey: (event: KeyboardEvent<HTMLInputElement>) => boolean;
  /** The line above the box saying what the bar will act on, with the two keys that move it,
   *  and null on a page that puts nothing under the bar. */
  readonly header: ReactNode;
  /** What each offer acts on at the level the chip is at now, so a row acts on what the chip
   *  above it names rather than on the page the bar was opened over. */
  readonly targets: BarTargets;
}

/**
 * What the bar will act on, in the words the chip says it in (`bar-scope.ts`), as one open of
 * the bar holds it: the page seeds it, `Tab` narrows it to the block the writing cursor was in
 * and `Backspace` widens it to the project.
 *
 * Every value the page itself gives is read on the first render, the cursor because `useDialog`
 * takes the focus a frame later and the document's selection is no longer the page's once it
 * has, and the rest so that a hit the reader arrows past or opens cannot move what the chip
 * says and what an offer would act on before the bar closes.
 */
export function useBarScope(
  /** What is in the box, as the person typed it: `Backspace` widens the chip on an empty box
   *  alone, since with words in it that key rubs out a letter. */
  query: string
): BarScope {
  const location = useLocation();
  // The project of the page this open was made from (its own page, or an issue of it). It
  // narrows nothing: a search from a project page searches everything, as a search from
  // anywhere does. The route reads it to tell which issue a query that is a bare number names,
  // and a reader who wants this project alone adds its chip.
  const [pageProject] = useState(() => routeProjectOf(location.pathname));
  const [blockScope] = useState(scopeAtCursor);
  const [projectScope] = useState(() => scopeOfProject(location.pathname));
  const [scopeLevel, setScopeLevel] = useState<BarScopeLevel>("page");
  // The issues the issue list had selected as this open was made, so no render of the list
  // behind the bar moves a chip the reader is looking at.
  const [selectedIssues] = useState(readSelectedIssues);
  // The document page's own read, under the key that page uses, so the chip names the document
  // by its name with no request of its own.
  const [documentRoute] = useState(() => documentRouteOf(location.pathname, location.search));
  // The issue the bar was opened over, which is what an offer acts on while the chip is at page
  // level.
  const [pageIssue] = useState(() => issueOfPage(location.pathname, location.search));
  const documentName = useProjectArtifact(documentRoute).data?.name;
  const pageScope = scopeOfPage(location.pathname, location.search, documentName, selectedIssues);
  const scope =
    scopeLevel === "block" ? blockScope : scopeLevel === "project" ? projectScope : pageScope;
  // `Tab` narrows the chip to the block the writing cursor was in, and only where there was
  // one: everywhere else Tab still steps through the bar's own controls, which is the only way
  // a reader without a pointer reaches the filter chips. `Backspace` widens it to the project,
  // and only on an empty box: with words in the box it rubs out a letter, as it does in every
  // other field.
  const canNarrow = blockScope !== undefined && scopeLevel !== "block";
  const canWiden = projectScope !== undefined && scopeLevel !== "project" && scope !== projectScope;
  return {
    handleKey: (event) => {
      if (event.key === "Tab" && !event.shiftKey && canNarrow) {
        setScopeLevel("block");
        return true;
      }
      if (event.key === "Backspace" && query === "" && canWiden) {
        setScopeLevel("project");
        return true;
      }
      return false;
    },
    // What the bar will act on, above the box, with the two keys that move it. The filter chips
    // below the box are another thing and read as one: they say which items the words search,
    // each with a Remove control of its own, while this says what the bar acts on and is moved
    // by Tab and Backspace alone.
    header:
      scope === undefined ? null : (
        <div
          className={`flex shrink-0 flex-wrap items-center gap-x-3 gap-y-1 border-b px-3 py-2 text-xs ${borderDefault} ${textSecondaryOnSurface}`}
          data-testid="search-scope"
          role="status"
        >
          <span className="min-w-0">
            Acting on <span className="font-medium">{scope}</span>
          </span>
          {canNarrow ? (
            <span className={`inline-flex items-center gap-1 ${textMutedOnSurface}`}>
              <KeyHints keys={["Tab"]} />
              for the block you are writing in
            </span>
          ) : null}
          {canWiden ? (
            <span className={`inline-flex items-center gap-1 ${textMutedOnSurface}`}>
              <KeyHints keys={["Backspace"]} />
              for the whole project
            </span>
          ) : null}
        </div>
      ),
    targets: barTargets(scopeLevel, {
      block: blockScope,
      issue: pageIssue,
      project: pageProject,
    }),
  };
}
