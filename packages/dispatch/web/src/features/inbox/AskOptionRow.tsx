import type { ReactNode } from "react";

import type { AskOptionWithTrees } from "../../api/types";
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  selectedCardBg,
  selectedCardBorder,
  textOptionDescription,
  textPrimaryOnSurface,
} from "../../theme/classes";
import { MarkdownBody } from "../refs/MarkdownBody";
import type { TextSource } from "../refs/text-references";
import { digitKbdClass } from "../shell/ShortcutHelp";
import { type OptionDigit, ROW_IN_HAND_CLASS } from "./option-digits";

/** Which of an approval request's two options a row is: Approve, or Request changes. */
export type AskApprovalAction = "approve" | "request-changes";

const ROW_CLASS = `flex min-h-11 items-start gap-3 rounded-lg border px-3 py-2 text-sm ${borderDefault}`;

/** The label/description column is a flex item; `min-w-0` overrides its `min-width: auto` so it
 *  shrinks to the row and `MarkdownBody`'s own `break-words` folds a long word. The row's parent
 *  `<fieldset>` needs `min-w-0` too: its UA default `min-inline-size: min-content` would let the
 *  longest unbroken word widen the fieldset past a narrow card before anything here applies. */
function OptionText({
  option,
  source,
}: {
  option: AskOptionWithTrees;
  source?: TextSource;
}): ReactNode {
  return (
    <span className="min-w-0">
      <span className={`font-medium ${textPrimaryOnSurface}`}>
        <MarkdownBody
          markdown={option.label}
          source={source}
          surface="ask-option-text"
          tree={option.label_tree}
          variant="inline"
        />
      </span>
      {option.description === undefined ? null : (
        <span className={`mt-0.5 block text-sm ${textOptionDescription}`}>
          <MarkdownBody
            markdown={option.description}
            source={source}
            surface="ask-option-text"
            tree={option.description_tree}
            variant="inline"
          />
        </span>
      )}
    </span>
  );
}

/** A choosable option: a radio or checkbox row. The `name` groups radios per ask. `hotkey` marks
 * the input `data-ask-option` so the Inbox's digit keys pick it - every option of the ask in hand,
 * Other included as the digit after the last real option - and `digit` is the key the row draws
 * for that, beside its text and hidden from assistive tech, whose name for the option stays its
 * label (`option-digits.ts`). `approval` marks one of an approval request's two options
 * `data-ask-approval`, which the Inbox's `a` and Shift+R pick it by. While the card's answer is
 * out the options are held by their group (`AskCard`), so the row puts no attribute of its own on
 * its input. */
export function AskChoiceRow({
  approval,
  checked,
  className = "",
  digit,
  hotkey = false,
  multiple,
  name,
  onChange,
  option,
  source,
}: {
  approval?: AskApprovalAction;
  checked: boolean;
  className?: string;
  digit?: OptionDigit;
  hotkey?: boolean;
  multiple: boolean;
  name: string;
  onChange: () => void;
  option: AskOptionWithTrees;
  /** The ask whose option this is (`TextSource`), so a `#id` in its label resolves against the
   *  document that ask is scoped to; none where the option is a document block's own
   *  attribute, which no ask row stands behind. */
  source?: TextSource;
}): ReactNode {
  // A chosen option takes the same border and fill the recorded answer shows afterwards, so
  // choosing and having chosen read alike.
  return (
    <label
      className={`${ROW_CLASS} cursor-pointer ${checked ? `${selectedCardBorder} ${selectedCardBg}` : cardHoverBorder} ${className}`}
    >
      <input
        checked={checked}
        data-ask-approval={approval}
        data-ask-option={hotkey ? "" : undefined}
        name={name}
        onChange={onChange}
        type={multiple ? "checkbox" : "radio"}
      />
      <OptionText option={option} source={source} />
      {/* The digit is for the eye: the option's name stays its label. */}
      {digit === undefined ? null : (
        <span
          aria-hidden="true"
          className={`ml-auto shrink-0 ${digit.whileRowInHand ? ROW_IN_HAND_CLASS : ""}`}
        >
          <kbd className={digitKbdClass}>{digit.key}</kbd>
        </span>
      )}
    </label>
  );
}

/** A recorded option: ticked and tinted when it was chosen. */
export function AskRecordRow({
  className = "",
  option,
  selected,
  source,
}: {
  className?: string;
  option: AskOptionWithTrees;
  selected: boolean;
  /** The ask whose option this is (`TextSource`); see `AskChoiceRow`. */
  source?: TextSource;
}): ReactNode {
  return (
    <li
      className={`${ROW_CLASS} ${selected ? `${selectedCardBorder} ${selectedCardBg}` : className}`}
      data-selected={selected}
    >
      {selected ? (
        <span aria-label="Selected" className={`font-semibold ${linkText}`} role="img">
          ✓
        </span>
      ) : (
        <span aria-hidden className="w-3" />
      )}
      <OptionText option={option} source={source} />
    </li>
  );
}
