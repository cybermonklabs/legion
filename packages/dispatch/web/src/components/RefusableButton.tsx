import { type ReactNode, useId } from "react";

import {
  pageInkFill,
  pageLineBorder,
  pageMutedText,
  pagePaperBg,
  pageSend,
  primaryButton,
  secondaryButtonCompact,
} from "../theme/classes";
import { useTooltips } from "./tooltips";

/** Each look whole, refused or not: the `refused:` variant (`styles.css`) paints `disabled` and
 *  `aria-disabled` alike, so a look states its cannot-press state once. */
const LOOKS = {
  "page-send": pageSend,
  primary: primaryButton,
  "secondary-compact": secondaryButtonCompact,
} as const;

/** Each look inside `NoTooltips`, in the new issue page's colours, on the same shape as
 *  `LOOKS`: the primary is filled with the page's ink, and refused is the page's own refused
 *  send, muted words on the hairline grey; the compact secondary is the page's header control,
 *  its rule and muted words on the paper, with quiet words while refused. The issue page's own
 *  send is drawn in its colours already, so it reads the same inside it. */
const PAGE_LOOKS: Record<keyof typeof LOOKS, string> = {
  "page-send": pageSend,
  primary: `rounded-lg px-3 py-2 text-sm font-semibold ${pageInkFill} refused:cursor-not-allowed refused:bg-page-hair refused:text-page-muted`,
  "secondary-compact": `min-h-11 shrink-0 rounded-lg border px-3 py-1 text-xs font-medium whitespace-nowrap sm:min-h-7 sm:px-2 ${pageLineBorder} ${pagePaperBg} ${pageMutedText} refused:text-page-quiet`,
};

/**
 * A button that says why it cannot be pressed, in one of `LOOKS`. A refusal is `aria-disabled`
 * rather than `disabled`, so the button stays in the tab order and its reason reaches a keyboard
 * and a screen reader: on `title`, and through `aria-describedby` on the line that shows it
 * (`refusalShownBy`), or, where no line on screen does, on a hidden one of the button's own. A
 * click on a refused button does nothing, a submit button's included, so the refusal holds
 * whatever the host's handler checks; a form's other ways to submit (Ctrl/Cmd+Enter) remain the
 * host's to refuse. A host that keeps its line blank until a person tries takes that press as
 * `onRefusedPress`. Only `busy` - a press already in flight, which its label names - disables it
 * outright.
 */
export function RefusableButton({
  busy,
  children,
  className = "",
  describedBy,
  look = "primary",
  onPress,
  onRefusedPress,
  refusal,
  refusalShownBy,
  type = "button",
}: {
  /** The label while a press is in flight; the one state that renders `disabled`. */
  readonly busy?: string;
  readonly children: ReactNode;
  /** Placement only: the button's look, refused or not, is this component's. */
  readonly className?: string;
  /** The id of a line on screen that bears on the press, read after the reason. */
  readonly describedBy?: string;
  readonly look?: keyof typeof LOOKS;
  readonly onPress?: () => void;
  /** Told of a press the refusal swallowed, for a host whose line reads nothing until then. */
  readonly onRefusedPress?: () => void;
  /** Why the button cannot be pressed, or null while it can. */
  readonly refusal: string | null;
  /** The id of the line on screen that already says `refusal`. */
  readonly refusalShownBy?: string;
  readonly type?: "button" | "submit";
}): ReactNode {
  const tooltips = useTooltips();
  const ownReasonId = useId();
  const reasonId = refusal === null ? undefined : (refusalShownBy ?? ownReasonId);
  const description = [reasonId, describedBy].filter((id) => id !== undefined).join(" ");
  return (
    <>
      <button
        aria-describedby={description === "" ? undefined : description}
        aria-disabled={refusal === null ? undefined : true}
        className={`${(tooltips ? LOOKS : PAGE_LOOKS)[look]} ${className}`}
        disabled={busy !== undefined}
        onClick={(event) => {
          if (refusal !== null) {
            event.preventDefault();
            onRefusedPress?.();
            return;
          }
          onPress?.();
        }}
        title={tooltips ? (refusal ?? undefined) : undefined}
        type={type}
      >
        {busy ?? children}
      </button>
      {refusal === null || refusalShownBy !== undefined ? null : (
        <span className="sr-only" id={ownReasonId}>
          {refusal}
        </span>
      )}
    </>
  );
}

// The page's looks, which the decision card's own buttons draw too (`DecisionCard.tsx`'s `TONES`).
export { PAGE_LOOKS };
