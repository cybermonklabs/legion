import type { ReactNode } from "react";

import type { DisplayTree } from "../../api/types";
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
