/**
 * The line of `dispatch spec-form` before the texts that Dispatch read in version `version` of
 * the document `name`. It has no import, so that scripts/spec-review-eval, which finds this line
 * in an agent's transcript as the proof that Dispatch answered its call, reads the line from here
 * and not from a copy.
 */
export function textsHeading(name: string, version: number): string {
  return `Dispatch read these texts in ${name} version ${version}, each by its kind and its node:`;
}
