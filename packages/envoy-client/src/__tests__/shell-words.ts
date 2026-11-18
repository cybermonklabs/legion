/** Splits a command line the `dispatch` command writes (`commandLine`, the resend line of a write
 *  that may have landed) back into its words: bare words, and words of single- and double-quoted
 *  segments, as bash reads them (the CLI writes no `$`, backtick or `\` inside double quotes). */
export function shellWords(line: string): string[] {
  const words: string[] = [];
  let current = "";
  let quote: "'" | '"' | undefined;
  let inWord = false;
  for (const c of line) {
    if (quote !== undefined) {
      if (c === quote) quote = undefined;
      else current += c;
    } else if (c === "'" || c === '"') {
      quote = c;
      inWord = true;
    } else if (c === " ") {
      if (inWord) words.push(current);
      current = "";
      inWord = false;
    } else {
      current += c;
      inWord = true;
    }
  }
  if (inWord) words.push(current);
  return words;
}
