#!/usr/bin/env python3
"""Count coordination instances in this deployment's Dispatch record, over a fixed window.

Input: window.json beside this file, $DISPATCH_URL, and the bearer token in the file
$DISPATCH_TOKEN_FILE names.  The token is never printed.
Output: one JSON object on stdout.

What it measures
----------------
A record read is one `comment.created` or `message.created` event of the window's project
inside the window; those two rows carry the whole body in their payload, so they are the only
rows whose text can be read from the feed.  That count is the denominator of every class.

An instance is one such record whose body matches any of the three patterns below.

A class is assigned to an instance from the row's `actor` and from the issue tree of the
record its body cites, never from prose.  The three classes overlap, so they are applied in a
fixed order and the first that holds wins:

  1. between an agent and a person: the actor's kind differs from the cited record's
     author kind (`user` is a person, every other actor kind is an agent);
  2. inside one tree: the cited record's issue and the acting row's issue have the same
     root, found by walking `parent` (`child_of`) to the root through
     GET /api/v1/issues/<KEY>, each answer cached;
  3. across sessions: the actor is a session.  The rule above has already taken every
     instance whose cited record is in the acting row's own tree, that row's own issue
     among them, so what reaches this rule cites another tree.

An instance whose body cites no `dispatch://` reference to an issue carries no cited record,
so none of the three classes can be decided for it; it is counted apart.  An instance whose
cited issue the deployment does not hold is counted apart too.  Where a body cites more than
one reference, the first by position in the body decides the class.

The cited record's author kind is read from the feed, never from prose:
a `/comment/<id>`, `/ask/<id>` or `/message/<id>` reference from the row that created that
record (the reference may carry a prefix of the uuid, which is resolved when it is unique);
a `/artifact/<slug>` reference from the authors of that document's first version; and a bare
issue reference or a `/spec` reference from the issue's creator.

Handoff bodies are not in the feed (a `handoff.recorded` row carries only `handoff_id`,
`phase` and `version`), so they are read over a sample of their own: the ten issues of the
window's project with the most `handoff.recorded` rows inside the window, ordered by that
count and then by key.  Every handoff of those issues recorded inside the window is read
whole through GET /api/v1/handoffs/<id> and matched against the same three patterns; its
denominator is the handoffs read.

Three censuses of the same window come with the counts, because the judgement rests on them:
`event_types_in_window`, every event type the project wrote inside the window with its count,
which is what the record can hold at all; `reviews_in_window`, every `artifact.approved` and
`artifact.changes_requested` row by the document's name and the acting kind, which is who
agrees to a plan, to a spec and to a Verification document; and `results_in_window`, every
`result.recorded` row by the role it names and the acting kind, which is who says a check
passed.
"""

import json
import os
import re
import sys
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
    rec_author = {}     # record uuid -> author kind
    art_author = {}     # "<KEY>/<slug>" -> author kind
    issue_author = {}   # issue key -> creator kind
    rows = []           # the window's bodied records of the project
    handoffs = Counter()
    types = Counter()   # every event type the project wrote inside the window
    reviews = Counter() # each review of a document, by document name and the actor's kind
    verdicts = Counter()# each recorded check result, by the role named and the actor's kind
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
            if kind in ("comment.created", "message.created", "ask.opened"):
                rid = payload.get("id")
                author = (payload.get("author") or {}).get("kind")
                if rid and author:
                    rec_author[rid] = author
            elif kind == "artifact.created":
                art = payload.get("artifact") or {}
                versions = art.get("versions") or []
                authors = (versions[0].get("authors") if versions else None) or []
                if art.get("ref_key") and authors:
                    art_author[art["ref_key"]] = authors[0].get("kind")
            elif kind == "issue.created":
                created_by = payload.get("created_by") or {}
                if payload.get("key") and created_by.get("kind"):
                    issue_author[payload["key"]] = created_by["kind"]
            if not in_window(row):
                continue
            if first_id is None:
                first_id = row["id"]
            last_id = row["id"]
            if row.get("project") != project:
                continue
            types[kind] += 1
            if kind in ("artifact.approved", "artifact.changes_requested"):
                reviews[
                    "%s %s %s"
                    % (kind, payload.get("name"), (row.get("actor") or {}).get("kind"))
                ] += 1
            if kind == "result.recorded":
                verdicts[
                    "%s %s" % (payload.get("role"), (row.get("actor") or {}).get("kind"))
                ] += 1
            if kind == "handoff.recorded" and row.get("issue_key"):
                handoffs[row["issue_key"]] += 1
            elif kind in ("comment.created", "message.created"):
                rows.append(
                    {
                        "id": row["id"],
                        "issue_key": row.get("issue_key"),
                        "actor_kind": (row.get("actor") or {}).get("kind"),
                        "body": payload.get("body") or "",
                    }
                )
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

    # ---- the instances
    instances = []
    by_pattern = Counter()
    for row in rows:
        refs, matched = patterns_in(row["body"], by_pattern)
        if not matched:
            continue
        cited_key, cited_author = (None, None)
        for ref in refs:
            cited_key, cited_author = author_of(ref)
            if cited_key:
                break
        instances.append(
            {
                "issue_key": row["issue_key"],
                "actor_kind": row["actor_kind"],
                "cited_key": cited_key,
                "cited_author": cited_author,
            }
        )

    # ---- the trees, by walking child_of to the root
    wanted = set()
    for inst in instances:
        for key in (inst["issue_key"], inst["cited_key"]):
            if key:
                wanted.add(key)
    issue_cache = {}

    def load(key):
        try:
            issue_cache[key] = fetch("/api/v1/issues/" + key, token, base)
        except urllib.error.HTTPError as err:
            if err.code == 404:
                issue_cache[key] = None
            else:
                raise

    def resolve(keys):
        frontier = sorted(set(keys))
        while frontier:
            todo = [k for k in frontier if k not in issue_cache]
            if todo:
                with ThreadPoolExecutor(THREADS) as pool:
                    list(pool.map(load, todo))
            nxt = set()
            for key in frontier:
                issue = issue_cache.get(key)
                if issue and issue.get("parent") and issue["parent"] not in issue_cache:
                    nxt.add(issue["parent"])
            frontier = sorted(nxt)

    resolve(sorted(wanted))

    def root_of(key):
        seen = set()
        while key and key not in seen:
            seen.add(key)
            issue = issue_cache.get(key)
            if not issue:
                return None
            if not issue.get("parent"):
                return key
            key = issue["parent"]
        return key

    roots = {k: root_of(k) for k in wanted}

    # ---- the classes, in a fixed order
    classes = Counter()
    for inst in instances:
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
        if actor_is_person != cited_is_person:
            classes["between_an_agent_and_a_person"] += 1
        elif inst["issue_key"] and roots.get(inst["issue_key"]) == roots[cited_key]:
            classes["inside_one_tree"] += 1
        elif inst["actor_kind"] == "session":
            classes["across_sessions"] += 1
        else:
            classes["unclassed"] += 1

    # ---- the handoff sample, with its own denominator
    sample = [k for k, _ in sorted(handoffs.items(), key=lambda kv: (-kv[1], kv[0]))[:10]]
    sample_ids = []
    for key in sample:
        for h in fetch("/api/v1/issues/%s/handoffs" % key, token, base):
            if t_from <= h["created_at"] < t_to:
                sample_ids.append(h["id"])

    def strings_in(value, out):
        if isinstance(value, str):
            out.append(value)
        elif isinstance(value, dict):
            for item in value.values():
                strings_in(item, out)
        elif isinstance(value, list):
            for item in value:
                strings_in(item, out)

    def handoff_body(hid):
        # A handoff body is one JSON object as a string.  Its text is every string value it
        # holds, joined as paragraphs, so the patterns read the written words and not the
        # escaping of the JSON that carries them.
        body = fetch("/api/v1/handoffs/" + hid, token, base).get("body")
        try:
            body = json.loads(body)
        except (TypeError, ValueError):
            return body if isinstance(body, str) else ""
        out = []
        strings_in(body, out)
        return "\n\n".join(out)

    with ThreadPoolExecutor(THREADS) as pool:
        bodies = list(pool.map(handoff_body, sample_ids))
    hand = Counter()
    for body in bodies:
        _refs, matched = patterns_in(body, hand)
        if matched:
            hand["instances"] += 1

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
                "denominator_records_read": len(rows),
                "instances": len(instances),
                "by_pattern": dict(sorted(by_pattern.items())),
                "classes": {
                    "between_an_agent_and_a_person": classes["between_an_agent_and_a_person"],
                    "inside_one_tree": classes["inside_one_tree"],
                    "across_sessions": classes["across_sessions"],
                },
                "unclassed": {
                    "no_cited_record": classes["no_cited_record"],
                    "cited_record_not_held": classes["cited_record_not_held"],
                    "cited_author_unknown": classes["cited_author_unknown"],
                    "other": classes["unclassed"],
                },
                "issues_resolved": len(issue_cache),
                "handoff_sample": {
                    "issues": sample,
                    "chosen": "the ten issues of the project with the most handoff.recorded rows inside the window, by that count then by key",
                    "denominator_handoffs_read": len(sample_ids),
                    "instances": hand["instances"],
                    "by_pattern": {
                        "dispatch_reference": hand["dispatch_reference"],
                        "commit_sha": hand["commit_sha"],
                        "attributed_quote": hand["attributed_quote"],
                    },
                },
            },
            indent=1,
            sort_keys=False,
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
