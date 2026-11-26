import { type MutationKey, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useMemo, useState } from "react";

import { myRepliesKey } from "../../../api/queries";
import type { Actor } from "../../../api/types";
import type { ReplyParent, ReplyTarget } from "../../conversation/composer-model";
import { MentionComposer } from "../../conversation/MentionComposer";
import { saveCommentEdit } from "../../conversation/send-request";
import { agentsQuery, EMPTY_AGENTS } from "../../refs/agent-registry";

/** Nothing to do where the composer would close: a box of Threads goes by its host's state. */
const noClose = (): void => {};

/** The agents a box's mentions offer, from the client's cache of `GET /api/v1/agents`: the read
 *  is enabled only once the reader reaches for the box, by focus, pointer or touch, so opening a
 *  record sends none (R15), as Dashboard's thread item reaches for them (`ThreadItem.tsx`). */
function useReachedAgents(): { agents: typeof EMPTY_AGENTS; reach: () => void } {
  const [reached, setReached] = useState(false);
  const agents = useQuery({ ...agentsQuery(), enabled: reached }).data ?? EMPTY_AGENTS;
  return { agents, reach: () => setReached(true) };
}

/** The post a box answers, as the record holds it now. */
export interface AnsweredPost extends ReplyParent {
  readonly id: string;
  readonly author: Actor;
}

/**
 * A box of Threads that writes a comment (dispatch://INFRA-5704/spec#b-r-mentions, R9, R11): the
 * box at the foot of a comment thread or of a settled ask, and the box a reply control opens under
 * a comment. It is a `MentionComposer` with the `@` picker, as Dashboard's thread item mounts one
 * (`ThreadItem.tsx`), under this part's own box id (`threads-reply` in `DISPATCH_WRITING_BOXES`),
 * so `@` offers the parties the picker offers on `main` and the agent picked is stored in the
 * comment's mentions. A reply to a comment names it (`reply_to`), which the server stores as its
 * thread's head with the comment it answers (`answers`), and on a settled ask it names the ask
 * (`ask_id`).
 *
 * Its send name is the box's own, so a draft typed in a box that goes - another entry's Reply,
 * Cancel reply, Escape - is kept under it, and the box that opens there again shows it. The box
 * at the foot is the record's standing reply, which nothing ends.
 */
export function CommentBox({
  closed,
  issueKey,
  onLeave,
  parent,
  parentKind,
  sendKey,
}: {
  /** The issue is closed, which takes a comment all the same; the composer says so itself. */
  readonly closed: boolean;
  readonly issueKey: string;
  /** The reader ends the reply with Cancel reply or Escape. The box at the foot takes none: it is
   *  the record's standing reply. */
  readonly onLeave?: () => void;
  readonly parent: AnsweredPost;
  readonly parentKind: "ask" | "comment";
  readonly sendKey: MutationKey;
}): ReactNode {
  const queryClient = useQueryClient();
  const { agents, reach } = useReachedAgents();
  const owner = useMemo(() => ({ issueKey, kind: "issue" as const }), [issueKey]);
  const replyTo = useMemo<ReplyTarget>(
    () => ({ actor: parent.author, excerpt: "", id: parent.id, parentKind }),
    [parent.author, parent.id, parentKind]
  );
  return (
    <fieldset
      className="m-0 min-w-0 border-0 p-0"
      onFocus={reach}
      onPointerEnter={reach}
      onTouchStart={reach}
    >
      <MentionComposer
        agentNames="cache"
        agents={agents}
        box="threads-reply"
        closed={closed}
        inline
        mutationKey={sendKey}
        onCancelReply={onLeave}
        onClose={noClose}
        onSent={() => {
          // Threads' pages stand under these prefixes (plan choice 3), so the record draws the
          // reply where it was sent; the reader's own write moves their unread threads too.
          void queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
          if (parentKind === "ask")
            void queryClient.invalidateQueries({ queryKey: ["asks", issueKey] });
          void queryClient.invalidateQueries({ queryKey: myRepliesKey });
          onLeave?.();
        }}
        owner={owner}
        replyParent={parent}
        replyTo={replyTo}
        standing={onLeave === undefined}
      />
    </fieldset>
  );
}

/**
 * The reader's own comment opened to edit (R9): a `MentionComposer` over its words, which saves
 * them with `PATCH /api/v1/comments/{id}` (`saveCommentEdit`), as part L's edit line does
 * (`IssueComposer.tsx`'s `EditLine`), under this part's own box id (`threads-comment-edit` in
 * `DISPATCH_WRITING_BOXES`). Once the save lands, the issue's comment reads are read again, which
 * Threads' comment pages stand under, and the box goes.
 */
export function CommentEditBox({
  body,
  id,
  issueKey,
  onLeave,
}: {
  readonly body: string;
  readonly id: string;
  readonly issueKey: string;
  /** The box goes: the save landed, or the reader closed it. */
  readonly onLeave: () => void;
}): ReactNode {
  const queryClient = useQueryClient();
  const owner = useMemo(() => ({ issueKey, kind: "issue" as const }), [issueKey]);
  const edit = useMemo(() => ({ body, id }), [body, id]);
  const mutationKey = useMemo(() => ["threads-comment-edit", id], [id]);
  return (
    <MentionComposer
      box="threads-comment-edit"
      edit={edit}
      kind="comment"
      mutationKey={mutationKey}
      onClose={onLeave}
      onSent={onLeave}
      owner={owner}
      saveEdit={async (comment, words, carried, bounds) => {
        const saved = await saveCommentEdit(comment, words, carried, bounds);
        await queryClient.invalidateQueries({ queryKey: ["comments", issueKey] });
        return saved;
      }}
    />
  );
}
