#!/usr/bin/env python3
"""loopback-dsn.py: read a PostgreSQL DSN with libpq, and hand a command a run's own database.

usage:
  loopback-dsn.py NAME WHAT
  loopback-dsn.py NAME WHAT --dbname DATABASE [--uri | --libpq-env] --check
  loopback-dsn.py NAME WHAT --dbname DATABASE [--uri] --env VAR -- COMMAND [ARG...]
  loopback-dsn.py NAME WHAT --dbname DATABASE [--uri] --env-only VAR -- COMMAND [ARG...]
  loopback-dsn.py NAME WHAT --dbname DATABASE --libpq-env -- COMMAND [ARG...]
  loopback-dsn.py NAME WHAT --own-dbname --libpq-env --check
  loopback-dsn.py NAME WHAT --own-dbname --libpq-env -- COMMAND [ARG...]

NAME is the environment variable that holds the DSN. WHAT is what the run does on that server,
in a few words, and a refusal that is about the host ends with it. The DSN is read from the
environment and never from the command line, so no process list holds its password.

The helper prints the host and exits 0 when libpq reads one loopback host from the DSN:
127.0.0.1, localhost or ::1. Every other answer is a refusal: one line on stderr that names what
libpq read and why, and exit 1. It refuses a DSN with a character that does not print, a DSN
whose host, hostaddr or service holds a character that does not print after libpq decodes it, a
DSN libpq cannot parse, a DSN with no host, a host list of more than one, a `hostaddr`
parameter, a `service` parameter, and a host that is not one of the three loopback names. It
takes a DSN whose other keywords hold what they like: it writes none of them back, and libpq
connects with what the DSN says.

The caller clears PGHOSTADDR, PGSERVICE and PGSERVICEFILE before its first connection. This
helper reads the DSN alone. psql reads PGHOSTADDR, and PGSERVICE with PGSERVICEFILE name an
entry of ~/.pg_service.conf that can set hostaddr. Measured with an exported entry that sets
`hostaddr=127.0.0.2`: psql connected to 127.0.0.2, and this helper printed 127.0.0.1 for the
same DSN.

`--dbname DATABASE` gives a run its own database on the host the DSN names, and prints nothing
that names it: the connection string it rebuilds carries the DSN's password, and whatever reads
this helper's output would hold that password too (a shell's `$(…)` assigns it to a variable,
and that shell's xtrace prints the variable). Each of its forms runs every check below, and a
refusal is one line on stderr and exit 1, before anything runs.

  --check prints nothing and exits 0. A caller runs it where it reads the DSN, so a refused
  input ends the run before the builds.

  --env VAR -- COMMAND replaces this helper with COMMAND (execve), so COMMAND keeps the helper's
  pid, which a shell that started the helper with `&` holds in `$!`. COMMAND's environment is
  the one the helper was started with, with VAR set to the rebuilt string and without NAME. NAME
  goes because it holds the caller's DSN itself, the password and the database the caller named
  with it, which nothing the run starts may write: COMMAND is handed the run's own database
  alone. VAR may be NAME, which then holds the rebuilt string. The environment is read from
  /proc/self/environ, which holds what this process was started with: Python sets LC_CTYPE in its
  own environment when it starts under the C locale (PEP 538), and os.environ holds that, so a
  command run under os.environ would get a variable its caller never set. Where that file cannot
  be read, os.environ is the environment.

  --env-only VAR -- COMMAND hands COMMAND an environment that holds VAR and nothing else, as
  `env -i` leaves one, for a consumer the caller starts with no variable of its own: the caller
  names the rest with `env K=V… PROGRAM` as COMMAND.

  --libpq-env -- COMMAND is for a libpq client such as psql, which then needs no connection
  string. COMMAND's environment is the one --env hands over, without NAME, with the environment
  variable libpq reads each keyword of the rebuilt string from (PQconninfoOption's envvar:
  PGHOST, PGPORT, PGUSER, PGPASSWORD, PGDATABASE, PGSSLMODE and the rest), so the password
  reaches no argument list and no variable of the caller's. A keyword libpq reads from no
  variable (keepalives and fallback_application_name are two) is refused. PGSERVICE goes as well:
  libpq fills a keyword from that entry of ~/.pg_service.conf before it reads the keyword's own
  variable, so an inherited PGSERVICE would outrank the host this form hands over, where the
  connection string outranks it. Every other variable the caller left in the environment reaches
  COMMAND, and libpq reads it for a keyword the DSN does not set, as it does beside a connection
  string. libpq reads the variables back (PQconndefaults) before the helper runs COMMAND, and
  must read DATABASE as the database, the same host as the DSN names, and every other keyword as
  the DSN sets.

  --own-dbname in place of --dbname DATABASE, with --libpq-env alone, hands COMMAND the database
  the DSN itself names, for a run that works in that database rather than in one of its own
  (lib/two-builds.sh's): every check and every variable --libpq-env hands over, with the DSN's
  own dbname as PGDATABASE. It refuses a DSN that names no database, where libpq would read
  PGDATABASE or the user name in its place, and none of its refusals names the database.

COMMAND is looked up on the PATH this helper was started with, under --env-only too. A COMMAND
that cannot be found or run is one line on stderr naming it, and exit 127 or 126.

The connection string is the keywords libpq reads from the DSN, with DATABASE as dbname, written
as `dbname='…' host='…'`, the keyword/value form libpq and pgx read wherever they read a URL.
The string is rebuilt and not appended to: a `?dbname=` or `&dbname=` appended to the DSN's own
text takes its separator from a `?` that can be inside the user name, where it starts no query,
and it breaks a DSN whose text ends in a `?`. libpq reads the rebuilt string back before COMMAND
runs. libpq must read DATABASE as the database, the same host as the DSN names, and every other
keyword as the DSN sets it. Every value reaches COMMAND as libpq decoded it, so this mode also
refuses a value that holds a character that does not print: a control character is a line
break or an escape sequence wherever COMMAND's environment is written out (`env`, a log of a
server's own settings), and a byte that is no UTF-8 text is a surrogate (U+DCFF for the byte
0xFF) that the URI form cannot percent-encode. A refusal names the database, the host, and every
keyword libpq does not read back as the DSN sets it.

`--uri` after `--dbname DATABASE` writes the same keywords as a connection URI instead,
`postgres://user:password@host:port/DATABASE?key=value`, for a consumer that reads a URL: every
value percent-encoded, ::1 in brackets, and each keyword but the user, the password, the host,
the port and the database in the query, in name order. libpq reads the URI back as it reads the
keyword/value string, and this form refuses what that one refuses.

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
terminal. A refusal about another keyword names that keyword and prints no value of it. It
prints no other part of the DSN, because the user name and the password are in it.

This helper needs libpq itself. Where ctypes.util.find_library("pq") finds none, every DSN is
refused and the refusal names libpq: there is no second, weaker check to fall back on.
"""

import ctypes
import ctypes.util
import os
import re
import shutil
import sys
import urllib.parse

USAGE = __doc__.split("\n\n", 2)[1]

#: The three host names this helper accepts.
LOOPBACK = ("127.0.0.1", "localhost", "::1")

#: The variable that names an entry of ~/.pg_service.conf, which --libpq-env leaves out.
SERVICE = b"PGSERVICE"

#: What VAR must be: a name a shell can export.
VARIABLE = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")

#: --own-dbname's DATABASE: the database the DSN itself names, which the helper never prints.
OWN = object()


class Refusal(Exception):
    """A DSN the run must not use. The message starts after the variable's name."""


class ConnInfoOption(ctypes.Structure):
    """libpq's PQconninfoOption, the array PQconninfoParse and PQconndefaults answer with."""

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
    """Return libpq, with the three functions this helper calls declared."""
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
    library.PQconndefaults.restype = ctypes.POINTER(ConnInfoOption)
    library.PQconndefaults.argtypes = []
    library.PQconninfoFree.restype = None
    library.PQconninfoFree.argtypes = [ctypes.POINTER(ConnInfoOption)]
    return library


def entries(library, options):
    """Return each option of the array libpq answered with as (keyword, envvar, value), and free
    the array. envvar is None for a keyword libpq reads from no environment variable, and value
    is None for a keyword the answer gives no value. A value is decoded with
    errors="surrogateescape", which round-trips every byte, so the host this helper prints is the
    host libpq reads whatever bytes the DSN holds. A byte that is no UTF-8 text becomes a
    surrogate, U+DCFF for the byte 0xFF, which does not print: the --dbname mode refuses it with
    every other character it cannot write back, and host mode refuses it in the host, the
    hostaddr or the service alone.
    """
    found = []
    index = 0
    try:
        while options[index].keyword:
            option = options[index]
            envvar = None if option.envvar is None else option.envvar.decode()
            value = None if option.val is None else option.val.decode("utf-8", "surrogateescape")
            found.append((option.keyword.decode(), envvar, value))
            index += 1
    finally:
        library.PQconninfoFree(options)
    return found


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
    return {keyword: value for keyword, _, value in entries(library, options) if value is not None}


def variables(library):
    """Return the environment variable libpq reads each keyword from, or None for a keyword it
    reads from none: the envvar of every option of an empty connection string."""
    message = ctypes.c_char_p()
    options = library.PQconninfoParse(b"", ctypes.byref(message))
    if not options:
        raise Refusal("cannot be read: libpq parses no empty connection string")
    return {keyword: envvar for keyword, envvar, _ in entries(library, options)}


def defaults(library):
    """Return the value libpq reads for each keyword with no connection string: its environment
    variable's, else the service's or its own default."""
    options = library.PQconndefaults()
    if not options:
        raise Refusal("cannot be read: libpq answered no defaults")
    return {keyword: value for keyword, _, value in entries(library, options) if value is not None}


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


def connection_uri(read):
    """Return the connection URI libpq reads as the keywords read holds: the user and the
    password in its userinfo, the host and the port in its authority, the database as its path,
    and every other keyword in its query, each value percent-encoded."""
    rest = dict(read)

    def encoded(value):
        return urllib.parse.quote(value, safe="")

    user, password = rest.pop("user", None), rest.pop("password", None)
    userinfo = ""
    if user is not None:
        userinfo = encoded(user) + ("" if password is None else ":" + encoded(password)) + "@"
    elif password is not None:
        rest["password"] = password
    host = rest.pop("host")
    authority = f"[{host}]" if ":" in host else host
    port = rest.pop("port", None)
    if port is not None:
        authority += f":{encoded(port)}"
    path = encoded(rest.pop("dbname"))
    query = "&".join(f"{encoded(keyword)}={encoded(rest[keyword])}" for keyword in sorted(rest))
    return f"postgres://{userinfo}{authority}/{path}" + (f"?{query}" if query else "")


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


def run_keywords(dsn, what, database):
    """Return the host dsn names and the keywords that name database on it, which are every
    keyword libpq reads from dsn with database as dbname (for OWN, the dbname dsn itself sets),
    or raise the Refusal dsn earns."""
    host, read = host_and_keywords(dsn, what)
    # Every value the DSN sets reaches the command, and libpq decodes a percent sign, so a value
    # can hold a character that does not print where the DSN's own text holds none.
    for keyword in sorted(read):
        code = unprintable_character(read[keyword])
        if code is not None:
            raise Refusal(
                f"gives {keyword} U+{code:04X}, a character that does not print: every value the"
                " DSN sets reaches the command's environment, and this refusal prints no value"
                " that holds one"
            )
    if database is OWN:
        if read.get("dbname", "") == "":
            raise Refusal(
                "names no database: --own-dbname hands the command the database the DSN names,"
                " and libpq would read PGDATABASE or the user name in its place"
            )
        return host, read
    return host, dict(read, dbname=database)


def named(database):
    """What a refusal calls database: its name, or for OWN no name, which is the caller's own."""
    return "the database the DSN names" if database is OWN else f"the database {shown(database)}"


def rewritten(dsn, what, database, as_uri=False):
    """Return the connection string that names database on the host dsn names, as a keyword/value
    string or, with as_uri, as a connection URI, or raise the Refusal it earns. libpq reads the
    string back: the check is libpq's answer, not the rebuild.
    """
    host, want = run_keywords(dsn, what, database)
    string = connection_uri(want) if as_uri else connection_string(want)
    got = parsed(libpq(), string.encode("utf-8", "surrogateescape"))
    lost = sorted(keyword for keyword in want if got.get(keyword) != want[keyword])
    if lost:
        raise Refusal(
            f"cannot be rewritten to name {named(database)} on the host {host}:"
            f" the rewritten connection string loses what the DSN sets for {', '.join(lost)}"
        )
    return string


def libpq_environment(dsn, what, database):
    """Return the environment variable libpq reads each keyword that names database on the host
    dsn names from, with that keyword's value, or raise the Refusal it earns. libpq reads the
    variables back from this process's own environment, which the helper replaces next.
    """
    host, want = run_keywords(dsn, what, database)
    library = libpq()
    envvars = variables(library)
    unread = sorted(keyword for keyword in want if envvars.get(keyword) is None)
    if unread:
        raise Refusal(
            f"sets {', '.join(unread)}, which libpq reads from no environment variable:"
            " --libpq-env hands the command every keyword the DSN sets in the variable libpq"
            " reads it from"
        )
    environment = {envvars[keyword]: want[keyword] for keyword in want}
    os.environ.pop(SERVICE.decode(), None)
    os.environ.update(environment)
    got = defaults(library)
    lost = sorted(keyword for keyword in want if got.get(keyword) != want[keyword])
    if lost:
        raise Refusal(
            f"cannot be handed over to name {named(database)} on the host {host}:"
            f" libpq reads back from the environment otherwise what the DSN sets for"
            f" {', '.join(lost)}"
        )
    return environment


def started_environment():
    """Return the environment this process was started with, each name and value in bytes.

    /proc/self/environ holds it, where os.environ holds the LC_CTYPE Python sets for itself under
    the C locale. getenv reads the first of two entries of one name, and so does this.
    """
    try:
        with open("/proc/self/environ", "rb") as file:
            text = file.read()
    except OSError:
        return dict(os.environb)
    environment = {}
    for entry in text.split(b"\0"):
        name, equals, value = entry.partition(b"=")
        if equals and name:
            environment.setdefault(name, value)
    return environment


def encoded(text):
    """Return text as the bytes an environment carries, each surrogate back as its own byte."""
    return text.encode("utf-8", "surrogateescape")


def run(command, environment):
    """Replace this process with command, looked up on the PATH this helper was started with,
    and give it environment. Return the status a shell gives a command it cannot find (127) or
    cannot run (126), having said which, when the replacement fails."""
    program = command[0]
    if os.sep not in program:
        program = shutil.which(program, path=os.environ.get("PATH", os.defpath))
        if program is None:
            print(f"loopback-dsn.py: {command[0]} is no command on PATH", file=sys.stderr)
            return 127
    try:
        os.execve(program, command, environment)
    except OSError as error:
        print(f"loopback-dsn.py: cannot run {command[0]}: {error.strerror}", file=sys.stderr)
    return 126


def arguments(argv):
    """Return what argv asks for, (name, what, database, form, action, variable, command), or None
    for argv that matches no usage line. database is None for host mode and OWN for --own-dbname;
    form is "", "--uri" or "--libpq-env"; action is "check", "--env", "--env-only" or
    "--libpq-env"."""
    if len(argv) < 2:
        return None
    name, what, rest = argv[0], argv[1], argv[2:]
    if rest == []:
        return name, what, None, "", "", None, None
    if rest[0] == "--own-dbname":
        # The DSN's own database is handed over in libpq's variables alone.
        database, rest = OWN, rest[1:]
        if rest[:1] != ["--libpq-env"]:
            return None
    elif rest[0] != "--dbname" or len(rest) < 2 or rest[1] == "":
        return None
    else:
        database, rest = rest[1], rest[2:]
    form = ""
    if rest[:1] in (["--uri"], ["--libpq-env"]):
        form, rest = rest[0], rest[1:]
    if rest == ["--check"]:
        return name, what, database, form, "check", None, None
    if form == "--libpq-env":
        if len(rest) >= 2 and rest[0] == "--":
            return name, what, database, form, "--libpq-env", None, rest[1:]
        return None
    if len(rest) >= 4 and rest[0] in ("--env", "--env-only") and rest[2] == "--":
        if VARIABLE.fullmatch(rest[1]):
            return name, what, database, form, rest[0], rest[1], rest[3:]
    return None


def main(argv):
    if argv[:1] in (["-h"], ["--help"]):
        print(__doc__, end="")
        return 0
    asked = arguments(argv)
    if asked is None:
        print(USAGE, file=sys.stderr)
        return 2
    name, what, database, form, action, variable, command = asked
    dsn = os.environ.get(name)
    if dsn is None:
        print(f"{name} is not set", file=sys.stderr)
        return 1
    try:
        if database is None:
            print(host_and_keywords(dsn, what)[0])
            return 0
        if form == "--libpq-env":
            handed = libpq_environment(dsn, what, database)
        else:
            string = rewritten(dsn, what, database, form == "--uri")
            handed = {} if variable is None else {variable: string}
    except Refusal as refusal:
        print(f"{name} {refusal}", file=sys.stderr)
        return 1
    if action == "check":
        return 0
    environment = {} if action == "--env-only" else started_environment()
    environment.pop(encoded(name), None)
    if action == "--libpq-env":
        environment.pop(SERVICE, None)
    environment.update({encoded(key): encoded(value) for key, value in handed.items()})
    return run(command, environment)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
