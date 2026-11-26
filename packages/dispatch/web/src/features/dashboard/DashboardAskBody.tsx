import type { ReactNode } from "react";

import type { AskWithTrees, IssueAskRow } from "../../api/types";
import { AskBody } from "../ask-body/AskBody";
import { answerAsk, createReply, declineAsk, getAskThread } from "../ask-body/ask-writes";
import { useAskAnswerForm } from "../inbox/useAskAnswerForm";
import { rowText } from "./row-text";

/** An ask Dashboard answers in place, by the ask body: a question or an approval request of the
 *  list, and the ask of a thread item. The ask is the read the item draws from - the issue's open
 *  asks, or the thread's own read, under the key the form reads a thread by - so the form reads
 *  no thread of its own. Agents are named from the registry the cache holds, which starts no
 *  read, and the urgency word stands beside the kind, where no card's notch says it. */
export function DashboardAskBody({ ask }: { ask: IssueAskRow | AskWithTrees }): ReactNode {
  const form = useAskAnswerForm({
    answer: answerAsk,
    ask,
    createReply,
    getAskThread,
    readThread: false,
  });
  return (
    <AskBody
      agentNames="cache"
      ask={ask}
      decline={declineAsk}
      documentLink
      form={form}
      frame="card"
      renderText={rowText}
      urgencyWord
    />
  );
}
