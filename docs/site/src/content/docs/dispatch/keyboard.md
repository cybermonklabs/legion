---
title: Keyboard shortcuts
description: Every keyboard shortcut in Dispatch, by page, plus the ? help, the search palette, the menus, and how to change a key.
sidebar:
  order: 5
---

Dispatch can be driven from the keyboard. Press `?` on any page to see the shortcuts that page
offers.

`Ctrl` in a key below means `⌘` (Cmd) on a Mac, not the Control key (`⌃`), `Shift` means `⇧`, and
`Alt` means `⌥` (Option). On a Mac, Dispatch draws each key with these signs. A shortcut written
`g i` is a sequence: press `g`, let go, then press `i` within a second. While you wait, the bottom
right corner shows the keys pressed so far, and under them each key that can come next, with what
it does.

While you type in a text field, or while the focus is in an open menu, list or panel, only
`Ctrl+K`, `Escape`, and in a document `Ctrl+Alt+M` and `Alt+Enter`, work as shortcuts, or the keys
you gave those actions in their place. So single letters never interrupt your typing. A focused
**Snooze**, priority or **Status** control is not a text field: a letter or a digit you press on
it works as it does on the row or the page (see [Menus and lists](#menus-and-lists)).

Of the shortcuts, only the ones that move or scroll repeat while you hold their key down: `j`, `k`
and the arrow keys that go to the next or previous row, card, column, turn, agent, component or
pull request, the Board's `h` and `l`, `Space` and `Shift+Space` in the Inbox, the Board's move
keys, `Shift+J`, `Shift+K`, `Shift+L` and `Shift+H`, where each repeat moves the card one more
time, and `Home`, `End`, `Page Up` and `Page Down`. Every other shortcut acts once for each press,
the digits included. This is also true of the palette's actions when you give them a key. A key
that does something on the page acts once for each press as well, `Enter` and `Escape` included.
If you hold such a key down, it does nothing more: a held `1` that snoozes an ask picks no option
of the ask after it, a held `Enter` presses a button once, and a held `Escape` closes one list or
panel. A held key that puts the cursor in a text field types nothing there: a held `f` puts it in
the Inbox's search box, and a held `b` or `p` opens its menu, each with nothing typed.

Every other key repeats as it does on any web page. In a text field that had the focus before you
pressed the key, typing repeats as usual: a held character, one you type with AltGr too,
`Backspace` or `Delete`, and `Enter` in a box of several lines. This is also true in a menu's
search box, where a held letter is typed again on each repeat, but a held digit in a menu still
picks its choice once. The browser's own keys repeat too: a held `Ctrl+-` or `Ctrl+=` zooms one
more step on each repeat, and a Mac text field's own keys with the Control key (`⌃`), such as
`⌃H`, act on each repeat. A held `Space` repeats where nothing took its press, and the page
scrolls by it.

You can change each key, give an action a second key, or take its keys away. See
[Change the keys](#change-the-keys).

## The help and the palette

![The Keyboard shortcuts help, grouped by page](/legion/media/dispatch/shortcuts.png)

`?` opens **Keyboard shortcuts**: every shortcut the current page offers, grouped by where it
applies. Type in its filter box to narrow the list. A shortcut that would do nothing right now,
such as one that needs a focused row, is greyed out. **Change these keys in Settings**, at its
foot, opens [Change the keys](#change-the-keys).

`?` works in the Inbox drawer too. The help opens over the drawer and lists the drawer's keys under
**Inbox**. `Escape` closes the help and leaves the drawer open.

`Ctrl+K` opens the palette. With an empty box it lists the actions for the page you are on, each
with the keys that also run it. Type to filter those actions and to search all of Dispatch. Use
the arrow keys and `Enter` to pick a row. Some actions have no key, because a single key would be
too easy to press by mistake. Run them from the palette: **Close issue**, **Reopen issue**, and
**Set priority P0** to **P3**.

Other actions have no key either: **Go to Priorities**, **Go to Delivery** and **Go to Pipeline**
on every page, and in the Inbox the actions for the selected row and the ticked rows. These
include **Assign to me**, **Hand on**, **Un-snooze**, **Mark read**, **Decline**, **Ask back**,
**Change answer**, **Skip to the answer**, **Next change**, **Select all**, and
**Dismiss, or Move to Inbox, the selected items**.

You can give each of these actions a key in [Change the keys](#change-the-keys).

`/` opens the palette for search alone, with your last search selected so that typing replaces
it. `g p` opens it as a project list.

In the palette, `Ctrl+Enter` opens the highlighted row in a new tab: a search result of any kind
(an issue, a document, a comment, an ask or a message), a project or a recent page. A `Ctrl`+click
or a `⌘`+click opens the row that you click in a new tab. The tab you are on stays as it was, and
the palette stays open on the same words. An action row has no page. A row that makes an issue
from your words, or sends them to an agent, has no page either. On these rows, `Ctrl+Enter` and a
`Ctrl`+click do nothing. The palette's foot shows this key with its other keys.

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
| `Shift+M` | Toggle the margin (wide screens, on a page with a document). On the new issue page, open or close **Details** |
| `y` on a focused link | Copy the link's address: the `dispatch://` reference of a link to a Dispatch item, else its URL |

`y` on a focused link also works in every dialog, the Inbox drawer and the margin's sheet included,
and it comes before the page's own `y`. In a document you edit, `y` types the letter.

## Inbox

From 1280 px the Inbox page is two columns, and these keys act on the **selected** row: the one
the right column is showing. Below that width, and in the Inbox drawer, they act on the
**focused** row.

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous row |
| `↓` / `↑` | Next / previous row, while the focus is in the list (below 1280 px, while a row is focused) |
| `Space` / `Shift+Space` | Scroll the right column down / up by most of its height, while the focus is in the list or on nothing. On a focused button, link, checkbox or menu control, `Space` presses that control. |
| `g g` / `Shift+G` | Scroll the right column to its top / end, while the focus is in the list or on nothing |
| `l` | Move the focus into the reading pane, which the arrow keys, Page Up, Page Down, Space, Home and End then scroll (nothing below 1280 px, in the drawer, or with no row selected) |
| `Enter` | Put the cursor in the answer box of the selected ask, from its row, with the focus on nothing, from the right column after `l`, or from an option of its card. A decision block opens its note field first. |
| `1` – `9`, `0` | Pick that option of the selected ask. `0` is the tenth option. The digit after the last option picks **Other**. |
| `a` | Approve the version that the selected approval request shows |
| `Shift+R` | Request changes on the selected approval request: open the box for your reason, with the cursor in it |
| `r` | Put the cursor in the reply box of the selected thread |
| `b` or `h` | Open the snooze menu of the selected ask, or the selection bar's snooze menu while asks are ticked |
| `p` | Open the priority menu of the selected ask's issue |
| `e` | Dismiss the selected thread (**Move to Inbox** in the Dismissed view) |
| `x` | Tick or untick the selected row |
| `o` | Open the row's issue, document or thread |
| `Shift+O` | Open the row's issue, document or thread in a new tab |
| `Shift+F` | Open the filter panel |
| `Shift+V` | Open the display menu |
| `y` | Copy the selected row's reference |
| `f` | Move the focus to the **Search asks** box above the list |
| `Escape` | From the search box: to the selected row, or to the first row when none is selected. From the right column: back to the row you pressed `l` on, else the row it is showing. From a row: off it. With no row focused: drop every tick. |

`Space`, `Shift+Space`, `g g` and `Shift+G` scroll only the right column. The focus and the
selection stay where they are. They do nothing below 1280 px and in the drawer.

In the right column, `a` and `Shift+R` do nothing until the approval request's document has
loaded, and nothing while the focus is in an ask's card there. In the Request changes box,
`Ctrl+Enter` sends the request, and `Escape` closes the box and sends nothing.

`Enter`, the digits, `b`, `h` and `p` are an ask's keys: a thread row has no options, no answer
box, no snooze and no priority, so they do nothing on one. While asks are ticked, `b` and `h` open
the selection bar's snooze menu, also from a thread row. `r` and `e` are a thread row's keys. An
agent-message row has no `dispatch://` reference, so `y` does nothing on one either.

After you snooze, dismiss, approve, request changes on or answer a row, the row after it is
selected, and it takes the keyboard focus. If no row comes after it, the row before it is
selected. If no row is left, the focus goes to the list's empty message, and the Inbox's keys still
work. If every row left is in the folded **Later** band, the focus goes to the list itself, and the
keys work there too. For ticked rows, the row after the last of them is selected. A dismiss moves
on at once. A snooze, an approval, a request for changes and an answer move on when Dispatch has
saved them.

Each control at the head of the right column shows its first key, such as `p` on the priority and
`b` on **Snooze**.

## Menus and lists

The **Snooze**, priority and **Status** controls each open a menu. The menu is a list of choices
with a search box above it. A key opens the same menu as a click: `b` or `h` for **Snooze** in the
Inbox, `p` for the priority, and `s` for the status on the issue page.

On a focused **Snooze**, priority or **Status** control, `Enter`, `Space`, `↓` and `↑` open its
menu. A letter or a digit is the page's key there, as on the row or the page: `b` or `h` opens the
snooze menu, `p` the priority menu and `s` the status menu, on the first choice or the current
value, with nothing typed in the search box. So `p` on a focused **Snooze** opens the priority
menu, and `s` on the issue page's focused priority opens the status menu.

| Keys | Action |
| --- | --- |
| `↓` / `↑`, `Home` / `End` | Move the highlight. Nothing changes yet. |
| A letter | Type in the search box. The menu shows only the choices that match. Nothing changes yet. |
| `Enter` | Choose the highlighted choice |
| `1` – `4` | In the snooze menu: **Later today**, **Tomorrow**, **Next week**, **Until I clear it** |
| `0` | In the snooze menu of a snoozed ask: **Un-snooze** |
| `0` – `3` | In the priority menu: **P0** – **P3** |
| `Escape`, `Tab` | Close the menu. Nothing changes. |

A digit chooses its choice at once, also when the search box hides that choice. The status menu
has no digits. Each choice that has a digit shows it, and the foot of each menu shows its keys.

The snooze menu opens on its first choice, and the priority and status menus on the current
value. A click on a choice chooses it. A click outside the menu closes it, and nothing changes. If
that click is on another control, the focus goes to that control.

When the menu closes, the focus goes back to where it was before the menu opened: the row, the
card or the page where you pressed the key. If you opened the menu with a click, the focus goes
back to the menu's control. In the Inbox drawer, `Escape` closes the menu, and the drawer stays
open.

On a phone, a tap opens the menu but does not put the cursor in the search box, so the phone's
keyboard does not cover the choices. Tap the search box to type in it. Each choice is at least
44 px high.

The issue page's assignee and repository controls, and an ask's **Asked of** control, open a list
with a search box. On one of these controls, an arrow key, `Home`, `End`, `Page Up` or `Page Down`
opens the list and moves its highlight, and a letter opens the list with that letter in its search
box. Neither changes the value. `Enter` or a click on a choice changes it. `Escape`, `Tab` and a
click outside close the list, and nothing changes.

## Panels

`Shift+F` opens a page's filter panel, and `Shift+V` opens its display menu. The focus goes to the
first control inside. On a panel that is already open, the key moves the focus into it and does
not close it. In the panel, `Tab` moves between the controls. On a field, `Enter` opens its list:
type to narrow the list, use the arrow keys to move, and press `Enter` to turn a value on or off.
The arrow keys move between the choices of a group and select one. `Escape` closes the open list,
then the panel. The focus then goes back to where it was before you pressed the key.

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
| `Shift+O` | Open the focused issue in a new tab |
| `x` | Select or deselect the focused issue |
| `Shift+F` | Open the filter panel |
| `Shift+V` | Open the display menu |

After you select issues, the **Selected issues** toolbar shows actions for the selection.
When the toolbar is too narrow for one line, whole actions move to the next line without moving the list rows.

`j`, `k`, `o`, `Enter` and `Shift+O` work the same way on the tables of the Priorities page and of
a priority's page. On the Priorities page, `Shift+V` opens the display menu. On a priority's page,
`Shift+F` opens the filter panel and `Shift+V` opens the display menu.

On the project's Documents tab, the list of documents takes the same row keys:

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous document |
| `o` | Open the focused document |
| `Enter` | Open the focused document (from the row) |
| `Shift+O` | Open the focused document in a new tab |

## Table columns

The tables of the List, the Priorities page, a priority's page and the Delivery page's list take
these keys on a column's grip, the thin bar at the right edge of the column's header. `Tab` goes to
each grip.

| Keys | Action |
| --- | --- |
| `→` / `←` | Make the column 10 px wider / narrower, while its grip has the focus |
| `Shift+→` / `Shift+←` | Make the column 50 px wider / narrower, while its grip has the focus |
| `Enter` | Give the column its default width, while its grip has the focus |

A column does not become narrower or wider than its limits. A double-click on the grip also gives
the column its default width. The **Move** menu in a column's header moves the column one place to
the left or to the right. The menu shows only the moves that the column can make. A pinned column
has no **Move** menu, and no move takes a column past a pinned column. After a move, the focus
stays on the menu's button. On the List, on the Priorities page's table of priorities and on a
priority's page, `j` and `k` still go to the next and previous row while a grip or a **Move** menu
has the focus. The Delivery page's list moves between rows with `↓` and `↑` only while a row has
the focus.

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
| `Shift+O` | Open the focused card's issue in a new tab |
| `p` | Open the focused card's priority menu |
| `y` | Copy the card's issue reference |
| `Shift+Y` | Copy the card's issue key |
| `Escape` | Back to the card, then off it |
| `Shift+F` | Open the filter panel |

## Architecture

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous component |
| `↓` / `↑` | Next / previous component, while one is focused |
| `Enter` or `o` | Open the focused component: its children, else its details |
| `l` or `→` | Into the focused component's children |
| `h` or `←` | Up one level |
| `Escape` | Back to the component row, then off it |
| `Shift+O` | Open the focused component's own link in a new tab |

## Delivery

On the Delivery page's list:

| Keys | Action |
| --- | --- |
| `↓` / `↑` | Next / previous pull request, while a row is focused |
| `o` or `Enter` | Open the focused pull request's details |
| `Shift+F` | Open the filter panel, in the List and in the Timeline |

## Issue

| Keys | Action |
| --- | --- |
| `s` | Open the status menu |
| `p` | Open the priority menu |
| `0` – `3` | Set priority P0–P3 |
| `l` | Edit labels |
| `e` | Edit the title |
| `Shift+P` | Pin or unpin the issue |
| `a` | Approve the spec, while its approval request waits on you |
| `Shift+R` | Request changes on the spec, while its approval request waits on you |
| `t s` | Spec tab |
| `t k` | Verification tab |
| `t p` | Plan tab |
| `t e` | Steps tab |
| `t c` | Conversation tab |
| `t h` | Children tab |
| `t a` | Artifacts tab |
| `t v` | Activity tab |
| `t d` | Decisions tab |
| `t o` | Collaborators tab |
| `]` / `[` | Next / previous tab. After the last tab `]` goes to the first, and before the first `[` goes to the last. |

`0` – `3` and `Shift+P` do nothing while focus is in an ask's card or on a checkbox or option,
where the key belongs to that control. `a` and `Shift+R` also do nothing on a turn of the
Conversation tab, in an ask's card, or in a text field.

On a keyboard layout where `[` and `]` need AltGr or Option (`⌥`), German and French among them,
the key press can carry `Alt`. Then `[` and `]` may not work. Give the two actions other keys in
[Change the keys](#change-the-keys).

### The Conversation tab

These work while the Conversation tab shows. A turn is one comment, message, ask or handoff, or
one reply of a thread you expanded.

| Keys | Action |
| --- | --- |
| `j` / `k` | Next / previous turn |
| `Enter` or `o` | Expand or collapse the focused turn's thread, or open or close its other fold, such as its earlier replies or a handoff's fields |
| `r` | Put the cursor in the reply box of the focused turn's thread |
| `y` | Copy the focused turn's `dispatch://` reference |
| `Shift+Y` | Copy the focused turn's web address |

The focus stays on the turn when `Enter` or `o` expands its thread. An open ask waits on its
answer and a handoff takes no reply, so `r` does nothing on one. A handoff has no reference, so
`y` and `Shift+Y` do nothing on one.

### The new issue page

**Settings → Preview → New issue page** shows an issue as the new page. On that page, the outline
takes these keys while the focus is on the page itself or on a row of the outline.

| Keys | Action |
| --- | --- |
| `Tab` / `Shift+Tab` | From the page itself, move into the outline, at the row you were on last |
| `j` or `↓` / `k` or `↑` | Next / previous row |
| `Home` / `End` | First / last row, while the focus is in the outline |
| `→` | Unfold the focused row, or move into it |
| `←` | Fold the focused row, or move to its parent |
| `o` | Keep the focused row's body open, or let it close again |
| `Enter` | Put the cursor in the focused row's text (from the row) |
| `Escape` | Leave the row's text. From a control in a row, go back to the row. From a row, leave the outline. |
| `f` | Move to the next link or control on the focused row |
| `Shift+K` / `Shift+C` | Keep / Change the focused decision |
| `r` | Reply to the focused row |
| `a` | Answer the focused question |
| `q` | Move to the next open question |
| `w` | Move into the decision behind the focused row |
| `.` / `,` | Zoom into the focused row / zoom back out one level |
| `z` | Change how much of the outline shows |
| `t s`, `t k`, `t p`, `t d` | Turn the Spec, Verification, Plan or Decisions filter on or off |
| `t c` | Turn every filter off |
| `t h` | Move to the issue's children |
| `Shift+M` | Open or close **Details**, at every width |

On the header, `s`, `p`, `0` – `3`, `e` and `Shift+P` work as on today's issue page. `a` and
`Shift+R` approve and request changes while the focus is on an approval chip whose request waits
on you. `l` opens **Details** at **Labels** and opens the labels' control.

The Plan and Verification filters show their own rows in place of the outline. Their rows take
the outline's keys, but `Enter` does a different thing on each filter. A key that you change for
the outline also works on these rows.

Under **Chart**, the Plan filter takes these keys while the focus is on the page itself or on a
row of the chart. Under **Steps**, the rows are the outline's rows and take all of its keys.

| Keys | Action |
| --- | --- |
| `Tab` / `Shift+Tab` | From the page itself, move into the chart, at the row you were on last |
| `j` or `↓` / `k` or `↑` | Next / previous step |
| `Home` / `End` | First / last step, while the focus is in the chart |
| `f` | Move to the next link on the focused row |
| `Escape` | From a link in a row, go back to the row. From a row, leave the chart. |
| `Enter` | Go to the focused step in the conversation (from the row) |

The steps that need each other are one row. `j`, `k` and the arrows do not stop on that row.

The Verification filter takes these keys while the focus is on a row of the filter, or on a link
or control in a row.

| Keys | Action |
| --- | --- |
| `j` or `↓` / `k` or `↑` | Next / previous row |
| `Home` / `End` | First / last row |
| `→` | From a statement, move to its first check |
| `←` | From a check, move back to its statement |
| `o` | Keep the facts of the focused row's checks open, or let them close again |
| `Enter` | Open or close the facts of the focused row's checks (from the row) |

On a row that has no checks, such as the issue's row, `Enter` and `o` do nothing.

The `t` keys are the same actions on both issue pages, so a key that you change works on both.
Settings lists each of them once, with what it does on each page, such as **Spec: open its tab
(on the new page, its filter)**. Settings lists the outline's keys under **Issue**, and **Open or
close Details** under **Global**. It also lists the `Enter` of the Plan filter and the `Enter` of
the Verification filter under **Issue**.

## Document

These work in an issue's documents and in a project's documents.

| Keys | Action |
| --- | --- |
| `Ctrl+Alt+M` | Comment on the selected text |
| `Alt+Enter` | Open the link, or the issue key or reference, at the cursor (in a document you edit) |
| `Escape` | Move the focus out of the document you edit, so the page's keys work again |

`Ctrl+Alt+M` is the shortcut Google Docs uses. It does what the selection bar's **Comment** does:
it opens a comment on the text you selected and puts the cursor in it. It works while the cursor is
in the document and text is selected. With nothing selected, in a comment you are writing, or on a
document you cannot edit, it does nothing.

Each new comment puts the cursor in the comment box, also when a comment box is open already, after
you hid the margin and showed it again, and below 1280 px, where the margin is a sheet. `Escape` in
an empty comment box, or a comment you send, puts the cursor back in the document at the text you
commented on. Below 1280 px they also close the sheet.

A plain click on a link opens it, also in a document you edit. `Alt+Enter` opens the link at the
cursor as a click does, and changes no text. In a document you cannot edit, `Tab` reaches each
link, the titles Dispatch draws for issue keys and references included, and `Enter` opens the
focused one.

After `Escape`, the next `Tab` goes back into the document, and the cursor is where you left it.

## Agents

| Keys | Action |
| --- | --- |
| `/` | Move the focus to the **Search agents** box. While the page shows that box, `/` does not open search. |
| `j` / `k` | Next / previous agent |
| `Enter` | Message the focused agent |
| `i` | Pick an issue for the message |
| `Shift+P` | Pin or unpin the focused agent |
| `x` | Select or deselect the focused agent |
| `Escape` | Close the issue picker, or go back to the agent row |
| `Shift+F` | Open the filter panel |

## Dialogs

| Keys | Action |
| --- | --- |
| `Escape` | Close the dialog |
| `Ctrl+K`, or the key you gave **Search and actions** | Close the search palette |

While a dialog is open, only its own keys work. `y` on a focused link works there too.

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
| `[[`, `@` or `#` in a writing box | Open the reference picker; arrow keys choose, `Tab` or `Enter` picks it, `Escape` closes the list |

## Change the keys

**Settings → Keyboard shortcuts** lists every action by the part of Dispatch it belongs to, with
its keys, also for pages you do not have open. Type in **Filter actions** to narrow the list. Sign
in to change a key.

- To change a key, choose it and press the new key.
- **Add key** gives the action one more key.
- **×** beside a key removes that key.
- **Reset** puts one action back on its default keys, and **Reset all** puts every action back.
  Where another action has one of those keys now, Dispatch refuses the **Reset** and names that
  action.
- An action you changed shows the keys it had before (**Changed from**), or **Added** where it had
  none.

The new key is the next key you press, with or without `Ctrl`, `Alt` or `Shift`. Press a second key
within one second to make a sequence, such as `g r`. `Escape`, `Tab` or a click elsewhere stops
without a change.

Dispatch refuses a key, and names the action that has it, when:

- another action has it on the same page, or on a part shown at the same time. Each issue page is
  shown with its documents, and a project's page with its List, Board or Architecture tab.
  Today's issue page and the new one are never shown together, so an action of one page only can
  have the key of an action of the other page only. The header's keys, `l` and the `t` keys are on
  both pages, so each of them meets the keys of both pages. On the new issue page, `a` is the key
  of **Approve** on a focused approval chip and of **Answer the focused question** on a focused
  row. Only one of the two can run at a time, so the two actions can have the same key.
- it is the first key of another action's sequence, or another action's key is its first key.
- it is a sequence that starts with `y`, the key that copies the address of a focused link.

Dispatch also refuses the digits, the arrow keys, `Enter`, `Escape`, `Tab` and `Space`, alone or
with `Shift`, which keep their own actions. It also refuses `Ctrl` with `L`, `N`, `Q`, `R`, `T`,
`W` or `Tab`, which the browser keeps. It refuses one key given twice to an action, and two keys
of one action where one is the first key of the other, such as `g` and `g i`. An action whose
keys all keep their own actions shows **Fixed**, and you cannot change it.

**Search and actions**, **Comment on selection** and **Open the link at the caret** also work
while you type in a text field. So each key you give them needs `Ctrl` with a key that a text
field does not use, or `Alt` with a key that neither types nor edits text, such as `Alt+Enter`.
Dispatch refuses them a key without `Ctrl` or `Alt`, and `Alt` with a letter, a digit or another
character, `Space`, `Backspace`, `Delete`, an arrow key, `Home`, `End`, `Page Up` or `Page Down`.
It also refuses them `Ctrl` with `V`, `C`, `X`, `Z`, `Y`, `A`, `Backspace`, `Delete`, an arrow
key, `Home` or `End`, also with `Shift` or `Alt`. A text field uses these keys to paste, copy,
cut, undo, redo and select text, to delete a word, and to move the cursor.

On a Mac, Dispatch also refuses these three actions the Control key (`⌃`) with `A`, `B`, `D`,
`E`, `F`, `H`, `K`, `N`, `O`, `P`, `T`, `V` or `Y`, also with `⇧`, `⌥` or `⌘`. A Mac text field
uses these keys to move the cursor and to edit text.

A global key that you give to a page's action runs that action on its page whenever the action can
run. Where it cannot, the key runs the global action. The list shows a note under each of the two
actions. The Agents page's `/` and `i` are two such keys already: `/` opens search while the page
shows no **Search agents** box, and `i` opens the Inbox drawer while no agent row that can pick an
issue has the focus.

Dispatch keeps your keys in this browser, for your sign-in. Every open Dispatch tab uses them at
once, and so do the `?` help, the palette, and the keys the controls show.
