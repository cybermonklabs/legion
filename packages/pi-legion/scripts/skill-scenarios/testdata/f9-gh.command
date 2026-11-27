bun packages/pi-legion/scripts/skill-scenarios/testdata/f9-gh-capture.ts /usr/local/bin/gh packages/pi-legion/scripts/skill-scenarios/testdata
# captured 2026-11-27T06:08:41Z with gh version 2.98.0 (2026-08-20), the gh of the worker image
f9-gh-help.json is the stdout of `gh <command> --help` for each command whose flags gh-standin.ts
reads. f9-gh-requests.json holds, for each call of f9-gh-capture.ts's list, the requests the real gh
sent for it, in order, its exit status and its stderr. The calls come from real runs (the F2
transcript's gh calls and f8's merger calls, on the recorder's repository acme/widgets) and from
gh's other forms of the same flags (short, joined, attached, `--name=value`, a selector before or
after the flags, none). gh ran in a git checkout on branch legion/ACME-12 (or, where a call's
`branch` is "detached", on no branch) against a loopback recorder that stands for
api.github.localhost through HTTP_PROXY, with a fake token and an empty gh config, so nothing
reached GitHub, and nothing was written anywhere. The recorder's answers are constructed: each
GraphQL answer holds exactly the fields the query selects, each a plausible value by its name; a
branch query finds pull request 7, or none where the call's `branch` is "none"; pull request 99 is
refused with GitHub's NOT_FOUND error. Each of gh's own GraphQL queries is kept as its operation's
first line and its variables; a query the call itself names is kept whole.
