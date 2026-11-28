import type { ReactNode } from "react";
import { Link } from "react-router-dom";

import type { AuthenticatedUser } from "../../api/types";
import {
  focusVisibleRing,
  iconRailAccentBg,
  iconRailActive,
  iconRailBg,
  iconRailHoverBg,
  iconRailText,
} from "../../theme/classes";
import { PRIORITIES_PATH } from "../priorities/priority-routes";
import { buildProjectPath } from "../refs/routes";

/**
 * The shell's navigation on the issue page, from the phone's width up: a 56 px column of icons
 * in place of the 320 px sidebar, so the issue's header and outline have the window less 56 px
 * (R14, the prototype's `#rail`). Every other page keeps the sidebar.
 *
 * Each icon is a link whose accessible name is its label, the words the sidebar gives the same
 * entry, and nothing opens on hover: the page carries no tooltip (R6). What the sidebar holds
 * beyond these - search, a new issue, the pinned agents and issues, every project, sign-out - is
 * one press away, under the control at the foot, which opens the whole navigation over the page.
 */
export function IconRail({
  inboxCount,
  navigationOpen,
  onOpenNavigation,
  project,
  user,
}: {
  /** The one number the sidebar's Inbox carries, which is part of the entry's name. */
  inboxCount: number;
  navigationOpen: boolean;
  onOpenNavigation: () => void;
  /** The project of the issue on the page, which the rail marks as where the reader is. */
  project: string | undefined;
  user: AuthenticatedUser;
}): ReactNode {
  return (
    <nav
      aria-label="App"
      className={`fixed inset-y-0 left-0 z-10 flex w-14 flex-col overflow-y-auto py-2.5 ${iconRailBg} ${iconRailText}`}
      data-testid="icon-rail"
    >
      <Link
        aria-label="Dispatch"
        className={`mb-3 flex h-10 shrink-0 items-center px-[22px] ${focusVisibleRing}`}
        to="/"
      >
        <i aria-hidden="true" className={`block size-3 rounded-full ${iconRailAccentBg}`} />
      </Link>
      {/* An agent viewer has no Inbox: its asks and threads are a person's. */}
      {user.kind === "user" ? (
        <RailLink
          icon={INBOX_ICON}
          label={inboxCount === 0 ? "Inbox" : `Inbox ${inboxCount}`}
          to="/"
        />
      ) : null}
      <RailLink icon={AGENTS_ICON} label="Agents" to="/agents" />
      <RailLink icon={PRIORITIES_ICON} label="Priorities" to={PRIORITIES_PATH} />
      <RailLink icon={DELIVERY_ICON} label="Delivery" to="/delivery" />
      <RailLink icon={PIPELINE_ICON} label="Pipeline" to="/pipeline" />
      {/* The issue's own project, marked as where the reader is by the accent rule at its edge
          as well as its colour. */}
      {project === undefined ? null : (
        <RailLink
          current
          icon={PROJECT_ICON}
          label={project}
          to={buildProjectPath({ kind: "project", project })}
        />
      )}
      <div className="flex-1" />
      {user.kind === "user" ? (
        <RailLink icon={SETTINGS_ICON} label="Settings" to="/settings" />
      ) : null}
      <button
        aria-expanded={navigationOpen}
        aria-haspopup="dialog"
        aria-label="Open navigation"
        className={`flex h-10 w-full shrink-0 items-center px-[18px] ${iconRailHoverBg} ${focusVisibleRing}`}
        onClick={onOpenNavigation}
        type="button"
      >
        {EXPAND_ICON}
      </button>
    </nav>
  );
}

function RailLink({
  current = false,
  icon,
  label,
  to,
}: {
  current?: boolean;
  icon: ReactNode;
  label: string;
  to: string;
}): ReactNode {
  return (
    <Link
      aria-current={current ? "true" : undefined}
      aria-label={label}
      className={`flex h-10 w-full shrink-0 items-center px-[18px] ${
        current ? iconRailActive : iconRailHoverBg
      } ${focusVisibleRing}`}
      to={to}
    >
      {icon}
    </Link>
  );
}

/** The prototype's own icons (`#rail`), 20 px, drawn in the entry's colour. Each is decoration:
 *  the link around it carries the entry's name. */
function icon(children: ReactNode): ReactNode {
  return (
    <svg
      aria-hidden="true"
      className="size-5 shrink-0"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      viewBox="0 0 20 20"
    >
      {children}
    </svg>
  );
}

const INBOX_ICON = icon(
  <>
    <path d="M3 11l2-7h10l2 7v5H3z" />
    <path d="M3 11h4l1 2h4l1-2h4" />
  </>
);
const AGENTS_ICON = icon(
  <>
    <circle cx="10" cy="7" r="3" />
    <path d="M4 17c1-3.5 3.3-5 6-5s5 1.5 6 5" />
  </>
);
const PRIORITIES_ICON = icon(
  <>
    <path d="M5 17V3" />
    <path d="M5 4h10l-2.5 3.5L15 11H5" />
  </>
);
const DELIVERY_ICON = icon(
  <>
    <path d="M3 15l4-5 3 3 4-6 3 4" />
    <path d="M3 17h14" />
  </>
);
const PIPELINE_ICON = icon(
  <>
    <rect height="12" rx="1" width="4" x="3" y="4" />
    <rect height="8" rx="1" width="4" x="8" y="4" />
    <rect height="5" rx="1" width="4" x="13" y="4" />
  </>
);
const PROJECT_ICON = icon(<path d="M3 6h5l1.5 2H17v8H3z" />);
const SETTINGS_ICON = icon(
  <>
    <circle cx="10" cy="10" r="2.5" />
    <path d="M10 2.5v2M10 15.5v2M2.5 10h2M15.5 10h2M4.7 4.7l1.4 1.4M13.9 13.9l1.4 1.4M4.7 15.3l1.4-1.4M13.9 6.1l1.4-1.4" />
  </>
);
const EXPAND_ICON = icon(<path d="M7 5l5 5-5 5" />);
