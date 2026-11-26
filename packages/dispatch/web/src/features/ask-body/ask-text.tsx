import type { ReactNode } from "react";

import type { DisplayTree } from "../../api/types";
import type { PageWords } from "../../components/tooltips";
import { MarkdownBody } from "../refs/MarkdownBody";
import type { TextSource } from "../refs/text-references";

/** The rows of the reading list (`DISPATCH_READING_SURFACES`) the ask body's texts are drawn on:
 *  the question on its card, an option's label and description, a question's earlier wording, an
 *  earlier answer and an option it chose, and the settled record's own texts. */
export type AskTextSurface =
  | "answered-ask"
  | "ask-answer-history"
  | "ask-card-question"
  | "ask-edit-history"
  | "ask-option-text"
  | "earlier-chosen-option"
  | "resolved-ask";

/**
 * A text of an ask as the ask body hands it to its caller's renderer (R17): the question, an
 * option's label and its description, the answer, an earlier answer, the reason a question was
 * closed, and a question's earlier wording. Each is its markdown, the reading the server made of
 * it where it stored one, the record where a `#id` in it resolves where the text links one
 * (`textSourceOf`), the row of the reading list it is drawn on, and whether it stands inline in
 * a line of the body's or as a block of its own. Each field is one the body hands for that text,
 * and only then.
 */
export interface AskText {
  readonly markdown: string;
  readonly source?: TextSource;
  /** The row of the reading list this text is drawn on (`MarkdownBody`'s `surface`). */
  readonly surface: AskTextSurface;
  readonly tree?: DisplayTree;
  /** `inline` for a text inside a line of the body's own (an option's label and description,
   *  the closed question's reason); a block where unset. */
  readonly variant?: "block" | "inline";
}

/** Draws a text of an ask. The ask body, its record and its option rows draw every text of the
 *  ask only through the renderer its caller hands them (R17). */
export type AskTextRenderer = (text: AskText) => ReactNode;

/** The renderer of today's surfaces and the Inbox: `MarkdownBody`, handed each text's fields as
 *  they are, so it is drawn with the props `main` draws that text with. Each row is a mount of
 *  its own, which is how the reading list's unit test ties it to this file
 *  (`features/refs/surface-lists.test.ts`). */
export const markdownText: AskTextRenderer = (text) => {
  switch (text.surface) {
    case "answered-ask":
      return <MarkdownBody {...text} surface="answered-ask" />;
    case "ask-answer-history":
      return <MarkdownBody {...text} surface="ask-answer-history" />;
    case "ask-card-question":
      return <MarkdownBody {...text} surface="ask-card-question" />;
    case "ask-edit-history":
      return <MarkdownBody {...text} surface="ask-edit-history" />;
    case "ask-option-text":
      return <MarkdownBody {...text} surface="ask-option-text" />;
    case "earlier-chosen-option":
      return <MarkdownBody {...text} surface="earlier-chosen-option" />;
    case "resolved-ask":
      return <MarkdownBody {...text} surface="resolved-ask" />;
  }
};

/** The two marks a host that divides its own words from what a person or an agent wrote hands
 *  the ask body (`AskBodyProps.marks`): the mark of the body's own words and the mark of a
 *  value, as `components/tooltips.tsx` names them. Dashboard hands the new issue page's; every
 *  other host hands none, and the body then draws no mark of its own. */
export type TextMarks = Pick<PageWords, "authorText" | "pageChrome">;

/** Whose colours the ask body's children and its settled record draw (`AskBodyProps.palette`):
 *  `main`'s, as every surface of today's pages and the Inbox draws them, or the new issue page's
 *  (`theme/classes.ts`' page tokens, dispatch://INFRA-3940 R13). Dashboard on the new page hands
 *  `page`; every other host hands none. */
export type AskPalette = "main" | "page";

/** `value` inside the mark of a value, where the host hands the marks, and as it is where it
 *  hands none, so a host that hands none draws the same elements it always drew. */
export function authored(marks: TextMarks | undefined, value: ReactNode): ReactNode {
  return marks === undefined ? value : <span {...marks.authorText}>{value}</span>;
}

/** Words of the body's own inside the mark of the body's words, where the host hands the marks,
 *  and as they are where it hands none: for words no element of the body's holds alone, so the
 *  mark stands on them and not on a box that holds other words beside them. */
export function ownWords(marks: TextMarks | undefined, words: ReactNode): ReactNode {
  return marks === undefined ? words : <span {...marks.pageChrome}>{words}</span>;
}

/** A shared element that takes no mark of its own - the dot between two parts of a line, the
 *  disclosure toggle - inside the mark of the body's words, where the host hands the marks, and
 *  as it is where it hands none. The mark's element draws no box (`contents`), so `part` is laid
 *  out as it is with no mark. */
export function ownPart(marks: TextMarks | undefined, part: ReactNode): ReactNode {
  return marks === undefined ? (
    part
  ) : (
    <span className="contents" {...marks.pageChrome}>
      {part}
    </span>
  );
}
