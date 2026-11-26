import { type ReactNode, useRef } from "react";

import { useCopyFeedback } from "../../hooks/useCopyFeedback";
import {
  pageFailedText,
  pageMarkBg,
  pageMutedText,
  pageQuietText,
  pageStateText,
} from "../../theme/classes";
import { DispatchPicture } from "../refs/DispatchPicture";
import { parseNodeReference } from "../refs/routes";
import { ChildIssueTag } from "./IssueChildren";
import type { NodeFact } from "./issue-tree";
import { authorText, pageChrome } from "./page-structure";
import { ANSWER_LINES, CONTROL_LABELS, nodeFactLabel } from "./page-words";
import { useOutOfTabOrder } from "./RowText";

/**
 * The facts a node's body draws under its text (R15): a check's four attributes and what a
 * recorded result says of itself (`check-facts.ts`).
 *
 * It is a two-column grid and not a wrapping row, which is the cause of most of the sixteen
 * faults measured on the old Verification tab (section 5 of the page's design notes): a label
 * never wraps away from
 * its value, and every value sits in a column of its own that may be zero wide
 * (`min-w-0`) and breaks inside a word where it has nothing else to break on, so nothing
 * reaches past the card at 390 px and no two pieces of text are drawn over each other.
 *
 * A `run` is a command: a monospace block with its own copy control, marked `[data-node-command]`
 * (`e2e/issue-page.ts`). A screenshot is a picture, drawn through the same bytes route every
 * other picture of Dispatch is drawn through (`DispatchPicture`); one whose reference names
 * nothing the browser can draw reads as a link to it, as it does in a document.
 *
 * Every control the facts hold - a command's copy control, a picture's link - is out of the tab
 * order, as everything inside a row of the outline is, unless `tabStops` holds: a view that no
 * tree's keys reach keeps them in it, a link drawn once a picture's read answers included.
 */
export function NodeFacts({
  facts,
  tabStops = false,
}: {
  facts: readonly NodeFact[];
  /** Whether the facts' controls keep their place in the tab order. */
  tabStops?: boolean;
}): ReactNode {
  const list = useRef<HTMLDListElement | null>(null);
  // The list is drawn only once there are facts, so the hook starts watching it then.
  useOutOfTabOrder(list, !tabStops && facts.length > 0);
  if (facts.length === 0) return null;
  return (
    <dl
      className="grid min-w-0 grid-cols-[auto_minmax(0,1fr)] gap-x-2 gap-y-0.5 text-xs"
      ref={list}
    >
      {facts.map((fact, index) => (
        <Fact
          fact={fact}
          // A node may record two screenshots and nothing else tells them apart, so the key is
          // the fact's own place in the list.
          // biome-ignore lint/suspicious/noArrayIndexKey: a fact has no id of its own
          key={`${fact.name}:${index}`}
          tabStops={tabStops}
        />
      ))}
    </dl>
  );
}

function Fact({ fact, tabStops }: { fact: NodeFact; tabStops: boolean }): ReactNode {
  return (
    <>
      <dt className={`min-w-0 break-words ${pageQuietText}`} {...pageChrome}>
        {nodeFactLabel(fact.name)}
      </dt>
      <dd className="m-0 min-w-0">
        <FactValue fact={fact} tabStops={tabStops} />
      </dd>
    </>
  );
}

/** A value drawn as what it is: the page's own word, a command, a picture, or the author's own
 *  text, which breaks inside a word where a long value has nothing else to break on. */
function FactValue({ fact, tabStops }: { fact: NodeFact; tabStops: boolean }): ReactNode {
  if (fact.value === "") {
    // A check that names no negative control says so by keeping the name with no value, which
    // is itself what a reader came to see.
    return (
      <span className={pageQuietText} {...pageChrome}>
        —
      </span>
    );
  }
  if (fact.draw === "command") {
    return <Command command={fact.value} tabStops={tabStops} />;
  }
  // An issue's key, a link to that issue, drawn as the children's section draws a child's key
  // (`ChildIssueTag`): the Plan view's step of another issue's task names that issue so.
  if (fact.draw === "issue") {
    return <ChildIssueTag child={{ key: fact.value }} tabStops={tabStops} />;
  }
  if (fact.draw === "picture") {
    return <Picture reference={fact.value} />;
  }
  return (
    <span
      className={`block min-w-0 break-words ${pageMutedText}`}
      {...(fact.draw === "word" ? pageChrome : authorText)}
    >
      {fact.value}
    </span>
  );
}

/** A command, in a monospace block a reader can read and copy whole: it wraps inside the body
 *  rather than scrolling, since a row has to fit the screen after every move of the keyboard
 *  (R1, R17). The copy control takes no tab stop in the outline - the tree's one tab stop is its
 *  rows - and one in a view with `tabStops`; it carries no `title`, because a reader on a phone
 *  or on the keyboard never sees one (R2). */
function Command({ command, tabStops }: { command: string; tabStops: boolean }): ReactNode {
  const { copy, status } = useCopyFeedback();
  return (
    <span className="flex min-w-0 items-start gap-1">
      <code
        className={`min-w-0 flex-1 rounded px-1 font-mono break-all whitespace-pre-wrap ${pageMarkBg} ${pageMutedText}`}
        data-node-command=""
        {...authorText}
      >
        {command}
      </code>
      <button
        aria-label={CONTROL_LABELS.copy_command}
        className={`shrink-0 underline ${pageQuietText}`}
        onClick={() => copy(command)}
        tabIndex={tabStops ? undefined : -1}
        type="button"
        {...pageChrome}
      >
        {CONTROL_LABELS.copy}
      </button>
      {status === "idle" ? null : (
        <span
          aria-live="polite"
          className={`shrink-0 ${status === "copied" ? pageStateText.passed : pageFailedText}`}
          {...pageChrome}
        >
          {status === "copied" ? ANSWER_LINES.copied : ANSWER_LINES.copy_failed}
        </span>
      )}
    </span>
  );
}

/** A screenshot a result recorded, as the picture it is. */
function Picture({ reference }: { reference: string }): ReactNode {
  const route = parseNodeReference(reference);
  if (route === undefined) {
    return (
      <span className={`block min-w-0 break-all ${pageMutedText}`} {...authorText}>
        {reference}
      </span>
    );
  }
  return (
    <span className="block min-w-0" {...authorText}>
      <DispatchPicture caption="" route={route} variant="block" />
    </span>
  );
}
