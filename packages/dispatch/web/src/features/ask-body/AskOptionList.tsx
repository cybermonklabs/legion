import type { ReactNode } from "react";

import type { AskOptionWithTrees } from "../../api/types";
import type { TextSource } from "../refs/text-references";
import { AskRecordRow } from "./AskOptionRow";
import { ASK_RECORD_WORDS } from "./ask-body-words";
import { type AskPalette, type AskTextRenderer, markdownText } from "./ask-text";

export function AskOptionList({
  options,
  palette = "main",
  renderText = markdownText,
  rowClassName,
  selected,
  source,
}: {
  options: AskOptionWithTrees[];
  /** The palette each row draws in (`AskRecordRow`): `main`'s, unless the ask body's host hands
   *  the page's. */
  palette?: AskPalette;
  /** The renderer of each option's label and description (`AskTextRenderer`): `MarkdownBody`,
   *  as every surface of today's pages draws them, unless the ask body it stands in hands
   *  another. */
  renderText?: AskTextRenderer;
  /** Extra classes for each unchosen row, e.g. a surface fill on a tinted host. */
  rowClassName?: string;
  selected: readonly string[];
  /** The ask these options belong to (`TextSource`); none where they are a document block's
   *  own attributes, which no ask row stands behind. */
  source?: TextSource;
}): ReactNode {
  if (options.length === 0) {
    return null;
  }
  return (
    <ul aria-label={ASK_RECORD_WORDS.options} className="mt-2 space-y-1">
      {options.map((option) => (
        <AskRecordRow
          className={rowClassName}
          key={option.label}
          option={option}
          palette={palette}
          renderText={renderText}
          selected={selected.includes(option.label)}
          source={source}
        />
      ))}
    </ul>
  );
}
