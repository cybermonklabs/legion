// Which lines of a live session file hold a card. The pane appends to that file while a case
// reads it, so the selection goes through the shared withholding rule rather than a bare split:
// a read landing between a card line's bytes and its newline would otherwise hand `JSON.parse`
// half an object, or count a card the host has not finished writing (dispatch://INFRA-2188).
import { type Writer, writtenLines } from "@legion/test-fixtures/transcript-lines";

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
