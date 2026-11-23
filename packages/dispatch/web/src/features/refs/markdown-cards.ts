import { type ComponentType, createContext } from "react";

import type { AskBlockHost } from "../doc/typed-block-drawing";
import type { IssueListQuery } from "./routes";

/**
 * The component that draws a decision card into a text body's `ask` shell (`useRenderedMarkdown`
 * mounts one per shell, which portals into its header and footer slots). It is a context rather
 * than an import because `useRenderedMarkdown` cannot import `AskBlockCard` itself: the card
 * renders `AskCard` and `AskOptionList`, whose `AskOptionRow` renders a `MarkdownBody`, which is
 * `useRenderedMarkdown` again - an import cycle biome refuses. So the app provides the card
 * (`TextAskBlockCard`), and the body still mounts it synchronously in its layout commit. With no
 * provider a body leaves the shell's slots empty and keeps the ask's question and options as the
 * source list in the frame.
 */
export const TextAskCard = createContext<ComponentType<{ host: AskBlockHost }> | undefined>(
  undefined
);

/**
 * The component that draws a list of issues a text writes on a line of its own
 * (`dispatch://issues?<query>`, `parseIssueListReference`): a rendered body puts one in place of
 * the line (`useRenderedMarkdown`), and the document editor one below it (`ProofDocument`), each
 * under the address prefix `list<N>`, N its place among the text's lists from 1. A context for the
 * reason `TextAskCard` is one: the issues table renders references, whose previews render
 * `MarkdownBody`. With no provider a line keeps its text and draws no list.
 */
export const TextIssueList = createContext<
  ComponentType<{ query: IssueListQuery; stateKey: string }> | undefined
>(undefined);
