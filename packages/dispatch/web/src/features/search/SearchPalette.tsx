import { snippetSegments } from "@legion/contracts/dispatch-snippet";
import { SEARCH_QUERY_MAX } from "@legion/contracts/dispatch-tools";
import { type UseQueryResult, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  Fragment,
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useNavigate } from "react-router-dom";

import { goingConversationsQuery, projectsQuery, whoAmIQuery } from "../../api/queries";
import type {
  GoingContextInput,
  GoingLine,
  Project,
  SearchOperator,
  SearchResult,
} from "../../api/types";
import { ControlRow } from "../../components/ControlRow";
import { filterChips } from "../../components/filter-chips";
import { MultiSelect } from "../../components/MultiSelect";
import { PanelTrigger } from "../../components/PanelTrigger";
import { QueryError } from "../../components/QueryError";
import { SearchBox } from "../../components/SearchBox";
import { useNestedPanels } from "../../components/useNestedPanels";
import { inputMethodKey } from "../../lib/input-method-key";
import {
  backdrop50,
  badgeLow,
  borderDefault,
  card,
  dangerText,
  expandedButtonAccent,
  inputClasses,
  kbdHint,
  searchHitBg,
  searchHitText,
  secondaryButtonCompact,
  selectedCardBg,
  selectedCardBorder,
  surfaceMutedBg,
  textMutedOnSelectedCard,
  textMutedOnSurface,
  textMutedOnSurfaceMuted,
  textPrimaryOnSurface,
  textSecondaryOnSurface,
  textSecondaryOnSurfaceMuted,
} from "../../theme/classes";
import { ConversationView } from "../going/ConversationView";
import {
  asksPerson,
  conversationWorking,
  movePanel,
  newConversation,
  openConversation,
  titleWords,
  useGoingValue,
  useSendQueued,
} from "../going/going-store";
import { History } from "../going/History";
import { usePlanCardKeys } from "../going/PlanRows";
import { BROWSER_PANEL_KEYS } from "../going/panel-states";
import {
  BoxFrame,
  BoxRow,
  boxHint,
  boxInputClassName,
  boxInputStyle,
  boxWords,
  ConversationHeader,
  MoveButton,
} from "../going/RunControls";
import { NO_MODEL_WORDS } from "../going/run-states";
import { useAsk } from "../going/useAsk";
import { issueStatuses, statusText } from "../project/board-model";
import { RefKindIcon } from "../refs/kind-icons";
import { MarkdownPreview } from "../refs/MarkdownPreview";
import { DeletedBody, EditedMark } from "../refs/PostMarks";
import { postDeletion } from "../refs/post-marks";
import { referenceTriggerProps } from "../refs/RefPreview";
import { ReferenceHostIs } from "../refs/reference-host";
import { issueSummaryQuery } from "../refs/reference-target";
import {
  buildIssuePath,
  buildProjectPath,
  type DispatchReferenceRoute,
  nodeRouteFromHref,
  parseIssuePath,
} from "../refs/routes";
import { textSourceKind, textSourceOf } from "../refs/text-references";
import { useFollowInPlace } from "../refs/useFollowInPlace";
import { appKeymap, DIALOG_SCOPE, type KeymapAction, useKeymap } from "../shell/keymap";
import { KeyHints } from "../shell/ShortcutHelp";
import { useCloseOnNavigation, useDialog } from "../shell/useDialog";
import { AnswerPanel } from "./AnswerPanel";
import { AskHitStatus } from "./AskHitStatus";
import { documentRouteOf } from "./bar-scope";
import { dropFiling, fileNewIssue, filingParent, useBarOpen, useFiling } from "./filing";
import { NEW_ISSUE_GROUP_ID, useNewIssueOffer } from "./NewIssueOffer";
import { barNoteClassName, type OfferRow } from "./offer";
import { type RecentItem, readRecents } from "./recents";
import { SearchOperators } from "./SearchOperators";
import {
  type SearchBarFilters,
  type SearchFilterName,
  searchFilterChips,
  searchRequest,
} from "./search-filters";
import {
  groupResults,
  hitIsItsIssue,
  issueHitTitle,
  kindLabel,
  optionId,
  ownerWords,
  searchOperators,
  searchResultKind,
  snippetCut,
  snippetFromSentence,
  snippetWithoutDirectives,
  stepActive,
  withoutDirectiveSyntax,
} from "./search-model";
import { useBarScope } from "./useBarScope";
import { useGoingBar } from "./useGoingBar";
import { useTieredSearch } from "./useTieredSearch";

const emptyResults: SearchResult[] = [];
const emptyProjects: Project[] = [];
const ACTIONS_GROUP_ID = "search-group-actions";
/** The line on top's first row, which keeps this id whatever it reads - Ask, Do, File as a new
 *  issue, the filing that is out or the one that failed - so the highlight stays on it as it
 *  changes; and its second row, Ask, while the first reads Do or File. */
const LINE_OPTION_ID = "search-option-line";
const LINE_ASK_OPTION_ID = "search-option-line-ask";
const CONTINUE_OPTION_ID = "search-option-continue";
const MORE_ACTIONS_GROUP_ID = "search-group-more-actions";
const PROJECTS_GROUP_ID = "search-group-projects";
const RECENT_GROUP_ID = "search-group-recent";

/** How recent a conversation is for the bar to offer "Continue" on it as it opens (design v3,
 *  Keys): the last hour. */
const CONTINUE_WITHIN_MS = 60 * 60 * 1000;

/** Words that are an issue's key alone (`CORE-12`): their hit heads the list, and they ask no
 *  model for an answer. */
const ISSUE_KEY = /^[A-Za-z][A-Za-z0-9]*-\d+$/;

/** Whether `result` is the very item the words name: an issue hit whose key or title they are, or
 *  a document hit whose name they are, whatever the case. `said` is the words in lower case. A
 *  comment, an ask or a message is never the item named, though its issue may be. Such a hit
 *  ranks above the line on top, so Enter opens it (R2), and the words ask no model. */
function namesHit(result: SearchResult, said: string): boolean {
  const owner = result.owner;
  if (result.kind === "issue") {
    return (owner.key ?? "").toLowerCase() === said || (owner.title ?? "").toLowerCase() === said;
  }
  return (
    result.kind === "document" && (result.artifact?.name ?? owner.name ?? "").toLowerCase() === said
  );
}

interface ActionRow {
  action: KeymapAction;
  id: string;
  kind: "action";
}

interface ProjectRow {
  id: string;
  kind: "project";
  project: Project;
}

interface ResultRow {
  id: string;
  kind: "result";
  result: SearchResult;
}

/** An item the person opened lately, which the bar lists back while its box is empty. */
interface RecentRow {
  id: string;
  item: RecentItem;
  kind: "recent";
}

/** A row of the line on top (R2, design v3, states 1 and 13): Ask, Do or File as a new issue for
 *  the words in the box, the filing that is out, or the filing that failed, with its reason as
 *  its words and Try again as its press. */
interface LineRow {
  readonly id: string;
  readonly kind: "line";
  readonly line: "ask" | "do" | "file" | "filing" | "failed";
  /** What the row says before the words: "Ask", "Do", "File as a new issue in DEMO", "File as a
   *  new issue under DEMO-6", "Filing in DEMO…", "Not filed". */
  readonly label: string;
  readonly words: string;
  readonly select: () => void;
}

/** One selectable line, whatever it came from: arrows, Enter and `aria-activedescendant` see
 *  only these. An offer's row (`OfferRow`) and a line's carry their own press, so the dialog
 *  never reaches back into what made them to run it. */
type PaletteRow = ActionRow | LineRow | OfferRow | ProjectRow | RecentRow | ResultRow;

/** What Enter does on `row`, for the foot's key hint; undefined where it does nothing. */
function enterWord(row: PaletteRow | undefined): string | undefined {
  switch (row?.kind) {
    case undefined:
      return undefined;
    case "line":
      return {
        ask: "Ask",
        do: "Do",
        failed: "Try again",
        file: "File",
        filing: undefined,
      }[row.line];
    case "offer":
      // Continue, or the New issue offer's row: each names what Enter does on it.
      return row.enter;
    case "action":
      return "Run";
    default:
      return "Open";
  }
}

/** A key as the bar's hints draw it. */
function Key({ children }: { children: string }): ReactNode {
  return <kbd className={`rounded px-1 py-0.5 font-medium ${kbdHint}`}>{children}</kbd>;
}

/** One row of the listbox, a hit or a command: the selected outline and fill, and a click, Enter
 *  or Space selecting it. `layout` arranges the row's own content; `line` and `status` are read
 *  back as `data-line` (which line on top the row is) and `data-status` (a hit's issue status). */
function PaletteOption({
  active,
  children,
  id,
  layout = "",
  line,
  onSelect,
  reference,
  status,
}: {
  active: boolean;
  children: ReactNode;
  id: string;
  layout?: string;
  line?: LineRow["line"];
  onSelect: () => void;
  /** A hit's target, which makes the row a `RefPreview` trigger. */
  reference?: DispatchReferenceRoute;
  status?: string;
}): ReactNode {
  // A row is one button in all but its tag: a click or Enter anywhere in it opens what it
  // found, so nothing drawn inside it is a link of its own.
  return (
    <ReferenceHostIs host="control">
      <div
        aria-selected={active}
        className={`relative isolate min-h-11 cursor-pointer border-b px-3 py-2 last:border-b-0 ${layout} ${borderDefault} ${
          active ? selectedCardBorder : ""
        }`}
        data-line={line}
        data-status={status}
        id={id}
        onClick={onSelect}
        onKeyDown={(event) => {
          if (event.key === "Enter" || event.key === " ") {
            event.preventDefault();
            onSelect();
          }
        }}
        role="option"
        tabIndex={-1}
        {...(reference === undefined ? undefined : referenceTriggerProps(reference))}
      >
        {active ? (
          <div
            aria-hidden="true"
            className={`pointer-events-none absolute inset-0 -z-10 ${selectedCardBg}`}
          />
        ) : null}
        {children}
      </div>
    </ReferenceHostIs>
  );
}

/**
 * The one look of a row that opens an item (design v3, state 1; review a12), a hit's and a recent
 * item's alike, as Raycast gives every list of its bar one row look: its kind's icon, its owner's
 * key small and grey, its words at the size of the line on top (`name`), then `after` on the
 * same line, and the issue's status at the row's end, the one place the row says it, in a column
 * of its own beside `under`, a second line under the key, which stops before it rather than
 * running on under it (review R4). A row of a done issue reads muted.
 */
function ItemRow({
  after,
  icon,
  itemKey,
  muted = false,
  name,
  status,
  under,
}: {
  after?: ReactNode;
  icon: ReactNode;
  itemKey: string;
  muted?: boolean;
  name: ReactNode;
  status: string | undefined;
  under?: ReactNode;
}): ReactNode {
  return (
    <div className="flex min-w-0 flex-1 items-start gap-2">
      <div className="min-w-0 flex-1">
        <div
          className={`flex min-w-0 items-center gap-2 text-sm ${
            muted ? textMutedOnSelectedCard : textSecondaryOnSurface
          }`}
        >
          {icon}
          {itemKey === "" ? null : (
            <span className="shrink-0 text-xs font-semibold">{itemKey}</span>
          )}
          {name}
          {after}
        </div>
        {under}
      </div>
      {status === undefined ? null : (
        <span
          className={`shrink-0 rounded-full px-2 py-0.5 text-xs font-medium ${badgeLow.bg} ${badgeLow.text}`}
          data-testid="search-hit-status"
        >
          {statusText(status)}
        </span>
      )}
    </div>
  );
}

/**
 * A hit under the row of the issue or document it belongs to, which names that owner, so the hit
 * names only itself: its kind's icon and word, the document it is in where it is one, and its
 * snippet under them - the server's reading of it (`snippet_tree`), its formatting shown rather
 * than its syntax and the words the search matched marked, each link drawn as the words around
 * it, since a link inside the row would compete with the row's own Enter and click. The snippet is
 * cleaned as a best match's is (`BestOption`): the syntax of a Proof directive the window cut
 * comes out of its text (`snippetWithoutDirectives`; rule 1, no marker text), and a line whose
 * window the server says began or stopped inside the text (`snippetCut`) begins or ends with "…",
 * and starts at the next sentence's first word where the window began on a sentence's tail
 * (`snippetFromSentence`). `muted` is its owner's: the hits of a done issue read muted.
 */
function ResultOption({
  active,
  muted,
  onSelect,
  result,
}: {
  active: boolean;
  muted: boolean;
  onSelect: () => void;
  result: SearchResult;
}): ReactNode {
  // The sentinels the server leaves in `snippet` itself come out for the literal text a preview
  // falls back to where no tree reached it.
  const source = snippetSegments(result.snippet)
    .map((segment) => segment.text)
    .join("");
  // The route table carries the kind as a plain string; a kind this build has no icon or label
  // for keeps its own word and draws no icon, rather than borrowing another kind's.
  const kind = searchResultKind(result.kind);
  // The same tree, and so the same drawing, on every render of the row: a tree cut anew each
  // time would paint the snippet again on each.
  const snippet = useMemo(() => {
    const cut = snippetCut(result.snippet_tree);
    const drawn = snippetWithoutDirectives(result.snippet_tree);
    return { cut, tree: cut.start ? snippetFromSentence(drawn) : drawn };
  }, [result.snippet_tree]);
  // The hit's snippet is the record's own text, cut: the row names the record (`result.id`), the
  // kind it was indexed under, which is what a `#id` in it resolves against, and the issue it is
  // on, its owner. A kind that names no reference source - a project, an agent - answers none
  // (`textSourceKind`).
  const record = textSourceOf(
    textSourceKind(result.kind),
    result.id,
    result.owner.kind === "issue" ? result.owner.key : undefined
  );
  // A delete takes the post's windows with its body, so a deleted post is no hit once the
  // delete commits. The mark is the backstop for a hit of one all the same, whose snippet is
  // empty: the deleted line stands where the snippet would, as it does in the referenced-by
  // list.
  const deleted = postDeletion(result);

  return (
    <PaletteOption
      active={active}
      id={optionId(result)}
      onSelect={onSelect}
      reference={nodeRouteFromHref(result.href)}
    >
      <div
        className={`flex min-w-0 items-center gap-2 text-xs ${
          muted ? textMutedOnSelectedCard : textSecondaryOnSurface
        }`}
      >
        {kind === undefined ? null : <RefKindIcon kind={kind} />}
        <span className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}>
          {kind === undefined ? result.kind : kindLabel(kind)}
        </span>
        {result.artifact === undefined ? null : (
          <span className="truncate">{result.artifact.name}</span>
        )}
        {/* A comment or a message its author edited says so, as its byline does. */}
        <EditedMark at={result.edited_at} className="shrink-0" deleted={deleted} />
      </div>
      <div className="mt-1 flex min-w-0" data-testid="search-hit-snippet">
        {deleted === null ? (
          <MarkdownPreview
            className={`min-w-0 flex-1 text-sm ${muted ? textMutedOnSelectedCard : textPrimaryOnSurface}`}
            highlightClassName={`rounded px-0.5 ${searchHitBg} ${searchHitText}`}
            lead={snippet.cut.start ? "…" : undefined}
            lines={2}
            linkLook="text"
            markdown={withoutDirectiveSyntax(source)}
            source={record}
            surface="search-result"
            trail={snippet.cut.end ? "…" : undefined}
            tree={snippet.tree}
          />
        ) : (
          <DeletedBody className="min-w-0 flex-1 text-sm" deleted={deleted} />
        )}
      </div>
      {result.ask === undefined ? null : (
        <AskHitStatus ask={result.ask} muted={muted} source={record} />
      )}
    </PaletteOption>
  );
}

/** A best match (R2: the item the words name, which ranks above the line on top) in the one look
 *  of a row that opens an item (`ItemRow`), since it stands under no owner's row. A hit that is
 *  the issue itself is one line, its title. Any other hit names its owner - the issue's title,
 *  and the document's name where it is one of the issue's - on the first line, and its snippet on
 *  a second line under it, one size below the title and in the grey of the bar's hints (review
 *  A2), cleaned as a hit under its owner's row is (`ResultOption`). */
function BestOption({
  active,
  onSelect,
  result,
}: {
  active: boolean;
  onSelect: () => void;
  result: SearchResult;
}): ReactNode {
  // The server reads a hit's snippet for display and marks the words that matched in the tree
  // it sends (`snippet_tree`), which a hit that is the issue itself draws as its title. The
  // sentinels the server leaves in `snippet` itself come out for the literal text a title falls
  // back to where no tree reached it, and mark the matched words of a whole title its headline
  // cut (`issueHitTitle`).
  const segments = useMemo(() => snippetSegments(result.snippet), [result.snippet]);
  const source = segments.map((segment) => segment.text).join("");
  // The route table carries the kind as a plain string; a kind this build has no icon for draws
  // none, rather than borrowing another kind's.
  const kind = searchResultKind(result.kind);
  const owner = ownerWords(result.owner);
  const status = result.owner.kind === "issue" ? result.owner.status : undefined;
  const muted = status === "done";
  // A document of an issue is named beside the issue it belongs to; a project document is the
  // owner itself.
  const document = result.owner.kind === "issue" ? result.artifact?.name : undefined;
  const itself = hitIsItsIssue(result);
  const titleTree = useMemo(() => {
    const title = itself ? issueHitTitle(result, segments) : undefined;
    if (title === undefined) return result.snippet_tree;
    return {
      type: "doc",
      content: [
        {
          type: "paragraph",
          content: title
            .filter((run) => run.text !== "")
            .map(({ mark, text }) => ({
              type: "text",
              text,
              ...(mark ? { marks: [{ type: "searchHighlight" }] } : {}),
            })),
        },
      ],
    };
  }, [itself, result, segments]);
  const words = muted ? textMutedOnSelectedCard : textPrimaryOnSurface;
  const highlight = `rounded px-0.5 ${searchHitBg} ${searchHitText}`;
  // The same tree, and so the same drawing, on every render of the row: a tree cut anew each
  // time would paint the snippet again on each.
  const snippet = useMemo(() => {
    const cut = snippetCut(result.snippet_tree);
    const drawn = snippetWithoutDirectives(result.snippet_tree);
    return { cut, tree: cut.start ? snippetFromSentence(drawn) : drawn };
  }, [result.snippet_tree]);
  // The hit's snippet is the record's own text, cut: the row names the record (`result.id`), the
  // kind it was indexed under, which is what a `#id` in it resolves against, and the issue it is
  // on, its owner. A kind that names no reference source - a project, an agent - answers none
  // (`textSourceKind`).
  const record = textSourceOf(
    textSourceKind(result.kind),
    result.id,
    result.owner.kind === "issue" ? result.owner.key : undefined
  );
  // A delete takes the post's windows with its body, so a deleted post is no hit once the
  // delete commits. The mark is the backstop for a hit of one all the same, whose snippet is
  // empty: the deleted line stands where the snippet would, as it does in the referenced-by
  // list.
  const deleted = postDeletion(result);
  let named: ReactNode;
  if (!itself) {
    named = (
      <span className={`min-w-0 flex-1 truncate text-sm ${words}`}>
        {document === undefined ? owner.name : `${owner.name} · ${document}`}
      </span>
    );
  } else {
    named = (
      <MarkdownPreview
        className={`min-w-0 flex-1 text-sm ${words}`}
        highlightClassName={highlight}
        lines={1}
        linkLook="text"
        markdown={source}
        source={record}
        surface="search-result"
        tree={titleTree}
      />
    );
  }

  return (
    <PaletteOption
      active={active}
      id={optionId(result)}
      onSelect={onSelect}
      reference={nodeRouteFromHref(result.href)}
      status={status}
    >
      <ItemRow
        // A comment or a message its author edited says so, as its byline does.
        after={<EditedMark at={result.edited_at} className="shrink-0" deleted={deleted} />}
        icon={kind === undefined ? null : <RefKindIcon kind={kind} />}
        itemKey={owner.key}
        muted={muted}
        name={named}
        status={status}
        under={
          itself ? null : (
            <div
              className={`mt-0.5 flex min-w-0 pl-6 text-xs ${textMutedOnSurface}`}
              data-testid="search-hit-snippet"
            >
              {deleted === null ? (
                <MarkdownPreview
                  className={`min-w-0 flex-1 ${textMutedOnSurface}`}
                  highlightClassName={highlight}
                  lead={snippet.cut.start ? "…" : undefined}
                  lines={1}
                  linkLook="text"
                  markdown={withoutDirectiveSyntax(source)}
                  source={record}
                  surface="search-result"
                  trail={snippet.cut.end ? "…" : undefined}
                  tree={snippet.tree}
                />
              ) : (
                <DeletedBody className="min-w-0 flex-1 text-xs" deleted={deleted} />
              )}
            </div>
          )
        }
      />
      {result.ask === undefined ? null : (
        <AskHitStatus ask={result.ask} muted={muted} source={record} />
      )}
    </PaletteOption>
  );
}

/** The glyph of a conversation of the bar, which Continue opens: two speech bubbles. */
function ConversationIcon(): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="size-4 shrink-0"
      fill="none"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth={1.75}
      viewBox="0 0 24 24"
    >
      <path d="M4 5h11v8H8l-4 3z" />
      <path d="M15 9h5v8l-3-2h-6v-2" />
    </svg>
  );
}

/** An action, project, recent item or Continue row: its icon where it has one (`icon`) and its
 *  label - or `body`, which draws the whole row, as a recent item's one look does - and an
 *  optional trailing hint. */
function CommandOption({
  active,
  body,
  hint,
  icon,
  id,
  label,
  onSelect,
}: {
  active: boolean;
  body?: ReactNode;
  hint: ReactNode;
  icon?: ReactNode;
  id: string;
  label: string;
  onSelect: () => void;
}): ReactNode {
  return (
    <PaletteOption
      active={active}
      id={id}
      layout={body === undefined ? "flex items-center gap-3" : "flex items-center"}
      onSelect={onSelect}
    >
      {body ?? (
        <>
          {icon === undefined ? null : (
            <span className={`flex shrink-0 ${textMutedOnSurface}`}>{icon}</span>
          )}
          <span className={`min-w-0 flex-1 truncate text-sm ${textPrimaryOnSurface}`}>{label}</span>
        </>
      )}
      {hint}
    </PaletteOption>
  );
}

/** A recent item (design v3, state 1) in the one look of a row that opens an item (`ItemRow`;
 *  review a12), since it is an item to open in the same list as a hit: an issue by its icon, its
 *  key, its title and its status, read from the issue's summary - the one read every reference
 *  to it shares - with the title its visit recorded until that read answers; a project document
 *  by its icon, its project's key and its name. */
function RecentItemRow({ item }: { item: RecentItem }): ReactNode {
  const issue = parseIssuePath(item.path)?.key;
  const summary = useQuery({ ...issueSummaryQuery(issue), enabled: issue !== undefined }).data;
  // A visit records an issue as its key and its title, and a document as its name.
  const recorded =
    issue !== undefined && item.label.startsWith(`${issue} `)
      ? item.label.slice(issue.length + 1)
      : item.label;
  return (
    <ItemRow
      icon={<RefKindIcon kind={issue === undefined ? "document" : "issue"} />}
      itemKey={issue ?? documentRouteOf(item.path, "")?.project ?? ""}
      muted={summary?.status === "done"}
      name={
        <span
          className={`min-w-0 flex-1 truncate text-sm ${textPrimaryOnSurface}`}
          data-testid="search-hit-title"
        >
          {summary?.title ?? recorded}
        </span>
      }
      status={summary?.status}
    />
  );
}

/**
 * A row of the line on top (R2): "✦ Ask ‹the words›", "✦ Do ‹the words›" or "✦ File as a new
 * issue in DEMO: ‹the words›", with "↵" on the right of the first row; the filing that is out,
 * with a spinner; or the filing that failed, its reason in words and Try again. While Jev may
 * still name another line (`deciding`), a faint dot pulses in a place the row always keeps, so
 * the row does not move when its label changes.
 */
function LineOption({
  active,
  deciding,
  first,
  onSelect,
  row,
}: {
  active: boolean;
  deciding: boolean;
  /** Whether this is the line's first row, which carries "↵". */
  first: boolean;
  onSelect: () => void;
  row: LineRow;
}): ReactNode {
  const failed = row.line === "failed";
  return (
    <PaletteOption
      active={active}
      id={row.id}
      layout="flex items-center gap-2"
      line={row.line}
      onSelect={onSelect}
    >
      {row.line === "filing" ? (
        <span
          aria-hidden="true"
          className={`size-3.5 shrink-0 animate-spin rounded-full border-2 border-current border-t-transparent ${textMutedOnSurface}`}
        />
      ) : (
        <span aria-hidden="true" className={`shrink-0 text-sm ${textMutedOnSurface}`}>
          ✦
        </span>
      )}
      <span
        className={`min-w-0 flex-1 truncate text-sm ${failed ? dangerText : textPrimaryOnSurface}`}
        data-testid="search-line-words"
      >
        <span className="font-medium">{row.label}</span>
        {row.words === "" ? null : (
          <>
            {row.line === "file" || failed ? ": " : " "}
            {row.words}
          </>
        )}
      </span>
      <span aria-hidden="true" className="flex w-2 shrink-0 justify-center">
        <span
          className={`size-1.5 rounded-full bg-current opacity-40 ${textMutedOnSurface} ${
            deciding ? "animate-pulse" : "invisible"
          }`}
          data-testid={deciding ? "search-line-deciding" : undefined}
        />
      </span>
      {failed ? (
        <span className={`shrink-0 text-xs font-medium ${textSecondaryOnSurface}`}>Try again</span>
      ) : null}
      {first && row.line !== "filing" ? <Key>↵</Key> : null}
    </PaletteOption>
  );
}

/* A group's heading is `aria-hidden` for the same reason as a hit's owner words: the group below
   takes its name from it, and a reader would otherwise hear it twice. The group is a `fieldset`
   named by the heading, so a reader hears where the list changes from one kind of row to the
   next. A group with no heading on the screen (the line on top, the best match) is named by
   `label` alone. `min-w-0` overrides a fieldset's UA `min-inline-size: min-content`, which a
   long row would otherwise widen the bar to. */
function ListGroup({
  children,
  heading,
  id,
  label,
}: {
  children: ReactNode;
  /** Whether the group draws its label as a heading above its rows. */
  heading: boolean;
  id: string;
  label: string;
}): ReactNode {
  if (!heading) {
    return (
      <fieldset aria-label={label} className="min-w-0" id={id}>
        {children}
      </fieldset>
    );
  }
  return (
    <>
      <div
        aria-hidden="true"
        className={`flex min-w-0 items-center border-b px-3 py-1.5 text-xs font-semibold ${borderDefault} ${surfaceMutedBg} ${textSecondaryOnSurfaceMuted}`}
        id={id}
        role="presentation"
      >
        {label}
      </div>
      <fieldset aria-labelledby={id} className="min-w-0">
        {children}
      </fieldset>
    </>
  );
}

/** One labelled run of command rows: the Recent items the bar opens on, the Actions of the page
 *  it opens on and that a query leads to, the More actions it matches further in, the Projects it
 *  names, or the New issue it offers for words that matched nothing. */
interface CommandSection {
  id: string;
  label: string;
  options: readonly {
    body?: ReactNode;
    hint: ReactNode;
    icon?: ReactNode;
    label: string;
    row: ActionRow | OfferRow | ProjectRow | RecentRow;
  }[];
}

function CommandGroup({
  activeId,
  onSelect,
  section,
}: {
  activeId: string | undefined;
  onSelect: (row: PaletteRow) => void;
  section: CommandSection;
}): ReactNode {
  if (section.options.length === 0) {
    return null;
  }
  return (
    <ListGroup heading id={section.id} label={section.label}>
      {section.options.map(({ body, hint, icon, label, row }) => (
        <CommandOption
          active={row.id === activeId}
          body={body}
          hint={hint}
          icon={icon}
          id={row.id}
          key={row.id}
          label={label}
          onSelect={() => onSelect(row)}
        />
      ))}
    </ListGroup>
  );
}

/** What a chip removed puts back, under the name `searchFilterChips` gives it. */
const FILTER_REMOVALS: Record<SearchFilterName, SearchBarFilters> = {
  author: { author: undefined },
  project: { project: undefined },
  status: { status: undefined },
  updatedSince: { updatedSince: undefined },
};

/** The two lists the panel opens inside itself, which `useNestedPanels` keeps one open at a time,
 *  as the Delivery page's facets are. */
type FilterListName = "project" | "status";

/**
 * The Filters button at the box's right and the panel it opens (design v3, state 1), which hold
 * the four narrowings the bar offers and the search syntax help, and show nowhere else: a
 * project, an issue status, an author and the day since which the item was last written, then
 * the three operator forms the words can use, each marked where the query uses it
 * (`SearchOperators`). The button says how many narrowings are on (`Filters · 2`), and "/" in the
 * empty box opens it, which is why the bar holds it open or shut (`open`). Each narrowing is a
 * control in the panel while it is unset and a chip in the bar's control row once the person has
 * chosen, and pressing that chip drops that narrowing alone and brings its control back.
 *
 * The project and the status are each picked from the shared list's single-pick mode
 * (`MultiSelect`), a text box over the options that narrows them. Its `option` rows join neither
 * the rows this dialog's arrows walk nor the hits a reader is told there are: the list is
 * portalled to the body (`AnchoredPanel`), outside the dialog. A pick takes its control out of
 * the panel, and the panel hands the focus to the control now at its place (`PanelTrigger`).
 *
 * The author is typed rather than picked: an item's author is a person's login or a session id,
 * and no list holds both. It is taken when the person presses Enter or leaves the box, so a
 * half-typed login never searches. The box keeps what was typed while the panel is shut, so a
 * reader who opens it again finds their half-typed login where they left it.
 *
 * The Project list is drawn from the bar's one read of the projects, which can still be out or
 * have failed when the panel opens: it says which, as the Agents composer's Issue list does,
 * rather than call an unread list empty.
 */
function SearchNarrowing({
  count,
  filters,
  onFilters,
  onOpenChange,
  open,
  operators,
  projects,
}: {
  /** How many narrowings are on. */
  count: number;
  filters: SearchBarFilters;
  onFilters: (filters: SearchBarFilters) => void;
  onOpenChange: (open: boolean) => void;
  open: boolean;
  /** The operator forms the answer read the query as using. */
  operators: readonly SearchOperator[];
  projects: UseQueryResult<Project[]>;
}): ReactNode {
  const projectList = projects.data ?? emptyProjects;
  const filterList = useNestedPanels<FilterListName>(open);
  const [author, setAuthor] = useState("");
  const set = (patch: SearchBarFilters) => onFilters({ ...filters, ...patch });
  const takeAuthor = () => {
    const typed = author.trim();
    if (typed !== "") {
      setAuthor("");
      set({ author: typed });
    }
  };

  return (
    <PanelTrigger
      align="right"
      count={count === 0 ? undefined : count}
      label="Filters"
      onOpenChange={(next) => {
        // Shutting the panel is leaving the author box as much as moving off it is: a press
        // outside takes the box away with the panel, before it could lose focus on its own.
        if (!next) takeAuthor();
        onOpenChange(next);
      }}
      open={open}
      stacked
      // Held down while the panel shows, as History is while it shows.
      triggerClassName={`${secondaryButtonCompact} ${expandedButtonAccent}`}
    >
      {() => (
        <>
          {filters.project === undefined ? (
            <MultiSelect
              {...filterList("project")}
              emptyMessage={
                projects.isPending
                  ? "Loading projects…"
                  : projects.isError || projectList.length > 0
                    ? "No project matches that."
                    : "No projects yet"
              }
              error={projects.isError ? "Could not load projects." : null}
              label="Project"
              mode="single"
              onPick={(project) => set({ project })}
              options={projectList.map((project) => project.key)}
              searchLabel="Search projects"
            />
          ) : null}
          {filters.status === undefined ? (
            <MultiSelect
              {...filterList("status")}
              emptyMessage="No status matches that."
              label="Status"
              mode="single"
              onPick={(status) => set({ status })}
              optionLabel={statusText}
              options={issueStatuses}
              searchLabel="Search statuses"
            />
          ) : null}
          {filters.author === undefined ? (
            <SearchBox
              className="w-full"
              label="Author"
              onBlur={takeAuthor}
              onChange={setAuthor}
              onKeyDown={(event) => {
                // An Enter that ends or moves an input method's composition is the composition's
                // (`inputMethodKey`).
                if (event.key === "Enter" && !inputMethodKey(event.nativeEvent)) {
                  event.preventDefault();
                  takeAuthor();
                }
              }}
              placeholder="Author"
              value={author}
            />
          ) : null}
          {/* The date box carries its words on screen: a browser draws an empty one as its own
              date format and nothing else, so without them a reader cannot tell what the box
              narrows. */}
          {filters.updatedSince === undefined ? (
            <label className={`inline-flex items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
              Updated since
              <input
                className={`min-h-11 rounded-lg border px-3 py-2 text-sm ${inputClasses(true)}`}
                onChange={(event) => {
                  // A date the reader types is taken once its year is whole: an engine reports a
                  // valid date from the year's first digit (Chromium reads `2` as the year 0002),
                  // and taking that would narrow to a year the reader is still typing and draw
                  // the box away from under their next keys. The box holds no `value` of its
                  // own: React puts a controlled empty value back after every change it hands
                  // here, so Chromium's date would be emptied at each digit before its year is
                  // whole.
                  const { value } = event.target;
                  if (Number(value.slice(0, 4)) >= 1000) {
                    set({ updatedSince: value });
                  }
                }}
                type="date"
              />
            </label>
          ) : null}
          <SearchOperators inUse={operators} />
        </>
      )}
    </PanelTrigger>
  );
}

export function SearchPalette({
  onClose,
  open,
}: {
  onClose: () => void;
  open: boolean;
}): ReactNode {
  // A chosen row runs once the bar is gone and `useDialog`'s cleanup has put focus back on
  // whatever opened it: run inside the bar, with focus in its input, it would act on nothing,
  // and a row that moves focus would have its move undone by that cleanup a moment later.
  const pendingAction = useRef<(() => void) | null>(null);
  // On the commit that closes the bar React runs every passive-effect cleanup, the unmounted
  // bar's `useDialog` among them, before any passive-effect setup, so focus is back by the
  // time this effect runs the chosen row.
  useEffect(() => {
    if (open) {
      return;
    }
    const run = pendingAction.current;
    pendingAction.current = null;
    run?.();
  }, [open]);

  if (!open) {
    return null;
  }
  // Mounted once per open, so each open starts on its first row with the rows, the chip and the
  // writing cursor of the page it opened over, and a closed bar holds no search of its own.
  return (
    <PaletteDialog
      onAction={(run) => {
        pendingAction.current = run;
      }}
      onClose={onClose}
    />
  );
}

function PaletteDialog({
  onAction,
  onClose,
}: {
  onAction: (run: () => void) => void;
  onClose: () => void;
}): ReactNode {
  const [query, setQuery] = useState("");
  // The highlight: the row the reader sees highlighted, held by its id, beside the search - its
  // words and its filters - whose hits were listed when it was last decided ("" while none
  // were). The two are one state because they are read together, to tell a fresh search's first
  // hits - which take the head - from the same search's hits re-ranked, which never move the
  // highlight. `left` is true while no row is highlighted because the row that was left the list
  // (a re-rank dropped it), so the highlight stays on no row until the reader arrows; a list that
  // simply held nothing to highlight leaves it false, so the search's first row to arrive takes
  // the head.
  const [active, setActive] = useState<{
    readonly hits: string;
    readonly id: string | null;
    readonly left: boolean;
  }>({ hits: "", id: null, left: false });
  // Whether the reader moved the highlight with an arrow on the list as it stands: since the
  // words last changed, and since a conversation last took the list's place. Until they do, a
  // best match or Continue arriving takes the head, so Enter opens it (R2).
  const [arrowed, setArrowed] = useState(false);
  // The filters and the search syntax help, behind the Filters button at the box's right, and
  // opened by "/" in the empty box (design v3, state 1).
  const [filtersOpen, setFiltersOpen] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  // The search's parts under the box, one column that scrolls where the window is too short for
  // the list's one row.
  const searchPartsRef = useRef<HTMLDivElement>(null);
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [filters, setFilters] = useState<SearchBarFilters>({});
  // What the bar will act on, in the words the chip in the box says it in, the two keys that
  // move it, and what each offer acts on at the level they have left it at.
  const scope = useBarScope(query);
  // The person's recent items, read from the browser on the render that opens the bar: the
  // keystroke that opens it makes no request, which is what lets the list paint at once. They
  // are one person's own, so they wait for the signed-in identity the shell already read.
  const whoAmI = useQuery(whoAmIQuery());
  const [recents] = useState(() => readRecents(whoAmI.data?.login));
  // The rows this open offers, decided on its first render, while focus is still on whatever
  // opened the bar, since `useDialog` moves it into the input after that commit. Read any
  // later and a `when()` that asks what is focused would answer "the bar", withdrawing every
  // row bound to the card, row or control the reader was actually on.
  const [actions] = useState(() => appKeymap.actions());
  const dialog = useDialog<HTMLDivElement>({ initialFocusRef: inputRef, onClose, open: true });
  // The conversation the bar holds, which outlives this open of it (`features/going`), and what
  // its box does with Enter and Esc there (`useAsk`), with the chip's context. The bar reads the
  // parts of it it draws here, so a frame of a run draws only the conversation again. Once a
  // conversation is open in the bar - the run Enter on Do starts, one Continue or History opens,
  // or one the bar opens on or takes back from the side (`setPlace`, `movePanel`) - the bar is
  // that conversation (design v2, state 2): the search parts leave it, it grows to 80% of the
  // window's height, and the box moves under the conversation. New brings the search back. Enter
  // on Ask, and Open in panel on the answer's card, open no conversation here: they start the
  // run, hand it to the panel at the side (`handOffToPanel`) and close the bar.
  const refusal = useGoingValue((of) => of.refusal);
  const talking = useGoingValue((of) => of.conversation !== undefined || of.starting !== undefined);
  const asksBack = useGoingValue(asksPerson);
  const asking = useAsk(scope.context);
  // The plan card's keys from the box while a plan waits: ↑ in the empty box moves into the card.
  const cardKeys = usePlanCardKeys();
  // Words sent while the run worked go once it no longer works.
  useSendQueued(asking.enter);
  const [history, setHistory] = useState(false);
  // New leaves the header, and the button pressed with it, so once the search is back the cursor
  // goes into the box, where typing then lands (review n2); the box shows again only once History
  // has gone too, so this waits for that render.
  const focusAfterNew = useRef(false);
  useLayoutEffect(() => {
    if (!focusAfterNew.current) return;
    focusAfterNew.current = false;
    inputRef.current?.focus();
  });
  const login = whoAmI.data?.login;
  // The filing Enter on File as a new issue starts, which outlives this open (`filing.ts`): it is
  // the line's first row while it is out or once it failed, and the bar closes once it is filed.
  // A filing belongs to the open it was made in: one kept from an earlier open of the bar is no
  // row of this one, and its end closes this open of the bar no more than it draws in it.
  const open = useBarOpen();
  const filingStore = useFiling();
  const filing = filingStore.filing?.open === open ? filingStore.filing : undefined;
  const filedSeen = useRef(filingStore.filed);
  useEffect(() => {
    if (filingStore.filed === filedSeen.current) {
      return;
    }
    filedSeen.current = filingStore.filed;
    if (filingStore.filed?.open === open && !talking) {
      onClose();
    }
  }, [filingStore.filed, onClose, open, talking]);
  // History's conversations, the newest first: the newest, where it is from the last hour, is
  // Continue on the empty box (design v3, Keys).
  const conversations = useQuery(goingConversationsQuery());
  // Every global key is masked while any dialog is open; the palette keeps `$mod+k` as its own
  // close, so pressing it twice still opens and closes, and `$mod+Shift+k` keeps the
  // conversation at the side of the page. In a browser whose page never gets Ctrl-Shift-K,
  // `$mod+k` pressed while the bar is open does that instead (`BROWSER_PANEL_KEYS`).
  useKeymap(DIALOG_SCOPE, [
    {
      id: "search-close",
      inEditable: true,
      keys: "$mod+k",
      label: BROWSER_PANEL_KEYS.sideByModK ? "Keep at the side" : "Close search",
      run: () =>
        BROWSER_PANEL_KEYS.sideByModK
          ? movePanel({ kind: "mod_k" }, login, BROWSER_PANEL_KEYS)
          : onClose(),
    },
    {
      id: "search-side",
      inEditable: true,
      keys: "$mod+Shift+k",
      label: "Keep at the side",
      run: () => movePanel({ kind: "mod_shift_k" }, login),
    },
  ]);
  const searchText = query.trim();
  const needle = searchText.toLowerCase();
  const tooShort = searchText.length < 2;
  // The server refuses a longer query; name the limit here instead of sending it.
  const tooLong = searchText.length > SEARCH_QUERY_MAX;
  // A conversation's box searches nothing: its words are the next turn.
  const queryEnabled = !tooShort && !tooLong && !talking;

  // The chips narrow every leg of the search, so they belong to the request and to the key the
  // answer is kept under: a chip added or dropped is another search, not another reading of
  // this one. The page's project rides with them, under the parameter that narrows nothing,
  // and it is the chip's project at every level because every level sits in one project.
  const narrowed = searchRequest(filters, scope.targets.project);
  // What is on, in the words the person chose it in: one chip of the control row for each
  // narrowing, and the control of that narrowing stays out of the panel while its chip is up.
  const narrowings = searchFilterChips(filters);
  // The one search the list reads, in two tiers the hook answers as one. Everything below -
  // the hits, the operators the query was read as and the message under the list - reads this
  // one answer, so the list, what it says about itself and the highlight all speak of one
  // search.
  const {
    asked,
    search,
    waiting: waitingForQuery,
  } = useTieredSearch({ enabled: queryEnabled, request: narrowed, words: searchText });
  // The project chip's own list, which every open of the bar offers, and the rows a query that
  // names a project leads to.
  const projects = useQuery(projectsQuery());
  const results = search.data?.results ?? emptyResults;
  // What search made of the words, read from its own answer rather than parsed a second time
  // here. A Dispatch that predates the field answers none, so the bar marks no operator.
  const operators = searchOperators(search.data?.query?.operators);
  // The hits answer the query in the box, so a query still in flight shows none: the message
  // below the list says which of "type more", "searching", "failed" and "nothing" it is. A
  // refresh that fails keeps the answer the query already has, and so do the hits.
  const showHits = queryEnabled && !waitingForQuery && search.data !== undefined;
  // A hit the words name exactly - its key or its title - ranks above the line on top, so Enter
  // opens it (R2); the rest stay under the issue or document they belong to.
  const bestResults = showHits
    ? results.filter((result) => namesHit(result, needle))
    : emptyResults;
  const bestRows = bestResults.map(
    (result): ResultRow => ({ id: optionId(result), kind: "result", result })
  );
  const hitGroups = showHits
    ? groupResults(results.filter((result) => !bestResults.includes(result))).map(
        ({ owner, results: ownerResults }) => ({
          owner,
          rows: ownerResults.map(
            (result): ResultRow => ({ id: optionId(result), kind: "result", result })
          ),
        })
      )
    : [];
  // Words that are an issue's key, or that name a hit, ask no model: their hit is the next step.
  const namesItsHit = ISSUE_KEY.test(searchText) || bestResults.length > 0;
  // Dispatch's short answer to the words (useGoingBar), which only reads, beside the hits, and
  // the line on top Jev chooses for them.
  const { answers, answersOff, decidesOff, deciding, enterLine, line, shownAnswer, stale } =
    useGoingBar({
      context: scope.context,
      enabled: queryEnabled && !namesItsHit,
      login,
      project: filters.project ?? "",
      showHits,
      words: searchText,
    });
  const project = scope.context.project ?? "";
  // Enter on a line of Ask or Do: a run of the agent on the words, which records the line pressed.
  // Ask - the line, and the card's Open in panel with it - is one action that hands the run to the
  // panel at the side (`useAsk.openInPanel`) and lets the bar go, so the words, the level and the
  // chip's context it starts with are the ones in the bar now, and it starts one run at most. Do
  // stays in the bar, which shows its conversation and its plan, as it always did.
  const askWords = (words: string, pressed: GoingLine) => {
    const started =
      pressed === "ask" ? asking.openInPanel(words, pressed) : asking.enter(words, pressed);
    if (started) {
      setQuery("");
      setActive({ hits: "", id: null, left: false });
    }
  };
  // Enter on File as a new issue, or on Try again: the filing, which the line's first row shows,
  // with the chip's context it is filed with: Try again files with the context the filing that
  // failed was made with, whatever the chip says since.
  const fileWords = (words: string, into: string, context: GoingContextInput) => {
    void fileNewIssue({ context, login, open, project: into, queryClient, words });
  };
  // The line on top (R2, R9): offered for any words the bar can ask about, and for an issue's
  // key once its hit is listed above it. Its first row reads Ask, Do or File as a new issue;
  // where it reads Do or File, the second reads Ask, so the person can always ask instead. A
  // filing that is out, or that failed, takes the first row's place, and nothing else changes.
  // A filing line draws no short answer: Enter on it files the words, and answers nothing.
  const lineOffered = answers && queryEnabled && (!ISSUE_KEY.test(searchText) || showHits);
  const askRow: LineRow = {
    id: LINE_ASK_OPTION_ID,
    kind: "line",
    label: "Ask",
    line: "ask",
    select: () => askWords(searchText, "ask"),
    words: searchText,
  };
  let lineRows: readonly LineRow[] = [];
  if (filing !== undefined && !talking) {
    const where = filing.project === "" ? "" : ` in ${filing.project}`;
    const first: LineRow =
      filing.state === "filing"
        ? {
            id: LINE_OPTION_ID,
            kind: "line",
            label: `Filing${where}…`,
            line: "filing",
            // A filing that is out takes no second press.
            select: () => {},
            words: "",
          }
        : {
            id: LINE_OPTION_ID,
            kind: "line",
            label: "Not filed",
            line: "failed",
            select: () => fileWords(filing.words, filing.project, filing.context),
            words: filing.reason,
          };
    lineRows = lineOffered ? [first, askRow] : [first];
  } else if (lineOffered) {
    // The filing says what it will do: the issue it files under, where the page's issue becomes
    // the parent (`filingParent`), else the project it files in.
    const parent = filingParent(searchText, scope.context.issue ?? "");
    const fileLabel =
      parent !== undefined
        ? `File as a new issue under ${parent}`
        : project === ""
          ? "File as a new issue"
          : `File as a new issue in ${project}`;
    const first: LineRow = {
      id: LINE_OPTION_ID,
      kind: "line",
      label: line === "file" ? fileLabel : line === "do" ? "Do" : "Ask",
      line,
      // Enter acts on the label on the screen at the moment of the key, never on a later one, and
      // the run records that line.
      select: () => {
        const pressed = enterLine();
        if (pressed === "file") {
          fileWords(searchText, project, scope.context);
        } else {
          askWords(searchText, pressed);
        }
      },
      words: searchText,
    };
    lineRows = line === "ask" ? [first] : [first, askRow];
  }
  const filingOnTop = lineRows[0]?.line === "file" || filing !== undefined;
  // Continue on the empty box: the newest conversation History lists, where it is from the last
  // hour, by its first question.
  const newest = conversations.data?.rows?.[0];
  const newestTitle = titleWords(newest?.runs?.at(-1)?.words ?? "");
  const continueRow: OfferRow | undefined =
    !talking &&
    searchText === "" &&
    newest !== undefined &&
    newestTitle !== "" &&
    Date.now() - Date.parse(newest.newest_at) < CONTINUE_WITHIN_MS
      ? {
          enter: "Continue",
          id: CONTINUE_OPTION_ID,
          kind: "offer",
          label: `Continue: ${newestTitle}`,
          select: () => openConversation(newest),
        }
      : undefined;
  // An action whose label the words start (every action of the page, while the box is empty) is
  // the one the reader is typing, and it is listed above the hits; with the box empty the page's
  // actions follow Continue and Recent (design v3, state 1), as main lists them. One that holds
  // the words further in (`issue` in `Close issue`) is a coincidence of words: it is listed below
  // the hits and never highlighted for the reader, so a search and Enter opens a hit, and such an
  // action runs only once the reader has arrowed to it.
  const actionOptions = actions
    .filter((action) => action.label.toLowerCase().includes(needle))
    .map((action) => {
      const row: ActionRow = {
        action,
        id: `search-option-action-${action.scope}-${action.id}`,
        kind: "action",
      };
      return {
        hint: <KeyHints keys={action.keys} />,
        label: action.label,
        leads: action.label.toLowerCase().startsWith(needle),
        row,
      };
    });
  // Projects are rows of the one bar, found by typing: `g p` opens the bar as every other key
  // does, and the project a reader is after is one or two letters away. With an empty box they
  // are not listed at all - every project of a Dispatch would bury the recent items and the
  // actions a reader opened the bar for.
  const projectRows: ProjectRow[] =
    needle === ""
      ? []
      : (projects.data ?? emptyProjects)
          .filter((project) => `${project.key} ${project.name}`.toLowerCase().includes(needle))
          .map((project) => ({
            id: `search-option-project-${project.key}`,
            kind: "project",
            project,
          }));
  // What the person opened lately, listed while the box is empty: with words in the box the
  // hits answer them, and a recent item among the hits would be the same item twice.
  const recentRows: RecentRow[] =
    searchText === ""
      ? recents.map((item) => ({ id: `search-option-recent-${item.path}`, item, kind: "recent" }))
      : [];
  // A conversation lists nothing: the box under it says what Enter does there.
  const commandSections: readonly CommandSection[] = talking
    ? []
    : [
        {
          id: RECENT_GROUP_ID,
          label: "Recent",
          options: recentRows.map((row) => ({
            body: <RecentItemRow item={row.item} />,
            hint: null,
            label: row.item.label,
            row,
          })),
        },
        {
          id: ACTIONS_GROUP_ID,
          label: "Actions",
          options: actionOptions.filter((option) => option.leads),
        },
        {
          id: PROJECTS_GROUP_ID,
          label: "Projects",
          options: projectRows.map((row) => ({
            hint: (
              <span className={`shrink-0 font-mono text-xs ${textMutedOnSurface}`}>
                {row.project.key}
              </span>
            ),
            label: row.project.name,
            row,
          })),
        },
      ];
  // Words the search answered and matched nothing with, on a Dispatch with no deciding model,
  // whose line on top is always Ask and never File (R2): the bar offers them as a new issue,
  // under the hits and the rows this page can do, as main does, so a person can still file from
  // the bar (design v3, state 13). Where a deciding model is configured, the line's File is the
  // way to file, so nothing is offered here, however late that model answers.
  const newIssue = useNewIssueOffer({
    offered: decidesOff && showHits && results.length === 0,
    onOpen: (key) => {
      navigate(buildIssuePath({ key, kind: "issue" }));
      onClose();
    },
    project: scope.targets.project,
    words: searchText,
  });
  // The two sections at the foot of the list, which the highlight never starts on and the arrows
  // reach last: a More action is a coincidence of words, and the new issue is a write, so neither
  // runs on an Enter the reader meant for the row above. The new issue comes under the actions,
  // because it is the row that writes.
  const newIssueRow = newIssue?.row;
  const newIssueSection: CommandSection = {
    id: NEW_ISSUE_GROUP_ID,
    label: "New issue",
    options:
      newIssueRow === undefined ? [] : [{ hint: null, label: newIssueRow.label, row: newIssueRow }],
  };
  const moreActions: CommandSection = {
    id: MORE_ACTIONS_GROUP_ID,
    label: "More actions",
    options: actionOptions.filter((option) => !option.leads),
  };
  // The order the foot is listed and walked in, written once: the rows, the count the highlight
  // rule keeps off them, and the groups drawn below all read this one array.
  const footSections: readonly CommandSection[] = talking ? [] : [moreActions, newIssueSection];
  const footRows = footSections.reduce((count, section) => count + section.options.length, 0);
  const hitRows = hitGroups.flatMap((group) => group.rows);
  // What the list shows, in the order it shows it: the arrows walk this same list.
  const rows: PaletteRow[] = [
    ...bestRows,
    ...lineRows,
    ...(continueRow === undefined ? [] : [continueRow]),
    ...commandSections.flatMap((section) => section.options.map((option) => option.row)),
    ...hitRows,
    ...footSections.flatMap((section) => section.options.map((option) => option.row)),
  ];
  // The hits the highlight is decided against: the search they answer, its words and its
  // filters, "" while the list holds none. A filter chip is another search under another key
  // (`useTieredSearch`), so a chip's change whose answer is held already is that search's first
  // hits, never this one's re-ranked.
  const listedSearch = showHits ? JSON.stringify([asked, narrowed]) : "";
  // The highlight is the row the reader sees highlighted, held by its id: hits arriving below
  // the rows above them, or an answer re-ranking them, leave it on that row; the line on top
  // keeps its id whatever it reads, so its label changing leaves it there too. The first hits of
  // a query send it to the head, and the head it lands on is then adopted, so a row coming back
  // does not take the highlight back from it. The head is the first row unless that is a foot
  // row - a More action or the new issue, which come last: the highlight never moves to a write
  // by itself, and with only foot rows listed nothing is highlighted, and Enter runs nothing
  // until the reader arrows. A best match or Continue take the highlight as they arrive until
  // the reader arrows: the row they are, which a filing of this open that is still out comes
  // above (it takes no press).
  // A row that leaves a list already answering this query takes the highlight with it and
  // leaves it on no row: that is the pause's fused order dropping a row the keystrokes' hits
  // held. Sending it to the head there would move the selection under the reader's hand, and
  // the next Enter would open a row they never chose - so nothing is highlighted, and Enter
  // does nothing, until they arrow.
  // A list that answered this search and highlighted nothing - it held no row to highlight, a
  // stale empty answer among them - held no row the reader could lose, so the first row of that
  // search to arrive takes the head (INFRA-2952).
  // A conversation takes the list's place, and the search that New brings back is a new list:
  // the arrows the reader pressed on the old one are let go with it, as new words let them go.
  // So Continue takes the empty box's highlight again, whichever row Enter opened the
  // conversation in the bar from - Do after an arrow, or Continue itself - rather than the head,
  // which a filing still out holds and where Enter does nothing. Enter on Ask after an arrow
  // opens none here: it hands the run to the panel at the side and closes the bar, whose next
  // open is mounted anew, with no arrow pressed.
  if (talking && arrowed) {
    setArrowed(false);
  }
  const chosen = rows.findIndex((row) => row.id === active.id);
  const held = active.id !== null || active.left;
  const reranked = listedSearch !== "" && listedSearch === active.hits && held;
  const head = rows.length > footRows ? 0 : -1;
  const lead = arrowed
    ? -1
    : bestRows.length > 0
      ? 0
      : continueRow === undefined
        ? -1
        : rows.indexOf(continueRow);
  const activeIndex = lead !== -1 ? lead : chosen === -1 && !reranked ? head : chosen;
  const activeRow = rows[activeIndex];
  const shownId = activeRow?.id ?? null;
  const left = shownId === null && reranked;
  if (shownId !== active.id || listedSearch !== active.hits || left !== active.left) {
    setActive({ hits: listedSearch, id: shownId, left });
  }
  // Focus stays in the input while the arrows move the highlight (`aria-activedescendant`), so
  // the list scrolls the highlighted row into its own view, and the column under the box into
  // its own where a short window scrolls it: an issue page offers more rows than the list's
  // height holds. It does again whenever the list's height or the column's changes (the window
  // shrinks, or the message under the list grows), since the row did not move but its view did.
  useLayoutEffect(() => {
    if (shownId === null) {
      return;
    }
    const keepInView = () => {
      document.getElementById(shownId)?.scrollIntoView({ block: "nearest" });
    };
    keepInView();
    const list = listRef.current;
    if (list === null) {
      return;
    }
    const sizes = new ResizeObserver(keepInView);
    sizes.observe(list);
    if (searchPartsRef.current !== null) {
      sizes.observe(searchPartsRef.current);
    }
    return () => sizes.disconnect();
  }, [shownId]);

  useCloseOnNavigation(true, onClose);
  // A link in the bar - an item's chip in an answer or the short answer - opens its item with the
  // router, which closes the bar as any other navigation does, rather than loading the whole page.
  const follow = useFollowInPlace();

  const selectRow = (row: PaletteRow) => {
    if (row.kind === "offer" || row.kind === "line") {
      // Continue, the New issue offer and the line on top, pressed through the row they carry
      // rather than through what they start, and the bar is not closed here: Continue opens its
      // conversation in the bar and Do starts one there, which the bar stays open to show; the new
      // issue is made with the bar still open, so the person reads what was made, and that press
      // closes the bar itself where it opens the issue; a filing is the line's first row while it
      // is out or once it failed, and the bar closes once it is filed; and Ask hands its run to
      // the panel at the side (`useAsk.openInPanel`), which closes the bar.
      row.select();
      return;
    }
    if (row.kind === "action") {
      onAction(row.action.run);
    } else {
      navigate(
        row.kind === "project"
          ? buildProjectPath({ kind: "project", project: row.project.key })
          : row.kind === "recent"
            ? row.item.path
            : row.result.href
      );
    }
    onClose();
  };
  // While the search the list shows is out - its first answer, or a refetch behind a cached one
  // - its hits can still arrive above the More actions, so an arrow aimed at the list now would
  // land on whatever row then sits there: until the search answers, the arrows walk only the
  // rows above the More actions, and with none they do nothing. A highlight the reader moved
  // into the More actions after an earlier answer keeps moving over the whole list. The pause's
  // search running behind a painted keyword list is not this case: it re-ranks rows already
  // listed, and the highlight is held by id, so no row moves out from under an arrow.
  // `isFetching`, not the message's `isPending`: with a stale cached answer `isPending` is false
  // while the refetch runs.
  const searchOut = waitingForQuery || (queryEnabled && search.isFetching);
  const firstMore = rows.length - footRows;
  const walkable = searchOut && activeIndex < firstMore ? firstMore : rows.length;
  // The box before Enter: "/" in the empty box opens the filters, Tab and Backspace move the
  // chip, the arrows walk the list, and Enter acts on the highlighted row. A key of an input
  // method's composition is the input method's (`inputMethodKey`): the Enter that confirms a
  // conversion opens no row.
  const handleSearchKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (inputMethodKey(event.nativeEvent)) return;
    if (event.key === "/" && query === "") {
      // In a box with words, "/" is a character.
      event.preventDefault();
      setFiltersOpen(true);
    } else if (scope.handleKey(event)) {
      // The browser's own move is what this cancels: Tab narrows the chip rather than leaving
      // the box.
      event.preventDefault();
    } else if ((event.key === "ArrowDown" || event.key === "ArrowUp") && walkable > 0) {
      event.preventDefault();
      const delta = event.key === "ArrowDown" ? 1 : -1;
      // From no highlight, Down takes the first row and Up the last.
      const next =
        activeIndex === -1
          ? delta === 1
            ? 0
            : walkable - 1
          : stepActive(activeIndex, delta, walkable);
      setArrowed(true);
      setActive({ hits: listedSearch, id: rows[next]?.id ?? null, left: false });
    } else if (
      event.key === "Enter" &&
      !event.metaKey &&
      !event.ctrlKey &&
      activeRow !== undefined
    ) {
      event.preventDefault();
      selectRow(activeRow);
    }
  };
  // The box under the conversation (design v2, Keys): ↑ in the empty box moves into the plan
  // card that waits; Esc in the empty box while the run works stops the run, and any other Esc
  // is the dialog's, which closes the bar while a run goes on; Enter answers the plan that waits
  // (approves it from the empty box, revises it with words), does nothing before the plan is
  // read or while its read says Dispatch withdrew it, holds words sent while the run works until
  // it ends, and otherwise asks the next turn. A key of an input method's composition is the
  // input method's: the Enter that confirms a conversion sends nothing.
  const handleTalkKey = (event: KeyboardEvent<HTMLInputElement>) => {
    if (inputMethodKey(event.nativeEvent)) return;
    if (cardKeys(event)) {
      return;
    }
    if (event.key === "Escape" && query === "" && conversationWorking()) {
      // The dialog's own Escape, which closes the bar, never hears this one.
      event.preventDefault();
      event.stopPropagation();
      asking.escape("");
    } else if (scope.handleKey(event)) {
      event.preventDefault();
    } else if (event.key === "Enter" && !event.shiftKey && !event.metaKey && !event.ctrlKey) {
      event.preventDefault();
      if (asking.enter(query)) {
        setQuery("");
      }
    }
  };
  // What the box says it does now (design v2, rule 5).
  const placeholder = boxWords({
    asked: asksBack,
    asking,
    open: talking,
    search: true,
  });
  // The box, one element in every state of the bar, so the person's focus and words stay in it as
  // the bar turns from the search into the conversation and back. Before Enter it is the combobox
  // of the list under it; as a conversation it is a plain text box. It is the shared search box
  // (`SearchBox`), bare, since its frame draws the border: the frame holds the context chip on
  // its left and, before Enter, the Filters button on its right; each keeps its place, empty
  // where it does not show, so the input is the same element throughout. It takes words past the
  // search's limit, which the bar names under the list rather than cutting them.
  const box = (
    <BoxFrame chip={scope.chip} chipTitle={scope.chipTitle}>
      <SearchBox
        bare
        className={boxInputClassName}
        combobox={
          talking
            ? undefined
            : { activeId: activeRow?.id, controls: "search-results", expanded: rows.length > 0 }
        }
        data-testid="bar-box"
        label={talking ? "Ask the command bar" : "Search"}
        maxLength={null}
        onChange={(value) => {
          setQuery(value);
          // A new query is a new list, so the highlight is undecided again and the first
          // hits to answer it take the head.
          setActive({ hits: "", id: null, left: false });
          setArrowed(false);
          // A failed filing belongs to the words it failed on.
          if (filing?.state === "failed") {
            dropFiling();
          }
        }}
        onKeyDown={talking ? handleTalkKey : handleSearchKey}
        placeholder={placeholder}
        ref={inputRef}
        style={boxInputStyle(placeholder)}
        type={talking ? "text" : "search"}
        value={query}
      />
      {talking ? null : (
        <SearchNarrowing
          count={narrowings.length}
          filters={filters}
          onFilters={setFilters}
          onOpenChange={setFiltersOpen}
          open={filtersOpen}
          operators={operators}
          projects={projects}
        />
      )}
    </BoxFrame>
  );
  // What Enter does on the highlighted row, for the foot.
  const enterHint = enterWord(activeRow);

  return (
    <>
      <div aria-hidden="true" className={`fixed inset-0 z-40 ${backdrop50}`} onClick={onClose} />
      {/* The wrapper holds the palette's place: below the top bar, to the screen's foot (1rem
          short of it from `xl`). The dialog is a column no taller than that. Before Enter the
          box is on top and the search's parts under it are one column (`searchPartsRef`) that
          takes the room the frame leaves under the box, whatever the box holds: the list takes
          what the parts above and below it leave in that column, down to one row's height, and
          a window too short for that row scrolls the column instead, so the row the reader is
          on still shows whole and the key hints and then the message under the list are what
          leave the view. As a conversation it is 80% of the window's height, the conversation
          scrolls inside it, and the box sits at its foot. The children keep their places in
          both states (an empty place where a part does not show), so the box is the same
          element throughout. The frame clips (`overflow-clip`) rather than hides what overflows
          it: a hidden overflow is still a scroll that a script, a focus or a scroll into view
          moves, and one moved the header and the box out of the frame (review B1). Only the
          list, the column under the box and the conversation scroll; the box never moves. */}
      <div className="pointer-events-none fixed inset-x-0 top-12 bottom-0 z-50 flex items-start justify-center xl:top-20 xl:bottom-4 xl:px-4">
        {/* biome-ignore lint/a11y/useKeyWithClickEvents: a link's Enter is a click, which this
            follows in place: an item's chip in an answer opens its item with the router, rather
            than loading the whole page again. */}
        <div
          aria-label="Search"
          aria-modal="true"
          className={`pointer-events-auto relative flex max-h-full w-full flex-col overflow-clip rounded-b-lg border shadow-2xl xl:max-w-[720px] xl:rounded-lg ${talking ? "h-[80vh]" : ""} ${card} ${borderDefault}`}
          data-state={talking ? "conversation" : "search"}
          onClick={follow}
          ref={dialog.containerRef}
          role="dialog"
        >
          {/* The header names the conversation, or History while it shows (design v3, states 2
              and 11). The search before Enter has none: its box is on top, as Raycast's and
              Notion's are, with History and the move in its foot (state 1). */}
          {talking || history ? (
            <ConversationHeader
              history={history}
              move="Keep at the side"
              onClose={onClose}
              onHistory={() => setHistory((shown) => !shown)}
              onMove={() => movePanel({ kind: "keep_at_side" }, login)}
              onNew={() => {
                newConversation();
                setHistory(false);
                focusAfterNew.current = true;
              }}
            />
          ) : null}
          {/* The conversation the bar holds, above its box: the runs, their steps and their
              plan; or History in their place, with its own filter box the only box on the
              screen. */}
          {history ? (
            <div className="min-h-0 flex-1 overflow-y-auto">
              <History onOpen={() => setHistory(false)} />
            </div>
          ) : talking ? (
            <ConversationView
              answering={asking.answering}
              onContinue={asking.onContinue}
              onRetry={asking.retry}
            />
          ) : null}
          <div
            className={`shrink-0 p-3 ${history ? "hidden" : ""} ${talking ? "border-t" : "border-b"} ${borderDefault}`}
          >
            {/* The bar's one row of controls (`ControlRow`): the box, with the Filters button
                at its right before Enter, and under it a chip for each narrowing that is on. The
                row stands in every state of the bar, so the box in it stays the same element. */}
            <ControlRow
              chips={
                talking
                  ? undefined
                  : filterChips(
                      narrowings.map(({ label, name }) => ({
                        key: name,
                        label,
                        remove: () => setFilters({ ...filters, ...FILTER_REMOVALS[name] }),
                      }))
                    )
              }
              search={
                <div className="min-w-0 flex-1">
                  <BoxRow
                    box={box}
                    hint={talking ? boxHint(asking, query) : undefined}
                    onStop={asking.stop}
                  />
                </div>
              }
            />
            {refusal === undefined ? null : (
              <p className={`mt-2 text-xs ${dangerText}`} role="alert">
                {refusal}
              </p>
            )}
          </div>
          {talking || history ? null : (
            <div className="flex min-h-0 flex-col overflow-y-auto" ref={searchPartsRef}>
              {/* On a Dispatch with no answering model, the one sentence that says so, in the
                  line on top's place (design v3, state 13); search works as usual. */}
              {answersOff && queryEnabled ? (
                <p
                  className={`shrink-0 border-b px-3 py-3 text-sm ${borderDefault} ${textSecondaryOnSurface}`}
                  data-testid="going-cannot-answer"
                >
                  {NO_MODEL_WORDS}
                </p>
              ) : null}
              {/* The short answer of the pause (R3), as a compact card above the hits and outside
                  the list, with Open in panel; none under a filing line, whose Enter files the
                  words and answers nothing, and no button on an answer to words typed past, nor
                  on the last answer drawn again while the box is empty. */}
              {shownAnswer === undefined || filingOnTop ? null : (
                <AnswerPanel
                  answer={shownAnswer}
                  onOpenInPanel={
                    stale || shownAnswer.replayed || searchText === ""
                      ? undefined
                      : () => askWords(searchText, "ask")
                  }
                  stale={stale}
                />
              )}
              {rows.length === 0 ? null : (
                // The list takes the room its column leaves it once the parts above it (the
                // no-model line, the answer card) and under it are drawn, which ends where the
                // frame clips, and keeps one row's height (5rem) where that room is less: there
                // the column scrolls, and the arrows' scroll into view brings the row into the
                // column's view as well as the list's, so a window that holds the box and one row
                // shows the row the reader is on whole, whatever stands above the list.
                <div
                  className="max-h-[60vh] min-h-20 flex-1 overflow-y-auto"
                  id="search-results"
                  ref={listRef}
                  role="listbox"
                >
                  {bestRows.length === 0 ? null : (
                    <ListGroup heading={false} id="search-group-best" label="Best match">
                      {bestRows.map((row) => (
                        <BestOption
                          active={activeRow?.id === row.id}
                          key={row.id}
                          onSelect={() => selectRow(row)}
                          result={row.result}
                        />
                      ))}
                    </ListGroup>
                  )}
                  {lineRows.length === 0 ? null : (
                    <ListGroup heading={false} id="search-group-line" label="Next step">
                      {lineRows.map((row, index) => (
                        <LineOption
                          active={activeRow?.id === row.id}
                          deciding={index === 0 && deciding && filing === undefined}
                          first={index === 0}
                          key={row.id}
                          onSelect={() => selectRow(row)}
                          row={row}
                        />
                      ))}
                    </ListGroup>
                  )}
                  {continueRow === undefined ? null : (
                    <CommandOption
                      active={activeRow?.id === continueRow.id}
                      hint={null}
                      icon={<ConversationIcon />}
                      id={continueRow.id}
                      label={continueRow.label}
                      onSelect={() => selectRow(continueRow)}
                    />
                  )}
                  {commandSections.map((section) => (
                    <CommandGroup
                      activeId={activeRow?.id}
                      key={section.id}
                      onSelect={selectRow}
                      section={section}
                    />
                  ))}
                  {hitGroups.map(({ owner, rows: ownerRows }) => {
                    const header =
                      owner.kind === "issue"
                        ? {
                            id: `issue:${owner.key}`,
                            key: owner.key,
                            name: owner.title,
                            status: owner.status,
                          }
                        : {
                            id: `document:${owner.artifact_id}`,
                            key: owner.project,
                            name: owner.name,
                            status: undefined,
                          };
                    const muted = header.status === "done";
                    return (
                      <Fragment key={header.id}>
                        {/* The group's own row: a label for the hits under it, on the recessed
                            surface so the hits read as the list and this reads as its heading.
                            Its status is the lifecycle label the rest of the product shows -
                            `Needs review`, never the raw value. */}
                        {/* `aria-hidden` because the group below takes its name from this row:
                            a reader would otherwise hear the owner twice, once as the row and
                            once as the group's label. `aria-labelledby` computes a name from a
                            hidden element, so the group keeps it. */}
                        <div
                          aria-hidden="true"
                          aria-label={`${header.key}: ${header.name}`}
                          className={`flex min-w-0 items-center gap-2 border-b px-3 py-1.5 text-xs ${borderDefault} ${surfaceMutedBg} ${
                            muted ? textMutedOnSurfaceMuted : ""
                          }`}
                          data-status={header.status}
                          id={`search-group-${header.id}`}
                          role="presentation"
                        >
                          <span className="font-semibold">{header.key}</span>
                          <span
                            className={`min-w-0 flex-1 truncate ${
                              muted ? textMutedOnSurfaceMuted : textSecondaryOnSurfaceMuted
                            }`}
                          >
                            {header.name}
                          </span>
                          {header.status === undefined ? null : (
                            <span
                              className={`rounded-full px-2 py-0.5 font-medium ${badgeLow.bg} ${badgeLow.text}`}
                            >
                              {statusText(header.status)}
                            </span>
                          )}
                        </div>
                        {/* The hits of one owner are a group named by the row above them, so a
                            reader who cannot see that row still hears which issue or document a
                            hit belongs to. The options stay direct children of the group and the
                            listbox's own arrow navigation is untouched. A `fieldset` carries the
                            group role implicitly; `min-w-0` overrides its UA `min-inline-size:
                            min-content`, which a long hit would otherwise widen the palette to. */}
                        <fieldset aria-labelledby={`search-group-${header.id}`} className="min-w-0">
                          {ownerRows.map((row) => (
                            <ResultOption
                              active={activeRow?.id === row.id}
                              key={row.id}
                              muted={muted}
                              onSelect={() => selectRow(row)}
                              result={row.result}
                            />
                          ))}
                        </fieldset>
                      </Fragment>
                    );
                  })}
                  {footSections.map((section) => (
                    <CommandGroup
                      activeId={activeRow?.id}
                      key={section.id}
                      onSelect={selectRow}
                      section={section}
                    />
                  ))}
                </div>
              )}
              {/* What the bar says about the new issue it made from these words, above the
                  message that says the search found none: each is an answer to that search. */}
              {newIssue?.said}
              {searchText.length === 1 ? (
                <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>
                  Type at least 2 characters
                </p>
              ) : tooLong ? (
                <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>
                  Search with a short phrase: {searchText.length} characters is over the{" "}
                  {SEARCH_QUERY_MAX}-character limit
                </p>
              ) : !queryEnabled ? null : waitingForQuery || search.isPending ? (
                <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>Searching…</p>
              ) : search.isError && search.data === undefined ? (
                <div className="shrink-0 p-3">
                  <QueryError
                    message="Search failed."
                    onRetry={() => {
                      void search.refetch();
                    }}
                    retrying={search.isFetching}
                  />
                </div>
              ) : results.length === 0 ? (
                <p className={`shrink-0 px-3 py-3 text-sm ${textMutedOnSurface}`}>
                  No results for &quot;{searchText}&quot;
                </p>
              ) : search.data?.total !== undefined && search.data.total > results.length ? (
                // The bar lists one page of hits, so a query matching more says so rather than
                // looking complete.
                <p className={barNoteClassName}>
                  Showing the best {results.length} of {search.data.total} matches
                </p>
              ) : projects.isError && needle !== "" ? (
                // Projects are rows of this list too, so a query that found none has to say
                // whether that is because the list of them never arrived.
                <div className="shrink-0 p-3">
                  <QueryError
                    message="Could not load projects."
                    onRetry={() => {
                      void projects.refetch();
                    }}
                    retrying={projects.isFetching}
                  />
                </div>
              ) : null}
              {/* The foot: the keys of the highlighted row alone (design v3, state 1), quieter
                  than any row above them - and only where there is a pointer that can hover,
                  since a touch reader has none of these keys - then History and the move, which
                  the search keeps here rather than in a header of their own. Esc in the hints is
                  the one Close the search shows. */}
              <div
                className={`flex shrink-0 items-center gap-2 border-t px-3 py-1.5 ${borderDefault}`}
                data-testid="search-foot"
              >
                <p
                  className={`hidden min-w-0 flex-1 text-xs pointer-fine:block ${textMutedOnSurface}`}
                  data-testid="search-keyboard-hints"
                >
                  {enterHint === undefined ? null : (
                    <>
                      <Key>↵</Key> {enterHint} ·{" "}
                    </>
                  )}
                  <Key>Esc</Key> Close
                </p>
                <span className="flex-1 pointer-fine:hidden" />
                <button
                  aria-pressed={false}
                  className={secondaryButtonCompact}
                  data-testid="going-history-button"
                  onClick={() => setHistory(true)}
                  type="button"
                >
                  History
                </button>
                <MoveButton
                  move="Keep at the side"
                  onMove={() => movePanel({ kind: "keep_at_side" }, login)}
                />
              </div>
            </div>
          )}
        </div>
      </div>
    </>
  );
}
