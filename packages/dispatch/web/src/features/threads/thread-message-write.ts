/**
 * The write kind of a message a box of the new issue page's Threads sends
 * (`issue-page/threads/MessageBox.tsx`, dispatch://INFRA-5704): a box that
 * writes a message is that part's own, since `MentionField` has no setting that turns `@` off
 * (R11) and `MentionComposer` writes no root message of the issue, so its sends go through the
 * write store under a kind of their own (`defineWriteKind`), as every content write does.
 *
 * It stands here, beside the shared thread code and outside `issue-page/threads/`, because the
 * store takes up the writes a reload interrupted once, as the person signs in (`Writes.resume`),
 * and drops each whose kind it does not know then: so the kind loads with the shell
 * (`writes/kinds.ts`), which no module of `issue-page/threads/` may join (spec R18).
 */
import { myRepliesKey } from "../../api/queries";
import { type Shape, shape } from "../writes/tab-storage";
import { defineWriteKind } from "../writes/write-store";

/** What the store keeps of one message a box sent: the issue it is on, and the reader's words. */
export interface ThreadMessage {
  readonly issueKey: string;
  readonly body: string;
}

const keptMessage = shape.record({
  body: shape.string,
  issueKey: shape.string,
}) satisfies Shape<ThreadMessage>;

/**
 * A message of a box of Threads. Once it lands, the issue's message reads are read again, which
 * Threads' message pages stand under (`["issue-messages", key, "threads", …]`), so
 * the record draws the new entry whether or not its box is still drawn, and so are the reader's
 * unread threads, which a write of theirs moves. The words it carried are the reader's own text,
 * which the shell's line offers to copy where a conflict refuses it.
 */
export const threadMessageWrite = defineWriteKind<ThreadMessage>({
  draft: (write) => write.detail.body,
  kept: keptMessage,
  landed: (client, write) => {
    void client.invalidateQueries({ queryKey: ["issue-messages", write.detail.issueKey] });
    void client.invalidateQueries({ queryKey: myRepliesKey });
  },
  name: "threads-message",
});
