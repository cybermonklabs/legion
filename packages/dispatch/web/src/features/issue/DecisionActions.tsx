import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useId, useMemo, useState } from "react";

import { api } from "../../api/client";
import type { Artifact, Decision, DisplayTree, IssueDecisions } from "../../api/types";
import {
  borderDefault,
  dangerText,
  inputClasses,
  textMutedOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";
import { issueDecisionsQuery } from "../doc/spec-marks";
import { wordsOf } from "../refs/tree-fragment";
import { useKeyedWrite } from "../writes/useKeyedWrite";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";

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

/**
 * What a reader does with a decision, on the decision's own node (dispatch://INFRA-1196 R7, the
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
}: {
  /** The decisions document this node is a block of, which the Keep writes against;
   *  `undefined` for a decision whose document the issue read does not carry. */
  document: Artifact | undefined;
  isClosed: boolean;
  /** The node's own `dispatch://` address, which is the decision's own (`Decision.ref`). */
  nodeReference: string | undefined;
}): ReactNode {
  const issueKey = document?.issue_key ?? "";
  const decisions = useQuery(issueDecisionsQuery(issueKey, issueKey !== ""));
  const decision = (decisions.data?.decisions ?? []).find((row) => row.ref === nodeReference);
  if (decision === undefined || document === undefined || isClosed) return null;
  return <DecisionControls decision={decision} documentId={document.id} issueKey={issueKey} />;
}

function DecisionControls({
  decision,
  documentId,
  issueKey,
}: {
  decision: Decision;
  documentId: string;
  issueKey: string;
}): ReactNode {
  const queryClient = useQueryClient();
  const [changing, setChanging] = useState(false);
  // Read once per answer of the decisions read, which is when the decision is a new object.
  const quote = useMemo(() => anchorQuote(decision), [decision]);
  const keep = useKeyedWrite({
    inline: true,
    label: () => `${CONTROL_LABELS.keep_decision} ${decision.heading}`,
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
      <p className="flex min-w-0 flex-wrap items-baseline gap-2">
        {decision.waiting_review ? (
          // Out of the tab order, as every control inside a row is, and found by its mark: the
          // tree is one tab stop and the row's own key presses this (R9, WCAG 2.1.1,
          // `useTreeKeymap.ts`'s `tree-keep`).
          <button
            className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
            disabled={keep.isPending}
            onClick={() => keep.mutate(documentId)}
            tabIndex={-1}
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
            className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
            onClick={() => setChanging(!changing)}
            tabIndex={-1}
            type="button"
            {...ROW_ACTION_MARKS.change}
            {...pageChrome}
          >
            {CONTROL_LABELS.change_decision}
          </button>
        )}
      </p>
      {keep.error === null ? null : (
        <p className={`text-xs ${dangerText}`} role="alert" {...pageChrome}>
          {keep.error.message}
        </p>
      )}
      {changing && quote !== undefined ? (
        <ChangeComposer
          decision={decision}
          documentId={documentId}
          issueKey={issueKey}
          onDone={() => setChanging(false)}
          quote={quote}
        />
      ) : null}
    </>
  );
}

/** The change itself: a comment of the issue's, anchored to the decision's own words, which is
 *  the write the Decisions tab made. */
function ChangeComposer({
  decision,
  documentId,
  issueKey,
  onDone,
  quote,
}: {
  decision: Decision;
  documentId: string;
  issueKey: string;
  onDone: () => void;
  quote: string;
}): ReactNode {
  const [body, setBody] = useState("");
  const fieldId = useId();
  const send = useKeyedWrite({
    inline: true,
    label: () => `${CONTROL_LABELS.change_decision} ${decision.heading}`,
    // A comment on an issue's document is the issue's own write: the artifact route refuses it,
    // naming the issue route (`send-request.ts` follows the same rule for a thread's replies).
    write: (input: { body: string; quote: string }, bounds) =>
      api.createComment(
        issueKey,
        { anchor: { artifact: documentId, quote: input.quote }, body: input.body },
        bounds
      ),
    onSuccess: () => {
      setBody("");
      onDone();
    },
  });
  // The one place inside a row that takes the tab order, and only while it is open: this is a
  // form a reader is typing in, so Tab walks its field, Send change and Cancel as it does in
  // every other form of the app. At rest - no form open - the tree is still one tab stop (R9).
  //
  // The field takes the caret as it mounts, which is what opens the form, so a reader who asked
  // to change a decision is left where they are there to type however many frames React takes
  // over the commit, and whether they asked with the pointer or with the row's own key
  // (`useTreeKeymap.ts`'s `tree-change`).
  return (
    <form
      aria-label={`${CONTROL_LABELS.change_decision} ${decision.heading}`}
      className="flex min-w-0 flex-col gap-1"
      onSubmit={(event) => {
        event.preventDefault();
        if (body.trim() !== "") send.mutate({ body, quote });
      }}
    >
      <blockquote
        className={`border-l-2 pl-2 text-xs break-words ${borderDefault} ${textSecondaryOnCanvas}`}
        data-testid="decision-quote"
        {...authorText}
      >
        {quote}
      </blockquote>
      <label className={`text-xs ${textMutedOnCanvas}`} htmlFor={fieldId} {...pageChrome}>
        {CONTROL_LABELS.your_change}
      </label>
      <textarea
        // biome-ignore lint/a11y/noAutofocus: the form mounts because a reader asked to type in it
        autoFocus
        className={`block w-full min-w-0 rounded-lg border px-2 py-1 text-sm outline-none ${inputClasses(true)}`}
        id={fieldId}
        onChange={(event) => setBody(event.target.value)}
        rows={3}
        value={body}
      />
      {send.error === null ? null : (
        <p className={`text-xs ${dangerText}`} role="alert" {...pageChrome}>
          {send.error.message}
        </p>
      )}
      <p className="flex min-w-0 flex-wrap items-baseline gap-2">
        <button
          className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
          disabled={send.isPending || body.trim() === ""}
          type="submit"
          {...pageChrome}
        >
          {CONTROL_LABELS.send_change}
        </button>
        <button
          className={`rounded border px-1 text-xs ${borderDefault} ${textMutedOnCanvas}`}
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

/** The words of the decision's first block below its heading, which is what a change is
 *  anchored to: the server matches an anchor quote against the document's own text
 *  (`pmdoc.FindQuote`), never its markdown, so a list or a hard break quoted as markdown would
 *  anchor on nothing. They are read from the tree Go read the decision's text into
 *  (`Decision.text_tree`), whose top-level blocks are the paragraphs the markdown would have
 *  been split into; an answer that carried no reading - a pod of the build before during a
 *  rolling deploy - leaves the first paragraph's own markdown, which is all the browser has.
 *  Undefined for a decision whose heading is all it holds. */
function anchorQuote(decision: Decision): string | undefined {
  const content = decision.text_tree?.content;
  if (!Array.isArray(content)) {
    return firstParagraph(decision.text);
  }
  const first = (content as DisplayTree[]).find((node) => node.type !== "heading");
  return first === undefined ? undefined : wordsOf(first);
}

/** The decision's first paragraph below its heading, as markdown: the quote of an answer that
 *  carried no reading of the decision's text. Undefined for a decision whose heading is all it
 *  holds. */
function firstParagraph(text: string): string | undefined {
  return text
    .split(/\n\s*\n/)
    .map((paragraph) => paragraph.trim())
    .find((paragraph) => paragraph !== "" && !paragraph.startsWith("#"));
}
