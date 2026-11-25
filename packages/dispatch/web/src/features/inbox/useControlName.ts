import type { ReactNode } from "react";

import type { DisplayTree } from "../../api/types";
import { usePlainHeadline } from "../refs/plain-text";
import { textSourceOf } from "../refs/text-references";
import { type AskOrdinal, askIssueKey, controlName, type NamedAsk } from "./ask-name";

/**
 * What an Inbox row's controls are called (`controlName`), with the question read by the one
 * reader: a name is an `aria-label`, which holds no markup, so it carries the question's
 * plain-text projection - its words with the formatting gone and each reference read as its
 * target's name. The ask is the record the question is, so a `#id` in it reads as the question
 * it names, as it does on the card. Why it is drawn so is the reading list's
 * `inbox-control-name` (`DISPATCH_READING_SURFACES`).
 *
 * `reads` draws nothing: it reads the names of the references the question holds, and the caller
 * mounts it beside the controls it names.
 */
export function useControlName(
  ask: NamedAsk & { readonly question_tree?: DisplayTree },
  ordinal: AskOrdinal | undefined
): { name: string; reads: ReactNode } {
  const { reads, text } = usePlainHeadline(ask.question_tree, ask.question, {
    source: textSourceOf("ask", ask.id, askIssueKey(ask)),
    surface: "inbox-control-name",
  });
  // Until the reader's engine has answered, the name carries the question as written, which the
  // reading list's `inbox-control-name` says is a load state.
  return { name: controlName(ask, ordinal, text?.text ?? ask.question), reads };
}
