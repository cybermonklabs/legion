# The commands of the tools reference

Read this when `skill://legion-worker/references/tools.md` sends you here; that file states the rule each command here carries out.

## Reading a shared database

The read `tools.md`, "Reading a shared database", asks for: the reader URL piped to psql as a service file, in one read-only transaction, with no `COMMIT` in `<file>`.

```sh
PGSERVICEFILE=<(python3 -c 'from urllib.parse import*;u=urlsplit(input());q=unquote;print(f"[r]\nhost={u.hostname}\nport={u.port or 5432}\ndbname={q(u.path[1:])}\nuser={q(u.username)}\npassword={q(u.password)}",*map("=".join,parse_qsl(u.query)),sep="\n")' <"$SESSION_READER_DSN_FILE" 2>/dev/null) PGSERVICE=r psql -1qX -v ON_ERROR_STOP=1 -c 'SET TRANSACTION READ ONLY' -f <file>
```
