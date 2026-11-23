# outside-edge-refusals.sh

`outside-edge-refusals.sh` is the HTTPS refusal check of an outside edge: a public load balancer
with one IPv4 address, whose routes forward a few paths of a few hosts to their backends. It asks
that address, on 443, for each path and Host the edge must not forward, and reads whether one of the
edge's routes took the request. `outside-path-reach.sh` cannot see this: it measures TCP connects,
and an address that answers on 443 forwards by path and by Host there. Every route of the edge sets
the response header `X-Outside-Edge-Backend` to the backend it reached, so an answer without it came
through none of them, and that marker is what the check reads. The script's own header has every
rule; this page says what a person running it or changing it needs.

## What it needs

Like `outside-path-reach.sh` it is one file that sources nothing. It needs bash (3.2, as macOS ships
it, or later), curl 8.4.0 or later, `mktemp`, and `sha256sum` or `shasum` to hash each answer's
body. An older curl reads `UNKNOWN: curl missing or too old`, since before 8.4.0 curl's
`--max-filesize` does not bound a body whose length the answer did not give first. Started by a
shell that is not bash it prints `UNKNOWN: run this with bash` and stops.

## Inputs

- `OUTSIDE_EDGE_ADDRESS`, the edge's IPv4 address, four numbers of 0 to 255 with no leading zero:
  every probe goes to it, wherever the names it sends resolve where it runs (`curl --resolve`).
- `OUTSIDE_EDGE_ROUTED`, a file of `name method sni host path backend` lines, the positive controls:
  one or more per backend, each a request a route takes and marks with
  `X-Outside-Edge-Backend: <backend>`. A backend is never `none`, the word a probe line prints for
  an answer with no marker.
- `OUTSIDE_EDGE_REFUSALS`, a file of `name method sni host path [405:marker]` lines, each a request
  the edge must refuse. The optional sixth field is for a method the edge refuses on a path it
  forwards on purpose: `405:` and the marker of the backend that refuses it, such as `405:broker`
  for a `GET` on the broker's token path. No route of the edge matches on method (its Gateway class
  supports none), so that `GET` reaches the broker, and the broker's own `405`, marked, is its
  refusal. 405 is the status RFC 9110 gives a method its target does not support; another status
  there, a 4xx included, would let a list take an answer the edge forwarded, such as a backend's
  own 401 or 404, for a refusal. Any other sixth field, `405:none` among them, refuses the run.

Both lists split their fields on spaces and tabs, skip blank lines and lines whose first field
starts with `#`, and take LF or CRLF line endings; any other byte outside printable ASCII and tab
refuses the run. Each name belongs to one line across both lists. `method` is one of `GET`, `POST`,
`PUT`, `PATCH`, `DELETE` and `OPTIONS`; `sni` and `host` are host names, never an address, since a
route's hostname cannot be an address and no route could be planted for one. `path` starts with `/`
and holds URL characters alone. It holds no `//` and no encoded slash (`%2f`), and its part before
any `?` holds no dot segment: no segment that is `.` or `..` once each `%2e` or `%2E` in it is read
as `.` (`/a/..?x`, `/a/.?x` and `/a/%2e%2e/b` are each refused). RFC 3986 makes `%2e` the same as
`.`, so an edge may read such a path as the one it reduces to, and the Gateway API refuses a route
whose path holds `//`, `%2f` or a literal dot segment, so no route could be planted for such a line.
`OUTSIDE_EDGE_CONNECT_SECONDS` and `OUTSIDE_EDGE_READ_SECONDS` bound each probe as curl's
`--connect-timeout` and `--max-time`, 3 and 10 s when unset or empty, on the rule
`outside-path-reach.sh`'s two settings follow.
`OUTSIDE_EDGE_ALLOW_HTTP=1` lets the address be `http://127.0.0.1:<port>`, and nothing else, for the
script's own tests.

```bash
OUTSIDE_EDGE_ADDRESS=192.0.2.10 OUTSIDE_EDGE_ROUTED=routed.txt \
  OUTSIDE_EDGE_REFUSALS=refusals.txt bash outside-edge-refusals.sh
```

## How it reads an answer

Each probe is one HTTP/1.1 request, which keeps the Host it sends literal: `curl --http1.1
--path-as-is --resolve <sni>:443:<address> -H 'Host: <host>' -X <method> [-d ''] --max-filesize
1048576 https://<sni><path>`, with an empty body for `POST`, `PUT` and `PATCH`, and the certificate
verified against the SNI. `--path-as-is` sends the path exactly as the line writes it: without it,
curl removes a dot segment before it sends a path, `%2e` read as `.` in current releases. curl stops
reading a body at 1 MiB, whether or not the answer gave its length first. Its curls ignore
`~/.curlrc` and any proxy, and read no URL as a pattern. curl writes every header block of an
answer, an informational `1xx` before the final one, and the status it prints is
the final answer's; an answer names a backend when its final block carries a marker and every
marker of every block is that backend.

The routed controls go first: each must answer naming its backend, whatever its status, or the run
prints `UNKNOWN: the marker of <name> is not visible, so no refusal can be read` and reads no
refusal line, since a 404 with no marker says nothing where no marker can be seen.

Then each refusal line of five fields first has its unrouted control sent: the line's own method,
SNI and path, with the Host `unrouted.outside-edge-refusals.invalid`, which no route names. Its
answer must come whole, with no marker; the line is refused only on a 404 with no marker and that
same body. A route that forwards a line without a marker to a backend that answers 404 may give
another body, and so may an edge whose own 404 differs by Host; the run cannot tell the two apart,
so such a line reads `UNKNOWN`, never refused. An edge's own 404 with an empty body tells nothing
either, since such a forward could give an empty body too, so it reads `UNKNOWN` as well. A body's
SHA-256 counts only when the hasher exits 0 printing exactly one, so a line whose body or whose
control's the hasher gave none of reads `UNKNOWN` too: two failed hashes would compare equal. And
before any list is read or any request sent, the run hashes the empty input and `abc` and stops at
`UNKNOWN` unless both digests are SHA-256's, since a hasher that ignored its input would make every
body look alike. An `UNKNOWN` line that says "another body than its unrouted control's 404" is a
possible forward: look into it before running the check again.

The comparison rests on a rule this check cannot hold: every route of the edge sets the marker. A
route with no hostname that forwards without a marker to a backend that answers 404 would give the
control and the line the same body, and no request sent from outside can tell that from the edge's
own 404. The deployment's own tests hold that rule, not this check.

| The answer | Line |
|---|---|
| the one the line names, read whole: for a line of five fields, 404 with no marker in any block and its unrouted control's body; for a line of six, exactly 405, naming exactly the sixth field's marker | `refused` |
| any other answer: for a line of five fields, a marker of any value in any block or a status other than 404, a redirect included | `REACHED` |
| no status (a connect or TLS failure, a timeout), the named answer that curl did not read whole, its body past 1 MiB included, or a 404 with no marker whose unrouted control got no such 404, whose body or its unrouted control's the hasher gave no SHA-256 of, whose body is not its unrouted control's, or whose body and its unrouted control's are both empty; the verdict names each cause and its lines | `UNKNOWN` |

Each line prints the status, every marker in order (`none` for none) and the SHA-256 of the body
(`unreadable` where the hasher did not exit 0 printing exactly one), so two runs can be compared,
and curl's exit code and first message where curl did not exit 0; an unrouted control prints
`unrouted <name>` the same way. No line names the machine's host name, since the output goes to
logs anyone may read.

## The verdict

`PASS` (exit 0) needs every control's marker and every refusal line refused. `FAIL` (exit 1) names
each line reached, whatever else the run could not read, a signal that stopped the run after it
included. Every run that proved nothing is `UNKNOWN` (exit 2): an input refused, a tool missing or
too old, an empty or unreadable list, a control's marker not seen, a line whose answer could not be
read as a refusal or a reach, or a run a signal stopped before any line was reached.

A HUP, INT or TERM always ends the run at one verdict line, never at bash's own exit with nothing
printed. From the first line of any verdict on, through the exit's cleanup, the three are ignored,
so a second signal neither prints a second verdict nor leaves the answers' directory behind; the
exit kills every job the run started, a list's reader included. A signal sent to the script's own
process ends the run at once while it reads a list, and once the probe in flight returns while it
probes; one sent to its process group ends it at once.

## Its tests

`cd packages/dispatch && bun test e2e/__tests__/outside-edge-refusals` runs its three test files,
with their fixtures in `outside-edge-refusals-fixtures.ts`:

- `outside-edge-refusals.test.ts`, the answers it reads. It holds the check's planted-route control:
  against a table of the routes an edge serves and sixteen refusal lines (spoofed Hosts, cluster
  names, paths beside the routed ones), a route planted for exactly one line makes the run fail
  naming that line and no other, once for each line. A line whose sixth field is `405:broker` is
  refused on the broker's `405` marked `broker`, and fails naming it on a `404` with no marker, a
  `405` with another marker or none, a `200`, a `103` marked `broker` before a final answer marked
  otherwise or not at all, and a final `405` with a second marker. An unmarked `200`, `301`, `302`
  or `403` is a line reached, and an unmarked `404` with a backend's own body, or with an empty
  body where the edge's own 404 is empty too, is `UNKNOWN`.
- `outside-edge-refusals-input.test.ts`, the inputs, tools and shells it refuses before any probe.
  Its rows that run the script under dash and zsh skip where either is missing; CI's `dispatch-ui`
  job installs zsh.
- `outside-edge-refusals-signal.test.ts`, a signal while it reads a list, while it probes, after a
  line was reached, and during the exit's cleanup.

The rows run it against a stand-in edge on the loopback (`outside-edge-refusals-edge.ts`, served
for each stand-in from a process of its own, so a row that passes its timeout takes down no other
row's stand-in), which routes by Host, method and the path as it came on the wire from a table,
marks each routed answer and answers 404 with no marker otherwise, and through a `curl` of the
row's own for the HTTPS command line and the answers no stand-in gives. A run that probes is a
bounded child under the `edgeRun` bound of `e2e/__tests__/bounded-subprocess.ts`, whose header holds
the reading behind it.
