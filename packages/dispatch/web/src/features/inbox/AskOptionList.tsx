import type { ReactNode } from "react";

import type { AskOption } from "../../api/types";
import type { TextSource } from "../refs/text-references";
import { AskRecordRow } from "./AskOptionRow";

export function AskOptionList({
  options,
  rowClassName,
  selected,
  source,
}: {
  options: AskOption[];
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
    <ul aria-label="Options" className="mt-2 space-y-1">
      {options.map((option) => (
        <AskRecordRow
          className={rowClassName}
          key={option.label}
          option={option}
          selected={selected.includes(option.label)}
          source={source}
        />
      ))}
    </ul>
  );
}
