import type { ReactNode } from "react";

import type { DisplayTree } from "../../api/types";
import { MarkdownBody } from "../refs/MarkdownBody";
import type { TextSource } from "../refs/text-references";

/**
 * A text of an ask as the ask body hands it to its caller's renderer (R17): the question, an
 * option's label and its description, the answer, an earlier answer, the reason a question was
 * closed, and a question's earlier wording. Each is its markdown, the reading the server made of
 * it where it stored one, the record where a `#id` in it resolves where the text links one
 * (`textSourceOf`), and whether it stands inline in a line of the body's or as a block of its own.
 * Each field is one the body hands for that text, and only then.
 */
export interface AskText {
  readonly markdown: string;
  readonly source?: TextSource;
  readonly tree?: DisplayTree;
  /** `inline` for a text inside a line of the body's own (an option's label and description,
   *  the closed question's reason); a block where unset. */
  readonly variant?: "block" | "inline";
}

/** Draws a text of an ask. The ask body, its record and its option rows draw every text of the
 *  ask only through the renderer its caller hands them (R17). */
export type AskTextRenderer = (text: AskText) => ReactNode;

/** The renderer of today's surfaces and the Inbox: `MarkdownBody`, handed each text's fields as
 *  they are, so it is drawn with the props `main` draws that text with. */
export const markdownText: AskTextRenderer = (text) => <MarkdownBody {...text} />;
