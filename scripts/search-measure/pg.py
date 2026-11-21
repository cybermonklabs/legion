#!/usr/bin/env python3
"""The psql calls the search-measure scripts share.

Rows come back as JSON, one object per line: a document's markdown holds every character a text
format would have to escape, and `row_to_json` escapes them all.

psql is given the DSN without its password and the password in `PGPASSWORD`, set in that one
call's environment: a process's arguments are in the process list for any user of the machine to
read, and its environment is not.
"""

import json
import os
import re
import subprocess
import urllib.parse


def split_dsn(dsn):
    """The DSN without its password, and the password percent-decoded (None when it holds none).

    It splits as libpq does (fe-connect.c, conninfo_uri_parse_options): the credentials run to the
    first @ before the first /, and the password is what follows their first :, an empty one being
    none; a `password=` in the query string, its name percent-decoded, is a password too, is taken
    out with its &, and the last one wins. Every other byte of the DSN is kept as it is. It refuses,
    with a `SystemExit` that names the fault and prints nothing of the DSN, what libpq refuses in a
    password (a % that starts no %XX escape, and %00), a host whose [ has no ] or whose ] is
    followed by anything but :, /, ? or ,, and any DSN but a postgres:// or postgresql:// URL,
    since a key=value string can hold a password this does not take out. The same rules are the
    shell's packages/envoy/scripts/lib/pg-url.sh and the TypeScript packages/dispatch/e2e/pg-url.ts,
    which change with this one.
    """
    scheme, separator, rest = dsn.partition("://")
    if not separator or scheme not in ("postgres", "postgresql"):
        _refuse("it is not a postgres:// or postgresql:// URL")
    password = None
    user = ""
    location = rest
    if "@" in rest.partition("/")[0]:
        credentials, _, location = rest.partition("@")
        user = credentials + "@"
        name, colon, given = credentials.partition(":")
        if colon and given:
            password = _decode(given, "its password")
            user = name + "@"
    # The query string starts at the first ? past the hosts, read as libpq reads them: a host in
    # [ ] runs to its ], every other host and each port to a :, /, ? or the , before the next host,
    # and the database to a ?.
    tail = location
    while True:
        if tail.startswith("["):
            if "]" not in tail:
                _refuse("a host's [ has no ]")
            tail = tail.partition("]")[2]
        else:
            tail = tail[re.match(r"[^:/?,]*", tail).end():]
        if tail.startswith(":"):
            tail = tail[1:]
            tail = tail[re.match(r"[^/?,]*", tail).end():]
        if tail.startswith(","):
            tail = tail[1:]
            continue
        break
    if tail and tail[0] not in "/?":
        _refuse("a host's ] is followed by something other than :, /, ? or ,")
    if tail.startswith("/"):
        tail = tail[tail.find("?"):] if "?" in tail else ""
    head = location[: len(location) - len(tail)]
    kept = []
    removed = False
    for param in tail[1:].split("&") if tail else []:
        key, equals, value = param.partition("=")
        if equals and _valid(key) and urllib.parse.unquote(key) == "password":
            password = _decode(value, "its query string's password")
            removed = True
        else:
            kept.append(param)
    if removed:
        tail = "?" + "&".join(kept) if kept else ""
    return f"{scheme}://{user}{head}{tail}", password


def _valid(text):
    """Whether every % of text starts a %XX escape other than %00, which libpq decodes."""
    return re.search(r"%(?![0-9A-Fa-f]{2})|%00", text) is None


def _decode(text, what):
    """text percent-decoded as libpq decodes it, as the str whose fsencode is those bytes."""
    if not _valid(text):
        _refuse(f"{what} holds a % that starts no %XX escape, or %00")
    return os.fsdecode(urllib.parse.unquote_to_bytes(text))


def _refuse(fault):
    raise SystemExit(f"pg: refused a Postgres URL: {fault} (the URL is not printed)")


def run(dsn, *arguments, label="psql", limit=2000, tolerate=False):
    """psql against dsn with `ON_ERROR_STOP=1` and arguments, answering its stdout.

    A non-zero return raises `SystemExit` reading "<label>: <stderr>", unless tolerate. label is
    what that line calls this call and limit bounds the stderr it quotes (None quotes all of it):
    both are the caller's, because these lines are the harness's own failure messages. A DSN with
    no password reaches psql whole, and psql keeps the caller's own `PGPASSWORD`.
    """
    url, password = split_dsn(dsn)
    env = None if password is None else {**os.environ, "PGPASSWORD": password}
    result = subprocess.run(
        ["psql", url, "-v", "ON_ERROR_STOP=1", *arguments], capture_output=True, text=True, env=env
    )
    if result.returncode != 0 and not tolerate:
        stderr = result.stderr.strip()
        raise SystemExit(f"{label}: {stderr if limit is None else stderr[:limit]}")
    return result.stdout


def rows(dsn, sql):
    """Every row of sql as a dict.

    `select row_to_json(...)` under `-At`, not `copy ... to stdout`: COPY's text format escapes a
    backslash, which would double every escape the JSON already holds, and row_to_json escapes
    every control character itself, so each row is one line.
    """
    statement = f"select row_to_json(t)::text from ({sql}) t"
    out = run(dsn, "-q", "-At", "-c", statement)
    return [json.loads(line) for line in out.splitlines() if line]
