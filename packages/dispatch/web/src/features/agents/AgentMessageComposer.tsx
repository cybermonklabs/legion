import { hashKey, type MutationKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useLayoutEffect, useRef, useState } from "react";

import { api } from "../../api/client";
import { agentConversationQuery } from "../../api/queries";
import type { Agent } from "../../api/types";
import { MultiSelect } from "../../components/MultiSelect";
import type { Sending } from "../../hooks/useSending";
import {
  borderDefault,
  secondaryButtonBorder,
  secondaryButtonDisabledText,
  secondaryButtonHoverBorder,
  secondaryButtonText,
} from "../../theme/classes";
import { seededDraft } from "../conversation/composer-draft";
import type { ListedReplies, ReplyParent, ReplyTarget } from "../conversation/composer-model";
import { useHeldSends } from "../conversation/held-sends";
import { type HostDraft, MentionComposer } from "../conversation/MentionComposer";
import { PendingSendTurn } from "../conversation/PendingSendTurn";
import { useSessionSends } from "../conversation/useSessionSends";
import { meInboxReadsBack } from "../inbox/one-read-cache";
import { focusOnDocument } from "../shell/roving";
import {
  AGENT_ROW_SELECTOR,
  ISSUE_LIST_MARKS,
  ISSUE_PICKER_SELECTOR,
  issueSearchOf,
} from "./keyboard";

/** A reply the agent composer answers: the quoted message and the conversation it belongs to
 *  (an issue, or none), which decides the route the reply is created through. */
export interface AgentReply {
  readonly issueKey: string | null;
  readonly target: ReplyTarget;
}

/** The name of an agent row's send, which names the session it is held for: the row reads it
 *  (`useSending`) to hold what a message on its way has already fixed - the picker and the Reply
 *  buttons - and the composer names its send with it, so the two keys cannot drift apart. A row
 *  that mounts again while its send is out, or after it was refused - the reader came back to the
 *  page - shows that send (`useComposerSend`), and holds for it. The Inbox's thread box names its
 *  send by its thread instead (`threadSendKey` in ThreadBody.tsx): two threads can be one
 *  session's, and each holds its own reply. */
export function agentSendKey(sessionID: string): MutationKey {
  return ["agent-composer", sessionID];
}

/**
 * An agent row's composer: the issue picker and the `MentionComposer` it addresses. The row keeps
 * it mounted from its first open on, collapsed or moved, so an unsent draft, the issue picked and
 * its uploads stay with it; a send it has out, and that send's refusal, are the held-send store's,
 * which keeps them for the session whatever unmounts the row.
 */
export function AgentMessageComposer({
  agent,
  heldSend = false,
  hold,
  hostDraft,
  listed,
  onCancelReply,
  onClose,
  onSent,
  replyParent,
  replyTo,
  sendKey,
  standing = false,
  thread,
}: {
  agent: Agent;
  /** Holds Send until the Inbox is read again after the send lands (`MentionComposer`'s
   *  `heldSend`): the Inbox's Agent row passes it, since the reader's own reply takes the thread
   *  out of their Inbox (the own-answer rule, R1). The read it waits for is the one the send's
   *  landing already started (`held-sends.ts`' `refreshAfter`), waited on and not sent again
   *  (`meInboxReadsBack`). */
  heldSend?: boolean;
  /** The row's hold on that send (`useSending(sendKey)`): while it is out the message is
   *  addressed, so the picker holds until it lands, from Send's own task on. The send's request is
   *  frozen at Send either way. */
  hold: Sending;
  /** A host that keeps the message itself (`MentionComposer`'s `hostDraft`): the box starts from
   *  `starting`, and every message it holds is reported to `onChange` as the person wrote it,
   *  without the mention of this session an issue's channel writes in front of it. The Inbox's
   *  compose dialog passes it, so the message stays when the person adds or removes a recipient. */
  hostDraft?: HostDraft;
  /** The messages of the session's conversation the row lists (`MentionComposer`'s `listed`),
   *  or none where its host has not read them, or where the box answers only its own thread's
   *  newest message (`standing`), which leaves the page with the box. */
  listed?: ListedReplies;
  onCancelReply: () => void;
  /** One level out of the composer: the row it belongs to takes focus. The composer calls it on
   *  Escape from an untouched draft and on Discard - and also right after a successful send,
   *  which is NOT one level out; that case is filtered below. */
  onClose: () => void;
  /** Told that a send landed, with Dispatch's answer: the message to the session, or the comment
   *  on the issue picked that mentions it. A host that passes it draws what follows the send
   *  itself, so a held Send waits for no read once the answer is in. */
  onSent?: (answer: unknown) => void;
  /** The message `replyTo` quotes, as the host's read of the session's exchanges holds it now:
   *  its quote says the message is deleted or edited (`MentionComposer`'s `replyParent`). */
  replyParent?: ReplyParent;
  replyTo: AgentReply | null;
  /** The name of the composer's send: the row's `agentSendKey`, or the Inbox thread's own. */
  sendKey: MutationKey;
  /** The reply target is this box's standing address rather than a Reply the reader pressed,
   *  so the box takes no focus of its own (`MentionComposer`'s `standing`): what the Inbox
   *  column's thread box is, mounted again for every row the reader steps onto. */
  standing?: boolean;
  /** The ids of the messages of the one thread this box answers, where it is a thread's box (the
   *  Inbox's): of the session's other sends it shows only those answering one of them, since a
   *  send into another thread, or a message of its own, is not this thread's. */
  thread?: ReadonlySet<string>;
}): ReactNode {
  const queryClient = useQueryClient();
  const store = useHeldSends();
  const { sending, sendingNow } = hold;
  // The session's other messages still on their way - its live view's - show here too, as their
  // own turns, and Retry, Discard or Stop trying here acts on the same write there: the session is
  // the write's target, not the surface that made it. This composer's own send shows in it. A
  // thread's box shows those answering its thread alone.
  const sends = useSessionSends(agent.session_id);
  const ownName = hashKey(sendKey);
  const others = sends.pending.filter(
    (held) =>
      hashKey(held.mutationKey) !== ownName &&
      (thread === undefined ||
        (held.request.replyTo !== null && thread.has(held.request.replyTo.id)))
  );
  // The issue a held send is addressed to - out, or refused - is the one the picker names when
  // the row mounts again, so what the toggle says and where Retry sends stay one issue. A pick
  // made over a refusal carries it to the issue picked (`ownMoves` below), so that is the one.
  const [issueKey, setIssueKey] = useState(() => {
    const held = store.get(sendKey)?.request;
    return held?.replyTo === null && held.owner.kind === "issue" ? held.owner.issueKey : "";
  });
  const [issuePickerOpen, setIssuePickerOpen] = useState(false);
  // `MentionComposer` calls `onSent` and then `onClose` on a successful send, and a reader who has
  // just sent a message is still writing to this agent: moving focus to the row would turn their
  // next letters into `x` / `i` / `Shift+P` shortcuts. The flag is set on the way past `onSent`
  // and consumed by the `onClose` that follows it.
  const sentJustNow = useRef(false);
  const box = useRef<HTMLDivElement>(null);
  // `MentionComposer` holds its control fieldset while a send is in flight (`aria-disabled`, each
  // text field read-only), never disabled, so this textarea keeps the focus through the send. A
  // focus that is on the document once the send lands is given back to the field then, and a
  // field disabled at that moment is given it once it is enabled again - watched, rather than
  // guessed at with a frame or timer.
  const refocusWatcher = useRef<MutationObserver | null>(null);
  useEffect(() => () => refocusWatcher.current?.disconnect(), []);
  /** Only a focus left on the document is the composer's to give back: a reader who has clicked
   *  something else while the send was out keeps where they went - otherwise the next keys,
   *  `Ctrl+Enter` included, would land in the composer they have already sent from, addressed to
   *  another agent. */
  const refocusComposer = () => {
    const field = box.current?.querySelector("textarea");
    if (field === null || field === undefined) return;
    const takeBack = () => {
      if (focusOnDocument()) field.focus();
    };
    refocusWatcher.current?.disconnect();
    if (!field.matches(":disabled")) {
      takeBack();
      return;
    }
    const watcher = new MutationObserver(() => {
      if (field.matches(":disabled")) return;
      watcher.disconnect();
      takeBack();
    });
    watcher.observe(field.closest("fieldset") ?? field, { attributeFilter: ["disabled"] });
    refocusWatcher.current = watcher;
  };
  /** Where the focus goes once the Issue list has closed, set by the close that moves it on: a
   *  pick - the issue changed or not - hands the reader to the message it addresses, and Escape
   *  is one level out, to the row. The list has put the focus on its trigger by then, and leaves
   *  a focus moved on from there where it is. */
  const [handOff, setHandOff] = useState<{ readonly to: "field" | "row" } | null>(null);
  /** Bumped by every pick, the issue changed or not. The composer counts it among the reader's
   *  own moves of its address (`ownMoves`): a pick is the reader's, so the draft goes with it, and
   *  a refusal and the files pasted into the draft with it. */
  const [commits, setCommits] = useState(0);
  const commitIssue = (value: string) => {
    if (sendingNow()) return;
    setIssueKey(value);
    setCommits((count) => count + 1);
    setHandOff({ to: "field" });
  };
  // A layout effect, so the frame the close paints already has the focus where it goes rather
  // than on the trigger: the reader's next keystroke is the message, or the row's.
  useLayoutEffect(() => {
    if (handOff === null) return;
    if (handOff.to === "field") box.current?.querySelector("textarea")?.focus();
    else box.current?.closest<HTMLElement>(AGENT_ROW_SELECTOR)?.focus();
  }, [handOff]);
  const issues = useQuery({
    enabled: issuePickerOpen,
    queryFn: () => api.listIssues({ open: true }),
    queryKey: ["agents", "issue-picker"],
  });
  const titles = new Map((issues.data ?? []).map((issue) => [issue.key, issue.title]));
  /** The committed issue, once the open list has come back without it: closed since it was
   *  picked. The list and the toggle keep naming it, marked, until the reader picks another -
   *  the issue header's Status pick keeps a closed issue's own status among its options the same
   *  way - so what the picker shows, what the toggle says and where the message goes stay one
   *  issue; a send to it posts, since a closed issue takes comments. */
  const committedClosed = issueKey !== "" && issues.data !== undefined && !titles.has(issueKey);
  const committedLabel = committedClosed ? `${issueKey} (closed)` : issueKey;
  // "No issue" is a pick no read stands behind, so only a list still on its way offers nothing;
  // a shut list draws no options, so it is handed none.
  const issueOptions =
    !issuePickerOpen || issues.isPending
      ? []
      : ["", ...(committedClosed ? [issueKey] : []), ...titles.keys()];
  // The list opens at once and draws its issues once the read lands, and only then takes the
  // focus, into its text box: it is what `i` needs (a key that opened something no keystroke
  // could then reach would be a dead end) and what a pointer wants too.
  //
  // That wait is the whole latency of `GET /issues`, and a reader who has roved on in the
  // meantime keeps where they went - `takeBack`'s rule above, widened to the row this composer
  // belongs to: focus is the list's to take only while it is still where the open left it.
  // Once per open, so a refetch behind the reader never pulls them back either.
  const pickerTookFocus = useRef(false);
  useEffect(() => {
    if (!issuePickerOpen) {
      pickerTookFocus.current = false;
      return;
    }
    if (issues.data === undefined || pickerTookFocus.current) return;
    const toggle = box.current?.querySelector(ISSUE_PICKER_SELECTOR) ?? null;
    const search = toggle === null ? null : issueSearchOf(toggle);
    if (search === null) return;
    // Where the open can have left focus, named: the row `i` was pressed on, the toggle a
    // pointer clicked, or nothing at all. A reader who has gone on - to another row, or into
    // this composer's own field - keeps where they went.
    const active = document.activeElement;
    const openedOn =
      focusOnDocument() || active === box.current?.closest(AGENT_ROW_SELECTOR) || active === toggle;
    if (!openedOn) return;
    pickerTookFocus.current = true;
    search.focus();
  }, [issuePickerOpen, issues.data]);
  // Replies retain their parent owner: issue-attached legacy messages stay on that issue's
  // message route, while issue-less roots keep the S3-deferred direct session channel.
  const replyIssueKey = replyTo?.issueKey;
  const useDirectChannel =
    replyIssueKey === null || (replyIssueKey === undefined && issueKey === "");
  const composerOwner = useDirectChannel
    ? { kind: "session" as const, sessionId: agent.session_id }
    : { issueKey: replyIssueKey ?? issueKey, kind: "issue" as const };
  // The host keeps the message the person wrote; the mention of this session is the issue
  // channel's, which the box writes in front of it again at its next mount.
  const keptDraft =
    hostDraft === undefined
      ? undefined
      : {
          onChange: (body: string) =>
            hostDraft.onChange(
              seededDraft([], body, { kind: "session", sessionId: agent.session_id })
            ),
          starting: hostDraft.starting,
        };

  return (
    <div className={`mt-3 border-t pt-3 ${borderDefault}`} ref={box}>
      {replyTo === null ? (
        <MultiSelect
          disabled={sending}
          emptyMessage={issues.isPending ? "Loading issues…" : "No issue matches that."}
          error={issues.isError ? "Could not load issues." : null}
          focusOnOpen={false}
          label="Issue"
          marks={ISSUE_LIST_MARKS}
          mode="single"
          onOpenChange={(open, closedBy) => {
            if (open) {
              if (!sendingNow()) setIssuePickerOpen(true);
              return;
            }
            setIssuePickerOpen(false);
            if (closedBy === "escape") setHandOff({ to: "row" });
          }}
          onPick={commitIssue}
          open={issuePickerOpen}
          // An issue's key and title, as the characters they are: the reading list's
          // `agent-message-issue-option`.
          optionLabel={(key) =>
            key === ""
              ? "No issue"
              : key === issueKey && committedClosed
                ? committedLabel
                : `${key} · ${titles.get(key)}`
          }
          options={issueOptions}
          searchLabel="Issue"
          triggerAriaLabel="Choose issue"
          triggerClassName={`min-h-11 rounded-lg border px-3 text-sm font-medium disabled:cursor-not-allowed ${secondaryButtonBorder} ${secondaryButtonText} ${secondaryButtonHoverBorder} ${secondaryButtonDisabledText}`}
          value={issueKey}
        >
          Issue: {issueKey === "" ? "No issue" : committedLabel}
        </MultiSelect>
      ) : null}
      {others.map((held) => (
        <PendingSendTurn held={held} key={held.id} sends={sends} />
      ))}
      <MentionComposer
        box="agents-row-composer"
        // The channel decides which mention the message needs - an issue comment reaches this
        // agent by mentioning it, a direct message does not. The composer takes a changed seed
        // into its live draft in place, so one instance holds the draft across every pick.
        seedMentions={
          useDirectChannel
            ? undefined
            : [{ target: `session:${agent.session_id}`, title: agent.title || agent.session_id }]
        }
        heldSend={heldSend}
        hostDraft={keptDraft}
        listed={listed}
        mutationKey={sendKey}
        onCancelReply={onCancelReply}
        onClose={() => {
          if (sentJustNow.current) {
            sentJustNow.current = false;
            return;
          }
          onClose();
        }}
        onSent={(answer) => {
          sentJustNow.current = true;
          onCancelReply();
          void queryClient.invalidateQueries({
            queryKey: agentConversationQuery(agent.session_id).queryKey,
          });
          // A focus left on the document goes back to the field (`refocusComposer`).
          refocusComposer();
          if (onSent !== undefined) {
            onSent(answer);
            return undefined;
          }
          return heldSend ? meInboxReadsBack(queryClient) : undefined;
        }}
        owner={composerOwner}
        ownMoves={commits}
        replyParent={replyParent}
        replyTo={replyTo?.target ?? null}
        standing={standing}
      />
    </div>
  );
}
