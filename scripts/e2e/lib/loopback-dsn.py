#!/usr/bin/env python3
"""loopback-dsn.py: print what libpq reads from a PostgreSQL DSN, or refuse the DSN.

usage:
  loopback-dsn.py NAME WHAT
  loopback-dsn.py NAME WHAT --read
  loopback-dsn.py NAME WHAT --dbname DATABASE

NAME is the environment variable that holds the DSN. WHAT is what the run does on that server,
in a few words, and a refusal that is about the host ends with it. The DSN is read from the
environment and never from the command line, so no process list holds its password.

The helper prints the host and exits 0 when libpq reads one loopback host from the DSN:
127.0.0.1, localhost or ::1. `--read` prints `host=<host>` and `dbname=<database>` instead, one
per line, which is what libpq reads of the DSN. Every other answer is a refusal: one line on
stderr that names what libpq read and why, and exit 1. It refuses a DSN with a character that
does not print, a DSN whose host, hostaddr or service holds one after libpq decodes it, a DSN
libpq cannot parse, a DSN with no host, a host list of more than one, a `hostaddr` parameter, a
`service` parameter, and a host that is not one of the three loopback names.

The caller clears PGHOSTADDR, PGSERVICE and PGSERVICEFILE before its first connection. This
helper reads the DSN alone. psql reads PGHOSTADDR, and PGSERVICE with PGSERVICEFILE name an
entry of ~/.pg_service.conf that can set hostaddr. Measured with an exported entry that sets
`hostaddr=127.0.0.2`: psql connected to 127.0.0.2, and this helper printed 127.0.0.1 for the
same DSN.

`--dbname DATABASE` prints the connection string that gives a run its own database on the host
the DSN names. It is the keywords libpq reads from the DSN, with DATABASE as dbname, written as
`dbname='…' host='…'`, the keyword/value form libpq and pgx read wherever they read a URL. The
string is rebuilt and not appended to: a `?dbname=` or `&dbname=` appended to the DSN's own text
takes its separator from a `?` that can be inside the user name, where it starts no query, and
it breaks a DSN whose text ends in a `?`. libpq reads the rebuilt string back before the helper
prints it. libpq must read DATABASE as the database, the same host as the DSN names, and every
other keyword as the DSN sets it. A refusal names the database and the host libpq read.

libpq's own parser reads the DSN (PQconninfoParse, through ctypes), because a shell pattern and
libpq disagree about which text is the host:

  postgres://localhost?sslmode=disable@evil.example/db

libpq ends the user name at the first `@`, and a `?` does not end it, so libpq reads the user
`localhost?sslmode=disable` and the host `evil.example`. A pattern that ends the authority at the
first `?` reads the host `localhost`. The run would then create and drop databases on
evil.example. libpq also decodes a percent sign in a parameter key, so `%68ost=` reaches it as
`host=`, and it reads the whole DSN, so a newline in it hides nothing.

A refusal prints the host, the hostaddr or the service name it read, and prints that value only
when every character of it prints, so a refusal is one line and writes no escape sequence to the
terminal. It prints no other part of the DSN, because the user name and the password are in it.

This helper needs libpq itself. Where ctypes.util.find_library("pq") finds none, every DSN is
refused and the refusal names libpq: there is no second, weaker check to fall back on.
"""

import ctypes
import ctypes.util
import os
import sys

USAGE = __doc__.split("\n\n", 2)[1]

#: The three host names this helper accepts.
LOOPBACK = ("127.0.0.1", "localhost", "::1")


class Refusal(Exception):
    """A DSN the run must not use. The message starts after the variable's name."""


class ConnInfoOption(ctypes.Structure):
    """libpq's PQconninfoOption, the array PQconninfoParse answers with."""

    _fields_ = [
        ("keyword", ctypes.c_char_p),
        ("envvar", ctypes.c_char_p),
        ("compiled", ctypes.c_char_p),
        ("val", ctypes.c_char_p),
        ("label", ctypes.c_char_p),
        ("dispchar", ctypes.c_char_p),
        ("dispsize", ctypes.c_int),
    ]


def libpq():
    """Return libpq, with the two functions this helper calls declared."""
    found = ctypes.util.find_library("pq")
    if found is None:
        raise Refusal(
            "cannot be read: this machine has no libpq, which is the parser this check asks"
            ' which host the DSN names (ctypes.util.find_library("pq") found none). Install the'
            " PostgreSQL client library (libpq5 on Debian and Ubuntu)"
        )
    try:
        library = ctypes.CDLL(found)
    except OSError as error:
        raise Refusal(f"cannot be read: this machine's {found} does not load: {error}") from error
    library.PQconninfoParse.restype = ctypes.POINTER(ConnInfoOption)
    library.PQconninfoParse.argtypes = [ctypes.c_char_p, ctypes.POINTER(ctypes.c_char_p)]
    library.PQconninfoFree.restype = None
    library.PQconninfoFree.argtypes = [ctypes.POINTER(ConnInfoOption)]
    return library


def parsed(library, dsn):
    """Return the keywords libpq reads from dsn, each with the value the DSN gives it.

    The parse applies no default and reads no environment variable: what comes back is what the
    DSN itself says.
    """
    message = ctypes.c_char_p()
    options = library.PQconninfoParse(dsn, ctypes.byref(message))
    if not options:
        # libpq's own message quotes the text it stopped at, which can be the password, so the
        # refusal says that libpq refused the DSN and not what it said about it.
        raise Refusal("is no connection string libpq can parse")
    read = {}
    index = 0
    while options[index].keyword:
        value = options[index].val
        if value is not None:
            read[options[index].keyword.decode()] = value.decode(errors="replace")
        index += 1
    library.PQconninfoFree(options)
    return read


def unprintable_character(text):
    """Return the code point of the first character of text that does not print, or None.

    str.isprintable() is false for the C0 controls and DEL, for the C1 controls — U+009B is the
    8-bit CSI, which a terminal reads as the start of an escape sequence — and for the
    bidirectional format characters, which reorder the text around them.
    """
    for character in text:
        if not character.isprintable():
            return ord(character)
    return None


def shown(value):
    """Return value, or what to print in its place where a character of it does not print."""
    code = unprintable_character(value)
    if code is None:
        return value
    return f"a value holding U+{code:04X}"


def quoted(value):
    """Return value as libpq reads a keyword/value: in single quotes, with a backslash before
    each backslash and each single quote of it."""
    escaped = value.replace("\\", "\\\\").replace("'", "\\'")
    return f"'{escaped}'"


def connection_string(read):
    """Return the keyword/value connection string libpq reads as the keywords read holds."""
    return " ".join(f"{keyword}={quoted(read[keyword])}" for keyword in sorted(read))


def host_and_keywords(dsn, what):
    """Return the one loopback host libpq reads from dsn and every keyword it reads, or raise
    the Refusal dsn earns."""
    code = unprintable_character(dsn)
    if code is not None:
        raise Refusal(
            f"holds U+{code:04X}, a character that does not print: libpq reads the whole DSN,"
            " and a reader that stops at the end of a line reads less of it"
        )
    read = parsed(libpq(), dsn.encode("utf-8", "surrogateescape"))
    # libpq decodes a percent sign, so a value it read can hold a character that does not print
    # where the DSN's own text holds none. These three are the values a refusal prints.
    for keyword in ("host", "hostaddr", "service"):
        code = unprintable_character(read.get(keyword, ""))
        if code is not None:
            raise Refusal(
                f"gives {keyword} U+{code:04X}, a character that does not print: libpq decodes a"
                " percent sign in the DSN, and this refusal prints no value that holds one"
            )
    if read.get("service", "") != "":
        raise Refusal(
            f"must carry no service parameter: libpq reads service={read['service']} from"
            " ~/.pg_service.conf, and that entry can set hostaddr, which sends the connection to"
            " another address"
        )
    if read.get("hostaddr", "") != "":
        raise Refusal(
            f"must carry no hostaddr parameter: libpq reads hostaddr={read['hostaddr']} and"
            " connects to that address, whatever the host name is"
        )
    host = read.get("host", "")
    if host == "":
        raise Refusal(f"names no host: libpq would read PGHOST or its own default, and {what}")
    hosts = host.split(",")
    if len(hosts) > 1:
        named = ", ".join(hosts)
        raise Refusal(
            f"must name one host: libpq reads {len(hosts)} ({named}), and it connects to the next"
            " one when the first does not answer"
        )
    if host not in LOOPBACK:
        raise Refusal(
            f"must name 127.0.0.1, localhost or ::1: libpq reads the host {host}, and {what}"
        )
    return host, read


def host_of(dsn, what):
    """Return the one loopback host libpq reads from dsn, or raise the Refusal it earns."""
    return host_and_keywords(dsn, what)[0]


def rewritten(dsn, what, database):
    """Return the connection string that names database on the host dsn names, or raise the
    Refusal it earns. libpq reads the string back: the check is libpq's answer, not the rebuild.
    """
    host, read = host_and_keywords(dsn, what)
    want = dict(read, dbname=database)
    string = connection_string(want)
    got = parsed(libpq(), string.encode("utf-8", "surrogateescape"))
    if got.get("dbname", "") != database or got.get("host", "") != host:
        raise Refusal(
            f"cannot be rewritten to name the database {shown(database)} on the host {host}:"
            f" libpq reads dbname={shown(got.get('dbname', ''))} and"
            f" host={shown(got.get('host', ''))} from the rewritten connection string"
        )
    lost = sorted(keyword for keyword in want if got.get(keyword) != want[keyword])
    if lost:
        raise Refusal(
            f"cannot be rewritten to name the database {shown(database)} on the host {host}:"
            f" the rewritten connection string loses what the DSN sets for {', '.join(lost)}"
        )
    return string


def main(argv):
    if argv[:1] in (["-h"], ["--help"]):
        print(__doc__, end="")
        return 0
    mode, database, rest = "host", "", argv
    if len(argv) == 3 and argv[2] == "--read":
        mode, rest = "read", argv[:2]
    elif len(argv) == 4 and argv[2] == "--dbname" and argv[3] != "":
        mode, database, rest = "dbname", argv[3], argv[:2]
    if len(rest) != 2:
        print(USAGE, file=sys.stderr)
        return 2
    name, what = rest
    dsn = os.environ.get(name)
    if dsn is None:
        print(f"{name} is not set", file=sys.stderr)
        return 1
    try:
        if mode == "dbname":
            print(rewritten(dsn, what, database))
        elif mode == "read":
            host, read = host_and_keywords(dsn, what)
            print(f"host={host}")
            print(f"dbname={shown(read.get('dbname', ''))}")
        else:
            print(host_of(dsn, what))
    except Refusal as refusal:
        print(f"{name} {refusal}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
