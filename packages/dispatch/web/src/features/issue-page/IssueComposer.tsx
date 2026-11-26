import { dispatchWritingBox, IDEMPOTENCY_KEY_TTL_MS } from "@legion/contracts";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

import type { Actor } from "../../api/types";
import { RefusableButton } from "../../components/RefusableButton";
import { pageComposer } from "../../theme/classes";
import {
  type ListedReplies,
  type ListedReply,
  replyParentOf,
} from "../conversation/composer-model";
import { useHeldSend } from "../conversation/held-sends";
import {
  keepFocusOnPress,
  MentionComposer,
  type OutlineAnswers,
  type OutlineKind,
  type OutlineLook,
  type OutlineNames,
  OutlineTargetLine,
  sendOnEnter,
} from "../conversation/MentionComposer";
import { MentionField } from "../conversation/MentionField";
import { type StatementAnchor, sameStatement, saveCommentEdit } from "../conversation/send-request";
import { AgentName } from "../refs/AgentName";
import type { RefPickerWords } from "../refs/ref-picker";
import { EVENT_FRESH } from "../refs/reference-target";
import { focusOnDocument } from "../shell/roving";
import { useMediaQuery } from "../shell/useDialog";
import { FOOT_HEIGHT_PROPERTY, HEADER_SCROLLS_QUERY } from "./identity-block";
import type { TreeNode } from "./issue-tree";
import { DETAILS_ATTRIBUTE } from "./margin-slot";
import { useCriterionLine } from "./model-rows/criterion-write";
import { useRoomFacts } from "./model-rows/room-target";
import {
  type CriterionItem,
  conversationItem,
  type Landing,
  type OutlineWriter,
  replyItemOf,
  statementTargetOf,
  useOutlineWriter,
} from "./outline-writer";
import { authorText, pageChrome } from "./page-structure";
import {
  ANSWER_LINES,
  BOX_PICKER_LINES,
  BOX_PICKER_NAMES,
  COMPOSER_ANSWER_WORDS,
  CONTROL_LABELS,
  NODE_TYPE_NAMES,
  type NodeType,
} from "./page-words";
import { TAB_STOP_ROW } from "./tree-focus";
import { nodesInOrder } from "./tree-keys";
import { issueCommentsQuery, issueMessagesQuery } from "./useIssueTree";

/** The kinds the line writes, as its chips before Send, in this order: a comment, which is on at
 *  first, and a question, which is an ask of the issue with no options (R2). A later part adds a
 *  kind as one entry here. */
const KINDS: readonly OutlineKind[] = [
  {
    kind: "comment",
    label: (
      <>
        <span aria-hidden="true">{CONTROL_LABELS.composer_comment_mark}</span>
        <span {...pageChrome}>{CONTROL_LABELS.composer_comment}</span>
      </>
    ),
  },
  {
    kind: "ask",
    label: (
      <>
        <span aria-hidden="true">{CONTROL_LABELS.composer_question_mark}</span>
        <span {...pageChrome}>{CONTROL_LABELS.composer_question}</span>
      </>
    ),
  },
];

/** The line's placeholder for each thing it writes, wide and on a phone, where a shorter one fits
 *  the field whole (R1, R3, R11; the tester's findings F2 and F9). */
const PLACEHOLDERS = {
  criterion: [CONTROL_LABELS.criterion_placeholder, CONTROL_LABELS.criterion_placeholder_phone],
  edit: [CONTROL_LABELS.edit_placeholder, CONTROL_LABELS.edit_placeholder_phone],
  issue: [CONTROL_LABELS.issue_placeholder, CONTROL_LABELS.issue_placeholder_phone],
  reply: [CONTROL_LABELS.reply_placeholder, CONTROL_LABELS.reply_placeholder_phone],
  statement: [CONTROL_LABELS.statement_placeholder, CONTROL_LABELS.statement_placeholder_phone],
} as const;

/** The placeholder of a line that writes `target`, in its phone form where `phone`. */
function placeholderOf(target: keyof typeof PLACEHOLDERS, phone: boolean): string {
  const [wide, narrow] = PLACEHOLDERS[target];
  return phone ? narrow : wide;
}

/** How many hours Dispatch keeps a write's key, which the box's sentences of what a Retry does
 *  name (`COMPOSER_ANSWER_WORDS.stopped`). */
const KEY_HOURS = String(IDEMPOTENCY_KEY_TTL_MS / 3_600_000);

/** One of the box's sentences that names how many hours Dispatch keeps a write's key, whole. */
function withKeyHours({
  after,
  before,
}: {
  readonly after: string;
  readonly before: string;
}): string {
  return `${before}${KEY_HOURS}${after}`;
}

/** What the box says in answer to the reader's act, each one sentence of the page's own list
 *  (`COMPOSER_ANSWER_WORDS`), in place of the composer's own and of Dispatch's (decision 527). */
const ANSWERS: OutlineAnswers = {
  changedEdit: COMPOSER_ANSWER_WORDS.changed_edit,
  deletedReply: COMPOSER_ANSWER_WORDS.deleted_reply,
  delivery: COMPOSER_ANSWER_WORDS.delivery,
  elsewhere: COMPOSER_ANSWER_WORDS.elsewhere,
  goneReply: COMPOSER_ANSWER_WORDS.gone_reply,
  keptReply: COMPOSER_ANSWER_WORDS.kept_reply,
  late: COMPOSER_ANSWER_WORDS.late,
  refusals: COMPOSER_ANSWER_WORDS.send_refusals,
  refused: COMPOSER_ANSWER_WORDS.refused,
  stopped: {
    expired: COMPOSER_ANSWER_WORDS.stopped.expired,
    interrupted: withKeyHours(COMPOSER_ANSWER_WORDS.stopped.interrupted),
    moved: COMPOSER_ANSWER_WORDS.stopped.moved,
    unconfirmed: withKeyHours(COMPOSER_ANSWER_WORDS.stopped.unconfirmed),
  },
  upload: {
    other: COMPOSER_ANSWER_WORDS.upload.other,
    refused: COMPOSER_ANSWER_WORDS.upload.refused,
    stopped: {
      expired: COMPOSER_ANSWER_WORDS.upload.stopped.expired,
      unconfirmed: withKeyHours(COMPOSER_ANSWER_WORDS.upload.stopped.unconfirmed),
    },
  },
};

/** The words the reference picker in the box draws, in the page's kind words: its names
 *  (`BOX_PICKER_NAMES`), its lines (`BOX_PICKER_LINES`), and the page's own words of a list of
 *  references still loading and one that could not be read. */
const PICKER: RefPickerWords = {
  headings: BOX_PICKER_NAMES.headings,
  instruction: BOX_PICKER_LINES.instruction,
  label: BOX_PICKER_NAMES.label,
  parties: BOX_PICKER_LINES.agents,
  parts: {
    blocks: { control: BOX_PICKER_NAMES.parts.blocks, none: BOX_PICKER_LINES.parts_none.blocks },
    documents: {
      control: BOX_PICKER_NAMES.parts.documents,
      none: BOX_PICKER_LINES.parts_none.documents,
    },
  },
  states: {
    empty: BOX_PICKER_LINES.empty,
    loading: ANSWER_LINES.references_loading,
    unreachable: ANSWER_LINES.references_unreadable,
  },
  stoppedRole: BOX_PICKER_LINES.stopped_role,
};

/** The names a screen reader reads the write line by, and its field's description, which says
 *  what its keys do; the edit line's say Enter saves (`EDIT_NAMES`). */
const WRITE_NAMES: OutlineNames = {
  description: CONTROL_LABELS.composer_keys,
  field: {
    ask: CONTROL_LABELS.composer_question,
    comment: CONTROL_LABELS.composer_comment,
    edit: CONTROL_LABELS.composer_edit,
  },
  form: CONTROL_LABELS.composer_form,
  targets: CONTROL_LABELS.composer_mentioned,
};
const EDIT_NAMES: OutlineNames = { ...WRITE_NAMES, description: CONTROL_LABELS.composer_keys_edit };

/** Where the box stands on the page, as the slot reads it off the address (`slots/foot.tsx`). */
export interface BoxPlace {
  /** A document is open in place of the outline (`IssueAddress.document`). */
  readonly documentOpen: boolean;
  /** A view is on (`IssueAddress.preset` is not `conversation`, `isFilter`): the stage draws a
   *  view, any control of the row but Conversation, and no outline. */
  readonly viewOn: boolean;
}

/**
 * Whether the box is drawn (R1): always while the address names no view and no document, and
 * otherwise only while the reader has asked to write - the writer holds a reply, a statement or
 * an edit, which a row's Reply or Edit sets there or in Details - or holds a send of the line's
 * own that is out or was refused. So a press is never answered by nothing, and a refusal is never
 * out of sight. An answer the writer holds draws nothing here, and the box sends none
 * (`OutlineWriter.answer`). The box reads it for itself, and the slot for the reach its wrapper
 * publishes (`useFootInset`).
 * A criterion the writer holds, set by a row's Write a criterion, draws the box as a reply does.
 */
export function useComposerShown({ documentOpen, viewOn }: BoxPlace): boolean {
  const writer = useOutlineWriter();
  const held = useHeldSend(writer?.sendKey);
  if (writer === undefined) return false;
  return (
    (!viewOn && !documentOpen) ||
    held !== undefined ||
    writer.reply !== null ||
    writer.statement !== undefined ||
    writer.criterion !== undefined ||
    writer.editing !== undefined
  );
}

/**
 * The box at the foot of the new issue page, held there by its slot (the prototype's
 * `#composer`): one line, which Enter sends, while the outline runs on under it. It is the
 * composer every box of the dashboard is (`MentionComposer`), in the look only this page asks for
 * (`OutlineLook`), so no file of the page holds a field of its own. It sends no answer to an ask
 * (`OutlineWriter.answer`).
 *
 * With no target it writes a comment on the issue, or a question with the Question chip pressed.
 * A row's Reply sets it to reply to that row - a comment, a message, an ask in any state - or to
 * comment on a statement, its target named above the line by its author and its kind; the
 * reader's own comment's Edit sets it to that comment's words, which Save writes back
 * (`outline-writer.tsx`). An open question's Answer, one of its options and a Change answer set
 * nothing here: while the writer holds an answer the box is as it is with no target.
 * A row's Write a criterion sets it to write a criterion under that row (`CriterionLine`).
 *
 * Once a write lands, the outline takes the reader to it (`Landing`): the row of what they wrote,
 * its branch unfolded, on screen and focused (R4). A comment on a statement is drawn as a row
 * under the issue, at the outline's end, and the reader is taken back to the statement's own
 * row; no margin beside the statement draws it yet. Under a view no outline is drawn to
 * consume a landing, so the box asks for none there and gives the focus back to where the reader
 * came to it from. Either way the focus never falls to the page's body as a line's controls
 * unmount.
 *
 * Hidden rather than unmounted while it is not drawn (`useComposerShown`), and the line that
 * writes on the issue stays mounted, hidden, while the line edits a comment, so what the reader
 * was typing is there when they come back to it.
 */
export function IssueComposer({
  documentOpen,
  isClosed,
  issueKey,
  tree,
  viewOn,
}: BoxPlace & {
  readonly isClosed: boolean;
  readonly issueKey: string;
  /** The outline's whole tree once every read it is built from and its names are held
   *  (`slots/foot.tsx`), which says which entries and statements a draft the write line keeps
   *  can go back to; none until then. */
  readonly tree: TreeNode | undefined;
}): ReactNode {
  const writer = useOutlineWriter();
  const shown = useComposerShown({ documentOpen, viewOn });
  const phone = useMediaQuery(HEADER_SCROLLS_QUERY);
  const section = useRef<HTMLElement | null>(null);
  // The element that held the focus when the reader came to the box - a row, or a control of
  // Details - while it is still in the document; the view's tab-stop row once it is not.
  const cameFrom = useRef<HTMLElement | null>(null);
  // Each ask to give the focus back, acted on once the commit that ended the line's target has
  // drawn: a line's controls unmount in that commit, which would take the focus after an earlier
  // move.
  const [givenBack, setGivenBack] = useState(0);
  // How far up from the screen's foot the box reaches while it is drawn, on the document element,
  // so a row that takes the focus is brought clear of it as it is of the band at the top
  // (`identity-block.ts`'s `bringOnScreen`). Unset while the box is hidden. The reach is the
  // section's own height - the box, the 8 px under it and the fade above it - which is how far up
  // it reaches while it is stuck to the foot, and not read off where it stands: at the outline's
  // end the read mark, which this reach moves down (`slots/foot.tsx`), lengthens the page, so a
  // box that has just shrunk stands above the foot, and a reach read off its place would hold the
  // mark that low, and the box that high, until the box next changed size.
  useLayoutEffect(() => {
    const element = section.current;
    if (!shown || element === null) return;
    const root = document.documentElement;
    const publish = () =>
      root.style.setProperty(FOOT_HEIGHT_PROPERTY, `${element.getBoundingClientRect().height}px`);
    publish();
    const sizes = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(publish);
    sizes?.observe(element);
    window.addEventListener("resize", publish);
    return () => {
      sizes?.disconnect();
      window.removeEventListener("resize", publish);
      root.style.removeProperty(FOOT_HEIGHT_PROPERTY);
    };
  }, [shown]);
  useEffect(() => {
    if (givenBack === 0) return;
    const from = cameFrom.current;
    cameFrom.current = null;
    // The element the reader came from while it is still drawn; else the tab-stop row of the
    // stage's tree, the outline's and each view's alike, outside Details' drawer: the row the
    // reader last stood on, or the view's own.
    const back =
      from?.isConnected === true && from.closest("[hidden]") === null
        ? from
        : [...document.querySelectorAll<HTMLElement>(`[data-issue-page] ${TAB_STOP_ROW}`)].find(
            (row) => row.closest(`aside[${DETAILS_ATTRIBUTE}]`) === null
          );
    back?.focus({ preventScroll: true });
  }, [givenBack]);
  if (writer === undefined) return null;
  const place: LinePlace = {
    atRest: !viewOn && !documentOpen,
    giveBack: () => setGivenBack((count) => count + 1),
    outlineDrawn: !viewOn,
    phone,
  };
  const writing = writer.editing === undefined;
  return (
    <section
      aria-label={CONTROL_LABELS.write}
      className={pageComposer.foot}
      data-testid="issue-composer"
      hidden={!shown}
      onFocus={(event) => {
        // Where the reader came to the box from. A move inside the box is no arrival, and nor is
        // one from nowhere - the window taking the focus back - which keeps the element the
        // reader came from before.
        const from = event.relatedTarget;
        if (!(from instanceof HTMLElement) || event.currentTarget.contains(from)) return;
        cameFrom.current = from;
      }}
      ref={section}
      {...authorText}
    >
      {writer.editing === undefined ? null : (
        <EditLine
          // One line per comment: its words in the box are that comment's.
          key={writer.editing.id}
          editing={writer.editing}
          issueKey={issueKey}
          place={place}
          writer={writer}
        />
      )}
      {writer.criterion === undefined ? null : (
        <CriterionLine
          // One line per node: what the reader typed is a criterion under that node.
          key={writer.criterion.id}
          issueKey={issueKey}
          place={place}
          target={writer.criterion}
          writer={writer}
        />
      )}
      {/* Last, so the line that is drawn is the first field of the section, and hidden rather
          than unmounted while the line edits: its draft is the reader's. */}
      <div data-composer-write="" hidden={!writing}>
        <div hidden={writer.criterion !== undefined}>
          <WriteLine
            isClosed={isClosed}
            issueKey={issueKey}
            place={place}
            tree={tree}
            writer={writer}
          />
        </div>
      </div>
    </section>
  );
}

/** What a line of the box reads of where it stands, to put the reader back once it is done. */
interface LinePlace {
  /** No view is on and no document is open, so the box stays drawn when the line's target ends
   *  (`useComposerShown`). */
  readonly atRest: boolean;
  /** Gives the focus back to where the reader came to the box from, once the commit that ends
   *  the line's target has drawn. */
  readonly giveBack: () => void;
  /** The stage draws the outline - no view is on, with or without a document open - whose tree
   *  takes the reader to a landing (`IssueTree.tsx`, the one consumer of one). */
  readonly outlineDrawn: boolean;
  /** 760 px and below, where each placeholder says its shorter words. */
  readonly phone: boolean;
}

/**
 * Ends the line's target and puts the reader back: on the outline, on the row `landing` names
 * once its read draws it (R4); under a view, where no outline is drawn to consume a landing and
 * one would act on the outline the next time it mounts, where the reader came to the box from.
 */
function finish(writer: OutlineWriter, place: LinePlace, landing: Landing): void {
  if (place.outlineDrawn) {
    writer.land(landing);
    writer.clear();
    return;
  }
  writer.clear();
  place.giveBack();
}

/** What the write line is aimed at: the entry a reply answers, or the statement a comment is on.
 *  The writer holds at most one of the two (`OutlineWriter.statement`), and the line reads its
 *  placeholder, its target line and where a send takes the reader off this one value. */
interface LineAim {
  /** The author the target line names: the entry's, or the statement's where its row carries
   *  one; none for a statement taken up from a held send, which keeps no author. */
  readonly author: Actor | undefined;
  /** The page's word for the target's kind, which the target line names it by. */
  readonly kind: NodeType;
  /** Where a send from the line takes the reader (`Landing.to`): their newest node under the
   *  entry a reply answers, or the statement's own row. */
  readonly landsOn: Landing["to"];
  /** Which of the line's placeholders it says while it is empty (`PLACEHOLDERS`). */
  readonly placeholder: "reply" | "statement";
  /** What a landing names (`Landing.under`): the entry, or the statement's row, which a
   *  statement the line took up from a held send names as well (`StatementAnchor.row`). */
  readonly under: NonNullable<Landing["under"]>;
}

/** The line that writes a comment, a question, a reply, or a comment on a statement: the
 *  composer in the issue page's look (`OutlineLook`). */
function WriteLine({
  isClosed,
  issueKey,
  place,
  tree,
  writer,
}: {
  isClosed: boolean;
  issueKey: string;
  place: LinePlace;
  tree: TreeNode | undefined;
  writer: OutlineWriter;
}): ReactNode {
  const { reply, statement } = writer;
  // The issue's comments and messages as the outline reads them, under its keys and kept fresh as
  // it keeps them (`OutlineWriterProvider`), so they cost no request of their own: the post a reply
  // answers as it stands now, which tells the composer a post its author deleted while the line
  // was open (`MentionComposer`'s `replyParent`).
  const comments = useQuery({ ...issueCommentsQuery(issueKey), ...EVENT_FRESH });
  const messages = useQuery({ ...issueMessagesQuery(issueKey), ...EVENT_FRESH });
  // A statement's comment carries the statement's document, its whole words and its row alone,
  // never the author a Reply aimed the line with, so a send held over a refusal or a reload
  // stores those three fields and nothing more.
  const anchor = useMemo(
    () =>
      statement === undefined
        ? undefined
        : { artifact: statement.artifact, quote: statement.quote, row: statement.row },
    [statement]
  );
  const owner = useMemo(() => ({ issueKey, kind: "issue" as const }), [issueKey]);
  // What the line is aimed at (`LineAim`); none for a write on the issue itself.
  const aim: LineAim | undefined =
    reply !== null
      ? {
          author: reply.actor,
          kind: reply.parentKind,
          landsOn: "newest",
          placeholder: "reply",
          under: { id: reply.id, kind: reply.parentKind },
        }
      : statement !== undefined
        ? {
            author: statement.author,
            kind: "requirement",
            landsOn: "self",
            placeholder: "statement",
            under: { id: statement.row, kind: "statement" },
          }
        : undefined;
  const aimed = aim !== undefined;
  const under = aim?.under;
  // A reply and a statement's comment are aimed at a row, so the kinds are drawn only while the
  // line writes on the issue. A closed issue takes a comment and no question, so the line writes
  // a comment there whatever chip was pressed while it was open, rather than holding a question
  // the issue refuses.
  const writesKinds = !aimed && !isClosed;
  // The target line's control and Escape. With no target they leave the line. With the box drawn
  // at rest the line goes back to writing on the issue with the caret kept in it, ending the
  // target itself: the issue's own Reply keeps a refused send's target (`writeOnIssue`), and
  // this is the line's own end of it. Elsewhere ending the target hides the box (R1), so the
  // reader is put back: on the row they were replying to where the outline is drawn, and where
  // they came from under a view.
  const end = () => {
    if (!aimed) {
      place.giveBack();
      return;
    }
    if (place.atRest) {
      writer.clear();
      writer.focus();
      return;
    }
    finish(writer, place, { to: "self", under });
  };
  // The targets the outline draws (`drawnTargets`), so a draft the line keeps for an entry or a
  // statement it no longer draws - a comment, a message or a question deleted, a statement a new
  // version took out or rewrote - is drawn beside the line with Discard my draft and Use it here,
  // rather than named with a way back that is gone. None until the foot hands the line a tree
  // whose reads have all answered, so no draft is drawn as gone while a read is out.
  const targets = useMemo(() => (tree === undefined ? undefined : drawnTargets(tree)), [tree]);
  const look: OutlineLook = {
    anchor,
    answers: ANSWERS,
    cancelReply: CONTROL_LABELS.write_on_issue,
    discardDraft: CONTROL_LABELS.discard_draft,
    end,
    endMark: CONTROL_LABELS.target_end_mark,
    kept: { issue: ANSWER_LINES.kept_on_issue, statement: ANSWER_LINES.kept_on_statement },
    kinds: writesKinds ? KINDS : [],
    names: WRITE_NAMES,
    picker: PICKER,
    placeholder: placeholderOf(aim?.placeholder ?? "issue", place.phone),
    retry: CONTROL_LABELS.retry,
    send: CONTROL_LABELS.send,
    sending: ANSWER_LINES.sending,
    statementTwice: ANSWER_LINES.statement_twice,
    statements:
      targets === undefined
        ? undefined
        : {
            drawn: (kept) => targets.statements.some((one) => sameStatement(one, kept)),
            drop: CONTROL_LABELS.discard_my_draft,
            gone: ANSWER_LINES.kept_statement_gone,
            goneWaits: ANSWER_LINES.kept_statement_gone_waits,
            take: CONTROL_LABELS.use_it_here,
          },
    target:
      aim === undefined ? undefined : (
        <TargetNamed author={aim.author} doing={CONTROL_LABELS.replying_to} kind={aim.kind} />
      ),
    uploadedUnanswered: ANSWER_LINES.uploaded_unanswered,
    uploading: ANSWER_LINES.uploading,
  };
  return (
    <MentionComposer
      box="issue-page-composer"
      closed={isClosed}
      kind={writesKinds ? writer.kind : "comment"}
      listed={targets?.entries}
      mutationKey={writer.sendKey}
      onClose={place.giveBack}
      onKindChange={(next) => {
        if (next === "ask" || next === "comment") writer.setKind(next);
        return undefined;
      }}
      // The reader is taken to what they wrote - their newest node under the row the line was
      // aimed at, or under the issue - and the line writes on the issue again. A comment on a
      // statement's text takes the reader back to the statement's own row, whichever row draws
      // the comment itself. Neither is refused here: the write store drops the send before it
      // calls this, so no send is out by then.
      onSent={() => finish(writer, place, { to: aim?.landsOn ?? "newest", under })}
      outline={look}
      owner={owner}
      replyParent={replyParentOf(reply, comments.data ?? [], messages.data ?? [])}
      replyTo={reply}
    />
  );
}

/** What the outline's tree draws that a draft the write line keeps can go back to: each entry a
 *  row's Reply answers (`replyItemOf`) - a comment, a message, a question - as standing, each
 *  comment or message whose author deleted it, which keeps its row and takes no Reply, as deleted
 *  (`ListedReplies`), and each statement as a Reply on its row aims the line
 *  (`statementTargetOf`). The line's `drawn` holds a kept draft's statement to each by
 *  `sameStatement`, the one rule for two statements being the same: the document and the whole
 *  words, and not the row. An entry the tree draws under two parents counts once. */
function drawnTargets(root: TreeNode): {
  readonly entries: ListedReplies;
  readonly statements: readonly StatementAnchor[];
} {
  const entries = new Map<string, ListedReply>();
  const statements: StatementAnchor[] = [];
  for (const node of nodesInOrder(root)) {
    const item = replyItemOf(node);
    if (item?.kind === "statement") {
      statements.push(statementTargetOf(item));
    } else if (item !== undefined) {
      entries.set(item.id, "standing");
    } else if (node.deletedAt !== undefined) {
      const deleted = conversationItem(node);
      if (deleted !== undefined) entries.set(deleted.id, "deleted");
    }
  }
  return { entries, statements };
}

/** The reader's own comment, edited in the line: its words in the box, Save writes them back,
 *  and the outline draws the comment again from the read that answers after and takes the
 *  reader back to its row. */
function EditLine({
  editing,
  issueKey,
  place,
  writer,
}: {
  editing: { readonly body: string; readonly id: string };
  issueKey: string;
  place: LinePlace;
  writer: OutlineWriter;
}): ReactNode {
  const queryClient = useQueryClient();
  // The comment's author, which the target line names it by, from the read the outline is drawn
  // from.
  const comments = useQuery(issueCommentsQuery(issueKey));
  const author = comments.data?.find((one) => one.id === editing.id)?.author;
  const owner = useMemo(() => ({ issueKey, kind: "issue" as const }), [issueKey]);
  const mutationKey = useMemo(
    () => [...writer.sendKey, "edit", editing.id],
    [editing.id, writer.sendKey]
  );
  // Escape, the target line's control, and a save that landed: the edit ends, and the reader is
  // back on the comment's row.
  const back = () =>
    finish(writer, place, { to: "self", under: { id: editing.id, kind: "comment" } });
  const look: OutlineLook = {
    answers: ANSWERS,
    cancelReply: CONTROL_LABELS.write_on_issue,
    discardDraft: CONTROL_LABELS.discard_draft,
    end: back,
    endMark: CONTROL_LABELS.target_end_mark,
    kinds: [],
    names: EDIT_NAMES,
    picker: PICKER,
    placeholder: placeholderOf("edit", place.phone),
    retry: CONTROL_LABELS.retry,
    send: CONTROL_LABELS.save,
    sending: ANSWER_LINES.saving_field,
    target: <TargetNamed author={author} doing={CONTROL_LABELS.editing} kind="comment" />,
    uploadedUnanswered: ANSWER_LINES.uploaded_unanswered,
    uploading: ANSWER_LINES.uploading,
  };
  return (
    <MentionComposer
      box="issue-page-composer"
      edit={editing}
      mutationKey={mutationKey}
      onClose={back}
      onSent={back}
      outline={look}
      owner={owner}
      saveEdit={async (id, body, carried, bounds) => {
        const saved = await saveCommentEdit(id, body, carried, bounds);
        await queryClient.invalidateQueries({ queryKey: issueCommentsQuery(issueKey).queryKey });
        return saved;
      }}
    />
  );
}

/** The line that writes a criterion under a node, once the issue's tree read names the tree
 *  artifact it is written into (`useRoomFacts`). With none read it draws nothing, and no row
 *  offers the act that aims it (`OutlineWriter.takesCriterion`). */
function CriterionLine({
  issueKey,
  place,
  target,
  writer,
}: {
  issueKey: string;
  place: LinePlace;
  target: CriterionItem;
  writer: OutlineWriter;
}): ReactNode {
  const tree = useRoomFacts(issueKey)?.tree;
  return tree === null || tree === undefined ? null : (
    <CriterionForm issueKey={issueKey} place={place} target={target} tree={tree} writer={writer} />
  );
}

/**
 * The criterion line: "Criterion under:", the node's author and the page's word for its kind
 * above it (`TargetNamed`), with none of the node's words, then the field and Send. Enter sends
 * and Shift+Enter breaks the line; Escape, from any control of the line, and the target line's
 * control end the aim and take the reader back to the node's row. The text stays in the field
 * while the write is out and after a refusal, whose sentence is drawn under the line; a landing
 * lets the text go and takes the reader to their new criterion under the node. While the write
 * is out the box keeps the line's aim (`OutlineWriter.holdCriterion`): no other target, Escape or
 * the target line's control ends it, so its text and its refusal stay to be read.
 *
 * A send disables the line while its write is out, and the browser then moves the focus from
 * the field to the page. A refusal gives it back to the field, so Escape and the next Enter reach
 * the line again; a focus the reader moved elsewhere meanwhile stays where they put it.
 *
 * It is a form of its own, with the line that writes on the issue kept mounted and hidden beside
 * it, so the page keeps one box. Its words go into the issue's own text, which tells nobody they
 * name, and its hint says so in words of its own (`criterion`).
 */
function CriterionForm({
  issueKey,
  place,
  target,
  tree,
  writer,
}: {
  issueKey: string;
  place: LinePlace;
  target: CriterionItem;
  tree: string;
  writer: OutlineWriter;
}): ReactNode {
  const form = useRef<HTMLFormElement>(null);
  const [pickerOpen, setPickerOpen] = useState(false);
  const under: Landing["under"] = { id: target.id, kind: target.kind };
  const back = () => finish(writer, place, { to: "self", under });
  const hold = writer.holdCriterion;
  const line = useCriterionLine({
    issueKey,
    // A write lands whether or not its line is still drawn: the box holds the aim while the
    // write is out, but the page can stop drawing the line. One that lands after its line went
    // takes the reader nowhere and ends nothing, and lets go of no hold: the line let go of it
    // as it went, and a later line's own write may hold the box now.
    onLanded: () => {
      if (form.current !== null) finish(writer, place, { to: "newest", under });
    },
    onOut: (out) => {
      if (form.current !== null) hold(out);
    },
    parent: target.address,
    tree,
  });
  // A line the page stops drawing while its write is out lets go of the box.
  useEffect(() => () => hold(false), [hold]);
  // Whether a write of the line was out at the last render: its end with a refusal gives the
  // field back the focus its disabled line moved to the page.
  const wasOut = useRef(false);
  useEffect(() => {
    if (line.pending) {
      wasOut.current = true;
      return;
    }
    if (!wasOut.current) return;
    wasOut.current = false;
    if (line.answer !== undefined && focusOnDocument()) {
      // The line's one field is its only textarea (`MentionField`).
      form.current?.querySelector<HTMLTextAreaElement>("textarea")?.focus();
    }
  }, [line.answer, line.pending]);
  return (
    <form
      aria-label={CONTROL_LABELS.criterion_target}
      className={pageComposer.box}
      data-criterion-under={target.id}
      data-issue-composer=""
      onKeyDown={(event) => {
        if (event.key !== "Escape") return;
        event.preventDefault();
        event.stopPropagation();
        // While the write is out the line keeps its aim, as the fieldset holds its ×.
        if (!line.pending) back();
      }}
      onSubmit={(event) => {
        event.preventDefault();
        line.send();
      }}
      ref={form}
    >
      {/* Held while the write is out: the node it names is the criterion's parent, fixed once
          the send is out. */}
      <fieldset className="flex min-w-0 flex-col gap-[3px] border-0 p-0" disabled={line.pending}>
        <OutlineTargetLine
          clearLabel={CONTROL_LABELS.write_on_issue}
          mark={CONTROL_LABELS.target_end_mark}
          onClear={back}
        >
          <TargetNamed
            author={target.author}
            doing={CONTROL_LABELS.criterion_target}
            kind={target.kind}
          />
        </OutlineTargetLine>
        {/* The line wraps only for what asks a row of its own, as the write line does: the open
            picker's list, and on a phone Send under the field while it holds the caret. */}
        <div className={`${pageComposer.line} flex-wrap`}>
          <MentionField
            body={line.text}
            disabled={line.pending}
            issueKey={issueKey}
            label={CONTROL_LABELS.write_criterion}
            labelHidden
            look={pageComposer.field}
            onChange={(change) => line.setText(change.body)}
            // Enter sends and Shift+Enter breaks the line, as on the box's other lines.
            onKeyDown={(event) => sendOnEnter(event, pickerOpen, line.send)}
            onPickerOpenChange={setPickerOpen}
            placeholder={placeholderOf("criterion", place.phone)}
            writing={dispatchWritingBox("issue-page-composer", "criterion")}
          />
          {/* A press here leaves the focus where it is (`keepFocusOnPress`), so on a phone the
              line keeps its form under the pointer and the press lands where it was aimed. */}
          {/* biome-ignore lint/a11y/noStaticElementInteractions: nothing here is made interactive - the handler only keeps a press on the buttons inside from moving the focus, and each of them is its own control. */}
          <div className={pageComposer.controls} onMouseDown={keepFocusOnPress}>
            <RefusableButton
              busy={line.pending ? ANSWER_LINES.sending : undefined}
              look="page-send"
              // An empty line says why it sends nothing: what to type, in the shorter words of
              // its placeholder, which fit beside Send at any width.
              refusal={line.blank ? CONTROL_LABELS.criterion_placeholder_phone : null}
              type="submit"
            >
              {CONTROL_LABELS.send}
            </RefusableButton>
          </div>
        </div>
        {line.answer === undefined ? null : (
          <p data-criterion-answer="" role="status" {...pageChrome}>
            {ANSWER_LINES[line.answer]}
          </p>
        )}
      </fieldset>
    </form>
  );
}

/**
 * What a target line says of its target (R3, R5, R11): what the line does to it - "Replying to:"
 * or "Editing:" - the target's author, where the line knows one, named as the page names an
 * author (`AgentName`'s `author`), so a person who acted through a session reads as their row
 * names them, and the page's word for its kind, with a middle dot between the two. It draws none
 * of the target's words: the entry stands whole in its own row, and an edited comment's words
 * stand whole in the field.
 * The criterion line names the node a criterion goes under the same way, after "Criterion under:".
 */
function TargetNamed({
  author,
  doing,
  kind,
}: {
  author: Actor | undefined;
  doing: string;
  kind: NodeType;
}): ReactNode {
  return (
    <>
      <span {...pageChrome}>{doing}</span>{" "}
      {author === undefined ? null : (
        <>
          <b className="font-semibold text-page-ink">
            <AgentName actor={author} author inline />
          </b>{" "}
          <span aria-hidden="true">{CONTROL_LABELS.target_separator}</span>{" "}
        </>
      )}
      <span {...pageChrome}>{NODE_TYPE_NAMES[kind]}</span>
    </>
  );
}
