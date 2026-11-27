/** The two marks of a key an input method sends as a composition ends, for `test.each`: its name
 *  in the row's title, and the fields of the `keydown` that carry it. Chromium and Firefox mark
 *  that key `isComposing`; Safari sends it after the composition's end, unmarked, with keyCode 229
 *  (WebKit bug 165004). user-event has no form for either, so a row fires the key with these. */
export const INPUT_METHOD_MARKS = [
  ["isComposing", { isComposing: true }],
  ["keyCode 229", { isComposing: false, keyCode: 229 }],
] as const;
