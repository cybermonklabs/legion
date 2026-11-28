import type { ReactNode } from "react";

import { REF_PICKER_KIND_NAMES, type RefPickerKind } from "./ref-picker";

/**
 * The glyph each kind of node wears: the search palette's hit icons, now the one set the
 * reference picker's rows carry too, so a comment is drawn the same wherever a reader meets
 * it. The four block kinds share one glyph, each under its own name: a block is a part of the
 * node above it, and a second glyph for a part of an ask would say it is a different sort of
 * thing.
 *
 * Each `<title>` is written out in its own `svg` rather than built once above them, because
 * that is what the accessibility rule reads (`lint/a11y/noSvgWithoutTitle`).
 */
export function RefKindIcon({ kind }: { kind: RefPickerKind }): ReactNode {
  const common = {
    "aria-hidden": true,
    className: "size-4 shrink-0",
    fill: "none",
    stroke: "currentColor",
    strokeLinecap: "round" as const,
    strokeLinejoin: "round" as const,
    strokeWidth: 1.75,
    viewBox: "0 0 24 24",
  };
  const name = REF_PICKER_KIND_NAMES[kind];

  if (kind === "issue") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <circle cx="12" cy="12" r="8" />
        <path d="M12 8v4l2.5 2" />
      </svg>
    );
  }
  if (kind === "document") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <path d="M7 3h7l3 3v15H7z" />
        <path d="M14 3v4h4M10 12h4M10 16h4" />
      </svg>
    );
  }
  if (kind === "comment") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <path d="M5 6.5h14v10H9l-4 3z" />
        <path d="M9 10h6M9 13h4" />
      </svg>
    );
  }
  if (kind === "ask") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <circle cx="12" cy="12" r="8" />
        <path d="M9.8 9.5a2.3 2.3 0 1 1 3.3 2.1c-.9.5-1.1.9-1.1 1.9M12 16h.01" />
      </svg>
    );
  }
  if (kind === "message") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <path d="M5 6.5h14v10H9l-4 3z" />
        <path d="M9 10h6M9 13h6" />
      </svg>
    );
  }
  if (kind === "person") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <circle cx="12" cy="8" r="3.5" />
        <path d="M5.5 20a6.5 6.5 0 0 1 13 0" />
      </svg>
    );
  }
  if (kind === "agent") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <rect height="11" rx="2.5" width="16" x="4" y="7" />
        <path d="M9.5 11.5h.01M14.5 11.5h.01M9.5 15h5M12 7V4" />
      </svg>
    );
  }
  if (kind === "role") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <path d="M12 3.5 19 6v6c0 4-3 7-7 8.5C8 19 5 16 5 12V6z" />
        <path d="M9.5 12l1.8 1.8 3.2-3.6" />
      </svg>
    );
  }
  if (kind === "component") {
    return (
      <svg {...common}>
        <title>{name}</title>
        <path d="M12 3.5 19.5 8v8L12 20.5 4.5 16V8z" />
        <path d="M4.5 8 12 12.2 19.5 8M12 12.2v8.3" />
      </svg>
    );
  }
  return (
    <svg {...common}>
      <title>{name}</title>
      <path d="M6 4v16M9 7.5h9M9 12h9M9 16.5h5" />
    </svg>
  );
}
