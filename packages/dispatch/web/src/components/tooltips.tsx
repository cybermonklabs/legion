import { createContext, type ReactNode, useContext } from "react";

/** A table of words, by the keys a shared control reads from it. */
type Words<Key extends string> = { readonly [key in Key]: string };

/**
 * The words and marks a shared control draws on a surface with no tooltips: the tables of the
 * issue page's word list (`features/issue-page/page-words.ts` and `page-structure.ts`) by the
 * keys the controls read, and the two marks that divide the page's own words from what a person
 * or an agent wrote. This module names the keys and imports nothing of that list, so the list
 * loads with the issue page alone; `features/issue-page/NoTooltips.tsx` fills it, and a key the
 * list lacks fails the type check there.
 */
export interface PageWords {
  readonly ANSWER_LINES: Words<
    | "loading_priorities"
    | "priorities_not_saved"
    | "priorities_unreadable"
    | "saving_components"
    | "saving_labels"
  >;
  readonly AUTHOR_KIND_WORDS: Words<"agent">;
  readonly CONTROL_LABELS: Words<
    | "add"
    | "add_label"
    | "add_priority"
    | "advances_inherited"
    | "approval_document_criteria"
    | "approval_document_decisions"
    | "approval_document_spec"
    | "approval_latest"
    | "approval_request_changes_to"
    | "approval_version_of"
    | "cancel"
    | "close"
    | "components_inherited_from"
    | "components_none"
    | "components_not_attached"
    | "components_retired"
    | "components_set"
    | "control_name_of"
    | "control_name_to"
    | "external_link_unsafe"
    | "github_checks"
    | "no_repository"
    | "none"
    | "not_requested"
    | "not_running"
    | "priority_to_advance"
    | "repository_inherited"
    | "takeable"
    | "takeable_now"
    | "unassigned"
    | "unchecked"
    | "unknown_author"
    | "unnamed_author"
    | "unresponsive"
    | "unresponsive_since"
    | "version"
  >;
  readonly PROPERTY_LABELS: Words<"advances" | "claim" | "components" | "priority" | "repository">;
  readonly STATE_LINES: Words<"advances_at_cap" | "github_unreadable" | "subscription_wider">;
  /** The mark of an element whose text is the page's own words (`data-page-chrome`). */
  readonly pageChrome: Readonly<Record<string, string>>;
  /** The mark of an element whose text a person or an agent wrote (`data-author-text`). */
  readonly authorText: Readonly<Record<string, string>>;
}

/** A word of the page's list as a control's accessible name says it: a name is said rather than
 *  read, so the ellipsis that shows a write under way ("Saving labels…") is no word of it, and
 *  one `…`, or three dots, at the end goes ("Saving labels"). Inside `NoTooltips` the shared
 *  controls name a busy control by the words it draws this way, then `CONTROL_LABELS`'
 *  `control_name_of` and the issue's key: `labelsControlName` and `componentsControlName`, each
 *  given the control's state inside `NoTooltips`, whether a save is in flight among it, or `null`
 *  outside it for `main`'s name. */
export function withoutEllipsis(words: string): string {
  return words.replace(/\s*(?:…|\.{3})$/u, "");
}

/**
 * Whether the surface a control is drawn on offers pointer tooltips, and the words it draws
 * where it offers none.
 *
 * Every shared control carries its `title` as it always has, because a tooltip is a real
 * affordance on a surface a reader points at. One surface does not: the issue page, where a
 * fact a pointer alone can reach is a fact a reader on a phone or on the keyboard does not
 * have, so the page draws what it means and carries no `title` at all. The page wraps what it
 * draws in `NoTooltips` (`features/issue-page/NoTooltips.tsx`), and every control inside
 * answers by leaving its `title` off, drawing the fact the `title` held, and saying the page's
 * words with their marks.
 *
 * It is a context rather than a prop because Details is drawn in the margin, which the shell
 * mounts beside the router, so no prop of the page reaches it. Outside the issue page it holds
 * nothing, and every control draws what it draws on `main`.
 *
 * Inside it a control also draws in the page's colours (`PAGE_COLORS`), lets every value wrap
 * with `[overflow-wrap:anywhere]`, draws a glyph the page's typeface lacks as an inline svg, and
 * keeps the focus on a control whose own write is out (`aria-disabled`, never `disabled`); the
 * cases of `features/issue-page/shared-controls.tooltips.test.tsx` and
 * `shared-controls.options.test.tsx` hold each rule. No control imports a module of
 * `features/issue-page/`, so the page's word list loads with that page alone.
 */
const SurfaceContext = createContext<PageWords | null>(null);

/** Everything inside draws no `title`, and draws `words` where a control has a word of the page
 *  to say. `NoTooltips` is the one caller: it passes the issue page's list. */
export function PageWordsProvider({
  children,
  words,
}: {
  children: ReactNode;
  words: PageWords;
}): ReactNode {
  return <SurfaceContext.Provider value={words}>{children}</SurfaceContext.Provider>;
}

/** Whether this control may carry a `title`. True everywhere but inside `NoTooltips`. */
export function useTooltips(): boolean {
  return useContext(SurfaceContext) === null;
}

/** The issue page's words and marks inside `NoTooltips`, and `null` everywhere else, where a
 *  control says what it says on `main` and carries no mark. */
export function usePageWords(): PageWords | null {
  return useContext(SurfaceContext);
}
