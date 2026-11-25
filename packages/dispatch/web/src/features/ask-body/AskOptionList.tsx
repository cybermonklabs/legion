import type { ReactNode } from "react";

import type { AskOptionWithTrees } from "../../api/types";
import { AskRecordRow } from "./AskOptionRow";
import { ASK_RECORD_WORDS } from "./ask-body-words";
import { type AskTextRenderer, markdownText } from "./ask-text";

export function AskOptionList({
  options,
  renderText = markdownText,
  rowClassName,
  selected,
}: {
  options: AskOptionWithTrees[];
  /** The renderer of each option's label and description (`AskTextRenderer`): `MarkdownBody`,
   *  as every surface of today's pages draws them, unless the ask body it stands in hands
   *  another. */
  renderText?: AskTextRenderer;
  /** Extra classes for each unchosen row, e.g. a surface fill on a tinted host. */
  rowClassName?: string;
  selected: readonly string[];
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
          renderText={renderText}
          selected={selected.includes(option.label)}
        />
      ))}
    </ul>
  );
}
