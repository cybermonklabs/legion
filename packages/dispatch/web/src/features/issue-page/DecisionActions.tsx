import { type QueryClient, useQuery, useQueryClient } from "@tanstack/react-query";
import { createContext, type ReactNode, useContext, useId, useMemo, useState } from "react";

import { api } from "../../api/client";
import { refreshQueries } from "../../api/query-refresh";
import type { AnchorInput, Artifact } from "../../api/types";
import { useTooltips } from "../../components/tooltips";
import {
  borderDefault,
  dangerText,
  inputClasses,
  pageFailedText,
  pageLineBorder,
  pageMutedText,
  pageWordsText,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import {
  type DecisionNode,
  decisionQuote,
  decisionsOfAnswer,
} from "./decision-nodes/decision-nodes";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS } from "./page-words";
import { dropViews, issueTreeKey, issueTreeQuery } from "./tree-read/tree-read";

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
 *  (`useTooltips`): inside `NoTooltips`, the new page, its own pairs (`theme/classes.ts`); on
 *  today's page, `main`'s. */
const TONES = {
  main: {
    failed: dangerText,
    muted: textMutedOnCanvas,
    rule: borderDefault,
    words: textSecondaryOnCanvas,
  },
  page: {
    failed: pageFailedText,
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
 * Reads the tree of issue `issueKey` again and drops every view of it the page holds: what a Keep
 * does however it ends, and a Change once Dispatch took it. The rule: a write whose answer
 * changes what the tree draws reads the tree by its own key, as the page refreshes it on an event
 * (`subscribeTreeFreshness`'s `refreshQueries`), so a walk of its pages already in flight is
 * waited for rather than cut off, and drops every held view (`dropViews`), since each draws how a
 * decision stands. It reaches the tree through no other read's invalidation, which reaches
 * nothing where that read is already invalidated.
 */
function readDecisionsAgain(queryClient: QueryClient, issueKey: string): void {
  refreshQueries(queryClient, [{ exact: true, queryKey: issueTreeKey(issueKey) }]);
  dropViews(queryClient, issueKey);
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
 * Both writes are the ones that tab made: Keep is `POST /artifacts/{id}/blocks/{block}/keep`
 * against the decision node's own document and block, carrying the sum of the text this node
 * drew, so a decision the agent changed since is refused rather than agreed with, and Change is a
 * comment anchored to the words of the decision's first block (`decisionQuote`). The decision is
 * the node of the tree read the page already holds (`issueTreeKey`), found by its address among
 * that answer's decisions (`decisionsOfAnswer`), so the node makes no read of its own
 * to draw its controls.
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
  // The tree read under the key the page reads it by, watched by an observer that is never
  // enabled: the node sends no request of its own and sees every answer the page's own observer
  // fetches. Its decisions are read once per answer for every node of the page
  // (`decisionsOfAnswer`), with no asks: they only make an answered question a person's
  // decision, and these controls are drawn only on a decision waiting for review, which is an
  // agent's, so the decision its address finds is the same without them.
  const tree = useQuery({ ...issueTreeQuery(issueKey), enabled: false });
  const decision =
    tree.data === undefined || nodeReference === undefined
      ? undefined
      : decisionsOfAnswer(tree.data, issueKey).get(nodeReference);
  const documentId = decision?.document;
  if (decision === undefined || documentId === undefined || isClosed) return null;
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
  // Read once per answer of the tree read, which is when the decision is a new object.
  const quote = useMemo(() => decisionQuote(decision), [decision]);
  const keep = useKeyedWrite({
    inline: true,
    label: () => `${CONTROL_LABELS.keep_decision} ${decision.heading}`,
    // The node's own block, and the sum of the text this node showed: a decision the agent
    // changed since is refused (`409 DECISION_CHANGED`), so a Keep never stands over words the
    // reader did not see.
    write: (id: string, bounds) =>
      api.keepDecision(id, decision.block_id, decision.text_sha256, bounds),
    // However the Keep ends - taken, or refused because the decision changed since - the tree is
    // read again and every view the page holds is dropped (`readDecisionsAgain`), the Decisions
    // view's and the Spec view's among them, since each draws how the decision stands: so every
    // one draws the decision as it now stands, kept, or in its new words with Keep to press
    // again. A refusal is said below, on the decision, until the next press.
    onSettled: () => readDecisionsAgain(queryClient, issueKey),
  });
  return (
    <>
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
          decision={decision}
          documentId={documentId}
          issueKey={issueKey}
          focusField={!restored}
          onDone={() => change(false)}
          quote={quote}
        />
      ) : null}
    </>
  );
}

/** A Change's anchor: the decision's document, its words, and the decision node's own block.
 *  The API's anchor input (`AnchorInput`) does not declare `block_id` yet, and the server
 *  ignores a member it does not declare, so until it declares it the quote is found in the
 *  whole document; once it does, the server finds the quote inside that block alone, so words
 *  the decision shares with another place in the document still name the decision. */
type ChangeAnchor = AnchorInput & { block_id: string };

/** The change itself: a comment of the issue's, anchored to the decision's own block and words,
 *  which is the write the Decisions tab made. */
function ChangeComposer({
  decision,
  documentId,
  focusField,
  issueKey,
  onDone,
  quote,
}: {
  decision: DecisionNode;
  documentId: string;
  /** Whether the field takes the caret as it mounts: it does when a press on Change opened the
   *  form, and not when the form comes back with its row's body. */
  focusField: boolean;
  issueKey: string;
  onDone: () => void;
  quote: string;
}): ReactNode {
  const drafts = useContext(ChangeDrafts);
  // The words the reader typed before the body last closed, if it did.
  const [body, setBody] = useState(() => drafts?.get(decision.ref) ?? "");
  const queryClient = useQueryClient();
  const tones = TONES[useTooltips() ? "main" : "page"];
  const fieldId = useId();
  const send = useKeyedWrite({
    inline: true,
    label: () => `${CONTROL_LABELS.change_decision} ${decision.heading}`,
    // A comment on an issue's document is the issue's own write: the artifact route refuses it,
    // naming the issue route (`send-request.ts` follows the same rule for a thread's replies).
    // The anchor names the decision node's own block beside its words (`ChangeAnchor`).
    write: (input: { body: string }, bounds) => {
      const anchor: ChangeAnchor = { artifact: documentId, block_id: decision.block_id, quote };
      return api.createComment(issueKey, { anchor, body: input.body }, bounds);
    },
    // The change is a comment the tree read hangs under its decision (`decision-review.ts`), so
    // the decision says so, leaves the review list and draws the change under it once the tree
    // is read again: the tree is read again and every held view dropped, as a Keep does
    // (`readDecisionsAgain`), and the comments read is then invalidated too.
    onSuccess: () => {
      setBody("");
      readDecisionsAgain(queryClient, issueKey);
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
  // The one place inside a row that takes the tab order, and only while it is open: this is a
  // form a reader is typing in, so Tab walks its field, Send change and Cancel as it does in
  // every other form of the app. At rest - no form open - the tree is still one tab stop (R9).
  //
  // The field takes the caret as it mounts, which is what opens the form, so a reader who asked
  // to change a decision is left where they are there to type however many frames React takes
  // over the commit, and whether they asked with the pointer or with the row's own key
  // (`useTreeKeymap.ts`'s `tree-change`). A form that comes back with its row's body takes none
  // (`focusField`): the reader moved to the row, not into the field.
  return (
    <form
      aria-label={`${CONTROL_LABELS.change_decision} ${decision.heading}`}
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
        {quote}
      </blockquote>
      <label className={`text-xs ${tones.muted}`} htmlFor={fieldId} {...pageChrome}>
        {CONTROL_LABELS.your_change}
      </label>
      <textarea
        // biome-ignore lint/a11y/noAutofocus: the form mounts because a reader asked to type in it
        autoFocus={focusField}
        className={`block w-full min-w-0 rounded-lg border px-2 py-1 text-sm outline-none ${inputClasses(true)}`}
        id={fieldId}
        onChange={(event) => {
          setBody(event.target.value);
          drafts?.set(decision.ref, event.target.value);
        }}
        rows={3}
        value={body}
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
