/**
 * The boxes of `CopyButton`, in one place. Both of its forms, the icon-only one and the one that
 * shows its value as a label, stand in one base box, written once here, so a change to the tap
 * height or the radius reaches both.
 *
 * The icon-only box is here too because two surfaces draw it: the button itself, and the space
 * the issue page keeps beside the issue's key while the header is still to come
 * (`features/issue/issue-page-skeleton.tsx`, dispatch://INFRA-341). The space stands at the
 * header's own height only while it keeps this box at the button's own size: it is the key row's
 * height below `md`, and with the key badge it is where the title starts from `sm` up. A copy of
 * these classes in the space would part from the button the first time one of them changed.
 *
 * Data only, as `tab-classes.ts` is, so the shell's own chunk, which draws that space, takes
 * nothing of the button with it.
 */

/** The box both forms stand in: a 44 px tap target, 32 px from `md`, its content in one row
 *  centred on the box's height. Whether the box keeps its width is each form's own: the icon-only
 *  one always does, and the one that shows its value does everywhere but inside `NoTooltips`,
 *  where the value it draws wraps and the box narrows to its line (`CopyButton`). */
export const copyBoxClassName = "inline-flex min-h-11 items-center rounded-lg md:min-h-8";

/** The icon-only button's box: the base, no narrower than it is tall and never giving up that
 *  width, the icon centred in it. */
export const copyIconBoxClassName = `${copyBoxClassName} min-w-11 shrink-0 justify-center md:min-w-8`;
