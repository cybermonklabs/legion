import type { ReactNode } from "react";

import type { AskOptionWithTrees } from "../../api/types";
import {
  borderDefault,
  cardHoverBorder,
  linkText,
  pageHairBorder,
  pageInkBorder,
  pageInkText,
  pageMarkBg,
  pageMutedText,
  pageRowHoverBg,
  selectedCardBg,
  selectedCardBorder,
  textOptionDescription,
  textPrimaryOnSurface,
} from "../../theme/classes";
import type { TextSource } from "../refs/text-references";
import { ASK_RECORD_WORDS } from "./ask-body-words";
import type { AskPalette, AskTextRenderer } from "./ask-text";

/** The shape of an option's row, whichever palette draws it. */
const ROW_SHAPE = "flex min-h-11 items-start gap-3 rounded-lg border px-3 py-2 text-sm";

/**
 * The colours of an option's row by the palette its host hands (`AskPalette`), as
 * `issue-page/DecisionActions.tsx` keeps its own: `main`'s are the classes every surface of
 * today's pages draws, and the new issue page's take the role `main`'s colour has - a row's rule
 * is the hairline, a row under the pointer is on the mark grey (`pageRowHoverBg`), a chosen or a
 * recorded choice is the ink's rule on the mark grey where `main` draws its selected card, the
 * label and the recorded choice's tick are the ink and the description the muted grey. Each pair
 * is one `theme/classes.ts` registers for the page (the ink and the muted grey on the paper and
 * on the mark grey).
 */
const TONES = {
  main: {
    choice: `${borderDefault} cursor-pointer ${cardHoverBorder}`,
    chosen: `${borderDefault} cursor-pointer ${selectedCardBorder} ${selectedCardBg}`,
    description: textOptionDescription,
    label: textPrimaryOnSurface,
    record: borderDefault,
    recorded: `${borderDefault} ${selectedCardBorder} ${selectedCardBg}`,
    tick: linkText,
  },
  page: {
    choice: `${pageHairBorder} cursor-pointer ${pageRowHoverBg}`,
    chosen: `${pageInkBorder} cursor-pointer ${pageMarkBg}`,
    description: pageMutedText,
    label: pageInkText,
    record: pageHairBorder,
    recorded: `${pageInkBorder} ${pageMarkBg}`,
    tick: pageInkText,
  },
} as const;

/** The label/description column is a flex item; `min-w-0` overrides its `min-width: auto` so it
 *  shrinks to the row and `MarkdownBody`'s own `break-words` folds a long word. The row's parent
 *  `<fieldset>` needs `min-w-0` too: its UA default `min-inline-size: min-content` would let the
 *  longest unbroken word widen the fieldset past a narrow card before anything here applies. The
 *  label and the description are drawn inline through the caller's renderer (R17). */
function OptionText({
  option,
  palette,
  renderText,
  source,
}: {
  option: AskOptionWithTrees;
  palette: AskPalette;
  renderText: AskTextRenderer;
  source?: TextSource;
}): ReactNode {
  const tones = TONES[palette];
  return (
    <span className="min-w-0">
      <span className={`font-medium ${tones.label}`}>
        {renderText({
          markdown: option.label,
          source,
          surface: "ask-option-text",
          tree: option.label_tree,
          variant: "inline",
        })}
      </span>
      {option.description === undefined ? null : (
        <span className={`mt-0.5 block text-sm ${tones.description}`}>
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
  palette = "main",
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
  /** The palette the row draws in (`TONES`): `main`'s, unless the ask body's host hands the
   *  page's. */
  palette?: AskPalette;
  renderText: AskTextRenderer;
  /** The ask whose option this is (`TextSource`), so a `#id` in its label resolves against the
   *  document that ask is scoped to; none where the option is a document block's own
   *  attribute, which no ask row stands behind. */
  source?: TextSource;
}): ReactNode {
  const tones = TONES[palette];
  // A chosen option takes the same border and fill the recorded answer shows afterwards, so
  // choosing and having chosen read alike.
  return (
    <label className={`${ROW_SHAPE} ${checked ? tones.chosen : tones.choice} ${className}`}>
      <input
        checked={checked}
        data-ask-option={hotkey ? "" : undefined}
        disabled={disabled}
        name={name}
        onChange={onChange}
        type={multiple ? "checkbox" : "radio"}
      />
      <OptionText option={option} palette={palette} renderText={renderText} source={source} />
    </label>
  );
}

/** A recorded option: ticked and tinted when it was chosen. */
export function AskRecordRow({
  className = "",
  option,
  palette = "main",
  renderText,
  selected,
  source,
}: {
  className?: string;
  option: AskOptionWithTrees;
  /** The palette the row draws in (`TONES`), as `AskChoiceRow`'s. */
  palette?: AskPalette;
  renderText: AskTextRenderer;
  selected: boolean;
  /** The ask whose option this is (`TextSource`); see `AskChoiceRow`. */
  source?: TextSource;
}): ReactNode {
  const tones = TONES[palette];
  return (
    <li
      className={`${ROW_SHAPE} ${selected ? tones.recorded : `${tones.record} ${className}`}`}
      data-selected={selected}
    >
      {selected ? (
        <span
          aria-label={ASK_RECORD_WORDS.option_selected}
          className={`font-semibold ${tones.tick}`}
          role="img"
        >
          ✓
        </span>
      ) : (
        <span aria-hidden className="w-3" />
      )}
      <OptionText option={option} palette={palette} renderText={renderText} source={source} />
    </li>
  );
}
