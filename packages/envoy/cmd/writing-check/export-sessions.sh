#!/usr/bin/env bash
# export-sessions.sh writes the writer sessions of INFRA-3231's dataset out of the transcript
# store, for the harness to read beside each case (spec R1). It runs on a machine whose `tq` reads
# the store; a Legion pod cannot reach the store today.
#
#   export-sessions.sh --sessions sessions.tsv --exclude exclude.txt --out <dir>
#
# --sessions  one line per session: "<session id>\t<cut time, RFC 3339>". `writing-check dataset
#             build` writes it as sessions.tsv. A session that writes several cases appears once,
#             with the latest cut time of its cases.
# --exclude   one session id per line: the sessions to leave out, none of whose rows is read. The
#             owner's answer reads every session (dataset/DATASET, sessions_read), so it is empty.
# --out       an empty directory outside any repository: a path at or under a directory that
#             holds .git or .jj is refused, since its files hold the sessions' text, as every
#             step of the dataset refuses one. The script writes parts part-NNN.jsonl.gz (each at
#             most 24 MiB) and manifest.json, then prints the rows found for each session. Before
#             a row is written, every match of a secret pattern in any of its text values becomes
#             a marker naming the pattern ([REDACTED:<name>]), and manifest.json counts the masks
#             of each session by pattern (masks_by_session).
#
# Each row is one message part of the store's messages table, read with
# `tq sql --format JSONEachRow`: session_id, host, seq, ts, role, kind, text, raw_file, raw_line.
# Only rows at or before the session's cut time are read, and no row of kind `file` (the files
# beside a transcript, such as a tool's whole output, which are not the session itself). The query
# reads DISTINCT rows: the store keeps a second copy of a part loaded again after its
# deduplication window (transcript-store/README.md, "Tables"), and raw_file and raw_line keep
# two messages that share their text apart. A session the store holds no row of is reported with
# 0 rows; it is never read as an empty session, and nothing writes a part for it.
#
# The environment variable TQ names the tq command (default: tq), and BATCH the number of
# sessions one query reads (default: 50).
set -euo pipefail

usage() {
	echo "usage: export-sessions.sh --sessions <sessions.tsv> --exclude <exclude.txt> --out <dir>" >&2
	exit 2
}

sessions="" exclude="" out=""
while [ $# -gt 0 ]; do
	case "$1" in
	--sessions) sessions="${2:-}"; shift 2 ;;
	--exclude) exclude="${2:-}"; shift 2 ;;
	--out) out="${2:-}"; shift 2 ;;
	*) usage ;;
	esac
done
[ -n "$sessions" ] && [ -n "$exclude" ] && [ -n "$out" ] || usage
[ -r "$sessions" ] || { echo "export-sessions: cannot read $sessions" >&2; exit 2; }
[ -r "$exclude" ] || { echo "export-sessions: cannot read $exclude" >&2; exit 2; }
command -v python3 >/dev/null || { echo "export-sessions: python3 is required" >&2; exit 2; }
tq="${TQ:-tq}"
command -v "$tq" >/dev/null || { echo "export-sessions: $tq is not on PATH" >&2; exit 2; }
batch="${BATCH:-50}"
# --out is absolute and clean as the harness reads it (OutsideRepository, filepath.Abs), so each
# directory it walks up is one that holds --out.
dir=$(python3 -c 'import os, sys; print(os.path.abspath(sys.argv[1]))' "$out")
while :; do
	if [ -e "$dir/.git" ] || [ -e "$dir/.jj" ]; then
		echo "export-sessions: --out $out is inside the repository at $dir: its files hold Dispatch text, so give a path under \$TMPDIR" >&2
		exit 2
	fi
	[ "$dir" != / ] || break
	dir=$(dirname -- "$dir")
done
mkdir -p "$out"
if [ -n "$(ls -A "$out")" ]; then
	echo "export-sessions: $out is not empty" >&2
	exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The session list, checked: every id is a UUID and every cut time an RFC 3339 UTC time, so no
# text from either file can change the SQL the script builds. Excluded sessions are dropped here.
python3 - "$sessions" "$exclude" "$work/wanted.tsv" "$work/excluded.txt" <<'PY'
import re, sys
uuid = re.compile(r"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
when = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$")
sessions_path, exclude_path, wanted_path, excluded_path = sys.argv[1:]
excluded = set()
for n, line in enumerate(open(exclude_path), 1):
    line = line.strip()
    if not line:
        continue
    if not uuid.match(line):
        sys.exit(f"export-sessions: {exclude_path}:{n} is not a session id")
    excluded.add(line)
cuts = {}
for n, line in enumerate(open(sessions_path), 1):
    line = line.rstrip("\n")
    if not line:
        continue
    parts = line.split("\t")
    if len(parts) != 2 or not uuid.match(parts[0]) or not when.match(parts[1]):
        sys.exit(f"export-sessions: {sessions_path}:{n} is not '<session id>\\t<RFC 3339 UTC time>'")
    sid, cut = parts
    cuts[sid] = max(cuts.get(sid, cut), cut)
left_out = sorted(s for s in cuts if s in excluded)
with open(wanted_path, "w") as w:
    for sid in sorted(cuts):
        if sid not in excluded:
            w.write(f"{sid}\t{cuts[sid]}\n")
with open(excluded_path, "w") as w:
    for sid in left_out:
        w.write(sid + "\n")
PY

# Read the rows, one query per batch of sessions, into one JSONL file.
: >"$work/rows.jsonl"
split -l "$batch" "$work/wanted.tsv" "$work/batch-"
for b in "$work"/batch-*; do
	[ -s "$b" ] || continue
	where=$(awk -F'\t' '{ printf "%s(session_id = '\''%s'\'' AND ts <= parseDateTime64BestEffort('\''%s'\'', 9))", (NR > 1 ? " OR " : ""), $1, $2 }' "$b")
	"$tq" sql --format JSONEachRow \
		"SELECT DISTINCT session_id, host, seq, ts, role, kind, text, raw_file, raw_line FROM messages WHERE kind != 'file' AND ($where) ORDER BY session_id, seq, raw_file, raw_line" \
		>>"$work/rows.jsonl"
done

# Mask secrets. Each row is decoded, and every string value is read as text: tq's JSONEachRow
# writes `/` as `\/`, so a pattern run over the raw bytes would miss a URL. Every match of a
# pattern of transcript_store/redact.py (in the deployment repository) becomes a fixed marker naming
# the pattern, and the masks are counted per session and per pattern for the manifest. The rows
# are written again with every character as itself, and `/` is no longer escaped.
python3 - "$work/wanted.tsv" "$work/rows.jsonl" "$work/masked.jsonl" "$work/masks.json" <<'PY'
import json, re, sys
wanted_path, rows_path, masked_path, masks_path = sys.argv[1:]
SECRET_KEY = r"[\w\-]*(?:passw(?:or)?d|secret|token|api[_\-]?key)"
VALUE = r"""(?!["']?(?:\[REDACTED:|\$[{(A-Za-z_]))"""
# (name, pattern, replacement), in redact.py's order: its eleven SECRET_PATTERNS, then its added
# shapes, then the two token shapes this fleet mints (a Dispatch agent token, a NATS nkey seed).
PATTERNS = [
    ("api_key", r"sk-[a-zA-Z0-9]{20,}", "[REDACTED:api_key]"),
    ("anthropic_key", r"ANTHROPIC_API_KEY\s*[=:]\s*\S+", "[REDACTED:anthropic_key]"),
    ("openai_key", r"OPENAI_API_KEY\s*[=:]\s*\S+", "[REDACTED:openai_key]"),
    ("aws_key", r"AWS_ACCESS_KEY_ID\s*[=:]\s*\S+", "[REDACTED:aws_key]"),
    ("aws_secret", r"AWS_SECRET_ACCESS_KEY\s*[=:]\s*\S+", "[REDACTED:aws_secret]"),
    ("github_token", r"GITHUB_TOKEN\s*[=:]\s*\S+", "[REDACTED:github_token]"),
    ("github_pat", r"ghp_[a-zA-Z0-9]{36,}", "[REDACTED:github_pat]"),
    ("github_oauth", r"gho_[a-zA-Z0-9]{36,}", "[REDACTED:github_oauth]"),
    ("password", r"(?i)password\s*[=:]\s*\S+", "[REDACTED:password]"),
    ("bearer_token", r"(?i)Bearer\s+[a-zA-Z0-9\-._~+/]+=*", "[REDACTED:bearer_token]"),
    ("private_key", r"-----BEGIN [A-Z ]+ PRIVATE KEY-----[\s\S]*?-----END [A-Z ]+ PRIVATE KEY-----", "[REDACTED:private_key]"),
    ("api_key", r"sk-(?:ant|proj|svcacct)-[A-Za-z0-9_\-]{20,}", "[REDACTED:api_key]"),
    ("github_pat", r"github_pat_[A-Za-z0-9_]{22,}", "[REDACTED:github_pat]"),
    ("github_token", r"gh[usr]_[A-Za-z0-9]{36,}", "[REDACTED:github_token]"),
    ("slack_token", r"xox[abprs]-[A-Za-z0-9\-]{10,}", "[REDACTED:slack_token]"),
    ("aws_key", r"AKIA[0-9A-Z]{16}", "[REDACTED:aws_key]"),
    ("google_key", r"AIza[0-9A-Za-z_\-]{35}", "[REDACTED:google_key]"),
    ("google_oauth", r"ya29\.[0-9A-Za-z_\-]{20,}", "[REDACTED:google_oauth]"),
    ("tailscale_key", r"tskey-[A-Za-z0-9\-]{16,}", "[REDACTED:tailscale_key]"),
    ("gitlab_token", r"glpat-[A-Za-z0-9_\-]{20,}", "[REDACTED:gitlab_token]"),
    ("hf_token", r"hf_[A-Za-z0-9]{30,}", "[REDACTED:hf_token]"),
    ("jwt", r"eyJ[A-Za-z0-9_\-]{8,}\.eyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}", "[REDACTED:jwt]"),
    ("azure_sas", r"(?<![A-Za-z0-9])sig=[A-Za-z0-9%+/=]{16,}", "[REDACTED:azure_sas]"),
    ("azure_account_key", r"(?i)AccountKey=[A-Za-z0-9+/=]{20,}", "[REDACTED:azure_account_key]"),
    ("secret_value", rf'(?i)(\\?"{SECRET_KEY}\\?"\s*:\s*\\?"){VALUE}[^"\\]+', r"\1[REDACTED:secret_value]"),
    ("secret_value", rf"""(?im)^([ \t]*(?:-[ \t]+)?["']?{SECRET_KEY}["']?[ \t]*:[ \t]*){VALUE}(?![|>][ \t]*$)\S+""", r"\1[REDACTED:secret_value]"),
    ("env_secret", rf"""\b([A-Z][A-Z0-9_]*_(?:TOKEN|KEY|SECRET|PASSWORD)=){VALUE}\S+""", r"\1[REDACTED:env_secret]"),
    ("authorization", rf"""(?i)(Authorization["']?\s*[:=]\s*["']?(?:token|basic|bearer)\s+){VALUE}[^\s"']+""", r"\1[REDACTED:authorization]"),
    ("url_credentials", r"://[^\s/@:]*:(?!\[REDACTED:)[^\s/@]+@", "://[REDACTED:url_credentials]@"),
    ("password", rf"""(?i)(--password[ \t]+){VALUE}(?!-)\S+""", r"\1[REDACTED:password]"),
    ("header_secret", rf"""(?i)(\bX-[A-Za-z0-9\-]*-(?:Key|Token)["']?\s*:\s*["']?){VALUE}[^\s"']+""", r"\1[REDACTED:header_secret]"),
    ("private_key", r"-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----(?:(?:\r?\n|(?:\\r)?\\n)[ \t]*[A-Za-z0-9+/=]+(?=[\r\n\\\"']|$))*", "[REDACTED:private_key]"),
    ("dispatch_token", r"dsp_[A-Za-z0-9_\-]{43}", "[REDACTED:dispatch_token]"),
    ("nkey_seed", r"S[UAONCX]A[A-P][A-Z2-7]{53}[AEIMQUY4]", "[REDACTED:nkey_seed]"),
]
COMPILED = [(name, re.compile(pattern), repl) for name, pattern, repl in PATTERNS]

def mask(text, tally):
    for name, pattern, repl in COMPILED:
        text, n = pattern.subn(repl, text)
        if n:
            tally[name] = tally.get(name, 0) + n
    return text

masks = {line.split("\t")[0]: {} for line in open(wanted_path) if line.strip()}
with open(rows_path) as rows, open(masked_path, "w") as out:
    for line in rows:
        if not line.strip():
            continue
        row = json.loads(line)
        sid = row["session_id"]
        if sid not in masks:
            sys.exit(f"export-sessions: the store answered a row of session {sid}, which was not asked for")
        for key, value in row.items():
            if isinstance(value, str) and key != "session_id":
                row[key] = mask(value, masks[sid])
        out.write(json.dumps(row, ensure_ascii=False) + "\n")
with open(masks_path, "w") as f:
    json.dump(masks, f)
PY

# Parts: split the masked rows at 24 MiB, on line boundaries, so each gzipped part is smaller than
# that and under Dispatch's 25 MiB file limit.
if [ -s "$work/masked.jsonl" ]; then
	(cd "$work" && split -C 24m -d -a 3 masked.jsonl part-)
	for p in "$work"/part-*; do
		gzip -n -9 <"$p" >"$out/$(basename "$p").jsonl.gz"
	done
fi

python3 - "$work/wanted.tsv" "$work/excluded.txt" "$work/masked.jsonl" "$work/masks.json" "$out" <<'PY'
import hashlib, json, os, sys
wanted_path, excluded_path, rows_path, masks_path, out = sys.argv[1:]
counts = {line.split("\t")[0]: 0 for line in open(wanted_path) if line.strip()}
with open(rows_path) as rows:
    for line in rows:
        if line.strip():
            counts[json.loads(line)["session_id"]] += 1
masks = json.load(open(masks_path))
parts = []
for name in sorted(os.listdir(out)):
    if not name.endswith(".jsonl.gz"):
        continue
    data = open(os.path.join(out, name), "rb").read()
    parts.append({"name": name, "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()})
excluded = [line.strip() for line in open(excluded_path) if line.strip()]
manifest = {
    "schema": "infra-3231-session-export/2",
    "columns": ["session_id", "host", "seq", "ts", "role", "kind", "text", "raw_file", "raw_line"],
    "parts": parts,
    "rows_by_session": counts,
    "masks_by_session": {sid: m for sid, m in sorted(masks.items()) if m},
    "sessions_missing": sorted(s for s, n in counts.items() if n == 0),
    "sessions_left_out": len(excluded),
}
with open(os.path.join(out, "manifest.json"), "w") as f:
    json.dump(manifest, f, indent=1, sort_keys=True)
    f.write("\n")
for sid in sorted(counts):
    print(f"{sid}\t{counts[sid]}")
masked = sum(sum(m.values()) for m in masks.values())
print(f"sessions: {len(counts)}, with rows: {sum(1 for n in counts.values() if n)}, "
      f"missing: {len(manifest['sessions_missing'])}, left out: {len(excluded)}, "
      f"masks: {masked} in {len(manifest['masks_by_session'])} sessions, parts: {len(parts)}",
      file=sys.stderr)
PY
