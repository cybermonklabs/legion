import { dispatchWritingBox } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  type ReactNode,
  useContext,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import { api } from "../../api/client";
import type { Artifact, Decision, DisplayTree, IssueDecisions } from "../../api/types";
import { useTooltips } from "../../components/tooltips";
import {
  borderDefault,
  dangerText,
  focusBorder,
  pageFailedText,
  pageHairBorder,
  pageInkText,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageWordsText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { MentionField, type MentionFieldHandle } from "../conversation/MentionField";
import {
  anchorBlock,
  anchorQuote,
  type ChangeAnchor,
  changeAnchor,
} from "../decision-card/change-anchor";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { useDecisionHeadingWords } from "../issue/decision-heading";
import { usePlainHeadline } from "../refs/plain-text";
import { EVENT_FRESH } from "../refs/reference-target";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";

/** One control a row of the tree draws of its own: Keep and Change on a decision. */
export type RowAction = "change" | "keep";

/** The attribute marking such a control, which is how the row's own key finds it: the tree is
 *  one tab stop, so no Tab reaches a control inside a row and `useTreeKeymap.ts` presses it
 *  instead (R9, WCAG 2.1.1). It is written here and read through `rowActionSelector`, so
 *  neither end spells the other's string. */
const ROW_ACTION_ATTRIBUTE = "data-row-action";

/** The mark one of those controls carries, spread onto the control itself. */
export const ROW_ACTION_MARKS: Readonly<Record<RowAction, Readonly<Record<string, RowAction>>>> = {
  change: { [ROW_ACTION_ATTRIBUTE]: "change" },
  keep: { [ROW_ACTION_ATTRIBUTE]: "keep" },
};

/** The selector that finds the control `action` marks inside one row. */
export function rowActionSelector(action: RowAction): string {
  return `button[${ROW_ACTION_ATTRIBUTE}="${action}"]`;
}

/** Keep and Change as the approved picture draws them (the prototype's `.dbtn`): 24 px tall, in
 *  the decisions' violet, Keep filled and Change outlined; the words on them say which is which
 *  (R6). */
const DECISION_BUTTON =
  "rounded-[7px] border-[1.5px] border-page-decision px-3 py-0.5 text-xs leading-5 font-semibold";

/** The colours of the lines Keep and Change draw under them, by the surface they are drawn on
 *  (`useTooltips`): inside `NoTooltips`, the new page, its own pairs (`theme/classes.ts`), and the
 *  Change's box in the shape the page gives every box it writes in (`MentionField`'s
 *  `fieldClassName`); on today's page, `main`'s, and the box in the field's own. */
const TONES = {
  main: {
    failed: dangerText,
    field: undefined,
    muted: textMutedOnCanvas,
    rule: borderDefault,
    words: textSecondaryOnCanvas,
  },
  page: {
    failed: pageFailedText,
    field: `${pageHairBorder} ${pagePaperBg} ${pageInkText} ${focusBorder}`,
    muted: pageMutedText,
    rule: pageLineBorder,
    words: pageWordsText,
  },
} as const;

/**
 * Moves the focus off `control` onto its decision's own row, where `control` holds it, before a
 * write disables or removes `control`. A focused control that is disabled drops the focus to the
 * document body (measured in Chromium: a `focusout` from Keep with no related target 45 ms
 * after its write went out), which loses a keyboard reader's place on the page, and the tree
 * then closes the body the focus held (`IssueTree.tsx`), taking Change with it. On the row the
 * focus has not left the tree, so the body stays open. The row is `NodeRow`'s `data-node-row`,
 * and the move takes no scroll: the reader is already looking at it.
 */
function keepFocusOnRow(control: HTMLElement): void {
  if (!control.contains(document.activeElement)) return;
  control.closest<HTMLElement>("[data-node-row]")?.focus({ preventScroll: true });
}

/**
 * Each decision's Change the reader has open, by the decision's own address (`Decision.ref`),
 * with the words typed in it so far: `""` for a form opened with nothing typed yet, and no entry
 * for a Change that is closed, or whose words are out or landed. A row's body closes once the focus
 * leaves the rows, and Keep and Change go with it; held here, above the rows, the open form and
 * its words come back as the reader left them when the body opens again, rather than being
 * dropped with no word said.
 * Outside a view that holds one, a Change holds its form only while it is drawn.
 */
const ChangeDrafts = createContext<Map<string, string> | undefined>(undefined);

/** Holds every open Change of the rows under it for as long as the view is drawn: the view that
 *  draws the rows wraps them in it. */
export function ChangeDraftsProvider({ children }: { children: ReactNode }): ReactNode {
  const [drafts] = useState(() => new Map<string, string>());
  return <ChangeDrafts.Provider value={drafts}>{children}</ChangeDrafts.Provider>;
}

/**
 * What a reader does with a decision, on the decision's own node (R7, the
 * spec's "Decisions tab ... Keep and Change" row, `plan-md#b-t18a`).
 *
 * The old Decisions tab drew four sections of cards in a panel of its own and put Keep and
 * Change on the cards of one of them; the tree draws each decision as a node, so the two
 * controls are in the node's body, where a reader is already reading the decision. The body is
 * the focused row's and a row the reader pinned open, so the page offers one Keep and one
 * Change at a time, on the decision in front of the reader.
 *
 * Both writes are the ones that tab made, unchanged: Keep is
 * `POST /artifacts/{id}/blocks/{block}/keep` carrying the sum of the text this node drew, so a
 * decision the agent changed since is refused rather than agreed with, and Change is a comment
 * anchored to the words of the decision's first block (`anchorQuote`). They are read from the
 * decisions read the tree is already built from (`["decisions", <key>]`), so the node makes no
 * read of its own.
 */
export function DecisionActions({
  document,
  isClosed,
  nodeReference,
  tabStops = false,
}: {
  /** The decisions document this node is a block of, which the Keep writes against;
   *  `undefined` for a decision whose document the issue read does not carry. */
  document: Artifact | undefined;
  isClosed: boolean;
  /** The node's own `dispatch://` address, which is the decision's own (`Decision.ref`). */
  nodeReference: string | undefined;
  /** Whether Keep and Change keep their place in the tab order: false in a tree whose keys press
   *  them (`useTreeKeymap.ts`), true in a view that no tree's keys reach (R9). */
  tabStops?: boolean;
}): ReactNode {
  const issueKey = document?.issue_key ?? "";
  const decisions = useQuery({ ...issueDecisionsQuery(issueKey, issueKey !== ""), ...EVENT_FRESH });
  const decision = (decisions.data?.decisions ?? []).find((row) => row.ref === nodeReference);
  if (decision === undefined || document === undefined || isClosed) return null;
  return (
    <DecisionControls
      decision={decision}
      documentId={document.id}
      issueKey={issueKey}
      tabStops={tabStops}
    />
  );
}

function DecisionControls({
  decision,
  documentId,
  issueKey,
  tabStops,
}: {
  decision: Decision;
  documentId: string;
  issueKey: string;
  tabStops: boolean;
}): ReactNode {
  const queryClient = useQueryClient();
  const tones = TONES[useTooltips() ? "main" : "page"];
  const drafts = useContext(ChangeDrafts);
  // A Change the reader left open on this decision opens again with the body.
  const [changing, setChanging] = useState(() => drafts?.has(decision.ref) === true);
  // A form that comes back with the body takes no caret: the reader came back to the row, and a
  // press on Change is what puts the caret in the field.
  const [restored, setRestored] = useState(changing);
  const change = (open: boolean) => {
    setChanging(open);
    setRestored(false);
    if (open) drafts?.set(decision.ref, "");
    else drafts?.delete(decision.ref);
  };
  // Read once per answer of the decisions read, which is when the decision is a new object: the
  // block a Change is anchored to, and its words, which the write anchors to.
  const block = useMemo(() => anchorBlock(decision), [decision]);
  const quote = useMemo(() => anchorQuote(decision), [decision]);
  const source = useMemo(
    () => textSourceOf("artifact", documentId, issueKey),
    [documentId, issueKey]
  );
  // The heading as plain words, which Keep and Change are named after: a write's label and a
  // form's name hold no markup (the reading list's `decision-heading-words`).
  const { reads, words: name } = useDecisionHeadingWords(decision, source);
  const keep = useKeyedWrite({
    inline: true,
    // A write's label holds no markup: the reading list's `decision-heading-words`.
    label: () => `${CONTROL_LABELS.keep_decision} ${name}`,
    method: "POST",
    // The sum of the text this node showed: a decision the agent changed since is refused
    // (`409 DECISION_CHANGED`), so a Keep never stands over words the reader did not see.
    write: (id: string, bounds) =>
      api.keepDecision(id, decision.block_id, decision.text_sha256, bounds),
    onSuccess: (kept) => {
      if (kept === null) {
        void queryClient.invalidateQueries({ exact: true, queryKey: ["decisions", issueKey] });
        return;
      }
      queryClient.setQueryData<IssueDecisions>(["decisions", issueKey], (view) =>
        view === undefined
          ? view
          : {
              ...view,
              // The Keep is of the text this node showed, which the server took, so it applies
              // to the row that still holds that text.
              decisions: (view.decisions ?? []).map((row) =>
                row.block_id === kept.block_id && row.text_sha256 === decision.text_sha256
                  ? { ...row, kept, waiting_review: false }
                  : row
              ),
            }
      );
    },
  });
  return (
    <>
      {reads}
      <p className="flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-2">
        {decision.waiting_review ? (
          // Out of the tab order, as every control inside a row is, unless `tabStops` holds,
          // and found by its mark: the tree is one tab stop and the row's own key presses this
          // (R9, WCAG 2.1.1, `useTreeKeymap.ts`'s `tree-keep`).
          <button
            className={`${DECISION_BUTTON} bg-page-decision text-page-paper`}
            disabled={keep.isPending}
            onClick={(event) => {
              keepFocusOnRow(event.currentTarget);
              keep.mutate(documentId);
            }}
            tabIndex={tabStops ? undefined : -1}
            type="button"
            {...ROW_ACTION_MARKS.keep}
            {...pageChrome}
          >
            {CONTROL_LABELS.keep_decision}
          </button>
        ) : null}
        {quote === undefined ? null : (
          <button
            aria-expanded={changing}
            className={`${DECISION_BUTTON} bg-page-paper text-page-decision`}
            onClick={() => change(!changing)}
            tabIndex={tabStops ? undefined : -1}
            type="button"
            {...ROW_ACTION_MARKS.change}
            {...pageChrome}
          >
            {CONTROL_LABELS.change_decision}
          </button>
        )}
      </p>
      {keep.error === null ? null : (
        <p className={`text-xs ${tones.failed}`} role="alert" {...pageChrome}>
          {keep.error.message}
        </p>
      )}
      {changing && quote !== undefined ? (
        <ChangeComposer
          block={block}
          decision={decision}
          documentId={documentId}
          issueKey={issueKey}
          focusField={!restored}
          name={name}
          onDone={() => change(false)}
          quote={quote}
          source={source}
        />
      ) : null}
    </>
  );
}

/** The change itself: a comment of the issue's, anchored to the decision's own words, which is
 *  the write the Decisions tab made, written in the box every place a person writes a comment
 *  in mounts (`MentionField`), with the reference picker in it (the writing list's
 *  `decisions-change`). */
function ChangeComposer({
  block,
  decision,
  documentId,
  focusField,
  issueKey,
  name,
  onDone,
  quote,
  source,
}: {
  /** The block the change is anchored to (`anchorBlock`), whose words the form quotes. */
  block: DisplayTree | undefined;
  decision: Decision;
  documentId: string;
  /** Whether the field takes the caret as it mounts: it does when a press on Change opened the
   *  form, and not when the form comes back with its row's body. */
  focusField: boolean;
  issueKey: string;
  /** The decision's heading as plain words (`useDecisionHeadingWords`), which the form and its
   *  write are named after. */
  name: string;
  onDone: () => void;
  /** The words of `block` as the decisions document holds them, which the change is anchored to
   *  (`anchorQuote`). */
  quote: string;
  /** The decisions document, the record the quote is a passage of. */
  source: TextSource | undefined;
}): ReactNode {
  // The quote as the reader reads the decision: the block's plain-text projection, so a
  // reference in it reads as its target's name and as the words its author wrote until it
  // resolves (R8, the reading list's `decision-change-quote`), while the change is still
  // anchored to the document's own words (`quote`), which the server matches. Until the
  // projection answers, the form quotes those words.
  const { reads: quoteReads, text: quoted } = usePlainHeadline(block, undefined, {
    source,
    surface: "decision-change-quote",
  });
  const drafts = useContext(ChangeDrafts);
  // The words the reader typed before the body last closed, if it did.
  const [body, setBody] = useState(() => drafts?.get(decision.ref) ?? "");
  const queryClient = useQueryClient();
  const tones = TONES[useTooltips() ? "main" : "page"];
  const field = useRef<MentionFieldHandle | null>(null);
  const send = useKeyedWrite({
    inline: true,
    // A write's label holds no markup: the reading list's `decision-heading-words`.
    label: () => `${CONTROL_LABELS.change_decision} ${name}`,
    method: "POST",
    // A comment on an issue's document is the issue's own write: the artifact route refuses it,
    // naming the issue route (`send-request.ts` follows the same rule for a thread's replies).
    write: (input: { anchor: ChangeAnchor; body: string }, bounds) =>
      api.createComment(
        issueKey,
        {
          anchor: {
            artifact: documentId,
            occurrence: input.anchor.occurrence,
            quote: input.anchor.quote,
          },
          body: input.body,
        },
        bounds
      ),
    // The change is a comment of the issue's, and the comments read is what tells the page a
    // decision was changed (`decision-review.ts`): read again, so the decision says so, leaves
    // the review list and draws the change under it as soon as the comment lands.
    onSuccess: () => {
      setBody("");
      void queryClient.invalidateQueries({ exact: true, queryKey: ["comments", issueKey] });
      onDone();
    },
    // A send Dispatch refused, or one the reader stopped, gives the words back to the draft: the
    // form keeps them while it is drawn, and a body that closed before the refusal, or closes
    // after it, opens again with the Change and its words. The mutation runs this after the form
    // has gone too, as it runs `onSuccess`. The form says so in the page's own words, never in
    // the server's: a refusal's text names what the reader cannot act on, such as a quote that
    // matches two places and the field that would say which (decision 92).
    onError: (_error, input) => {
      drafts?.set(decision.ref, input.body);
    },
  });
  // Whether the send is reading the decisions document's text, which it does before the comment
  // goes out: Send is off while it does, as it is while the comment is out.
  const [reading, setReading] = useState(false);
  // The one place inside a row that takes the tab order, and only while it is open: this is a
  // form a reader is typing in, so Tab walks its field, Send change and Cancel as it does in
  // every other form of the app. At rest - no form open - the tree is still one tab stop (R9).
  //
  // The field takes the caret as it mounts, which is what opens the form, so a reader who asked
  // to change a decision is left where they are there to type however many frames React takes
  // over the commit, and whether they asked with the pointer or with the row's own key
  // (`useTreeKeymap.ts`'s `tree-change`). A form that comes back with its row's body takes none
  // (`focusField`): the reader moved to the row, not into the field. The caret goes in from the
  // commit that mounts the field, as `autoFocus` puts it there, through the handle the field
  // gives its host.
  useLayoutEffect(() => {
    if (focusField) field.current?.textarea()?.focus();
  }, [focusField]);
  return (
    <form
      // A form's name holds no markup: the reading list's `decision-heading-words`.
      aria-label={`${CONTROL_LABELS.change_decision} ${name}`}
      className="flex min-w-0 flex-col gap-1"
      onSubmit={(event) => {
        event.preventDefault();
        if (body.trim() === "" || reading) return;
        // Send is disabled while the comment is out, and the form goes once it lands. The draft
        // goes while the send is out: a body that closes then comes back with Change closed, and
        // the shell's pending-writes indicator holds the send, so a second press cannot write the
        // change twice. A refusal gives the words back to the draft (`onError`).
        keepFocusOnRow(event.currentTarget);
        drafts?.delete(decision.ref);
        // Which of the quote's places in the decisions document is this decision's own, counted
        // here, at the send, over the document's own reading, which the send reads once now:
        // `changeAnchor` walks every decision above this one, which no render of a row pays for
        // (decision 92). With no reading - the read failed - the anchor names no place: Dispatch
        // takes it where the quote stands once, and refuses it where the quote stands more than
        // once, which the form says in the page's words with the draft kept.
        setReading(true);
        void queryClient
          .fetchQuery({
            queryFn: () => api.getArtifactText(documentId),
            queryKey: ["artifact", documentId, "text"],
            staleTime: 0,
          })
          .then(
            (text) => text.markdown_tree,
            () => undefined
          )
          .then((tree) => {
            setReading(false);
            const anchor = changeAnchor(
              decision,
              queryClient.getQueryData<IssueDecisions>(issueDecisionsQuery(issueKey, true).queryKey)
                ?.decisions,
              tree
            );
            send.mutate({ anchor: anchor ?? { quote }, body });
          });
      }}
    >
      <blockquote
        className={`border-l-2 pl-2 text-xs break-words ${tones.rule} ${tones.words}`}
        data-testid="decision-quote"
        {...authorText}
      >
        {quoteReads}
        {quoted?.text ?? quote}
      </blockquote>
      <MentionField
        body={body}
        className="text-sm"
        fieldClassName={tones.field}
        fieldRef={field}
        hintClassName={`text-xs ${tones.muted}`}
        issueKey={issueKey}
        label={CONTROL_LABELS.your_change}
        labelClassName={`block text-xs ${tones.muted}`}
        onChange={(change) => {
          setBody(change.body);
          drafts?.set(decision.ref, change.body);
        }}
        rows={3}
        writing={dispatchWritingBox("decisions-change", "comment")}
      />
      {send.error === null ? null : (
        <p className={`text-xs ${tones.failed}`} role="alert" {...pageChrome}>
          {ANSWER_LINES.change_not_sent}
        </p>
      )}
      <p className="flex min-w-0 flex-wrap items-baseline gap-2">
        <button
          className={`rounded border px-1 text-xs ${tones.rule} ${tones.muted}`}
          disabled={send.isPending || reading || body.trim() === ""}
          type="submit"
          {...pageChrome}
        >
          {CONTROL_LABELS.send_change}
        </button>
        <button
          className={`rounded border px-1 text-xs ${tones.rule} ${tones.muted}`}
          onClick={onDone}
          type="button"
          {...pageChrome}
        >
          {CONTROL_LABELS.cancel}
        </button>
      </p>
    </form>
  );
}
