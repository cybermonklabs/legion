import type { ReactNode } from "react";

import {
  focusVisibleRing,
  pageHairBorder,
  pageInkBorder,
  pageInkFill,
  pageMutedText,
  pagePaperBg,
} from "../../../theme/classes";
import { pageChrome } from "../page-structure";
import { THREADS_WORDS } from "../page-words";

/** One of the four choices over the list (dispatch://INFRA-5704/spec#b-r-view, R2). */
export type ThreadChoice = keyof typeof THREADS_WORDS.choices;

/** The order the choices are drawn in, left to right (R2). */
const CHOICE_ORDER: readonly ThreadChoice[] = ["mine", "waiting", "all", "agents"];

/** The choice that is on when Threads opens (R2). */
export const FIRST_CHOICE: ThreadChoice = "all";

/**
 * The four choices over the list (R2): Mine, Waiting on me, All and Agents, in that order, each a
 * toggle (`aria-pressed`) named by its word alone, one on at a time. They draw no count of their
 * own (the spec's choice "The choices draw no count of their own").
 *
 * One group, the four toggles, which wraps as a whole line of toggles where the list is narrow.
 */
export function ThreadChoices({
  choice,
  onChoose,
}: {
  /** The choice that is on. */
  readonly choice: ThreadChoice;
  /** Turns `next` on. */
  readonly onChoose: (next: ThreadChoice) => void;
}): ReactNode {
  return (
    <div className="flex min-w-0 flex-wrap gap-1.5" data-thread-choices="">
      {CHOICE_ORDER.map((one) => (
        <button
          aria-pressed={one === choice}
          className={`rounded-full border px-3 py-0.5 text-sm leading-5 font-semibold ${focusVisibleRing} ${
            one === choice
              ? `${pageInkFill} ${pageInkBorder}`
              : `${pageHairBorder} ${pagePaperBg} ${pageMutedText}`
          }`}
          key={one}
          onClick={() => onChoose(one)}
          type="button"
          {...pageChrome}
        >
          {THREADS_WORDS.choices[one]}
        </button>
      ))}
    </div>
  );
}
