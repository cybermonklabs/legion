import { type KeyboardEvent, useState } from "react";

import type { GoingContextInput } from "../../api/types";
import { usePageAddress } from "../../lib/page-address";
import { useProjectArtifact } from "../document/useProjectArtifact";
import { routeProjectOf } from "../refs/routes";
import {
  type BarScopeLevel,
  type BarTargets,
  barTargets,
  blockAtCursor,
  documentRouteOf,
  issueOfPage,
  readSelectedIssues,
  scopeOfPage,
  scopeOfProject,
} from "./bar-scope";

/** The chip the bar acts through: its words in the box, the two keys that move it, and what each
 *  offer acts on at the level those keys have left it at. */
export interface BarScope {
  /** What the bar acts on, in the chip's words (design v3, state 1): the page's own - the
   *  issue's key, the document's name, the selected issues, or the project - and "the whole
   *  project" or "a block" once `Backspace` widened it or `Tab` narrowed it; undefined on a page
   *  that puts nothing under the bar. The box carries it on its left. */
  readonly chip: string | undefined;
  /** The chip's words in full, with the keys that move it, for the chip's tooltip; undefined
   *  where there is no chip. */
  readonly chipTitle: string | undefined;
  /** `Tab` and `Backspace`, the two keys that move the chip, for the box to offer them before
   *  its own. It answers whether the key moved the chip, which the box reads as the key being
   *  spent: the browser's own move is then the one thing left to cancel. */
  readonly handleKey: (event: KeyboardEvent<HTMLInputElement>) => boolean;
  /** What each offer acts on at the level the chip is at now, so a row acts on what the chip
   *  above it names rather than on the page the bar was opened over. */
  readonly targets: BarTargets;
  /** The chip as the bar's answer is told it (`POST /api/v1/going/requests`' `context`): the
   *  page the bar opened over, and the project, issue and document block the chip names at the
   *  level it is at now, each `""` where it names none. */
  readonly context: GoingContextInput;
}

/**
 * What the bar will act on, in the words the chip says it in (`bar-scope.ts`), as one open of
 * the bar holds it: the page seeds it, `Tab` narrows it to the block the writing cursor was in
 * and `Backspace` widens it to the project.
 *
 * Every value the page itself gives is read on the first render, the cursor because `useDialog`
 * takes the focus in the commit that opens the bar, after that render, and the document's
 * selection is no longer the page's once it has, and the rest so that a hit the reader arrows
 * past or opens cannot move what the chip says and what an offer would act on before the bar
 * closes.
 */
export function useBarScope(
  /** What is in the box, as the person typed it: `Backspace` widens the chip on an empty box
   *  alone, since with words in it that key rubs out a letter. */
  query: string
): BarScope {
  const address = usePageAddress();
  // The project of the page this open was made from (its own page, or an issue of it). It
  // narrows nothing: a search from a project page searches everything, as a search from
  // anywhere does. The route reads it to tell which issue a query that is a bare number names,
  // and a reader who wants this project alone adds its chip.
  const [pageProject] = useState(() => routeProjectOf(address.pathname));
  const [cursor] = useState(blockAtCursor);
  const blockScope = cursor?.scope;
  const [pagePath] = useState(() => address.pathname);
  const [projectScope] = useState(() => scopeOfProject(address.pathname));
  const [scopeLevel, setScopeLevel] = useState<BarScopeLevel>("page");
  // The issues the issue list had selected as this open was made, so no render of the list
  // behind the bar moves a chip the reader is looking at.
  const [selectedIssues] = useState(readSelectedIssues);
  // The document page's own read, under the key that page uses, so the chip names the document
  // by its name with no request of its own.
  const [documentRoute] = useState(() => documentRouteOf(address.pathname, address.search));
  // The issue the bar was opened over, which is what an offer acts on while the chip is at page
  // level.
  const [pageIssue] = useState(() => issueOfPage(address.pathname, address.search));
  const documentName = useProjectArtifact(documentRoute).data?.name;
  const pageScope = scopeOfPage(address.pathname, address.search, documentName, selectedIssues);
  const scope =
    scopeLevel === "block" ? blockScope : scopeLevel === "project" ? projectScope : pageScope;
  // `Tab` narrows the chip to the block the writing cursor was in, and only where there was
  // one: everywhere else Tab still steps through the bar's own controls, which is the only way
  // a reader without a pointer reaches the filter chips. `Backspace` widens it to the project,
  // and only on an empty box: with words in the box it rubs out a letter, as it does in every
  // other field.
  const canNarrow = blockScope !== undefined && scopeLevel !== "block";
  const canWiden = projectScope !== undefined && scopeLevel !== "project" && scope !== projectScope;
  const targets = barTargets(scopeLevel, {
    block: blockScope,
    issue: pageIssue,
    project: pageProject,
  });
  // The chip's words: the page's own, or the short words of the level a key moved it to, the full
  // words of which the tooltip keeps beside the keys that move it.
  const chip =
    scopeLevel === "block" ? "a block" : scopeLevel === "project" ? "the whole project" : scope;
  const keys = [
    canNarrow ? "Tab: the block you are writing in" : undefined,
    canWiden ? "Backspace in the empty box: the whole project" : undefined,
  ].filter((hint) => hint !== undefined);
  return {
    chip: scope === undefined ? undefined : chip,
    chipTitle: scope === undefined ? undefined : [`Acting on ${scope}`, ...keys].join(" · "),
    context: {
      block: targets.block === undefined ? "" : (cursor?.id ?? ""),
      issue: targets.issue ?? "",
      path: pagePath,
      project: targets.project ?? "",
    },
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
    targets,
  };
}
