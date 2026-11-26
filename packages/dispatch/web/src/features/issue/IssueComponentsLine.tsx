import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../../api/client";
import { projectArchitectureSourcesQuery } from "../../api/queries";
import type { IssueComponents, IssueDetails } from "../../api/types";
import {
  type PageWords,
  usePageWords,
  useTooltips,
  withoutEllipsis,
} from "../../components/tooltips";
import {
  badgeHigh,
  badgePrimary,
  pageFailedText,
  pageLabelChip,
  pageWaitingChip,
} from "../../theme/classes";
import {
  dangerText,
  linkHoverText,
  linkText,
  pageInkText,
  pageMutedText,
  pageQuietText,
  textMutedOnSurface,
  textSecondaryOnSurface,
} from "../../theme/shell-classes";
import { ComponentPicker } from "../architecture/ComponentPicker";
import { MarkdownBody } from "../refs/MarkdownBody";
import { buildIssuePath } from "../refs/page-routes";
import { useIssueComponents } from "./useIssueComponents";

/** Whether the components picker's trigger draws a bare "+": the issue's own explicit set, which
 *  the picker edits. Any other attachment, an ancestor's set among them, draws "Set components". */
function editsOwnSet(components: Pick<IssueComponents, "inherited_from" | "mode">): boolean {
  return components.mode === "explicit" && components.inherited_from === null;
}

/** The name the components picker's trigger answers to: what it opens and the issue it is on, so
 *  a voice user saying what they read presses it (WCAG 2.5.3). The new issue page's add menu
 *  reaches this trigger by this name rather than drawing a second picker, so a word changed here
 *  changes both sides at once.
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
 * The `Components:` line of the issue header's metadata rail: the issue's effective
 * attachment as chips. An explicit set lists its live component ids and marks each id a
 * re-import retired; `none` shows the reason; an issue no ancestor chain attached reads
 * `Not attached`. When the attachment is an ancestor's, that ancestor is named and linked.
 * The line is editable only when the project has an architecture source — the small
 * `["architecture-sources", project]` list the project page shares, whose `[]` means
 * read-only — through the same `ComponentPicker` the Architecture pane uses, offering every
 * source's components: a picked set saves `explicit`, clearing every pick saves `inherit`
 * (back to the ancestors'). Ids are project-wide, so a chip is the bare id whichever source
 * supplies it. The component list itself (the whole tree, refetched on every issue event in
 * the project) is fetched only while the picker is open.
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
  // Inside `NoTooltips` nothing of the line stops its wrap, in a row of Details and in the header
  // alike: the words break between themselves, and a long name breaks inside its chip only where
  // no break between words is left, since a component, the issue it is inherited from and a
  // reason are values, which wrap there. Outside it each part stays whole, as on `main`.
  const part = page === null ? "shrink-0" : "min-w-0 [overflow-wrap:anywhere]";
  const value = "max-w-full [overflow-wrap:anywhere]";
  // Inside `NoTooltips` each colour is one of the new issue page's: the line is the ink in a row
  // of Details (the prototype's `.frow .fv`) and the muted grey elsewhere (its `#hdr-props`); a
  // component is the page's label chip, a retired one its waiting chip, the one warning here;
  // what qualifies the set is quiet words, the issue it is inherited from a link in the ink, and
  // a refused save the failed red.
  const quiet = page === null ? textMutedOnSurface : pageQuietText;
  const write = useIssueComponents(issue);
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
  return (
    <div
      className={`flex ${page === null ? "shrink-0" : "min-w-0 flex-wrap"} items-center gap-2 text-sm ${page === null ? textSecondaryOnSurface : inDetails ? pageInkText : pageMutedText}`}
      data-testid="issue-components"
    >
      {inDetails ? null : (
        <span className="font-medium" {...page?.pageChrome}>
          {`${page?.PROPERTY_LABELS.components ?? "Components"}:`}
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
      ) : (
        <>
          {(components.ids ?? []).map((id) => (
            <span
              className={`inline-flex ${part} items-center rounded-full px-2 py-0.5 text-xs font-medium ${page === null ? `${badgePrimary.bg} ${badgePrimary.text}` : pageLabelChip}`}
              key={id}
              {...page?.authorText}
            >
              {componentName(id)}
            </span>
          ))}
          {(components.unknown ?? []).map((id) => (
            <span
              className={`inline-flex ${part} items-center rounded-full px-2 py-0.5 text-xs font-medium ${page === null ? `${badgeHigh.bg} ${badgeHigh.text}` : pageWaitingChip}`}
              key={id}
              title={
                tooltips ? `${id} is no longer in the project's architecture model` : undefined
              }
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
          ))}
        </>
      )}
      {inheritedFrom}
      {(sources.data?.length ?? 0) === 0 ? null : (
        <ComponentPicker
          components={tree.data?.components ?? []}
          error={tree.error === null ? null : tree.error.message}
          loading={tree.data === undefined && tree.error === null}
          onOpenChange={setPickerOpen}
          onSave={(ids) =>
            void write.submit(ids.length === 0 ? { mode: "inherit" } : { ids, mode: "explicit" })
          }
          saving={write.pending}
          selected={editsOwnSet(components) ? (components.ids ?? []) : []}
          // Inside `NoTooltips` the name begins with the words the trigger draws, the page's
          // "Saving components…" while it saves among them, so a voice user says what they read;
          // the page's add menu finds it by the same name. Outside it is `main`'s.
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
          className={`${page === null ? "shrink-0" : value} text-xs ${page === null ? dangerText : pageFailedText}`}
          role="alert"
          {...page?.authorText}
        >
          {write.error}
        </span>
      )}
    </div>
  );
}
