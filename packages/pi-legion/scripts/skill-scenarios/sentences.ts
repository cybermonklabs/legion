// sentences.ts: the sentences of a text, as rule-search.ts and gate-search.ts cut it. A sentence
// is a heading, a list item, a table row, a line of a fenced block, or a paragraph, each split
// after `.`, `?` or `!` where the next word starts with a capital, a backtick or a quote.

export interface Sentence {
  readonly file: string;
  /** The line it starts on. */
  readonly line: number;
  /** The line it ends on. */
  readonly last: number;
  /** Its unit's place in the file: sentences of one heading, list item, table row, fenced line or
   * paragraph share it. */
  readonly unit: number;
  readonly text: string;
  /** A line of a fenced block. */
  readonly fenced: boolean;
}

/** Each sentence of `text`, with the lines it starts and ends on and its unit. */
export function sentences(file: string, text: string): Sentence[] {
  // A unit is a heading, a list item, a table row, a line of a fenced block or a paragraph: its
  // lines, each trimmed and with its runs of white space as one space, joined by one space, and
  // the offset in the joined text where each line starts.
  type Unit = { starts: { line: number; at: number }[]; text: string; fenced: boolean };
  const units: Unit[] = [];
  let fenced = false;
  let open: Unit | undefined;
  const close = () => {
    if (open !== undefined) units.push(open);
    open = undefined;
  };
  text.split("\n").forEach((raw, index) => {
    const line = index + 1;
    const words = raw.trim().replace(/\s+/g, " ");
    if (words.startsWith("```")) {
      close();
      fenced = !fenced;
      return;
    }
    if (words === "") {
      close();
      return;
    }
    if (fenced || open === undefined || /^(#{1,6} |[-*+] |\d+\. |\|)/.test(words)) {
      close();
      open = { starts: [{ line, at: 0 }], text: words, fenced };
      if (fenced || words.startsWith("#") || words.startsWith("|")) close();
      return;
    }
    open.starts.push({ line, at: open.text.length + 1 });
    open.text += ` ${words}`;
  });
  close();
  const found: Sentence[] = [];
  units.forEach((unit, index) => {
    const lineAt = (offset: number) =>
      unit.starts.filter((start) => start.at <= offset).at(-1)?.line ?? unit.starts[0]?.line ?? 0;
    let from = 0;
    const ends = [...unit.text.matchAll(/(?<=[.?!])\s+(?=[A-Z`"*(])/g)].map((m) => [
      m.index,
      m.index + m[0].length,
    ]);
    for (const [end, next] of [...ends, [unit.text.length, unit.text.length]]) {
      const at = from;
      found.push({
        file,
        line: lineAt(at),
        last: lineAt(Math.max(at, (end as number) - 1)),
        unit: index,
        text: unit.text.slice(from, end),
        fenced: unit.fenced,
      });
      from = next as number;
    }
  });
  return found;
}
