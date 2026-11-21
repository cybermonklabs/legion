import { statusText } from "../project/board-model";

/**
 * The accessible name of a bare issue reference, which draws as its status mark and its key
 * alone: the key, the status's words and the title (INFRA-4009 R3), as `CORE-12, In progress,
 * Fix the thing`. The title is drawn nowhere else on the line, so a reader with no peek - touch,
 * or the issue page's rows, which open none - hears it here. A rendered body (`RefLink`), the
 * document editor (`document-references.tsx`) and the issue page's rows (`row-references.ts`)
 * name it by this one rule.
 */
export function issueReferenceName(key: string, status: string, title: string): string {
  return `${key}, ${statusText(status)}, ${title}`;
}
