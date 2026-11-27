// Which lines of a live session file hold a card. The pane appends to that file while a case
// reads it, so the selection goes through the shared withholding rule rather than a bare split:
// a read landing between a card line's bytes and its newline would otherwise hand `JSON.parse`
// half an object, or count a card the host has not finished writing (dispatch://INFRA-2188).
import { readFile } from "node:fs/promises";
import { endsWhole, type Writer, writtenLines } from "@legion/test-fixtures/transcript-lines";
import { until } from "@legion/test-fixtures/wait";

/**
 * The `envoy-message` custom messages of a session file's `text`, as raw JSON lines, in the order
 * the file holds them. `writer` is "alive" while the pane may still be appending, where an
 * unfinished last line is withheld, and "gone" once it cannot be, where every line is selected so
 * that a fragment nothing will finish is parsed and throws rather than being passed over.
 */
export function cardLines(text: string, writer: Writer): string[] {
  return writtenLines(text, writer).filter(
    (line) => line.includes('"custom_message"') && line.includes('"envoy-message"')
  );
}

/**
 * The card lines of the session file at `file`, read once that file ends on a finished line.
 *
 * Every case that reads cards asserts an exact count of them, and the rule above withholds a
 * line the host has not finished: without this wait such a read would count one card short
 * rather than throw on half an object, which is the same window with a different red
 * (dispatch://INFRA-2188). The wait lives in the reader rather than at each case, so a case
 * cannot forget it, and the lines come from the very text that was found whole, so there is no
 * gap between the two. One line is one write, so a wait that reaches its row's deadline is a file
 * nothing is finishing, and its failure names the file. A writer that is gone is never waited for,
 * since nothing will finish its last line.
 */
export async function readCardLines(file: string, writer: Writer): Promise<string[]> {
  const text = await until(`the session file ${file} to end on a finished line`, async () => {
    const read = await readFile(file, "utf8");
    return writer === "gone" || endsWhole(read) ? { read } : undefined;
  });
  return cardLines(text.read, writer);
}
