import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect, useId, useRef, useState } from "react";
import { flushSync } from "react-dom";
import { useLocation } from "react-router-dom";

import { whoAmIQuery } from "../../api/queries";
import type { IssueChild } from "../../api/types";
import { pageInkText, pageQuietText, pageWordsText } from "../../theme/classes";
import { EVENT_FRESH } from "../refs/reference-target";
import type { IssueRoute } from "../refs/routes";
import { focusOnDocument } from "../shell/roving";
import { useUserPreference } from "../shell/userPreference";
import { ChildIssueFacts, ChildIssueTag } from "./IssueChildren";
import { ChildrenProgress } from "./IssuePropertiesRow";
import { bringOnScreen } from "./identity-block";
import { issueAddress, presetOf } from "./issue-address";
import { authorText, pageChrome } from "./page-structure";
import { CONTROL_LABELS } from "./page-words";
import { rowTiming } from "./RowText";
import type { SlotProps } from "./slot-props";
import { useFollowing } from "./slots/following";
import { outlineOf } from "./tree-read/tree-read";

/** How many children the section lists before its control lists the next ones, and how many
 *  each press of that control adds (R14, R15). */
export const CHILDREN_PAGE = 20;

/** The section's own mark, by which the children's chord (`useIssueKeymap.ts`) and a child's tag
 *  on its row of the outline (`openChildEntry`) find it. */
export const CHILDREN_SECTION = "[data-children-section]";

/** The section's heading control, which opens and closes its list, and which the children's
 *  address and chord put the focus on. */
export const CHILDREN_TOGGLE = "[data-children-toggle]";

/** The event a child's tag sends the section, with the child's key as its `detail`
 *  (`openChildEntry`). */
const CHILD_ENTRY_EVENT = "children-section-entry";

/** The reader's kept choice, as their browser stores it: anything but "collapsed" is the section
 *  open, so it opens expanded until they close it. */
function readExpanded(stored: string | null): boolean {
  return stored !== "collapsed";
}

function writeExpanded(expanded: boolean): string {
  return expanded ? "expanded" : "collapsed";
}

/**
 * Opens the child `key` in the children's section, which a child's tag on its row of the outline
 * does (R9, `NodeRow.tsx`): the section opens, lists the page of children that holds the child,
 * and puts the focus on the child's entry, on its key. A child the section does not list leaves
 * the focus on the section's heading control. Where the page draws no section, nothing happens.
 */
export function openChildEntry(key: string): void {
  document
    .querySelector<HTMLElement>(CHILDREN_SECTION)
    ?.dispatchEvent(new CustomEvent<string>(CHILD_ENTRY_EVENT, { detail: key }));
}

/**
 * The issue's children, a section at the top of the body above the outline (R14), which the
 * stage draws once, in Conversation, in each filter and in Graph - the views that draw an outline
 * read (`outlineOf`) - and in no other view. An issue with no children draws no section.
 *
 * It opens expanded. Open, it lists each child of the issue's own read (`IssueDetails.children`),
 * in that read's order: its key, a link to its own page (`ChildIssueTag`), its title, and how it
 * stands, what blocks it and who holds it (`ChildIssueFacts`). It lists 20 at a time, with a
 * control after them that says how many are left and lists the next 20. Its header holds its one
 * control, which opens and closes the list, and beside it the header's bar and its two labelled
 * counts, the direct children done of total and the whole subtree done of total
 * (`IssuePropertiesRow.tsx`' `ChildrenProgress`), which stay while the list is closed. The
 * reader's choice, open or closed, is a preference of their own browser (`issue.children-section`),
 * so the next issue they open draws the section the same way. The outline still draws each child
 * as a row where the tree places it. Each link of the section is in the tab order, as no tree's
 * keys reach it.
 *
 * The children's address (`/children`) and its chord (`t h`) put the focus on the section's
 * control, and a child's tag on its row of the outline opens the child here (`openChildEntry`).
 */
export function ChildrenSection({ issue, route }: SlotProps): ReactNode {
  const childIssues = issue.children ?? [];
  const preset = presetOf(route);
  // The reader's saved fold of the section is theirs, kept under their login
  // (`useUserPreference`), so the section is drawn once the login has settled, open or folded as
  // they saved it: drawn before, it was drawn open and then folded under the rows' feet as the
  // login answered (R3, "The places held" row 4). The login is the shell's one read, kept fresh as
  // the tree keeps it (`EVENT_FRESH`), so this costs no request.
  const loginOut = useQuery({ ...whoAmIQuery(), ...EVENT_FRESH }).isPending;
  if (childIssues.length === 0 || outlineOf(preset) === undefined || loginOut) return null;
  // Keyed on the issue, so the pages the reader opened and a child's opening are the issue's own.
  return <ChildrenList childIssues={childIssues} key={issue.key} route={route} />;
}

function ChildrenList({
  childIssues,
  route,
}: {
  childIssues: readonly IssueChild[];
  route: IssueRoute;
}): ReactNode {
  const { hash, search } = useLocation();
  const following = useFollowing();
  const [expanded, setExpanded] = useUserPreference(
    "issue.children-section",
    readExpanded,
    writeExpanded
  );
  // A child's tag opens the section over the reader's kept choice, for this drawing of it, and
  // keeps nothing: the choice is the one their own control makes.
  const [opened, setOpened] = useState(false);
  const open = expanded || opened;
  // How many of the children the section lists: whole pages of `CHILDREN_PAGE` (R15).
  const [listed, setListed] = useState(CHILDREN_PAGE);
  const shown = childIssues.slice(0, listed);
  const left = childIssues.length - shown.length;
  const box = useRef<HTMLElement>(null);
  const control = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const headingId = useId();
  const listId = useId();

  // The children's own address lands on the section's control, once for each following of it
  // (`useFollowing`), in the frame after it is drawn: the app focuses its main element at a change
  // of page after this commit's effects. A focus that moved meanwhile to anything but the page
  // itself or an element holding the section - a script's - keeps it.
  const lands = issueAddress(route, search, hash, undefined).target?.kind === "children";
  const landed = useRef<string | undefined>(undefined);
  useEffect(() => {
    const heading = control.current;
    if (!lands || heading === null || landed.current === following) return;
    landed.current = following;
    const from = document.activeElement;
    requestAnimationFrame(() => {
      if (!heading.isConnected) return;
      const now = document.activeElement;
      if (now !== from && !focusOnDocument() && now?.contains(heading) !== true) return;
      focusOn(heading);
    });
  }, [following, lands]);

  // A child's tag asks for its child (`openChildEntry`): the section opens and lists the page that
  // holds the child, in the same task, so the child's key is drawn when the focus goes to it.
  const children = useRef(childIssues);
  children.current = childIssues;
  useEffect(() => {
    const section = box.current;
    if (section === null) return;
    const openAt = (event: Event) => {
      if (!(event instanceof CustomEvent) || typeof event.detail !== "string") return;
      const key = event.detail;
      const at = children.current.findIndex((child) => child.key === key);
      flushSync(() => {
        setOpened(true);
        if (at !== -1) {
          const page = Math.floor(at / CHILDREN_PAGE) + 1;
          setListed((count) => Math.max(count, page * CHILDREN_PAGE));
        }
      });
      focusOn(entryKey(list.current, key) ?? control.current);
    };
    section.addEventListener(CHILD_ENTRY_EVENT, openAt);
    return () => section.removeEventListener(CHILD_ENTRY_EVENT, openAt);
  }, []);

  return (
    <section
      aria-labelledby={headingId}
      className="mx-auto mb-4 flex w-full max-w-[1000px] min-w-0 flex-col"
      data-children-section=""
      ref={box}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1.5 text-xs leading-6">
        <h2 className="text-xs leading-6 font-semibold" id={headingId}>
          <button
            aria-controls={listId}
            aria-expanded={open}
            className={`inline-flex min-h-6 items-center gap-1.5 font-semibold ${pageInkText}`}
            data-children-toggle=""
            onClick={() => {
              setOpened(false);
              setExpanded(!open);
            }}
            ref={control}
            type="button"
            {...pageChrome}
          >
            <span aria-hidden="true" className={`inline-block w-3 text-center ${pageQuietText}`}>
              {open ? "−" : "+"}
            </span>
            <span>{CONTROL_LABELS.children_section}</span>
          </button>
        </h2>
        <ChildrenProgress childIssues={childIssues} />
      </div>
      <ul className="mt-1 flex min-w-0 flex-col" hidden={!open} id={listId} ref={list}>
        {shown.map((child) => (
          // The entry paints its first line's text itself: its key and its title flow inline in
          // it, so it carries the row's identifier (`RowText.tsx`'s `ROW_TIMING`).
          <li
            className={`min-w-0 text-[15px] leading-6 break-words ${pageWordsText}`}
            data-child-entry={child.key}
            key={child.key}
            {...rowTiming}
          >
            <ChildIssueTag child={child} tabStops /> <span {...authorText}>{child.title}</span>
            <ChildIssueFacts child={child} tabStops />
          </li>
        ))}
        {left === 0 ? null : (
          <li className="text-xs leading-7">
            {/* A press replaces the control with the children it lists, so it takes no focus
                from that press: a pointer's leaves the focus where it is, and a press of the
                control while it holds the focus - Enter, Space - leaves it on the first child it
                listed, rather than on the page as the control moves down past them. */}
            <button
              className={`dispatch-inline-link font-medium underline decoration-page-hair underline-offset-[3px] ${pageQuietText}`}
              data-children-more=""
              onClick={(event) => {
                const first = childIssues[shown.length]?.key;
                const held = event.currentTarget === document.activeElement;
                flushSync(() => setListed((count) => count + CHILDREN_PAGE));
                if (held) focusOn(entryKey(list.current, first));
              }}
              onMouseDown={(event) => event.preventDefault()}
              type="button"
              {...pageChrome}
            >
              {/* One run of text: below 1280 px a button is a flex box (`styles.css`), which
                  would drop the space that opens the words after the count. */}
              <span>
                <span {...authorText}>{left.toLocaleString()}</span>{" "}
                {left === 1
                  ? CONTROL_LABELS.children_page_child
                  : CONTROL_LABELS.children_page_children}
              </span>
            </button>
          </li>
        )}
      </ul>
    </section>
  );
}

/** The key of the child `key`'s entry in `list`, the link it takes the focus on; undefined where
 *  the list draws no entry for it. */
function entryKey(list: HTMLElement | null, key: string | undefined): HTMLElement | undefined {
  if (list === null || key === undefined) return undefined;
  return (
    list.querySelector<HTMLElement>(`[data-child-entry="${CSS.escape(key)}"] [data-child-key]`) ??
    undefined
  );
}

/** Puts the focus on `target` without scrolling to it, and brings it on screen by the page's one
 *  rule (`bringOnScreen`), clear of the band at the top and the foot. */
function focusOn(target: HTMLElement | null | undefined): void {
  if (target === null || target === undefined) return;
  target.focus({ preventScroll: true });
  bringOnScreen(target);
}
