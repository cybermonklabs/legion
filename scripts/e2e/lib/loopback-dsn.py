#!/usr/bin/env python3
"""loopback-dsn.py: print the loopback host libpq reads from a PostgreSQL DSN, or refuse the DSN.

usage:
  loopback-dsn.py NAME WHAT

NAME is the environment variable that holds the DSN. WHAT is what the run does on that server,
in a few words, and a refusal that is about the host ends with it. The DSN is read from the
environment and never from the command line, so no process list holds its password.

The helper prints the host and exits 0 when libpq reads one loopback host from the DSN:
127.0.0.1, localhost or ::1. Every other answer is a refusal: one line on stderr that names what
libpq read and why, and exit 1. It refuses a DSN with a control character, a DSN libpq cannot
parse, a DSN with no host, a host list of more than one, a `hostaddr` parameter, a `service`
parameter, and a host that is not one of the three loopback names.

libpq's own parser reads the DSN (PQconninfoParse, through ctypes), because a shell pattern and
libpq disagree about which text is the host:

  postgres://localhost?sslmode=disable@evil.example/db

libpq ends the user name at the first `@`, and a `?` does not end it, so libpq reads the user
`localhost?sslmode=disable` and the host `evil.example`. A pattern that ends the authority at the
first `?` reads the host `localhost`. The run would then create and drop databases on
evil.example. libpq also decodes a percent sign in a parameter key, so `%68ost=` reaches it as
`host=`, and it reads the whole DSN, so a newline in it hides nothing.

A refusal prints the host, the hostaddr or the service name it read. It prints no other part of
the DSN, because the user name and the password are in it.

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


def host_of(dsn, what):
    """Return the one loopback host libpq reads from dsn, or raise the Refusal it earns."""
    if dsn == "":
        raise Refusal("is empty")
    for character in dsn:
        if ord(character) < 0x20 or ord(character) == 0x7F:
            raise Refusal(
                f"holds the control character 0x{ord(character):02x}: libpq reads the whole DSN,"
                " and a reader that stops at the end of a line reads less of it"
            )
    read = parsed(libpq(), dsn.encode("utf-8", "surrogateescape"))
    if read.get("service", "") != "":
        raise Refusal(
            f"must carry no service parameter: libpq reads service={read['service']} from"
            " ~/.pg_service.conf, and that entry can name any host"
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
    return host


def main(argv):
    if argv[:1] in (["-h"], ["--help"]):
        print(__doc__, end="")
        return 0
    if len(argv) != 2:
        print(USAGE, file=sys.stderr)
        return 2
    name, what = argv
    dsn = os.environ.get(name)
    if dsn is None:
        print(f"{name} is not set", file=sys.stderr)
        return 1
    try:
        print(host_of(dsn, what))
    except Refusal as refusal:
        print(f"{name} {refusal}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
