import type { ReactNode } from "react";

import { pageMutedText } from "../../../theme/classes";
import { pageChrome } from "../page-structure";
import { EMPTY_STATE_LINES, PRESET_NAMES } from "../page-words";

/**
 * The Threads view (dispatch://INFRA-5704/spec#b-r-view), which the stage draws in the outline's
 * place (`slots/view-threads.tsx`). It is one region named by its control, so a reader finds it as
 * a landmark, and it holds two columns: the list (`data-thread-list`), and the main column
 * (`data-thread-main`), which draws the opened record, New thread, or one line that says how to
 * open or start a thread (R2).
 *
 * It draws no outline, no children's section, none of the issue's three sentences and no version
 * control (R1): only the two columns, in the outline's measure.
 */
export function ThreadsView(): ReactNode {
  return (
    <section
      aria-label={PRESET_NAMES.threads}
      className="mx-auto flex w-full max-w-[1000px] min-w-0 gap-6 pb-6"
    >
      <div className="min-w-0" data-thread-list="" />
      <div className="min-w-0 flex-1" data-thread-main="">
        <p className={`text-sm ${pageMutedText}`} {...pageChrome}>
          {EMPTY_STATE_LINES.threads_none_open}
        </p>
      </div>
    </section>
  );
}
