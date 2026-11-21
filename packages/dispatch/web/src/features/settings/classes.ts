import {
  inputClasses,
  primaryButtonBg,
  primaryButtonHoverBg,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
} from "../../theme/classes";

/** The recipes every Settings section's form shares, so the sections stay in step. The table
 *  each section draws is `components/TableShell.tsx`, which owns its box, head and rows. */
export const settingsFieldLabel = `grid gap-1 text-sm font-medium ${textSecondaryOnCanvas}`;
export const settingsMonoInput = `rounded-md px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnSurface}`;
/** The trigger of a Settings list of the shared list's single pick, in the place of the form's
 *  `<select>`, with `settingsListText` on its text. Chromium makes a select's box its font's
 *  ascent and descent, its line gap left out, plus 1 px above and below, plus its padding, where
 *  a button takes the 20 px line the page sets. So each project row of the table stood taller
 *  than the select made it: by 2 px in DejaVu Sans, the pod's font (36 px against 34), and by
 *  1 px in WenQuanYi Zen Hei, the font the visual job's image draws (36 against 35). The text's
 *  line at the font's own edges (`text-box: trim-both text`, on a `line-height: normal` line)
 *  and 9 px above and below it give the select's box in both, measured: 34 and 35 px. */
export const settingsListTrigger = `rounded-md px-3 py-[9px] text-left leading-[normal] disabled:cursor-not-allowed disabled:opacity-50 ${inputClasses(false)} ${textPrimaryOnSurface}`;
export const settingsListText = "[text-box:trim-both_text]";
export const settingsSubmitButton = `min-h-11 rounded-md px-4 py-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 md:min-h-9 ${primaryButtonBg} ${primaryButtonHoverBg}`;
/** An `owner/repo` field's `pattern`: one slash between an owner and a name that hold neither a
 *  slash nor whitespace. Its slashes are escaped because browsers compile a `pattern` with the `v`
 *  flag, where an unescaped `/` inside a character class is a syntax error, and a `pattern` that
 *  does not compile checks nothing. */
export const ownerRepoPattern = "[^\\/\\s]+\\/[^\\/\\s]+";

/** The gap between one Settings section and the next: the spec's step between the parts of a
 *  page. The page owns the rhythm - no section sets an outer margin of its own, which is how
 *  three sections came to line up at 40px by coincidence rather than by rule. */
export const settingsSectionGap = "space-y-4";
