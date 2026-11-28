import type { ReactNode } from "react";

import { pageQuietText } from "../../theme/classes";
import { pageChrome } from "./page-structure";
import { VIEW_LOADING_LINES } from "./page-words";

/** The line a view draws while a read it needs is out (`ViewLoadingLine`). */
export const VIEW_LOADING = "[data-view-loading]";

/** `names` as one phrase, in the order given: "a", "a and b", "a, b and c". */
function listed(names: readonly string[]): string {
  let phrase = names[0] ?? "";
  for (let at = 1; at < names.length; at += 1) {
    phrase += at === names.length - 1 ? ` ${VIEW_LOADING_LINES.and} ` : ", ";
    phrase += names[at];
  }
  return phrase;
}

/**
 * What a view draws in its place while a read it needs has no answer yet (part N's R19): one
 * line in the page's quiet text that names each read still out, in the view's order, "Loading
 * the plan and the spec…", so the view is never blank while it waits. It is a status the screen
 * reader says, drawn over the view's tree and never in it, as a failed read's line is
 * (`FailedReadLine`): a status inside a tree would break the tree's own structure of rows. A
 * view draws it only while no read stands failed, since the failed line with its Retry is then
 * what the reader acts on, and never for a read that holds an answer and is read again.
 *
 * `names` are the words each view holds for its reads (`VIEW_LOADING_LINES`), each read still
 * out once and none that has answered.
 */
export function ViewLoadingLine({ names }: { names: readonly string[] }): ReactNode {
  return (
    <p
      className={`py-1 text-sm leading-5 ${pageQuietText}`}
      data-view-loading=""
      role="status"
      {...pageChrome}
    >
      {`${VIEW_LOADING_LINES.loading} ${listed(names)}…`}
    </p>
  );
}
