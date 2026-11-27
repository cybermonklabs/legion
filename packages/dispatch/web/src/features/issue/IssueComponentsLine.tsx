import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { projectArchitectureSourcesQuery } from "../../api/queries";
import type { IssueComponents, IssueDetails } from "../../api/types";
import { CappedGroup } from "../../components/CappedGroup";
import { QueryError } from "../../components/QueryError";
import {
  type PageWords,
  usePageWords,
  useTooltips,
  withoutEllipsis,
} from "../../components/tooltips";
import {
  badgeHigh,
  badgePrimary,
  dangerText,
  linkHoverText,
  linkText,
  pageFailedText,
  pageInkText,
  pageLabelChip,
  pageMutedText,
  pageQuietText,
  pageWaitingChip,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { ComponentPicker } from "../architecture/ComponentPicker";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath } from "../refs/routes";
import { ARCHITECTURE_SOURCES_HEADING_ID } from "../settings/ArchitectureSourcesSection";
import { useDetailsRefusal } from "./HeaderDetails";
import { propertyControl } from "./property-control";
import { useIssueComponents } from "./useIssueComponents";

/** What the line says while the project's architecture sources are read, in place of a control
 *  it cannot yet say it has. */
export const SOURCES_LOADING = "Loading architecture sources…";
/** Why the line is read-only once the project answered that it has no architecture source. */
export const NO_SOURCE = "This project has no architecture source to take components from.";
/** The link to where a source is added: the Architecture sources section of Settings. */
export const ADD_SOURCE = "Add one in Settings";
/** Settings, scrolled to its Architecture sources section (`settings/SettingsPage.tsx` scrolls
 *  to the element the address's hash names). */
export const ADD_SOURCE_PATH = `/settings#${ARCHITECTURE_SOURCES_HEADING_ID}`;

/** Whether the components picker's trigger draws a bare "+": the issue's own explicit set, which
 *  the picker edits. Any other attachment, an ancestor's set among them, draws "Set components". */
function editsOwnSet(components: Pick<IssueComponents, "inherited_from" | "mode">): boolean {
  return components.mode === "explicit" && components.inherited_from === null;
}

/** The name the components picker's trigger answers to: what it opens and the issue it is on, so
 *  a voice user saying what they read presses it (WCAG 2.5.3).
 *
 *  `inside` is required: the trigger's state inside `NoTooltips`, or `null` outside it, where the
 *  name is `main`'s "Edit components of CORE-1" in every state. Inside it carries the
 *  issue's attachment, whether a save is in flight, and the page's words (`ANSWER_LINES` and
 *  `CONTROL_LABELS`); the name begins with the words the trigger draws, then the list's
 *  `control_name_of` and the issue's key: "Set components of CORE-1" while it draws
 *  `components_set`, and while it saves, when `ComponentPicker` draws `saving_components`
 *  ("Saving components…") in place of the trigger's words, those words with their trailing
 *  ellipsis dropped ("Saving components of CORE-1"), as `labelsControlName` names a saving labels
 *  control. On the issue's own set the trigger draws a bare "+", a symbol rather than words a
 *  voice user can say, so the name stays `main`'s word-first "Edit components of CORE-1", as the
 *  labels control keeps `main`'s "Edit labels" beside its "+". The words are passed in rather
 *  than imported: the page names the trigger outside React from the list it imports, and this
 *  file imports nothing of that list (`components/tooltips.tsx`). */
export function componentsControlName(
  issueKey: string,
  inside: {
    readonly components: Pick<IssueComponents, "inherited_from" | "mode">;
    readonly saving: boolean;
    readonly words: Pick<PageWords, "ANSWER_LINES" | "CONTROL_LABELS">;
  } | null
): string {
  if (inside === null || (!inside.saving && editsOwnSet(inside.components))) {
    return `Edit components of ${issueKey}`;
  }
  const { words } = inside;
  const drawn = inside.saving
    ? withoutEllipsis(words.ANSWER_LINES.saving_components)
    : words.CONTROL_LABELS.components_set;
  return `${drawn} ${words.CONTROL_LABELS.control_name_of} ${issueKey}`;
}

/**
 * The issue's components: its effective attachment as chips. An explicit set lists its live
 * component ids and marks each id a re-import retired; `none` shows the reason; an issue no
 * ancestor chain attached reads `Not attached`. When the attachment is an ancestor's, that
 * ancestor is named and linked. The line is editable only when the project has an architecture
 * source — the small `["architecture-sources", project]` list the project page shares — through
 * the same `ComponentPicker` the Architecture pane uses, offering every source's components: a
 * picked set saves `explicit`, clearing every pick saves `inherit` (back to the ancestors'). Ids
 * are project-wide, so a chip is the bare id whichever source supplies it. The component list
 * itself (the whole tree, refetched on every issue event in the project) is fetched only while
 * the picker is open. Each state of the sources read is drawn: loading, unavailable with Retry,
 * no source with a link to where one is added, and only then the picker.
 *
 * Outside `NoTooltips` it is the value of the Components row of today's issue header's Details,
 * which names it: no `Components:` of its own, a line that wraps inside its row, and at most 10
 * components before a "+N" that shows the rest (`CappedGroup`). Its groups, in this order: the
 * components with their "+N" (or the inherited set, or the reason there is none); the ancestor
 * the set is inherited from; the picker, or what the sources read says in its place; then the
 * line that says a refused save.
 *
 * Inside `NoTooltips` (`components/tooltips.tsx`, the new issue page) it says that page's
 * words with their marks and carries no `title`. There `inRow` draws it as the value of a row
 * of Details, which draws the label: no `Components:` of its own, a line that wraps, and each
 * component named by its title from the architecture read, which it then makes as soon as the
 * issue names a component.
 */
export function IssueComponentsLine({
  inRow = false,
  issue,
}: {
  /** Whether the line is the value of a row of the new issue page's Details. It acts inside
   *  `NoTooltips` alone. */
  inRow?: boolean;
  issue: Pick<
    IssueDetails,
    "components" | "components_reason_tree" | "key" | "project" | "status" | "title"
  >;
}): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  const inDetails = page !== null && inRow;
  const { components } = issue;
  const sources = useQuery(projectArchitectureSourcesQuery(issue.project));
  const [pickerOpen, setPickerOpen] = useState(false);
  const namesComponents = components.mode === "explicit" && (components.ids ?? []).length > 0;
  const tree = useQuery({
    queryKey: ["architecture", issue.project],
    queryFn: () => api.getArchitecture(issue.project),
    enabled: pickerOpen || (inDetails && namesComponents),
  });
  const componentName = (id: string) =>
    inDetails
      ? ((tree.data?.components ?? []).find((component) => component.id === id)?.title ?? id)
      : id;
  const part = "min-w-0 [overflow-wrap:anywhere]";
  const value = "max-w-full [overflow-wrap:anywhere]";
  // Inside `NoTooltips` each colour is one of the new issue page's: the line is the ink in a row
  // of Details (the prototype's `.frow .fv`) and the muted grey elsewhere (its `#hdr-props`); a
  // component is the page's label chip, a retired one its waiting chip, the one warning here;
  // what qualifies the set is quiet words, the issue it is inherited from a link in the ink, and
  // a refused save the failed red.
  const quiet = page === null ? textMutedOnSurface : pageQuietText;
  const write = useIssueComponents(issue);
  const refusal = write.stopped ? null : write.error;
  useDetailsRefusal(refusal);
  const inheritedFrom =
    components.inherited_from === null ? null : (
      <span className={`${part} text-xs ${quiet}`} {...page?.pageChrome}>
        {page?.CONTROL_LABELS.components_inherited_from ?? "inherited from"}{" "}
        <Link
          className={`dispatch-inline-link underline ${page === null ? `${linkText} ${linkHoverText}` : `${pageInkText} ${value}`}`}
          to={buildIssuePath({ key: components.inherited_from, kind: "issue" })}
          {...page?.authorText}
        >
          {components.inherited_from}
        </Link>
      </span>
    );
  // No `source`: why the reason is no record is the reading list's `issue-components-reason`
  // (`DISPATCH_READING_SURFACES`). It is drawn from the tree the issue read sends of it
  // (`components_reason_tree`).
  const reasonBody = (
    <MarkdownBody
      markdown={components.reason ?? ""}
      surface="issue-components-reason"
      tree={issue.components_reason_tree}
      variant="inline"
    />
  );
  const chips = [
    ...(components.ids ?? []).map((id) => (
      <span
        className={`inline-flex ${part} items-center rounded-full px-2 py-0.5 text-xs font-medium ${page === null ? `${badgePrimary.bg} ${badgePrimary.text}` : pageLabelChip}`}
        key={id}
        {...page?.authorText}
      >
        {componentName(id)}
      </span>
    )),
    ...(components.unknown ?? []).map((id) => (
      <span
        className={`inline-flex ${part} items-center rounded-full px-2 py-0.5 text-xs font-medium ${page === null ? `${badgeHigh.bg} ${badgeHigh.text}` : pageWaitingChip}`}
        key={`retired-${id}`}
        title={tooltips ? `${id} is no longer in the project's architecture model` : undefined}
        {...page?.pageChrome}
      >
        {page === null ? (
          <>retired: {id}</>
        ) : (
          <>
            {page.CONTROL_LABELS.components_retired}:{" "}
            <span className={value} {...page.authorText}>
              {id}
            </span>
          </>
        )}
      </span>
    )),
  ];
  return (
    <div
      className={`flex min-w-0 flex-wrap items-center gap-2 text-sm ${page === null ? textSecondaryOnSurface : inDetails ? pageInkText : pageMutedText}`}
      data-testid="issue-components"
    >
      {page === null || inDetails ? null : (
        <span className="font-medium" {...page.pageChrome}>
          {`${page.PROPERTY_LABELS.components}:`}
        </span>
      )}
      {components.mode === "inherit" ? (
        <span className={quiet} {...page?.pageChrome}>
          {page?.CONTROL_LABELS.components_not_attached ?? "Not attached"}
        </span>
      ) : components.mode === "none" ? (
        // Inside `NoTooltips` the line may shrink below its words (`min-w-0`): the renderer's own
        // element breaks a long word only where it would overflow (`break-words`), so without it
        // the line could be no narrower than that word.
        <span className={page === null ? quiet : `min-w-0 ${quiet}`} {...page?.pageChrome}>
          {page === null ? "None" : page.CONTROL_LABELS.components_none} —{" "}
          {/* Inside `NoTooltips` the reason is a value, with the author's mark, on a wrapper,
              since the renderer takes no class. */}
          {page === null ? (
            reasonBody
          ) : (
            <span className={value} {...page.authorText}>
              {reasonBody}
            </span>
          )}
        </span>
      ) : page === null ? (
        <CappedGroup className="contents" label="components" name="components">
          {chips}
        </CappedGroup>
      ) : (
        chips
      )}
      {inheritedFrom}
      {/* Whether the line is editable is the answer of the project's architecture sources. On
          today's page each of that read's states is drawn, so nothing appears or goes on a state
          the reader cannot see (dispatch://INFRA-5200): while it is out the line says it is
          loading and offers no control yet; a failed read says the components are unavailable,
          with Retry; an answer of no source says why the line is read-only and where a source is
          added; and only an answer naming a source offers the picker. A refresh that fails over
          a cached answer keeps what that answer drew. Inside `NoTooltips` (the new issue page)
          those words are in none of that page's own lists, so it keeps its own rule there: the
          picker while the project's answer names a source. */}
      {page !== null ? null : sources.isPending ? (
        <span className={`${part} text-xs ${quiet}`} role="status">
          {SOURCES_LOADING}
        </span>
      ) : sources.data === undefined ? (
        <QueryError
          error={sources.error}
          message="Components unavailable: this project's architecture sources could not be read."
          onRetry={() => void sources.refetch()}
          retrying={sources.isFetching}
        />
      ) : sources.data.length === 0 ? (
        <span className={`${part} text-xs ${quiet}`}>
          {NO_SOURCE}{" "}
          <Link
            className={`dispatch-inline-link underline ${linkText} ${linkHoverText}`}
            to={ADD_SOURCE_PATH}
          >
            {ADD_SOURCE}
          </Link>
        </span>
      ) : null}
      {(sources.data?.length ?? 0) === 0 ? null : (
        <ComponentPicker
          components={tree.data?.components ?? []}
          error={tree.error === null ? null : tree.error.message}
          loading={tree.data === undefined && tree.error === null}
          // The new issue page's add menu reaches the trigger by this mark rather than drawing a
          // second picker (`IssueAddMenu.tsx`), whatever the name says of the trigger's state.
          marks={{ trigger: propertyControl("components", issue.key) }}
          onOpenChange={setPickerOpen}
          onSave={(ids) =>
            void write.submit(ids.length === 0 ? { mode: "inherit" } : { ids, mode: "explicit" })
          }
          saving={write.pending}
          selected={editsOwnSet(components) ? (components.ids ?? []) : []}
          // Inside `NoTooltips` the name begins with the words the trigger draws, the page's
          // "Saving components…" while it saves among them, so a voice user says what they read.
          // Outside it is `main`'s.
          triggerAriaLabel={componentsControlName(
            issue.key,
            page === null ? null : { components, saving: write.pending, words: page }
          )}
        >
          {page === null ? (
            editsOwnSet(components) ? (
              "+"
            ) : (
              "Set components"
            )
          ) : (
            <span {...page.pageChrome}>
              {editsOwnSet(components) ? "+" : page.CONTROL_LABELS.components_set}
            </span>
          )}
        </ComponentPicker>
      )}
      {write.error === null ? null : (
        <span
          className={`${value} text-xs ${page === null ? dangerText : pageFailedText}`}
          role="alert"
          {...page?.authorText}
        >
          {write.error}
        </span>
      )}
    </div>
  );
}
