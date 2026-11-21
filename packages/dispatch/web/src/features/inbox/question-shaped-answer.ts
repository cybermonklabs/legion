import { ownWords } from "../threads/quote-reply";

const questionStart = /^(?:who|what|why|how|when|where|which|can|could|should|is|are|do|does)\b/iu;

/** Whether what the reader wrote reads as a question: their own lines, never a quote's (`> `
 *  lines, which Quote reply puts in the box and which are the asker's words). */
export function isQuestionShapedAnswer(value: string): boolean {
  const answer = ownWords(value).trim();
  return answer.endsWith("?") || questionStart.test(answer);
}
