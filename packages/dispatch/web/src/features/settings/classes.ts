import {
  borderDefault,
  card,
  inputClasses,
  primaryButtonBg,
  primaryButtonHoverBg,
  surfaceMutedBg,
  textPrimaryOnSurface,
  textSecondaryOnCanvas,
  textSecondaryOnSurface,
} from "../../theme/classes";

/** The recipes every Settings section's table and form share, so the sections stay in step. */
/** `relative` is load-bearing: a table's `sr-only` header cell is absolutely positioned, and
 *  without a positioned ancestor inside the scroll container its containing block is the page,
 *  so it sits at the scrolled table's own x and widens the document past the viewport (the
 *  Settings page measured 417px at 390px). The table itself already scrolls here. */
export const settingsTableWrapper = `relative mt-6 overflow-x-auto rounded-xl border ${card}`;
export const settingsTableHead = `border-b ${surfaceMutedBg} ${borderDefault} ${textSecondaryOnSurface}`;
export const settingsTableRow = `border-b last:border-0 ${borderDefault}`;
/** A form field: its words over its control. `min-w-0` and the one `minmax(0,1fr)` column let
 *  the field shrink with the form's grid track, and the control with the field, so a long value
 *  - a select's longest option sets its natural width - never pushes the form's last control past
 *  the viewport (Settings scrolled sideways at 1280 px on a long project name). */
export const settingsFieldLabel = `grid min-w-0 grid-cols-[minmax(0,1fr)] gap-1 text-sm font-medium ${textSecondaryOnCanvas}`;
export const settingsMonoInput = `rounded-md px-3 py-2 font-mono ${inputClasses(false)} ${textPrimaryOnSurface}`;
export const settingsSubmitButton = `min-h-11 rounded-md px-4 py-2 font-medium disabled:cursor-not-allowed disabled:opacity-50 md:min-h-9 ${primaryButtonBg} ${primaryButtonHoverBg}`;
/** An `owner/repo` field's `pattern`: one slash between an owner and a name that hold neither a
 *  slash nor whitespace. Its slashes are escaped because browsers compile a `pattern` with the `v`
 *  flag, where an unescaped `/` inside a character class is a syntax error, and a `pattern` that
 *  does not compile checks nothing. */
export const ownerRepoPattern = "[^\\/\\s]+\\/[^\\/\\s]+";

/** What a repository box says about a value it refuses before it sends it: one that is not
 *  `owner/name` by the box's own pattern. */
export const NOT_OWNER_NAME = "Write the repository as owner/name.";

/** `pattern` as a test of a whole value, as a field's `pattern` attribute applies it: the check a
 *  repository box makes before it sends, so a typo shows at the box and not as the server's 400 a
 *  round trip later. */
export function wholeValue(pattern: string): RegExp {
  return new RegExp(`^(?:${pattern})$`);
}

/** The gap between one Settings section and the next: the spec's step between the parts of a
 *  page. The page owns the rhythm - no section sets an outer margin of its own, which is how
 *  three sections came to line up at 40px by coincidence rather than by rule. */
export const settingsSectionGap = "space-y-4";
