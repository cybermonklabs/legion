/**
 * The step count of this folder's text readers: every loop of `composerReferences`,
 * `referenceSpans`, `trimReference`, `readTextReferences` and the unfurl cards' reading
 * (`Unfurl.tsx`) calls `readerStep` once a turn. Its rows hold each reader to one pass over the
 * text it reads by counting those turns on a text and on one four times as long, since no clock
 * can say that: a pass that takes twice as long on a loaded machine is still one pass
 * (`composer-references.test.ts`, `Unfurl.test.tsx`, `text-references.test.ts`). What a regular
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
