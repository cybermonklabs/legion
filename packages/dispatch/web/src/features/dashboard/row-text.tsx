import type { AskTextRenderer, TextMarks } from "../ask-body/ask-text";
import { authorText, pageChrome } from "../issue-page/page-structure";
import { RowText } from "../issue-page/RowText";

/** The renderer Dashboard hands the ask body (dispatch://INFRA-5702/spec#b-r-ask-reads): every
 *  text of an ask drawn by `RowText`, from the reading the server made of it, its references
 *  named from the names Dashboard publishes. No keys of a tree reach Dashboard, so the links in
 *  a text keep their place in the tab order. */
export const rowText: AskTextRenderer = (text) => (
  <RowText markdown={text.markdown} source={text.source} tabStops tree={text.tree} />
);

/** The two marks Dashboard hands the ask body and the decision card (`AskBodyProps.marks`,
 *  `DecisionCardProps.marks`): the issue page's own (`page-structure.ts`), so each divides its
 *  own words from its values as Dashboard's own parts do. */
export const textMarks: TextMarks = { authorText, pageChrome };
