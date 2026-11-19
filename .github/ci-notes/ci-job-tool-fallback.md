# Notes for the tool fallback proof's steps

The derivations and the history of `.github/workflows/ci-job-tool-fallback.yaml`, which points
into this file. Each `# why:` line of that workflow names the section below that holds the
derivation or the record its step, its job or its key rests on, and the sections stand in the
order their comment blocks stood in the workflow file. A value's own arithmetic stays beside the
value, and a comment inside a `run:` body stays beside its command, in the script that body is
now in.
In the text moved here, "this file" refers to the workflow,
`.github/workflows/ci-job-tool-fallback.yaml`.

## The steps of ci.yaml's tool-fallback job, in a file of their own

The steps of ci.yaml's `tool-fallback` job, in a file of its own.
ci.yaml's job of the same id calls this file, and that caller keeps `needs: [changes, ci-images]`
and the whole `if:` that decides whether these steps run, so this file carries no `if:` of its
own. The notes on that job,
.github/ci-notes/ci.md#the-tool-fallback-proof-what-it-drives-what-it-only-reasons-about-and-how-to-read-it-when-it-is-red,
say what the proof is and why it is permanent.

The split is for the filters. The tool_fallback filter names this file beside ci.yaml, so a
change to these steps selects this job alone, where a change to ci.yaml selects every job.

The job id here is the one ci.yaml calls this file under, and it has to stay: a composite
action reads `github.job` as the id of the job that called it, and .github/actions/setup-bun
passes no-cache for every job whose id starts with `tool-fallback`, which is what keeps every
case of this job a reading of an install rather than of the Actions cache.

The job's last step reports ci-ok for the caller: on a merge-group run this job's own result
decides, it posts the failing ci-ok at once (.github/actions/ci-ok-report), as every job of
ci.yaml ends. `ci-ok-changes` carries ci.yaml's `toJSON(needs.changes)` for it.

## The job's budget, and the bound every install step carries

The budget, added up from the slowest run this job can have, the way the
tool-fallback-arm job of .github/workflows/ci-job-tool-fallback-arm.yaml adds up its own:
the job's set-up, the two checkouts and the runner's download of the actions (1), the two
steps that read the package and the manifest, and `stage`, which fetches about 303 MB over
the real sources while the network is whole and so keeps the pinned ladder (2), the
jj-primary case, which runs before the fault (1), the fault and its probe, whose two
requests are bounded at 15 s each (1), the one case that keeps the ladder, the head's own
`install-crane`, which measured 64 s and which `--retry-max-time 60` bounds at a minute in
which a new attempt may start plus one last attempt of the row's `max_time` of 60 s (2),
the other steps whose primary the fault or a dead port holds, each now about a second
because the stand-in shortens them: job 129874475871 made 65 fetches through the stand-in,
and 29 of them were shortened — 19 of github.com, 9 of the dead address and 1 of
get.pulumi.com behind the base-pulumi control's dead proxy. The other 36 kept the pinned
ladder: the kept case's own fetch at 63 s, the probe's reserved control name, and the
honest fetches of the package, npm, PyPI, get.pulumi.com and the local origin, none of
which took more than a second (1), the bytes the installs and the sixteen cases move, over
npm, PyPI, get.pulumi.com and loopback, the two 107 MB Bun tarballs among them (3), and the
readings, the `ladder-report` step, the stop-fault step and the report (1).
That is 12, and a job its own limit cuts off reports `cancelled`, which ci-ok fails, so the
limit carries ten minutes over the sum.
This budget replaces 65 minutes with a limit of 75, and 45 of those 65 were the twenty-one
steps whose primary the fault holds, allowed at 126 s each. That 126 s is what a negative
control costs, because it drives two ladders; the other fourteen of those steps drive one
and cost about 64 s, so the old allowance was about twice what two thirds of them could
cost. What one ladder costs, and how it is derived, is in the comment above ci_tools_curl
of .github/scripts/ci-tools.sh.
What the change did: job 129743677408 ran 1,994 s and job 129885098900 ran 284 s, and the
one case that still drives the real ladder measured 63 s inside it.

THIS CHANGE ADDS TEN STEPS TO THAT SUM, AND NINE OF THEM COST NOTHING A READER WOULD
NOTICE: eight marks, one before each install with nowhere to go, each a single `echo`
which measured 0 s in jobs 130015112773, 129940828354 and 129966328215 and at most 1 s
in job 130289341628 at the merge of main, and the watch's report, 0 s in all four. The
tenth is the watch's start, which spawns the sampler and reads the file system it will
watch: 3 s, 5 s and 4 s in those three jobs and 9 s in job 130306447638, so it is the
one added step whose cost a reader can see, and the item above allows for it. The watch
itself is a sampler beside the job rather than a step of it, so those two steps are its
whole cost to this budget (dispatch://INFRA-2605).

EVERY INSTALL STEP OF THIS JOB AND OF tool-fallback-arm DECLARES `timeout-minutes: 11`,
and the twenty-four installs whose control accepts a failure are timed as well, against
the seconds .github/scripts/ci-tools-proof.sh holds every timed install to: a step the
runner ends at its own bound leaves `outcome=failure` behind it, which those controls
accept, so the wall time is the only reading that tells a hang from the refusal the case
expects.
The eleven minutes is derived from the installs as they ran before the ladders stopped
sleeping: over the green runs of these two jobs between 2026-11-09T19:38Z and
2026-11-10T10:51Z, 2422 install steps — the sixteen-install shape this job carried for
part of that window counted as well as the thirty-two it carries now — gave a median of
64 s, a 99th percentile of 128 s and a maximum of 326 s, which is `install-jj` on five
slow runs. Eleven minutes is twice that maximum, rounded up to the minute the key takes,
so no step that would have finished is ended by it. Since the ladders stopped sleeping
every install sits under that 99th percentile: the slowest in job 130289341628 is the
head's own `install-crane`, the one kept ladder, at 65 s.
THE BOUND WOULD HAVE CHANGED NOTHING IN THE INCIDENT THAT ASKED FOR IT, because a bound
the runner enforces needs a runner that is alive. It is here for the other failure: a
command that hangs while the runner answers, where without it the job's own ceiling is
the only thing that ends it, and the ceiling costs the merge queue the pull request it
holds.
That the runner does enforce it on a `uses:` step of a composite action is measured and
not assumed: on this change's own first run, an action sleeping 180 s under
`timeout-minutes: 1` was ended after 72.5 s and the steps after it ran. The job record of
that step (job 129786363688, step 3) gives the wall time, 10:57:03Z to 10:58:16Z, and
`conclusion: success`, and holds no `outcome` at all; its log gives `The action has timed
out.`; and the `outcome=failure` the caller's later steps read was measured in job
129872208213, where a later step printed `steps.<id>.outcome`. The runner is about
twelve seconds late, so a bound is a ceiling and not a stopwatch.

## Why the merge base is checked out

The merge base, for the negative control the seven `base-*` steps run: a second tree of this same repository,
whose seven installs that control drives with nowhere left to go.
.github/ci-notes/ci-job-tool-fallback.md#the-negative-control-and-why-it-does-not-expire
says what keeps this checkout here.

## Why the package's blob is read for every mirrored row

The package holds the blob of every mirrored row, asked for with no credential at all
and at the address .github/scripts/ci-tools.sh builds, which is how an install asks for
it: this step reads the blob an install reads, through the same CI_TOOLS_GHCR_BASE
seam, and writes no address of its own.
This rule is the opposite of the filler's, on purpose. .github/scripts/mirror-ci-tools.sh
leaves a cold row green, because an asset that does not answer is the outage the copy
exists to survive and a red ci-ok during that outage helps nobody. Here a cold row is a
refusal, because this job records a pass for a fallback it drove, and a pass recorded for
a row whose second source does not exist yet is a pass for nothing.

## Why every row is printed beside the two call sites that read its digest

Each row beside the two call sites that read its digest: the line of the manifest that
holds the literal, and what reads that row in an action or in the rig.
One literal is what both paths are held to, and this is the evidence of it — there is no
second digest in an action to read it out of, which is the point.

## What the machine watch records, and what it cannot show

What the machine had, every second from here to the report of the `Report what this job ran on` step: free disk, the memory
the kernel reports available, and the one-minute load, one line per second, then the
worst each of them reached (.github/scripts/disk_watch.py, which envoy-smoke drives
around its live-tests step and which prints the series there too).
The job record times every step boundary, so a reader lines any step of this job up
against the second it ran in. That is what this job could not answer on 2026-11-10,
when one of its runs lost its hosted runner and GitHub's annotation named starvation of
CPU or memory as one of three causes (dispatch://INFRA-2605).
WHAT IT DOES NOT SHOW is the loss itself: a runner that stops answering stores no log at
all, so the series ends where the log ends and says nothing about the second after it.
It is the headroom of every run that does store a log, and the reading for a command
that hangs while the runner is alive.
It starts before the fault, so the two tools `start` asks to name a directory are not
routed through a proxy that refuses them. The report prints and never fails the job:
this is the measurement, not a new gate.

## Case 5 of 16, and why it runs before the fault

Case 5 of 16 of the corrupted payloads, and the one that runs here rather than with the
other seven: the jj binary in mise's install directory is one byte changed.
That directory is where .github/actions/install-jj reads an installed jj from, so a
changed binary there is the primary path's own mismatch: `mise install` finds the tool
installed, the action holds the binary mise points at to the jj row, and refuses it.
It runs before the fault because `mise install` resolves the tool's version list over
api.github.com even when the install directory already holds it, and the fault that the `Start the fault in front of GitHub's release assets` step starts
holds every host under github.com. Under the fault that resolve fails, the action takes
its second source instead, and this case would read the fallback rather than the
primary — which is what happened at 1725f7ce before this step moved.
Here it needs no release asset either: the rig seeds mise's directory from the payload
it staged, so a release-asset outage, which is what this whole job is about, leaves this
case exactly as it is. api.github.com is not a release-asset host and does not go down
with one.

## Why mise's install directory is taken away again

What that case seeded goes, so nothing of it reaches the later installs of this job: the head's own
install-jj meets an empty mise directory, as a fresh runner would.
It is not what decides that install. Measured in CI at run 42507815176, `mise install`
resolves the tool's version list over api.github.com even when its install directory
already holds the tool, so under the fault that the `Start the fault in front of GitHub's release assets` step starts, that install fails whether this
directory is here or not, and the action reads the copy either way. What a directory
left here would leave behind is the one-byte-changed jj this case seeded, on a runner
whose later steps run what this job installed.

## The stand-in curl, what it shortens, and why it is installed at this position

The stand-in curl every step after this one runs, which shortens only a fetch this job
arranged to fail. Every case of this job is a refusal this rig arranged, and each of
those refusals rode a retry ladder nobody reads: job 129743677408 ran 1994 s and spent
1952 s in the steps that pay for them.
Every other fetch keeps the pinned ladder, so that ladder still rides out a flap of
npm, PyPI, get.pulumi.com or the local origin, each of which this job reaches through
the fault proxy and none of which it faults. No case reading of this job counts
requests or durations, so a shortened ladder takes no statement away: what a case
proves is which source served its bytes and that the digest held, and the stand-in
changes neither. The ladder-report step is the one reading of durations.
One case keeps the pinned ladder, the head's own install-crane (the step whose `env:` sets `CI_TOOLS_PROOF_FULL_LADDER: crane-head`), and the
ladder-report step at the end of this job reads what that one measured. That install
carries no seam, so its primary is the real faulted host and its second source the real
package copy, and it drives the whole claim the shortened cases give up: a real ladder,
then a second source, then the digest check. The negative controls cannot be that case,
because a control ends in failure and proves no fall-through. The head's installs
cannot move before the controls either, because .github/actions/setup-bun takes its
existing-install branch when a Bun of the pinned version is already on the runner,
which is exactly what the base-bun control must not find.
It runs here, immediately before the fault comes up, and the window it may run in is
the one between `stage` and that step. It cannot run before `stage`, because `stage`
fetches about 303 MB from the real sources while the network is whole and must keep
the pinned ladder for them, and because $GITHUB_PATH reaches the steps after the step
that writes it and not that step itself.
It cannot stay straight after `stage` either, which is where it ran before: the
jj-primary case runs in that window, and its install-jj makes a real fetch of
github.com through mise which succeeds. github.com is one of the hosts this rig
faults, so a stand-in installed before that case shortened a fetch this job means to
succeed and took the pinned ladder off it, while the fault that makes the shortening
true was not up yet. In job 129841257239 that fetch is the first one the ladder-report
step printed, `github.com: shortened, curl exit 0, 0s`: 2 attempts over 1 s where the
pinned ladder gives 7 over 63 s, so one short failure of github.com would redden this
required job where the pinned ladder rode it out.
The step after this one starts the fault, so every fetch made from here goes through
it and no fetch this job did not arrange to fail is ever shortened. The ladder-report
step refuses the job for exactly that reading — a fetch that was shortened and then
served the bytes — which is what holds this step at this position.
The remedy is the position and not a guard in the stand-in on a state file the fault
writes: a position cannot drift out of the truth it states, while such a guard can.

## The fault, and the routing of every later step through it

The fault, and the routing of every later step through it, in one step: the rig starts
.github/scripts/ci-fault-proxy.py over the release hosts its own FAULT_HOSTS names, and
writes the four spellings of the two proxy variables and the two emptied bypass lists
to $GITHUB_ENV itself. Those hosts and those variables are written there and in no step
of this file, so a release host that moves again is one edit in one place.
The runner resolves and downloads an action with its own environment rather than a
step's, so every action this job uses is already on disk when the fault starts, and the
fault is met only by what an action fetches.

## The negative control, and why it does not expire

The negative control, under the same fault: eight installs with nowhere to go — the
merge base's seven, each given a dead port where its second source would be read, a ghcr
base, an npm base, a URL base or a proxy, and the docker provider plugin, which the
merge base has no action for, as the head's own action with its second source on a dead
port — so every one of them must fail here.
What it asserts is a property of this change and not a fact about history: an install
whose primary meets the fault and whose second source cannot be reached fails. That is
what makes the passes of the head's own installs a reading of the second source rather than of a cache, of a
tool the runner image already carries, or of a proxy that let the asset through.
The merge base's seven fail for two reasons, and which one is each install's depends on
whether that install already read the helper there. Its crane, changie, jj and Bun do,
so their primary meets the fault and the ghcr and npm seams send them to a dead port.
Its elan, uv and Pulumi CLI read no seam at all, each being a `curl` of its own: elan's
and uv's one source is the release asset this fault holds, and the CLI's is
get.pulumi.com, which this fault does not hold, so that step carries a dead proxy of its
own instead.
Once this change is on main the base is this tree, and every one of the eight fails for
the dead ports alone, which leave its second source nothing to reach.
A control that leaned on the merge base having one source each would expire the day this
change lands on main: the base would then be this tree, each primary would meet the
fault, each second source would reach the real ghcr — whose blobs redirect to
pkg-containers.githubusercontent.com, which this fault leaves alone on purpose — or the
real npm, PyPI or get.pulumi.com, every one of them would succeed, and this step would
fail every pull request and merge group that selects this job. On a workflow_dispatch
the `ref:` of the `Check out the merge base the negative control installs from` step falls through to github.sha, the head itself, so that control is wrong
already.
A dead port rather than a manifest with its `second_source` rows deleted: a row with no
second source makes ci_tools__second_source refuse the empty field, so the install would
fail for the wrong reason — it would prove the helper rejects an empty field, not that
the install had nowhere to go, and a negative control must fail for the reason the check
names. A base that does not have the helper ignores both variables and fails as it
always did.
The merge-base checkout stays. It is this repository's only caller of the rule
.github/scripts/check-image-trigger-paths.sh keeps about a local `uses:` under a second
checkout of this same repository, and that check's own comment names this job for it.
After this change is on main the checkout is redundant to the dead ports (`http://127.0.0.1:1`) of the controls' `env:`, which
drive the control by themselves, so a later reader who deletes the merge base's seven
steps is deleting that caller too and should move the rule's exercise rather than lose
it.

## Why each install with nowhere to go carries a mark step

When the install a mark step names started (`mark-base-crane` for `base-crane`, and so on). Each of the eight with nowhere to go carries a mark step
of its own, and the control step after them reads every one, because a bound that fires
on one of them is otherwise silent: each carries `continue-on-error` and the control
accepts a step whose `outcome` is `failure`, which is exactly what the runner leaves
behind when it ends a step at its bound. The clock is the one signal that tells the two
apart, since GitHub gives a later step nothing that says an earlier one was cut.
$GITHUB_OUTPUT rather than $GITHUB_ENV: a step of a job that writes $GITHUB_ENV or
$GITHUB_PATH makes ci_filter_coverage.py's job_reason non-empty for the whole job.

## Why elan is taken away before the control installs

elan's one source at the merge base is the release asset this fault holds. The removal
before it is what every elan step of this job carries: .github/actions/install-elan
exits 0 when $RUNNER_TEMP/elan/bin/elan is executable, and $RUNNER_TEMP is one path for
the whole job, so an elan step without it reads the elan the step before it installed
and installs nothing at all.

## Which seam sends uv's control to a dead port

uv's second source is PyPI, which .github/scripts/ci-tools.sh reads through
CI_TOOLS_URL_BASE and not through either seam the controls before it set (CI_TOOLS_GHCR_BASE, CI_TOOLS_NPM_BASE), so that is the one this control
sends to a dead port.

## The docker provider plugin's control, and why it does not expire

The docker provider plugin has no action at the merge base at all, so its control is the
head's own action with its second source on a dead port: its primary meets this fault
and get.pulumi.com is nowhere to go.
This control does not expire the day this change lands on main, which is the fault
.github/ci-notes/ci-job-tool-fallback.md#the-negative-control-and-why-it-does-not-expire
names in the controls that lean on the merge base: it reads nothing of the
base tree, and the property it states — an install whose primary is faulted and whose
second source cannot be reached fails — is a property of the head itself.

## The Pulumi CLI's control, and the two proxy spellings it carries

The Pulumi CLI is the row whose primary this job does not fault: get.pulumi.com is where
the docker plugin row's second source is, and faulting it would take that source down
inside the job that proves it.
So this control carries a proxy of its own on a dead port instead. The merge base's
action is a bare `curl` to get.pulumi.com and that is the one source it has, and a base
that reads the seams has its second source, the release asset, behind the same dead
proxy, so either way the install has nowhere to go.
The two lower-case spellings and no more: curl reads `http_proxy` in lower case only and
prefers the lower-case `https_proxy` to the upper-case one, and a step `env:` map that
held two spellings of one name is a workflow GitHub refuses to start at all, which is
why the rig writes the four spellings of the fault through $GITHUB_ENV.

## Why the control step reads the clock as well

A bound belongs only where firing it is loud, and on these eight it is not: each carries
`continue-on-error`, and this step's test, .github/scripts/tool-fallback-nowhere-controls.sh, accepts a step whose `outcome` is `failure`,
which is what the runner leaves when it ends a step at its bound. So this step reads the
clock as well. Without it an install that hung would satisfy its own control and leave
the job green, where before those bounds the same hang held the job to its own ceiling,
which cancels it and fails ci-ok, so somebody looked.

## The head's own installs under the fault

The head's own installs, under the same fault and with no continue-on-error: each must
succeed, and the seven whose primary the fault holds do it through the second source
their row names, with no seam of any kind. The Pulumi CLI, whose primary this fault does
not hold, is read by the case at the end of this list instead.
install-jj meets an empty mise data directory here, since nothing warmed one before the
fault: its `mise install` must reach the release asset mise fetches jj from, which the
fault holds, so that install fails and the action reads the copy. This step is therefore
the proof of jj's second source itself, rather than a warm directory answering for it.

## Why the faulted elan install must read the copy

elan's release asset is behind the fault, so this install must read the copy its row
names. The removal before it is the one every elan step carries, for the reason
.github/ci-notes/ci-job-tool-fallback.md#why-elan-is-taken-away-before-the-control-installs
gives.

## Why the faulted uv install reads the wheel PyPI serves

uv's release tarball is behind the fault, so this install must read the wheel PyPI
serves, which packages the same binary. Another provider and not another GitHub host is
the whole reason that row's second source is PyPI.

## Why the faulted docker provider plugin install reads get.pulumi.com

The docker provider plugin's release asset is behind the fault, so this install must
read get.pulumi.com, which serves that tarball byte for byte. The step carries an id
because the reading of the `Check every tool is the version its row pins` step is of the tarball it hands back.

## The Pulumi CLI's own fallback, and why the origin serves its second source

The Pulumi CLI's own fallback, which is a case rather than a plain install: its primary
is get.pulumi.com, and this job does not fault that host, so the seam puts the primary
on a dead port and the install must read its second source — the pulumi/pulumi release
asset, served here by the origin from the bytes `stage` fetched from that very release
before the fault and held to the row's digest.
It is the origin and not the real github.com because this job faults github.com for
every step after the fault started: inside this job the origin is what can serve a
`github-release` second source at all, and what it serves is that release's own bytes,
staged and verified.

## The sixteen corrupted payloads, and case 1 of them

The sixteen corrupted payloads, one case at a time: the rig says which rows the origin
serves with one byte changed, the install meets them through the seams the case's
own env: names, and the report after it is what passes or fails this job.

Case 1 of 16: crane's primary serves one byte changed, and the second source must stay
untried — a digest mismatch is a supply-chain signal, never a reason to read elsewhere.

## Case 6 of 16, and what the case before it left

Case 6 of 16: the copy of the jj binary serves one byte changed, with mise unable to
install the tool at all.
Its data directory goes, so `mise install` must reach the faulted host and fails, which
is what sends the action to the second source. Only jj is corrupted, so the action's own
mise fetch still serves from the origin.
Case 5, which ran before the fault, left ~/.local/bin/jj as it found it: install-jj
verifies the binary where mise put it and copies only after that passes, so a refused
jj never reaches that path.

## Case 7 of 16, and the branch setup-bun takes for it

Case 7 of 16: the installed Bun is one byte changed and still answers the pinned revision,
so setup-bun's third step takes the branch that installs nothing — a Bun of the pinned
version is already at ~/.bun/bin/bun, and the first step found it there, so the word this
one writes for it is `existing-install` — and the fourth step refuses those bytes.
Measured on 2026-11-08: the 107308936-byte binary of the bun-linux-x64 row answers
`--revision 1.4.4+38afee6c8` with its final byte changed, as it does whole, so the
branch this case needs is the branch it takes. The `Change the last byte of the installed Bun` step reads that answer again
and refuses the case rather than proving another one, should a later Bun read its own
last byte.

## Case 8 of 16, and why it takes the Bun pair away itself

Case 8 of 16: with no Bun of the pinned version on this runner, setup-bun's third step
must read the second source, and the npm package serves one byte changed.
This case takes the pair away itself rather than leaning on the case before it. That
case's refusal does remove the bun and the bunx it refused from ~/.bun/bin — setup-bun's
fourth step says so — but a case that must read the second source is held to its own
state and not to the one the case before it happened to leave.

## Case 11 of 16, and the digest the changed byte trips

Case 11 of 16: uv's primary serves one byte changed, and PyPI must stay untried.
What the changed byte trips here is the row's `primary_digest`, the sha256 of the
tarball GitHub serves, which is read before the binary is taken out of it: the row's own
`digest` is of that binary, so the archive carries a digest of its own and this is the
refusal this case is about.

## Case 12 of 16, where the changed byte is, and how each archive refuses it

Case 12 of 16: PyPI's wheel serves one byte changed, after the primary — the real host,
which the fault holds — did not serve the tarball at all.
The byte the origin changes here is inside the wheel's own member, and the rig says
why: a zip's last byte is in its end-of-directory record, which a reader taking a
member out never looks at, so a wheel changed there would hand the pinned binary back
whole and this case would install it. The member is what this row's digest answers for.
A corrupted wheel is then refused when its member is read and not by the digest:
python3's zipfile hands over no member whose CRC-32 the archive's own header no longer
matches. .github/scripts/ci-tools.sh says that in the words of what it is — the bytes
that address served are not a readable archive of what this row pins, quoting what
python3 reported — and not as a member the archive does not hold, which is the other
way a member can fail to come out and a different thing to tell a reader.
The bun npm case meets the same byte differently: there the change breaks the gzip
framing, and `tar -xzO` answers a damaged stream, an absent member and a destination
that filled up with one exit, so that refusal names no cause at all — it names the
member and the archive and leaves tar's own words on stderr to say why.
Either way nothing unverified is installed, which is what the case is for.

## Case 14 of 16, and the one payload the origin serves on two routes

Case 14 of 16: get.pulumi.com serves the plugin's tarball with one byte changed, after
the release asset — the real host, which the fault holds — did not serve it at all.
Pulumi serves one file at both addresses, so the origin serves one payload under one
name on its two routes, and which route a case read is what its report prints.

## Case 15 of 16, and why the untried reading is a reading

Case 15 of 16: the Pulumi CLI's primary serves one byte changed, and the release asset
must stay untried. This row's two sides are cases 11 to 14 of 16 reversed: its primary
is get.pulumi.com, so the seam that puts a corrupted primary in front of it is the URL
one, and the GitHub seam is here so that the untried reading is a reading: with it the
origin can serve this row's second source, and what the case says is that it was never
asked to. Without it that source would be the faulted real host, which answers nothing
whether the rule holds or not.

## Case 16 of 16, and why it is a dead port rather than the fault

Case 16 of 16: the Pulumi CLI's second source, the pulumi/pulumi release asset, serves
one byte changed, after its primary found a dead port where get.pulumi.com is.
The dead port and not the fault, because this job leaves get.pulumi.com alone; the
origin stands in for the release asset, as it does in the install that must succeed
(the Pulumi CLI's own fallback), and here it serves those bytes with one changed.

## What this runner holds after the sixteen refusals

What this runner holds after the sixteen refusals, said plainly: no corrupted byte was
installed, and what the installs themselves left is what this step reads.
.github/scripts/ci-tools.sh verifies the bytes before it puts them at their destination,
so every refused fetch of the sixteen cases left nothing behind, and the two installs that meet a
binary a third party put there refuse it without leaving it either. install-jj verifies
the binary where mise left it and copies to ~/.local/bin/jj only after that passes, so
the jj here is the pinned one the head's own install under the fault wrote and neither
jj case moved it. setup-bun removes the bun and the bunx it refused from ~/.bun/bin,
which is on this job's PATH, so the bun-primary case took away the Bun it refused; the
case after it took that pair away again before its own install and then refused the npm
bytes before anything was written in their place: this runner ends with no bun at all.
crane and changie are held to the version they answer, because their rows pin the release
tarball rather than the binary inside it; jj is held to its row's own digest, which is of
the installed binary.

## What the reading of the stand-in curl refuses

The reading of what the stand-in curl measured, before the fault stops, so that its own
refusal is this job's. It refuses the job for a stand-in that never reached PATH, which
leaves an empty log; for a job where no case kept the pinned ladder, or where more than
one did; for a kept measurement outside the window the rig holds it to; for more
fetches carrying CI_TOOLS_PROOF_FULL_LADDER than the bound the rig holds that count to,
which is a second step's `env:` carrying that variable, or a kept case that makes more
requests than it did; for a fetch that stayed on the ladder, spent one and then
failed, which is a fault shape the stand-in's rule did not recognise; and for a fetch
the stand-in shortened that then served the bytes, which is the reading that holds the
shorten-ladder step at its position. Every run of it that reads the log through prints
that count beside the bound, whether the reading passes or refuses.

## Why the fault stops before the last steps

The proxy stops with this job, and the steps after this one must not be routed through
it: the ci-ok-report step, `Report ci-ok when this job decides a merge-group run`, runs actions/upload-artifact, which would otherwise
upload through a proxy that is about to go. The rig writes the emptied variables, as it
wrote the proxy ones.
