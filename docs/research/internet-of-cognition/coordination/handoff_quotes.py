#!/usr/bin/env python3
"""Print what count.py's quotation pattern takes for an author in the handoffs of named events.

Input: the Dispatch address as the first argument, then one event id or more, each the id of a
`handoff.recorded` row, and the bearer token in the file $DISPATCH_TOKEN_FILE names, which is
never printed.  Output: one line on stdout, so that a row's set membership, whose candidates are
each one line, can hold it whole.  Run, from the repository root, as the row
c-handoff-quote-spans-1 runs it:

  python3 docs/research/internet-of-cognition/coordination/handoff_quotes.py \
      "$DISPATCH_URL" <event id>...

For each event it reads the row by its id, through GET /api/v1/events/log?after=<id - 1>&limit=1,
then the handoff the row names, through GET /api/v1/handoffs/<id>, and reads the handoff's text
by count.py's own rule, handoff_text.  It matches count.py's own PAT_QUOTE against each
paragraph of that text, a paragraph at a time as the count does.  For each event, in the order
given, it writes the event id and, for each match in order, the part of it the pattern's author
alternative matched, the word the match ends on, after a dash or said, or before wrote, each
written as a JSON string; the events are separated by "; ".  A read that fails
ends the run on urllib's own error, after a line naming the event; an event that is no
`handoff.recorded` row ends it too, naming the event.  Either way it exits non-zero.
"""

import json
import os
import sys

sys.dont_write_bytecode = True  # the import below writes no bytecode beside count.py
import count  # noqa: E402  count.py, beside this file


def author_parts(paragraph):
    """The part each PAT_QUOTE match of `paragraph` matched with its author alternative.

    The quoted span of a match ends at the first quotation mark after its opening one, and its
    lazy middle stops where the author alternative first matches, so that part is AUTHOR_MARK's
    leftmost match after the closing mark, which ends where the match ends.
    """
    out = []
    for match in count.PAT_QUOTE.finditer(paragraph):
        close = count.QUOTE_MARK.search(paragraph, match.start() + 1).end()
        author = count.AUTHOR_MARK.search(paragraph, close)
        if author is None or author.end() != match.end():
            sys.exit("handoff_quotes.py: the author of the match at %d is not where PAT_QUOTE ends it" % match.start())
        out.append(author.group(0))
    return out


def main():
    base = sys.argv[1].rstrip("/")
    token = open(os.environ["DISPATCH_TOKEN_FILE"]).read().strip()

    def read(path, what):
        try:
            return count.fetch(path, token, base)
        except Exception:
            print("handoff_quotes.py: the read of %s failed" % what, file=sys.stderr)
            raise

    out = []
    for word in sys.argv[2:]:
        event_id = int(word)
        page = read("/api/v1/events/log?after=%d&limit=1" % (event_id - 1), "event %d" % event_id)
        row = page[0] if page else {}
        if row.get("id") != event_id or row.get("type") != "handoff.recorded":
            sys.exit("handoff_quotes.py: event %d is no handoff.recorded row of the feed" % event_id)
        hid = (row.get("payload") or {}).get("handoff_id")
        body = read("/api/v1/handoffs/%s" % hid, "handoff %s of event %d" % (hid, event_id)).get("body")
        parts = [part for p in count.PARAGRAPH.split(count.handoff_text(body)) for part in author_parts(p)]
        out.append(" ".join([str(event_id)] + [json.dumps(part, ensure_ascii=False) for part in parts]))
    print("; ".join(out))
    return 0


if __name__ == "__main__":
    sys.exit(main())
