import { dispatchWritingBox } from "@legion/contracts";
import { type QueryClient, useQueryClient } from "@tanstack/react-query";
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
import { refreshQueries } from "../../api/query-refresh";
import type { AnchorInput, Artifact, DisplayTree } from "../../api/types";
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
import { useDecisionHeadingWords } from "../issue/decision-heading";
import { usePlainHeadline } from "../refs/plain-text";
import { type TextSource, textSourceOf } from "../refs/text-references";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { anchorBlock, type DecisionNode, decisionsOfAnswer } from "./decision-nodes/decision-nodes";
import { decidedByPerson } from "./decision-review";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import {
  dropViews,
  outlineTargets,
  type TreeAnswer,
  useDecisionReads,
} from "./tree-read/tree-read";

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
 * then lets go of the row the focus held (`IssueTree.tsx`'s `useReleaseFocus`). On the row the
 * focus has not left the tree, so the row stays focused. The row is `NodeRow`'s `data-node-row`,
 * and the move takes no scroll: the reader is already looking at it.
 */
function keepFocusOnRow(control: HTMLElement): void {
  if (!control.contains(document.activeElement)) return;
  control.closest<HTMLElement>("[data-node-row]")?.focus({ preventScroll: true });
}

/**
 * Reads the outline of issue `issueKey` again and drops every view of it the page holds: what a
 * Keep does however it ends, and a Change once Dispatch took it. The rule: a write whose answer
 * changes what the outline draws reads each outline read the page may hold by its own key
 * (`outlineTargets`), as the page refreshes them on an event (`subscribeTreeFreshness`'s
 * `refreshQueries`), so a walk of its pages already in flight is waited for rather than cut off,
 * and drops every held view (`dropViews`), since each draws how a decision stands. It reaches
 * the outline through no other read's invalidation, which reaches nothing where that read is
 * already invalidated.
 */
function readDecisionsAgain(queryClient: QueryClient, issueKey: string): void {
  refreshQueries(queryClient, outlineTargets(issueKey));
  dropViews(queryClient, issueKey);
}

/**
 * Each decision's Change the reader has open, by the decision's own address (`Decision.ref`),
 * with the words typed in it so far: `""` for a form opened with nothing typed yet, and no entry
 * for a Change that is closed, or whose words are out or landed. A read can draw a row again
 * under a new key, and Keep and Change leave the page with the old row; held here, above the
 * rows, the open form and its words come back as the reader left them, rather than being
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

/** The decision at `reference` in the first of `reads` whose decisions hold it
 *  (`decisionsOfAnswer`), each read for issue `key`; none where no read holds it. */
function decisionIn(
  reads: readonly (TreeAnswer | undefined)[],
  key: string,
  reference: string
): DecisionNode | undefined {
  for (const read of reads) {
    const decision = read === undefined ? undefined : decisionsOfAnswer(read, key).get(reference);
    if (decision !== undefined) return decision;
  }
  return undefined;
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
 * Both writes are the ones that tab made: Keep is `POST /artifacts/{id}/blocks/{block}/keep`
 * against the decision node's own document and block, carrying the sum of the text this node
 * drew, so a decision the agent changed since is refused rather than agreed with, and Change is a
 * comment anchored to the decision's own block and the words of its first paragraph, its
 * `heading` (`decision-nodes.ts`). The decision is a node of a read the page already holds, found
 * by its address among that answer's decisions (`decisionIn`): first in the read its row is
 * drawn from - on the Decisions view the decisions view read (`?view=decisions`), which holds
 * every decision of the issue, and in every other view the outline read the address chooses -
 * and, where that read does not hold it, in the other (`useDecisionReads`). The outline read is
 * paged (R15), so a decision past the pages of it held so far is found in the decisions view
 * wherever the page holds that read; and the page reads the decisions view only once the
 * Decisions filter is on (`ViewReadProvider`), so until then a decision is found in the outline
 * read. Both are watched and never sent, so the node makes no read of its own to draw its
 * controls.
 */
export function DecisionActions({
  document,
  isClosed,
  nodeReference,
  tabStops = false,
}: {
  /** The document the row stands in, which names the issue (`issue_key`) whose tree read holds
   *  the decision; `undefined` for a decision whose document the issue read does not carry. The
   *  writes name the decision node's own document (`decision-nodes.ts`' `DecisionNode.document`),
   *  never this one. */
  document: Artifact | undefined;
  isClosed: boolean;
  /** The node's own `dispatch://` address, which is the decision's own (`Decision.ref`). */
  nodeReference: string | undefined;
  /** Whether Keep and Change keep their place in the tab order: false in a tree whose keys press
   *  them (`useTreeKeymap.ts`), true in a view that no tree's keys reach (R9). */
  tabStops?: boolean;
}): ReactNode {
  const issueKey = document?.issue_key ?? "";
  // The decisions of each read are read once per answer for every node of the page
  // (`decisionsOfAnswer`), with no asks: the read answers a person's decision on every ask a
  // person answered, which is what says a question is a person's decision and an agent's
  // decision restates it (`restatesAnswer`), and these controls are drawn only on a decision
  // waiting for review, which is an agent's, so the decision its address finds is the same
  // without them. A decision a person decided is never one, whatever the read's
  // `waiting_review` says (`decidedByPerson`, INFRA-1196's decision 516): the person's own
  // decision is not an agent's to keep or change, so neither control is drawn on it.
  const reads = useDecisionReads(issueKey);
  const decision =
    nodeReference === undefined ? undefined : decisionIn(reads, issueKey, nodeReference);
  const documentId = decision?.document;
  if (decision === undefined || documentId === undefined || isClosed || decidedByPerson(decision)) {
    return null;
  }
  return (
    <DecisionControls
      decision={decision}
      documentId={documentId}
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
  decision: DecisionNode;
  /** The document `decision` is a block of (`DecisionNode.document`), which both writes name. */
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
  // A Change quotes the decision's heading, the words of its first paragraph
  // (`decision-nodes.ts`): a decision whose first paragraph holds no words, whose heading is
  // empty, has none to anchor a Change to.
  const quotable = decision.heading !== "";
  // Read once per decision node, which is a new object with each answer of the outline read: the
  // decision's first paragraph as a reading of that block alone (`anchorBlock`), which the Change
  // quotes, and the document it stands in, the record that paragraph is a passage of.
  const block = useMemo(() => anchorBlock(decision), [decision]);
  const source = useMemo(
    () => textSourceOf("artifact", documentId, issueKey),
    [documentId, issueKey]
  );
  // The heading as plain words, which Keep and Change are named after: a write's label and a
  // form's name hold no markup (the reading list's `decision-heading-words`). The heading is the
  // first paragraph, so the hook is handed that block as the decision's reading and its words
  // as its text: the names read the words the Change quotes, each reference by its name
  // (decision 448). A decision node asks no question.
  const heading = useMemo(
    () => ({ question: null, text: decision.heading, text_tree: block }),
    [block, decision.heading]
  );
  const { reads, words: name } = useDecisionHeadingWords(heading, source);
  const keep = useKeyedWrite({
    inline: true,
    issue: issueKey,
    // A write's label holds no markup: the reading list's `decision-heading-words`.
    label: () => `${CONTROL_LABELS.keep_decision} ${name}`,
    method: "POST",
    // The node's own block, and the sum of the text this node showed: a decision the agent
    // changed since is refused (`409 DECISION_CHANGED`), so a Keep never stands over words the
    // reader did not see.
    write: (id: string, bounds) =>
      api.keepDecision(id, decision.block_id, decision.text_sha256, bounds),
    // However the Keep ends - taken, or refused because the decision changed since - the tree is
    // read again and every view the page holds is dropped (`readDecisionsAgain`), the Decisions
    // view's and the Spec view's among them, since each draws how the decision stands: so every
    // one draws the decision as it now stands, kept, or in its new words with Keep to press
    // again. Each view draws the answer the drop took until the next one lands - the Decisions
    // view's by the one rule every reader of it takes (`useDecisionsView`), the Spec view's by
    // its column's (`useLastAnswer`) - so a decision only the Decisions view read holds keeps
    // these controls meanwhile, and a refusal is said below, on the decision, until the next
    // press. Where that read is refused, the Decisions view says so over the rows it keeps.
    onSettled: () => readDecisionsAgain(queryClient, issueKey),
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
        {quotable ? (
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
        ) : null}
      </p>
      {keep.error === null ? null : (
        <p className={`text-xs ${tones.failed}`} role="alert" {...pageChrome}>
          {keep.error.message}
        </p>
      )}
      {changing && quotable ? (
        <ChangeComposer
          block={block}
          decision={decision}
          documentId={documentId}
          issueKey={issueKey}
          focusField={!restored}
          name={name}
          onDone={() => change(false)}
          source={source}
        />
      ) : null}
    </>
  );
}

/** The change itself: a comment of the issue's, anchored to the decision's own block and words,
 *  which is the write the Decisions tab made, written in the box every place a person writes a
 *  comment in mounts (`MentionField`), with the reference picker in it (the writing list's
 *  `decisions-change`). */
function ChangeComposer({
  block,
  decision,
  documentId,
  focusField,
  issueKey,
  name,
  onDone,
  source,
}: {
  /** The decision's first paragraph (`anchorBlock`), whose words the change quotes. */
  block: DisplayTree | undefined;
  decision: DecisionNode;
  documentId: string;
  /** Whether the field takes the caret as it mounts: it does when a press on Change opened the
   *  form, and not when the form comes back with its row's body. */
  focusField: boolean;
  issueKey: string;
  /** The decision's heading as plain words (`useDecisionHeadingWords`), which the form and its
   *  write are named after. */
  name: string;
  onDone: () => void;
  /** The document the decision is a block of, the record the quote is a passage of. */
  source: TextSource | undefined;
}): ReactNode {
  // The quote as the reader reads the decision: the plain-text projection of its first
  // paragraph, so a reference in it reads as its target's name and as the words its author wrote
  // until it resolves (R8, decision 448, the reading list's `decision-change-quote`), while the
  // change is still anchored to that paragraph's words as the document's own text holds them
  // (`heading`), which the server matches. Until the projection answers, the form quotes those
  // words.
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
    // The issue the Change is written on, as Keep names it: the tree read and its views are held
    // while the write is out and read once it settles, so the Change the read hangs under the
    // decision is drawn as soon as the comment lands, and the decision says it was changed and
    // leaves the review list (`decision-review.ts`).
    issue: issueKey,
    // A write's label holds no markup: the reading list's `decision-heading-words`.
    label: () => `${CONTROL_LABELS.change_decision} ${name}`,
    method: "POST",
    // A comment on an issue's document is the issue's own write: the artifact route refuses it,
    // naming the issue route (`send-request.ts` follows the same rule for a thread's replies).
    // The anchor names the decision node's own block beside its words, and the first of their
    // matches there (`occurrence` 0): the server finds the quote inside that block alone and
    // takes its first match, which is the decision's first paragraph itself, so words the
    // decision shares with another place in the document, or that come again inside its block,
    // still name the decision; it refuses a block that does not hold the quote. No match is
    // counted over the document here.
    write: (input: { body: string }, bounds) => {
      const anchor: AnchorInput = {
        artifact: documentId,
        block_id: decision.block_id,
        occurrence: 0,
        quote: decision.heading,
      };
      return api.createComment(issueKey, { anchor, body: input.body }, bounds);
    },
    // The change is a comment the tree read hangs under its decision (`decision-review.ts`), so
    // the decision says so, leaves the review list and draws the change under it once the tree
    // is read again: the tree is read again and every held view dropped, as a Keep does
    // (`readDecisionsAgain`).
    onSuccess: () => {
      setBody("");
      readDecisionsAgain(queryClient, issueKey);
      onDone();
    },
    // A send Dispatch refused, or one the reader stopped, gives the words back to the draft: the
    // form keeps them while it is drawn, and a body that closed before the refusal, or closes
    // after it, opens again with the Change and its words. The mutation runs this after the form
    // has gone too, as it runs `onSuccess`. The form says so in the page's own words, never in
    // the server's: a refusal's text names what the reader cannot act on, such as the block id of
    // a `400 INVALID_ANCHOR` that names a block that does not hold the quote (decision 92).
    onError: (_error, input) => {
      drafts?.set(decision.ref, input.body);
    },
  });
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
        if (body.trim() === "") return;
        // Send is disabled while the comment is out, and the form goes once it lands. The draft
        // goes while the send is out: a body that closes then comes back with Change closed, and
        // the shell's pending-writes indicator holds the send, so a second press cannot write the
        // change twice. A refusal gives the words back to the draft (`onError`).
        keepFocusOnRow(event.currentTarget);
        drafts?.delete(decision.ref);
        send.mutate({ body });
      }}
    >
      <blockquote
        className={`border-l-2 pl-2 text-xs break-words ${tones.rule} ${tones.words}`}
        data-testid="decision-quote"
        {...authorText}
      >
        {quoteReads}
        {/* Until the engine answers, the first paragraph's words as written: the reading
            list's `decision-change-quote`. */}
        {quoted?.text ?? decision.heading}
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
          disabled={send.isPending || body.trim() === ""}
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
