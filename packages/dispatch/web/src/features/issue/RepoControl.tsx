import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useRef, useState } from "react";

import { api, apiErrorMessage } from "../../api/client";
import { MultiSelect } from "../../components/MultiSelect";
import { QueryError } from "../../components/QueryError";
import { usePageWords, useTooltips } from "../../components/tooltips";
import {
  badgeLow,
  badgePrimary,
  dangerText,
  focusBorder,
  focusVisibleRing,
  inputClasses,
  pageFailedText,
  pageHairBorder,
  pageInkText,
  pageMutedText,
  pagePaperBg,
  pageQuietText,
  textSecondaryHoverToPrimary,
  textSecondaryOnSurface,
} from "../../theme/classes";
import { focusOnDocument } from "../shell/roving";
import {
  badgeReachHandlers,
  badgeSelectBadge,
  badgeSelectRailTrigger,
  badgeSelectRailWrapper,
} from "./badge-select";
import { pickLook } from "./issue-field-looks";
import { useIssueRepo } from "./useIssueRepo";

/** The option of the shared list that clears the issue's own repository, so it resolves to its
 *  nearest ancestor's, else to the project's default, again. */
const INHERIT = "";

/** The grammar a repository GitHub could hold is written in, which is the grammar the server
 *  holds an issue's repository to (`parseIssueRepo`, `issue_repo.go`, through
 *  `refs.ParseGitHubRepo`, whose rule is copied from the daemon's `ghrepo`): an owner is a letter
 *  or digit and then up to 38 letters, digits, hyphens and underscores - as GitHub serves logins,
 *  which is looser than its sign-up form, since `hello--world`, `artofcode-` and a managed user's
 *  `mona-cat_octo` are all served - and a repository name is at most 100 letters, digits, `.`,
 *  `-` and `_`, and is neither `.` nor `..`. It is written here rather than taken from
 *  `ownerRepoPattern`, the looser `owner/name` shape the settings forms share, because those
 *  forms write a repository mapping, which the server holds to that looser shape alone.
 *
 *  The hyphen is written `\-` in both classes because this is also the box's `pattern`, which a
 *  browser compiles with the `v` flag, and under that flag a bare hyphen in a class is a syntax
 *  error: the browser then ignores the attribute and logs the error, and only the check below
 *  holds the box. */
const ownerRepoPattern = "[A-Za-z0-9][A-Za-z0-9_\\-]{0,38}/(?!\\.{1,2}$)[A-Za-z0-9._\\-]{1,100}";

/** `ownerRepoPattern` as a test of a whole value: the check this control makes before it writes,
 *  so a typo shows at the box, or in the list, rather than coming back as the server's 400 a round
 *  trip later. */
const ownerRepo = new RegExp(`^(?:${ownerRepoPattern})$`);

/** What the box, or the list, says about a value GitHub could hold no repository at. The server
 *  says the same thing in its own words for anything that reaches it. */
const NOT_OWNER_NAME = "Write the repository as owner/name.";

/** What clearing the issue's own repository does: the clear control's `title`, and inside
 *  `NoTooltips`, where no control carries a `title`, that control's description. */
const CLEAR_EXPLAINS = "Clear it, and the issue inherits a repository again";

/** Inside `NoTooltips`, the new issue page's colours for the box, its clear control and the text a
 *  row of Details draws in the box's place: the box as `AskCard`'s answer box draws them - the
 *  page's hairline rule, its paper and its ink, the repository the issue inherits (the
 *  placeholder) in the quiet grey, and `main`'s focus rule, the application's one focus indicator
 *  - the clear control, drawn on the box's paper, or in a row of Details on the row's own ground
 *  under the box, in the muted grey that turns ink under the pointer, "None" in the quiet grey
 *  every empty row of Details draws, which turns muted under the pointer as the page's quiet
 *  controls do, and the repository's text as the box draws it: the issue's own in the ink, and an
 *  inherited one in the quiet grey of the placeholder. */
const ON_PAGE = {
  box: `${pageHairBorder} ${pagePaperBg} ${pageInkText} placeholder:text-page-quiet ${focusBorder}`,
  clear: `${pageMutedText} hover:text-page-ink`,
  inherited: pageQuietText,
  none: `${pageQuietText} hover:text-page-muted`,
  own: pageInkText,
} as const;

/** The name the repository control answers to, in its words on every page: the property it sets
 *  and the issue it is on, so a voice user saying what they read reaches it (WCAG 2.5.3). The
 *  new issue page's add menu reaches this control by this name rather than drawing a second one,
 *  so a word changed here changes both sides at once. */
export function repoControlName(issueKey: string): string {
  return `Repository of ${issueKey}`;
}

/**
 * The issue's repository. It has two shapes, one per surface: the shared list
 * (`RepositoryList`) on every page today, and `main`'s box (`RepositoryBox`) inside `NoTooltips`,
 * the new issue page's design, which that page's own tree (INFRA-1196) decides. `inRow` acts
 * inside `NoTooltips` alone, so only the box reads it. `RepositoryBox` is `main`'s text
 * unchanged: its arms for outside `NoTooltips` cannot run here, and it stays apart from
 * `RepositoryList` until INFRA-1196 decides the new page's repository.
 */
export function RepoControl({
  disabled = false,
  inRow = false,
  issueKey,
  project,
  repo,
  resolvedRepo,
}: {
  disabled?: boolean;
  /** Whether the control is the value of a row of the new issue page's Details. It acts inside
   *  `NoTooltips` alone. */
  inRow?: boolean;
  issueKey: string;
  project: string;
  repo: string | null;
  resolvedRepo: string | null;
}): ReactNode {
  const shared = { disabled, issueKey, project, repo, resolvedRepo };
  return useTooltips() ? (
    <RepositoryList {...shared} />
  ) : (
    <RepositoryBox {...shared} inRow={inRow} />
  );
}

/**
 * The issue's repository in the details rail: `Repository:` and a badge naming the repository the
 * issue resolves to (its own, else its nearest ancestor's, else the project's default, muted when
 * not its own; "No repository" when none resolves), which is the trigger of the shared list's
 * single pick (`MultiSelect`), laid over the badge as `AssigneeControl`'s is, and ends in the
 * shared chevron, since the control it replaced was a visible text box. Labelled
 * `Repository of <KEY>`. The badge is hidden from assistive technology; the trigger's name goes on
 * from its label to the words that read what it shows: the repository,
 * `, inherited or the project's default` when it is not the issue's own, or `No repository`.
 *
 * The list holds Inherit first, which clears the issue's own repository, then the repositories
 * mapped to the issue's project in Settings, and the issue's own repository when no mapping lists
 * it; its text box narrows them as the reader types. The mappings are suggestions rather than the
 * set of values: any `owner/name` may be given, one this project maps, one another project maps,
 * or one no project maps, which is what the server takes. So a typed text that matches no option
 * gets a row of its own, `Use "<typed>"`. A pick writes at once through `useIssueRepo`, and a pick
 * of the value the issue holds writes nothing. A typed text GitHub could hold no repository at
 * (`ownerRepo` above) writes nothing: the list stays open with the text in its box and says so
 * under the options until the text changes. A typed text that passes is written as typed, less
 * the spaces at its ends, and the server's own refusal shows beside the trigger with the write
 * rolled back (`useIssueFieldWrite`). Leaving the list picks nothing; a typed repository is given
 * by its row or Enter.
 *
 * The item renders whenever the issue can carry a repository, which is always: a project that
 * maps none has nothing to suggest, and its issues still take any repository. The mappings are
 * read once the reader reaches for the control (pointer over it, a touch on it, or focus), and at
 * once while the issue resolves to no repository at all, which is the issue most likely to be
 * given one. Until they answer, the list says the repositories are loading where a typed word
 * matches no option. A read that fails costs the suggestions and nothing else: the failure shows
 * beside the trigger with a Retry, and in the list, which still takes a typed repository.
 */
function RepositoryList({
  disabled,
  issueKey,
  project,
  repo: hostRepo,
  resolvedRepo: hostResolvedRepo,
}: {
  disabled: boolean;
  issueKey: string;
  project: string;
  repo: string | null;
  resolvedRepo: string | null;
}): ReactNode {
  const [wanted, setWanted] = useState(false);
  const write = useIssueRepo(issueKey, hostRepo);
  // The issue's own repository and the one it resolves to, as every part of the control draws
  // them: the write's `shown` (`IssueFieldWrite.shown`), and while that is a pick the host does
  // not pass, the repository the pick resolves to as the caches draw it (`pickLook`), which for
  // an inherited one is the host's until Dispatch answers.
  const repo = write.shown;
  const resolvedRepo =
    repo === hostRepo
      ? hostResolvedRepo
      : (pickLook({ field: "repo", issueKey, value: repo }).fields.resolved_repo ??
        hostResolvedRepo);
  const mappings = useQuery({
    enabled: wanted || resolvedRepo === null,
    queryKey: ["repo-projects"],
    queryFn: () => api.listRepoProjects(),
  });
  const [open, setOpen] = useState(false);
  /** Whether the typed repository was refused here, for not being `owner/name`. */
  const [refused, setRefused] = useState(false);
  const mapped = (mappings.data ?? [])
    .filter((mapping) => mapping.project === project)
    .map((mapping) => mapping.repo)
    .sort();
  const failure = mappings.isError
    ? apiErrorMessage(mappings.error, `Could not load the repositories mapped to ${project}.`)
    : null;
  // The issue's own repository stays in the list whether or not its project maps it, so the list
  // never lacks the value it marks picked.
  const options = repo === null || mapped.includes(repo) ? mapped : [repo, ...mapped];
  const value = repo ?? INHERIT;
  const tone = repo === null ? badgeLow : badgePrimary;
  const spoken =
    resolvedRepo === null
      ? "No repository"
      : repo === null
        ? `${resolvedRepo}, inherited or the project's default`
        : resolvedRepo;
  // The badge's tooltip says what the badge means, which the picked row's text does not: an
  // issue with no repository of its own has Inherit picked. "No repository" says all there is,
  // so it shows none.
  const title =
    repo !== null ? repo : resolvedRepo !== null ? "Inherited or the project's default" : "";
  // What is read is what the server reads: it drops a `.git` suffix before it judges the name
  // (canonicalRepo), so a clone URL's name is no longer to the list than to the server. The list
  // offers no row for a text that names an option, so the typed text is never the value the
  // issue holds.
  const give = (typed: string): boolean => {
    if (!ownerRepo.test(typed.replace(/\.git$/i, ""))) {
      setRefused(true);
      return false;
    }
    write.submit(typed);
    return true;
  };
  return (
    <div className={`flex shrink-0 items-center gap-2 text-sm ${textSecondaryOnSurface}`}>
      <span className="font-medium">Repository:</span>
      <span
        className={`${badgeSelectRailWrapper} ${disabled ? "opacity-50" : ""}`}
        {...badgeReachHandlers(() => setWanted(true))}
      >
        <MultiSelect
          createLabel={(typed) => `Use "${typed}"`}
          disabled={disabled}
          emptyMessage={
            mappings.isPending ? "Loading repositories…" : "No repository matches that."
          }
          error={refused ? NOT_OWNER_NAME : failure}
          label={repoControlName(issueKey)}
          mode="single"
          onCreate={give}
          onOpenChange={(next) => {
            setOpen(next);
            setRefused(false);
          }}
          onPick={(picked) => {
            if (picked !== value) write.submit(picked === INHERIT ? null : picked);
          }}
          onQueryChange={() => setRefused(false)}
          open={open}
          optionLabel={(candidate) => (candidate === INHERIT ? "Inherit" : candidate)}
          options={[INHERIT, ...options]}
          searchLabel="Search repositories"
          triggerClassName={badgeSelectRailTrigger}
          triggerTitle={title}
          value={value}
        >
          {/* A single pick's trigger is named for its list and then for what it shows, and the
              badge is hidden from that name: a screen reader hears the words beside it, the
              target as well as Inherit. */}
          <span
            aria-hidden="true"
            className={`${badgeSelectBadge} font-mono ${tone.bg} ${tone.text}`}
          >
            {resolvedRepo ?? "No repository"}
          </span>
          <span className="sr-only">{spoken}</span>
        </MultiSelect>
      </span>
      {failure === null ? null : (
        <QueryError
          message={failure}
          onRetry={() => void mappings.refetch()}
          retrying={mappings.isFetching}
        />
      )}
      {write.failed ? (
        <QueryError
          message={write.error ?? `Could not update the repository of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </div>
  );
}

/**
 * The issue's repository in the details rail: `Repository:` and one combobox, labelled
 * `Repository of <KEY>`, holding the issue's own repository, with a `<datalist>` of the
 * repositories Dispatch maps to this issue's project. The mappings are suggestions rather than the set
 * of values: any `owner/name` may be typed, one this project maps, one another project maps, or
 * one no project maps, which is what the server takes, and that is why this is a text box with a
 * list and not a `<select>`, which cannot hold a value its options lack.
 *
 * The box shows the issue's own repository and nothing else, so the repository it resolves to -
 * its nearest ancestor's, else the project's default - is its placeholder, which is what an empty
 * box stands for; the description (`aria-describedby`, an `sr-only` span) says the same in words,
 * with `, inherited or the project's default` after it when it is not the issue's own, and
 * `No repository` when none resolves. A clear control inside the box, shown only while the issue
 * has a repository of its own, puts it back to inheriting; emptying the box does the same.
 *
 * What is typed is written when the box is left or Enter is pressed, never per keystroke, since
 * `owner/name` is only a repository once the whole of it is there; Escape restores what the issue
 * holds. A value GitHub could hold no repository at - one that is not `owner/name`, or one whose
 * owner or name is outside GitHub's own grammar (`ownerRepoPattern` above) - is refused here, at
 * the box, with what was typed still in it, and the write path (`useIssueRepo`,
 * `useIssueFieldWrite`) is unchanged, so the optimistic write, the rollback and the server's own
 * refusal - `INVALID_ISSUE` for anything that reaches it - show as they did.
 *
 * The item renders whenever the issue can carry a repository, which is always: a project that
 * maps none has nothing to suggest, and its issues still take any repository. The mappings are
 * read once the reader reaches for the control (pointer over it, a touch on it, or focus), and at
 * once while the issue resolves to no repository at all, which is the issue most likely to be
 * given one. A read that fails costs the suggestions and nothing else: the failure and a Retry
 * show beside the box, which takes a typed repository as ever.
 *
 * Inside `NoTooltips` (`components/tooltips.tsx`, the new issue page) the same box says that
 * page's words with their marks and carries no `title`. `Repository:` is the page's label, and
 * `inRow` draws the control as the value of a row of Details, which draws the label, so the
 * control draws none of its own. The description says the page's words around the repository,
 * which is the issue's value, and the page's "inherited or the project's default" is drawn beside
 * the box, or beside the text that takes its place in a row of Details, too, hidden from
 * assistive technology because the description already says it. The clear control's `title` is
 * its description there (`aria-describedby`), so its name stays `Clear the repository of <KEY>`
 * and a reader still hears that the issue then inherits a repository. The refusal keeps its
 * words, which the page's list has none for; a failed read or write shows `QueryError` as on
 * every page. Each colour is the page's: the box is the page's field, the repository the issue
 * inherits (its placeholder) in the quiet grey; the clear control is the muted grey that turns
 * ink under the pointer; a refusal is the failed red; and the line is the ink in a row of Details
 * and the muted grey elsewhere. Nothing there keeps its width: the line wraps, the box, at most
 * `main`'s 200 px, narrows to its line, and the text a row draws in its place breaks anywhere. In
 * a row of Details the box fills the row's value at every width and draws no datalist arrow, so a
 * name that fits the value shows whole, and a longer one scrolls in the box as any one-line box
 * does. Its clear control is drawn there on the line under the box, at the line's start, and not
 * over the box's right end: below 1280 px `styles.css` sets every input in 16 px and gives every
 * button a 44 px minimum width, which leaves a name that fits the value no room beside a control
 * laid over the box. Escape in the box's place, the box or its clear control, is the box's alone
 * there: marked `defaultPrevented` and stopped, it passes by the dialog binding of a sheet around
 * the box, as Details is on a phone, which a second Escape, pressed outside the box's place,
 * closes.
 *
 * There a row of Details draws its value as text rather than in the box, as the other rows of
 * Details do: a text box holds its value on one line and cuts a long one at its width, while the
 * text wraps, so the repository is whole at every width. While the issue resolves to no
 * repository the text is "None", as every empty row of Details reads, in the page's quiet words
 * with their mark; else it is the repository, the issue's value with the author's mark, in the
 * box's font, in the ink, or where it is inherited in the quiet grey the box's placeholder draws
 * it in, and described by the box's description. Each is a button whose name begins with the
 * words it draws (WCAG 2.5.3) and says what it sets - `None, Repository of <KEY>`,
 * `<owner/name>, Repository of <KEY>` - and whose press draws the box above in its place, at the
 * row's full width, with the focus in it. The box gives the row back to the text when it is left,
 * on Escape in the box or on its clear control, and once a write made from it is no longer out,
 * the text then saying what the issue resolves to after the write; while it holds a draft,
 * refused or not, or a write that is out, it stays. Its clear control is part of the box there: a
 * focus moved between the two, by Tab or by Shift+Tab, leaves neither, so a keyboard reaches the
 * control, and the row is given back to the text once the focus leaves both. A press of the
 * control, from the keyboard as from the pointer, leaves the focus in the box while its write is
 * out. Whichever of the text and the box takes the other's place takes the focus the other held,
 * the clear control's included, where that focus fell to the document with it. Outside a row of
 * Details the box is drawn whether or not a repository resolves, and its description says
 * `No repository` when none does.
 */
function RepositoryBox({
  disabled = false,
  inRow = false,
  issueKey,
  project,
  repo: hostRepo,
  resolvedRepo: hostResolvedRepo,
}: {
  disabled?: boolean;
  /** Whether the control is the value of a row of the new issue page's Details. It acts inside
   *  `NoTooltips` alone. */
  inRow?: boolean;
  issueKey: string;
  project: string;
  repo: string | null;
  resolvedRepo: string | null;
}): ReactNode {
  const tooltips = useTooltips();
  const page = usePageWords();
  const inDetails = page !== null && inRow;
  const [wanted, setWanted] = useState(false);
  /** What the reader has typed since the box was last written, left or restored; null while the
   *  box shows the issue's own repository. */
  const [draft, setDraft] = useState<string | null>(null);
  /** Whether the draft was refused here, for not being `owner/name`. */
  const [refused, setRefused] = useState(false);
  /** Inside `NoTooltips`, in a row of Details, whether the reader has opened the box from the
   *  row's text - "None" or the repository - and has not since left it and its clear control,
   *  pressed Escape in it or made a write from it. */
  const [opened, setOpened] = useState(false);
  const write = useIssueRepo(issueKey, hostRepo);
  // The issue's own repository and the one it resolves to, as every part of the control draws
  // them: the write's `shown` (`IssueFieldWrite.shown`), and while that is a pick the host does
  // not pass, the repository the pick resolves to as the caches draw it (`pickLook`), which for
  // an inherited one is the host's until Dispatch answers.
  const repo = write.shown;
  const resolvedRepo =
    repo === hostRepo
      ? hostResolvedRepo
      : (pickLook({ field: "repo", issueKey, value: repo }).fields.resolved_repo ??
        hostResolvedRepo);
  const mappings = useQuery({
    enabled: wanted || resolvedRepo === null,
    queryKey: ["repo-projects"],
    queryFn: () => api.listRepoProjects(),
  });
  const want = () => setWanted(true);
  const descriptionId = useId();
  const suggestionsId = useId();
  // Inside `NoTooltips` the clear control's description, named after the box's own so that the
  // control asks React for no id `main` does not.
  const clearDescriptionId = `${descriptionId}clear`;
  const mapped = (mappings.data ?? [])
    .filter((mapping) => mapping.project === project)
    .map((mapping) => mapping.repo)
    .sort();
  const description =
    resolvedRepo === null
      ? "No repository"
      : repo === null
        ? `${resolvedRepo}, inherited or the project's default`
        : resolvedRepo;
  // Inside `NoTooltips` the repository is the issue's value, and the words around it are the
  // page's own.
  const shownDescription =
    page === null ? (
      description
    ) : resolvedRepo === null ? (
      page.CONTROL_LABELS.no_repository
    ) : (
      <>
        <span {...page.authorText}>{resolvedRepo}</span>
        {repo === null ? `, ${page.CONTROL_LABELS.repository_inherited}` : null}
      </>
    );
  // Inside `NoTooltips` the line is the ink in a row of Details (the prototype's `.frow .fv`) and
  // the muted grey elsewhere (its `#hdr-props`).
  const lineText = page === null ? textSecondaryOnSurface : inDetails ? pageInkText : pageMutedText;
  const restore = () => {
    setDraft(null);
    setRefused(false);
  };
  // The draft goes back to the issue's own repository as it is written: the control draws the
  // repository written from the press on (`repo`), and a rollback puts the previous one back
  // under it. A refused draft stays in the box: it is what the reader has to correct.
  const commit = () => {
    if (draft === null) {
      return;
    }
    const typed = draft.trim();
    // What is read is what the server reads: it drops a `.git` suffix before it judges the name
    // (canonicalRepo), so a clone URL's name is no longer to the box than to the server.
    if (typed !== "" && !ownerRepo.test(typed.replace(/\.git$/i, ""))) {
      setRefused(true);
      return;
    }
    restore();
    if (typed !== (repo ?? "")) {
      write.submit(typed === "" ? null : typed);
      setOpened(false);
    }
  };
  // Inside `NoTooltips` a row of Details draws its value as text - "None" while the issue resolves
  // to no repository, else the repository - until the reader opens the box from it, and the box
  // stays while it holds something of theirs: a draft, refused or not, or a write that is out.
  const showsText = inDetails && !opened && draft === null && !write.pending;
  const showsNone = showsText && resolvedRepo === null;
  const noneControl = useRef<HTMLButtonElement>(null);
  const textControl = useRef<HTMLButtonElement>(null);
  const box = useRef<HTMLInputElement>(null);
  /** The place the row's text or the box is drawn in, the box's clear control with it. */
  const place = useRef<HTMLSpanElement>(null);
  // Whether the focus is on the row's text or in the box, its clear control included. Whichever
  // of them takes the other's place takes the focus the other held, which the one leaving the
  // page drops to the document: a press of the text leaves it in the box; Escape, or a write that
  // lands, on the text, which then says what the issue resolves to; and "None" and the
  // repository's text, each drawn in the other's place when the issue's repository changes under
  // them, hand it on alike. Only a focus still on the document is taken (`focusOnDocument`):
  // anywhere else, the reader put it there.
  const focusHeld = useRef(false);
  useEffect(() => {
    if (!focusHeld.current || !focusOnDocument()) return;
    (showsNone ? noneControl : showsText ? textControl : box).current?.focus();
  }, [showsNone, showsText]);
  // What the row's text does, "None" and the repository alike: a press draws the box in its
  // place. A click focuses no button in some browsers, and the box takes the focus all the same.
  const opensBox = {
    onBlur: () => {
      focusHeld.current = false;
    },
    onClick: () => {
      focusHeld.current = true;
      setOpened(true);
    },
    onFocus: () => {
      focusHeld.current = true;
    },
  };
  // The place the box is drawn in, its width and its flow: `main`'s keeps its 200 px; inside
  // `NoTooltips` nothing keeps its width, the box at most 200 px and narrowing to its line, and in
  // a row of Details as wide as the row, or the repository's text as wide as its words. In a row
  // of Details the place wraps, so the box's clear control, drawn after the box at the place's
  // full width, goes onto the line under it.
  const placeLayout =
    page === null
      ? "inline-flex shrink-0"
      : showsText
        ? "inline-flex min-w-0 max-w-full"
        : inDetails
          ? "flex w-full min-w-0 max-w-full flex-wrap gap-y-1"
          : "inline-flex w-[200px] min-w-0 max-w-full";
  // The line's layout: `main`'s keeps its width; inside `NoTooltips` it narrows to its row and
  // wraps, and in a row of Details it is as wide as the row's value as well. That value is a flex
  // row, in which a line of no width of its own is only as wide as what it holds, so the place's
  // `w-full` and the box's would fill no more than that, and the box would fall back to a text
  // box's own width, which cuts a name the value has room for.
  const lineLayout =
    page === null
      ? "shrink-0"
      : inDetails
        ? "w-full min-w-0 max-w-full flex-wrap"
        : "min-w-0 max-w-full flex-wrap";
  // The box's width, and in a row of Details the room it gives a name: `main`'s 200 px, and
  // inside `NoTooltips` its place's width. In a row of Details the box draws no datalist arrow
  // (Chromium's `::-webkit-calendar-picker-indicator`, some 22 px of a narrow row's box), so a
  // name that fits the row's value shows whole; the list stays bound, and Chromium still opens
  // its suggestions on a click in the box, on ArrowDown and as the reader types. Its padding is
  // `pl-1.5 pr-1.5` there whether or not the issue has a repository of its own, since the clear
  // control is on the line under it (`clearPlace`). Elsewhere the box keeps `pr-8` beside the
  // clear control, which keeps the name off the × laid over the box's right end.
  const boxWidth =
    page === null
      ? "w-[200px]"
      : inDetails
        ? "w-full min-w-0 [&::-webkit-calendar-picker-indicator]:hidden"
        : "w-full min-w-0";
  // Where the clear control is drawn: `main`'s, and the one outside a row of Details, over the
  // box's right end, 32 px wide and as tall as the box. In a row of Details it is in the flow after
  // the box, on the line under it at the line's start: below 1280 px `styles.css` sets every input
  // in 16 px and gives every button a 44 px minimum width, so a control over the box takes 44 px
  // of a row's box and leaves a name that fits the row's value no room beside it. There it keeps
  // a 44 px target below `md`, as that minimum makes it anyway, and from `md` the size it has over
  // the box: 32 px wide and the box's 28 px tall where that minimum no longer holds.
  const clearPlace = inDetails
    ? "inline-flex min-h-11 min-w-11 md:min-h-7 md:min-w-8"
    : "absolute inset-y-0 right-0 inline-flex w-8";
  return (
    <div className={`flex ${lineLayout} items-center gap-2 text-sm ${lineText}`}>
      {inDetails ? null : (
        <span className="font-medium" {...page?.pageChrome}>
          {`${page?.PROPERTY_LABELS.repository ?? "Repository"}:`}
        </span>
      )}
      {showsNone ? (
        <button
          aria-label={`${page.CONTROL_LABELS.none}, ${repoControlName(issueKey)}`}
          className={`min-h-11 rounded-sm outline-none focus-visible:ring-2 disabled:opacity-50 md:min-h-7 ${ON_PAGE.none} ${focusVisibleRing}`}
          disabled={disabled}
          ref={noneControl}
          type="button"
          {...opensBox}
          {...page.pageChrome}
        >
          {page.CONTROL_LABELS.none}
        </button>
      ) : (
        // `relative` is load-bearing: the description is `sr-only`, which is absolutely
        // positioned, and with no positioned ancestor it is laid out against the page and makes
        // it as wide as its own text at a phone width.
        // biome-ignore lint/a11y/noStaticElementInteractions: the place takes no key of its own; it only hears an Escape the box or its clear control inside it sends up, so it needs no role.
        <span
          className={`relative ${placeLayout} items-center`}
          onKeyDown={
            page === null || showsText
              ? undefined
              : (event) => {
                  // A press that ends an IME composition gives up what the reader was composing,
                  // not the box: the place leaves it alone, unmarked, as the key registry does.
                  if (event.key !== "Escape" || event.nativeEvent.isComposing) return;
                  // Inside `NoTooltips` the press is the box's, from the box or from its clear
                  // control: it puts back what the issue holds, closes the box in a row of
                  // Details, and closes nothing around it. On a phone the box sits in Details, a
                  // sheet that closes on Escape through the key registry's dialog binding
                  // (`useDialog`), which takes a press inside a field and passes over one already
                  // `defaultPrevented` (`keymap.ts`); unmarked, one Escape would close the box and
                  // the whole sheet with it, and a press stopped here reaches no listener above
                  // the place either. The focus the box or the control held goes back to the
                  // row's text as it takes the box's place (`focusHeld`). While the text is drawn
                  // the place takes no Escape: the box is not open, and the press is the sheet's.
                  event.preventDefault();
                  event.stopPropagation();
                  restore();
                  setOpened(false);
                }
          }
          ref={place}
        >
          {showsText && resolvedRepo !== null ? (
            // The repository in the box's font, so that the box opened in its place shows it as
            // the text did, breaking anywhere so that a name with no space in it wraps too.
            <button
              aria-describedby={descriptionId}
              aria-label={`${resolvedRepo}, ${repoControlName(issueKey)}`}
              className={`min-h-11 min-w-0 max-w-full rounded-sm text-left font-mono text-xs [overflow-wrap:anywhere] outline-none focus-visible:ring-2 disabled:opacity-50 md:min-h-7 ${repo === null ? ON_PAGE.inherited : ON_PAGE.own} ${focusVisibleRing}`}
              disabled={disabled}
              ref={textControl}
              type="button"
              {...opensBox}
              {...page.authorText}
            >
              {resolvedRepo}
            </button>
          ) : (
            <>
              <input
                aria-describedby={descriptionId}
                aria-invalid={refused || undefined}
                aria-label={repoControlName(issueKey)}
                autoComplete="off"
                className={`min-h-11 ${boxWidth} rounded-sm border py-0.5 pl-1.5 font-mono text-xs outline-none focus-visible:ring-2 disabled:opacity-50 md:min-h-7 ${repo === null || inDetails ? "pr-1.5" : "pr-8"} ${page === null ? inputClasses(false) : ON_PAGE.box} ${focusVisibleRing}`}
                disabled={disabled}
                list={suggestionsId}
                onBlur={(event) => {
                  // Inside `NoTooltips` a focus moved from the box to its clear control, as Tab
                  // moves it, stays in the box's place: the box stays drawn, and with it the
                  // control the focus is now on, which gives the row back to the text once the
                  // focus leaves the place from there (its `onBlur`). A blur while the document
                  // itself has no focus leaves the box drawn as well: a switch to another tab or
                  // window blurs the box with no control to take the focus and leaves the
                  // document's focus on it, which the box holds for the reader's return rather
                  // than drop it to the body. What was typed is written all the same, as
                  // whenever the box is left.
                  if (
                    page === null ||
                    (document.hasFocus() && place.current?.contains(event.relatedTarget) !== true)
                  ) {
                    focusHeld.current = false;
                    setOpened(false);
                  }
                  commit();
                }}
                onChange={(event) => {
                  setDraft(event.target.value);
                  setRefused(false);
                }}
                onFocus={() => {
                  focusHeld.current = true;
                  want();
                }}
                onKeyDown={(event) => {
                  if (event.key === "Enter") {
                    event.preventDefault();
                    commit();
                  } else if (event.key === "Escape" && page === null) {
                    // Inside `NoTooltips` the box's place takes Escape, the box's own included
                    // (its `onKeyDown`). `main`'s page leaves the press unmarked.
                    restore();
                  }
                }}
                onMouseDown={(event) => event.stopPropagation()}
                onPointerDown={(event) => event.stopPropagation()}
                onPointerEnter={want}
                onTouchStart={(event) => {
                  event.stopPropagation();
                  want();
                }}
                pattern={ownerRepoPattern}
                placeholder={resolvedRepo ?? "owner/name"}
                ref={box}
                spellCheck={false}
                value={draft ?? repo ?? ""}
              />
              {repo === null ? null : (
                <button
                  aria-describedby={page === null ? undefined : clearDescriptionId}
                  aria-label={`Clear the repository of ${issueKey}`}
                  className={`${clearPlace} items-center justify-center rounded-sm text-base outline-none focus-visible:ring-2 disabled:opacity-50 ${page === null ? textSecondaryHoverToPrimary : ON_PAGE.clear} ${focusVisibleRing}`}
                  disabled={disabled}
                  // Inside `NoTooltips` the control holds the focus as the box does: a focus
                  // moved back into the box, or a blur while the document itself has no focus,
                  // leaves both drawn, and one that leaves the box's place from here gives the
                  // row back to the text, as leaving the box does.
                  onBlur={
                    page === null
                      ? undefined
                      : (event) => {
                          if (
                            document.hasFocus() &&
                            place.current?.contains(event.relatedTarget) !== true
                          ) {
                            focusHeld.current = false;
                            setOpened(false);
                          }
                        }
                  }
                  onClick={() => {
                    // Inside `NoTooltips` the focus goes into the box first, where a pointer
                    // press leaves it (`onMouseDown`), so a press from the keyboard does not
                    // leave it on the body when the write's optimistic value takes this control
                    // off the page; once the write lands the box hands it to the row's text.
                    if (page !== null) {
                      box.current?.focus();
                    }
                    restore();
                    write.submit(null);
                    setOpened(false);
                  }}
                  onFocus={
                    page === null
                      ? undefined
                      : () => {
                          focusHeld.current = true;
                        }
                  }
                  // A press here must not take focus from the box. The box writes what was typed
                  // when it is left, so the press would write a repository the reader is throwing
                  // away and the click after it would write the clear: two writes for one action.
                  onMouseDown={(event) => event.preventDefault()}
                  title={tooltips ? CLEAR_EXPLAINS : undefined}
                  type="button"
                >
                  <span aria-hidden="true">×</span>
                </button>
              )}
              <datalist id={suggestionsId}>
                {mapped.map((candidate) => (
                  <option key={candidate} value={candidate} />
                ))}
              </datalist>
            </>
          )}
          {/* One description, the box's or the repository's text's, whichever is drawn. */}
          <span className="sr-only" id={descriptionId} {...page?.pageChrome}>
            {shownDescription}
          </span>
          {page === null || repo === null || showsText ? null : (
            <span className="sr-only" id={clearDescriptionId} {...page.pageChrome}>
              {CLEAR_EXPLAINS}
            </span>
          )}
        </span>
      )}
      {/* Inside `NoTooltips`, why the repository shown may not be the issue's own, in words beside
          the box or the text in its place. Hidden from assistive technology because their
          description says the same thing, and nothing is said twice. */}
      {page === null || resolvedRepo === null || repo !== null ? null : (
        <span aria-hidden="true" className="text-xs" {...page.pageChrome}>
          {page.CONTROL_LABELS.repository_inherited}
        </span>
      )}
      {refused ? (
        <p
          className={page === null ? dangerText : pageFailedText}
          role="alert"
          {...page?.pageChrome}
        >
          {NOT_OWNER_NAME}
        </p>
      ) : null}
      {mappings.isError ? (
        <QueryError
          message={apiErrorMessage(
            mappings.error,
            `Could not load the repositories mapped to ${project}.`
          )}
          onRetry={() => void mappings.refetch()}
          retrying={mappings.isFetching}
        />
      ) : null}
      {write.failed ? (
        <QueryError
          message={write.error ?? `Could not update the repository of ${issueKey}.`}
          onRetry={write.retry}
          retrying={write.pending}
        />
      ) : null}
    </div>
  );
}
