// Reading a file a live process is appending to, line by line. A test that polls the transcript
// of an Oh My Pi pane it drives can read between a line's bytes and its newline; the readers here
// say which lines a read may parse. It lives in this package because the panes are driven from
// both packages/pi-legion and packages/pi-envoy, and neither one's tests import the other's.

/** Whether something may still be appending to the text being read. */
export type Writer = "alive" | "gone";

/**
 * Every line the writer has finished: the lines before the last newline, empties dropped. A
 * trailing fragment with no newline yet is a line a write has not finished, so it is left for the
 * next read and a poll answers "not yet" rather than throwing on half an object
 * (dispatch://INFRA-2188). A line that is complete and malformed is returned, so parsing it still
 * throws: that is a defect in whatever wrote it. The RPC frame reader of the pane harness buffers
 * to a newline for the same reason (`@legion/pi-shared/test/omp-harness`).
 */
export function completeLines(text: string): string[] {
  const end = text.lastIndexOf("\n");
  return (end === -1 ? "" : text.slice(0, end)).split("\n").filter(Boolean);
}

/**
 * Whether `text` ends on a finished line, so that `completeLines` withholds nothing of it. This
 * is the one statement of the rule: a reader that wants to know whether a file is whole asks
 * this rather than testing for the newline itself.
 */
export function endsWhole(text: string): boolean {
  return text === "" || text.endsWith("\n");
}

/**
 * The lines of `text` a reader may parse now. While the writer is alive an unfinished last line is
 * withheld; once it is gone every line is parsed, so a record written last is never withheld for
 * ever and a fragment that will never be finished throws where it is written rather than timing
 * out a poll.
 */
export function writtenLines(text: string, writer: Writer): string[] {
  return writer === "alive" ? completeLines(text) : text.split("\n").filter(Boolean);
}
