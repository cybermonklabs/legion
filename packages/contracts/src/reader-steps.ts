/**
 * The step count of the text readers that are held to one pass: every loop of `referenceSpans`
 * and `trimReference` (`reference-spans.ts`), and of the dashboard's `readTextReferences`, the
 * code contexts every reading reads first (`codeRanges`), the inline links of a draft
 * (`inlineLinks`) and its unfurl cards' reading (`text-references.ts`, `code-ranges.ts`,
 * `party-references.ts` and `Unfurl.tsx` in `packages/dispatch/web/src/features/refs`), calls
 * `readerStep` once a turn. The dashboard's rows hold each reader to one pass over the text it
 * reads by counting those turns on a text and on one four times as long, since no clock can say
 * that: a pass that takes twice as long on a loaded machine is still one pass
 * (`text-references.test.ts`, `party-references.test.ts`, `Unfurl.test.tsx`). What a regular
 * expression does inside one call is no turn of a loop; those expressions are checked as they are
 * written (`reader-expressions.test.ts`).
 *
 * Outside `countReaderSteps` a step is a call to a function that does nothing.
 */
const noStep = (): void => {};

/** One turn of a loop of a text reader. */
export let readerStep: () => void = noStep;

/** How many steps `read` takes. */
export function countReaderSteps(read: () => void): number {
  let steps = 0;
  readerStep = () => {
    steps += 1;
  };
  try {
    read();
  } finally {
    readerStep = noStep;
  }
  return steps;
}
