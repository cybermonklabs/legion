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
import type { TextSource } from "../refs/text-references";
import { ASK_RECORD_WORDS } from "./ask-body-words";
import type { AskTextRenderer } from "./ask-text";

const ROW_CLASS = `flex min-h-11 items-start gap-3 rounded-lg border px-3 py-2 text-sm ${borderDefault}`;

/** The label/description column is a flex item; `min-w-0` overrides its `min-width: auto` so it
 *  shrinks to the row and `MarkdownBody`'s own `break-words` folds a long word. The row's parent
 *  `<fieldset>` needs `min-w-0` too: its UA default `min-inline-size: min-content` would let the
 *  longest unbroken word widen the fieldset past a narrow card before anything here applies. The
 *  label and the description are drawn inline through the caller's renderer (R17). */
function OptionText({
  option,
  renderText,
  source,
}: {
  option: AskOptionWithTrees;
  renderText: AskTextRenderer;
  source?: TextSource;
}): ReactNode {
  return (
    <span className="min-w-0">
      <span className={`font-medium ${textPrimaryOnSurface}`}>
        {renderText({
          markdown: option.label,
          source,
          surface: "ask-option-text",
          tree: option.label_tree,
          variant: "inline",
        })}
      </span>
      {option.description === undefined ? null : (
        <span className={`mt-0.5 block text-sm ${textOptionDescription}`}>
          {renderText({
            markdown: option.description,
            source,
            surface: "ask-option-text",
            tree: option.description_tree,
            variant: "inline",
          })}
        </span>
      )}
    </span>
  );
}

/** A choosable option: a radio or checkbox row. The `name` groups radios per ask. `hotkey` marks
 * the input `data-ask-option` so the Inbox's number keys 1–9 pick it (the real options of the
 * focused ask, not Other). */
export function AskChoiceRow({
  checked,
  className = "",
  disabled = false,
  hotkey = false,
  multiple,
  name,
  onChange,
  option,
  renderText,
  source,
}: {
  checked: boolean;
  className?: string;
  disabled?: boolean;
  hotkey?: boolean;
  multiple: boolean;
  name: string;
  onChange: () => void;
  option: AskOptionWithTrees;
  renderText: AskTextRenderer;
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
        data-ask-option={hotkey ? "" : undefined}
        disabled={disabled}
        name={name}
        onChange={onChange}
        type={multiple ? "checkbox" : "radio"}
      />
      <OptionText option={option} renderText={renderText} source={source} />
    </label>
  );
}

/** A recorded option: ticked and tinted when it was chosen. */
export function AskRecordRow({
  className = "",
  option,
  renderText,
  selected,
  source,
}: {
  className?: string;
  option: AskOptionWithTrees;
  renderText: AskTextRenderer;
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
        <span
          aria-label={ASK_RECORD_WORDS.option_selected}
          className={`font-semibold ${linkText}`}
          role="img"
        >
          ✓
        </span>
      ) : (
        <span aria-hidden className="w-3" />
      )}
      <OptionText option={option} renderText={renderText} source={source} />
    </li>
  );
}
