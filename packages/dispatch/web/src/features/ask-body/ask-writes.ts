import { api, type DeclineAskInput, type WriteBounds } from "../../api/client";
import type { AnswerAskInput, Ask, AskRead, Comment, CreateCommentInput } from "../../api/types";

/** The writes and the read an ask's form makes, which every host of the ask body hands it
 *  (`inbox/AskCard.tsx`, Dashboard's items): each calls the client when it runs, so the client a
 *  test stands in for is the one called. */
export const answerAsk = (id: string, input: AnswerAskInput, bounds: WriteBounds): Promise<Ask> =>
  api.answerAsk(id, input, bounds);
export const declineAsk = (id: string, input: DeclineAskInput, bounds: WriteBounds): Promise<Ask> =>
  api.declineAsk(id, input, bounds);
export const getAskThread = (id: string): Promise<AskRead> => api.getAsk(id);
export const createReply = (
  issueKey: string,
  input: CreateCommentInput,
  bounds: WriteBounds
): Promise<Comment> => api.createComment(issueKey, input, bounds);
