#!/bin/sh
# Rebuilds /legion/scratch/tmp/infra3134/in/out/concord-run.txt from a fresh pod.
set -e
IOC_CLONES=/legion/scratch/tmp/ioc-clones
CP=$IOC_CLONES/cognition-protocols
PY=./venv/bin/python
sec() { printf '\n\n########## %s\n' "$1"; }

printf '%s\n' "CONCORD library run, INFRA-3134 r7. Every command below is shown before its output."
printf '%s\n' "IOC_CLONES=/legion/scratch/tmp/ioc-clones"
printf '%s\n' "cognition-protocols commit: $(git -C $CP rev-parse HEAD)"
printf '%s\n' "our tree: /legion/workspaces/cybermonklabs/legion/infra-3134 at 9542c6fbbecd184ccee73523ab3defa39d6cf23a"

sec '$ uv pip install --python venv/bin/python $IOC_CLONES/cognition-protocols/packages/ioc-concord'
uv pip install --python venv/bin/python "$CP/packages/ioc-concord" 2>&1

sec '$ ./venv/bin/python -c "import ioc_concord, sys; print(ioc_concord.__version__); print(sys.version.split()[0])"'
$PY -c "import ioc_concord, sys; print(ioc_concord.__version__); print(sys.version.split()[0])"

sec '$ ./venv/bin/python /legion/scratch/tmp/infra3134/in/out/concord-drive.py'
$PY /legion/scratch/tmp/infra3134/in/out/concord-drive.py 2>&1
printf 'driver exit status: %s\n' "$?"

sec '$ sed -n "27,35p" $IOC_CLONES/cognition-protocols/packages/ioc-concord/src/ioc_concord/wire.py'
sed -n '27,35p' "$CP/packages/ioc-concord/src/ioc_concord/wire.py"

sec '$ sed -n "16p;22,25p" $IOC_CLONES/cognition-protocols/packages/ioc-concord/src/ioc_concord/engine.py'
sed -n '16p;22,25p' "$CP/packages/ioc-concord/src/ioc_concord/engine.py"

sec '$ grep -n "concord-init\b\|concord-init-reply\|concord-result\|concord-resend\|concord-cancel" $IOC_CLONES/cognition-protocols/protocols/concord/spec/CONCORD-SPEC.md | sed -n "1,8p"'
grep -n 'concord-init\b\|concord-init-reply\|concord-result\|concord-resend\|concord-cancel' "$CP/protocols/concord/spec/CONCORD-SPEC.md" | sed -n '1,8p'

sec '$ sed -n "121p" $IOC_CLONES/cognition-protocols/protocols/concord/spec/CONCORD-SPEC.md'
sed -n '121p' "$CP/protocols/concord/spec/CONCORD-SPEC.md"

sec '$ sed -n "113,119p" $IOC_CLONES/cognition-protocols/protocols/concord/spec/CONCORD-SPEC.md'
sed -n '113,119p' "$CP/protocols/concord/spec/CONCORD-SPEC.md"

sec '$ ./venv/bin/python -c "exact rational vs float: the spec example value"'
$PY - <<'PY'
from fractions import Fraction
from ioc_concord.engine import norm
st = {"anchors": {"a": {"raw_reject": 0.1, "raw_ideal": 0.3}}}
lib = norm(st, "a", 0.2)
spec = Fraction(Fraction("0.2") - Fraction("0.1"), Fraction("0.3") - Fraction("0.1"))
print("library norm(0.2) =", repr(lib))
print("spec exact        =", spec, "=", float(spec))
print("library == spec rounded to 6 places:", round(lib, 6) == round(float(spec), 6))
print("library value is exactly 0.5:", lib == 0.5)
PY

sec '$ ./venv/bin/python -c "does the library refuse a raw value the spec calls invalid?"'
$PY - <<'PY'
from ioc_concord.wire import parse_reply
for raw in ("1e9", "NaN", "-40", "250"):
    doc = f'<concord-score-reply><score candidate_id="seed-a"><raw>{raw}</raw><reasoning>x</reasoning></score></concord-score-reply>'
    try:
        print(f"raw={raw!r:8} accepted ->", parse_reply(doc)["scores"][0]["raw"])
    except Exception as exc:
        print(f"raw={raw!r:8} refused  ->", exc)
PY

sec '$ grep -rnE "severity|Minor|Medium|High|Critical" $IOC_CLONES/cognition-protocols/packages/ioc-concord/src/ioc_concord/'
grep -rnE 'severity|Minor|Medium|High|Critical' "$CP/packages/ioc-concord/src/ioc_concord/" || echo '(no output; exit 1)'

sec '$ grep -rnE "finding|verdict|abstain|confidence" $IOC_CLONES/cognition-protocols/packages/ioc-concord/src/ioc_concord/'
grep -rnE 'finding|verdict|abstain|confidence' "$CP/packages/ioc-concord/src/ioc_concord/" || echo '(no output; exit 1)'

sec '$ grep -rnE "Fraction|Decimal|millionth" $IOC_CLONES/cognition-protocols/packages/ioc-concord/src/ioc_concord/'
grep -rnE 'Fraction|Decimal|millionth' "$CP/packages/ioc-concord/src/ioc_concord/" || echo '(no output; exit 1)'

sec '$ grep -rnE "run_id|uuid|version=|concord-resend|concord-cancel" $IOC_CLONES/cognition-protocols/packages/ioc-concord/src/ioc_concord/'
grep -rnE 'run_id|uuid|version=|concord-resend|concord-cancel' "$CP/packages/ioc-concord/src/ioc_concord/" || echo '(no output; exit 1)'

sec '$ the disagreement search over our own review record'
printf '%s\n' 'corpus: the review bodies of the 200 merged pull requests listed in out/concord-prs.txt,'
printf '%s\n' 'fetched 2026-11-12 with:'
printf '%s\n' '  for n in $(cat concord-prs.txt); do gh api --paginate "repos/cybermonklabs/legion/pulls/$n/reviews" \\'
printf '%s\n' '    --jq ".[] | {pr: $n, id: .id, state: .state, commit: .commit_id, author: .user.login, submitted: .submitted_at, body: .body}" \\'
printf '%s\n' '    >> reviews.jsonl; done'
printf '%s\n' "reviews.jsonl: $(wc -l < reviews.jsonl) review bodies, sha256 $(sha256sum reviews.jsonl | cut -d' ' -f1)"
printf '%s\n' ''
printf '%s\n' '$ jq -r '"'"'select(.body|test("[Tt]hermonuclear pair at")) | [(.pr|tostring),(.id|tostring),.commit,.submitted] as $m | (.body|split("\n")[]) | select(test("[Tt]hermonuclear pair at")) | ($m|join("\t")) + "\t" + .'"'"' reviews.jsonl \\'
printf '%s\n' '  | awk -F"\t" '"'"'{ l=$5; d=""; q=""; if (match(l, /[Dd]eep[- ]review[^.;]{0,40}(APPROVE|CHANGES)/)) { s=substr(l,RSTART,RLENGTH); d=(s ~ /APPROVE/)?"APPROVE":"CHANGES" } if (match(l, /[Cc]ode[- ]quality[^.;]{0,40}(APPROVE|CHANGES)/)) { s=substr(l,RSTART,RLENGTH); q=(s ~ /APPROVE/)?"APPROVE":"CHANGES" } if (d != "" && q != "" && d != q) printf "%s\\t%s\\t%s\\t%s\\tdeep=%s\\tquality=%s\\n", $1,$2,substr($3,1,12),$4,d,q }'"'"''
printf '%s\n' ''
jq -r 'select(.body|test("[Tt]hermonuclear pair at")) | [(.pr|tostring),(.id|tostring),.commit,.submitted] as $m | (.body|split("\n")[]) | select(test("[Tt]hermonuclear pair at")) | ($m|join("\t")) + "\t" + .' reviews.jsonl \
 | awk -F'\t' '{ l=$5; d=""; q=""; if (match(l, /[Dd]eep[- ]review[^.;]{0,40}(APPROVE|CHANGES)/)) { s=substr(l,RSTART,RLENGTH); d=(s ~ /APPROVE/)?"APPROVE":"CHANGES" } if (match(l, /[Cc]ode[- ]quality[^.;]{0,40}(APPROVE|CHANGES)/)) { s=substr(l,RSTART,RLENGTH); q=(s ~ /APPROVE/)?"APPROVE":"CHANGES" } if (d != "" && q != "" && d != q) printf "%s\t%s\t%s\t%s\tdeep=%s\tquality=%s\n", $1,$2,substr($3,1,12),$4,d,q }'

sec '$ gh api repos/cybermonklabs/legion/pulls/3522/reviews --jq ".[] | select(.id==5854791039) | .body" | grep -n "thermonuclear pair at this head"'
gh api repos/cybermonklabs/legion/pulls/3522/reviews --jq '.[] | select(.id==5854791039) | .body' | grep -n 'thermonuclear pair at this head'

sec '$ gh api repos/cybermonklabs/legion/pulls/3522/reviews --jq ".[] | select(.id==5854791039) | .body" | grep -o "dispatch results --issue INFRA-[0-9]*"'
gh api repos/cybermonklabs/legion/pulls/3522/reviews --jq '.[] | select(.id==5854791039) | .body' | grep -o 'dispatch results --issue INFRA-[0-9]*'

sec '$ does our review record hold a satisfaction rating or an anchor pair for either grader?'
printf '%s\n' '$ jq -r '"'"'select(.body|test("[Tt]hermonuclear pair at")) | .body'"'"' reviews.jsonl | grep -nE "raw_reject|raw_ideal|satisfaction[_ ]threshold|0-100 scale|normali[sz]ed score"'
jq -r 'select(.body|test("[Tt]hermonuclear pair at")) | .body' reviews.jsonl | grep -nE 'raw_reject|raw_ideal|satisfaction[_ ]threshold|0-100 scale|normali[sz]ed score' || echo "(no output; exit 1; scope: the 155 review bodies of the corpus, on 58 pull requests, that carry a thermonuclear-pair line)"

sec '$ grep -n "Nothing requests a score" $IOC_CLONES/cognition-protocols/protocols/concord/spec/CONCORD-SPEC.md'
grep -n 'Nothing requests a score' "$CP/protocols/concord/spec/CONCORD-SPEC.md"
