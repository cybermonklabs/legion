/**
 * The rows of `DISPATCH_TEXT_REFERENCES` that write a reference where code begins or ends: the
 * three contexts (a fenced code block, an indented code block, a code span), the containers each
 * is read inside, and the edges of each rule. Two deciders read exactly this and must agree
 * character for character - `packages/envoy/internal/dispatch/text/code.go` and
 * `packages/dispatch/web/src/features/refs/code-ranges.ts` - and the table is the only place
 * both are held to one body, so an edge belongs here rather than in either language's own test,
 * where it would be run against one decider alone.
 */

import type { DispatchTextReferenceRow } from "./dispatch-text-references";

/** The rows the table spreads in after `WRITTEN`, each carrying its own `refs`, `links` and
 *  `inCode`: `inCode` is what the two deciders read, and `links` what the markdown engine draws,
 *  which is nothing inside a code span or a code block. */
export const CODE_CONTEXT: readonly DispatchTextReferenceRow[] = [
  // A party reference a code context holds is read and indexed as every other is, linked by no
  // renderer, and delivered to nobody: the three contexts are an indented code block, a fenced
  // one and a code span, and a body may hold both a party in code and a party outside it, which
  // only the second of them names. The code spans the parties alone sit in are the twins
  // `IN_CODE` builds; these are the bodies no twin writes.
  {
    body: "Pasted from the handbook:\n\n    dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551\n",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
    links: [],
    inCode: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "Ask [the reviewer](dispatch://role/reviewer) to run:\n\n```sh\ndispatch send-to-agent dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551\n```\n",
    refs: ["dispatch://role/reviewer", "dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
    links: ["dispatch://role/reviewer"],
    inCode: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "Run `dispatch message --to dispatch://role/reviewer` before the deploy.",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  // Where a code context begins and ends is read through the containers a body holds, so a
  // party written under a bullet or inside a quote is read as a reader of the record sees it:
  // the indentation an ordinary nested item carries is that item's text, and a fence or an
  // indented block written inside a quote or an item is code there.
  {
    body: "- step one\n    - ask dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  {
    body: "1. step one\n\n    more about dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  // A marker followed by three spaces indents its item's own content four columns, which is
  // where that item's text starts and not where code inside it opens; five spaces leave the
  // item one of them and the rest to an indented block inside it.
  {
    body: "-   See this:\n\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  {
    body: "-     dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  // A line carrying none of the containers it stands in continues the paragraph above it and
  // holds them open, so the line after the blank one is still the item's own text.
  {
    body: "1.  step one\nlazy line\n\n    more about dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  {
    body: "> ```\n> dispatch://role/reviewer\n> ```\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  {
    body: "> > ```\n> > dispatch://role/reviewer\n> > ```\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  {
    body: "> See this:\n>\n>     dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  {
    body: "- Run this:\n\n  ```\n  dispatch://role/reviewer\n  ```\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  {
    body: "- Ask:\n\n  > ```\n  > dispatch://role/reviewer\n  > ```\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  // A fence is closed by the line that leaves the containers it opened in, so the paragraph
  // after the quote is prose and the party it names is delivered to.
  {
    body: "> ```\n> text\n\ndispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  // A thematic break opens no list item and no paragraph, so the indented line under it opens a
  // code block as CommonMark opens one there.
  {
    body: "***\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  // The edges of a fence: an info string, tildes, a closing run longer than the opener, a run
  // too short to close one, a fence nothing closes, and the three columns of indentation one
  // may carry.
  {
    body: "```\ndispatch://CORE-1\n```",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "```sh\ndispatch://CORE-1\n```",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "~~~\ndispatch://CORE-1\n~~~",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "```\ndispatch://CORE-1\n`````",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "````\n``\ndispatch://CORE-1\n````",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "```\ndispatch://CORE-1",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "   ```\ndispatch://CORE-1\n   ```",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  { body: "```\nx\n```\ndispatch://CORE-1", refs: ["dispatch://CORE-1"] },
  // A backtick in the info string makes a backtick fence no fence at all, that backtick being a
  // code span's; a tilde fence takes the same info string as code.
  {
    body: "~~~`sh\ndispatch://CORE-1\n~~~",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  { body: "```a`b\ndispatch://CORE-1\n```", refs: ["dispatch://CORE-1"] },
  // An indented block opens where no paragraph is open, by four columns or a tab, and holds the
  // blank lines inside it; four columns under a paragraph are that paragraph's own text.
  {
    body: "See:\n\n    dispatch://CORE-1\n",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "    dispatch://CORE-1\n",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "See:\n\n\tdispatch://CORE-1\n",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "See:\n\n    x\n\n    dispatch://CORE-1\n",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  { body: "See:\n    dispatch://CORE-1\n", refs: ["dispatch://CORE-1"] },
  { body: "See:\n\n    x\n\ndispatch://CORE-1", refs: ["dispatch://CORE-1"] },
  // A code span's run is closed by the next run of exactly its length, and a run nothing closes
  // is literal text the next character is read on from.
  {
    body: "Run `dispatch://CORE-1` now.",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "Run ``dispatch://CORE-1`` now.",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  {
    body: "Run `` dispatch://CORE-1 ``` ``",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  { body: "Run `dispatch://CORE-1 now.", refs: ["dispatch://CORE-1"] },
  { body: "`a` dispatch://CORE-1 `b`", refs: ["dispatch://CORE-1"] },
  {
    body: "Run `one\ndispatch://CORE-1` now.",
    refs: ["dispatch://CORE-1"],
    links: [],
    inCode: ["dispatch://CORE-1"],
  },
  // Spans are read in the text the blocks leave, so a backtick inside a fenced block opens
  // nothing.
  { body: "```\n`\n```\ndispatch://CORE-1", refs: ["dispatch://CORE-1"] },
  { body: "See dispatch://CORE-1 now.", refs: ["dispatch://CORE-1"] },
  // A span's search for its closing run ends where the paragraph ends, at a blank line: one
  // unmatched backtick mutes nothing after it, and the party under it is delivered to, as every
  // renderer draws it (INFRA-1702).
  {
    body: "Rename the `foo field.\n\n[Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) please check `bar` too.",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  // A paragraph ends where CommonMark ends one without a blank line too, and the rows under
  // this comment are one body per ender: the next list item's marker, a line blank past the
  // containers it carries (a bare `>` inside a quote), a blank line, an ATX heading, a
  // thematic break, and a setext heading's underline, written as `--` and as `===`. The span
  // search ends at each of them as well, so one backtick nothing closes in an item, in a
  // quote, above a heading or above an underline mutes no party written under it
  // (INFRA-1702 R2).
  {
    body: "- Rename `foo field\n- ask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "> Rename `foo\n>\n> [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "Rename `foo field\n\nask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "Rename `foo field\n# Next up\nask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "Rename `foo field\n***\nask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "Rename `foo field\n--\nask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "Rename `foo field\n===\nask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  // An ATX heading is no paragraph's line and opens none, so the indented line under it opens a
  // code block, as CommonMark opens one there.
  {
    body: "# Where to ask\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  // A setext heading's underline ends the paragraph above it, so the indented line under the
  // underline opens a code block. goldmark, the browser editor's parser and the server's own
  // pmdoc each read this body that way (measured 2026-11-11, INFRA-1702): a party written there
  // is named to no reader of the record, so it is delivered to nobody.
  {
    body: "Where to ask\n============\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  // A footnote definition is a container whose content stands four columns in, so the indented
  // line under it is the note's own text and not a code block. goldmark, the browser editor's
  // parser and pmdoc each read this body that way (measured 2026-11-11, INFRA-1702): every
  // reader of the record sees the party named there, so it is delivered to.
  {
    body: "[^who]: who to ask\n\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  // Those four columns are counted from where the definition's own container's content begins,
  // not from its marker, so a definition the writer indented one to three spaces holds its
  // note's lines as text too. goldmark with its footnote extension and remark with
  // gfm-footnote both read it that way (measured 2026-11-12, INFRA-1702 round 5).
  {
    // Four spaces, not six: four is where this fix moves the note's content column to, so the
    // line is code under the old reading (a definition two spaces in put its content at column
    // six) and prose under this one. A line six spaces in reads as prose either way and would
    // hold nothing.
    body: "  [^who]: who to ask\n\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  // Item 2 of a numbered list ends item 1's paragraph, as a bullet does, so one backtick
  // nothing closes in item 1 mutes no party written in item 2. CommonMark holds an ordered
  // marker that does not count from 1 back only where the paragraph is the open tip, which
  // item 2 is not (INFRA-1702 round 3).
  {
    body: "1. Rename `foo field\n2. ask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  {
    body: "1) Rename `foo field\n2) ask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  // An empty item's marker is the same rule: it ends the paragraph of the item above it. The
  // party stands on a line of that empty item's own, indented to its content column, so the
  // marker is the only thing that can end the paragraph above it: a third line opening a
  // bullet of its own would end that paragraph whatever this rule said.
  {
    body: "- Rename `foo\n-\n  ask [Planner](dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551) about `bar`",
    refs: ["dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"],
  },
  // A `-` run is an underline as much as an `=` run, whatever its length, so the indented line
  // under it opens a code block where the paragraph it closes stood.
  {
    body: "Where to ask\n--\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
    links: [],
    inCode: ["dispatch://role/reviewer"],
  },
  // A line that left the containers its paragraph stands in is a lazy continuation of that
  // paragraph and never an underline (CommonMark example 93), so the quote's paragraph stays
  // open and the indented line under it is that paragraph's own text.
  {
    body: "> Where to ask\n===\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  {
    body: "- Where to ask\n===\n    dispatch://role/reviewer\n",
    refs: ["dispatch://role/reviewer"],
  },
  // A backslash takes the backtick after it as a literal character, which opens no span, so the
  // party stands in prose and the span is the one the author wrote.
  {
    body: "Type \\` for a backtick, ask dispatch://role/reviewer, then run `dispatch status`.",
    refs: ["dispatch://role/reviewer"],
  },
  // A run that closes a span is read as written, escaped or not, which is where CommonMark puts
  // the escape too: the span here is `` `grep \` ``, and what follows it is prose.
  {
    body: "Run `grep \\` here` then ask dispatch://role/reviewer.",
    refs: ["dispatch://role/reviewer"],
  },
];
