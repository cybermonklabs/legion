import { useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";

import type { Actor } from "../../api/types";
import { pageComposer } from "../../theme/classes";
import {
  type ListedReplies,
  type ListedReply,
  replyParentOf,
} from "../conversation/composer-model";
import { useHeldSend } from "../conversation/held-sends";
import {
  MentionComposer,
  type OutlineAnswers,
  type OutlineKind,
  type OutlineLook,
  type OutlineNames,
} from "../conversation/MentionComposer";
import { type StatementAnchor, sameStatement, saveCommentEdit } from "../conversation/send-request";
import { AgentName, type AgentStateWords } from "../refs/AgentName";
import type { RefPickerWords } from "../refs/ref-picker";
import { EVENT_FRESH } from "../refs/reference-target";
import { useMediaQuery } from "../shell/useDialog";
import { FOOT_HEIGHT_PROPERTY, HEADER_SCROLLS_QUERY } from "./identity-block";
import type { TreeNode } from "./issue-tree";
import { DETAILS_ATTRIBUTE } from "./margin-slot";
import {
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
  stopped: COMPOSER_ANSWER_WORDS.stopped,
  upload: COMPOSER_ANSWER_WORDS.upload,
};

/** What the state mark before an agent the target line names says (`AgentStateWords`), and the
 *  one before the agent a row of the box's picker names (`PICKER`), in the page's words: a
 *  running session's state, how long ago it was seen and what an unresponsive one's state means
 *  (`agent_running`, `agent_seen`, `agent_unresponsive_note`), and the other states in the words
 *  the claim says them in, which the registry judges alike, with no note after them (`notes`). */
const TARGET_STATE_WORDS: AgentStateWords = {
  labels: {
    "not-running": CONTROL_LABELS.not_running,
    running: CONTROL_LABELS.agent_running,
    unknown: CONTROL_LABELS.unchecked,
    unresponsive: CONTROL_LABELS.unresponsive,
  },
  notes: {
    "not-running": "",
    running: "",
    unknown: "",
    unresponsive: CONTROL_LABELS.agent_unresponsive_note,
  },
  seen: CONTROL_LABELS.agent_seen,
};

/** The words the reference picker in the box draws, in the page's kind words: its names
 *  (`BOX_PICKER_NAMES`), its lines (`BOX_PICKER_LINES`), the page's own words of a list of
 *  references still loading and one that could not be read, the state mark before the agent a
 *  party's row names, as the target line names it (`TARGET_STATE_WORDS`), and that agent named by
 *  the words the row draws for it, the page's word for a session nobody titled among them, never
 *  its short form (`sessionNames`, decision 548). */
const PICKER: RefPickerWords = {
  headings: BOX_PICKER_NAMES.headings,
  instruction: BOX_PICKER_LINES.instruction,
  label: BOX_PICKER_NAMES.label,
  marks: TARGET_STATE_WORDS,
  parties: BOX_PICKER_LINES.agents,
  parts: {
    blocks: { control: BOX_PICKER_NAMES.parts.blocks, none: BOX_PICKER_LINES.parts_none.blocks },
    documents: {
      control: BOX_PICKER_NAMES.parts.documents,
      none: BOX_PICKER_LINES.parts_none.documents,
    },
  },
  sessionNames: "shown",
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
  /** A view is on (`IssueAddress.preset` is not `conversation`, Dashboard included): the stage
   *  draws a view, any control of the row but Conversation, and no outline. */
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
      {/* Last, so the line that is drawn is the first field of the section, and hidden rather
          than unmounted while the line edits: its draft is the reader's. */}
      <div data-composer-write="" hidden={!writing}>
        <WriteLine
          isClosed={isClosed}
          issueKey={issueKey}
          place={place}
          tree={tree}
          writer={writer}
        />
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
    editing: writer.editing,
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

/**
 * What a target line says of its target (R3, R5, R11): what the line does to it - "Replying to:"
 * or "Editing:" - the target's author, where the line knows one, named as the page names an
 * author (`AgentName`'s `author`), so a person who acted through a session reads as their row
 * names them, an agent's state mark named in the page's words (`TARGET_STATE_WORDS`), and the
 * page's word for its kind, with a middle dot between the two. It draws none of the target's
 * words: the entry stands whole in its own row, and an edited comment's words stand whole in the
 * field.
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
            <AgentName actor={author} author inline stateWords={TARGET_STATE_WORDS} />
          </b>{" "}
          <span aria-hidden="true">{CONTROL_LABELS.target_separator}</span>{" "}
        </>
      )}
      <span {...pageChrome}>{NODE_TYPE_NAMES[kind]}</span>
    </>
  );
}
