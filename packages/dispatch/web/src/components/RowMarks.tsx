import type { ReactNode } from "react";

/** The mark between two parts of a line, which a screen reader does not read. */
export function Dot(): ReactNode {
  return (
    <span aria-hidden="true" className="shrink-0">
      ·
    </span>
  );
}

/** The bullet that hangs left of a row's first line, as root R34's row draws one: an 18 px
 *  column, its 7 px dot in the colour of the row's kind (`theme/classes.ts`'s `pageKindBullet`)
 *  centred on that line, which a screen reader does not read. `mark` is the attribute the row's
 *  bullet is found by. */
export function KindBullet({
  colour,
  mark,
}: {
  colour: string;
  mark: "data-ask-bullet" | "data-decision-bullet";
}): ReactNode {
  return (
    <span
      aria-hidden="true"
      className="flex h-6 w-[18px] shrink-0 items-center justify-center"
      {...{ [mark]: "" }}
    >
      <i className={`block size-[7px] rounded-full ${colour}`} />
    </span>
  );
}
