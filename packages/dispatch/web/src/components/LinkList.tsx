import type { ReactNode } from "react";

import type { EvalLink } from "../api/types";
import { linkText, textMutedOnSurface } from "../theme/classes";

/** What a link list draws of one link: where it goes, what it is called and how it relates. */
export type ListedLink = Pick<EvalLink, "href" | "label" | "rel">;

/** How a link is drawn: one of the four relations that each draw with an icon and words of their
 *  own (dispatch://INFRA-2918/spec R10), or a plain link. */
export type LinkKind = "via" | "describedby" | "enclosure" | "source-rule" | "plain";

/** A small line icon, drawn in the text's own colour beside a link's label. */
function icon(children: ReactNode): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="size-3.5 shrink-0 self-center"
      fill="none"
      stroke="currentColor"
      strokeLinecap="round"
      strokeLinejoin="round"
      strokeWidth="1.6"
      viewBox="0 0 20 20"
    >
      {children}
    </svg>
  );
}

/** The four relations a reader is shown apart from a plain link, each with its icon and the words
 *  that say what the far side is. */
const RELATIONS: Readonly<Record<Exclude<LinkKind, "plain">, { icon: ReactNode; words: string }>> =
  {
    // An arrow through a point: the result came by way of the far side.
    via: { icon: icon(<path d="M3 10h11M10 5.5l4.5 4.5-4.5 4.5" />), words: "Via" },
    // A page of lines: the far side describes this one.
    describedby: {
      icon: icon(
        <>
          <path d="M5 2.5h7l3 3v12H5z" />
          <path d="M8 9h4.5M8 12h4.5" />
        </>
      ),
      words: "Described by",
    },
    // A paper clip: a file that belongs with the result.
    enclosure: {
      icon: icon(
        <path d="M13 6.5l-5.2 5.2a1.6 1.6 0 002.3 2.3l5.4-5.4a3.1 3.1 0 00-4.4-4.4L5.6 9.7a4.6 4.6 0 006.5 6.5L16 12.3" />
      ),
      words: "Attachment",
    },
    // A ruler: the rule that made the result.
    "source-rule": {
      icon: icon(
        <>
          <path d="M2.5 7h15v6h-15z" />
          <path d="M6 7v2.5M9.5 7v3M13 7v2.5" />
        </>
      ),
      words: "Source rule",
    },
  };

/** How a link of relation `rel` is drawn. A relation is matched without regard to case, as link
 *  relations are (RFC 8288). */
export function linkKind(rel: string): LinkKind {
  const name = rel.toLowerCase();
  return Object.hasOwn(RELATIONS, name) ? (name as LinkKind) : "plain";
}

/** The host a link goes to, "" where its address names none. Read from the address alone:
 *  nothing is fetched. */
export function hostOf(href: string): string {
  try {
    return new URL(href).host;
  } catch {
    return "";
  }
}

/**
 * Links out to results (dispatch://INFRA-2918/spec R10, plan D12): each link drawn as its label,
 * its relation and the host it goes to, so a reader sees where it leads before pressing it. `via`,
 * `describedby`, `enclosure` and `source-rule` each draw with an icon and words of their own; any
 * other relation draws as a plain link that names its relation as written. Nothing here reads the
 * far side, so a link whose target is gone draws the same as any other. A link listed twice, the
 * same relation, address and label, is drawn once.
 */
export function LinkList({
  label,
  links,
}: {
  /** The list's accessible name. */
  label: string;
  links: readonly ListedLink[];
}): ReactNode {
  const drawn = new Map(links.map((link) => [`${link.rel}\n${link.href}\n${link.label}`, link]));
  return (
    <ul aria-label={label} className="flex flex-wrap gap-x-3 gap-y-1 text-xs">
      {[...drawn].map(([key, link]) => {
        const kind = linkKind(link.rel);
        const host = hostOf(link.href);
        const relation = kind === "plain" ? undefined : RELATIONS[kind];
        return (
          <li className="flex min-w-0 items-baseline gap-1" key={key}>
            <a
              className={`inline-flex min-w-0 items-baseline gap-1 hover:underline ${linkText}`}
              data-link-kind={kind}
              data-rel={link.rel}
              href={link.href}
              rel="noreferrer noopener"
              target="_blank"
              title={link.href}
            >
              {relation?.icon}
              <span className="truncate">{link.label === "" ? link.href : link.label}</span>
            </a>
            <span className={textMutedOnSurface} data-link-relation>
              {relation?.words ?? link.rel}
            </span>
            {host === "" ? null : (
              <span className={textMutedOnSurface} data-link-host>
                {host}
              </span>
            )}
          </li>
        );
      })}
    </ul>
  );
}
