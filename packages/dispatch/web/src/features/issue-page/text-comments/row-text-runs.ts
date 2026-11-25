/** One run of a node's own text: the text node that holds it, and where it starts and ends in
 *  `TextRuns.text`. */
export interface TextRun {
  readonly end: number;
  readonly node: Text;
  readonly start: number;
}

/** The characters a row's words element draws from the node's own text, joined as the server's
 *  `FindInBlock` joins a block's runs (one space between two text blocks, an atom as one space),
 *  with each run's place, and the way from an offset to a DOM point and back. */
export interface TextRuns {
  /** The offset of the DOM point in `text`, or null where the point is in no run. */
  readonly offsetOf: (node: Node, offset: number) => number | null;
  /** The DOM point of `offset` in `text`, or null past its end. */
  readonly pointOf: (offset: number) => { readonly node: Text; readonly offset: number } | null;
  readonly runs: readonly TextRun[];
  readonly text: string;
}

/**
 * The words element of a row: the one element the row's site draws its node's own words in
 * (`[data-node-words]`, which `CutWords` and the site draw around `RowText`), or null where the
 * row has none or more than one. Built by t5; it answers null until then.
 */
export function rowTextOf(_row: Element): HTMLElement | null {
  return null;
}

/**
 * The runs of `element`'s text that come from the node's own text: a run that a chip, a
 * reference's drawn name, an edited mark, an ask card or a deleted line draws is no run. Built by
 * t5; it answers no text until then.
 */
export function textRuns(_element: HTMLElement): TextRuns {
  return { offsetOf: () => null, pointOf: () => null, runs: [], text: "" };
}
