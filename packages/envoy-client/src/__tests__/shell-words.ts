/** Splits a command line the `dispatch` command writes (`commandLine`, the resend line of a write
 *  that may have landed) back into its words: bare words and POSIX single-quoted ones. */
export function shellWords(line: string): string[] {
  const words: string[] = [];
  let current = "";
  let quoted = false;
  let inWord = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (quoted) {
      if (c === "'") quoted = false;
      else current += c;
    } else if (c === "'") {
      quoted = true;
      inWord = true;
    } else if (c === "\\" && line[i + 1] === "'") {
      current += "'";
      i++;
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
