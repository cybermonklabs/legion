---
title: Keyboard shortcuts
description: Every keyboard shortcut in Dispatch, by page, plus the ? help and the search palette.
sidebar:
  order: 5
---

Dispatch can be driven from the keyboard. Press `?` on any page to see the shortcuts that page
offers.

`Ctrl` below means `⌘` on a Mac, and `Alt` means `⌥` (Option). A shortcut written `g i` is a
sequence: press `g`, let go, then press `i` within a second. While you wait, the keys pressed so far
show in the bottom right corner.

While you type in a text field, only `Ctrl+K`, `Escape`, and `Ctrl+Alt+M` in a document work as
shortcuts, so single letters never interrupt your typing.

## The help and the palette

![The Keyboard shortcuts help, grouped by page](/legion/media/dispatch/shortcuts.png)

`?` opens **Keyboard shortcuts**: every shortcut the current page offers, grouped by where it
applies. Type in its filter box to narrow the list. A shortcut that would do nothing right now,
such as one that needs a focused row, is greyed out.

`Ctrl+K` opens the palette. With an empty box it lists the actions for the page you are on, each
with the keys that also run it. Type to filter those actions and to search all of Dispatch. Use
the arrow keys and `Enter` to pick a row. Some actions are only in the palette, because a single
key would be too easy to press by mistake: **Close issue**, **Reopen issue**, and
**Set priority P0** to **P3**.

`/` opens the palette for search alone, with your last search selected so that typing replaces
it. `g p` opens it as a project list.

## Global

These work on every page.

| Keys | Action |
| --- | --- |
| `Ctrl+K` | Search and actions |
| `/` | Search only |
| `?` | Keyboard shortcuts |
| `c` | Create issue |
| `g i` | Go to Inbox |
| `i` | Open the Inbox as a drawer over the current page (not offered on the Inbox page itself) |
| `g a` | Go to Agents |
| `g s` | Go to Settings |
| `g d` | Go to the current project's Documents (on a project or issue page) |
| `g p` | Go to project… |
| `Shift+S` | Toggle the sidebar (wide screens) |
| `Shift+M` | Toggle the margin (wide screens, on a page with a document) |

## Inbox

From 1280 px the Inbox page is two columns, and these keys act on the **selected** row: the one
the right column is showing. Below that width, and in the Inbox drawer, they act on the
**focused** row.

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous row |
| `↓` / `↑` | Next / previous row, while the focus is in the list (below 1280 px, while a row is focused) |
| `l` | Move the focus into the reading pane, which the arrow keys, Page Up, Page Down, Space, Home and End then scroll (nothing below 1280 px, in the drawer, or with no row selected) |
| `Enter` | Answer the selected ask (moves into its answer box, opening the note field of a decision block first) |
| `1` – `9` | Pick option 1–9 of the selected ask |
| `o` | Open the row's issue, document or thread |
| `y` | Copy the selected row's reference |
| `x` | Tick or untick the selected ask, for a snooze of several at once |
| `h` | Snooze the selected ask, or every ticked ask |
| `f` | Move the focus to the **Search asks** box above the list |
| `Escape` | From the search box, to the selected row, or the first row where none is selected; from the right column, back to the row `l` was pressed on, else the row it is showing; then back to the row, then off it; from the bulk snooze menu, back to the list; with no row focused, drop every tick |

`Enter`, `1` – `9`, `x` and `h` are an open question's: a thread row has no options, no answer
box and no snooze, so they do nothing on one. An agent-message row has no `dispatch://`
reference, so `y` does nothing on one either.

## Project

| Keys | Action |
| --- | --- |
| `v` | Toggle List / Board (on the Issues tab) |

On the List table, `j` and `k` follow the current sorted, filtered page, skip collapsed groups, and scroll virtual rows into view:

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous issue |
| `o` | Open issue |
| `Enter` | Open the focused issue (from the row) |
| `x` | Select or deselect the focused issue |

## Board

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous card |
| `l` / `h` | Next / previous column |
| `↓` / `↑` | Next / previous card, while one is focused |
| `→` / `←` | Next / previous column, while one is focused |
| `Shift+J` / `Shift+K` | Move card down / up |
| `Shift+L` / `Shift+H` | Move card to the next / previous status |
| `o` | Open issue |
| `Enter` | Open the focused card's issue (from the card) |
| `p` | Focus the card's priority |
| `y` | Copy the card's issue reference |
| `Shift+Y` | Copy the card's issue key |
| `Escape` | Back to the card, then off it |

## Architecture

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous component |
| `↓` / `↑` | Next / previous component, while one is focused |
| `Enter` or `o` | Open the focused component: its children, else its details |
| `l` or `→` | Into the focused component's children |
| `h` or `←` | Up one level |
| `Escape` | Back to the component row, then off it |

## Issue

| Keys | Action |
| --- | --- |
| `s` | Focus the status |
| `p` | Focus the priority |
| `0` – `3` | Set priority P0–P3 |
| `l` | Edit labels |
| `e` | Edit the title |
| `Shift+P` | Pin or unpin the issue |
| `t t` | Open the Tree preset |
| `t s` | Open the Spec preset |
| `t k` | Open the Checks preset |
| `t p` | Open the Plan preset |
| `t d` | Open the Decisions preset |
| `t c` | Open the conversation in the panel |
| `t v` | Open the issue's history under the tree |
| `t h` | Open the issue's children |
| `t a` | Open the Files and documents menu |
| `t f` | Filter the preset |
| `Escape` | Back out of the focused status or priority |
| `Enter` on a file in **Files and documents** | Open it in place of the tree |
| `Escape` in a document opened in place of the tree | Leave its text, then close it and go back to the tree |

`0` – `3` and `Shift+P` do nothing while focus is in an ask's card or on a checkbox or option,
where the key belongs to that control.

Each `t` key but `t f` opens an address of the issue page and puts the focus in what it opens.
`t f` puts the focus in the **Filter** box. While a preset has the focus, `←` and `→` move to the
previous and next preset, and `Home` and `End` to the first and last. At 1024 px and below the
presets are one menu instead.

## Issue tree

These keys move through the issue page's tree, where each node is a row.

| Keys | Action |
| --- | --- |
| `Tab` / `Shift+Tab` | Move into the tree (from the page itself) |
| `j` / `↓` | Move to the next row |
| `k` / `↑` | Move to the previous row |
| `Home` | Move to the first row |
| `End` | Move to the last row |
| `→` | Unfold the focused row, or move into it |
| `←` | Fold the focused row, or move to its parent |
| `Enter` | Put the caret in the focused row |
| `Escape` in a row's text | Leave the row's text |
| `Escape` | Leave the control, then the tree |
| `o` | Keep the focused row's body open |
| `f` | Move to the next link on the focused row |
| `Shift+K` | Keep the focused decision |
| `Shift+C` | Change the focused decision |
| `.` | Zoom into the focused row |
| `,` | Zoom back out one level |
| `q` | Move to the next open question |
| `w` | Move into the decision behind the focused row |
| `z` | Cycle how much of the tree is drawn |

The tree is one stop of `Tab`: `Tab` from a row leaves the tree. `Home` and `End` move through
the tree only while the focus is in it, and scroll the page everywhere else. `j`, `k`, `↓`, `↑`,
`,`, `q`, `w` and `z` also work from the page itself, but not from a control elsewhere on the
page. `Shift+K` and `Shift+C` work only while the focused row shows **Keep** or **Change**. While
the caret is in a row's text, the tree's keys stop, and `Escape` takes you back to the row.

On a check, `w` moves to the decision behind the requirement the check serves. No link on the
issue page opens its preview card for a resting pointer: the card opens when `Tab` focuses the
link, or when the caret stands in a link of a document you are editing.

At 1024 px and below the tree shows one node at a time, and there `→` and `←` zoom into the
focused row and back out to its parent.

`z` steps through three views: each row's first line; those lines with every body down to the
second level open; and every branch unfolded with every body open. Your own folds and pins stay as
they were. A key that has nowhere to go says so in a line under the header, such as **No further
open question.**

## Document

These work in an issue's documents and in a project's documents.

| Keys | Action |
| --- | --- |
| `Ctrl+Alt+M` | Comment on the selected text |

`Ctrl+Alt+M` is the shortcut Google Docs uses. It does what the selection bar's **Comment** does:
it opens a comment on the text you selected and puts the cursor in it. It works while the cursor is
in the document and text is selected. With nothing selected, in a comment you are writing, or on a
document you cannot edit, it does nothing.

## Agents

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous agent |
| `Enter` | Message the focused agent |
| `i` | Pick an issue for the message |
| `Shift+P` | Pin or unpin the focused agent |
| `x` | Select or deselect the focused agent |
| `Escape` | Close the issue picker, or go back to the agent row |

## Dialogs

| Keys | Action |
| --- | --- |
| `Escape` | Close the dialog |
| `Ctrl+K` | Close the search palette |

While a dialog is open, only its own keys work.

## Writing

These keys belong to the message boxes rather than to a page, so `?` does not list them. The live
view's message box is the exception to the `Enter` row only: there `Enter` sends, as at the agent's
terminal, `Shift+Enter` starts a new line, and `Ctrl+Enter` sends as everywhere.

| Keys | Action |
| --- | --- |
| `Ctrl+Enter` | Send |
| `Enter` | New line |
| `Enter` in the live view's message box | Send |
| `Shift+Enter` in the live view's message box | New line |
| After `/` in the live view's Send or Aside box | Arrow keys choose a command; `Tab` or `Enter` picks it; `Escape` closes the list |
| `Ctrl+K` | In a new comment on an issue, open the reference picker to insert a `dispatch://` link |
