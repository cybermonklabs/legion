#!/usr/bin/env python3
"""Count coordination instances in this deployment's Dispatch record, over a fixed window.

Input: window.json beside this file, $DISPATCH_URL, and the bearer token in the file
$DISPATCH_TOKEN_FILE names.  The token is never printed.
Output: one JSON object on stdout.
Run, from the repository root, as the committed evidence/count.json was written:

  python3 docs/research/internet-of-cognition/coordination/count.py \
      >docs/research/internet-of-cognition/evidence/count.json

It took 721 s on 2026-11-18; on 2026-11-24, reading every handoff too, it took 396 s.

What it measures
----------------
A record read is one event row of the window's project, written inside the window, that
carries text a party wrote.  An instance is one such record whose text matches any of three
patterns: a `dispatch://` reference, a forty-character commit, or a quoted span attributed
to a named author (the expressions are PAT_REF, PAT_SHA and PAT_QUOTE below).  Each record
type has its own denominator, the rows of that type read in the window.

A class is assigned to an instance from the row's `actor` and from the issue tree of the
record its text cites, never from prose.  The three classes overlap, so they are applied in a
fixed order and the first that holds wins:

  1. between an agent and a person: the actor's kind differs from the cited record's
     author kind (`user` is a person, every other actor kind is an agent);
  2. inside one tree: the cited record's issue and the acting row's issue have the same
     root, found by walking `parent` (`child_of`) to the root;
  3. across sessions: the actor is a session.  The rule above has already taken every
     instance whose cited record is in the acting row's own tree, that row's own issue
     among them, so what reaches this rule cites another tree.

An instance whose text cites no `dispatch://` reference to an issue carries no cited record,
so none of the three classes can be decided for it; it is counted apart.  So is an instance
whose cited issue the deployment did not hold at the window's end, and one whose cited
record's author the feed does not name.  Where a text cites more than one reference, the
first by position decides the class.

The cited record's author kind is read from the feed, never from prose: a `/comment/<id>`,
`/ask/<id>` or `/message/<id>` reference from the `comment.created`, `message.created` or
`ask.opened` row that created that record (the reference may carry a prefix of the uuid,
which is resolved when it is unique); a `/artifact/<slug>` reference from the authors of that
document's first version; and a bare issue reference or a `/spec` reference from the issue's
creator.

Beside the classes, every count the output carries (the top-level one, each type of
`written_acts_r4`, `all_bodied_records`, the two lines of documents and `handoffs`) holds
`by_cited_author`: who cited whose record, whatever the tree, read from the acting row's
`actor`, its kind and its id, and the cited record's author, its kind and its id, the id
read from the same row as the kind (of a document's authors, the first).  It is no class
and is not applied in the fixed order: it reads the instances whose cited issue is held and
whose cited record's author the feed names, which are the ones the three classes and
`unclassed.other` hold, so its figures sum to those four.  Its keys are

  session_cites_another_session   a session cites a record a different session wrote (the
                                  actor's id differs from the author's id);
  session_cites_its_own           a session cites a record it wrote itself (the two ids are
                                  the same);
  session_cites_a_person          a session cites a record a person wrote;
  person_cites_a_session          a person cites a record a session wrote;
  person_cites_a_person           a person cites a record a person wrote;
  other                           the actor or the author is of another kind, such as
                                  `system`, or a session cites a session and the feed names
                                  no id for one of them.

So `session_cites_another_session` is coordination that crosses a session boundary,
whether the cited record is in the acting row's own tree or in another; the class
`across_sessions`, read only after `inside_one_tree`, counts only those in another tree.

The record types read, and where each keeps its text
----------------------------------------------------
  comment.created, message.created   payload.body.  The top-level fields of the output
                                     (`denominator_records_read`, `instances`, `by_pattern`,
                                     `classes`, `unclassed`, `by_cited_author`) count these
                                     two alone, as the first version of this count did.
  message.answered                   payload.body: the reply's own text.  A reply is not a
                                     message.created row; payload.reply_body is a copy of
                                     the message it answers and is not read.
  ask.opened                         payload.question, then each payload.options[] label
                                     and description: the asker's words.
  ask.answered                       payload.answer.text: the answerer's own words.
                                     payload.answer.selected repeats option labels the asker
                                     wrote, so it is not read.
  ask.resolved                       payload.resolution.reason: the resolver's own words.
  ask.edited                         payload.question and payload.options[], counted only for
                                     what is new against payload.previous, because a
                                     rewording repeats the ask it rewords.
`written_acts_r4` holds these five types, each with its own denominator, and
`all_bodied_records` sums them with comments and messages.

Documents are a line of their own.  `artifact_versions` reads every `artifact.version` row
of a document (an artifact whose version has no `mime`) through
GET /api/v1/artifacts/<id>/versions/<n>, beside the version before it, and counts only the
references, commits and quotations that are new against that previous version, so a
reference a document keeps across forty versions counts once, in the version that wrote it.
A version is an instance when any of the three is new in it, and its cited record is the
first new reference by position.  `artifact_first_versions` is the same line for the first
version of each document created in the window (`artifact.created`), every pattern of which
is new.  The feed's own `artifact.version` payload cannot stand in for these reads: it
carries a `diff` only for a named version, and cuts it at about 4 KB.

Handoff bodies are not in the feed (a `handoff.recorded` row carries only `handoff_id`,
`phase` and `version`), so `handoffs` reads the handoff of every such row of the window
whole, through GET /api/v1/handoffs/<id>, matches it against the same three patterns and
classes it as above, its actor the row's `actor`, the session that recorded it.  Its
denominator is every `handoff.recorded` row: a read that fails ends the run, naming it.

The record types left out, and why
----------------------------------
  ask.handed_back                    the payload is the ask as it stands (question, options,
                                     state); it carries no text of the party that hands it
                                     back.
  ask.follower_added, ask.follower_removed
                                     the payload is `by`, `ask_id` and `session_id`: no text.
  comment.resolved, comment.anchor_refreshed
                                     the payload carries the body of a comment that
                                     comment.created already counted.
  comment.edited                     an edit of a comment comment.created counted, with no
                                     previous body in the payload to tell what is new.
  artifact.human_edited              a person's edit; its text is the artifact.version row it
                                     settles into, which the document line reads.
  uploaded files                     an artifact version with a `mime` is an uploaded file
                                     (an archive, a log, an image), not a document; their
                                     rows are counted in `file_versions_not_read` and
                                     `files_not_read`, and not read.
Every other event type carries no text a party wrote: a status, a claim, a result, a review,
a setting or a delivery.

Fixed in time
-------------
Everything a class depends on is read from the rows of the feed written before the window's
`to`, so a run taken later gives the same classes.  The cut is by each row's `created_at`,
not by its id, because the feed's ids are not in `created_at` order.

  * The tree.  An issue's parent is the one its `issue.created` row names, then every
    `child.added` and `child.removed` (written on the parent's issue, naming `child_key`),
    in feed order.  An issue is held when its `issue.created` row was written before `to`.
    No event type deletes or moves an issue.  The same map follows from the last
    `issue.created`, `issue.updated` or `issue.closed` row of each issue, since each carries
    the whole issue, `parent` included.
  * The authors.  The author maps above read only rows written before `to`, so a later
    record whose uuid shares a cited prefix, or a later document under a cited slug, cannot
    move a cited author.

What still reads live, each read by id: the handoffs (each `handoff.recorded` row's
`handoff_id`, read whole through GET /api/v1/handoffs/<id>; a handoff the deployment no
longer holds is a read that fails, which ends the run), and the document versions (each
through GET /api/v1/artifacts/<id>/versions/<n>; a version the deployment no longer holds is
counted in `reads.version_unreadable` and not read).

Three censuses of the same window come with the counts, because the judgement rests on them:
`event_types_in_window`, every event type the project wrote inside the window with its count,
which is what the record can hold at all; `reviews_in_window`, every `artifact.approved` and
`artifact.changes_requested` row by the document's name and the acting kind, which is who
agrees to a plan, to a spec and to a Verification document; and `results_in_window`, every
`result.recorded` row by the role it names and the acting kind, which is who says a check
passed.  `issues_resolved` counts the issues the tree walks of comments and messages visited.
"""

import hashlib
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from collections import Counter
from concurrent.futures import ThreadPoolExecutor

HERE = os.path.dirname(os.path.abspath(__file__))
WINDOW_PATH = os.path.join(HERE, "window.json")
THREADS = 24

# ---------------------------------------------------------------- the patterns

# A reference to a record Dispatch holds.
PAT_REF = re.compile(
    r"dispatch://(?:[A-Z][A-Z0-9]*-[0-9]+|[A-Z][A-Z0-9]+)(?:/[A-Za-z0-9._~@/-]*[A-Za-z0-9_~/-])?"
)
# A forty-character lower-case hexadecimal commit.
PAT_SHA = re.compile(r"(?<![0-9A-Za-z])[0-9a-f]{40}(?![0-9A-Za-z])")
# A quoted span attributed to a named author, within one paragraph.
PAT_QUOTE = re.compile(
    r"[\"\u201c\u201d][^\"\u201c\u201d]*[\"\u201d]"
    r"[\s\S]*?"
    r"(?:\u2014\s*[A-Z][A-Za-z.'-]+|said\s+[A-Z][A-Za-z.'-]+|[A-Z][A-Za-z.'-]+\s+wrote\b)"
)
PARAGRAPH = re.compile(r"\n\s*\n")

REF_ISSUE = re.compile(r"dispatch://([A-Z][A-Z0-9]*-[0-9]+)(?:/([a-z-]+)/?([^/\s]*))?")
UUIDISH = re.compile(r"^[0-9a-f-]{8,}$")


def quoted_with_author(body):
    return any(PAT_QUOTE.search(p) for p in PARAGRAPH.split(body))


def patterns_in(body, counts):
    """Count each of the three patterns `body` matches into `counts`.

    Returns the dispatch:// references it holds, in the order it writes them, and whether
    it matched any of the three, which is what makes it an instance.
    """
    refs = PAT_REF.findall(body)
    matched = False
    for name, hit in (
        ("dispatch_reference", bool(refs)),
        ("commit_sha", bool(PAT_SHA.search(body))),
        ("attributed_quote", quoted_with_author(body)),
    ):
        if hit:
            counts[name] += 1
            matched = True
    return refs, matched


# PAT_QUOTE read as its two halves, so a long paragraph is scanned once rather than once per
# quotation mark.  Its quoted span ends at the first quotation mark after the opening one,
# which must be a closing one, and its lazy middle then stops at the first place the author
# marker matches; quote_matches(p) therefore answers PAT_QUOTE.findall(p) exactly.
QUOTE_MARK = re.compile("[\"\u201c\u201d]")
AUTHOR_MARK = re.compile(
    r"(?:\u2014\s*[A-Z][A-Za-z.'-]+|said\s+[A-Z][A-Za-z.'-]+|[A-Z][A-Za-z.'-]+\s+wrote\b)"
)


def quote_matches(p):
    marks = [m.start() for m in QUOTE_MARK.finditer(p)]
    out = []
    end = 0
    author = None
    for k in range(len(marks) - 1):
        start, close = marks[k], marks[k + 1]
        if start < end or p[close] not in "\"\u201d":
            continue
        after = close + 1
        # the leftmost author marker at or after `after`; one found from an earlier point
        # still is the leftmost while it starts at or after `after`
        if author is None or author.start() < after:
            author = AUTHOR_MARK.search(p, after)
            if author is None:
                break
        out.append(p[start : author.end()])
        end = author.end()
    return out


# What each paragraph of a text holds, so a paragraph a document carries across many versions
# is matched once, keyed by its sha256 digest, 32 bytes: the text would keep every version's
# paragraphs alive, and two paragraphs may share a hash().  No match of the three patterns
# crosses a blank line (a reference and a commit hold no white space, and a quotation is read
# a paragraph at a time), so a text's matches are its paragraphs' matches in order.
PARA_CACHE = {}
NO_MATCH = ((), frozenset(), frozenset())


def paragraph_matches(p):
    hit = PARA_CACHE.get(key := hashlib.sha256(p.encode("utf-8", "surrogatepass")).digest())
    if hit is None:
        refs = tuple(PAT_REF.findall(p)) if "dispatch://" in p else ()
        shas = frozenset(PAT_SHA.findall(p))
        quotes = frozenset(quote_matches(p))
        hit = (refs, shas, quotes) if (refs or shas or quotes) else NO_MATCH
        PARA_CACHE[key] = hit
    return hit


def text_matches(text):
    """The references in body order, and the sets of commits and quotations, of `text`."""
    refs, shas, quotes = [], set(), set()
    for p in PARAGRAPH.split(text):
        r, s, q = paragraph_matches(p)
        if r:
            refs.extend(r)
        if s:
            shas |= s
        if q:
            quotes |= q
    return tuple(refs), frozenset(shas), frozenset(quotes)


def new_against(cur, prev, counts):
    """Count into `counts` each pattern `cur` holds that `prev` does not.

    Returns the new references in body order, whether anything was new, and the number of
    distinct new references, commits and quotations.
    """
    seen = set(prev[0])
    refs = [r for r in cur[0] if r not in seen]
    shas = cur[1] - prev[1]
    quotes = cur[2] - prev[2]
    matched = False
    for name, hit in (
        ("dispatch_reference", bool(refs)),
        ("commit_sha", bool(shas)),
        ("attributed_quote", bool(quotes)),
    ):
        if hit:
            counts[name] += 1
            matched = True
    return refs, matched, (len(set(refs)), len(shas), len(quotes))


def ask_text(payload):
    """An ask's own words: its question, then each option's label and description."""
    out = [payload.get("question") or ""]
    for opt in payload.get("options") or []:
        out.append(opt.get("label") or "")
        out.append(opt.get("description") or "")
    return "\n\n".join(x for x in out if x)


# ------------------------------------------------------------------- the fetch


def fetch(path, token, base):
    req = urllib.request.Request(base + path, headers={"Authorization": "Bearer " + token})
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.load(r)


def main():
    base = os.environ["DISPATCH_URL"].rstrip("/")
    token = open(os.environ["DISPATCH_TOKEN_FILE"]).read().strip()
    window = json.load(open(WINDOW_PATH))
    t_from, t_to, project = window["from"], window["to"], window["project"]

    def in_window(row):
        return t_from <= row["created_at"] < t_to

    # ---- one pass over the whole feed, ascending by id
    # each author map holds (kind, id), over rows written before `to`
    rec_author = {}     # record uuid -> its author
    art_author = {}     # "<KEY>/<slug>" -> the first author of its first version
    issue_author = {}   # issue key -> its creator
    held = set()        # issue keys with an issue.created row written before `to`
    parent_at = {}      # issue key -> its parent at `to`
    rows = []           # the window's comments and messages of the project
    handoffs = []       # the window's handoff.recorded rows: event id, handoff id, issue, actor
    types = Counter()   # every event type the project wrote inside the window
    reviews = Counter() # each review of a document, by document name and the actor's kind
    verdicts = Counter()# each recorded check result, by the role named and the actor's kind
    extra = {t: [] for t in ("message.answered", "ask.opened", "ask.answered", "ask.resolved", "ask.edited")}
    doc_versions = {}   # (artifact id, number) -> the window's artifact.version row of a document
    file_versions = Counter()
    doc_created = {}    # (artifact id, 1) -> the window's artifact.created row of a document
    file_created = Counter()
    first_id = last_id = None
    events = 0
    after = 0
    while True:
        page = fetch("/api/v1/events/log?after=%d&limit=1000" % after, token, base)
        if not page:
            break
        for row in page:
            events += 1
            payload = row.get("payload") or {}
            kind = row["type"]
            if row["created_at"] < t_to:
                if kind in ("comment.created", "message.created", "ask.opened"):
                    rid = payload.get("id")
                    author = payload.get("author") or {}
                    if rid and author.get("kind"):
                        rec_author[rid] = (author["kind"], author.get("id"))
                elif kind == "artifact.created":
                    art = payload.get("artifact") or {}
                    versions = art.get("versions") or []
                    authors = (versions[0].get("authors") if versions else None) or []
                    if art.get("ref_key") and authors:
                        art_author[art["ref_key"]] = (authors[0].get("kind"), authors[0].get("id"))
                elif kind == "issue.created" and payload.get("key"):
                    created_by = payload.get("created_by") or {}
                    if created_by.get("kind"):
                        issue_author[payload["key"]] = (created_by["kind"], created_by.get("id"))
                    held.add(payload["key"])
                    parent_at[payload["key"]] = payload.get("parent")
                elif kind in ("child.added", "child.removed") and payload.get("child_key"):
                    child_key = payload["child_key"]
                    if kind == "child.added":
                        parent_at[child_key] = row.get("issue_key")
                    elif parent_at.get(child_key) == row.get("issue_key"):
                        parent_at[child_key] = None
            if not in_window(row):
                continue
            if first_id is None:
                first_id = row["id"]
            last_id = row["id"]
            if row.get("project") != project:
                continue
            types[kind] += 1
            actor = row.get("actor") or {}
            actor_kind, actor_id = actor.get("kind"), actor.get("id")
            if kind in ("artifact.approved", "artifact.changes_requested"):
                reviews["%s %s %s" % (kind, payload.get("name"), actor_kind)] += 1
            if kind == "result.recorded":
                verdicts["%s %s" % (payload.get("role"), actor_kind)] += 1
            if kind == "handoff.recorded":
                handoffs.append((row["id"], payload.get("handoff_id"), row.get("issue_key"), actor_kind, actor_id))
            elif kind in ("comment.created", "message.created"):
                rows.append(
                    {
                        "issue_key": row.get("issue_key"),
                        "actor_kind": actor_kind,
                        "actor_id": actor_id,
                        "body": payload.get("body") or "",
                    }
                )
            elif kind in extra:
                rec = {"issue_key": row.get("issue_key"), "actor_kind": actor_kind, "actor_id": actor_id}
                if kind == "message.answered":
                    rec["body"] = payload.get("body") or ""
                elif kind == "ask.opened":
                    rec["body"] = ask_text(payload)
                elif kind == "ask.answered":
                    rec["body"] = (payload.get("answer") or {}).get("text") or ""
                elif kind == "ask.resolved":
                    rec["body"] = (payload.get("resolution") or {}).get("reason") or ""
                else:
                    rec["body"] = ask_text(payload)
                    rec["previous"] = ask_text(payload.get("previous") or {})
                extra[kind].append(rec)
            elif kind == "artifact.version":
                version = payload.get("version") or {}
                mime = version.get("mime")
                if mime is None:
                    doc_versions[(payload.get("artifact_id"), version.get("number"))] = {
                        "issue_key": row.get("issue_key"),
                        "actor_kind": actor_kind,
                        "actor_id": actor_id,
                    }
                else:
                    file_versions["text" if mime.startswith("text/") else "binary"] += 1
            elif kind == "artifact.created":
                art = payload.get("artifact") or {}
                first = (art.get("versions") or [{}])[0]
                if art.get("kind") == "doc" and first.get("mime") is None:
                    doc_created[(art.get("id"), first.get("number") or 1)] = {
                        "issue_key": row.get("issue_key"),
                        "actor_kind": actor_kind,
                        "actor_id": actor_id,
                    }
                else:
                    mime = first.get("mime") or ""
                    file_created["%s %s" % (art.get("kind"), "text" if mime.startswith("text/") else "binary")] += 1
        after = page[-1]["id"]
        if len(page) < 1000:
            break

    # a uuid prefix of eight characters or more resolves when it is unique
    by_prefix = {}
    for rid in rec_author:
        by_prefix.setdefault(rid[:8], []).append(rid)

    def author_of(ref):
        m = REF_ISSUE.match(ref)
        if not m:
            return None, None
        key, part, tail = m.group(1), m.group(2), m.group(3) or ""
        if part in ("comment", "ask", "message") and UUIDISH.match(tail):
            if tail in rec_author:
                return key, rec_author[tail]
            same = by_prefix.get(tail[:8], [])
            hit = [r for r in same if r.startswith(tail)]
            return key, (rec_author[hit[0]] if len(hit) == 1 else None)
        if part == "artifact" and tail:
            return key, art_author.get(key + "/" + tail)
        return key, issue_author.get(key)

    def instance(rec, refs):
        """One instance of `rec`, citing the first of `refs` by position that names an issue."""
        cited_key, cited_author = (None, None)
        for ref in refs:
            cited_key, cited_author = author_of(ref)
            if cited_key:
                break
        # an author is (kind, id), or None when the feed names none
        author_kind, author_id = cited_author or (None, None)
        return {
            "issue_key": rec["issue_key"],
            "actor_kind": rec["actor_kind"],
            "actor_id": rec["actor_id"],
            "cited_key": cited_key,
            "cited_author": author_kind,
            "cited_author_id": author_id,
        }

    # ---- the trees, by walking child_of to the root as it stood at the window's end
    roots = {}
    visited = set()     # every issue a walk passed through, held or not

    def root_of(key):
        seen = set()
        while key and key not in seen:
            seen.add(key)
            visited.add(key)
            if key not in held:
                return None
            if not parent_at.get(key):
                return key
            key = parent_at[key]
        return key

    def cited_author_of(inst):
        """Who cites whose record: the actor beside the cited record's author, whatever the tree."""
        actor, cited = inst["actor_kind"], inst["cited_author"]
        if actor == "session" and cited == "session":
            if not (inst["actor_id"] and inst["cited_author_id"]):
                return "other"
            if inst["actor_id"] == inst["cited_author_id"]:
                return "session_cites_its_own"
            return "session_cites_another_session"
        if actor == "session" and cited == "user":
            return "session_cites_a_person"
        if actor == "user" and cited == "session":
            return "person_cites_a_session"
        if actor == "user" and cited == "user":
            return "person_cites_a_person"
        return "other"

    def classify(insts):
        """The classes of `insts`, in a fixed order, and who cites whose record beside them."""
        for inst in insts:
            for key in (inst["issue_key"], inst["cited_key"]):
                if key and key not in roots:
                    roots[key] = root_of(key)
        classes = Counter()
        cited_by = Counter()
        for inst in insts:
            actor_is_person = inst["actor_kind"] == "user"
            cited_key = inst["cited_key"]
            if not cited_key:
                classes["no_cited_record"] += 1
                continue
            if roots.get(cited_key) is None:
                classes["cited_record_not_held"] += 1
                continue
            cited_is_person = inst["cited_author"] == "user"
            if inst["cited_author"] is None:
                classes["cited_author_unknown"] += 1
                continue
            # by_cited_author reads every instance that reaches the classes, before their order
            cited_by[cited_author_of(inst)] += 1
            if actor_is_person != cited_is_person:
                classes["between_an_agent_and_a_person"] += 1
            elif inst["issue_key"] and roots.get(inst["issue_key"]) == roots[cited_key]:
                classes["inside_one_tree"] += 1
            elif inst["actor_kind"] == "session":
                classes["across_sessions"] += 1
            else:
                classes["unclassed"] += 1
        return classes, cited_by

    def section(denominator, insts, patterns):
        cls, cited_by = classify(insts)
        return {
            "denominator_records_read": denominator,
            "instances": len(insts),
            "by_pattern": {
                "dispatch_reference": patterns["dispatch_reference"],
                "commit_sha": patterns["commit_sha"],
                "attributed_quote": patterns["attributed_quote"],
            },
            "classes": {
                "between_an_agent_and_a_person": cls["between_an_agent_and_a_person"],
                "inside_one_tree": cls["inside_one_tree"],
                "across_sessions": cls["across_sessions"],
            },
            "unclassed": {
                "no_cited_record": cls["no_cited_record"],
                "cited_record_not_held": cls["cited_record_not_held"],
                "cited_author_unknown": cls["cited_author_unknown"],
                "other": cls["unclassed"],
            },
            "by_cited_author": {
                "session_cites_another_session": cited_by["session_cites_another_session"],
                "session_cites_its_own": cited_by["session_cites_its_own"],
                "session_cites_a_person": cited_by["session_cites_a_person"],
                "person_cites_a_session": cited_by["person_cites_a_session"],
                "person_cites_a_person": cited_by["person_cites_a_person"],
                "other": cited_by["other"],
            },
        }

    # ---- comments and messages
    instances = []
    by_pattern = Counter()
    for row in rows:
        refs, matched = patterns_in(row["body"], by_pattern)
        if matched:
            instances.append(instance(row, refs))
    comments_and_messages = section(len(rows), instances, by_pattern)
    issues_resolved = len(visited)

    # ---- the handoffs: every handoff.recorded row of the window, read whole
    def handoff_body(handoff):
        # Its text is handoff_text's, below.  A read that fails ends the run, naming the
        # handoff and its row, so no handoff of the window leaves the denominator.
        event_id, hid = handoff[:2]
        try:
            body = fetch("/api/v1/handoffs/" + hid, token, base).get("body")
        except Exception as err:
            raise RuntimeError("the read of handoff %s, of event %s, failed: %s" % (hid, event_id, err)) from err
        return handoff_text(body)

    # each handoff in feed order, its text read beside its row's issue and actor
    hand = Counter()
    hand_insts = []
    with ThreadPoolExecutor(THREADS) as pool:
        for (_, _, issue_key, actor_kind, actor_id), body in zip(handoffs, pool.map(handoff_body, handoffs)):
            refs, matched = patterns_in(body, hand)
            if matched:
                hand_insts.append(instance({"issue_key": issue_key, "actor_kind": actor_kind, "actor_id": actor_id}, refs))
    handoffs_out = section(len(handoffs), hand_insts, hand)

    # ---- the other written acts, each type with its own denominator
    extra_out = {}
    all_insts = list(instances)
    all_pat = Counter(by_pattern)
    for kind, recs in extra.items():
        pats = Counter()
        insts = []
        for rec in recs:
            if kind == "ask.edited":
                refs, matched, _n = new_against(
                    text_matches(rec["body"]), text_matches(rec["previous"]), pats
                )
            else:
                refs, matched = patterns_in(rec["body"], pats)
            if matched:
                insts.append(instance(rec, refs))
        out = {"records_with_text": sum(1 for rec in recs if rec["body"].strip())}
        out.update(section(len(recs), insts, pats))
        extra_out[kind] = out
        all_insts += insts
        all_pat.update(pats)
    all_out = section(len(rows) + sum(len(v) for v in extra.values()), all_insts, all_pat)
    all_out["types"] = ["comment.created", "message.created"] + list(extra)

    # ---- the documents: each version against the one before it
    def version_matches(key):
        """The matches of one document version; None when the deployment does not hold it."""
        aid, number = key
        path = "/api/v1/artifacts/%s/versions/%d" % (aid, number)
        for attempt in range(3):
            try:
                return text_matches(fetch(path, token, base).get("markdown") or "")
            except urllib.error.HTTPError as err:
                if err.code == 404:
                    return None
                if err.code < 500 or attempt == 2:
                    raise
            except (urllib.error.URLError, TimeoutError, ConnectionError):
                if attempt == 2:
                    raise
            time.sleep(2)

    counted = dict(doc_versions)
    for key, meta in doc_created.items():
        if key not in counted:
            counted[key] = dict(meta, first=True)
    need = set(counted)
    for aid, number in doc_versions:
        if number and number > 1:
            need.add((aid, number - 1))
    # in order by document and number, so each version comes right after the one before it
    tasks = sorted(need, key=lambda k: (k[0] or "", k[1] or 0))

    # each line: pattern counts, instances, distinct new items, read outcomes
    doc_line = {name: (Counter(), [], Counter(), Counter()) for name in ("version", "first")}
    prev_key = prev = None
    CHUNK = 2000
    with ThreadPoolExecutor(THREADS) as pool:
        for at in range(0, len(tasks), CHUNK):
            chunk = tasks[at : at + CHUNK]
            for key, cur in zip(chunk, pool.map(version_matches, chunk)):
                meta = counted.get(key)
                if meta is not None:
                    pats, insts, items, reads = doc_line["first" if meta.get("first") else "version"]
                    if meta.get("first"):
                        before = NO_MATCH
                    elif prev_key == (key[0], key[1] - 1):
                        before = prev
                    else:
                        before = None
                    if cur is None:
                        reads["version_unreadable"] += 1
                    elif before is None:
                        reads["previous_unreadable"] += 1
                    else:
                        reads["read_first" if meta.get("first") else "read_beside_previous"] += 1
                        refs, matched, n = new_against(cur, before, pats)
                        items["dispatch_reference"] += n[0]
                        items["commit_sha"] += n[1]
                        items["attributed_quote"] += n[2]
                        if matched:
                            insts.append(instance(meta, refs))
                prev_key, prev = key, cur

    doc_out = {}
    for name, (pats, insts, items, reads) in doc_line.items():
        denominator = len(doc_versions) if name == "version" else sum(1 for m in counted.values() if m.get("first"))
        out = section(denominator, insts, pats)
        out["new_items"] = {
            "dispatch_reference": items["dispatch_reference"],
            "commit_sha": items["commit_sha"],
            "attributed_quote": items["attributed_quote"],
        }
        out["reads"] = dict(sorted(reads.items()))
        doc_out[name] = out

    print(
        json.dumps(
            {
                "window": window,
                "feed": {
                    "events_read": events,
                    "first_event_id_in_window": first_id,
                    "last_event_id_in_window": last_id,
                },
                "event_types_in_window": dict(sorted(types.items())),
                "reviews_in_window": dict(sorted(reviews.items())),
                "results_in_window": dict(sorted(verdicts.items())),
                "denominator_records_read": comments_and_messages["denominator_records_read"],
                "instances": comments_and_messages["instances"],
                "by_pattern": dict(sorted(by_pattern.items())),
                "classes": comments_and_messages["classes"],
                "unclassed": comments_and_messages["unclassed"],
                "by_cited_author": comments_and_messages["by_cited_author"],
                "issues_resolved": issues_resolved,
                "handoffs": handoffs_out,
                "written_acts_r4": {
                    "types": extra_out,
                    "all_bodied_records": all_out,
                },
                "artifact_versions": dict(
                    {
                        "rows_in_window": types["artifact.version"],
                        "document_versions": len(doc_versions),
                        "file_versions_not_read": dict(sorted(file_versions.items())),
                    },
                    **doc_out["version"],
                ),
                "artifact_first_versions": dict(
                    {
                        "rows_in_window": types["artifact.created"],
                        "document_first_versions": sum(1 for m in counted.values() if m.get("first")),
                        "files_not_read": dict(sorted(file_created.items())),
                    },
                    **doc_out["first"],
                ),
            },
            indent=1,
            sort_keys=False,
        )
    )
    return 0


# ------------------------------------------------------------ a handoff's text
# Read by main and by handoff_quotes.py beside this file, so the two read one text.


def strings_in(value, out):
    """Append to `out` every string `value` holds, in order, however deep it stands."""
    if isinstance(value, str):
        out.append(value)
    elif isinstance(value, dict):
        for item in value.values():
            strings_in(item, out)
    elif isinstance(value, list):
        for item in value:
            strings_in(item, out)


def handoff_text(body):
    """The text of a handoff whose body is `body`.

    A handoff body is one JSON object as a string.  Its text is every string value it holds,
    joined as paragraphs, so the patterns read the written words and not the escaping of the
    JSON that carries them.  A body that is no JSON is read as it stands.
    """
    try:
        body = json.loads(body)
    except (TypeError, ValueError):
        return body if isinstance(body, str) else ""
    out = []
    strings_in(body, out)
    return "\n\n".join(out)


if __name__ == "__main__":
    sys.exit(main())
