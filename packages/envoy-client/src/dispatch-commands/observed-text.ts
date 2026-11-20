import {
  SPEC_CHECK_OBSERVED_CUT_NOTE,
  SPEC_CHECK_OBSERVED_END,
  SPEC_CHECK_OBSERVED_MAX,
  SPEC_CHECK_OBSERVED_START,
} from "@legion/contracts";

/** The UTF-16 length of the first `count` characters (code points) of `text`, or undefined when
 *  `text` holds no more than `count` characters. */
function lengthOfFirst(text: string, count: number): number | undefined {
  let at = 0;
  for (let seen = 0; seen < count && at < text.length; seen += 1) {
    at += (text.codePointAt(at) ?? 0) > 0xffff ? 2 : 1;
  }
  return at < text.length ? at : undefined;
}

/** The UTF-16 index where the last `count` characters (code points) of `text` start. */
function startOfLast(text: string, count: number): number {
  let at = text.length;
  for (let seen = 0; seen < count && at > 0; seen += 1) {
    const low = text.charCodeAt(at - 1);
    const high = at > 1 ? text.charCodeAt(at - 2) : 0;
    at -= low >= 0xdc00 && low <= 0xdfff && high >= 0xd800 && high <= 0xdbff ? 2 : 1;
  }
  return at;
}

/** What a result of a spec check keeps of `observed`, as Dispatch keeps it (`cutObserved`,
 *  spec_results.go): each NUL, which Dispatch cannot store, as U+FFFD, then the text whole when it
 *  is SPEC_CHECK_OBSERVED_MAX characters or fewer, else its first SPEC_CHECK_OBSERVED_START
 *  characters, SPEC_CHECK_OBSERVED_CUT_NOTE and its last SPEC_CHECK_OBSERVED_END characters,
 *  that many in all: the start names the test, and the end says why it failed. Each byte of a
 *  file that is not UTF-8 is already U+FFFD, as the command decodes the files it reads. A test's
 *  log can be larger than any request Dispatch takes, so `dispatch result --project` sends only
 *  what Dispatch keeps, and the result is written. */
export function observedAsKept(observed: string): string {
  const text = observed.replaceAll("\u0000", "\uFFFD");
  if (lengthOfFirst(text, SPEC_CHECK_OBSERVED_MAX) === undefined) return text;
  const start = text.slice(0, lengthOfFirst(text, SPEC_CHECK_OBSERVED_START));
  return `${start}${SPEC_CHECK_OBSERVED_CUT_NOTE}${text.slice(startOfLast(text, SPEC_CHECK_OBSERVED_END))}`;
}
