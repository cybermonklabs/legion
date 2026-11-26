import type { ReactNode } from "react";

import type { AskWithTrees, IssueAskRow } from "../../api/types";
import { AskBody } from "../ask-body/AskBody";
import { answerAsk, createReply, declineAsk, getAskThread } from "../ask-body/ask-writes";
import { useAskAnswerForm } from "../inbox/useAskAnswerForm";
import { authorText } from "../issue-page/page-structure";
import { rowText, textMarks } from "./row-text";

/** An ask Dashboard answers in place, by the ask body: a question or an approval request of the
 *  list, and the ask of a thread item. The ask is the read the item draws from - the issue's open
 *  asks, or the thread's own read, under the key the form reads a thread by - so the form reads
 *  no thread of its own. Agents are named from the registry the cache holds, which starts no
 *  read, the urgency word stands beside the kind, where no card's notch says it, and the body
 *  marks its own words apart from its values, as Dashboard's own parts do (`textMarks`). */
export function DashboardAskBody({ ask }: { ask: IssueAskRow | AskWithTrees }): ReactNode {
  const form = useAskAnswerForm({
    answer: answerAsk,
    ask,
    createReply,
    getAskThread,
    readThread: false,
  });
  return (
    // The settled record the body draws in its place once the ask is answered
    // (`AskCompletionCard`), and the lines under it, carry no mark of their own: a hosted
    // surface's words, marked as a value as the margin's own cards are (`IssueMargin.tsx`). The
    // open body's root marks its own words inside it, the nearer mark. A block and not
    // `contents`, so a host that spaces its children by their margins (`AskThreadItem`'s
    // `space-y-2`) still spaces the body.
    <div {...authorText}>
      <AskBody
        agentNames="cache"
        ask={ask}
        decline={declineAsk}
        documentLink
        form={form}
        frame="card"
        marks={textMarks}
        renderText={rowText}
        urgencyWord
      />
    </div>
  );
}
