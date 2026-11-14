import type { ReactNode } from "react";
import { linkHoverText, linkText, textMutedOnCanvas } from "../theme/classes";

/** One of a row's links to another system: where it goes, what it is called, and how it stands
 *  to the row. */
export interface LinkOut {
  readonly href: string;
  /** What the link reads as. */
  readonly label: string;
  /** How the link stands to the row — `via`, `describedby`, `enclosure`, `source-rule` — drawn
   *  beside the label so a reader knows what the far side is before pressing it. A link with
   *  none is a plain link. */
  readonly relation?: string;
}

export interface LinksOutCellProps {
  readonly links: readonly LinkOut[];
  /** What the cell says where the row has no link at all. */
  readonly empty?: ReactNode;
}

/** The host a link goes to, or nothing where the address names none: a link out is written
 *  whole, and one that is not — a path inside this application, an address this build cannot
 *  read — is drawn as a link with no host beside it rather than refused. */
function hostOf(href: string): string {
  try {
    return new URL(href).host;
  } catch {
    return "";
  }
}

/**
 * A row's links out: the places another system holds what this row stands for — a run, a report,
 * a rule — each opening in a window of its own, since following one leaves the application. Each
 * is drawn with its label, its relation and its host, so a reader sees where it goes before
 * pressing it, and nothing here reads the far side: a link whose target is gone still draws.
 */
export function LinksOutCell({ empty = "—", links }: LinksOutCellProps): ReactNode {
  if (links.length === 0) return <span className={textMutedOnCanvas}>{empty}</span>;
  return (
    <span className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
      {links.map(({ href, label, relation }) => {
        const detail = [relation ?? "", hostOf(href)].filter((part) => part !== "").join(" · ");
        return (
          <span className="flex min-w-0 items-baseline gap-1" key={`${relation ?? ""} ${href}`}>
            <a
              className={`truncate underline ${linkText} ${linkHoverText}`}
              href={href}
              rel="noreferrer"
              target="_blank"
            >
              {label}
            </a>
            {detail === "" ? null : (
              <span className={`truncate text-xs ${textMutedOnCanvas}`}>{detail}</span>
            )}
          </span>
        );
      })}
    </span>
  );
}
