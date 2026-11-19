#!/usr/bin/env bash
#
# check.sh is the gate of dispatch://INFRA-3134. It runs every command this reading
# recorded, reads the judgement for its shape, resolves every citation, asserts each
# landed step is in this tree with a passing proof, and proves its own bite.
#
#   bash docs/research/internet-of-cognition/check.sh --replay
#   bash docs/research/internet-of-cognition/check.sh --shape
#   bash docs/research/internet-of-cognition/check.sh --citations
#   bash docs/research/internet-of-cognition/check.sh --landed
#   bash docs/research/internet-of-cognition/check.sh --self-test
#   bash docs/research/internet-of-cognition/check.sh --admitted
#   bash docs/research/internet-of-cognition/check.sh --published
#   bash docs/research/internet-of-cognition/check.sh --coverage
#   bash docs/research/internet-of-cognition/check.sh --help
#
# The three endings. Every mode prints PASS and FAIL lines, each naming an assertion, and
# ends in one of three ways, and this is the one place they are defined:
#   all N assertions passed                exit 0: every assertion the mode declares passed
#   F of N assertions failed: <names>      exit 1: one or more of them failed
#   check.sh cannot run[ <what>]: <why>    exit 2: it could not run, so an assertion it
#                                          declares is not judged, and none failed;
#                                          <what> names the mode, as --replay, and <why>
#                                          is the reason
# N is the number of assertions the mode declares in the one assertion table below, not
# the number of lines it printed. The mode runs in a subshell, and its verdict reads what
# each declared assertion reported: one that reported a FAIL failed, and one that
# reported nothing, because its code crashed, exited or was skipped, is a FAIL naming it,
# so no assertion is passed by its silence; each prints its PASS only after all its parts
# have run. check.test.sh ends the same three ways, counting its cases where a mode counts
# its assertions: all N cases passed, F of N cases failed, or check.sh cannot run
# check.test.sh with each case that could not run and why; a case whose run of the gate
# could not run, in whole or in part, could not run itself, by the rule of the ending
# record below, and is neither passed nor failed.
#
# A run cannot run only when something outside the evidence it judges is missing or does
# not answer, and the reason it names is of one of four kinds, which its ending record
# (below) carries: prerequisite, a prerequisite its table declares (a tool, a variable, an
# install, the history, the clone root) or a tool its ending is written with; gate, env.sh
# or the install reader when it cannot load because env.sh is missing or python3 cannot
# start it, or a mode that does not exist; environment, the environment env.sh builds from
# the package index and the clones, or the run's own scratch and record (below); and
# source, the GitHub budget or a live source the gate reads itself (the live-read rule
# below). What the evidence holds is never such a reason: a line of claims.jsonl the one
# parse cannot read is a FAIL (below), and a reader that loaded and then stopped is a
# failure of the gate. A run that cannot run ends exit 2 with the reason as its last line,
# which names what is missing, and for a missing prerequisite ends with what provides it;
# for --self-test it names the run of the gate it made that could not run, ends with that
# run's own last line, and carries that run's kind. One more kind marks an assertion that
# was not judged without ending a run exit 2: evidence, each assertion --replay leaves
# after it failed claim-form on a row env.sh reads a value from, since env.sh builds what
# every recorded command reads from those rows (evidence_stops; "The one parse of
# claims.jsonl" below); that run ends exit 1 by its FAIL.
#
# The ending record. Every run of a mode writes how it ended as one record beside its
# text, a file of lines of tab-separated fields: status and its exit status; line and its
# last line; declared and a name, one line per assertion the mode declares; and one line
# per assertion it reported, passed or failed and the name, or unrun, the name, the kind of
# the reason and the reason. One function, run_ending, derives the exit status from those
# lines, which the run gathers in its own shell, by the precedence below, writes the record
# whole in one write and prints the last line, and check.test.sh's summary writes its own
# record by the same function, counting cases. The record goes to the file IOC_ENDING
# names, which the caller of one run sets on that run's command alone, so that no two runs
# share one, and otherwise under the run's scratch, removed with it; this script unsets
# IOC_ENDING when it starts, so no run it makes writes into its caller's record. --help and
# --coverage judge nothing and write no record. How a run ended is read from its record
# alone, never from its exit status alone or from its text, by one function, ending_read,
# which --self-test reads each run it makes by and check.test.sh each run its cases make;
# the text says what each FAIL found. Each such run is made by fixture_run or gate_run,
# which print the directory they made for it, holding its text, its record and the status
# it exited with. A run that wrote no record, or that exited with a status its record does
# not hold, crashed or was killed, and a crash is a failure, a bash syntax error among
# them; but a run whose directory takes no write, or that was never made because its
# directory could not be made under a scratch that takes no write, could not run, of the
# kind environment, since the place its record goes is its reader's own scratch.
# check.test.sh reads the run of each case by one rule, case_read: a run that crashed
# fails its case; a run an assertion or a case of which could not run for a reason the
# case does not plant makes the case one that could not run when the reason's kind is from
# outside the tree, prerequisite, environment or source, whatever its exit status, and a
# failed case when it is gate or evidence, the tree's own files; and a case that plants one
# failure asks that exactly that assertion failed. A reader who keeps a run's record sets
# IOC_ENDING on its command and reads it in the same way.
#
# The run's own scratch and record. Every run that judges, a mode or a script that sources
# this one as check.test.sh does, checks what its ending needs before anything else runs:
# the tools it is written and read with (ENDING_TOOLS, which --help prints); its scratch,
# made under TMPDIR and written once, so that a TMPDIR that is missing, cannot be written
# or is full is found there; and the record IOC_ENDING names, written once. A run that
# lacks one ends at once by early_end, which needs none of them: it prints the cannot-run
# line naming what failed, writes the record, when IOC_ENDING names one that takes a
# write, holding status 2 and the run itself, the mode or the script, declared and unrun
# with the kind prerequisite or environment, since nothing of it was declared yet, and
# exits 2 before anything writes under the scratch. A scratch that fills, or is removed,
# while the run goes on is found at the end: when it takes no write as the run ends, what
# the run wrote there may be lost, so nothing the run judged is read, every declared
# assertion or case could not run, of the kind environment, and the run ends exit 2, its
# last line naming the scratch. A record that cannot be written ends the run exit 2
# whatever it judged, its last line naming the record and then quoting the line the record
# would have held; its reader finds no record and reads it by the rule above. A scratch that
# fails part-way and takes writes again before the run ends is outside this rule: a FAIL a
# lost write caused then stands.
#
# Precedence. A run that reported any FAIL ends exit 1, and when part of it then could not
# run, its last line names after the failures each assertion, or each case, that could not
# run and why. Only a run with no FAIL and something that could not run ends exit 2. A run
# whose every assertion, or every case, passed ends exit 0. A run whose own scratch or
# record takes no write as it ends is no reading at all and ends exit 2 (above).
#
# Two rules hold what every mode reads, and this is the one place they are stated.
#
# The expected-set rule: no assertion passes on no input, and no count that decides a
# floor is read from the live input alone. Every count an assertion holds over is held to
# a committed expected set or floor: each step of sources.json's landed list to a page of
# the judgement that carries its Landed line; the candidate pages to PAGE_FLOOR and
# PAGE_CAP, three and seven, and every assertion over the pages (their eleven parts, their
# verdicts, what each commits us to, a refutation) to that floor; the three fixed pages;
# the six named mas-lab directories and the three questions; the four fixed interview
# rows; the three tests against our own records; the five deep-read directories of the
# order of work; and the rows of claims.jsonl that cite each record under check, which
# must be exactly the citations the record carries as it is read now. Where no committed
# set exists the floor is one: a claims.jsonl with no row, no command, no quotation or no
# absence search, a kind of citation with no row, a directory naming no dispatch://
# reference, an empty landed list or an empty question list fails the assertion over it
# by name.
#
# The live-read rule: every read of a live source the gate makes itself goes through one
# function, live_read, in the mode's own shell before any assertion over what it reads
# judges: the judgement and the two records under check from Dispatch, whose copies every
# row that reads a record reads too, Dispatch's answer to each dispatch:// reference, the
# Dispatch API, the GitHub budget, GitHub's answer to git ls-remote of the private
# repository rows read, the default branch and its fetch, the fetch of each
# pinned clone, and the run of coordination/count.py, the walker of the count, which
# reads the feed of the count's window, so that a read inside it that fails is that feed
# not answering. A source that does not answer is a mode that cannot run, naming the
# source, exit 2, or exit 1 after a FAIL, by the precedence above; it is never an
# assertion's failure. A source that answers that it holds no such thing has answered, and
# judges the deliverable: Dispatch's "not found", "No block", "no version N of" or
# "document ... not found" for a reference fails citation-dispatch, and for the judgement
# or a record under check leaves the text empty, which every assertion over it then fails
# by its floor or its committed rows. The live-read rule covers the gate's own reads. A row's
# own failed read of a live source, GitHub or Dispatch, is read by one rule of its own
# (source_answer), since only an answer from the source judges the deliverable: a curl of
# GitHub's REST API or of the Dispatch API, which the gate reads once more itself at the URL
# and with the header file the curl's record names; a dispatch command, by the first line it
# printed; and a python3 of a row whose command reads DISPATCH_URL, by the last urllib or
# transport error its output holds. A read that got no answer, a server error (5xx), a rate limit, and an
# answer about the reader's credential (a 401, or a 403 that names authentication, a
# permission or a token rather than a rate limit) are no verdict on the deliverable: the row
# prints no FAIL and the mode cannot run, of the kind source, naming the row, the source, the
# read and the line that shows it, exit 2, or exit 1 after an earlier FAIL, by the precedence
# above ("The GitHub tokens stand on no command line" below). Any other answer, such as a 404
# or Dispatch's NOT_FOUND, is the source's answer about the record, and the row fails, naming
# it; so does a row's failed read of anything else. The one live read that is no input is the
# report of each published page's live address, which reports an address that does not
# answer rather than stopping, as the Verification document asks of it.
#
# Prerequisites. Nothing in CI runs this script, and a gate nobody else can start passes by
# default, so each mode declares what it needs in one table, prerequisite_table below: the
# tools on PATH, the installed dependencies, the history, the clones and the environment
# variables, each with what provides it. One step, prerequisites, checks every line of the
# mode before its first live read and its first assertion, and one that is missing ends the
# mode exit 2, naming it and, at the end of the line, what provides it. --help prints the
# same table after this header, from the same function, so the list a reader reads is the
# list the mode checks. --self-test needs its own lines and every line of --replay, --shape,
# --citations and --landed, since it runs each of them; check.test.sh checks the lines of
# --self-test alone before its first case, since each run a case makes checks its own
# mode's. Before any of them, every run checks the tools its ending is written with ("The
# run's own scratch and record" above), which --help prints first.
#
# --landed's committed proof runs bun test in packages/pi-legion, which needs what
# `bun install --frozen-lockfile` writes at the root of the repository. The check tells
# "not installed" from "installed and the proof fails" by that package's own package.json:
# when a package it names in dependencies or devDependencies has no
# node_modules/<name>/package.json in packages/pi-legion or in a directory above it up to the
# root, the install is missing and the mode cannot run; when every one is there, the proof
# runs, and a proof that fails is the step failing, a FAIL of landed-proof. The check reads
# that the install is there and not that it is the lock's, so an install of another lock
# that breaks the proof fails the step too. A tree that holds no packages/pi-legion needs
# nothing installed, which is how the self-test's fixture, whose proof is `true`, runs.
#
# It reaches GitHub with git and the REST API and never with gh, which in a Legion pod is
# scoped to this project's owners and refuses ioc-project; so does every recorded command
# and every script one runs, and --admitted refuses a row that runs gh, since ADMITTED holds
# no entry for it. A repository's files are read from a clone at the commits sources.json
# pins, with git show, git log and git grep: the three projects' clones and agent-infra's,
# a private repository env.sh fetches blobless through the machine's git credential helper
# (env.sh's ioc_env_clone_private). What only the REST API answers, a pull request's reviews,
# its times and its size, is read with curl, sending the header file of the token git
# credential fill gives for that repository ("The GitHub tokens stand on no command line"
# below), or no header where it gives none, and then GitHub's sixty requests an hour for one
# address bound the read.
#
# Where it writes. It clones under the directory IOC_CLONES names, $TMPDIR/ioc-clones by
# default, at the commits sources.json pins. Everything env.sh builds is under IOC_CACHE,
# $TMPDIR/infra3134 by default, and so is every cache a tool of a recorded command writes:
# env.sh's ioc_env_cache_table names one variable per cache (go's GOMODCACHE and GOCACHE,
# uv's cache and the interpreters it downloads, pip's, huggingface's, torch's compiler
# cache, bun's, npm's and XDG_CACHE_HOME), env.sh exports each, and --replay exports them
# by the same table when its fixture builds nothing. No row names a cache of its own, so a
# fresh IOC_CACHE is a machine that never ran the replay; --help prints that table. Each
# recorded command runs with TMPDIR set to an empty directory of its own under the run's
# scratch, so no row reads what an earlier run or an earlier row left under $TMPDIR, and a
# row reads every input through env.sh's variables. Its scratch, each run's and the
# fixtures of --self-test with the IOC_CACHE of each, is under $TMPDIR and is removed when
# the run ends. It writes nothing else, but for what a tool writes outside every cache,
# which no cache variable moves, measured on 2026-11-15 over rows of every tool the replay
# runs at a fresh IOC_CACHE and over --landed: dispatch writes the state of the session
# that runs it (answers.json, results.jsonl and state.json) under DISPATCH_STATE_DIR on
# each read, and go keeps its telemetry counters under the user's configuration directory
# (go env GOTELEMETRYDIR), which its first run on a machine makes. No row reads either.
#
# Nothing a killed run leaves is trusted. A pod replacement kills a run part-way, and the
# next run reads the same IOC_CACHE and IOC_CLONES, so every entry env.sh keeps there is
# kept whole in one of three ways: it is written under a temporary name and renamed into
# place only when it is whole; it carries a stamp written last, which the next run reads
# before it reuses the entry, so an entry with no stamp, or with the stamp of other
# inputs, is built again from nothing; or it is written again at every run, as
# IOC_TM_OVERLAY is. IOC_OUR_TREE is kept both ways: renamed into place, and holding a
# stamp. env.sh's header lists each entry with the way that keeps it. An environment env.sh
# built is no longer its build once a row installs into it, so --replay removes env.sh's
# stamp of each environment a row installs into, as the install reader reads the row,
# before the row runs, and of each whose site-packages changed while it ran, after; the
# next run builds that environment again. A tool keeps its own cache whole by its own
# means, and what a row writes under IOC_CACHE is written again by a row of the same run
# before any row reads it. An install into an environment that no row the reader reads
# installs into, which a kill cuts short, is outside this rule, as an install the reader
# cannot see is outside install-pinned. The run's scratch is made fresh by each run, so
# what a killed run left there is never read.
#
# The fixture of --self-test. --self-test builds a small deliverable of its own under its
# scratch and runs the gate over it, and check.test.sh does the same in each of its cases.
# Each such run, fixture_run, has an environment of its own, made with env -i, and none
# of the caller's:
#   carried   PATH, without which a mode finds no tool; TMPDIR, under which the fixture's
#             scratch goes; HOME, under which git, uv and go keep their configuration;
#             DISPATCH_URL, DISPATCH_TOKEN_FILE, DISPATCH_HOST and DISPATCH_SESSION_ID,
#             without any one of which dispatch refuses to read, and DISPATCH_STATE_DIR,
#             without which it writes its session's state under HOME, each measured on
#             2026-11-15 by running the fixture's four modes with one taken away; and,
#             where they are set, IOC_CLONES, a place as TMPDIR and HOME are, under which
#             --citations clones each repository at the commit sources.json pins and each
#             clone's stamp holds, so that a fixture clones where the prerequisite line of
#             --citations tells a reader to point it; the proxy variables git, curl and go
#             read; and GOPROXY, GONOPROXY, GOSUMDB, GONOSUMDB, GOPRIVATE, GOINSECURE and
#             GOTOOLCHAIN, by which go reaches a module (FIXTURE_CARRIED)
#   set       IOC_ROOT, IOC_EVIDENCE and IOC_JUDGEMENT to the fixture's tree and judgement,
#             IOC_NO_ENV=1, IOC_PUBLISHED_COMMIT to the fixture's head, IOC_ENDING to the
#             record of that one run, and IOC_CACHE to the fixture's own directory cache/,
#             which the runs over that one fixture share and no run over another fixture
#             reads
#   cleared   every other variable: IOC_HISTORY_REF, IOC_OUR_TREE, every variable env.sh
#             exports and every cache of its table among them, so the fixture reads its
#             own history and keeps its caches in its own IOC_CACHE
# A case of check.test.sh plants a fault by setting one of FIXTURE_INPUTS on its own
# command (IOC_HISTORY_REF, IOC_CACHE, IOC_OUR_TREE, IOC_EVID, IOC_TC_WORK, the five
# program variables of PROGRAM_VARIABLES, IOC_FAKE_RATE_LIMIT, IOC_GITHUB_HEADER_AGE,
# UV_OFFLINE, UV_NO_INDEX and UV_FIND_LINKS), which fixture_run passes on; --self-test,
# and a shell that sources this script as check.test.sh does, unset the caller's values of
# them first, so a fixture reads one only from a case. fixture_run prints the directory of
# each run it makes, which holds the run's text and its ending record (above), and
# check.test.sh reads each case by that record. A run the self-test makes that cannot run,
# over the fixture or of check.test.sh or --admitted, stops the self-test there, naming the
# run and its reason, and ends it by the precedence above; it is never counted as an
# assertion that failed.
#
# No recorded command may read the scratch of the pod this reading was made in, because
# that pod's disk died with it. --replay therefore reads the two Dispatch records under
# check itself, then sources env.sh once, before it runs the first recorded command, and
# env.sh prepares the rest of what those commands read and this tree cannot hold: a python
# environment with sentence-transformers, torch and rdflib, the pinned all-MiniLM-L6-v2
# snapshot checked against the sha256 the evidence of the row s-model-checksum names,
# mas-lab installed from its clone, the SHACL shapes graph of the scope clone, and the
# review corpus one search reads, held to the corpus_floor the evidence of tc-corpus-1
# names. It caches all of that under IOC_CACHE, $TMPDIR/infra3134 by default, builds each
# thing once, by the rule above, and exports one variable per input, which every recorded
# command reads beside IOC_CLONES; the records it takes from --replay as IOC_DESIGN and
# IOC_COMMENT. env.sh's own header names each variable. A run whose environment env.sh
# cannot build ends there, exit 2, or exit 1 after a FAIL, by the precedence above, with
# env.sh's line naming what it could not build and, last, check.sh cannot run and the
# reason.
#
# --shape and --landed read the history of this pull request: the commit that adds the
# question list and the commits of the landed steps. After the squash merge the default
# branch holds none of them, so both modes read IOC_HISTORY_REF in place of HEAD when it
# is set: a ref that holds the pull request's history, such as the one a reader makes with
#   git fetch origin refs/pull/3771/head:refs/remotes/origin/pr-3771
# and names as IOC_HISTORY_REF=refs/remotes/origin/pr-3771. A ref that names no commit of
# this repository is a missing prerequisite of both modes.
#
# --landed runs no text it reads from Dispatch. The proof of each landed step is committed
# in the landed list of sources.json, one entry per landed page with the page's title, its
# commit and its command. The mode runs the committed command, and fails a page whose
# Landed commit or Proven by command differs from the committed entry for its title, and a
# page carrying a Landed line that no committed entry names.
#
# --admitted reads the one table of the forms the evaluator admits, ADMITTED in reader_py:
# it classifies the command of every row of claims.jsonl, prints each entry with the
# number of rows that use it, and fails an entry no row uses, a row the classifier refuses,
# and a line of the table in the header above run_recorded that differs from the one it
# measured; it fails as well a list of reserved names there that differs from the
# reader's, and a name of the runner no reserved prefix covers. --self-test runs it as one
# assertion.
#
# The one parse of claims.jsonl. Every mode that reads a row of claims.jsonl, --replay,
# --citations and --admitted, and --self-test through its fixture and its run of
# --admitted, first parses the whole file once, claims_judge, before it builds any
# environment or starts any reader. A line holding only space is no row. Every other line
# is a row when it is one JSON object, holds an id that is a string that is not empty and
# that no row above it holds, and holds each of requirement, source, kind, claim, mark,
# note, output and command only as a string and evidence only as an object, whose fields
# sha256 and corpus_floor, where it holds them, are 64 lowercase hexadecimal digits and a
# whole number, and checks only as an array of one or more objects, each holding row and
# mark, strings that are not empty, and no key but those two and refines, a string, the
# shape the list of keys below gives; expect may hold any value, since
# predicate-form judges it. A line that is no row is the evidence being wrong, which
# reading it again does not cure: it is one FAIL naming its line number and why, a second
# row of an id naming the line of the first, and the mode reads on and ends with its
# declared assertions counted, exit 1. In --replay the FAIL is claim-form's, since a line
# that is no claim carries no form of evidence; in --admitted admitted-rows', since it is a
# line the classifier reads no row of; in --citations citation-kinds', the assertion over
# how the mode reads a citation rather than over what one of a kind resolves to, since a
# line that is no row is a citation of no kind. Every reader after it, the install reader,
# jq over the rows and env.sh's reads of claims.jsonl when --replay sources it, reads only
# the rows the parse read (claims), so none of them meets a line it cannot read. --replay
# judges claim-marks-agree from those rows alone, first, before it reads a source or
# env.sh builds anything, so two rows that disagree on one claim are reported however the
# rest of the run goes. env.sh reads a value of a row only as a named field of its
# evidence, which this parse holds to its type, and never from its predicate: the sha256
# of s-model-checksum and the corpus_floor of tc-corpus-1, which env.sh's
# ioc_env_row_fields lists. The field and the predicate write one fact twice, so before
# it sources env.sh, --replay fails claim-form, naming the row and the field, when no row
# carries one of them or when the field is not a whole line of the value of its row's one
# predicate (env_rows_judge); a pin that differs from the value its row records is the
# evidence being wrong, never a refusal of env.sh. --replay then stops there, before
# env.sh builds anything, and every assertion after claim-form but claim-marks-agree
# could not run, of the kind evidence; the run ends exit 1. --shape, --landed and
# --published read no row of claims.jsonl; --published asks only that the default branch
# holds the file.
#
# The Dispatch bearer stands on no command line. A run writes it once as an HTTP header
# into a file under its own scratch, readable by its owner alone, exports that file's path
# as IOC_DISPATCH_HEADER, and every curl of the Dispatch API, the recorded ones included,
# reads it with -H @"$IOC_DISPATCH_HEADER". The scratch is removed when the run ends.
#
# The GitHub tokens stand on no command line either. Before it sources env.sh, --replay
# writes one header file per repository of env.sh's ioc_env_github_table under its own
# scratch (github_headers): each is made with umask 077 before a byte of it is written, so
# its owner alone reads it, and holds Authorization: Bearer and the token git credential
# fill gives for that repository, or nothing when it gives none. It exports each file's path
# as the variable the table names, IOC_GITHUB_AUTH_LEGION and IOC_GITHUB_AUTH_AGENT_INFRA,
# and env.sh's read of the review corpus, the read of GitHub's budget (github_budget) and
# every recorded curl of GitHub's REST API send the file of the repository they read with
# -H @"$<variable>". The files are written again while the run goes on: before each row,
# when one of them is GITHUB_HEADER_AGE seconds old, ten minutes, all are written as before
# into a directory beside them and each is renamed over its old file (github_headers_fresh),
# so a reader meets a whole file and each variable names one path through the run. A
# Legion pod's git credential fill gives a GitHub App installation token, which lives one
# hour, and a replay from a clean clone runs longer: at 882b668c one ran 3723 s, and the five
# rows that sent the file written at its start failed together after the hour. The pod's
# helper answers from a bundle its daemon refreshes in place, so a file written again
# carries a live token. A row whose curl of GitHub's REST API fails is read once more by the
# gate itself, at the URL and with the header file that curl's record names (source_answer),
# since the row's own pipeline reads GitHub's answer, and its failure line names the status
# and the first line of GitHub's message. Only an answer about the record judges the row:
# a refusal for GitHub's rate limit, an answer about the reader's credential, as a 401 for a
# token that expired, and a server error (5xx) are no verdict on the deliverable, and each
# ends the mode, cannot run, of the kind source. So does an outage, a read that got no HTTP
# answer: a row's curl that exited with any status but 22, the one --fail-with-body gives for
# GitHub's HTTP error, such as a transport error (6, 7, 28, 35, 52 or 56), whatever the
# gate's read again gets, and a read of the gate's own that gets no HTTP status at all. Its
# line names the row, the URL, the status the row's curl exited with and what the gate's read
# got, and no FAIL is printed. A row's curl of the Dispatch API runs with --fail-with-body
# too, sending the Dispatch header file, and is read again by the same rule. No line the gate
# prints and no output a row records holds a token: a failure names a header file by its
# path at most. The files go with the scratch, which the EXIT trap removes however the run
# ends, and check.test.sh's case 117 holds both: a replay of the rows that send them, with a
# token of the case's own, prints the token nowhere and leaves no header file behind. Case
# 118 holds that a row meets the files written again, case 119 that a failed read of
# GitHub's REST API names GitHub's answer, and that a 401 and a refusal for its rate limit
# cannot run, case 121 that a read that reaches no GitHub cannot run, case 122 that a row's
# read that got no HTTP answer cannot run though the gate's read again is answered, and case
# 125 that a read again answered 502 cannot run. Cases 123 and 124 hold the same rule for a
# row's read of Dispatch: through the dispatch command, by curl and through python3's urllib.
#
# The evidence it reads:
#
#   claims.jsonl   one line per claim, one JSON object. Keys: id, requirement, source,
#                  kind, claim, mark, evidence, command, expect, output, note, checks.
#                  evidence is an object, and its fields sha256 and corpus_floor, the
#                  two env.sh reads, are 64 lowercase hexadecimal digits and a whole
#                  number.
#                  checks is on a row that judges a claim of a record under check other
#                  than its own: one or more objects, each naming one such claim and the
#                  verdict this row gives it, as {"row": "d-17", "mark": "false"}. row
#                  is the id of a citation row of a record under check, <label>-<n>
#                  with <label> one of sources.json's records_under_check[].label; mark
#                  is one of verified, false, unverified and not-externally-checkable,
#                  the marks r1 fixes; and refines, where the row holds one part of the
#                  claim or narrows it, is a sentence, five words or more ending with a
#                  full stop, saying which part and why its verdict on that part differs
#                  from the named row's. Each is a string, and an entry holds no other
#                  key. claim-marks-agree holds each mark to the named row's: the two are
#                  one mark or the entry names its part, no part of a claim marked
#                  verified is marked otherwise, an entry marked false, with or without
#                  refines, names a row marked false, since a claim with a part marked
#                  false is false, and a record claim's own row carries no checks.
#                  kind is one of project-code, our-code, behaviour, published, absence
#                  and quotation, which are the five forms of the specification with the
#                  quotation form marked as a claim of the project rather than of its
#                  code. mark is one of verified, false, unverified,
#                  not-externally-checkable, supported, unsupported and unreleased.
#                  expect holds exactly one of equals, contains, matches, exit, at_most,
#                  at_least and one_of, or exit beside one other, and then both hold. A
#                  predicate written as a sentence fails here, and so does a value of
#                  another shape than its key takes (predicate_is_a_sentence): equals,
#                  contains and matches take a string, one_of a list of one or more
#                  strings, a bound a number, -?[0-9]+(.[0-9]+)?, and exit a whole number,
#                  [0-9]+, each of the last two as a JSON number or a string; matches,
#                  each candidate of one_of, a bound and exit hold no newline, and none
#                  but equals is empty. The output, and the value of an equals or a
#                  contains, are read as text without their trailing newlines, which
#                  carry no content; no value is handed to a tool as a list of patterns,
#                  a bound compares the two numbers as exact decimals, and a status that
#                  was never recorded meets no exit. What each key holds (evaluate):
#                    equals s     the whole output is s.
#                    contains s   s, every line of it, is one substring of the output.
#                    matches r    some line of the output matches r, as grep -E reads it.
#                    one_of [s]   the whole output is one of the strings s.
#                    at_most n    the output is exactly one number, and it is at most n.
#                    at_least n   the output is exactly one number, and it is at least n.
#                    exit n       the exit status recorded for the command is n.
#                  A predicate reads the line the row measures, never what a tool prints
#                  of its own progress: where a tool can write download, build or install
#                  lines into the stream a predicate reads on a machine whose caches are
#                  cold (go, uv, pip, bun, npm, curl without -s, git clone, huggingface),
#                  the row sends them where the predicate does not look, or its predicate
#                  matches the measured line, as {"matches":"^rows 38$"} does beside go's
#                  "go: downloading" lines.
#                  The pinned-read rule: a row that reads a live node, of Dispatch, GitHub,
#                  PyPI or any other address, is held to a fact fixed in time, such as a
#                  version's content, an event by its id or bounded by a time, or a file at
#                  a commit, never to the node's present state, and its predicate can still
#                  fail as the node moves on, an exact value rather than a bound on the
#                  side the node moves. A generator of committed evidence, a script whose
#                  output this tree commits, is held to the same rule: it reads each live
#                  source as it stood at a fixed instant, as coordination/count.py reads
#                  the issue tree and the cited authors from the feed's rows written before
#                  the window's end, so a later run measures what an earlier one measured;
#                  --replay runs count.py again and holds what it prints to the committed
#                  evidence/count.json, every figure but feed.events_read, the rows of the
#                  whole feed it read up to its run, which is no figure of the window
#                  (count-reruns).
#                  The predicate of a row with a command holds every part its claim names,
#                  and the claim names no more than that.
#   questions.md   the question list, which every candidate page answers from
#   judgement.md   read live from Dispatch, the document the reader reads
#
# The one assertion table below is where every PASS line, every planted fault of
# --self-test and every line of --coverage comes from, so "a bad case of every kind the
# gate reads" is true by construction rather than by two lists kept in step by hand.

set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# The variables a fixture takes from its caller, and the gate's inputs a case of
# check.test.sh sets on a fixture of its own (fixture_run; the header's "The fixture of
# --self-test" says why each). A shell that runs fixtures, --self-test or one that sources
# this script as check.test.sh does, first unsets its caller's FIXTURE_INPUTS, so that a
# fixture reads one only from a case that sets it on its own command.
FIXTURE_CARRIED="PATH HOME TMPDIR DISPATCH_URL DISPATCH_TOKEN_FILE DISPATCH_HOST DISPATCH_SESSION_ID DISPATCH_STATE_DIR IOC_CLONES http_proxy https_proxy no_proxy HTTP_PROXY HTTPS_PROXY NO_PROXY GOPROXY GONOPROXY GOSUMDB GONOSUMDB GOPRIVATE GOINSECURE GOTOOLCHAIN"
FIXTURE_INPUTS="IOC_HISTORY_REF IOC_CACHE IOC_OUR_TREE IOC_EVID IOC_TC_WORK IOC_PY IOC_MASLAB_PY IOC_MASLAB_CTL IOC_MASLAB_WHEEL_CTL IOC_TC_PY IOC_FAKE_RATE_LIMIT IOC_GITHUB_HEADER_AGE UV_OFFLINE UV_NO_INDEX UV_FIND_LINKS"
if [ "${BASH_SOURCE[0]}" != "$0" ] || [ "${1:-}" = --self-test ]; then
    # shellcheck disable=SC2086
    unset $FIXTURE_INPUTS
fi
ROOT="${IOC_ROOT:-$(cd "$HERE/../../.." && pwd)}"
CLONES="${IOC_CLONES:-${TMPDIR:-/tmp}/ioc-clones}"
EVIDENCE="${IOC_EVIDENCE:-$HERE}"
# The run's own scratch and record (the header's "The run's own scratch and record"): what
# the ending of a run needs is checked here, before anything else runs, and a run that
# lacks it ends at early_end, which needs none of it. ENDING_GIVEN is the record the
# caller named. RUN_WHAT names this run in a cannot-run line that no mode prints: the
# mode, as --admitted, when this script runs one, and else the script that sourced it, as
# check.test.sh. ENDING_TOOLS are the programs the ending is written and read with: cat
# and awk read the tables, grep, cut and sort read what each assertion reported, mktemp,
# mv and rm make the scratch and write the record, and tail and head quote a run's line.
ENDING_GIVEN="${IOC_ENDING:-}"
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
    RUN_WHAT="${1:-}"
else
    RUN_WHAT="${0##*/}"
fi
ENDING_TOOLS="mktemp rm mv cat awk grep cut sort tail head"
ENDING_TOOLS_FROM="apt-get install coreutils grep gawk"
# write_probe <directory>; writes one line into a new file of the directory and removes it,
# so that a directory that is missing, cannot be written or is full fails here. It prints
# why when it fails.
write_probe() {
    local why
    why="$({ printf 'probe\n' >"$1/.write-probe"; } 2>&1)" || {
        why="${why##*: }"
        printf '%s' "${why:-the write failed}"
        return 1
    }
    rm -f -- "$1/.write-probe"
}
# scratch_lost; the one probe of the run's own scratch as the run ends, made by finish, or
# by run_ending when no finish ran, as for check.test.sh: it sets SCRATCH_LOST to why the
# scratch takes no write, or to nothing when it takes one, and probes once per run, so the
# two read one answer.
scratch_lost() {
    [ -n "${SCRATCH_LOST+set}" ] && return 0
    SCRATCH_LOST="$(write_probe "$RUN_SCRATCH")" || :
}
# early_end <kind> <reason>; the ending of a run that stops before it has a scratch, a
# record or the tools to write them, made by bash's printf alone: it writes the record, in
# one write, to the file IOC_ENDING names when the caller named one, holding status 2, the
# line, and the run itself, RUN_WHAT, as declared and as one unrun of <kind> with that
# line, since the run stopped before it declared what it judges; it prints the cannot-run
# line and exits 2. A record it cannot write is named at the end of the line.
early_end() {
    local line="check.sh cannot run${RUN_WHAT:+ $RUN_WHAT}: ${2//[$'\t\n']/ }" why
    if [ -n "$ENDING_GIVEN" ]; then
        why="$({ printf 'status\t2\nline\t%s\ndeclared\t%s\nunrun\t%s\t%s\t%s\n' "$line" "$RUN_WHAT" "$RUN_WHAT" "$1" "$line" >"$ENDING_GIVEN"; } 2>&1)" ||
            line="$line; and its ending record, $ENDING_GIVEN, which IOC_ENDING names, cannot be written: ${why##*: }"
    fi
    printf '%s\n' "$line"
    exit 2
}
# scratch_make; the one check of what the ending needs, made by every run that judges, a
# mode or a script that sources this one, before anything else writes: each tool of
# ENDING_TOOLS on PATH, a prerequisite; the scratch, made under TMPDIR and written once, so
# that a scratch that is missing, cannot be written or is full ends here, naming it; and
# the record IOC_ENDING names, written once and removed; the last two of the kind
# environment.
scratch_make() {
    local tool missing="" why
    for tool in $ENDING_TOOLS; do
        command -v "$tool" >/dev/null 2>&1 || missing="$missing, $tool"
    done
    [ -z "$missing" ] ||
        early_end prerequisite "the tools its ending record is written and read with are not on PATH: ${missing#, }; provided by: $ENDING_TOOLS_FROM"
    RUN_SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/ioc-run.XXXXXX" 2>&1)" ||
        early_end environment "its own scratch cannot be made under TMPDIR, ${TMPDIR:-/tmp}: ${RUN_SCRATCH##*: }"
    trap 'rm -rf "$RUN_SCRATCH"' EXIT
    why="$(write_probe "$RUN_SCRATCH")" ||
        early_end environment "its own scratch $RUN_SCRATCH, made under TMPDIR, takes no write: $why"
    if [ -n "$ENDING_GIVEN" ]; then
        why="$({ printf 'probe\n' >"$ENDING_GIVEN.new"; } 2>&1)" || {
            ENDING_GIVEN=""
            early_end environment "its ending record, $IOC_ENDING, which IOC_ENDING names, cannot be written: ${why##*: }"
        }
        rm -f -- "$ENDING_GIVEN.new"
    fi
}
# This run's scratch, made fresh here and removed when the run ends, so that what one
# run reads from Dispatch is never read by the next. --help and --coverage judge nothing,
# write nothing and make none; every other run, a mode or a script that sources this one,
# makes it before anything else runs.
RUN_SCRATCH=""
if [ "${BASH_SOURCE[0]}" != "$0" ]; then
    scratch_make
else
    case "${1:---help}" in
    --help | -h | --coverage) ;;
    *) scratch_make ;;
    esac
fi
# This run's ending record (the header's "The ending record"): the file IOC_ENDING names,
# which the caller of this one run sets on its command, and otherwise one under this run's
# scratch. IOC_ENDING is unset here, so that no run this one makes writes into it, and
# what an earlier run left at that path is removed, so it is never read as this run's.
# ENDING_LINES gathers the record's lines in this shell until run_ending writes it whole,
# and SCRATCH_LOST, unset until scratch_lost probes the scratch as the run ends, holds
# that probe's answer.
ENDING="${ENDING_GIVEN:-${RUN_SCRATCH:+$RUN_SCRATCH/ending}}"
ENDING_LINES=""
unset IOC_ENDING SCRATCH_LOST
[ -z "$ENDING" ] || rm -f -- "$ENDING" "$ENDING.new"
# Every PASS and FAIL a mode reports is also written here, one line each, so its verdict
# reads them after the mode's subshell ends, however it ended.
REPORTED="$RUN_SCRATCH/reported"
[ -z "$RUN_SCRATCH" ] || : >"$REPORTED"
MODE=""  # the mode run_mode runs, which live_read names when a source does not answer

# family | mode | what it asserts | the fault --self-test plants, or - for none. A mode
# declares the assertions it runs by this table: the rows of its mode, and for
# --self-test its own rows and every row with a planted fault.
assertion_table() {
    cat <<'TABLE'
claim-form|replay|every line of claims.jsonl is a claim the one parse reads, and every claim carries one of the five forms of evidence the specification allows|a claim whose kind is none of them
claim-mark|replay|every claim carries one of the marks its requirement fixes|a claim with no mark
claim-marks-agree|replay|every row that checks a claim of a record under check names that claim's row and gives it the mark that row carries, or names the part of it the row holds, no part of a claim marked verified is marked otherwise, no part of a claim not marked false is marked false, and at least one row checks one|a row whose check of a record claim gives it another mark than its own row carries
predicate-form|replay|every row that carries a command carries exactly one predicate over its output, and every predicate is a bound, a set membership or an exit status, never a sentence|a predicate written as a sentence
install-pinned|replay|every package a recorded command installs or runs names its version and is read from a committed hashed lock, or is a local path built under a build constraint, and so does every build backend, and every environment a row installed into holds only packages its lock holds at their version and packages its pinned clone builds|a recorded command that runs a package with no version
replay-command|replay|every recorded command runs again, no read inside it failed unless its predicate is an exit status, and its output meets the predicate the row carries|a recorded output changed by one character
absence-still-empty|replay|every absence search reads its corpus and still finds nothing, and a read that failed before the search is no empty search|an absence search that now finds something
quotation-marked|replay|every quotation of a project's prose is marked a claim of the project and not of its code|a quotation marked as evidence of the code
citation-row|replay|every citation of the two records under check has a row with a mark|a verification row deleted
mas-lab-directory|replay|each of the six named directories of mas-lab has an evidence file at the clone commit|the evidence file of one directory deleted
mas-lab-question|replay|each of the three questions of mas-lab is answered by a file naming its evidence|an answer with no evidence named
interview-fixed-row|replay|the four rows the specification fixes for the authors' account are present by identifier|one fixed row deleted
our-data-input|replay|each test against our own records names the Dispatch key or the commit its input came from|a test whose input is a fixture of the project
count-reruns|replay|the count of our own failures runs again: coordination/count.py, run through the gate's live read, prints evidence/count.json again, every figure equal but feed.events_read, the rows of the whole feed it read up to the run, which grows with the feed and is no figure of the window|a figure of the window in evidence/count.json changed by one
count-window|replay|the count of our own failures names its window as two instants, in the window.json the count was run over|the window deleted
count-class|replay|the count classes are the three the specification names, in count.md and in the count coordination/count.py prints again|a fourth class
count-denominator|replay|every class of the count carries its denominator, and the count coordination/count.py prints again reads a whole number of records above zero|a class with no denominator
count-zero-premise|replay|a class of zero over a denominator above zero is stated on the front page as a premise that failed|a class of zero with no failed premise
page-count|shape|the judgement carries at least three and at most seven candidate pages|an eighth candidate page
page-parts|shape|every candidate page carries all eleven of its parts, and its landing component is Legion, Envoy or Dispatch|a page short of its reversal cost
page-verdict|shape|every candidate page carries exactly one verdict word of vendor, depend, port and reject|a page with two verdict words
verdict-commitment|shape|a depend names its upstream owner, a port names the change in our code, a reject names its reason|a reject with no reason
fixed-page|shape|the three pages the specification fixes exist and each answers by name the design already written for it|a fixed page that answers no named design
front-page|shape|the front page carries the ranking, the one thing to do first, the paragraph on where the two accounts differ, and what was not read|the paragraph deleted
question-list|shape|every page's question is on the question list, and every question is answered by a page or named on the front page|a page whose question is not on the list
triage-named|shape|every repository the triage dropped is named on the front page with its reason|a dropped repository the front page does not name
order-of-work|shape|the commit that adds the question list is an ancestor of every commit that adds a deep-read evidence file|a question-list commit that is not an ancestor
citation-our-code|citations|every citation into our own repository names a path that stands at the commit it names, and every part of its symbol that is an identifier, a line, a prefixed name or a file name stands in that path; a part written in words, and every part of a citation whose file is written in words, is reported as not checked|a cited symbol no file holds
citation-project-code|citations|every citation into one of the projects names a path the clone holds at the pinned commit, and every part of its symbol that is an identifier, a line, a prefixed name or a file name stands in that path; a part written in words, and every part of a citation whose file is written in words, is reported as not checked|a cited identifier the file does not hold
citation-published|citations|every citation of a published document names a committed copy whose checksum is the one sources.json recorded and whose text holds the sentence quoted|a byte appended to a committed copy
citation-dispatch|citations|every dispatch:// reference resolves to a node Dispatch holds|a reference that resolves to no node
citation-kinds|citations|every line of claims.jsonl is a row the one parse reads, and the extraction rule reads a citation of every kind it names, a number written in words among them|an extractor that reads digits alone
landed-proof|landed|every landed page names the commit and the command sources.json commits for its title, the commit is one this pull request added and not one the base branch held, and the committed command passes|a page whose proof command differs from the committed one
landed-refutation|landed|a page that refutes a step carries a command with output, never a search that found nothing|a refutation that rests on a search alone
landed-order|landed|the commit of the shortfall rows is an ancestor of the commit of the combining rule|the two commits in the other order
landed-no-refusal|landed|no landed step's diff reaches a refusal path of the daemon or of Dispatch, and none adds a refusal constant|a landed step that touches a refusal path
landed-third-change|landed|the third change, a refusal of a reviewer completion with no pair result, is not landed in any form|the third change landed as prose in a skill
published-tree|published|the default branch carries the evidence directory, the committed transcript, this script and every file a page links to|-
published-delivery|published|the record of the delivery is on the issue: the message, the comment on dispatch://INFRA-1098 and the comment recording the publication|-
admitted-form|admitted|every entry of ADMITTED, the forms the evaluator admits, is used by a row of claims.jsonl|-
admitted-rows|admitted|every line of claims.jsonl is a row the one parse reads, and the classifier refuses no row that carries a command|-
admitted-header|admitted|the table of admitted forms in the header above run_recorded is the one --admitted measures, line for line|-
admitted-reserved|admitted|the header's list of reserved names is the reader's, and every name the runner and the shims record with starts with a reserved prefix|-
self-test-baseline|self-test|the fixture deliverable passes --replay, --shape, --citations and --landed|-
gate-cases|self-test|every case of check.test.sh, the faults the tester found, passes|-
admitted-table|self-test|check.sh --admitted passes|-
coverage-complete|self-test|the coverage report names every assertion family the gate holds|-
TABLE
}

# declared_assertions <mode>; the assertions the mode declares, one per line.
declared_assertions() {
    assertion_table | awk -F'|' -v mode="$1" '
        mode == "self-test" { if ($2 == "self-test" || $4 != "-") print $1; next }
        $2 == mode { print $1 }'
}

usage() {
    # The header, down to the first line that is not a comment, then the two tables a
    # reader needs before a run: what each mode needs, and where every tool cache lives.
    awk 'NR == 1 { next } !/^#/ { exit } { sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"
    prerequisites_report
    caches_report
}

# mode | kind | name | what needs it | what provides it. The one table of what each mode
# needs before it judges anything: prerequisites reads it before the mode's first live
# read and first assertion, and --help prints it, so the two cannot drift. --self-test
# needs its lines and those of the four modes it runs (prerequisite_rows). The last column
# is what a reader runs to provide it, and may hold a |. How each kind is checked:
#   tool       the program is on PATH
#   variable   the variable is set, and one whose name ends _FILE names a readable file
#   installed  each package the directory's package.json names in dependencies or
#              devDependencies has its node_modules/<name>/package.json in that directory
#              or in one above it up to the root of the repository; a tree that holds no
#              such package.json needs nothing installed
#   history    the history ref, IOC_HISTORY_REF or else HEAD, names a commit of this
#              repository
#   clone      the directory IOC_CLONES names can be made, under which the mode clones each
#              repository at the commit sources.json pins; a fetch that fails is a live
#              source that did not answer
prerequisite_table() {
    cat <<'TABLE'
replay|tool|git|reads our own history, reads the private repository rows read with git ls-remote, clones it through the machine's credential helper with env.sh, and runs the recorded git commands|apt-get install git, with a credential helper that answers for https://github.com/theorem-labs/agent-infra.git, as a Legion pod's legion git-credential does
replay|tool|jq|reads claims.jsonl and runs the recorded jq commands|apt-get install jq
replay|tool|python3|runs the reader of every recorded command, the recorded python3 commands and coordination/count.py, which takes the count again|apt-get install python3
replay|tool|curl|reads the GitHub budget and the review corpus env.sh fetches, and runs the recorded reads of the Dispatch API and of GitHub's REST API|apt-get install curl
replay|tool|uv|builds env.sh's python environments and runs the recorded uv commands|curl -LsSf https://astral.sh/uv/0.13.5/install.sh | sh
replay|tool|go|runs the two recorded commands that count rows of our own table|curl -fsSL https://go.dev/dl/go1.27.3.linux-amd64.tar.gz | tar -xzC /usr/local, then PATH=/usr/local/go/bin:$PATH
replay|tool|dispatch|reads the two records under check, the judgement and the recorded Dispatch reads|the dispatch command of a Legion pod, /opt/legion/pi-envoy/bin/dispatch on its PATH
replay|variable|DISPATCH_URL|the address every recorded read of the Dispatch API reads, and so does coordination/count.py's read of the feed of the count's window|export DISPATCH_URL=<the Dispatch server>, which a Legion pod sets
replay|variable|DISPATCH_TOKEN_FILE|the bearer every recorded read of the Dispatch API sends, and so does coordination/count.py's read of the feed of the count's window|export DISPATCH_TOKEN_FILE=<a file holding the Dispatch bearer>, which a Legion pod sets
replay|clone|IOC_CLONES|env.sh clones mas-lab, scope, cognition-protocols and agent-infra under it|export IOC_CLONES=<a directory this user can write>, or leave it unset for $TMPDIR/ioc-clones
shape|tool|git|reads the history of this pull request|apt-get install git
shape|tool|jq|reads sources.json|apt-get install jq
shape|tool|dispatch|reads the judgement|the dispatch command of a Legion pod, /opt/legion/pi-envoy/bin/dispatch on its PATH
shape|history|IOC_HISTORY_REF|the commits of this pull request, which the default branch holds as one squash|git fetch origin refs/pull/3771/head:refs/remotes/origin/pr-3771, then export IOC_HISTORY_REF=refs/remotes/origin/pr-3771
citations|tool|git|resolves every citation into a clone or into this repository|apt-get install git
citations|tool|jq|reads claims.jsonl and sources.json|apt-get install jq
citations|tool|python3|runs citations.py, the one extraction rule|apt-get install python3
citations|tool|curl|reads each published page's live address|apt-get install curl
citations|tool|dispatch|resolves each dispatch:// reference|the dispatch command of a Legion pod, /opt/legion/pi-envoy/bin/dispatch on its PATH
citations|clone|IOC_CLONES|the mode clones each repository a project-code row cites under it|export IOC_CLONES=<a directory this user can write>, or leave it unset for $TMPDIR/ioc-clones
landed|tool|git|reads the commits of this pull request|apt-get install git
landed|tool|jq|reads the landed list of sources.json|apt-get install jq
landed|tool|dispatch|reads the judgement|the dispatch command of a Legion pod, /opt/legion/pi-envoy/bin/dispatch on its PATH
landed|tool|bun|runs the committed proof of a landed step, bun test in packages/pi-legion|curl -fsSL https://bun.sh/install | bash -s bun-v1.4.4
landed|installed|packages/pi-legion|the committed proof of a landed step imports its packages|bun install --frozen-lockfile, run at the root of the repository
landed|history|IOC_HISTORY_REF|the commits of this pull request, which the default branch holds as one squash|git fetch origin refs/pull/3771/head:refs/remotes/origin/pr-3771, then export IOC_HISTORY_REF=refs/remotes/origin/pr-3771
published|tool|git|fetches the default branch into the run's scratch|apt-get install git
published|tool|jq|reads sources.json|apt-get install jq
published|tool|curl|reads the Dispatch API|apt-get install curl
published|variable|DISPATCH_URL|the address of the Dispatch API|export DISPATCH_URL=<the Dispatch server>, which a Legion pod sets
published|variable|DISPATCH_TOKEN_FILE|the bearer the reads of the Dispatch API send|export DISPATCH_TOKEN_FILE=<a file holding the Dispatch bearer>, which a Legion pod sets
admitted|tool|python3|runs the classifier over every row|apt-get install python3
self-test|tool|git|builds the fixture deliverable's repository|apt-get install git
self-test|tool|jq|builds and reads the fixture's sources.json and claims.jsonl|apt-get install jq
self-test|tool|python3|runs the reader over the fixture's rows|apt-get install python3
TABLE
}

# prerequisite_rows <mode>; the lines of prerequisite_table the mode needs, without the
# mode, as kind|name|what needs it|what provides it, each kind and name once. cases names
# what check.test.sh checks before its first case, the lines of --self-test alone, which
# its fixture builder needs; each run a case makes checks the lines of its own mode.
prerequisite_rows() {
    local modes=" $1 " mode kind name why provides seen=" "
    [ "$1" = self-test ] && modes=" self-test replay shape citations landed "
    [ "$1" = cases ] && modes=" self-test "
    while IFS='|' read -r mode kind name why provides; do
        case "$modes" in *" $mode "*) ;; *) continue ;; esac
        case "$seen" in *" $kind:$name "*) continue ;; esac
        seen="$seen$kind:$name "
        printf '%s|%s|%s|%s\n' "$kind" "$name" "$why" "$provides"
    done < <(prerequisite_table)
}

# prerequisite_missing <kind> <name>; prints what is missing and returns 0 when the
# prerequisite is missing, and returns 1 when it is there, by the rule of its kind above.
prerequisite_missing() {
    local kind="$1" name="$2" value dir deps dep up missing=""
    case "$kind" in
    tool)
        command -v "$name" >/dev/null 2>&1 && return 1
        printf 'the tool %s is not on PATH' "$name"
        ;;
    variable)
        value="${!name:-}"
        if [ -z "$value" ]; then
            printf 'the variable %s is not set' "$name"
        elif [[ "$name" == *_FILE ]] && [ ! -r "$value" ]; then
            printf 'the variable %s names %s, which is no readable file' "$name" "$value"
        else
            return 1
        fi
        ;;
    installed)
        dir="$ROOT/$name"
        [ -e "$dir/package.json" ] || return 1
        deps="$(jq -r '(.dependencies // {}) + (.devDependencies // {}) | keys[]' "$dir/package.json" 2>/dev/null)" || {
            printf '%s/package.json does not read, so what it needs installed is unknown' "$name"
            return 0
        }
        while IFS= read -r dep; do
            [ -n "$dep" ] || continue
            up="$dir"
            while :; do
                [ -r "$up/node_modules/$dep/package.json" ] && continue 2
                [ "$up" = "$ROOT" ] || [ "$up" = / ] && break
                up="$(dirname "$up")"
            done
            missing="$missing, $dep"
        done <<<"$deps"
        [ -n "$missing" ] || return 1
        printf 'the packages %s/package.json names are not installed (%s)' "$name" "${missing#, }"
        ;;
    history)
        git -C "$ROOT" rev-parse --verify --quiet "$(history_ref)^{commit}" >/dev/null && return 1
        printf "the history ref '%s' names no commit of this repository" "$(history_ref)"
        ;;
    clone)
        mkdir -p "$CLONES" 2>/dev/null && return 1
        printf '%s names %s, which this run cannot make' "$name" "$CLONES"
        ;;
    *)
        printf 'prerequisite_table names the kind %s, which no rule here checks' "$kind"
        ;;
    esac
}

# prerequisites_missing <mode>; prints each prerequisite of the mode that is missing, each
# followed by what provides it, and returns 0 when one is missing and 1 when none is.
prerequisites_missing() {
    local kind name _why provides detail missing=""
    while IFS='|' read -r kind name _why provides; do
        detail="$(prerequisite_missing "$kind" "$name")" || continue
        missing="$missing; $detail; provided by: $provides"
    done < <(prerequisite_rows "$1")
    [ -n "$missing" ] || return 1
    printf '%s' "${missing#; }"
}

# prerequisites <mode>; the one step that checks what the mode needs, run by run_mode
# before the mode's first live read and first assertion, and by check.test.sh before its
# first case. A mode that lacks one cannot run: its last line names each missing
# prerequisite, each followed by what provides it.
prerequisites() {
    local missing
    missing="$(prerequisites_missing "$1")" || return 0
    cannot_run prerequisite " --$1: $missing"
}

# prerequisites_report; prerequisite_table as --help prints it, one block per mode, after
# the tools every mode needs to make its scratch and write its record (ENDING_TOOLS).
prerequisites_report() {
    local mode kind name why provides
    printf '\nWhat each mode needs (prerequisite_table). A mode checks its own before it reads or\n'
    printf 'judges anything, and one that is missing ends the mode exit 2, naming it and, last,\n'
    printf 'what provides it. --self-test needs its own and those of --replay, --shape,\n'
    printf -- '--citations and --landed, which it runs. --coverage and --help need nothing.\n'
    printf '\nevery mode, before anything else (ENDING_TOOLS)\n'
    printf '  %-9s %-20s %s\n' tool "${ENDING_TOOLS// /, }" "make the run's own scratch and write and read its ending record"
    printf '  %-9s %-20s provided by: %s\n' '' '' "$ENDING_TOOLS_FROM"
    for mode in replay shape citations landed published admitted self-test; do
        printf '\n--%s\n' "$mode"
        while IFS='|' read -r kind name why provides; do
            printf '  %-9s %-20s %s\n' "$kind" "$name" "$why"
            printf '  %-9s %-20s provided by: %s\n' '' '' "$provides"
        done < <(prerequisite_table | awk -F'|' -v mode="$mode" '$1 == mode { sub(/^[^|]*\|/, ""); print }')
    done
}

# caches_report; env.sh's ioc_env_cache_table, the one list of where every tool cache
# lives, as --help prints it.
caches_report() {
    local name dir what
    printf '\nWhere every tool cache lives (env.sh, ioc_env_cache_table): each variable is\n'
    printf 'exported for every recorded command, under IOC_CACHE, $TMPDIR/infra3134 by default.\n\n'
    if ! env_functions; then
        printf '  env.sh did not load, so the table is not read\n'
        return 0
    fi
    while IFS='|' read -r name dir what; do
        printf '  %-22s $IOC_CACHE/%-16s %s\n' "$name" "$dir" "$what"
    done < <(ioc_env_cache_table)
}

pass() {
    printf 'PASS %s\n' "$1" >>"$REPORTED"
    printf 'PASS %s: %s\n' "$1" "$2"
}

fail() {
    printf 'FAIL %s\n' "$1" >>"$REPORTED"
    printf 'FAIL %s: %s\n' "$1" "$2"
}

# finish <mode>; the verdict of the mode over the assertions it declares, by the three
# endings of the header and their precedence. One that reported a FAIL failed; one that
# reported only PASS passed; one that reported nothing could not run when the mode
# stopped at cannot_run or evidence_stops, with the kind and the reason it stopped for,
# and otherwise failed and is named in a FAIL line here. An assertion the mode reported
# but does not declare fails too. When the run's own scratch takes no write as the mode
# ends, what each assertion reported there may be lost, so finish reads none of it, and
# run_ending reads every declared assertion as one that could not run for that. Each
# verdict is a line of the run's ending record, which run_ending writes, ending the run
# with its last line and exit 0, 1 or 2. It relies on one verdict per assertion, printed
# after all of that assertion's parts have run: a PASS printed before a later part would
# stand however that part ended, so every assertion of more than one part prints its PASS
# last.
finish() {
    local family declared kind="" stopped=""
    declared="$(declared_assertions "$1")"
    scratch_lost
    if [ -n "$SCRATCH_LOST" ]; then
        while IFS= read -r family; do
            [ -z "$family" ] || ending_add declared "$family"
        done <<<"$declared"
        run_ending assertions
        return
    fi
    [ -s "$RUN_SCRATCH/stopped" ] && IFS=$'\t' read -r kind stopped <"$RUN_SCRATCH/stopped"
    while IFS= read -r family; do
        [ -n "$family" ] || continue
        ending_add declared "$family"
        if grep -qxF "FAIL $family" "$REPORTED"; then
            ending_add failed "$family"
        elif grep -qxF "PASS $family" "$REPORTED"; then
            ending_add passed "$family"
        elif [ -n "$stopped" ]; then
            ending_add unrun "$family" "$kind" "$stopped"
        else
            printf 'FAIL %s: it reported no verdict, so its code crashed, exited or was skipped before it passed or failed\n' "$family"
            ending_add failed "$family"
        fi
    done <<<"$declared"
    while IFS= read -r family; do
        [ -n "$family" ] || continue
        printf 'FAIL %s: --%s reported it, and the assertion table declares no such assertion for --%s\n' "$family" "$1" "$1"
        ending_add failed "$family"
    done < <(cut -d' ' -f2 "$REPORTED" | sort -u | grep -vxF -f <(printf '%s\n' "$declared"))
    run_ending assertions
}

# cannot_run <kind> <reason>; the third ending, beside every assertion passed (exit 0) and
# one or more failed (exit 1): the mode could not run, so no assertion it had not yet
# judged is judged. <kind> is one of the four the header names: prerequisite, gate,
# environment or source. It prints the reason, records it with its kind for finish, and
# ends the mode's subshell; run_ending prints no line after it when nothing failed, so it
# stands last.
cannot_run() {
    local line="check.sh cannot run${2//[$'\t\n']/ }"
    printf '%s\n' "$line"
    printf '%s\t%s\n' "$1" "$line" >"$RUN_SCRATCH/stopped"
    exit 2
}

# evidence_stops <reason>; the end of a mode at evidence it has just failed, by name, where
# every assertion still to come reads what that evidence feeds: it prints the reason,
# records it for finish with the kind evidence, so that each assertion not yet judged is
# one that could not run for it, and ends the mode's subshell. It is called only after a
# FAIL, so the run ends exit 1 by the precedence of the header, never exit 2.
evidence_stops() {
    local line="${1//[$'\t\n']/ }"
    printf '%s\n' "$line"
    printf 'evidence\t%s\n' "$line" >"$RUN_SCRATCH/stopped"
    exit 2
}

# run_mode <mode>; runs the mode in a subshell, after the one check of its prerequisites,
# so that however its code ends the verdict still reads what it reported, then ends the
# run by finish's verdict: exit 0, 1 or 2.
run_mode() {
    MODE="$1"
    (prerequisites "$1" && "mode_${1//-/_}")
    finish "$1"
}

# ending_add <field> <name> [<kind> <reason>]; one line of this run's ending record (the
# header's "The ending record"), gathered in ENDING_LINES, in this shell, until run_ending
# writes the record whole: declared and a name; passed or failed and a name; or unrun, a
# name, the kind of its reason and the reason. A tab or a line break inside a field is
# written as a space, so each line holds its fields alone.
ending_add() {
    local field line=""
    for field in "$@"; do
        line="$line${line:+$'\t'}${field//[$'\t\n']/ }"
    done
    ENDING_LINES="$ENDING_LINES$line"$'\n'
}

# run_ending <assertions|cases>; the one writer of how a run ended, a mode's or, counting
# cases, check.test.sh's. From the lines ending_add gathered it derives the exit status by
# the precedence of the header, 1 when anything failed, else 2 when anything could not
# run, else 0, and the last line of the three endings: for a mode, the failed assertions
# named and then each that could not run with the one reason the mode stopped for, or that
# reason alone, which cannot_run printed when the mode stopped; for check.test.sh, each
# case that could not run with its own run's reason. When the run's own scratch takes no
# write as it ends, what the run wrote there may be lost, so every unit it declared could
# not run, of the kind environment, the run ends exit 2 whatever it judged, and the last
# line names the scratch. It writes the record whole, in one write under a temporary name
# renamed into place, prints the last line, and returns the status. A record it cannot
# write ends the run exit 2, whatever it judged, with a last line that names the record
# and then quotes the line the record would have held.
run_ending() {
    local unit="$1" lines="$ENDING_LINES" field name kind reason total=0 failed=0 unrun=0 failed_names="" unrun_names="" unrun_cases="" first="" status line lost="" why
    scratch_lost
    if [ -n "$SCRATCH_LOST" ]; then
        lost="check.sh cannot run${RUN_WHAT:+ $RUN_WHAT}: its own scratch $RUN_SCRATCH takes no write as it ends: $SCRATCH_LOST, so nothing it judged is read, since what it wrote there may be lost"
        lines=""
        while IFS=$'\t' read -r field name kind reason; do
            [ "$field" = declared ] || continue
            lines="$lines$field"$'\t'"$name"$'\n'"unrun"$'\t'"$name"$'\t'"environment"$'\t'"$lost"$'\n'
        done <<<"$ENDING_LINES"
    fi
    while IFS=$'\t' read -r field name kind reason; do
        case "$field" in
        passed) total=$((total + 1)) ;;
        failed)
            total=$((total + 1)) failed=$((failed + 1))
            failed_names="$failed_names $name"
            ;;
        unrun)
            total=$((total + 1)) unrun=$((unrun + 1))
            unrun_names="$unrun_names $name"
            unrun_cases="$unrun_cases; $name: $reason"
            [ -n "$first" ] || first="$reason"
            ;;
        esac
    done <<<"$lines"
    if [ -n "$lost" ]; then
        status=2
        line="$lost"
    elif [ "$failed" -gt 0 ]; then
        status=1
        if [ "$unit" = cases ]; then
            line="$failed of $total cases failed${unrun_cases:+; $unrun could not run: ${unrun_cases#; }}"
        else
            line="$failed of $total assertions failed:$failed_names${first:+; $unrun could not run:$unrun_names, since $first}"
        fi
    elif [ "$unrun" -gt 0 ]; then
        status=2
        if [ "$unit" = cases ]; then
            line="check.sh cannot run check.test.sh: $unrun of $total cases could not run: ${unrun_cases#; }"
        else
            line="$first"
        fi
    else
        status=0
        line="all $total $unit passed"
    fi
    line="${line//[$'\t\n']/ }"
    if ! why="$({ printf 'status\t%s\nline\t%s\n%s' "$status" "$line" "$lines" >"$ENDING.new"; } 2>&1 && mv -f -- "$ENDING.new" "$ENDING" 2>&1)"; then
        rm -f -- "$ENDING.new" 2>/dev/null
        printf '%s\n' "check.sh cannot run${RUN_WHAT:+ $RUN_WHAT}: its ending record, $ENDING, cannot be written: ${why##*: }, so no reader reads this run, which ended: $line"
        return 2
    fi
    { [ "$status" = 2 ] && [ "$unit" = assertions ] && [ -z "$lost" ]; } || printf '%s\n' "$line"
    return "$status"
}

# ending_read <run>; the one reading of how a run ended, a mode of this gate or
# check.test.sh, from the record the run wrote, never from its exit status alone or its
# text. <run> is the directory fixture_run or gate_run made for it. It sets RUN_ENDING to
# passed, failed or cannot-run by the record's status, 0, 1 or 2; RUN_STATUS and RUN_LINE
# to that status and the run's last line; RUN_FAILED to the names that failed, one per
# line; and RUN_UNRUN to one line <name><TAB><kind><TAB><reason> per name that could not
# run. A run that wrote no record, or that exited with a status its record does not hold,
# crashed or was killed: it reads as failed, with RUN_CRASH=1, RUN_STATUS the status it
# exited with and a RUN_LINE that says so and quotes the last line of its text. Two runs
# with no record are read otherwise, since the place the record goes is this run's own
# scratch: a run whose directory takes no write, and a run never made because its
# directory could not be made (<run> empty) while this run's scratch takes no write, each
# could not run, of the kind environment, naming the directory.
ending_read() {
    local run="$1" field a b c exited="" lost
    RUN_STATUS="" RUN_LINE="" RUN_FAILED="" RUN_UNRUN="" RUN_CRASH=0 RUN_ENDING=failed
    if [ -z "$run" ] || [ ! -d "$run" ]; then
        if lost="$(write_probe "$RUN_SCRATCH")"; then
            RUN_CRASH=1 RUN_STATUS=none
            RUN_LINE="no run was made: its directory '$run' could not be made, and the scratch of this run takes writes, so its caller named a place it cannot make"
        else
            RUN_ENDING=cannot-run RUN_STATUS=2
            RUN_LINE="check.sh cannot run: no run was made, since its directory could not be made under the scratch of this run, $RUN_SCRATCH, which takes no write: $lost"
            RUN_UNRUN="run"$'\t'"environment"$'\t'"$RUN_LINE"$'\n'
        fi
        return 0
    fi
    [ -s "$run/status" ] && read -r exited <"$run/status"
    if [ -f "$run/ending" ]; then
        while IFS=$'\t' read -r field a b c; do
            case "$field" in
            status) RUN_STATUS="$a" ;;
            line) RUN_LINE="$a" ;;
            failed) RUN_FAILED="$RUN_FAILED$a"$'\n' ;;
            unrun) RUN_UNRUN="$RUN_UNRUN$a"$'\t'"$b"$'\t'"$c"$'\n' ;;
            esac
        done <"$run/ending"
    elif ! lost="$(write_probe "$run")"; then
        RUN_ENDING=cannot-run RUN_STATUS=2
        RUN_LINE="check.sh cannot run: the directory of the run, $run, takes no write: $lost, so the run could write no ending record there"
        RUN_UNRUN="run"$'\t'"environment"$'\t'"$RUN_LINE"$'\n'
        return 0
    fi
    case "$RUN_STATUS" in
    0) RUN_ENDING=passed ;;
    1) RUN_ENDING=failed ;;
    2) RUN_ENDING=cannot-run ;;
    *) RUN_CRASH=1 ;;
    esac
    [ -n "$exited" ] && [ "$exited" != "$RUN_STATUS" ] && RUN_CRASH=1
    [ "$RUN_CRASH" = 1 ] || return 0
    if [ -n "$RUN_STATUS" ]; then
        RUN_LINE="the run exited ${exited:-with no status}, and its ending record holds $RUN_STATUS, so it crashed after it wrote it"
    else
        RUN_LINE="the run exited ${exited:-with no status} and wrote no ending record, so it crashed or was killed"
    fi
    RUN_LINE="$RUN_LINE; its text ends: $(tail -1 "$run/text" 2>/dev/null | head -c 240)"
    RUN_ENDING=failed RUN_STATUS="${exited:-none}" RUN_FAILED="" RUN_UNRUN=""
}

claims_file() { printf '%s/claims.jsonl' "$EVIDENCE"; }

# claims_parsed; the one parse of claims.jsonl the header states, made once per run: it
# writes CLAIMS_ROWS, each line that is a row, as it stands, and CLAIMS_UNREAD, one line
# <line number><TAB><why> for each line that is not, a second row of an id among them,
# which names the line of the first. A claims.jsonl that is not there holds no row. It
# returns non-zero, and writes neither, when the parse could not run to its end, which
# leaves every reader with no row.
CLAIMS_ROWS="$RUN_SCRATCH/claims.rows"
CLAIMS_UNREAD="$RUN_SCRATCH/claims.unread"
claims_parsed() {
    [ -e "$CLAIMS_ROWS" ] && return 0
    python3 - "$(claims_file)" "$CLAIMS_ROWS.part" "$CLAIMS_UNREAD.part" 2>"$CLAIMS_ROWS.err" <<'PY' || return 1
import json, re, sys

path, rows_path, unread_path = sys.argv[1:4]
STRINGS = ("id", "requirement", "source", "kind", "claim", "mark", "note", "output", "command")
OBJECTS = ("evidence",)
# The named fields of an evidence object that a reader takes a value from, each held to its
# type wherever it stands: env.sh reads the sha256 of s-model-checksum and the corpus_floor
# of tc-corpus-1 (its ioc_env_row_fields).
TYPED = {
    "sha256": ("64 lowercase hexadecimal digits",
               lambda value: isinstance(value, str) and re.fullmatch("[0-9a-f]{64}", value) is not None),
    "corpus_floor": ("a whole number",
                     lambda value: isinstance(value, int) and not isinstance(value, bool) and value >= 0),
}

# The keys of an entry of checks, which names a claim of a record under check this row
# judges and the mark it gives it; claim-marks-agree reads each entry against that claim's
# own row. row and mark are in every entry, refines only where the row holds one part.
CHECK_KEYS = ("row", "mark", "refines")

def kind(value):
    for name, types in (("a boolean", bool), ("a number", (int, float)), ("a string", str),
                        ("an array", list), ("an object", dict)):
        if isinstance(value, types):
            return name
    return "null"

def constant(name):
    raise ValueError("%s is no JSON value" % name)

def checks_why(checks):
    # None when checks has the shape the header's list of keys gives it, or why it has not.
    if not isinstance(checks, list) or not checks:
        return "its checks is %s, and a checks is an array of one or more objects" % (
            "an empty array" if isinstance(checks, list) else kind(checks))
    for number, entry in enumerate(checks, 1):
        if not isinstance(entry, dict):
            return "entry %d of its checks is %s, and an entry of a checks is an object" % (number, kind(entry))
        for key in entry:
            if key not in CHECK_KEYS:
                return "entry %d of its checks holds the key %s, and an entry of a checks holds row, mark and refines alone" % (
                    number, json.dumps(key)[:80])
        for key in CHECK_KEYS:
            if key in entry and not isinstance(entry[key], str):
                return "the %s of entry %d of its checks is %s, and the %s of an entry of a checks is a string" % (
                    key, number, kind(entry[key]), key)
        for key in ("row", "mark"):
            if not entry.get(key):
                return "entry %d of its checks has %s, and an entry of a checks names a row and a mark, each a string that is not empty" % (
                    number, ("an empty " if key in entry else "no ") + key)
    return None

def read(text):
    # The row the line holds and None, or None and why the line holds no row.
    try:
        row = json.loads(text, parse_constant=constant)
    except ValueError as why:
        return None, "it is no JSON: " + " ".join(str(why).split())[:160]
    if not isinstance(row, dict):
        return None, "it holds %s, and a row is one JSON object" % kind(row)
    if not isinstance(row.get("id"), str) or not row["id"]:
        return None, "its id is %s, and a row is named by an id that is a string that is not empty" % (
            "missing" if "id" not in row else "an empty string" if row["id"] == "" else kind(row["id"]))
    for key in STRINGS:
        if key in row and not isinstance(row[key], str):
            return None, "its %s is %s, and a %s is a string" % (key, kind(row[key]), key)
    for key in OBJECTS:
        if key in row and not isinstance(row[key], dict):
            return None, "its %s is %s, and an %s is an object" % (key, kind(row[key]), key)
    evidence = row.get("evidence", {})
    for key, (what, holds) in TYPED.items():
        if key in evidence and not holds(evidence[key]):
            return None, "its evidence's %s is %s, and an evidence's %s is %s" % (
                key, json.dumps(evidence[key])[:80], key, what)
    if "checks" in row:
        why = checks_why(row["checks"])
        if why is not None:
            return None, why
    return row, None

try:
    data = open(path, "rb").read()
except FileNotFoundError:
    data = b""
first = {}
with open(rows_path, "w", encoding="utf-8") as rows, open(unread_path, "w", encoding="utf-8") as unread:
    for number, raw in enumerate(data.split(b"\n"), 1):
        try:
            text = raw.decode("utf-8")
        except UnicodeDecodeError:
            unread.write("%d\tit is not UTF-8\n" % number)
            continue
        if not text.strip():
            continue
        row, why = read(text)
        if row is not None and row["id"] in first:
            why = "its id %s is the id of line %d too, and an id names one row" % (json.dumps(row["id"]), first[row["id"]])
        elif row is not None:
            first[row["id"]] = number
        if why is None:
            rows.write(text.strip() + "\n")
        else:
            unread.write("%d\t%s\n" % (number, why))
PY
    mv "$CLAIMS_UNREAD.part" "$CLAIMS_UNREAD" && mv "$CLAIMS_ROWS.part" "$CLAIMS_ROWS"
}

# claims_judge <family>; the one parse, run by each mode that reads a row of claims.jsonl
# before it builds any environment or starts any reader: one FAIL of <family> for each
# line that is no row, naming the line and why, and one when the parse itself could not
# run to its end. It returns non-zero when it reported a FAIL.
claims_judge() {
    local number why unread=0
    if ! claims_parsed; then
        fail "$1" "the parse of claims.jsonl did not run to its end, so no row of it is read: $(tail -1 "$CLAIMS_ROWS.err" | head -c 240)"
        return 1
    fi
    while IFS=$'\t' read -r number why; do
        unread=1
        fail "$1" "line $number of claims.jsonl is no row, so nothing of it is read: $why"
    done <"$CLAIMS_UNREAD"
    return "$unread"
}

# Every row of claims.jsonl the one parse read, one per line, as the line stands.
claims() {
    claims_parsed || {
        printf 'check.sh: the parse of claims.jsonl did not run to its end: %s\n' "$(tail -1 "$CLAIMS_ROWS.err")" >&2
        return 1
    }
    cat "$CLAIMS_ROWS"
}

# env_rows_judge <family>; the rows env.sh reads a value from, by env.sh's own table and
# reader of them (ioc_env_row_fields, ioc_env_row_field), over the rows the one parse read,
# which --replay judges before it sources env.sh, once env_functions has loaded env.sh's
# functions. A row that is not there, or whose evidence names no such field, fails
# <family>, naming the row and the field, and so does a row whose field is not a whole line
# of the value of its own one predicate, as s-model-checksum's sha256 is the first line of
# its equals and tc-corpus-1's corpus_floor is its equals: the pin env.sh holds its input
# to and the value the row is held to are one fact, written twice. It returns non-zero
# when it reported a FAIL.
env_rows_judge() {
    local id field what value bad=0
    while IFS='|' read -r id field what; do
        value="$(IOC_ENV_CLAIMS="$CLAIMS_ROWS" ioc_env_row_field "$id" "$field")"
        if [ -z "$value" ]; then
            bad=1
            fail "$1" "claims.jsonl holds no row $id whose evidence names $field, which env.sh reads as $what"
        elif ! jq -e --arg id "$id" --arg value "$value" 'select(.id == $id) | .expect | type == "object" and any(to_entries[] | select(.key != "exit") | .value | tostring | split("\n")[]; . == $value)' "$CLAIMS_ROWS" >/dev/null; then
            bad=1
            fail "$1" "the row $id names $field $value in its evidence, which env.sh reads as $what, and its predicate holds no line $value, so the pin and the value the row is held to differ: $(jq -c --arg id "$id" 'select(.id == $id) | .expect' "$CLAIMS_ROWS" | head -c 200)"
        fi
    done < <(ioc_env_row_fields)
    return "$bad"
}

# claim_marks_judge; claim-marks-agree, which --replay judges first, from the rows the one
# parse read alone, before it reads a source or env.sh builds anything (the header's "The
# one parse of claims.jsonl"), so two rows that disagree on one claim are reported however
# the rest of the run goes. A row that judges a claim of a record under check names that
# claim's row in its checks, with the mark it gives it (the header's list of keys). Each
# entry fails, naming the row and the row it names, when the row that carries it is itself
# a citation row of a record under check, which names no other claim; when the row it
# names is not in the file, or is no citation row of a record under check; when its mark
# is none of the four r1 fixes; when its refines is no sentence, five words or more ending
# with a full stop; and when its mark disagrees with the named row's, named once, by the
# first of three rules it breaks: its mark differs from the named row's and it names no
# part it holds (refines), the two rows disagreeing on one claim; the named row is
# verified and it gives a part of that claim another mark, since a part of a claim marked
# verified is verified too; or its mark is false and the named row's is not, with or
# without refines, since a claim with a part marked false is false. A file in which no row
# carries checks compares no pair, and fails too.
claim_marks_judge() {
    local verdict message status
    if ! claims_parsed; then
        fail claim-marks-agree "the parse of claims.jsonl did not run to its end, so no row is compared with the record claim it checks"
        return 0
    fi
    python3 - "$CLAIMS_ROWS" "$EVIDENCE/sources.json" >"$RUN_SCRATCH/claim-marks" 2>"$RUN_SCRATCH/claim-marks.err" <<'PY'
import json, re, sys

rows_path, sources_path = sys.argv[1:3]
# The marks requirement r1 fixes for a claim of a record under check.
MARKS = ("verified", "false", "unverified", "not-externally-checkable")

def say(verdict, message):
    print("%s\t%s" % (verdict, re.sub(r"[\t\n]", " ", message)))

try:
    with open(sources_path, encoding="utf-8") as sources:
        labels = [record["label"] for record in json.load(sources)["records_under_check"]]
except (OSError, ValueError, KeyError, TypeError) as why:
    say("fail", "sources.json names no record under check this run can read, so no row is known as the row of a record claim: "
        + " ".join(str(why).split())[:160])
    sys.exit(0)

with open(rows_path, encoding="utf-8") as lines:
    rows = [json.loads(line) for line in lines if line.strip()]
by_id = {row["id"]: row for row in rows}

def citation(row_id):
    # A citation row of a record under check: <label>-<n>, <label> one of sources.json's.
    return any(re.fullmatch(re.escape(label) + "-[0-9]+", row_id) for label in labels)

def sentence(text):
    # A sentence by the gate's own reading: five words or more, ending with a full stop.
    return len(text.split()) >= 5 and text.rstrip().endswith(".")

failed = False
carrying = entries = 0
for row in rows:
    if "checks" not in row:
        continue
    carrying += 1
    me = row["id"]
    for entry in row["checks"]:
        entries += 1
        named, mark, refines = entry["row"], entry["mark"], entry.get("refines")
        target = by_id.get(named)
        whys = []
        if citation(me):
            whys.append("%s checks %s, and %s is itself the row of a claim of a record under check, which names no other claim"
                        % (me, named, me))
        if target is None:
            whys.append("%s checks %s, and claims.jsonl holds no row %s" % (me, named, named))
        elif not citation(named):
            whys.append("%s checks %s, which is no citation row of a record under check, <label>-<n> with <label> one of %s"
                        % (me, named, ", ".join(labels) or "none"))
        if mark not in MARKS:
            whys.append("%s gives %s's claim the mark %s, which is none of the marks r1 fixes: %s" % (me, named, mark, ", ".join(MARKS)))
        if refines is not None and not sentence(refines):
            whys.append("%s names the part of %s's claim it holds in a refines that is no sentence, five words or more ending with a full stop: %s"
                        % (me, named, json.dumps(refines)[:160]))
        if target is not None and citation(named) and mark in MARKS:
            theirs = target.get("mark") or "no mark"
            if refines is None and mark != theirs:
                whys.append("%s gives %s's claim the mark %s, %s carries %s, and %s names no part it holds, so the two rows disagree on one claim"
                            % (me, named, mark, named, theirs, me))
            elif refines is not None and theirs == "verified" and mark != "verified":
                whys.append("%s gives a part of %s's claim the mark %s, and %s carries verified, so the two rows disagree on one claim: a part of a claim marked verified is verified too"
                            % (me, named, mark, named))
            elif mark == "false" and theirs != "false":
                whys.append("%s gives a part of %s's claim the mark false, and %s carries %s, so the two rows disagree on one claim: a claim with a part marked false is false"
                            % (me, named, named, theirs))
        for why in whys:
            failed = True
            say("fail", why)
if carrying == 0:
    say("fail", "no row of claims.jsonl carries checks, so no row is compared with the record claim it judges")
elif not failed:
    say("pass", "each of the %d entries of checks on %d rows names the row of a claim of a record under check and gives it the mark that row carries, or names the part of it the row holds, no part of a claim marked verified is marked otherwise, and no part of a claim not marked false is marked false"
        % (entries, carrying))
PY
    status=$?
    while IFS=$'\t' read -r verdict message; do
        if [ "$verdict" = fail ]; then
            fail claim-marks-agree "$message"
        elif [ "$verdict" = pass ] && [ "$status" -eq 0 ]; then
            pass claim-marks-agree "$message"
        fi
    done <"$RUN_SCRATCH/claim-marks"
    [ "$status" -eq 0 ] ||
        fail claim-marks-agree "the comparison of each row's checks with the row it names stopped before its end: $(tail -1 "$RUN_SCRATCH/claim-marks.err" | head -c 240)"
}

# live_read <source> <file> <command...>; the one read of a live source the gate makes
# itself, which every mode makes in its own shell before any assertion over what it reads
# judges: the judgement and the records under check from Dispatch, a reference Dispatch is
# asked to resolve, the Dispatch API, the GitHub budget and default branch, GitHub's answer
# to git ls-remote of the private repository rows read (private_repository_read), a clone's
# fetch, and the run of coordination/count.py over the feed of the count's window
# (count_load); a recorded command's own reads are the row's, which source_answer reads
# (the header's live-read rule). It runs the command, writes what it printed into <file>,
# and returns 0 when the source answered. It returns 1 when the source answered that it
# holds no such thing, which is an answer that judges the deliverable: Dispatch's "not
# found", "No block", "no version N of", "document ... not found", or its refusal to read a
# reference it does not take ("dispatch read was not called"), each measured against
# Dispatch on 2026-11-15, each with or without the refusal's code before it ("NOT_FOUND: not
# found"), which the CLI of the worker image printed from 2026-11-17. Anything else that
# fails, a Dispatch that is unreachable, a curl or git transport error, a count.py that
# raised on a read and exited non-zero, is no answer at all: the mode cannot run, and it
# ends here, naming the source, exit 2, or exit 1 after a FAIL, by the precedence of the
# header.
LIVE_ANSWER_NO='^(NOT_FOUND: )?(not found$|No block |no version [0-9]+ of |document "[^"]*" not found|dispatch [a-z-]+ was not called)'
live_read() {
    local source="$1" file="$2"
    shift 2
    "$@" >"$file" 2>"$file.err" && return 0
    head -1 "$file" "$file.err" 2>/dev/null | grep -qE "$LIVE_ANSWER_NO" && return 1
    cannot_run source " --$MODE: $source did not answer: $(cat "$file" "$file.err" 2>/dev/null | head -c 240 | tr '\n' ' ')"
}

# clone_fetch <repository>; fetches the clone of a repository sources.json pins at its
# pinned commit, by env.sh's one rule, ioc_env_clone, under the clone root this run
# reads, through live_read: a fetch that fails is a mode that cannot run. It returns 1
# for a repository sources.json does not pin, which is an answer about the deliverable.
# clone_dir <repository> prints where that clone stands. A row may name a repository as
# `scope` or as `ioc-project/scope`; both name the row of sources.json that pins it by its
# last path element.
clone_fetch() {
    local name="${1##*/}"
    jq -e --arg n "$name" '.repositories[] | select(.name==$n) | .url and .commit' "$EVIDENCE/sources.json" >/dev/null 2>&1 || return 1
    env_functions || cannot_run gate " --$MODE: env.sh, which holds the rule a clone is made by, did not load."
    IOC_ENV_HERE="$EVIDENCE" IOC_CLONES="$CLONES" live_read "GitHub, the fetch of the clone of $name" "$RUN_SCRATCH/clone-$name.log" ioc_env_clone "$name"
}
clone_dir() { printf '%s/%s' "$CLONES" "${1##*/}"; }

our_commit() { jq -r '.our_repository.commit' "$EVIDENCE/sources.json"; }

# dispatch_header prints the path of a file holding the Dispatch bearer as one HTTP
# header, which curl reads with -H @<file>, so the token stands on no command line. The
# file is written once per run, readable by its owner alone, under this run's scratch,
# which the EXIT trap removes.
dispatch_header() {
    local file="$RUN_SCRATCH/dispatch.header"
    if [ ! -s "$file" ]; then
        [ -n "${DISPATCH_TOKEN_FILE:-}" ] && [ -r "$DISPATCH_TOKEN_FILE" ] || return 1
        (
            umask 077
            printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' <"$DISPATCH_TOKEN_FILE")" >"$file"
        ) || return 1
    fi
    printf '%s' "$file"
}

# dispatch_get <path>; a read of the Dispatch API, run through live_read by its caller: a
# pod with no bearer, a transport error and an HTTP error status each fail it.
dispatch_get() {
    local path="$1" header
    header="$(dispatch_header)" || {
        printf 'no Dispatch bearer: DISPATCH_TOKEN_FILE names no readable file\n' >&2
        return 1
    }
    curl -sS --fail-with-body -H @"$header" "${DISPATCH_URL%/}$path"
}

# env_functions makes env.sh's functions and lists those of this shell, so that check.sh
# and env.sh share each rule they both read: the command that reads a Dispatch record
# (ioc_env_record_command), a clone (ioc_env_clone), git that never prompts (ioc_env_git),
# the GitHub header files (ioc_env_github_auth), the table of tool caches and its
# export (ioc_env_cache_table, ioc_env_caches), the variables it sets or clears in every
# row's environment (IOC_ENV_ENFORCED), the environments and their locks (IOC_ENV_FROZEN,
# read by ioc_env_frozen_labels and ioc_env_frozen_entry), the stamp of an environment
# (ioc_env_unstamp), the reader of a lock (ioc_env_lock_py), the check of what an
# environment holds (ioc_env_frozen), and the values env.sh reads from rows of claims.jsonl
# and its reader of them (ioc_env_row_fields, ioc_env_row_field). env.sh read this way
# defines them and prepares nothing.
env_functions() {
    declare -F ioc_env_record_command >/dev/null && return 0
    [ -r "$EVIDENCE/env.sh" ] || return 1
    local IOC_ENV_FUNCTIONS_ONLY=1
    # shellcheck source=env.sh
    . "$EVIDENCE/env.sh"
}

# judgement_load; reads the judgement once per run, live from Dispatch through
# live_read, into this run's scratch, so the gate reads what a reader reads now rather
# than what an earlier run left behind. The self-test's fixture names a file instead, as
# IOC_JUDGEMENT. A Dispatch that does not answer ends the mode, exit 2, or exit 1 after a
# FAIL, by the precedence of the header; one that answers it holds no judgement leaves the
# text empty, and every assertion over it then fails by its committed floor. Each mode that
# reads the judgement calls this first, in its own shell; judgement_text then prints what it
# read.
judgement_load() {
    local cache="$RUN_SCRATCH/judgement.md"
    [ -e "$cache" ] && return 0
    if [ -n "${IOC_JUDGEMENT:-}" ]; then
        live_read "the judgement file $IOC_JUDGEMENT" "$cache" cat "$IOC_JUDGEMENT"
        return 0
    fi
    env_functions || cannot_run gate " --$MODE: env.sh, which holds the command a Dispatch record is read by, did not load."
    live_read "Dispatch, the read of dispatch://INFRA-3134/artifact/judgement-md" "$cache" \
        ioc_env_record_command dispatch://INFRA-3134/artifact/judgement-md || : >"$cache"
}
judgement_text() {
    [ -e "$RUN_SCRATCH/judgement.md" ] || {
        printf 'check.sh read the judgement before judgement_load\n' >&2
        return 1
    }
    cat "$RUN_SCRATCH/judgement.md"
}

# record_load <label>; reads one record under check, by its label in sources.json, into
# this run's scratch, once per run: from the copy sources.json names, which is how the
# self-test's fixture holds one, or else live from Dispatch through live_read. A Dispatch
# that answers it holds no such record leaves the text empty, and citation-row then fails
# by its committed rows. It names the copy of the design as IOC_DESIGN and that of the
# reconciliation as IOC_COMMENT, which every row that counts or quotes a line of one reads
# and env.sh then reads no record again, so the rows and citation-row read one copy, read
# in this run. record_text <label> prints what it read.
record_load() {
    local label="$1" ref copy cache="$RUN_SCRATCH/record-$1.md"
    if [ ! -e "$cache" ]; then
        ref="$(jq -r --arg l "$label" '.records_under_check[] | select(.label==$l) | .ref' "$EVIDENCE/sources.json")"
        copy="$(jq -r --arg l "$label" '.records_under_check[] | select(.label==$l) | .copy // ""' "$EVIDENCE/sources.json")"
        if [ -n "$copy" ]; then
            cp "$EVIDENCE/$copy" "$cache"
        else
            env_functions || cannot_run gate " --$MODE: env.sh, which holds the command a Dispatch record is read by, did not load."
            live_read "Dispatch, the read of $ref" "$cache" ioc_env_record_command "$ref" || : >"$cache"
        fi
    fi
    case "$label" in
    d) export IOC_DESIGN="$cache" ;;
    r) export IOC_COMMENT="$cache" ;;
    esac
}
record_text() { cat "$RUN_SCRATCH/record-$1.md"; }

# history_ref names the commit whose history --shape and --landed read: IOC_HISTORY_REF
# when it is set, which a reader sets once the squash merge has left the pull request's
# own commits off the default branch, and HEAD otherwise.
history_ref() { printf '%s' "${IOC_HISTORY_REF:-HEAD}"; }

# pr_base prints the base this pull request's own diff is read against: the base branch's
# commit that the newest forward merge on the pull request's first-parent line brought in,
# or the commit our code was read at when no merge did. A range from our_commit alone would
# carry every change the base branch made since, which are no step of this pull request.
pr_base() {
    local merge
    merge="$(git -C "$ROOT" rev-list --first-parent --merges -1 "$(our_commit)..$(history_ref)")"
    if [ -n "$merge" ]; then
        git -C "$ROOT" rev-parse "$merge^2"
    else
        our_commit
    fi
}

# pr_commits prints the commits this pull request added, one per line: those of its history
# that are neither the commit our code was read at nor reachable from a base commit a
# forward merge brought in. A forward merge itself is one of them.
pr_commits() {
    local merges
    merges="$(git -C "$ROOT" rev-list --merges "$(our_commit)..$(history_ref)" | sed 's/$/^2/')"
    # shellcheck disable=SC2086
    git -C "$ROOT" rev-list "$(history_ref)" --not "$(our_commit)" $merges
}

# ---------------------------------------------------------------- the predicate


# bound <at_most|at_least> <value> <output file>
#
# A bound reads the one number the output prints, which is how a score or a count is
# recorded; a row that counts lines says so in its command, with wc -l. Output that is not
# exactly one number holds no bound, so a traceback's line count never meets one, and the
# number read is named in the failure. The two numbers are compared as the decimals they
# are written as, never as floats, which would round two counts past 2^53 to one.
bound() {
    local key="$1" value="$2" out="$3" read_value
    # Only the space around the number is taken off, so two numbers on two lines stay
    # two and are no one number.
    read_value="$(cat "$out")"
    read_value="${read_value#"${read_value%%[![:space:]]*}"}"
    read_value="${read_value%"${read_value##*[![:space:]]}"}"
    if ! [[ "$read_value" =~ ^-?[0-9]+(\.[0-9]+)?$ ]]; then
        IOC_BOUND_READ="output that is not one number"
        return 1
    fi
    IOC_BOUND_READ="$read_value"
    python3 -c 'import sys; from decimal import Decimal; a=Decimal(sys.argv[1]); b=Decimal(sys.argv[2]); sys.exit(0 if (a <= b if sys.argv[3]=="at_most" else a >= b) else 1)' "$read_value" "$value" "$key"
}

# evaluate <expect-json> <output-file>; prints nothing, returns 0 when it holds, by what
# the header's rule for expect gives each key: the output, and the value of an equals or a
# contains, are read as text without their trailing newlines, and no value is handed to a
# tool as a list of patterns. A predicate whose shape predicate_is_a_sentence refuses
# holds over no output.
evaluate() {
    local spec="$1" out="$2" key value text candidate
    # An exit status beside a predicate over the output: both hold, or the row fails.
    if [ "$(printf '%s' "$spec" | jq 'type == "object" and has("exit") and length == 2')" = true ]; then
        evaluate "$(printf '%s' "$spec" | jq -c '{exit}')" "$out" &&
            evaluate "$(printf '%s' "$spec" | jq -c 'del(.exit)')" "$out"
        return
    fi
    predicate_is_a_sentence "$spec" && return 2
    key="$(printf '%s' "$spec" | jq -r 'keys[0] // empty')"
    value="$(printf '%s' "$spec" | jq -r --arg k "$key" '.[$k] | tostring')"
    case "$key" in
    equals) [ "$(cat "$out")" = "$value" ] ;;
    # The whole value, every line of it, is one substring of the output.
    contains)
        text="$(cat "$out")"
        [[ "$text" == *"$value"* ]]
        ;;
    # One extended regular expression, on one line, which some line of the output matches.
    matches) grep -qE -- "$value" "$out" ;;
    # A status that was never recorded is no status, and meets no value.
    exit) [ "$(cat "$out.status" 2>/dev/null)" = "$value" ] ;;
    at_most | at_least) bound "$key" "$value" "$out" ;;
    # A set membership: the whole output is one of the candidates, each one line.
    one_of)
        text="$(cat "$out")"
        while IFS= read -r candidate; do
            [ "$text" = "$candidate" ] && return 0
        done < <(printf '%s' "$spec" | jq -r '.one_of[]')
        return 1
        ;;
    *) return 2 ;;
    esac
}

# A predicate is a bound, a set membership or an exit status. Anything else, and anything
# whose value reads as a sentence, fails: that is what the Verification document asks for.
#
# predicate_is_a_sentence <expect-json>; true when any key of the predicate is none of the
# seven, or carries a value of another shape than the header's rule for expect gives that
# key, and IOC_PREDICATE_WHY then names the key and what its value is.
predicate_is_a_sentence() {
    local spec="$1" key type value
    IOC_PREDICATE_WHY=""
    while IFS= read -r key; do
        type="$(printf '%s' "$spec" | jq -r --arg k "$key" '.[$k] | type')"
        # The value as jq prints it raw, with every newline it holds, its last included.
        value="$(printf '%s' "$spec" | jq -j --arg k "$key" '.[$k] | tostring' && printf .)"
        value="${value%.}"
        case "$key" in
        equals)
            [ "$type" = string ] || IOC_PREDICATE_WHY="its equals is a JSON $type, not a string"
            ;;
        contains)
            if [ "$type" != string ]; then
                IOC_PREDICATE_WHY="its contains is a JSON $type, not a string"
            elif [ -z "${value//$'\n'/}" ]; then
                IOC_PREDICATE_WHY="its contains is empty without its trailing newlines, and an empty value is a substring of any output"
            fi
            ;;
        matches)
            if [ "$type" != string ]; then
                IOC_PREDICATE_WHY="its matches is a JSON $type, not a string"
            elif [ -z "$value" ]; then
                IOC_PREDICATE_WHY="its matches is empty, and an empty expression matches every line"
            elif [[ "$value" == *$'\n'* ]]; then
                IOC_PREDICATE_WHY="its matches holds a newline, and a matches is one regular expression over one line, where grep reads each line of it as an expression of its own"
            fi
            ;;
        one_of)
            if [ "$type" != array ] || ! printf '%s' "$spec" | jq -e '.one_of | length > 0 and (map(type == "string") | all)' >/dev/null; then
                IOC_PREDICATE_WHY="its one_of is no list of one or more strings"
            elif ! printf '%s' "$spec" | jq -e '.one_of | map(length > 0 and (split("\n") | length) == 1) | all' >/dev/null; then
                IOC_PREDICATE_WHY="its one_of holds a candidate that is empty or holds a newline, and each candidate is one whole line the output can be"
            fi
            ;;
        at_most | at_least)
            [[ "$value" =~ ^-?[0-9]+(\.[0-9]+)?$ ]] || IOC_PREDICATE_WHY="its $key is no one number"
            ;;
        exit)
            [[ "$value" =~ ^[0-9]+$ ]] || IOC_PREDICATE_WHY="its exit is no one whole number"
            ;;
        *) IOC_PREDICATE_WHY="its key $key is none of the seven" ;;
        esac
        [ -z "$IOC_PREDICATE_WHY" ] || return 0
    done < <(printf '%s' "$spec" | jq -r 'keys[]')
    return 1
}

# ---------------------------------------------------------------- --coverage

coverage_report() {
    printf 'What this gate reads, one line per assertion family.\n\n'
    printf '%-24s %-10s %s\n' 'family' 'mode' 'what it asserts'
    assertion_table | while IFS='|' read -r family mode asserts _planted; do
        printf '%-24s %-10s %s\n' "$family" "--$mode" "$asserts"
    done
    printf '\nWhat this gate does not read, and no run of it ever will:\n'
    cat <<'GAPS'
- whether the ranking is right. The pages carry a verdict, a cost and a default, and
  nothing here says the order is the order a reader should act in.
- whether the reconciliation is fair. It reads that the four fixed rows are present, not
  that the paragraph on where the two accounts differ is a fair reading of either.
- whether the reading is deep. It reads that each of the six mas-lab directories has an
  evidence file and that each of the three questions is answered; a file that answers
  shallowly passes.
- whether a cited text supports the claim beside it. It resolves the citation and reads
  the quoted words; the judgement of what they support is the reader's.
- whether a candidate that cannot be argued in one page was left off the list, and whether
  one fact is written once with the other half linking to it. Both are a reviewer's
  reading, and the plan of this issue names them so that nobody reads this gate as
  enforcing them.
- whether an engineer-day estimate is right. It reads the number on the page.
GAPS
}

# ---------------------------------------------------------------- --replay

# env.sh prepares every input a recorded command reads that this tree cannot hold and
# exports one variable per input. Sourcing it here puts those variables in this shell's
# environment, so that each recorded command reads them beside IOC_CLONES. The fixture of
# --self-test builds a deliverable whose rows need none of them and sets IOC_NO_ENV=1; its
# rows' tools still keep every cache under IOC_CACHE, the fixture's own directory, which
# no run outside that fixture reads (fixture_run), by env.sh's one table of them, so a row
# planted there runs as cold as the tree's own.
prepare_env() {
    if [ "${IOC_NO_ENV:-0}" = 1 ]; then
        env_functions || cannot_run gate ": env.sh, which names where every tool cache of a recorded command lives, did not load."
        ioc_env_caches
        return 0
    fi
    [ -r "$EVIDENCE/env.sh" ] ||
        cannot_run gate ": $EVIDENCE/env.sh is missing, and every recorded command needs what it prepares."
    # shellcheck source=env.sh
    . "$EVIDENCE/env.sh" || cannot_run environment ": env.sh did not prepare what the recorded commands read."
}

# What the evaluator admits, a closed table. It fails closed: the classifier of reader_py
# reads each row's command once, and admits it only when every part of it is an entry of
# the classifier's one table, ADMITTED; any other form is refused by name, and the row fails
# without being read. Every admission goes through one function, admit, which refuses a
# form the table does not hold whatever the parser read it as. The programs are entries of
# it too, one per program name the rows run. The table below is ADMITTED in its own order,
# each entry with the number of the rows of claims.jsonl with a command that use it.
# --admitted measures it, and fails when an entry is used by no row, when a row is
# refused, or when a line here differs from what it measured; --self-test runs it.
#
#   form                            rows  what it is
#   &&                               220  runs the next pipeline when this one exits 0
#   ||                                 9  runs the next pipeline when this one fails
#   |                                462  joins the commands of a pipeline
#   ;                                 63  ends a pipeline
#   newline                            3  an unquoted newline, ending a pipeline as ; does
#   >                                  8  writes the output to a file
#   2>                                23  writes the error output to a file
#   2>&1                               6  sends the error output where the output goes
#   {                                 28  opens a group
#   }                                 28  closes a group
#   (                                  2  opens a subshell
#   )                                  2  closes a subshell
#   for                               21  opens a for loop
#   in                                21  comes before a for loop's words
#   do                                21  comes before a for loop's body
#   done                              21  closes a for loop
#   simple command                   652  a command word and its arguments
#   pipeline                         462  two or more commands joined by |
#   list                             247  two or more pipelines joined by ;, &&, || or newline
#   group                             28  { list; }, whose status is its last command's
#   subshell                           2  ( list ), whose status is its last command's
#   for loop                          21  for NAME in words; do list; done
#   assignment before a command       29  NAME=value, set in that command's environment
#   assignment alone                   6  NAME=value with no command word
#   command substitution in a word    12  $( ) in a word, its code admitted by this table
#   assigns ACCORD_MODEL_DIR          16  a name the rows assign
#   assigns GOFLAGS                    1  a name the rows assign
#   assigns IOC_EXPECT_STATUS          9  the declared status, the last prefix of one command
#   assigns UV_REQUIRE_HASHES          4  0, before a uv pip install of local sources alone
#   assigns VIRTUAL_ENV                3  a name the rows assign
#   assigns W                          1  a name the rows assign
#   assigns c                          7  a name the rows assign
#   assigns f                          6  a name the rows assign
#   assigns g                          1  a name the rows assign
#   assigns m                          1  a name the rows assign
#   assigns n                          6  a name the rows assign
#   assigns o                          1  a name the rows assign
#   assigns p                          5  a name the rows assign
#   assigns r                          5  a name the rows assign
#   program awk                       27  wrapped in a function that records its status
#   program cat                       25  wrapped in a function that records its status
#   program cd                        76  wrapped in a function that records its status
#   program cmp                        2  wrapped in a function that records its status
#   program cp                         1  wrapped in a function that records its status
#   program curl                      33  wrapped in a function that records its status
#   program cut                        5  wrapped in a function that records its status
#   program dispatch                  76  wrapped in a function that records its status
#   program echo                      15  wrapped in a function that records its status
#   program git                      380  wrapped in a function that records its status
#   program go                         2  wrapped in a function that records its status
#   program grep                     245  wrapped in a function that records its status
#   program jq                        70  wrapped in a function that records its status
#   program mkdir                      1  wrapped in a function that records its status
#   program mktemp                     1  wrapped in a function that records its status
#   program printf                    12  wrapped in a function that records its status
#   program python3                   37  wrapped in a function that records its status
#   program rm                         2  wrapped in a function that records its status
#   program sed                      304  wrapped in a function that records its status
#   program sh                         1  wrapped in a function that records its status
#   program sha256sum                  8  wrapped in a function that records its status
#   program sort                      32  wrapped in a function that records its status
#   program stat                       2  wrapped in a function that records its status
#   program tail                       1  wrapped in a function that records its status
#   program tr                        11  wrapped in a function that records its status
#   program uniq                      18  wrapped in a function that records its status
#   program uv                         8  wrapped in a function that records its status
#   program wc                        66  wrapped in a function that records its status
#   program xargs                      5  wrapped in a function that records its status
#   variable $IOC_PY                  43  a program variable, whose shim records its status
#   variable $IOC_MASLAB_PY            4  a program variable, whose shim records its status
#   variable $IOC_MASLAB_CTL           5  a program variable, whose shim records its status
#   variable $IOC_MASLAB_WHEEL_CTL     2  a program variable, whose shim records its status
#   variable $IOC_TC_PY                2  a program variable, whose shim records its status
#   xargs running a program            5  xargs's status carries the program's
#   xargs -0                           3  xargs reads its input split at NUL bytes
#   xargs -n                           3  xargs passes at most N arguments to each run
#   uv run --no-project                4  uv run reads no project of the working directory
#   uv run --python                    4  uv run runs the python version named
#   uv run --with                      4  uv run installs the package named into its environment
#   uv run python                      4  uv run's python, whose status uv run exits with
#   sh running a script file           1  the script's status reaches the row as sh's
#   true                               9  a command whose status is no read
#   $NAME or ${NAME}                 447  a parameter, its name in braces or not
#   $?                                11  the status of the last command
#   $( )                              12  a command substitution
#   a literal $                        2  a $ that starts no expansion, left as it stands
#
# Each program the table names is wrapped in a function of that name that records its
# status, a builtin among them; a program variable points at a shim that does the same;
# true's status is no read. A program name the table holds no entry for is refused, a
# builtin or not. The program xargs runs is one of the table's programs, and the program
# uv run runs is one of uv run's own entries; each is held with its arguments to every
# rule a command word and its arguments are held to.
#
# The names a row assigns. An assignment, alone or before a command, and a for loop's
# variable are admitted only to a name the table holds an entry "assigns <name>" for, one
# per name the rows assign, which --admitted counts like every entry; every other name is
# refused by name, the shell's own among them, such as PATH, IFS, FUNCNEST, BASH_ENV and
# LC_ALL, which change how the runner's functions find, call or record a program. No tool
# cache is on the list, GOMODCACHE and GOCACHE among them: env.sh's ioc_env_cache_table sets
# each under IOC_CACHE, so a row reads its cache from there and one that assigns it is
# refused.
# printf -v, which would assign its target, is refused whole, since no row writes it, so
# no printf -v target is on the list. IOC_EXPECT_STATUS=<n> is admitted only before one
# command word and as its last prefix, since bash expands prefixes from left to right and
# a substitution in a later prefix would run with the status already declared; the
# declared status covers that one command, and nothing a prefix substitution runs.
#
# The names the gate records with, which no row may touch. The list below is the reader's
# RESERVED, which reader_py builds from PROGRAM_VARIABLES and RUNNER_PREFIXES, the two
# variables the shims and the runner are written from, and from env.sh's IOC_ENV_ENFORCED,
# the variables env.sh sets or clears in every row's environment; --admitted holds these
# lines as it holds the table, and fails a name the runner or a shim records with that no
# prefix covers. A row is refused by name when it assigns one, alone, before a command, in
# an argument such as export's, or as a for loop's variable; when it reads one other than
# IOC_EXPECT_STATUS with $ in an argument, an assignment, a for loop's words or a
# redirection target, such as >"$__ioc_failed", a program variable as a command word
# excepted; and when it passes one to printf as an argument of its own. The one other
# assignment admitted is UV_REQUIRE_HASHES=0 before the one install the header above
# reader_py states. A word is read without its quotes, so a quoted pattern that holds a
# reserved name is refused too.
#
#   reserved name                         what it is
#   __ioc*                                a name the runner or a shim records with
#   __IOC*                                a name the runner or a shim records with
#   IOC_PY                                a program variable, which points at its shim
#   IOC_MASLAB_PY                         a program variable, which points at its shim
#   IOC_MASLAB_CTL                        a program variable, which points at its shim
#   IOC_MASLAB_WHEEL_CTL                  a program variable, which points at its shim
#   IOC_TC_PY                             a program variable, which points at its shim
#   IOC_EXPECT_STATUS                     the declared status, the last prefix of one command
#   UV_REQUIRE_HASHES                     a variable env.sh sets or clears in every row's environment
#   UV_BUILD_CONSTRAINT                   a variable env.sh sets or clears in every row's environment
#   PIP_REQUIRE_HASHES                    a variable env.sh sets or clears in every row's environment
#   UV_NO_CONFIG                          a variable env.sh sets or clears in every row's environment
#   PIP_CONFIG_FILE                       a variable env.sh sets or clears in every row's environment
#   UV_INDEX_URL                          a variable env.sh sets or clears in every row's environment
#   UV_EXTRA_INDEX_URL                    a variable env.sh sets or clears in every row's environment
#   UV_INDEX                              a variable env.sh sets or clears in every row's environment
#   UV_DEFAULT_INDEX                      a variable env.sh sets or clears in every row's environment
#   UV_FIND_LINKS                         a variable env.sh sets or clears in every row's environment
#   UV_INDEX_STRATEGY                     a variable env.sh sets or clears in every row's environment
#   UV_NO_INDEX                           a variable env.sh sets or clears in every row's environment
#   UV_CONFIG_FILE                        a variable env.sh sets or clears in every row's environment
#   PIP_INDEX_URL                         a variable env.sh sets or clears in every row's environment
#   PIP_EXTRA_INDEX_URL                   a variable env.sh sets or clears in every row's environment
#   PIP_FIND_LINKS                        a variable env.sh sets or clears in every row's environment
#   PIP_NO_INDEX                          a variable env.sh sets or clears in every row's environment
#   UV_NO_VERIFY_HASHES                   a variable env.sh sets or clears in every row's environment
#   MAS_MANIFEST_VALIDATE                 a variable env.sh sets or clears in every row's environment
#   MAS_MANIFEST_RESOLVE_REFS             a variable env.sh sets or clears in every row's environment
#   MAS_MANIFEST_STRICT                   a variable env.sh sets or clears in every row's environment
#   OPENAI_API_KEY                        a variable env.sh sets or clears in every row's environment
#   MAS_LLM_API_KEY                       a variable env.sh sets or clears in every row's environment
#
# What the table is checked against. The classifier is a parser of the forms the rows use,
# so a spelling it misreads into an admitted entry, one that no row writes, is outside what
# it is checked against. Such a spelling is a defect only when it can hide a failed read,
# that is a read whose failure the gate then does not record. A form the rows use in one
# context that could hide a read in another is what the planted cases of check.test.sh are
# for.
#
# Refused by name, among every other form: a command holding a character bash reads as
# part of a word where the tokenizer would split, a carriage return, any other control
# character but tab and newline, DEL, and every space bash does not split at, such as
# U+00A0; every program name the table holds no entry for, such as unset, export and
# head; a word in command position that holds a / or is a variable other than the five;
# the output of a substitution as a command word; command, builtin, exec, eval, source,
# time, function definitions and the keywords if, while, until, case and select, each
# named with why; a shell given a string with -c, a shell reading its commands from its
# input or from a script under /dev/ or /proc/, such as /dev/stdin, a shell given an
# option, and a shell other than sh; env in any form, a wrapper no row runs, as are
# timeout, nice, nohup, sudo and stdbuf; xargs with no program, with an option the
# table holds no entry for, or running a program by a form no command word may take; uv given
# an option before its subcommand, and uv run with an option or a program the table holds
# no entry for, or with none; printf -v, which assigns a variable, printf with a format
# that is not a literal word, holds a $ or a substitution or starts with -, which could
# expand to -v, and printf given a reserved name as an argument; find, whose -exec would
# run a program past its function; an input redirection, a here-document, an output
# redirection other than >, 2> and 2>&1, a background &, process substitution <( ) and
# >( ), whose code runs beside the row and ends after it is read, a substitution written
# with backticks, arithmetic (( )), $(( )) and $[ ]; every special parameter but $?
# written bare, such as $1, $@, $#, $$, $!, $0, $- and $*, and ${?} among them; every
# other $ form, $' ', $" " and ${ } holding anything but a name among them, such as
# ${X:-y}; a for loop with no in; an assignment to an array element or by +=; an
# assignment or a for loop's variable to a name the table holds no entry for;
# IOC_EXPECT_STATUS written as a statement of its own or before another prefix; and
# declare, local, typeset and readonly, which a wrapping function would make local to it.
# A row that needs a new form, program or assigned name adds its entry to ADMITTED and
# its line to the table above, with a case of check.test.sh, in the same commit.
#
# What the evaluator refuses in an admitted row, read after it runs:
#
#   - a non-zero status of any program the row runs, wherever it stands, as its function
#     or its shim records it with its arguments;
#   - a non-zero status of any stage of the final pipeline, the last included, read from
#     PIPESTATUS and matched to the command whose status that stage carries (a group or a
#     loop carries its last command's);
#   - in both, the one search-miss rule holds: a search, grep, egrep, fgrep, rg, ag or git
#     grep, that exits exactly 1 found nothing, and one that exits above 1 failed;
#   - a bound, at_least or at_most, whose output is not exactly one number; a row that
#     counts lines says so in its command, with wc -l;
#   - a predicate any key of which is none of the seven or carries a value of another
#     shape than the header's rule for expect gives that key, a row with two predicates
#     over its output, and a row with a command and no predicate.
#
# A row claims a failure in one way: IOC_EXPECT_STATUS=<n> written before the one command
# that fails, which exempts that command's status n and nothing else, and an exit status
# beside the row's one predicate over its output, which the row's final status must meet.
# A row with an exit status is held to every rule above like any other.
#
# What the evaluator cannot see. It reads exit statuses and the output a row prints. A
# command that exits 0 after failing, or that writes its failure only to a file the row
# does not read, is outside what it sees; so is true's status anywhere but the final
# pipeline, an output redirection that fails before ;, && or || and so stops its command
# before the function runs, what a program does with code it is given, such as awk's
# system() or a program run with the arguments xargs's input adds, and what a script a
# row runs does inside it, whose status reaches the row only through that script's own.
# A script's path is held to a literal file or one under $IOC_EVID, ending in .sh, and
# any other path is refused. A row written on purpose to defeat the runner is outside
# what the gate checks, whatever it uses: one that deletes, overwrites, redirects into or
# re-points the runner's own files (its record, its shims, its scratch), one that makes
# an admitted program run a command its own arguments supply and swallow that command's
# failure, as a git alias written git -c alias.x='!cmd; true' does, or one that builds a
# printf format by brace or glob expansion so that it expands to -v. The test is whether
# an author could write the row by accident while writing an ordinary evidence row: if
# so, its failed read is a defect the gate must catch; if it can only be built to defeat
# the gate, it is outside what the gate checks, and a person reading every row at review
# covers it, as for install-pinned.

# The programs a row may name through a variable, each with the program it names. env.sh
# sets each to one executable; during a run each is pointed at a shim that runs that
# executable and records its status, as the functions below record a program named by a
# bare word. This is the one list of them: reader_py writes it into the reader, which
# builds from it the classifier's entries "variable $<name>" of ADMITTED, install-pinned's
# resolve(), which holds each word to the rules of the program it names, and the names no
# row may touch.
PROGRAM_VARIABLES="IOC_PY:python IOC_MASLAB_PY:python IOC_MASLAB_CTL:mas-ctl IOC_MASLAB_WHEEL_CTL:mas-ctl IOC_TC_PY:python"
# The prefixes of every name the runner and the shims below record with: __ioc_failed,
# __ioc_record, __ioc_s, __ioc_said, __ioc_o, __ioc_stages and __IOC_FAILED. reader_py
# writes them into the reader, which refuses a row that touches a name with one, and
# --admitted fails a name of the runner that starts with none of them.
RUNNER_PREFIXES="__ioc __IOC"

program_shims() {
    # Writes one shim per program variable env.sh set, once per run, and prints the
    # assignments that point each variable at its shim.
    local dir="$RUN_SCRATCH/programs" v real
    mkdir -p "$dir"
    for v in $PROGRAM_VARIABLES; do
        v="${v%%:*}"
        real="${!v:-}"
        # A shim is written whenever env.sh set the variable, whether or not the program
        # exists yet, since a row may install it first; a program that is not there when
        # the shim runs it exits 127, which the shim records like any other failure.
        [ -n "$real" ] || continue
        if [ ! -x "$dir/$v" ]; then
            {
                printf '#!/usr/bin/env bash\n'
                printf '%q "$@"\n' "$real"
                printf '__ioc_s=$?\n'
                printf 'if [ "$__ioc_s" -ne 0 ] && [ -n "${__IOC_FAILED:-}" ]; then\n'
                printf '    printf "%%s\\t%%s\\t%%s\\t-\\n" "$__ioc_s" "${IOC_EXPECT_STATUS:--}" "$(printf "%%s " %q "$@" | tr "\\n\\t" "  ")" >>"$__IOC_FAILED"\n' "\$$v"
                printf 'fi\n'
                printf 'exit "$__ioc_s"\n'
            } >"$dir/$v"
            chmod +x "$dir/$v"
        fi
        printf '%s=%q\n' "$v" "$dir/$v"
    done
}

run_recorded() {
    # run_recorded <command> <output-file>; the variables env.sh exported are already in
    # this shell's environment and reach the command through it. The command runs with
    # TMPDIR set to <output-file>.tmp, an empty directory of its own beside its output, so
    # it reads nothing an earlier run or an earlier row left under the caller's TMPDIR.
    #
    # Every program the command runs records its own status when it fails, as one line of
    # <output-file>.failed with four fields, separated by tabs: the status, the status the
    # row declared for it, the program and its arguments, and the first line it printed, each
    # of the second and the fourth - where there is none. Each program the row names in
    # command position by a bare word is wrapped in a shell function of that name, which runs
    # the real command and records its status, and each program variable points at a shim
    # that does the same. A function and a shim see their own status wherever the program
    # stands: in any stage of a pipeline, in any element of an && or || list, in a group, a
    # subshell, a loop or a substitution. That is what bash's ERR trap does not do, for a
    # stage that is not a pipeline's last and for a command before && or ||, and what
    # PIPESTATUS does not do, since it holds the statuses of one pipeline without its
    # commands' text. The function of dispatch alone keeps the first line its program
    # printed: the dispatch command prints a refusal on stdout, which every row pipes into
    # sed, grep or awk, so the row's output holds none of it, and only that line tells
    # Dispatch's answer about the record from no answer (source_answer). It writes what
    # dispatch prints into a file beside the record and then onto its own stdout, so the
    # row's pipeline reads every byte it read before, once dispatch has exited.
    local command="$1" out="$2" script name
    script="$out.sh"
    : >"$out.failed"
    : >"$out.unread"
    local names
    names="$(printf '%s' "$command" | python3 "$READER" names)"
    printf '%s\n' "$names" | sed -n 's/^UNREAD\t//p' >"$out.unread"
    {
        printf '__ioc_failed=%q\n' "$out.failed"
        printf '%s\n' '__ioc_record() { local __ioc_s="$1"; shift; printf "%s\t%s\t%s\t%s\n" "$__ioc_s" "${IOC_EXPECT_STATUS:--}" "$(printf "%s " "$@" | tr "\n\t" "  ")" "${__ioc_said:--}" >>"$__ioc_failed"; }'
        while IFS= read -r name; do
            [ -n "$name" ] || continue
            if [ "$name" = dispatch ]; then
                printf '%s\n' 'dispatch() { local __ioc_o="$__ioc_failed.$BASHPID.dispatch" __ioc_s __ioc_said=""; command dispatch "$@" >"$__ioc_o"; __ioc_s=$?; command cat -- "$__ioc_o"; IFS= read -r __ioc_said <"$__ioc_o"; command rm -f -- "$__ioc_o"; __ioc_said="${__ioc_said//[[:cntrl:]]/ }"; __ioc_said="${__ioc_said:0:240}"; [ "$__ioc_s" -eq 0 ] || __ioc_record "$__ioc_s" dispatch "$@"; return "$__ioc_s"; }'
                continue
            fi
            printf '%s() { command %s "$@"; local __ioc_s=$?; [ "$__ioc_s" -eq 0 ] || __ioc_record "$__ioc_s" %s "$@"; return "$__ioc_s"; }\n' "$name" "$name" "$name"
        done < <(printf '%s\n' "$names" | grep -v '^UNREAD')
        program_shims
        printf '%s\n' "$command"
        # The statuses of the final pipeline's stages, which a stage the functions do not
        # see, such as a group whose status a builtin set, still reports.
        printf '%s\n' '__ioc_stages=("${PIPESTATUS[@]}")'
        printf '%s\n' 'printf "%s\n" "${__ioc_stages[*]}" >&9'
        printf '%s\n' 'exit "${__ioc_stages[${#__ioc_stages[@]}-1]}"'
    } >"$script"
    printf '%s' "$command" >"$out.command"
    rm -rf "$out.tmp" && mkdir -p "$out.tmp"
    (
        cd "$ROOT" || exit 1
        __IOC_FAILED="$out.failed" IOC_CLONES="$CLONES" EVIDENCE="$EVIDENCE" TMPDIR="$out.tmp" bash "$script" 9>"$out.pipe"
    ) >"$out" 2>&1
    printf '%s' "$?" >"$out.status"
}

# search_missed <command text> <status>; true when the command is a search that read its
# corpus and found nothing: its first word is grep, egrep, fgrep, rg or ag, or it is git
# with grep as its subcommand, and it exited exactly 1. A search that exits above 1 could
# not read what it searches, as a git grep over a missing clone exits 128, and any other
# command that failed is a failed read. This is the one rule every path below reads.
search_missed() {
    local status="$2" words i=0
    [ "$status" = 1 ] || return 1
    read -r -a words <<<"$1"
    # An assignment written before the command sets its environment and is no command.
    while [ "$i" -lt "${#words[@]}" ] && [[ "${words[$i]}" =~ ^[A-Za-z_][A-Za-z0-9_]*= ]]; do
        i=$((i + 1))
    done
    case "${words[$i]:-}" in
    grep | egrep | fgrep | rg | ag) return 0 ;;
    git)
        # git's own options stand before its subcommand; -C and -c take a value.
        i=$((i + 1))
        while [ "$i" -lt "${#words[@]}" ]; do
            case "${words[$i]}" in
            -C | -c | --git-dir | --work-tree | --namespace) i=$((i + 2)) ;;
            -*) i=$((i + 1)) ;;
            *) break ;;
            esac
        done
        [ "${words[$i]:-}" = grep ]
        ;;
    *) return 1 ;;
    esac
}

declared_status_holds() {
    # declared_status_holds <status> <declared status>; true when the row declared, by
    # IOC_EXPECT_STATUS=<n> written before the one command, exactly the status it exited
    # with; - stands for no declared status, since a record's fields are tab-separated and
    # an empty one would collapse. This is the one place that exemption is read.
    [ -n "$2" ] && [ "$2" != - ] && [ "$1" = "$2" ]
}

# failed_read <output-file>; true when a program of the row's run recorded a status that
# is no search that missed, which is a read that failed wherever it stood in the row, and
# the one search-miss rule is applied to each program's own status. It names that program,
# its arguments and its status in IOC_FAILED_READ.
failed_read() {
    local out="$1" failed_status failed_declared failed_command _failed_said
    IOC_FAILED_READ=""
    if [ -s "$out.failed" ]; then
        while IFS=$'\t' read -r failed_status failed_declared failed_command _failed_said; do
            declared_status_holds "$failed_status" "$failed_declared" && continue
            search_missed "$failed_command" "$failed_status" && continue
            IOC_FAILED_READ="'$failed_command' exited $failed_status"
            return 0
        done <"$out.failed"
    fi
    # Every stage of the final pipeline, the last one included, by its own status and the
    # command whose status it carries, as the classifier splits it: that reads a condition
    # or a group whose status a builtin set, which no function records.
    local stages i texts=() declared
    [ -s "$out.pipe" ] || return 1
    read -r -a stages <"$out.pipe" || return 1
    mapfile -t texts < <(python3 "$READER" stages <"$out.command")
    if [ "${texts[0]:-}" = UNSPLIT ] || [ "${#texts[@]}" -ne "${#stages[@]}" ]; then
        IOC_FAILED_READ="the final pipeline of the row, whose ${#stages[@]} stages the classifier does not split into as many"
        for ((i = 0; i < ${#stages[@]}; i++)); do [ "${stages[$i]}" = 0 ] || return 0; done
        return 1
    fi
    for ((i = 0; i < ${#stages[@]}; i++)); do
        [ "${stages[$i]}" = 0 ] && continue
        declared=""
        [[ "${texts[$i]}" =~ (^|\ )IOC_EXPECT_STATUS=([0-9]+)(\ |$) ]] && declared="${BASH_REMATCH[2]}"
        declared_status_holds "${stages[$i]}" "$declared" && continue
        search_missed "${texts[$i]}" "${stages[$i]}" && continue
        IOC_FAILED_READ="the stage '${texts[$i]}' of the final pipeline exited ${stages[$i]}"
        return 0
    done
    return 1
}

# github_budget
#
# A --replay makes GITHUB_CORPUS_REQUESTS + GITHUB_ROW_REQUESTS requests to GitHub's REST
# API, counted below: env.sh's read of the review corpus and the rows' own curls. A read
# with no token is held to sixty an hour for one address, fewer than the corpus alone
# takes, and a run without the budget for them would see env.sh refuse the corpus at
# GitHub's refusal, or the rows fail for a reason that is not this deliverable's: the gate
# would read GitHub's refusal as a claim that no longer holds. So the budget is read after
# github_headers has written the header files and before env.sh builds anything, with curl
# sending the file of cybermonklabs/legion, IOC_GITHUB_AUTH_LEGION, which env.sh's read of
# the corpus and every row's curl but the reads of theorem-labs/agent-infra's pull requests
# send. The budget read is the token's when git credential fill gave one, and this
# address's unauthenticated one when it gave none and the file is empty, which GitHub
# answers with a limit of 60. A run with fewer requests left than the mode makes stops
# here, cannot run, of the kind source, naming which budget it is, how many of how many
# are left, how many the mode makes and the moment the budget resets, rather than printing
# failures nobody should act on. The count is a cold IOC_CACHE's, whatever the cache
# already holds, so a run with no token always stops here. An answer that holds no budget
# is left to the reads themselves, which name what GitHub answered them.
#
# The counts, at this tree's head: env.sh's read of the review corpus, a hundred reviews
# to a page for each pull request of evidence/concord-prs.txt, printed "env.sh read the
# review corpus: 5160 review bodies of 200 pull requests in 207 requests"; and the rows
# whose commands curl api.github.com make seven, two by n-arms-precedent and one each by
# n-3176-pr, tc-review-docs-only, tc-review-both-run, tc-input-1 and tc-input-2, and
# evidence/concord-build-run.sh, which tc-rng-2 runs, makes two more. A row or a script that
# comes to read that API, or a corpus that takes another page, changes them.
GITHUB_CORPUS_REQUESTS=207
GITHUB_ROW_REQUESTS=9
github_budget() {
    local answer remaining limit reset budget requests=$((GITHUB_CORPUS_REQUESTS + GITHUB_ROW_REQUESTS))
    # The self-test's fixture reads no GitHub, and it sets IOC_NO_ENV as it does for env.sh.
    [ "${IOC_NO_ENV:-0}" = 1 ] && return 0
    live_read "GitHub, the read of its rate limit" "$RUN_SCRATCH/rate-limit.json" curl -sS -H @"$IOC_GITHUB_AUTH_LEGION" https://api.github.com/rate_limit
    answer="$(cat "$RUN_SCRATCH/rate-limit.json")"
    remaining="$(printf '%s' "$answer" | jq -r '.resources.core.remaining // empty' 2>/dev/null)"
    limit="$(printf '%s' "$answer" | jq -r '.resources.core.limit // empty' 2>/dev/null)"
    reset="$(printf '%s' "$answer" | jq -r '.resources.core.reset // empty' 2>/dev/null)"
    [ -n "$remaining" ] || return 0
    [ "$remaining" -lt "$requests" ] || return 0
    if [ "$limit" = 60 ]; then
        budget="this address's unauthenticated GitHub budget, the one a read with no token draws on,"
    else
        budget="the GitHub budget of the token git credential fill gave for cybermonklabs/legion"
    fi
    cannot_run source " --replay now: $budget has $remaining of its $limit requests left, and this mode makes $requests: $GITHUB_CORPUS_REQUESTS to read env.sh's review corpus and $GITHUB_ROW_REQUESTS for the rows. The budget resets at $(date -u -d "@$reset" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || printf '%s' "$reset")."
}

# private_repository_read
#
# Rows of claims.jsonl read theorem-labs/agent-infra, a private repository: most with git,
# from the clone env.sh fetches through the machine's git credential helper, and a few its
# pull requests through the REST API, with the token the same helper gives. A reader whose
# git cannot read that repository would see env.sh refuse its clone, or each such row's read
# fail, a FAIL of replay-command that is no verdict on the deliverable. So the repository is
# read first, with git ls-remote through live_read, before the budget and before env.sh
# builds anything, and a run whose git cannot read it stops here, naming the repository and
# the rows that read it, taken from claims.jsonl and not from a list kept here, rather than
# printing a failure nobody should act on.
private_repository_read() {
    local name=theorem-labs/agent-infra file="$RUN_SCRATCH/private-repository" rows what
    rows="$(claims 2>/dev/null | jq -r 'select((.command // "") | test("agent-infra")) | .id' 2>/dev/null | paste -sd, - | sed 's/,/, /g')"
    what="GitHub's answer to git ls-remote of theorem-labs/agent-infra, the private repository the rows ${rows:-of claims.jsonl} read,"
    # The self-test's fixture reads no GitHub, and it sets IOC_NO_ENV as it does for env.sh.
    [ "${IOC_NO_ENV:-0}" = 1 ] && return 0
    env_functions || cannot_run gate " --replay: env.sh, which holds the git that never prompts, did not load."
    live_read "$what" "$file" ioc_env_git ls-remote "https://github.com/$name.git" HEAD
    grep -qE '^[0-9a-f]{40}'$'\t''HEAD$' "$file" && return 0
    cannot_run source " --replay: $what printed '$(head -c 160 "$file" | tr '\n' ' ')' where it names the repository's head, so git cannot read it"
}

# GITHUB_HEADER_AGE; the age in seconds at which --replay writes the GitHub header files
# again, checked before every row it runs (github_headers_fresh). A Legion pod's git
# credential fill gives a GitHub App installation token, which lives one hour, and a replay
# from a clean clone runs longer than that (3723 s at 882b668c, where the five rows that
# sent the file written at the start each failed with curl's exit 22 after the hour), so a
# file written once sends a dead token by the run's end. The pod's helper answers from a
# bundle its daemon refreshes in place, so a file written again carries a live token. Ten
# minutes keeps each file well inside its token's hour, at two calls of git credential fill
# a time. IOC_GITHUB_HEADER_AGE, one of FIXTURE_INPUTS, is check.test.sh's case 118's alone,
# which sets 0 so that every row meets files written again; a value that is no whole number,
# or is longer than GITHUB_HEADER_AGE, is read as GITHUB_HEADER_AGE. GITHUB_HEADERS_WRITTEN
# is 1 once github_headers has written the files in this run.
GITHUB_HEADER_AGE=600
GITHUB_HEADERS_WRITTEN=0

# github_headers; the GitHub header files (the header's "The GitHub tokens stand on no
# command line"): env.sh's ioc_env_github_auth writes one per repository of its
# ioc_env_github_table, each with umask 077, into github-headers.new under this run's
# scratch, and each is renamed over $RUN_SCRATCH/<variable>.header, which the variable then
# names. A rename replaces a file whole, so a reader meets the old file or the new one and
# never one half written, and each variable names one path through the run. The EXIT trap
# removes the scratch however the run ends. env.sh's read of the review corpus, the read of
# GitHub's budget and every recorded curl of GitHub's REST API send the files. --replay
# calls it before it sources env.sh, and github_headers_fresh calls it again while the rows
# run. A file that cannot be written or renamed is the run's own scratch failing, of the
# kind environment.
github_headers() {
    local new="$RUN_SCRATCH/github-headers.new" name _repo
    env_functions || cannot_run gate " --replay: env.sh, which holds how a GitHub header file is written, did not load."
    { mkdir -p -m 700 -- "$new" && IOC_ENV_SCRATCH="$new" ioc_env_github_auth; } 2>"$RUN_SCRATCH/github-headers.err" ||
        cannot_run environment " --replay: $(head -c 240 "$RUN_SCRATCH/github-headers.err" | tr '\n' ' ')"
    while IFS='|' read -r name _repo; do
        mv -f -- "$new/$name.header" "$RUN_SCRATCH/$name.header" 2>"$RUN_SCRATCH/github-headers.err" ||
            cannot_run environment " --replay: the header file $RUN_SCRATCH/$name.header could not be renamed into place: $(head -c 240 "$RUN_SCRATCH/github-headers.err" | tr '\n' ' ')"
        export "$name=$RUN_SCRATCH/$name.header"
    done < <(ioc_env_github_table)
    GITHUB_HEADERS_WRITTEN=1
}

# github_headers_fresh; before each row --replay runs, writes the GitHub header files again
# by github_headers when one of them is GITHUB_HEADER_AGE seconds old or older by its
# modification time, or has none to read, and does nothing in a run that wrote none. Every
# row is checked, whatever its command, since a row reads a file through a script it runs as
# well as on its own command line, as tc-rng-2 does through evidence/concord-build-run.sh,
# and the check is a stat of each file.
github_headers_fresh() {
    [ "$GITHUB_HEADERS_WRITTEN" = 1 ] || return 0
    local bound="${IOC_GITHUB_HEADER_AGE:-$GITHUB_HEADER_AGE}" now name _repo written stale=0
    [[ "$bound" =~ ^[0-9]+$ ]] && [ "$bound" -le "$GITHUB_HEADER_AGE" ] || bound="$GITHUB_HEADER_AGE"
    now="$(date +%s)"
    while IFS='|' read -r name _repo; do
        written="$(stat -c %Y -- "$RUN_SCRATCH/$name.header" 2>/dev/null)" && [ $((now - written)) -lt "$bound" ] || stale=1
    done < <(ioc_env_github_table)
    [ "$stale" = 0 ] || github_headers
}

# source_answer <id> <output-file>; reads the first failed read of a live source, GitHub or
# Dispatch, that the row's record holds with a status the row did not declare, and says
# whether the source's answer judges the row (the header's live-read rule). Only an answer
# about the record judges it. A read that got no answer, a server error (5xx), a rate limit,
# and an answer about the reader's credential, a 401 or a 403 that names authentication, a
# permission or a token rather than a rate limit, judge nothing of the deliverable: an
# expired token is the reader's, never a wrong deliverable. IOC_SOURCE_STOP then holds the
# reason the mode cannot run, naming the row, the source, the read and the line that shows
# it, as "GitHub refused the credential of tc-input-1's read of its REST API, so the row's
# failure is no verdict on the deliverable: ... GitHub answered 401: Bad credentials". Any
# other answer judges the row, and IOC_SOURCE_ANSWER names it for the row's FAIL line. Both
# are empty when no read of a source failed, or when one failed and printed nothing this
# rule reads. A source's reads are three, each found by its record (run_recorded):
#
#   - A curl of https://api.github.com/ or of DISPATCH_URL, by the URL its record holds.
#     Each such curl of a row runs with --fail-with-body, which exits 22 when the source
#     answered with an HTTP error, so any other exit, such as a transport error (6, 7, 28,
#     35, 52 or 56), is a read that got no answer, whatever comes after. A row's curl hands
#     the body to the row's pipeline and writes into the row's output only its own line,
#     "curl: (22) The requested URL returned error: 401", so the gate makes one read of its
#     own of that URL with the header file the curl sent (-H @<file>): a read again that gets
#     no HTTP status, curl's -w '%{http_code}' printing 000 or nothing, is no answer either,
#     and the line names curl's exit and the first line it printed on stderr. A status is
#     named with the body's message, GitHub's message or the code and the error of
#     Dispatch's error JSON, as "GitHub answered 401: Bad credentials" or "Dispatch answered
#     404 NOT_FOUND: not found". A 5xx is a server error. A 429, and GitHub's 403 whose
#     message names a rate limit or a secondary rate limit or whose x-ratelimit-remaining
#     header is 0, is a rate limit, named with the limit GitHub's headers give. A 401,
#     Dispatch's 403, which it answers a request whose credential lacks a scope or a project,
#     and GitHub's 403 whose message matches GITHUB_CREDENTIAL, as "Resource not accessible
#     by integration", are the reader's credential. Any other status is an answer.
#   - A dispatch command, by the first line it printed, which its function keeps in the
#     record, since the row's pipeline reads what it prints. DISPATCH_UNANSWERED holds the
#     lines of no answer: "Dispatch at <url> is unreachable: ...", which the CLI of the
#     worker image prints when it reaches no server, "HTTP_5NN: ...", which it prints for a
#     5xx whose body is no error JSON of Dispatch's, and each code Dispatch's API at
#     9542c6fb writes beside a 5xx status. DISPATCH_CREDENTIAL holds "HTTP_401: ...",
#     "HTTP_403: ..." and each code it writes beside a 401, and beside a 403 for the
#     request's credential, its scope or its project; DISPATCH_RATE_LIMITED holds "HTTP_429:
#     ..." and the code it writes beside a 429 for a credential. The CLI prints a refusal's
#     code and no status, so a code is read by these tables. Any other line, such as
#     "NOT_FOUND: not found", is Dispatch's answer about the record.
#   - A python3 of a row whose command reads DISPATCH_URL, by the last line of the row's
#     output that names urllib's error, the line a traceback ends with: a URLError is no
#     answer, and an HTTPError's "HTTP Error <status>: <message>" is read as the status of a
#     curl of Dispatch is.
#
# The read again is one request, made only on a failure, and its body, its headers and
# curl's stderr go into the run's scratch. Nothing of it is printed but the status, the
# message's first line, the rate limit's own headers and, when no status came, curl's own
# error line, so no token is, and a header file stands in a failure line by its path at
# most, in IOC_FAILED_READ.
DISPATCH_UNANSWERED='^(Dispatch at .* is unreachable: |HTTP_5[0-9][0-9]: |(AGENT_SECRETS_ACCOUNT_TOKEN_UNREADABLE|AGENT_SECRETS_UNAVAILABLE|ASK_STATE_INVALID|BROKER_UNAVAILABLE|DATABASE_UNAVAILABLE|ENVOY_REFILLING|ENVOY_UNAVAILABLE|ENVOY_UNSUBSCRIBE_FAILED|FILE_MISSING|FILE_STORE_UNAVAILABLE|GITHUB_APP_UNCONFIGURED|GITHUB_UNREADABLE|IDENTITY_ERROR|INTERNAL|OIDC_ISSUER_UNAVAILABLE|SHUTTING_DOWN|SIGNIN_UNAVAILABLE|SLOW_WRITE_CUT_OFF|STREAM_UNAVAILABLE|TEST_HOOK_FAILED|TEST_HOOK_TIMEOUT|TEST_HOOK_UNAVAILABLE): )'
DISPATCH_CREDENTIAL='^(HTTP_40[13]: |(UNAUTHORIZED|NO_IDENTITY|TOKEN_REVOKED|OIDC_TOKEN_INVALID|BROKER_TOKEN_NOT_ACTIVE|BROKER_TOKEN_KEY_MISMATCH|BROKER_TOKEN_PROOF_INVALID|BROKER_TOKEN_NOT_A_SESSION|OAUTH_SCOPE_REQUIRED|PROJECT_SCOPE|PROJECT_SCOPE_DEPLOYMENT|SESSION_SCOPE|SIGN_IN_REQUIRED|ACTOR_SESSION_MISMATCH): )'
DISPATCH_RATE_LIMITED='^(HTTP_429: |BROKER_TOKEN_RATE_LIMITED: )'
GITHUB_CREDENTIAL='authenticat|authoriz|credential|permission|not accessible|token|saml|sso|rights'
source_answer() {
    local id="$1" out="$2" failed_status failed_declared failed_command failed_said words word
    local source="" via="" what="" url="" header="" said="" kind="" detail="" answer="" code reread got row_got="" message="" error_code=""
    local remaining limit reset resource body="$RUN_SCRATCH/source-answer.body" dump="$RUN_SCRATCH/source-answer.headers"
    local header_sent='(^| )-H @([^ ]+)' http_error='^urllib\.error\.HTTPError: HTTP Error ([0-9]{3}): ?(.*)$'
    IOC_SOURCE_ANSWER="" IOC_SOURCE_STOP=""
    [ -s "$out.failed" ] || return 0
    while IFS=$'\t' read -r failed_status failed_declared failed_command failed_said; do
        declared_status_holds "$failed_status" "$failed_declared" && continue
        case "$failed_command" in
        "curl "*)
            read -r -a words <<<"$failed_command"
            for word in "${words[@]}"; do
                if [[ "$word" == https://api.github.com/* ]]; then
                    source=GitHub via=curl what="its REST API" url="$word"
                    break
                elif [ -n "${DISPATCH_URL:-}" ] && [[ "$word" == "${DISPATCH_URL%/}/"* ]]; then
                    source=Dispatch via=curl what="its API" url="$word"
                    break
                fi
            done
            ;;
        "dispatch "*)
            source=Dispatch via=dispatch what="its API through the dispatch command"
            [ "$failed_said" = - ] || said="$failed_said"
            ;;
        "python3 "*)
            grep -qF DISPATCH_URL "$out.command" 2>/dev/null && source=Dispatch via=python what="its API through python3"
            ;;
        esac
        [ -n "$source" ] && break
    done <"$out.failed"
    case "$via" in
    curl)
        [[ "$failed_command" =~ $header_sent ]] && header="${BASH_REMATCH[2]}"
        rm -f -- "$body" "$dump"
        code="$(curl -sS -o "$body" -D "$dump" -w '%{http_code}' ${header:+-H "@$header"} "$url" 2>"$body.err")"
        reread=$?
        if [[ "$code" =~ ^[1-5][0-9][0-9]$ ]]; then
            got="got $code"
            if [ "$source" = GitHub ]; then
                message="$(jq -r 'if type == "object" and (.message | type) == "string" then .message | split("\n")[0] | .[0:240] else empty end' "$body" 2>/dev/null)"
            else
                error_code="$(jq -r 'if type == "object" and (.code | type) == "string" then .code | .[0:80] else empty end' "$body" 2>/dev/null)"
                message="$(jq -r 'if type == "object" and (.error | type) == "string" then .error | split("\n")[0] | .[0:240] else empty end' "$body" 2>/dev/null)"
            fi
            answer="$source answered $code${error_code:+ $error_code}${message:+: $message}"
            case "$code" in
            5??) kind=server ;;
            401) kind=credential ;;
            403 | 429)
                IFS='|' read -r remaining limit reset resource < <(awk -F ': *' '{ sub(/\r$/, "") } { v[tolower($1)] = $2 }
                    END { printf "%s|%s|%s|%s\n", v["x-ratelimit-remaining"], v["x-ratelimit-limit"], v["x-ratelimit-reset"], v["x-ratelimit-resource"] }' "$dump" 2>/dev/null)
                if [ "$code" = 429 ] || { [ "$source" = GitHub ] && { [[ "${message,,}" == *"rate limit"* ]] || [ "$remaining" = 0 ]; }; }; then
                    kind=rate
                    [ -z "$limit" ] ||
                        answer="$answer; its ${resource:+$resource }rate limit has ${remaining:-none} of its $limit requests left${reset:+ until $(date -u -d "@$reset" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || printf '%s' "$reset")}"
                elif [ "$source" = Dispatch ] || [[ "${message,,}" =~ $GITHUB_CREDENTIAL ]]; then
                    kind=credential
                fi
                ;;
            esac
        else
            got="exited $reread with no HTTP status: $(head -1 "$body.err" | head -c 160)"
            kind=unanswered
        fi
        # The row's curl runs with --fail-with-body, which exits 22 when the source answered
        # with an HTTP error, so any other status is a read of the row's that got no HTTP
        # answer, and what the gate's read got after it judges nothing the row read.
        [ "$failed_status" = 22 ] || {
            kind=unanswered
            row_got=" with no HTTP answer"
        }
        case "$kind" in
        unanswered) detail="the row's curl of $url exited $failed_status$row_got, and the gate's read of it again $got" ;;
        server) detail="the row's curl of $url exited 22, and when the gate read it again $answer, a server error" ;;
        credential) detail="the row's curl of $url exited 22, and when the gate read it again $answer" ;;
        rate) detail="$answer" ;;
        esac
        ;;
    dispatch)
        if [[ "$said" =~ $DISPATCH_RATE_LIMITED ]]; then
            kind=rate
        elif [[ "$said" =~ $DISPATCH_CREDENTIAL ]]; then
            kind=credential
        elif [[ "$said" =~ $DISPATCH_UNANSWERED ]]; then
            kind=unanswered
        fi
        answer="${said:+the dispatch command printed '$said'}"
        detail="'${failed_command% }' exited $failed_status and printed '$said'"
        ;;
    python)
        # urllib wraps a failure to connect in URLError, but a read that times out or is cut off
        # after the connection raises the socket's or http.client's own exception, unwrapped:
        # each of those is no answer too.
        said="$(grep -E '^(urllib\.error\.(URLError|HTTPError)|TimeoutError|socket\.timeout|Connection(Reset|Refused|Aborted)Error|BrokenPipeError|http\.client\.(RemoteDisconnected|IncompleteRead))(: |$)' "$out" | tail -1 | tr -d '\r' | head -c 240)"
        if [ -n "$said" ] && [[ "$said" != urllib.error.HTTPError:* ]]; then
            kind=unanswered
        elif [[ "$said" =~ $http_error ]]; then
            code="${BASH_REMATCH[1]}" message="${BASH_REMATCH[2]}"
            answer="Dispatch answered $code${message:+: $message}"
            case "$code" in
            5??) kind=server ;;
            401 | 403) kind=credential ;;
            429) kind=rate ;;
            esac
        fi
        detail="its python3 exited $failed_status, and the last urllib or transport error of its output is '$said'"
        ;;
    *) return 0 ;;
    esac
    case "$kind" in
    "")
        IOC_SOURCE_ANSWER="$answer"
        return 0
        ;;
    rate) IOC_SOURCE_STOP="$source's rate limit refused $id's read of $what" ;;
    credential) IOC_SOURCE_STOP="$source refused the credential of $id's read of $what" ;;
    *) IOC_SOURCE_STOP="$source gave no answer that judges $id's read of $what" ;;
    esac
    IOC_SOURCE_STOP="$IOC_SOURCE_STOP, so the row's failure is no verdict on the deliverable: $detail"
}

# unpinned_installs; prints one line per package a recorded command installs or runs with
# no version, and per form this reader cannot read, as <id><TAB><what it runs>, over every
# row of claims.jsonl. It fails closed: what it does not recognise is refused and named,
# never passed.
#
# The rule. A package is pinned when it names a full release, nothing less:
#   - for Python (uvx, uv tool run, uv tool install, uv run --with, pipx run, pip install,
#     uv pip install), the name, then == and dotted digits only, such as pyshacl==0.40.2;
#   - for npm (bunx, npx, npm exec), the name, then @ and a full major.minor.patch, such
#     as tsx@4.1.0;
#   - for Go (go run, go install), the module, then @v and a full major.minor.patch, or a
#     Go pseudo-version, or a commit hash.
# A wildcard, a partial version, a tag such as latest or next, a comparison (>=, ~=, ^, ~)
# or any other spelling is no version. A local path and a variable of this deliverable
# that names one (IOC_...) are pinned. An option of a runner the reader does not know, a
# package written as any other variable, and a command it cannot open are refused.
#
# Three guarantees hold the lock: uv's, this reader's, and the check of what each
# environment holds after it is built and after every row that installs into it. uv's:
# env.sh exports UV_REQUIRE_HASHES=1, UV_BUILD_CONSTRAINT (the hashed
# evidence/build-constraints.txt) and PIP_REQUIRE_HASHES=1 into every row's environment.
# It sets uv's and pip's other settings there too, whatever the caller's shell held:
# UV_NO_CONFIG=1 and PIP_CONFIG_FILE=/dev/null, so neither reads a configuration file, and
# it unsets every variable that would point either at another source of packages, change
# which index a package is taken from, keep it from its index, name a configuration file or
# keep uv from checking a hash. It unsets as well the five optional inputs of mas-lab's
# programs that no row sets, MAS_MANIFEST_VALIDATE, MAS_MANIFEST_RESOLVE_REFS,
# MAS_MANIFEST_STRICT, OPENAI_API_KEY and MAS_LLM_API_KEY (IOC_ENV_ENFORCED in env.sh lists
# all twenty-three). The reader reserves each of those names, so no row assigns, unsets or
# re-points one. Measured with uv 0.13.5 and the three hash variables set, once with a
# package no lock hashes (requests==2.32.3, or setuptools as a build backend) and once with
# a local build under an unhashed -b:
#
#   spelling                   package no lock hashes       build under an unhashed -b
#   uv pip install -c/-r/-e/-b refused: "all requirements  refused: "all requirements
#                              must have a hash"            must have a hash" (the local
#                                                           source itself has none)
#   uv pip sync                refused: the same            refused: the same
#   uv build                   refused: "no trusted hash    refused: "No solution found
#                              was provided for setuptools" when resolving: hatchling"
#   uv run --with              installed, no refusal        built, no refusal; uv run reads
#                                                           no UV_BUILD_CONSTRAINT at all
#   uv tool run --from         installed, no refusal        built, no refusal
#   uvx                        installed, no refusal        built, no refusal
#   pip install                refused: "Hashes are         refused: "Can't verify hashes
#                              required in --require-hashes for these file:// requirements"
#                              mode"
#
# uvx and uv tool run check no hash a -c file carries either (a wrong hash for owlrl in
# -c passed), and uv run --with of a wheel checks no hash: the wheel has none to check.
# Under UV_REQUIRE_HASHES=1 uv refuses every local source path, since a directory has no
# hash, so one install is written with the visible prefix UV_REQUIRE_HASHES=0, and the
# classifier admits it there alone: before a uv pip install whose every package is a local
# source path, into an environment of env.sh's IOC_ENV_FROZEN named with --python or
# VIRTUAL_ENV, that names that environment's own lock as a constraint (-c, --constraint
# or --constraints) and no other constraint or requirements file (-r, --requirement or
# --requirements, refused whatever it names), names "$IOC_EVID/build-constraints.txt" as
# every build constraint (-b, --build-constraint or --build-constraints), each spelling
# read with its value beside it or after =, and passes --require-build-hashes. Under the
# prefix uv checks the hash of every package its -c lists and of every build dependency
# -b lists, and --require-build-hashes makes it refuse a build dependency -b does not list
# (measured: a setuptools-backend package fetched setuptools unhashed without it, and was
# refused with it; uv 0.13.5 calls the option experimental and prints a warning, which no
# row's predicate reads). A package it fetches that the environment's lock does not list
# installs unchecked, and the third guarantee refuses it; a package the lock lists was
# checked against the lock's hash, because the constraint is that lock. Each condition
# broken is refused by name.
#
# The reader's guarantee, which holds for every spelling uv does not enforce and stands
# behind uv for the rest. Each package a command names from an index is looked up in the
# hashed locks the command names, $IOC_EVID/<file> as a constraint or requirements file in
# every spelling above for pip install and uv pip install, with -c or --constraints for
# uvx and uv tool, and evidence/ioc-venv.lock for
# uv run, which takes no lock: a package no named lock holds at that exact version is
# refused by name, whatever other file the command names, and an install that names no
# lock is refused as "an install that reads no lock", as is a requirements file that is no
# hashed lock and a uv pip sync of one. pipx run, npx, bunx and npm exec take no lock, so a
# package they would resolve from an index is refused; go run and go install are left to
# go, which checks each module against go.sum or the checksum database. The locks are
# evidence/ioc-venv.lock, all 53 packages of IOC_VENV, evidence/tm-constraints.txt, the 24
# third-party packages of the mas-lab environments, and evidence/build-constraints.txt,
# each resolved at 2026-11-12T07:55:56Z, the instant sources.json records, from PyPI and,
# for torch alone, the PyTorch CPU index, and each read by env.sh's ioc_env_lock_py, the
# one reader of a lock.
#
# The third guarantee checks the result rather than the command. After every row, the
# gate runs env.sh's ioc_env_frozen on each environment of IOC_ENV_FROZEN the install
# reader finds the row installing into (by --python, -p, a VIRTUAL_ENV prefix or the pip
# it runs), and on each whose site-packages changed while the row ran, which finds an
# install inside a script the row runs. It lists every package installed there and fails
# install-pinned, naming the row, the environment, the package, its version and the lock,
# for a package from an index that the environment's own lock does not hold at that
# version, a package from a path or a URL that its pinned clone does not declare at that
# version, in a pyproject.toml at the pinned commit, or that was not installed from
# inside the clone, and a dist-info it cannot read; a check that ends without a verdict
# fails install-pinned too. env.sh runs the same check after it builds each environment.
#
# What the third guarantee can prove. uv 0.13.5 records, for a package from an index,
# METADATA (its name and version), RECORD (the sha256 of each file it installed),
# INSTALLER, REQUESTED and WHEEL, and for one from a path, direct_url.json, uv_build.json
# and uv_cache.json besides (measured in the environments env.sh builds). Nothing records
# the hash or the index of the archive a package came from. So the check proves a
# package's name, its version and whether it came from an index or from a path inside
# the clone, and nothing of its bytes: a package fetched unhashed at the version the lock
# holds passes it. The bytes are held by uv's hash check when it installs: every install
# but the exemption runs under UV_REQUIRE_HASHES=1, and the exemption's -c is the
# environment's own lock, so a package the check passes was checked against that lock's
# hash, and one the lock does not hold is refused.
#
# The environments made for one run are out of the third guarantee's reach: uv run --with,
# uv tool run --from and --with, uvx, pipx run, npx, bunx and npm exec. For them the
# reader's rule above stands alone, and it passes only a set of packages wholly in a lock,
# or a wheel: uvx and uv tool run a package the lock their -c names holds, whose
# dependencies -c holds to the lock's versions (though neither checks their hashes),
# and uv run, which takes no -c, a package ioc-venv.lock holds that requires no other
# package by the lock's "# via" lines; a package that requires others is refused.
#
# A build backend is an install. A local path that is no wheel is built, and its build
# backend and that backend's dependencies are fetched like any package: every local
# package of this replay declares build-system requires = ["hatchling"] with no version.
# So a pip install, a uv pip install (-e and --editable in every spelling) and a uv build
# of a local source path name -b "$IOC_EVID/build-constraints.txt", the seven packages of
# hatchling's build environment each at ==version and hashed, and any other -b is refused;
# uv run, uv tool run, uvx and pipx run check no hash of a build, so a local source path
# under any of them is refused. A wheel, a path ending in .whl or a variable env.sh sets to
# one (WHEEL_VARIABLES, IOC_CONCORD_WHEEL), builds nothing and passes. An option under
# which uv would check no hash (--no-require-hashes, --no-verify-hashes, --no-build-
# isolation) is refused by name.
#
# Every word in command position is read, and one this reader cannot resolve fails the
# row and is named. Command position is the first word of a command after its assignments
# and its keywords, the command a wrapper runs (env, nohup, nice, timeout, command, exec,
# sudo, stdbuf, time and xargs) and the command find runs after -exec. A command handed to
# a shell as a string (sh, bash, zsh, dash or ksh with -c, and eval) and the code of every
# substitution are read with this same reader, and the output of a substitution is no
# command word it can resolve. A command word is a literal, or one of a closed list of
# five variables, each of which env.sh sets to one program: $IOC_PY, $IOC_MASLAB_PY and
# $IOC_TC_PY, which resolve to python, and $IOC_MASLAB_CTL and $IOC_MASLAB_WHEEL_CTL,
# which resolve to mas-ctl; a variable with a path after it is no command word. A
# resolved word is held to every rule a literal is, so python code given
# to $IOC_PY -c that starts a process is refused as python -c is. Any other variable, a
# word built of several, and a command this reader cannot split are refused.
#
# A runner is known by the program a word names, not by how the word is spelled: the
# resolved word, its last path element, with a version after the name taken off, so
# /usr/bin/pip, pip3.12 and python3.12 -m pip are each pip and
# held to the rule for pip, and every runner above is matched that one way.
#
# What this reader cannot see. It compares text, and it guards against an install someone
# forgets to pin, not against one written to slip past it. A program that installs
# packages under a name no rule here names (a renamed copy of a runner, a wrapper script
# of the row's own, a script a row runs, or a binary that installs as a side effect) is
# outside what a reader of command text can see, and no further rule of this kind closes
# that. The third guarantee covers such an install into one of env.sh's environments,
# since it reads what the environment holds whatever wrote it; one into any other place
# is covered by a person reading every row at review, and by env.sh, which pins the
# interpreters and environments the rows run.
#
# reader_py prints the reader of command text, one Python program that holds two readers
# of command position over one tokenizer. The install reader (commands, command_words,
# KEYWORDS, CONTROL and WRAPPERS) finds every runner, inside forms the evaluator refuses
# too; the classifier (Parser, classify_word and check_simple) admits a command only
# through ADMITTED. The rules both read are each written once and read by both:
# XARGS_OPTIONS and xargs_program for xargs's options, UV_RUN_OPTIONS and uv_run_program
# for uv run's, shell_string for a shell given -c, variable_name for a variable command
# word, ASSIGNMENT for an assignment, and tokenize, which refuses a character bash does
# not read as it would, by foreign_character. Its last argument selects what it prints:
# "installs" reads the rows of claims.jsonl on stdin, as the gate's one parse read them
# (claims), and prints each refusal as <id><TAB><what it runs>; "environments" reads those
# rows and prints <id><TAB><label> for each environment of env.sh's IOC_ENV_FROZEN a row's
# command installs into; "names" reads one command on stdin and prints each program name
# the classifier admits in command position, one per line, or one UNREAD line naming the
# refusal; "stages" reads one command and prints the command of each stage of its final
# pipeline; "admitted" reads the rows and prints each refused row, each entry of ADMITTED
# with the number of rows that use it and each reserved name, which --admitted reads;
# "load" reads nothing, so the gate can start the reader before it reads a row
# (reader_load). The reader's first lines are PROGRAM_VARIABLES and RUNNER_PREFIXES,
# written from the two variables above program_shims, then env.sh's ENFORCED and
# ENVIRONMENTS, and env.sh's reader of a lock.
reader_py() {
    local v label
    # env.sh's IOC_ENV_ENFORCED, the variables it sets or clears in every row's environment,
    # which the reader reserves; its IOC_ENV_FROZEN, the environments a row may install into,
    # which the gate checks after the row; and ioc_env_lock_py, the one reader of a hashed
    # lock.
    # env.sh read for its functions defines them and prepares nothing.
    env_functions || return 1
    printf 'PROGRAM_VARIABLES = {'
    for v in $PROGRAM_VARIABLES; do
        printf '"%s": "%s", ' "${v%%:*}" "${v#*:}"
    done
    printf '}\nRUNNER_PREFIXES = ('
    for v in $RUNNER_PREFIXES; do
        printf '"%s", ' "$v"
    done
    printf ')\nENFORCED = ('
    for v in $IOC_ENV_ENFORCED; do
        printf '"%s", ' "$v"
    done
    printf ')\nENVIRONMENTS = {'
    for label in $(ioc_env_frozen_labels); do
        ioc_env_frozen_entry "$label"
        printf '"%s": ("%s", "%s"), ' "$label" "$IOC_ENV_ENTRY_WRITTEN" "$IOC_ENV_ENTRY_LOCK"
    done
    printf '}\nLOCKS_DIR = %s\n' "$(python3 -c 'import json, sys; print(json.dumps(sys.argv[1]))' "$EVIDENCE/evidence")"
    ioc_env_lock_py
    cat <<'PYREADER'
import json, os, re, shlex, sys

PY_PIN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*(\[[A-Za-z0-9,._-]+\])?==[0-9]+(\.[0-9]+)*$")
NPM_PIN = re.compile(r"^(@[A-Za-z0-9._-]+/)?[A-Za-z0-9][A-Za-z0-9._-]*@[0-9]+\.[0-9]+\.[0-9]+$")
GO_PIN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._~/-]*@(v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]*[0-9]{14}-[0-9a-f]{12})?|[0-9a-f]{7,40})$")
SHELLS = {"sh", "bash", "zsh", "dash", "ksh"}
PROCESS = re.compile(r"subprocess|os\.system|os\.popen|os\.exec|os\.spawn|pty\.spawn")

def local(arg):
    return arg.startswith((".", "/", "~")) or re.match(r"\$\{?IOC_[A-Z_]+\}?(/|$)", arg) is not None

# The variables env.sh sets to a wheel it built under evidence/build-constraints.txt, each
# one file whose name ends in .whl, which env.sh's header names.
WHEEL_VARIABLES = ("IOC_CONCORD_WHEEL",)

def wheel(arg):
    # True for a local path that is a built wheel, which is installed with no build: a
    # file named *.whl, or a variable WHEEL_VARIABLES holds.
    m = re.fullmatch(r"\$(?:\{([A-Z_]+)\}|([A-Z_]+))", arg)
    return arg.endswith(".whl") or (m is not None and (m.group(1) or m.group(2)) in WHEEL_VARIABLES)

# An install that resolves a package from an index reads a committed lock: a file under
# evidence/, named on the command as $IOC_EVID/<file>, every requirement of which carries
# a hash, read from LOCKS_DIR, this tree's evidence/, by read_lock, env.sh's one reader of
# a lock. Every package the command names is looked up in the locks it names, at that
# exact version; naming a lock exempts nothing it does not hold.
NO_LOCK = " (an install that reads no lock: name a hashed lock under $IOC_EVID)"
NOT_HELD = " (a package no lock the command names holds at that version)"
LOCKS = {}

def lock_read(path):
    # read_lock's reading of a hashed lock named as $IOC_EVID/<file>, or None for any
    # other path, a file that is missing and a file one of whose requirements carries no
    # hash.
    m = re.fullmatch(r"\$\{?IOC_EVID\}?/([A-Za-z0-9_][A-Za-z0-9._-]*)", path or "")
    if m is None:
        return None
    if m.group(1) not in LOCKS:
        LOCKS[m.group(1)] = read_lock(os.path.join(LOCKS_DIR, m.group(1)))
    return LOCKS[m.group(1)]

def lock_packages(path):
    # {normalized name: version} of a hashed lock named as $IOC_EVID/<file>, or None.
    found = lock_read(path)
    return None if found is None else found[0]

def hashed_lock(path):
    return lock_packages(path) is not None

def requires(package, path):
    # True when package, name==version, requires any other package, by the "# via" lines
    # of the hashed lock named path, and when that lock carries no such line, since then
    # what it requires cannot be read.
    found = lock_read(path)
    if found is None or found[1] is None:
        return True
    name = normalized(package.split("==", 1)[0].split("[", 1)[0])
    return any(name in by for by in found[1].values())

# The environment an install writes into, which the gate checks after the row: one of
# ENVIRONMENTS, env.sh's IOC_ENV_FROZEN, named as the rows name it, by its directory
# ($IOC_TC_WORK/venv) or by the python or pip under it ($IOC_TC_WORK/venv/bin/python). A
# uv pip install or sync names it with --python or -p, or with a VIRTUAL_ENV prefix; pip
# installs into the environment of the pip it runs, a path ending in /bin/pip or a python
# given -m pip. One that writes outside an environment (--system, --target, --prefix,
# --user, --root) or names none of them installs where the gate checks nothing.
OUTSIDE = {"--system", "--target", "-t", "--prefix", "--user", "--root"}
TARGETS = set()  # the environments read_command found an install into, for "environments"

def environment_of(written):
    if not written:
        return None
    path = re.sub(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}", r"$\1", written)
    base = re.sub(r"/bin/(python|pip)[0-9.]*$", "", path)
    for label, (directory, _) in ENVIRONMENTS.items():
        if base == directory:
            return label
    return None

def install_environment(w, i):
    # The label of the environment the pip, uv pip install or uv pip sync at w[i] (the
    # word pip) installs into, or None.
    rest, written, j = w[i + 2:], None, 0
    while j < len(rest):
        key, value = rest[j].split("=", 1)[0], option_value(rest[j])
        if key in OUTSIDE:
            return None
        if key in ("--python", "-p"):
            written = value if value is not None else (rest[j + 1] if j + 1 < len(rest) else "")
        j += 1
    if i > 0 and resolve(w[i - 1]) == "uv":
        for word in w[:i - 1]:
            if word.startswith("VIRTUAL_ENV=") and written is None:
                written = word.split("=", 1)[1]
    elif "/" in w[i]:
        written = w[i]
    elif i > 1 and w[i - 1] == "-m":
        written = w[i - 2]
    return environment_of(written)

def held(package, locks):
    # True when package, name==version, is held at that version by one of locks.
    m = re.fullmatch(r"([A-Za-z0-9][A-Za-z0-9._-]*)(?:\[[^]]*\])?==(.+)", package)
    return m is not None and any(lock.get(normalized(m.group(1))) == m.group(2) for lock in locks)

# A build backend is an install: every local source is built under the hashed
# evidence/build-constraints.txt itself, named with -b, and no other -b is a build lock.
BAD_BUILD = " (a build constraint that is not the hashed $IOC_EVID/build-constraints.txt)"
NO_BUILD = " (a local source path built with no build constraint; pass -b $IOC_EVID/build-constraints.txt)"

def build_lock(value):
    return re.fullmatch(r"\$\{?IOC_EVID\}?/build-constraints\.txt", value or "") is not None and hashed_lock(value)

def read_build(label, sources, builds):
    # The one build rule: each -b is the hashed build lock, and a command that builds a
    # local source names it.
    problems = [label + " -b " + b + BAD_BUILD for b in builds if not build_lock(b)]
    if sources and not any(build_lock(b) for b in builds):
        problems.append(label + " " + sources[0] + NO_BUILD)
    return problems

# The options under which uv or pip would check no hash, or build outside its isolated,
# constrained environment; a row passes none of them.
UNENFORCED = {"--no-require-hashes", "--no-verify-hashes", "--no-build-isolation", "--no-build-isolation-package"}
CONSTRAINT_KEYS = ("-c", "--constraint", "--constraints")
REQUIREMENT_KEYS = ("-r", "--requirement", "--requirements")
BUILD_KEYS = ("-b", "--build-constraint", "--build-constraints")
PIP_VALUE = {*CONSTRAINT_KEYS, *REQUIREMENT_KEYS, *BUILD_KEYS,
             "--python", "-p", "--target", "--prefix", "--index-url", "--extra-index-url", "-e", "--editable"}

def pip_words(rest):
    # The words of a pip install, uv pip install or uv pip sync after its subcommand:
    # (packages, locks, unlocked requirements files, -b values, options uv would enforce
    # nothing under). -e and --editable take a path in every spelling: -e PATH, -ePATH,
    # --editable PATH and --editable=PATH.
    packages, locks, unlocked, builds, flags, j = [], [], [], [], [], 0
    while j < len(rest):
        a = rest[j]
        if a[:1] in (">", "<") or re.match(r"[0-9]*[<>]", a):
            break
        key, value = a.split("=", 1)[0], option_value(a)
        if a.startswith("-e") and not a.startswith("--") and a != "-e":
            key, value = "-e", a[2:]
        if value is None and key in PIP_VALUE:
            value = rest[j + 1] if j + 1 < len(rest) else ""
            j += 1
        if key in BUILD_KEYS:
            builds.append(value)
        elif key in CONSTRAINT_KEYS + REQUIREMENT_KEYS:
            pins = lock_packages(value)
            if pins is not None:
                locks.append(pins)
            elif key in REQUIREMENT_KEYS:
                unlocked.append(value)
        elif key in ("-e", "--editable"):
            packages.append(value)
        elif key in UNENFORCED:
            flags.append(key)
        elif not a.startswith("-"):
            packages.append(a)
        j += 1
    return packages, locks, unlocked, builds, flags

def named_files(rest):
    # Each (option, file) of a constraint, a requirements file or a build constraint, in
    # every spelling of CONSTRAINT_KEYS, REQUIREMENT_KEYS and BUILD_KEYS, among the words
    # of a pip install, uv pip install or uv pip sync after its subcommand.
    named, j = [], 0
    while j < len(rest):
        key, value = rest[j].split("=", 1)[0], option_value(rest[j])
        if key in CONSTRAINT_KEYS + REQUIREMENT_KEYS + BUILD_KEYS:
            if value is None:
                value = rest[j + 1] if j + 1 < len(rest) else ""
                j += 1
            named.append((key, re.sub(r"\$\{IOC_EVID\}", "$IOC_EVID", value)))
        elif value is None and key in PIP_VALUE:
            j += 1
        j += 1
    return named

def hash_exemption(words, prefixes):
    # None when words, the command after a prefix UV_REQUIRE_HASHES=0 and the other
    # prefixes, is the one form the header above reader_py states the prefix is admitted
    # before, or the condition of that form it breaks.
    if words[:3] != ["uv", "pip", "install"]:
        return "it prefixes no uv pip install"
    rest = words[3:]
    packages, _, _, _, flags = pip_words(rest)
    if not packages or any(not local(p) for p in packages):
        return "a package it installs is no local source path"
    named = named_files(rest)
    builds = [(key, value) for key, value in named if key in BUILD_KEYS]
    if not builds:
        return "it names no -b $IOC_EVID/build-constraints.txt"
    for key, value in builds:
        if not build_lock(value):
            return "it names " + key + " " + value + ", which is not the hashed build lock $IOC_EVID/build-constraints.txt"
    label = install_environment(prefixes + words, len(prefixes) + 1)
    if label is None:
        return "it installs into no environment env.sh builds, named with --python or VIRTUAL_ENV, which the gate checks after the row"
    own = "$IOC_EVID/" + ENVIRONMENTS[label][1]
    locks = [(key, value) for key, value in named if key not in BUILD_KEYS]
    for key, value in locks:
        if key in REQUIREMENT_KEYS:
            return "it names " + key + " " + value + ", and the lock of " + label + " is read with -c alone"
        if value != own:
            return "it names " + key + " " + value + ", which is not " + own + ", the lock of " + label
    if not locks:
        return "it names no -c " + own + ", the lock of " + label
    if not hashed_lock(own):
        return own + ", the lock of " + label + ", is no committed hashed lock"
    if flags:
        return "it names " + flags[0] + ", an option under which uv checks no hash"
    if "--require-build-hashes" not in rest:
        return "it does not pass --require-build-hashes, so uv would check the hash of no build dependency -b leaves out"
    return None

def py_ok(arg):
    return local(arg) or PY_PIN.match(arg) is not None

def npm_ok(arg):
    return local(arg) or NPM_PIN.match(arg) is not None

def go_ok(arg):
    return local(arg) or GO_PIN.match(arg) is not None

OPS = ["&&", "||", ">>", ">&", "&>", "<<<", "<<", ";;", "|&", ";", "&", "|", "(", ")", "<", ">", "\n"]

class Refused(Exception):
    pass

# The programs the rows run by a bare word. Each is an entry "program <name>" of ADMITTED,
# and the runner wraps each in a function of its name that records its status; a program
# name with no entry is refused by name, whatever the shell would run. gh is not one: the
# gate reads GitHub through git and the REST API and never through gh (the header).
PROGRAMS = ("awk", "cat", "cd", "cmp", "cp", "curl", "cut", "dispatch", "echo", "git",
            "go", "grep", "jq", "mkdir", "mktemp", "printf", "python3", "rm", "sed", "sh",
            "sha256sum", "sort", "stat", "tail", "tr", "uniq", "uv", "wc", "xargs")

# The names the rows assign, alone, before a command or as a for loop's variable. Each is
# an entry "assigns <name>" of ADMITTED, and an assignment or a loop to any other name is
# refused by name, the shell's own names among them. IOC_EXPECT_STATUS is admitted as the
# last prefix of one command word alone, and UV_REQUIRE_HASHES as the prefix 0 of the one
# install hash_exemption reads. No tool cache is here: env.sh's ioc_env_cache_table sets
# each, so a row that assigns one is refused.
ASSIGNED_NAMES = ("ACCORD_MODEL_DIR", "GOFLAGS", "IOC_EXPECT_STATUS",
                  "UV_REQUIRE_HASHES", "VIRTUAL_ENV", "W", "c", "f", "g", "m", "n", "o", "p", "r")
HASH_EXEMPT = "UV_REQUIRE_HASHES"

# uv run runs the program its arguments name, as xargs does, and exits with its status.
# The programs the rows run with it are each an entry "uv run <program>" of ADMITTED; any
# other program is refused by name, and the program is held with its arguments to every
# rule check_program holds a command word to. Its programs are not PROGRAMS: uv run finds
# python itself, and the runner wraps none of them. Its options are UV_RUN_OPTIONS below.
UV_RUN_PROGRAMS = {
    "python": "uv run's python, whose status uv run exits with",
}

# The rules the install reader and the classifier both read, each written once.
#
# xargs's options the rows use, each to whether it takes a value and what it is. Each is
# an entry "xargs <option>" of ADMITTED. Both readers step over these to reach the program
# xargs runs, and both refuse any other option by name, since neither knows whether it
# takes a value and so which word is the program.
XARGS_OPTIONS = {
    "-0": (False, "xargs reads its input split at NUL bytes"),
    "-n": (True, "xargs passes at most N arguments to each run"),
}

def xargs_program(args):
    # (options, j) for the arguments of xargs: the options before its program, and the
    # index in args of the program, len(args) when it names none.
    options, j = [], 0
    while j < len(args) and args[j].startswith("-"):
        if args[j] not in XARGS_OPTIONS:
            raise Refused("xargs " + args[j] + ", an option of xargs the evaluator does not admit")
        options.append(args[j])
        j += 2 if XARGS_OPTIONS[args[j]][0] else 1
    return options, min(j, len(args))

# uv run's options the rows use, each to whether it takes a value and what it is, read
# the same way: each is an entry "uv run <option>" of ADMITTED, the install reader reads
# the value of each --with, and both readers refuse any other option by name.
UV_RUN_OPTIONS = {
    "--no-project": (False, "uv run reads no project of the working directory"),
    "--python": (True, "uv run runs the python version named"),
    "--with": (True, "uv run installs the package named into its environment"),
}

def uv_run_program(args):
    # (options, j) for the arguments of uv run, the words after run: each option before
    # its program with its value, None for an option that takes none, and the index in
    # args of the program, len(args) when it names none.
    options, j = [], 0
    while j < len(args) and args[j].startswith("-"):
        if args[j] not in UV_RUN_OPTIONS:
            raise Refused("uv run " + args[j] + ", an option of uv run the evaluator does not admit")
        takes = UV_RUN_OPTIONS[args[j]][0]
        options.append((args[j], (args[j + 1] if j + 1 < len(args) else "") if takes else None))
        j += 2 if takes else 1
    return options, min(j, len(args))

def shell_string(args):
    # The index in a shell's arguments of the option that hands it its commands as a
    # string, -c alone or among other letters as in -ec, when it stands among the options
    # before the script; None when there is none.
    for j, arg in enumerate(args):
        if not arg.startswith("-"):
            return None
        if "c" in arg.lstrip("-"):
            return j
    return None

VARIABLE_WORD = re.compile(r"^\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$")

def variable_name(word):
    # The name of a word that is one variable, $NAME or ${NAME}, and nothing beside it;
    # None for any other word.
    m = VARIABLE_WORD.match(word)
    return (m.group(1) or m.group(2)) if m else None

# An assignment: a name, then a subscript or none, then = or +=. The classifier admits only
# a plain one, a name and =; a subscript or += is a form of its own.
ASSIGNMENT = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)(\[[^]]*\])?(\+?)=")

def foreign_character(text):
    # The first character of text that bash does not read as the tokenizer below reads
    # it, as U+XXXX, or None. bash splits words at space, tab and newline alone, so a
    # carriage return, any other control character and DEL, and every character Python
    # counts as a space that bash does not (U+000B, U+000C, U+001C to U+001F, U+0085,
    # U+00A0, U+2028 and the rest) stand inside a word, where the tokenizer would split.
    for ch in text:
        if ch in "\t\n":
            continue
        if ord(ch) < 32 or ord(ch) == 127 or (ch.isspace() and ch != " "):
            return "U+%04X" % ord(ch)
    return None

# The names the gate records with, which no row may touch: every name of the runner, which
# starts with one of RUNNER_PREFIXES, the program variables, the declared status, and the
# variables env.sh sets or clears in every row's environment, ENFORCED, env.sh's
# IOC_ENV_ENFORCED. A row may write the declared status only as the last prefix of one
# command, UV_REQUIRE_HASHES only as the prefix 0 of the install hash_exemption reads, and a
# program variable only as a command word.
EXPECT_STATUS = "IOC_EXPECT_STATUS"
RESERVED = {
    **{prefix + "*": "a name the runner or a shim records with" for prefix in RUNNER_PREFIXES},
    **{name: "a program variable, which points at its shim" for name in PROGRAM_VARIABLES},
    EXPECT_STATUS: "the declared status, the last prefix of one command",
    **{name: "a variable env.sh sets or clears in every row's environment" for name in ENFORCED},
}

def reserved(name):
    return (name.startswith(RUNNER_PREFIXES) or name in PROGRAM_VARIABLES or name == EXPECT_STATUS
            or name in ENFORCED)

READS = re.compile(r"\$\{?([A-Za-z_][A-Za-z0-9_]*)")
SETS = re.compile(r"(?<![A-Za-z0-9_])([A-Za-z_][A-Za-z0-9_]*)(?:\[[^]]*\])?\+?=")

def refuse_touch(word, where, sets=True):
    # Refuses a word that reads a reserved name with $ or, when sets is true, holds an
    # assignment to one, NAME= anywhere in it; where says where the word stands. A word is
    # read without its quotes, so a reserved name a quoted pattern holds is refused too.
    for name in READS.findall(word):
        if reserved(name) and name != EXPECT_STATUS:
            raise Refused(word + ", " + where + " that reads " + name + ", a name the gate records with")
    for name in (SETS.findall(word) if sets else ()):
        if reserved(name):
            raise Refused(word + ", " + where + " that assigns " + name + ", a name the gate records with")

# ADMITTED is every form the classifier admits, one entry each, from the form's key to what
# it is, and nothing else admits a form: every admission goes through admit below, which
# refuses a key ADMITTED does not hold, whatever the parser read. --admitted reads this same
# table, counts the rows of claims.jsonl that use each entry, and fails an entry no row
# uses, so the table is exactly the forms the rows use. check.sh's header above
# run_recorded holds the table with those counts. One entry holds two spellings: a
# parameter, written $NAME or ${NAME}, which the shell expands alike. The entries of the
# assigned names, the programs, the program variables, xargs's options and uv run's
# options and programs are built from ASSIGNED_NAMES, PROGRAMS, PROGRAM_VARIABLES,
# XARGS_OPTIONS, UV_RUN_OPTIONS and UV_RUN_PROGRAMS, one each.
ADMITTED = {
    # Operators and separators.
    "&&": "runs the next pipeline when this one exits 0",
    "||": "runs the next pipeline when this one fails",
    "|": "joins the commands of a pipeline",
    ";": "ends a pipeline",
    "newline": "an unquoted newline, ending a pipeline as ; does",
    ">": "writes the output to a file",
    "2>": "writes the error output to a file",
    "2>&1": "sends the error output where the output goes",
    # Keywords and control words.
    "{": "opens a group",
    "}": "closes a group",
    "(": "opens a subshell",
    ")": "closes a subshell",
    "for": "opens a for loop",
    "in": "comes before a for loop's words",
    "do": "comes before a for loop's body",
    "done": "closes a for loop",
    # Shapes.
    "simple command": "a command word and its arguments",
    "pipeline": "two or more commands joined by |",
    "list": "two or more pipelines joined by ;, &&, || or newline",
    "group": "{ list; }, whose status is its last command's",
    "subshell": "( list ), whose status is its last command's",
    "for loop": "for NAME in words; do list; done",
    "assignment before a command": "NAME=value, set in that command's environment",
    "assignment alone": "NAME=value with no command word",
    "command substitution in a word": "$( ) in a word, its code admitted by this table",
    # Assigned names.
    **{"assigns " + name: {EXPECT_STATUS: "the declared status, the last prefix of one command",
                           HASH_EXEMPT: "0, before a uv pip install of local sources alone"}.get(name, "a name the rows assign")
       for name in ASSIGNED_NAMES},
    # Command words.
    **{"program " + name: "wrapped in a function that records its status" for name in PROGRAMS},
    **{"variable $" + name: "a program variable, whose shim records its status" for name in PROGRAM_VARIABLES},
    "xargs running a program": "xargs's status carries the program's",
    **{"xargs " + option: what for option, (_, what) in XARGS_OPTIONS.items()},
    **{"uv run " + option: what for option, (_, what) in UV_RUN_OPTIONS.items()},
    **{"uv run " + name: what for name, what in UV_RUN_PROGRAMS.items()},
    "sh running a script file": "the script's status reaches the row as sh's",
    "true": "a command whose status is no read",
    # $ forms.
    "$NAME or ${NAME}": "a parameter, its name in braces or not",
    "$?": "the status of the last command",
    "$( )": "a command substitution",
    "a literal $": "a $ that starts no expansion, left as it stands",
}

# The entries of ADMITTED the command being classified used, which admit records.
USED = set()

def admit(key, refusal=None):
    # The one way the classifier admits a form. A key ADMITTED does not hold, None
    # included, is refused, naming the form; any other is recorded as used.
    if key not in ADMITTED:
        raise Refused(refusal or str(key) + ", a form the evaluator does not admit")
    USED.add(key)

class ForeignCharacter(Refused):
    # The refusal of a command holding a character foreign_character names, which carries
    # that character as U+XXXX for each reader to name.
    def __init__(self, char):
        super().__init__("the character " + char + ", which bash reads as part of a word where the evaluator would split it")
        self.char = char

def tokenize(text):
    # Words and operators, quotes respected; an unquoted newline is an operator of its own,
    # which the parser reads as a separator like ;. Words are split at space and tab, as
    # bash splits them; a command holding a character foreign_character names is refused
    # here, before it is split, so no reader splits it.
    char = foreign_character(text)
    if char:
        raise ForeignCharacter(char)
    lexer = shlex.shlex(text, posix=True, punctuation_chars=";&|()<>\n")
    lexer.whitespace = " \t"
    lexer.whitespace_split = True
    lexer.commenters = ""
    try:
        raw = list(lexer)
    except ValueError:
        return None
    out = []
    for token in raw:
        if token and set(token) <= set(";&|()<>\n"):
            i = 0
            while i < len(token):
                for op in OPS:
                    if token.startswith(op, i):
                        out.append(("op", op))
                        i += len(op)
                        break
        else:
            out.append(("word", token))
    return out

def commands(text):
    # The words of each command of text, split at every operator of the one tokenizer;
    # None when the text does not split. The install reader reads its runners from these.
    tokens = tokenize(text)
    if tokens is None:
        return None
    out, current, target = [], [], False
    for kind, value in tokens:
        if target:
            target = False
            if kind == "word":
                continue
        if kind == "op" and value in (">", ">>", "<", ">&", "&>", "<<", "<<<"):
            if current and current[-1].isdigit():
                current.pop()
            target = True
            continue
        if kind == "op":
            if current:
                out.append(current)
            current = []
        else:
            current.append(value)
    if current:
        out.append(current)
    return out

# A command substitution, $(...) or a pair of backticks, that no backslash escapes; an
# escaped backtick is a literal one, as a quoted document holds.
SUBSTITUTION = re.compile(r"(?<!\\)[$<>]\(((?:[^()]|\([^()]*\))*)\)|(?<!\\)`((?:[^`\\]|\\.)*)`")
# Each substitution, once its code is read, stands in the line as this marker, inside the
# word it was written in, so an assignment from one stays an assignment and a
# command word that is the output of a substitution is refused as unresolved.
SUBST_WORD = "IOCSUBSTITUTION"

def substitutions(text):
    # Each command substitution the shell runs in text, as (start, end, code): $(...) and a
    # pair of backticks outside single quotes, inside double quotes too, and <(...) and
    # >(...) outside any quotes. Text inside single quotes is literal, so a backtick in a
    # quoted grep pattern is no substitution. A backslash escapes the next character
    # outside single quotes.
    out, i, single, double = [], 0, False, False
    while i < len(text):
        c = text[i]
        if single:
            if c == "\x27":
                single = False
            i += 1
            continue
        if c == "\\":
            i += 2
            continue
        if c == "\x27" and not double:
            single = True
            i += 1
            continue
        if c == "\x22":
            double = not double
            i += 1
            continue
        if (c == "$" or (c in "<>" and not double)) and text[i + 1:i + 2] == "(":
            depth, j = 1, i + 2
            while j < len(text) and depth:
                if text[j] == "(":
                    depth += 1
                elif text[j] == ")":
                    depth -= 1
                j += 1
            out.append((i, j, text[i + 2:j - 1]))
            i = j
            continue
        if c == "`":
            j = i + 1
            while j < len(text) and text[j] != "`":
                j += 2 if text[j] == "\\" else 1
            out.append((i, j + 1, text[i + 1:j]))
            i = j + 1
            continue
        i += 1
    return out

NAME_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")
# The characters bash reads after a $ as a one-character special parameter. dollar_forms
# reads them to name each one; which of them is admitted is ADMITTED's to say, and it holds
# $? alone.
SPECIAL = "?$!#@*-0123456789"

def dollar_forms(text):
    # Each $ the shell expands in text, outside single quotes, as (key, spelling), the key
    # being the form's entry in ADMITTED: "$NAME or ${NAME}" for a parameter, "$" and its
    # character for a one-character special parameter, $? or $1 alike, "${" and that
    # character and "}" for the same in braces, "$( )" for a command substitution and
    # "a literal $" for a $ that starts no expansion, which the shell leaves as it is.
    # Every other form has the key None, which no entry is: $(( )), $[ ], $' ', $" " and
    # ${ } with anything inside but a name or one special character, ${NAME:-x}, ${#NAME}
    # and ${!NAME} among them. classify admits each key through admit, so a special
    # parameter ADMITTED does not list is refused like every form without an entry.
    out, i, single, double = [], 0, False, False
    while i < len(text):
        c = text[i]
        if single:
            if c == "\x27":
                single = False
            i += 1
            continue
        if c == "\\":
            i += 2
            continue
        if c == "\x27" and not double:
            single = True
            i += 1
            continue
        if c == "\x22":
            double = not double
            i += 1
            continue
        if c != "$":
            i += 1
            continue
        nxt = text[i + 1:i + 2]
        if text.startswith("$((", i):
            out.append((None, "$(( ))"))
            i += 3
        elif nxt == "(":
            out.append(("$( )", "$( )"))
            i += 2
        elif nxt == "{":
            j = text.find("}", i)
            if j < 0:
                out.append((None, text[i:]))
                break
            body = text[i + 2:j]
            if NAME_RE.fullmatch(body) and body != "_":
                out.append(("$NAME or ${NAME}", "${" + body + "}"))
            elif body == "_":
                # ${_} is bash's special parameter, the last argument of the last command.
                out.append(("${_}", "${_}"))
            elif len(body) == 1 and body in SPECIAL:
                out.append(("${" + body + "}", "${" + body + "}"))
            else:
                out.append((None, "${" + body + "}"))
            i = j + 1
        elif nxt and NAME_RE.match(nxt):
            end = NAME_RE.match(text, i + 1).end()
            # $_ alone is bash's special parameter, the last argument of the last command,
            # and no name: its key is its own, so no entry admits it.
            key = "$_" if text[i + 1:end] == "_" else "$NAME or ${NAME}"
            out.append((key, text[i:end]))
            i = end
        elif nxt and nxt in SPECIAL:
            out.append(("$" + nxt, "$" + nxt))
            i += 2
        elif nxt in ("[", "\x27", "\x22") and not (double and nxt == "\x22"):
            # $[ ] is arithmetic, and $' ' and $" " are quoting forms; none is admitted.
            out.append((None, "$" + nxt))
            i += 2
        else:
            # A $ before any other character starts no expansion, and the shell leaves it
            # as it stands, as in a sed pattern's "$/".
            out.append(("a literal $", "$"))
            i += 1
    return out


def without_substitutions(text):
    # The text with each substitution replaced by the marker, inside the word it stands in.
    pieces, last = [], 0
    for start, end, _ in substitutions(text):
        pieces.append(text[last:start])
        pieces.append(SUBST_WORD)
        last = end
    pieces.append(text[last:])
    return "".join(pieces)

# The words the install-pinned reader steps over to reach a command word, and the wrappers
# whose command it reads. They admit nothing: they let that reader find runners inside
# forms the evaluator refuses, and the evaluator's admissions are ADMITTED's alone.
KEYWORDS = {"do", "then", "else", "elif", "if", "while", "until", "!", "{", "}", "time", "done", "fi", "esac"}
CONTROL = {"for", "case", "select", "function", "in"}
WRAPPERS = {"env", "nohup", "nice", "timeout", "command", "exec", "sudo", "stdbuf", "xargs", "time"}
# The options of each wrapper but xargs that take a value; xargs's are XARGS_OPTIONS.
WRAPPER_VALUE = {"nice": {"-n"}, "timeout": {"-s", "-k", "--signal", "--kill-after"},
                 "sudo": {"-u", "-g", "-C"}, "stdbuf": {"-i", "-o", "-e"}, "env": {"-u", "-C", "-S"}}

# The install-pinned reader resolves a command word written as a variable through
# PROGRAM_VARIABLES alone, the programs env.sh builds and pins, each naming one
# executable: each resolves to the program PROGRAM_VARIABLES names for it, and the word
# is then held to every rule a literal word is held to. A variable with a path after it,
# and any other variable, may not stand in command position.
def resolve(word):
    # The program a command word names, or None when this reader cannot resolve it.
    if SUBST_WORD in word or "`" in word:
        return None
    if "$" not in word:
        return word.rsplit("/", 1)[-1]
    return PROGRAM_VARIABLES.get(variable_name(word))

def runner_name(word):
    # The program a word names, as every runner rule below matches it: the resolved
    # word, its last path element, with a version written after the name taken off, so
    # /usr/bin/pip, pip3 and pip3.12 are all pip. A word
    # this reader cannot resolve names no runner, and is refused in command position.
    base = resolve(word)
    if base is None:
        return ""
    return re.sub(r"[0-9][0-9.]*$", "", base)

def command_words(w):
    # The indexes of the words of w that stand in command position: the first word of the
    # command after its assignments and keywords, and the command each wrapper runs. An
    # option of xargs that XARGS_OPTIONS does not hold is refused, naming it.
    i, out = 0, []
    while i < len(w):
        word = w[i]
        if ASSIGNMENT.match(word) or word in KEYWORDS:
            i += 1
            continue
        if word in CONTROL:
            return out
        break
    while i < len(w):
        out.append(i)
        base = resolve(w[i])
        if base not in WRAPPERS:
            # find runs the command after -exec or -execdir as well.
            if base == "find":
                for j in range(i + 1, len(w) - 1):
                    if w[j] in ("-exec", "-execdir", "-ok", "-okdir"):
                        out.append(j + 1)
            return out
        values = WRAPPER_VALUE.get(base, set())
        i += 1
        if base == "xargs":
            i += xargs_program(w[i:])[1]
            continue
        if base == "timeout" and i < len(w) and not w[i].startswith("-"):
            i += 1
        while i < len(w) and (w[i].startswith("-") or (base == "env" and ASSIGNMENT.match(w[i]))):
            if w[i].split("=", 1)[0] in values and "=" not in w[i]:
                i += 1
            i += 1
            if base == "timeout" and i < len(w) and not w[i].startswith("-") and re.match(r"^[0-9.]+[smhd]?$", w[i]):
                i += 1
    return out

# The options of each runner that take a value, and those that take none; any other
# option before the package is a form this reader does not know.
UV_TOOL_VALUE = {"--python", "-p", "--from", "--with", "--with-editable", "--with-requirements",
                 "--index", "--index-url", "--extra-index-url", "--default-index", "--cache-dir",
                 "--python-preference", "--constraints", "-c", "--overrides"}
UV_TOOL_FLAG = {"--isolated", "--offline", "--no-cache", "-q", "--quiet", "-v", "--verbose",
                "--refresh", "--no-project", "--no-config", "-n"}
PIPX_VALUE = {"--python", "--spec", "--index-url", "--pip-args"}
PIPX_FLAG = {"--no-cache", "-q", "--quiet", "-v", "--verbose"}
NODE_VALUE = {"--package", "-p", "--registry", "--cache"}
NODE_FLAG = {"-y", "--yes", "--no", "-q", "--quiet", "--silent", "--bun"}
GO_VALUE = {"-C", "-o", "-tags", "-ldflags", "-gcflags", "-mod", "-overlay", "-modfile", "-exec"}
GO_FLAG = {"-v", "-x", "-race", "-a", "-n", "-trimpath", "-buildvcs=false"}

def option_value(word):
    return word.split("=", 1)[1] if "=" in word else None

def read_runner(name, rest, value_opts, flag_opts, ok, pinned_by=(), lock_by=()):
    # lock_by names the options whose value may be a hashed lock: each package this runner
    # resolves from an index is held at its exact version by a lock the command names, or
    # it is refused; None exempts a runner, go alone, whose modules go itself checks
    # against go.sum or the checksum database. uv tool run, uvx and pipx check no hash of
    # what they install or build (measured, see the header), so a local source path under
    # a Python runner, which it would build, is refused, and a wheel, which builds
    # nothing, passes.
    problems, pins, i = [], {}, 0
    while i < len(rest):
        word = rest[i]
        if not word.startswith("-"):
            break
        key = word.split("=", 1)[0]
        if key in value_opts:
            value = option_value(word)
            if value is None:
                value = rest[i + 1] if i + 1 < len(rest) else ""
                i += 1
            pins.setdefault(key, []).append(value)
        elif word not in flag_opts and key not in flag_opts:
            return [name + " " + word + " (an option this reader does not know)"]
        i += 1
    python = ok is py_ok
    locks = [] if lock_by is None else [p for k in lock_by for v in pins.get(k, []) for p in [lock_packages(v)] if p is not None]

    def read(label, value):
        if not ok(value):
            problems.append(label + " " + value)
        elif local(value):
            if python and not wheel(value):
                problems.append(label + " " + value + " (a local source path " + name + " would build, checking no hash of its build environment)")
        elif lock_by is not None and not locks:
            problems.append(label + " " + value + NO_LOCK)
        elif lock_by is not None and not held(value, locks):
            problems.append(label + " " + value + NOT_HELD)

    for key in ("--with", "--with-editable"):
        for value in pins.get(key, []):
            read(name + " " + key, value)
    for value in pins.get("--with-requirements", []):
        if not hashed_lock(value):
            problems.append(name + " --with-requirements " + value + NO_LOCK)
    package = rest[i] if i < len(rest) else ""
    source = None
    for key in pinned_by:
        if pins.get(key):
            source = pins[key][-1]
    target = source if source is not None else package
    if not target:
        problems.append(name + " (no package this reader can find)")
    elif target.startswith("$") and not local(target):
        problems.append(name + " " + target + " (a package this reader cannot read)")
    else:
        read(name, target)
    return problems

def read_command(command, depth=0):
    found = []
    if depth > 4:
        return ["a command nested too deep for this reader to open"]
    # No recorded command holds a heredoc, and its body is text this reader does not
    # split into commands, so a heredoc is refused rather than read as words.
    if re.search(r"<<-?\s*[\x27\x22]?[A-Za-z_]", command):
        return ["a heredoc, whose body this reader does not read"]
    # A command substitution runs whether or not it stands inside double quotes, and a
    # backtick pair may span two words, so it is read in the text before the split.
    for _, _, code in substitutions(command):
        found += read_command(code, depth + 1)
    command = without_substitutions(command)
    try:
        parts = commands(command)
    except ForeignCharacter as why:
        # A character bash keeps inside a word, where the tokenizer would split, makes the
        # words this reader reads differ from those bash runs, so the command is refused.
        return ["a command holding the character " + why.char + ", which bash reads as part of a word"]
    if parts is None:
        return found + ["a command this reader cannot split into words"]
    for w in parts:
        part = " ".join(w)
        try:
            positions = command_words(w)
        except Refused as why:
            found.append(str(why))
            positions = []
        for i in positions:
            if resolve(w[i]) is None:
                what = "the output of a substitution" if SUBST_WORD in w[i] else w[i]
                found.append(what + " (a command word this reader cannot resolve)")
        for i, word in enumerate(w):
            nxt = w[i + 1] if i + 1 < len(w) else ""
            after = w[i + 1:]
            base = resolve(word) or word.rsplit("/", 1)[-1]
            name = runner_name(word)
            if name in SHELLS:
                k = shell_string(after)
                if k is not None:
                    if i + k + 2 < len(w):
                        found += read_command(w[i + k + 2], depth + 1)
                    else:
                        found.append(base + " -c (no command this reader can find)")
            elif word == "eval":
                found += read_command(" ".join(after), depth + 1)
            elif name == "python" and nxt == "-c" and i + 2 < len(w):
                if PROCESS.search(w[i + 2]):
                    found.append(base + " -c (python code that starts a process, which this reader cannot open)")
            elif name == "uvx":
                found += read_runner("uvx", after, UV_TOOL_VALUE, UV_TOOL_FLAG, py_ok, ("--from",), ("-c", "--constraints"))
            elif name == "uv" and nxt == "tool" and i + 2 < len(w) and w[i + 2] in ("run", "install"):
                found += read_runner("uv tool " + w[i + 2], w[i + 3:], UV_TOOL_VALUE, UV_TOOL_FLAG, py_ok, ("--from",), ("-c", "--constraints"))
            elif name == "uv" and nxt == "tool":
                found.append("uv tool " + (w[i + 2] if i + 2 < len(w) else "") + " (a uv tool form this reader does not read)")
            elif name == "uv" and nxt == "run":
                # uv run fetches each --with, which every one of them must pin. Its options
                # are read from UV_RUN_OPTIONS, the classifier's own table, so --python's
                # value is no program, and an option the table does not hold is refused.
                # uv run reads neither UV_REQUIRE_HASHES nor UV_BUILD_CONSTRAINT (measured,
                # see the header), and takes no -c, so a --with that is a local source path,
                # which it would build with whatever build backend is newest, is refused, a
                # built wheel passes, and a package from an index passes only when
                # evidence/ioc-venv.lock holds it at that version and it requires no other
                # package, which uv run would resolve to the newest release: the set it
                # installs is then the package alone, held by the lock.
                try:
                    options, _ = uv_run_program(w[i + 2:])
                except Refused as why:
                    found.append(str(why))
                    options = []
                lock = [p for p in [lock_packages("$IOC_EVID/ioc-venv.lock")] if p is not None]
                for key, value in options:
                    if key != "--with":
                        continue
                    if not py_ok(value):
                        found.append("uv run " + key + " " + value)
                    elif local(value) and not wheel(value):
                        found.append("uv run " + key + " " + value + " (a local source path uv run builds with no build constraint; run a wheel built under one)")
                    elif not local(value) and not held(value, lock):
                        found.append("uv run " + key + " " + value + " (a package evidence/ioc-venv.lock does not hold at that version)")
                    elif not local(value) and requires(value, "$IOC_EVID/ioc-venv.lock"):
                        found.append("uv run " + key + " " + value + " (a package that requires others, which uv run, reading no lock, resolves to the newest release; run it with uv tool run -c of the lock)")
            elif name == "uv" and nxt == "build":
                # uv build checks every hash of its build environment under UV_REQUIRE_HASHES
                # and UV_BUILD_CONSTRAINT (measured); the reader holds its -b to the hashed
                # build lock, which it must name, and refuses an option uv would check no
                # hash under.
                rest, j, sources, builds = w[i + 2:], 0, [], []
                while j < len(rest):
                    a = rest[j]
                    key, value = a.split("=", 1)[0], option_value(a)
                    if value is None and key in (*BUILD_KEYS, *CONSTRAINT_KEYS, "-o", "--out-dir", "--python", "-p", "--package"):
                        value = rest[j + 1] if j + 1 < len(rest) else ""
                        j += 1
                    if key in BUILD_KEYS:
                        builds.append(value)
                    elif key in UNENFORCED:
                        found.append("uv build " + key + " (an option under which uv checks no hash)")
                    elif not a.startswith("-"):
                        sources.append(a)
                    j += 1
                found += read_build("uv build", sources or ["."], builds)
            elif name == "uv" and nxt == "add":
                found.append("uv add (a form that installs and that this reader does not read)")
            elif name == "pipx" and nxt == "run":
                found += read_runner("pipx run", w[i + 2:], PIPX_VALUE, PIPX_FLAG, py_ok, ("--spec",))
            elif name == "pipx":
                found.append("pipx " + nxt + " (a pipx form this reader does not read)")
            elif name in ("bunx", "npx") or (name == "npm" and nxt == "exec"):
                label, rest = (name, after) if name != "npm" else ("npm exec", w[i + 2:])
                found += read_runner(label, rest, NODE_VALUE, NODE_FLAG, npm_ok, ("--package", "-p"))
            elif name in ("npm", "bun", "yarn", "pnpm") and nxt in ("install", "i", "add", "dlx", "x"):
                found.append(name + " " + nxt + " (a form that installs and that this reader does not read)")
            elif name == "go" and nxt in ("run", "install"):
                found += read_runner("go " + nxt, w[i + 2:], GO_VALUE, GO_FLAG, go_ok, (), None)
            elif name == "pip" and nxt in ("install", "sync"):
                # Every argument is read, in every spelling of -e and --editable, by
                # pip_words. A sync installs exactly the files it names, so each must be a
                # hashed lock. Every package an install names from an index is held at its
                # exact version by a hashed lock the command names, with -c or -r, or it
                # is refused by name, whatever other file the command names; a
                # requirements file that is no hashed lock is refused. A local path that is
                # no wheel is built, and its build backend is fetched like any package, so
                # the command names the hashed build lock with -b, and any other -b is
                # refused. An option under which uv would check no hash is refused.
                label = "pip " + nxt
                packages, locks, unlocked, builds, flags = pip_words(w[i + 2:])
                sources = []
                for a in flags:
                    found.append(label + " " + a + " (an option under which uv checks no hash)")
                for a in unlocked:
                    found.append(label + " -r " + a + NO_LOCK)
                for a in packages:
                    if nxt == "sync":
                        if not hashed_lock(a):
                            found.append(label + " " + a + NO_LOCK)
                    elif not py_ok(a):
                        found.append(label + " " + a)
                    elif local(a):
                        if not wheel(a):
                            sources.append(a)
                    elif not locks:
                        found.append(label + " " + a + NO_LOCK)
                    elif not held(a, locks):
                        found.append(label + " " + a + NOT_HELD)
                found += read_build(label, sources, builds)
                environment = install_environment(w, i)
                if environment is not None:
                    TARGETS.add(environment)
    return found


# The redirection operators of the tokenizer. Each output redirection is admitted by its
# spelling, its file descriptor and, for >&, the descriptor it copies; an input redirection
# and a here-document are refused by name.
REDIRECTS = (">", ">>", "<", ">&", "&>", "<<", "<<<")

class Parser:
    # A recursive descent over the shapes ADMITTED names: a list of pipelines joined by ;,
    # &&, || and newlines; a pipeline of elements joined by |; an element is a simple
    # command, a group { list; }, a subshell ( list ) or a for loop. Every operator,
    # keyword and shape it reads is admitted through admit, so a form ADMITTED does not
    # hold is refused by name whatever this parser reads it as.
    def __init__(self, tokens):
        self.t, self.i = tokens, 0
        self.commands = []  # every simple command: ("simple", words)

    def peek(self):
        return self.t[self.i] if self.i < len(self.t) else ("end", "")

    def take(self):
        tok = self.peek()
        self.i += 1
        return tok

    def at_word(self, *words):
        kind, value = self.peek()
        return kind == "word" and value in words

    def at_end(self, closers):
        kind, value = self.peek()
        return kind == "end" or (kind in ("word", "op") and value in closers)

    def separator(self):
        # Takes one ; or unquoted newline, admitting it, and says whether it took one.
        kind, value = self.peek()
        if kind == "op" and value in (";", "\n"):
            self.take()
            admit("newline" if value == "\n" else ";",
                  ("an unquoted newline" if value == "\n" else "the separator ;") + ", a form the evaluator does not admit")
            return True
        return False

    def parse_list(self, closers=()):
        pipelines = []
        while True:
            while self.separator():
                pass
            if self.at_end(closers):
                break
            pipelines.append(self.parse_pipeline(closers))
            kind, value = self.peek()
            if kind == "op" and value in ("&&", "||"):
                self.take()
                admit(value, "the operator " + value + ", a form the evaluator does not admit")
                continue
            if self.separator():
                continue
            if kind == "op" and value in ("&", ";;", "|&"):
                raise Refused("the operator " + value)
            if self.at_end(closers):
                break
            raise Refused("the word " + value + " after a command")
        if len(pipelines) > 1:
            admit("list")
        return pipelines

    def parse_pipeline(self, closers):
        elements = [self.parse_element(closers)]
        while self.peek() == ("op", "|"):
            self.take()
            admit("|", "the operator |, a form the evaluator does not admit")
            elements.append(self.parse_element(closers))
        if len(elements) > 1:
            admit("pipeline")
        return elements

    def parse_element(self, closers):
        kind, value = self.peek()
        if kind == "word" and value == "{":
            self.take()
            admit("{")
            body = self.parse_list(closers=("}",))
            if not self.at_word("}"):
                raise Refused("a group with no closing brace")
            self.take()
            admit("}")
            admit("group")
            self.trailing_redirections()
            return ("group", body)
        if kind == "op" and value == "(":
            self.take()
            if self.peek() == ("op", "("):
                raise Refused("an arithmetic command (( )), a form the evaluator does not admit")
            admit("(")
            body = self.parse_list(closers=(")",))
            if self.peek() != ("op", ")"):
                raise Refused("a subshell with no closing parenthesis")
            self.take()
            admit(")")
            admit("subshell")
            self.trailing_redirections()
            return ("group", body)
        if kind == "word" and value == "for":
            self.take()
            admit("for")
            kind, var = self.take()
            if kind != "word" or not re.match(r"^[A-Za-z_][A-Za-z0-9_]*$", var):
                raise Refused("a for loop with no variable")
            if reserved(var):
                raise Refused("for " + var + ", a loop that assigns " + var + ", a name the gate records with")
            admit("assigns " + var, "for " + var + ", a loop that assigns " + var + ", a name the evaluator does not admit")
            if self.at_word("in"):
                self.take()
                admit("in")
                while self.peek()[0] == "word":
                    refuse_touch(self.take()[1], "a word of a for loop")
            else:
                # With no in, the loop runs over the positional parameters, as $@ does.
                admit(None, "a for loop with no in, which loops over the positional parameters")
            while self.separator():
                pass
            if not self.at_word("do"):
                raise Refused("a for loop with no do")
            self.take()
            admit("do")
            body = self.parse_list(closers=("done",))
            if not self.at_word("done"):
                raise Refused("a for loop with no done")
            self.take()
            admit("done")
            admit("for loop")
            self.trailing_redirections()
            return ("group", body)
        return self.parse_simple()

    def redirection(self, fd):
        # One redirection, its operator next, written after the file descriptor fd, or
        # after none when fd is "".
        _, op = self.take()
        if op in ("<<", "<<<"):
            raise Refused("a here-document or here-string")
        if op == "<":
            raise Refused("an input redirection, which fails before the program it feeds runs")
        kind, target = self.peek()
        if kind != "word":
            raise Refused("a redirection with no target")
        self.take()
        if op == ">&":
            # >& copies a descriptor, or with a file name after it writes both outputs.
            spelling = fd + op + (target if re.fullmatch(r"[0-9]+|-", target) else " a file")
        else:
            spelling = fd + op
        admit(spelling, "the redirection " + spelling + ", a form the evaluator does not admit")
        refuse_touch(target, "a redirection target", sets=False)

    def trailing_redirections(self):
        # The redirections after a group, a subshell or a loop, written with no descriptor.
        while self.peek()[0] == "op" and self.peek()[1] in REDIRECTS:
            self.redirection("")

    def parse_simple(self):
        words = []
        while True:
            kind, value = self.peek()
            if kind == "word":
                self.take()
                nxt = self.peek()
                if nxt[0] == "op" and nxt[1] in (">", ">>", "<", ">&") and value.isdigit():
                    self.redirection(value)  # a file descriptor before a redirection
                    continue
                words.append(value)
                continue
            if kind == "op" and value in REDIRECTS:
                self.redirection("")
                continue
            break
        if not words:
            raise Refused("an empty command")
        admit("simple command")
        command = ("simple", words)
        self.commands.append(command)
        return command

# Why a command word ADMITTED holds no entry for is refused, for the words where the reason
# helps a reader. These only word the refusal: the rule is ADMITTED, which refuses every
# program name it holds no entry for, each one below and every name no row runs alike.
REFUSAL_REASONS = {
    **dict.fromkeys(("eval", "source", "."), "which runs code the evaluator does not read"),
    **dict.fromkeys(("exec", "command", "builtin"), "which runs a program past the function that records it"),
    **dict.fromkeys(("env", "timeout", "nice", "nohup", "sudo", "stdbuf"), "a wrapper, which runs a program past the function that records it"),
    **dict.fromkeys(("bash", "zsh", "dash", "ksh"), "a shell other than sh, a program the evaluator does not admit"),
    "find": "whose -exec runs a program past the function that records it",
    "gh": "which the gate never reads GitHub with: a repository's files are read with git, and its REST API with curl",
    **dict.fromkeys(("declare", "local", "typeset", "readonly"), "which a wrapping function would make local to it"),
    **dict.fromkeys(("export", "unset", "alias", "trap", "set", "enable"), "which changes the shell the runner records in"),
    **dict.fromkeys(("exit", "return"), "which ends the shell before its row is read"),
    **dict.fromkeys(("if", "then", "elif", "else", "fi", "while", "until", "case", "esac", "select", "!", "[[",
                     "do", "done", "in", "time", "function", "coproc"), "a keyword the evaluator does not admit"),
}

def classify_word(words):
    # The class of a simple command's command word: ("program", name), ("variable",
    # name), ("condition", word), or ("none",) for a command of assignments alone, each
    # admitted through admit, with the index of the command word. Every other word is
    # refused. An assignment is admitted only to a name ASSIGNED_NAMES holds. An assignment
    # to a name the gate records with is refused, with two exceptions: the declared status
    # written as the last prefix of a command word, and UV_REQUIRE_HASHES=0 written before
    # the one install hash_exemption reads.
    i = 0
    while i < len(words) and ASSIGNMENT.match(words[i]):
        i += 1
    for k, word in enumerate(words[:i]):
        m = ASSIGNMENT.match(word)
        if m.group(2) or m.group(3):
            admit(None, word + ", an assignment to an array element or by +=, a form the evaluator does not admit")
        name = m.group(1)
        if name == EXPECT_STATUS and i == len(words):
            raise Refused(EXPECT_STATUS + " written as a statement of its own, which would exempt every later program")
        if name == EXPECT_STATUS and k < i - 1:
            # bash expands prefixes from left to right, so a substitution in a later one
            # would run with the status already declared.
            raise Refused(word + ", " + EXPECT_STATUS + " before another prefix, whose substitution would run with the status declared")
        if name == HASH_EXEMPT:
            why = "it is no prefix" if i == len(words) else (
                "its value is not 0" if word != HASH_EXEMPT + "=0" else hash_exemption(words[i:], words[:i]))
            if why:
                raise Refused(word + ", which clears uv's hash check, admitted only before the one install the header above reader_py states: " + why)
        elif reserved(name) and name != EXPECT_STATUS:
            raise Refused(word + ", an assignment to " + name + ", a name the gate records with")
        admit("assigns " + name, word + ", an assignment to " + name + ", a name the evaluator does not admit")
        refuse_touch(word, "an assignment", sets=False)
    if i == len(words):
        admit("assignment alone")
        return ("none",), i
    if i:
        admit("assignment before a command")
    word = words[i]
    if SUBST_WORD in word or "`" in word:
        raise Refused("the output of a substitution as a command word")
    if word.startswith("$"):
        name = variable_name(word)
        # The key carries the prefix "variable ", which no other kind of key starts with,
        # so no variable's name can spell another entry of ADMITTED.
        admit("variable $" + name if name else None, word + ", a variable that names no program the evaluator records")
        return ("variable", name), i
    if "/" in word:
        raise Refused(word + ", a program named by a path, which no function can be named for")
    if word == "true":
        # A condition, whose status is a test's answer rather than a read.
        admit("true")
        return ("condition", word), i
    # The key carries the prefix "program ", as a variable's carries "variable ".
    admit("program " + word, word + ", " + REFUSAL_REASONS.get(word, "a program the evaluator does not admit"))
    return ("program", word), i


SCRIPT_PATH = re.compile(r"(\$IOC_EVID/|\./)?[A-Za-z0-9_][A-Za-z0-9_./-]*\.sh")

def check_program(name, args):
    # Refuses the forms of a program and its arguments whose program the evaluator would
    # not see, by the same rules whether the program is a command word, the program xargs
    # runs or the program uv run runs.
    if name in SHELLS:
        if shell_string(args) is not None:
            raise Refused(name + " -c, a command given to a shell as a string")
        # A shell with no script file reads its commands from its input, which no function
        # records, as -c does, and so does one whose script is a stream under /dev/ or
        # /proc/, such as /dev/stdin; one given an option is a form of its own.
        if not args:
            admit(None, name + " reading its commands from its input, which no function records")
        elif args[0].startswith("-"):
            admit(None, name + " " + args[0] + ", a shell given an option, a form the evaluator does not admit")
        elif re.search(r"(^|/)(dev|proc)(/|$)", args[0]):
            raise Refused(name + " " + args[0] + ", a script under /dev/ or /proc/, which reads its commands from a stream no function records")
        elif not SCRIPT_PATH.fullmatch(args[0]) or ".." in args[0].split("/"):
            # A script path is a file of this tree: a literal path, or one under $IOC_EVID,
            # the committed evidence directory, ending in .sh. Any other variable could
            # name a stream, so a path it builds is refused rather than read as written.
            raise Refused(name + " " + args[0] + ", a script path that is no literal file and not under $IOC_EVID")
        else:
            admit(name + " running a script file")
    elif name == "printf":
        # printf -v assigns a variable, and a format that is not a literal word can expand
        # to -v, so the format is a literal word, read as written, that starts with no -.
        if args and args[0].startswith("-v"):
            raise Refused("printf " + args[0] + ", which assigns a variable rather than printing")
        if not args:
            raise Refused("printf with no format, a form the evaluator does not admit")
        if args[0].startswith("-") or "$" in args[0] or SUBST_WORD in args[0] or "`" in args[0]:
            shown = "the output of a substitution" if SUBST_WORD in args[0] else args[0]
            raise Refused("printf " + shown + ", a format that is no literal word or starts with -, which could make printf assign a variable")
        for a in args:
            if NAME_RE.fullmatch(a) and reserved(a):
                raise Refused("printf " + a + ", an argument that is " + a + ", a name the gate records with")
    elif name == "xargs":
        # xargs exits non-zero when the program it runs does, so its own record reads the
        # program. That program and its arguments are held to every rule a command word
        # and its arguments are, and the program is named by a bare word ADMITTED holds.
        options, j = xargs_program(args)
        for option in options:
            admit("xargs " + option)
        if j == len(args):
            admit(None, "xargs with no program, which runs echo, a form the evaluator does not admit")
        if ASSIGNMENT.match(args[j]):
            raise Refused("xargs " + args[j] + ", an assignment xargs would run as a program's name")
        inner, _ = classify_word(args[j:])
        if inner[0] != "program":
            raise Refused("xargs " + args[j] + ", a program xargs runs that is no plain program")
        check_program(inner[1], args[j + 1:])
        admit("xargs running a program")
    elif name == "uv" and args and args[0].startswith("-"):
        # An option before the subcommand could stand before run, which runs a program.
        raise Refused("uv " + args[0] + ", an option of uv before its subcommand the evaluator does not admit")
    elif name == "uv" and args and args[0] == "run":
        # uv run exits with the status of the program it runs. Its options are read by
        # uv_run_program from UV_RUN_OPTIONS and any other is refused by name, since which
        # word is the program rests on them; the program is held with its arguments to
        # every rule above, and is one UV_RUN_PROGRAMS holds.
        rest = args[1:]
        options, j = uv_run_program(rest)
        for option, _ in options:
            admit("uv run " + option)
        if j >= len(rest):
            raise Refused("uv run with no program, a form the evaluator does not admit")
        program = rest[j]
        check_program(program, rest[j + 1:])
        admit("uv run " + program if program in UV_RUN_PROGRAMS else None,
              "uv run " + program + ", a program uv run runs that the evaluator does not admit")


def check_simple(words):
    # The programs a simple command runs, each by its class; refuses the forms whose
    # program the evaluator would not see, and an argument that touches a name the gate
    # records with.
    cls, i = classify_word(words)
    for word in words[i + 1:]:
        refuse_touch(word, "an argument")
    if cls[0] == "program":
        check_program(cls[1], words[i + 1:])
    return [cls]


def final_stage(element):
    # The simple command whose status an element exits with.
    if element[0] == "simple":
        return element
    body = element[1]
    if not body:
        return ("simple", ["true"])
    return final_stage(body[-1][-1])


def classify(command, depth=0):
    # ("admitted", programs, stages) or ("refused", reason) for one command. Each form it
    # admits is admitted through admit, which records it in USED.
    if depth > 3:
        return ("refused", "a command nested too deep")
    try:
        # Every $ form of the command is read first, quotes respected, and admitted only
        # when ADMITTED holds its key.
        for key, spelling in dollar_forms(command):
            admit(key, "the expansion " + spelling + ", a $ form the evaluator does not admit")
        inner_programs = []
        for start, _, code in substitutions(command):
            # Process substitution runs its code beside the row, so a failure in it lands
            # after the row is read; an arithmetic expansion is no command. No row uses either.
            if command[start] in "<>":
                raise Refused("a process substitution " + command[start] + "( ), whose code runs beside the row and ends after it is read")
            if command.startswith("$((", start):
                raise Refused("an arithmetic expansion $(( )), a form the evaluator does not admit")
            if command[start] == "$":
                admit("command substitution in a word")
            else:
                admit(None, "a substitution written with backticks, a form the evaluator does not admit")
            inner = classify(code, depth + 1)
            if inner[0] == "refused":
                return inner
            # A substitution's programs run like any other, so each is wrapped and recorded.
            inner_programs += inner[1]
        tokens = tokenize(without_substitutions(command))
        if tokens is None:
            return ("refused", "a command the evaluator cannot split into words")
        parser = Parser(tokens)
        pipelines = parser.parse_list()
        if parser.peek()[0] != "end":
            raise Refused("the word " + parser.peek()[1] + " the evaluator does not admit there")
        programs = list(inner_programs)
        for _, words in parser.commands:
            programs += check_simple(words)
    except Refused as why:
        return ("refused", str(why))
    stages = []
    if pipelines:
        for element in pipelines[-1]:
            _, words = final_stage(element)
            stages.append(" ".join(words))
    return ("admitted", programs, stages)


# Each mode that reads rows reads the rows the gate's one parse of claims.jsonl read, one
# JSON object per line, each id a string and each command a string, so no line here is one
# it cannot read.
if sys.argv[-1] == "load":
    # Reads nothing: the gate starts the reader once this way before it reads any row, so
    # that a reader that cannot load is told from one that stopped on a row.
    pass
elif sys.argv[-1] == "environments":
    # Reads the rows of claims.jsonl on stdin and prints <id><TAB><label> for each
    # environment of ENVIRONMENTS a row's command installs into, which the gate checks
    # after the row.
    for line in sys.stdin:
        row = json.loads(line)
        TARGETS.clear()
        read_command(row.get("command") or "")
        for label in sorted(TARGETS):
            print(row.get("id", "?") + "\t" + label)
elif sys.argv[-1] == "stages":
    result = classify(sys.stdin.read())
    if result[0] == "refused":
        print("UNSPLIT")
    else:
        for stage in result[2]:
            print(stage)
elif sys.argv[-1] == "names":
    result = classify(sys.stdin.read())
    if result[0] == "refused":
        print("UNREAD\t" + result[1])
    else:
        for kind in sorted({k for k in result[1] if k[0] == "program"}):
            print(kind[1])
elif sys.argv[-1] == "admitted":
    # Reads the rows of claims.jsonl on stdin and classifies the command of each row that
    # carries one. Prints REFUSED<TAB><id><TAB><why> for each row it refuses, then one line
    # per entry of ADMITTED, in its order, FORM<TAB><key><TAB><rows that use it><TAB><what
    # it is>, then one line per name no row may touch, RESERVED<TAB><name><TAB><what it is>.
    counts = {key: 0 for key in ADMITTED}
    for line in sys.stdin:
        row = json.loads(line)
        if not row.get("command"):
            continue
        USED.clear()
        result = classify(row["command"])
        if result[0] == "refused":
            print("REFUSED\t" + row.get("id", "?") + "\t" + result[1])
            continue
        for key in USED:
            counts[key] += 1
    for key, what in ADMITTED.items():
        print("FORM\t%s\t%d\t%s" % (key, counts[key], what))
    for name, what in RESERVED.items():
        print("RESERVED\t%s\t%s" % (name, what))
else:
    for line in sys.stdin:
        row = json.loads(line)
        for what in read_command(row.get("command") or ""):
            print(row.get("id", "?") + "\t" + what)
PYREADER
}

unpinned_installs() {
    # The install reader reader_load loaded, which reads each lock a command names from this
    # tree's evidence/, LOCKS_DIR, which reader_py writes into it, over the rows of the one
    # parse.
    claims | python3 "$READER" installs
}

# reader_load; builds the reader of command text from env.sh once per run, into READER
# under this run's scratch, and starts it once with nothing to read. A reader that cannot
# load, because env.sh did not load or python3 cannot start it, is a mode that cannot run.
# Every reader of a command runs this one file once it has loaded: the classifier of
# --admitted, the environments and the installs of --replay (unpinned_installs), and the
# names and the stages each row is run and read by (run_recorded, failed_read). It reads
# only rows the one parse read, so a reader that then stops on one is a failure of the
# gate, which its caller fails by name.
READER="$RUN_SCRATCH/reader.py"
reader_load() {
    [ -s "$READER" ] && return 0
    env_functions || cannot_run gate " --$MODE: env.sh, which holds the variables and the lock reader the install reader is built from, did not load."
    reader_py >"$READER.part" || cannot_run gate " --$MODE: the install reader could not be built from env.sh."
    python3 "$READER.part" load </dev/null >/dev/null 2>"$READER.err" ||
        cannot_run gate " --$MODE: the install reader cannot load: $(tail -1 "$READER.err" | head -c 240)"
    mv "$READER.part" "$READER"
}

# environment_stamps; one line per environment of env.sh's IOC_ENV_FROZEN that exists, its
# label and the time its site-packages last changed, which an install changes. Read before
# and after each row, it finds an install the reader cannot see, such as one inside a
# script the row runs.
environment_stamps() {
    local label
    for label in $(ioc_env_frozen_labels); do
        ioc_env_frozen_entry "$label" && [ -n "$IOC_ENV_ENTRY_DIR" ] || continue
        set -- "$IOC_ENV_ENTRY_DIR"/lib/python*/site-packages
        [ -d "$1" ] && printf '%s %s\n' "$label" "$(stat -c %y "$1")"
    done
}

# check_environments <id> <label>...; checks what each environment holds after the row
# <id> installed into it, by env.sh's ioc_env_frozen, and fails install-pinned for each
# refusal it prints, naming the row, the environment, the package, its version and the
# lock. ioc_env_frozen prints a refusal whenever it ends non-zero. It returns non-zero
# when one fails.
check_environments() {
    local id="$1" label out kind text status=0
    shift
    for label in "$@"; do
        out="$(ioc_env_frozen "$label" "$EVIDENCE/evidence")" && continue
        status=1
        while IFS=$'\t' read -r kind text; do
            [ "$kind" = REFUSED ] || continue
            fail install-pinned "$id installed into $label, and what $label holds after it is refused: $text"
        done <<<"$out"
    done
    return "$status"
}

# ---------------------------------------------------------------- --admitted

# admitted_line <form> <rows> <what it is>; one line of the table of admitted forms, as the
# header above run_recorded holds it after its "#   ".
admitted_line() { printf '%-31s %4s  %s' "$1" "$2" "$3"; }
# reserved_line <name> <what it is>; one line of the header's list of reserved names.
reserved_line() { printf '%-37s %s' "$1" "$2"; }

# mode_admitted; the audit of ADMITTED. It runs the classifier over the command of every
# row of claims.jsonl, through the one table the classifier admits by, and prints one line
# per entry with the number of rows that use it. It fails an entry no row uses, a row the
# classifier refuses, and a line of the header's table that differs from the one measured.
# It fails as well a header list of reserved names that differs from the reader's, which
# the reader builds from PROGRAM_VARIABLES, RUNNER_PREFIXES and env.sh's IOC_ENV_ENFORCED,
# and a name the runner or the shims record with that starts with none of RUNNER_PREFIXES,
# which no row would be kept from touching. The one parse comes first, and a line it reads
# no row of fails admitted-rows; a reader that cannot load is a mode that cannot run, and
# one that loaded and then stopped fails admitted-rows by name (the header).
mode_admitted() {
    local kind a b c line measured=() header=() reserved=() refused=0 differ=0 unused=0
    local table="$RUN_SCRATCH/admitted"
    claims_judge admitted-rows || refused=1
    reader_load
    claims | python3 "$READER" admitted >"$table" 2>"$table.err" || {
        refused=$((refused + 1))
        fail admitted-rows "the classifier stopped before its end, so what it printed is no reading of the rows: $(tail -1 "$table.err" | head -c 240)"
    }
    while IFS=$'\t' read -r kind a b c; do
        case "$kind" in
        REFUSED)
            refused=$((refused + 1))
            fail admitted-rows "the row $a is refused: $b"
            ;;
        FORM)
            line="$(admitted_line "$a" "$b" "$c")"
            measured+=("$line")
            if [ "$b" -eq 0 ]; then
                unused=$((unused + 1))
                fail admitted-form "$a is used by no row of claims.jsonl, so the classifier admits a form nothing checks it against: $line"
            else
                printf '  %s\n' "$line"
            fi
            ;;
        RESERVED)
            reserved+=("$(reserved_line "$a" "$b")")
            ;;
        esac
    done <"$table"
    [ "${#measured[@]}" -gt 0 ] && [ "$unused" -eq 0 ] &&
        pass admitted-form "every one of the ${#measured[@]} entries of ADMITTED, listed above with the rows that use each, is used by a row"
    if [ "${#measured[@]}" -eq 0 ]; then
        fail admitted-rows "the classifier printed no table, so no row was read"
    elif [ "$refused" -eq 0 ]; then
        pass admitted-rows "every line of claims.jsonl is a row the one parse reads, and the classifier refuses no row that carries a command"
    fi

    # The header's table: the lines after its column names, down to the first comment line
    # that is no line of it.
    mapfile -t header < <(awk '/^#   form +rows  what it is$/ { on = 1; next } on && !/^#   / { exit } on { sub(/^#   /, ""); print }' "${BASH_SOURCE[0]}")
    for line in "${measured[@]}"; do
        printf '%s\n' "${header[@]}" | grep -qxF -- "$line" && continue
        differ=1
        fail admitted-header "the header's table above run_recorded lacks the measured line: $line"
    done
    for line in "${header[@]}"; do
        printf '%s\n' "${measured[@]}" | grep -qxF -- "$line" && continue
        differ=1
        fail admitted-header "the header's table above run_recorded holds a line --admitted does not measure: $line"
    done
    if [ "$differ" -eq 0 ] && [ "$(printf '%s\n' "${header[@]}")" != "$(printf '%s\n' "${measured[@]}")" ]; then
        differ=1
        fail admitted-header "the header's table above run_recorded holds the measured lines in another order or number than ADMITTED"
    fi
    [ "$differ" -eq 0 ] && pass admitted-header "the header's table above run_recorded is the table --admitted measures, line for line"

    # admitted-reserved has two parts, and its one verdict follows both: the header's list
    # of reserved names, read as its table is, and the names the runner and the shims
    # record with.
    local reserved_ok=1
    mapfile -t header < <(awk '/^#   reserved name +what it is$/ { on = 1; next } on && !/^#   / { exit } on { sub(/^#   /, ""); print }' "${BASH_SOURCE[0]}")
    if [ "${#reserved[@]}" -eq 0 ] || [ "$(printf '%s\n' "${header[@]}")" != "$(printf '%s\n' "${reserved[@]}")" ]; then
        reserved_ok=0
        fail admitted-reserved "the header's list of reserved names above run_recorded is not the reader's, which reads: $(printf '%s; ' "${reserved[@]}")"
    fi
    local name prefix stray=""
    while IFS= read -r name; do
        for prefix in $RUNNER_PREFIXES; do
            [[ "$name" == "$prefix"* ]] && continue 2
        done
        stray="$stray $name"
    done < <(declare -f run_recorded program_shims | grep -oE '\b__[A-Za-z][A-Za-z0-9_]*' | sort -u)
    if [ -n "$stray" ]; then
        reserved_ok=0
        fail admitted-reserved "the runner or the shims record with a name no prefix of RUNNER_PREFIXES reserves:$stray"
    fi
    [ "$reserved_ok" -eq 1 ] &&
        pass admitted-reserved "the header's list of reserved names above run_recorded is the one the reader reserves ($(printf '%s\n' "${reserved[@]}" | awk '{ printf "%s%s", sep, $1; sep = ", " }')), and every name the runner and the shims record with starts with one of RUNNER_PREFIXES ($RUNNER_PREFIXES)"
    return 0
}

mode_replay() {
    local work="$RUN_SCRATCH/replay" unread_rows=0
    mkdir -p "$work"
    # The one parse of claims.jsonl comes first, before any environment is built or any
    # reader starts: a line that is no row fails claim-form, and every read below reads
    # only the rows it read, env.sh's reads of claims.jsonl among them (IOC_ENV_CLAIMS).
    claims_judge claim-form || unread_rows=1
    local IOC_ENV_CLAIMS="$CLAIMS_ROWS"
    # Two rows that judge one claim of a record under check are compared from those rows
    # alone, before anything below can stop the mode, so a disagreement is reported
    # however the rest of the run goes.
    claim_marks_judge
    # The rows env.sh reads a value from are judged here too, before env.sh builds anything
    # (the header's "The one parse of claims.jsonl"): a row env.sh would read wrong, or
    # whose pin is not the value its predicate records, fails claim-form by name, and the
    # mode stops there (evidence_stops), so no assertion after it is judged against an
    # environment built from evidence that is wrong, and none is put down to the
    # environment. The run then ends exit 1 by that FAIL.
    if [ "${IOC_NO_ENV:-0}" != 1 ]; then
        env_functions || cannot_run gate " --replay: env.sh, which names the values it reads from claims.jsonl, did not load."
        env_rows_judge claim-form ||
            evidence_stops "the evidence stops --replay at claim-form: env.sh builds what the recorded commands read from the rows it reads a value from, and claim-form failed on one of them, so env.sh builds nothing and no recorded command runs"
    fi
    # The scratch of the rows, each row's output and its own empty TMPDIR, is under the
    # run's scratch, which the EXIT trap removes however the run ends, and this function
    # removes it at its end too. env.sh clones the three repositories and agent-infra at
    # the commits sources.json pins, so nothing clones them here.
    # The private repository a row reads is read first, then the GitHub header files are
    # written and the budget is read, all before the live inputs below and before the
    # environment is prepared, so a run that cannot carry GitHub's reads refuses at once
    # rather than after count.py's read of the feed or an install, and a refused repository
    # is named whatever the budget. The header files are the ones env.sh's read of the
    # review corpus and the recorded curls of GitHub's REST API send, written under this
    # run's scratch before env.sh is sourced and again before a row when they are old
    # (github_headers_fresh), and the budget is read with the one of
    # cybermonklabs/legion, so it is the token's budget when there is a token
    # (github_budget). The self-test's fixture builds nothing with env.sh, so it writes them
    # only when one of its rows sends one, as check.test.sh's cases 117 to 119 plant, and
    # reads no budget.
    private_repository_read
    if [ "${IOC_NO_ENV:-0}" != 1 ] || claims | grep -qF 'IOC_GITHUB_AUTH_'; then
        github_headers
    fi
    github_budget
    # The live inputs of the assertions below, read before any of them judges and before
    # env.sh prepares anything: the two records under check, whose copies every row that
    # reads a record reads too, so env.sh reads neither again (record_load), the
    # judgement, which count-zero-premise reads, and the count of our own failures, taken
    # again by coordination/count.py over the feed of its window, which count-reruns,
    # count-class and count-denominator read (count_load).
    local label IOC_ENV_RECORDS_READ=1
    for label in $(jq -r '.records_under_check[].label' "$EVIDENCE/sources.json"); do
        record_load "$label"
    done
    judgement_load
    count_load
    prepare_env
    # The header file every recorded curl of the Dispatch API reads. The prerequisites read
    # that DISPATCH_TOKEN_FILE names a readable file; a header that still cannot be written
    # leaves it unset, and those rows then fail on the read they could not make.
    IOC_DISPATCH_HEADER="$(dispatch_header)" && export IOC_DISPATCH_HEADER

    local kinds='project-code our-code behaviour published absence quotation'
    local marks='verified false unverified not-externally-checkable supported unsupported unreleased'
    local bad_form="$unread_rows" bad_mark=0 bad_predicate=0 ran=0 command_failures="" absence_failures="" bad_quotation=0
    local row id kind mark command expect environments installs stamps changed frozen_failures=""
    # The environments each row installs into, as the install reader reads its command;
    # each is checked after the row, beside any whose site-packages changed while it ran.
    # A reader that cannot load cannot run the mode; one that loaded and then stopped
    # fails install-pinned, since which environment a row installs into is then unread.
    reader_load
    environments="$(claims | python3 "$READER" environments 2>"$work/environments.err")" || {
        frozen_failures="$frozen_failures reader"
        fail install-pinned "the install reader stopped before its end, so which environment each row installs into is unread: $(tail -1 "$work/environments.err" | head -c 240)"
    }
    while IFS= read -r row; do
        id="$(printf '%s' "$row" | jq -r '.id')"
        kind="$(printf '%s' "$row" | jq -r '.kind // ""')"
        mark="$(printf '%s' "$row" | jq -r '.mark // ""')"
        case " $kinds " in *" $kind "*) ;; *)
            bad_form=$((bad_form + 1))
            fail claim-form "$id carries the kind '$kind', which is none of the five forms"
            ;;
        esac
        case " $marks " in *" $mark "*) ;; *)
            bad_mark=$((bad_mark + 1))
            fail claim-mark "$id carries the mark '$mark', which its requirement does not fix"
            ;;
        esac
        expect="$(printf '%s' "$row" | jq -c '.expect // empty')"
        if [ -n "$expect" ] && [ "$(printf '%s' "$expect" | jq 'if type == "object" then ([keys[] | select(. != "exit")] | length) + (if has("exit") and length == 1 then 1 else 0 end) else 0 end')" != 1 ]; then
            bad_predicate=$((bad_predicate + 1))
            fail predicate-form "$id carries $(printf '%s' "$expect" | jq -r 'if type == "object" then (keys | join(" and ")) else "no key" end') in one expect, and a row is held to exactly one predicate over its output, which an exit status may stand beside: $expect"
        elif [ -n "$expect" ] && predicate_is_a_sentence "$expect"; then
            bad_predicate=$((bad_predicate + 1))
            fail predicate-form "$id carries a predicate that is not a bound, a set membership or an exit status of the shape the header's rule for expect gives it, since $IOC_PREDICATE_WHY: $expect"
        fi
        if [ "$kind" = quotation ]; then
            printf '%s' "$row" | jq -e '.evidence.marked // empty' >/dev/null ||
                {
                    bad_quotation=$((bad_quotation + 1))
                    fail quotation-marked "$id quotes a project's prose and is not marked a claim of the project"
                }
        fi
        command="$(printf '%s' "$row" | jq -r '.command // ""')"
        [ -n "$command" ] || continue
        if [ -z "$expect" ]; then
            # A command with nothing to meet is a command nobody reads: the gate would run
            # it and compare nothing, so a recorded output the command no longer prints
            # would pass.
            bad_predicate=$((bad_predicate + 1))
            fail predicate-form "$id carries a command and no predicate, so nothing of its output is read"
            continue
        fi
        ran=$((ran + 1))
        # A row's install is no build of env.sh's: env.sh's stamp of each environment the
        # row installs into goes before the row runs, and that of each whose site-packages
        # changed while it ran goes after, so the next run builds each again rather than
        # reuse what a killed install left (the header's "Nothing a killed run leaves is
        # trusted").
        installs="$(printf '%s\n' "$environments" | awk -F '\t' -v id="$id" '$1 == id { print $2 }')"
        for label in $installs; do
            ioc_env_unstamp "$label"
        done
        stamps="$(environment_stamps)"
        # The GitHub header files are written again when they are old, before the row and
        # never inside its command (the header's "The GitHub tokens stand on no command line").
        github_headers_fresh
        run_recorded "$command" "$work/$id.out"
        changed="$({
            printf '%s\n' "$installs"
            diff <(printf '%s\n' "$stamps") <(environment_stamps) | awk '$1 == ">" { print $2 }'
        } | sort -u)"
        for label in $changed; do
            ioc_env_unstamp "$label"
        done
        # shellcheck disable=SC2086
        [ -z "$changed" ] || check_environments "$id" $changed || frozen_failures="$frozen_failures $id"
        # A program the evaluator cannot record fails the row whatever its kind and its
        # predicate, since no status of it would be read.
        if [ -s "$work/$id.out.unread" ]; then
            if [ "$kind" = absence ]; then
                absence_failures="$absence_failures $id"
                fail absence-still-empty "$id runs $(head -1 "$work/$id.out.unread"), so its search is no reading the evaluator can make"
            else
                command_failures="$command_failures $id"
                fail replay-command "$id runs $(head -1 "$work/$id.out.unread"), so its status is no reading the evaluator can make"
            fi
            continue
        fi
        # A row's failed read of a live source, GitHub or Dispatch, is read by source_answer
        # before it judges the row, an absence search's as every other: a read that got no
        # answer, a server error, a rate limit or an answer about the reader's credential is
        # no verdict on the deliverable, and the mode cannot run, of the kind source, since
        # only the source's answer about the record judges the row.
        if [ "$kind" = absence ]; then
            if failed_read "$work/$id.out"; then
                source_answer "$id" "$work/$id.out"
                [ -z "$IOC_SOURCE_STOP" ] || cannot_run source " --replay now: $IOC_SOURCE_STOP"
                absence_failures="$absence_failures $id"
                fail absence-still-empty "$id searched a corpus this run never read: $IOC_FAILED_READ${IOC_SOURCE_ANSWER:+, and $IOC_SOURCE_ANSWER}, so the search found nothing because nothing was read"
                continue
            fi
            if grep -q '[^[:space:]]' "$work/$id.out"; then
                if [ -n "$expect" ] && evaluate "$expect" "$work/$id.out"; then
                    :
                else
                    absence_failures="$absence_failures $id"
                    fail absence-still-empty "$id searched again and found something: $(head -c 160 "$work/$id.out" | tr '\n' ' ')"
                fi
            fi
            continue
        fi
        # A read that failed anywhere in the row leaves its output no reading of what the
        # row claims, whatever that output still meets: a traceback has enough lines for a
        # bound. A row whose predicate is an exit status claims the failure itself. A failed
        # read of a live source is read by source_answer, as above, and the row's line names
        # what the source answered.
        if failed_read "$work/$id.out"; then
            source_answer "$id" "$work/$id.out"
            [ -z "$IOC_SOURCE_STOP" ] || cannot_run source " --replay now: $IOC_SOURCE_STOP"
            command_failures="$command_failures $id"
            fail replay-command "$id ran its command and a read inside it failed: $IOC_FAILED_READ${IOC_SOURCE_ANSWER:+, and $IOC_SOURCE_ANSWER}, so its output is no reading of what the row claims"
            continue
        fi
        IOC_BOUND_READ=""
        if ! evaluate "$expect" "$work/$id.out"; then
            command_failures="$command_failures $id"
            fail replay-command "$id ran '$command' and its output does not meet $expect${IOC_BOUND_READ:+ (the bound read $IOC_BOUND_READ)}: $(head -c 160 "$work/$id.out" | tr '\n' ' ')"
        fi
    done < <(claims)

    # unpinned_installs runs the reader reader_load loaded above, so one that stops here
    # stopped on a row the one parse read, a failure of the gate and no mode that cannot run.
    local unpinned unpinned_id unpinned_what
    unpinned="$(unpinned_installs 2>"$work/installs.err")" || {
        frozen_failures="$frozen_failures reader"
        fail install-pinned "the install reader stopped before its end, so what a row installs is unread: $(tail -1 "$work/installs.err" | head -c 240)"
    }
    while IFS=$'\t' read -r unpinned_id unpinned_what; do
        [ -n "$unpinned_id" ] || continue
        fail install-pinned "$unpinned_id runs '$unpinned_what': a package with no version, or read from no hashed lock, installs whatever release or file the index holds now, and a form this reader cannot read is refused rather than passed"
    done <<<"$unpinned"
    [ -z "$unpinned" ] && [ -z "$frozen_failures" ] && pass install-pinned "every package a recorded command installs or runs names its version and is read from a committed hashed lock, or is a local path built under a build constraint, and so does every build backend, and every environment a row installed into holds only packages its lock holds at their version and packages its pinned clone builds"

    # None of these passes on no input: a claims.jsonl with no row, no command, no
    # quotation or no absence search to read fails the assertion over it by name.
    local rows quotations absences
    rows="$(claims | wc -l)"
    quotations="$(claims | jq -c 'select(.kind=="quotation")' | wc -l)"
    absences="$(claims | jq -c 'select(.kind=="absence" and (.command // "") != "")' | wc -l)"
    [ "$rows" -gt 0 ] || { bad_form=$((bad_form + 1)) && fail claim-form "claims.jsonl holds no row, so no claim is judged"; }
    [ "$rows" -gt 0 ] || { bad_mark=$((bad_mark + 1)) && fail claim-mark "claims.jsonl holds no row, so no mark is judged"; }
    [ "$ran" -gt 0 ] || { bad_predicate=$((bad_predicate + 1)) && fail predicate-form "no row of claims.jsonl carries a command and a predicate, so no predicate is judged"; }
    [ "$quotations" -gt 0 ] || { bad_quotation=$((bad_quotation + 1)) && fail quotation-marked "claims.jsonl holds no quotation, though the authors' account is quoted, so no mark of one is judged"; }
    [ "$ran" -gt 0 ] || { command_failures="$command_failures none" && fail replay-command "no recorded command ran, so nothing was replayed"; }
    [ "$absences" -gt 0 ] || { absence_failures="$absence_failures none" && fail absence-still-empty "claims.jsonl holds no absence search, so none is read again"; }
    [ "$bad_form" -eq 0 ] && pass claim-form "every claim of $rows carries one of the five forms"
    [ "$bad_mark" -eq 0 ] && pass claim-mark "every claim of $rows carries a mark its requirement fixes"
    [ "$bad_predicate" -eq 0 ] && pass predicate-form "every one of the $ran rows with a command carries exactly one predicate, and each is a bound, a set membership or an exit status"
    [ "$bad_quotation" -eq 0 ] && pass quotation-marked "every one of the $quotations quotations is marked a claim of the project and not of its code"
    [ -z "$command_failures" ] && pass replay-command "$ran recorded commands ran again, no read inside one failed, and each met the predicate its row carries"
    [ -z "$absence_failures" ] && pass absence-still-empty "every one of the $absences absence searches still finds nothing"

    # The id list is read by three of the assertions below and built once here.
    claims | jq -r '.id' | sort -u >"$work/ids"
    replay_citation_rows "$work/ids"
    replay_mas_lab "$work/ids"
    replay_interview "$work/ids"
    replay_our_data
    replay_count
    rm -rf "$work"
}

replay_citation_rows() {
    # replay_citation_rows <id file>; the ids of claims.jsonl, one per line. The expected
    # set is committed: the rows of claims.jsonl whose id is a citation of a record under
    # check, <label>-<number>. Each citation the record carries has such a row, each such
    # row is a citation the record carries, and a label with no such row is a failure, so
    # a record read as nothing, or a set of rows taken out, is no pass.
    local ids="$1" work="$RUN_SCRATCH/citation-rows" label missing=0 total=0 cid
    mkdir -p "$work"
    if [ "$(jq '.records_under_check | length' "$EVIDENCE/sources.json")" -eq 0 ]; then
        missing=$((missing + 1))
        fail citation-row "sources.json names no record under check, so no citation is checked"
    fi
    for label in $(jq -r '.records_under_check[].label' "$EVIDENCE/sources.json"); do
        record_text "$label" >"$work/record-$label.md"
        python3 "$EVIDENCE/citations.py" extract --input "$work/record-$label.md" --label "$label" >"$work/cit-$label.jsonl" || {
            missing=$((missing + 1))
            fail citation-row "citations.py could not read the record labelled $label"
            continue
        }
        jq -r '.cid' "$work/cit-$label.jsonl" | sort -u >"$work/cids-$label"
        grep -E "^$label-[0-9]+\$" "$ids" | sort -u >"$work/rows-$label" || true
        if [ ! -s "$work/rows-$label" ]; then
            missing=$((missing + 1))
            fail citation-row "claims.jsonl carries no row of a citation of the record labelled $label, so nothing of it is checked"
            continue
        fi
        while IFS= read -r cid; do
            missing=$((missing + 1))
            fail citation-row "the citation $cid of the record labelled $label has no row with a mark"
        done < <(comm -23 "$work/cids-$label" "$work/rows-$label")
        while IFS= read -r cid; do
            missing=$((missing + 1))
            fail citation-row "the row $cid is no citation the record labelled $label carries as it was read now"
        done < <(comm -13 "$work/cids-$label" "$work/rows-$label")
        total=$((total + $(wc -l <"$work/rows-$label")))
    done
    [ "$missing" -eq 0 ] && pass citation-row "each of the $total rows of claims.jsonl that cite a record under check is a citation the record carries, and each citation it carries has its row"
    rm -rf "$work"
}

replay_mas_lab() {
    # replay_mas_lab <id file>; the ids of claims.jsonl, one per line.
    local ids="$1" commit missing=0 directory
    commit="$(jq -r '.repositories[] | select(.name=="mas-lab") | .commit' "$EVIDENCE/sources.json")"
    for directory in library-standard ctl runtime library-eval library-telemetry library-kg; do
        if [ ! -s "$EVIDENCE/mas-lab/$directory.md" ]; then
            missing=$((missing + 1))
            fail mas-lab-directory "the directory $directory of mas-lab has no evidence file"
            continue
        fi
        grep -q "$commit" "$EVIDENCE/mas-lab/$directory.md" || {
            missing=$((missing + 1))
            fail mas-lab-directory "the evidence file of $directory does not name the clone commit $commit"
        }
    done
    [ "$missing" -eq 0 ] && pass mas-lab-directory "each of the six named directories of mas-lab has an evidence file at the clone commit"

    local unanswered=0 question
    for question in \
        "Question 1: can its specification format express Legion's phase workflow?" \
        "Question 2: could its experiment harness measure whether a change to a role's prompt or skill made Legion better?" \
        "Question 3: what its boundary enforcement does that our refusals do not"; do
        local file
        file="$(grep -rl -F "$question" "$EVIDENCE/mas-lab/" 2>/dev/null | head -1)"
        if [ -z "$file" ]; then
            unanswered=$((unanswered + 1))
            fail mas-lab-question "no evidence file answers: $question"
            continue
        fi
        # The answer names its evidence: at least one claim id of a row of claims.jsonl.
        local named=0 cid
        for cid in $(sed -n "/$(printf '%s' "$question" | sed 's/[][\.*^$\/]/\\&/g')/,/^## /p" "$file" | grep -o '\[[a-z0-9-]*\]' | tr -d '[]' | sort -u); do
            grep -qx "$cid" "$ids" && named=1
        done
        [ "$named" -eq 1 ] || {
            unanswered=$((unanswered + 1))
            fail mas-lab-question "the answer to '$question' in ${file##*/} names no row of claims.jsonl"
        }
    done
    [ "$unanswered" -eq 0 ] && pass mas-lab-question "each of the three questions is answered by a file that names its evidence"
}

replay_interview() {
    # replay_interview <id file>; the ids of claims.jsonl, one per line.
    local ids="$1" missing=0 id
    for id in i-scope-triggers i-three-tiers i-skills-drop-in; do
        grep -qx "$id" "$ids" || {
            missing=$((missing + 1))
            fail interview-fixed-row "the row $id, which the specification fixes, is missing"
        }
    done
    local measurements
    measurements="$(grep -c '^i-measure-' "$ids" || true)"
    [ "${measurements:-0}" -ge 1 ] || {
        missing=$((missing + 1))
        fail interview-fixed-row "no row records a measurement the interview states"
    }
    [ "$missing" -eq 0 ] && pass interview-fixed-row "the four rows the specification fixes are present, with $measurements measurement rows"
}

replay_our_data() {
    # The three tests against our own records the specification names, each a file of
    # tests/; a test whose file is missing fails, so an empty directory is no pass.
    local bad=0 file name
    for name in concord scope mas-lab; do
        file="$EVIDENCE/tests/$name.md"
        [ -s "$file" ] || {
            bad=$((bad + 1))
            fail our-data-input "the test $name against our own records has no file tests/$name.md"
            continue
        }
        local input
        input="$(grep -m1 '^Input:' "$file" || true)"
        if [ -z "$input" ]; then
            bad=$((bad + 1))
            fail our-data-input "${file##*/} names no input"
            continue
        fi
        if ! printf '%s' "$input" | grep -qE 'dispatch://[A-Z]+-[0-9]+|[0-9a-f]{40}'; then
            bad=$((bad + 1))
            fail our-data-input "${file##*/} names an input that is neither a Dispatch key nor a commit of ours: $input"
        fi
        if printf '%s' "$input" | grep -qE 'tests/fixtures|/fixtures/|golden'; then
            bad=$((bad + 1))
            fail our-data-input "${file##*/} drives a fixture of the project rather than a record of ours: $input"
        fi
    done
    [ "$bad" -eq 0 ] && pass our-data-input "each test against our own records names the Dispatch key or the commit its input came from"
}

# The count of our own failures, taken again. coordination/count.py is the walker that
# took the count and printed evidence/count.json, the count this tree commits: it reads
# window.json beside it and the feed of that window from Dispatch, at DISPATCH_URL with
# the bearer DISPATCH_TOKEN_FILE names, and it reads every row, the issue tree and the
# cited authors as they stood at the window's end (the header's pinned-read rule), so a
# later run prints the count again, and --replay holds it to that.
#
# count_load; runs count.py once per run, before any assertion judges, through live_read,
# into this run's scratch. count.py raises on a read that fails, an HTTP error status or
# an address that does not answer, past the two retries it gives a document version, and
# prints its count only after its last read, so such a read leaves a non-zero status and
# nothing on stdout, which is the feed not answering: the mode cannot run, exit 2, or
# exit 1 after a FAIL, by the precedence of the header, and no assertion over the count
# is judged. Its reason is the last line of count.py's traceback, the error it raised,
# which count_run hands live_read in place of the frames above it. A document version
# the deployment no longer holds is no failed read: count.py counts it as unreadable,
# and the count it prints then differs. A count.py that is not there is not run, and
# count-reruns fails naming it. The run takes about twelve minutes against Dispatch:
# 721 seconds, measured on 2026-11-18.
count_load() {
    [ -f "$EVIDENCE/coordination/count.py" ] || return 0
    live_read "Dispatch, the feed of the count's window" "$RUN_SCRATCH/count-rerun.json" count_run || :
}
count_run() {
    python3 "$EVIDENCE/coordination/count.py" 2>"$RUN_SCRATCH/count-rerun.trace" && return 0
    tail -1 "$RUN_SCRATCH/count-rerun.trace" >&2
    return 1
}

# replay_count; the assertions over the count, count-reruns first. What count.py printed
# again is one JSON object, and each of its figures equals evidence/count.json's, key for
# key, but one: feed.events_read is the number of rows of the whole feed count.py read,
# from the first row to the newest at its run, since it walks the feed to its end to find
# the window's rows, so it grows with the feed and is no figure of the window. It is not
# compared, and the PASS line prints both of its values, so a reader sees it move; every
# other figure of feed, the first and the last event of the window, is compared. A key
# that differs is named, and for classes, denominator_records_read and instances so are
# its two values. count-class and count-denominator read the same object beside
# count.md; a count.py that is not there, or one that printed no JSON object, fails them
# too, since their part over the count taken again then reads nothing.
replay_count() {
    local window="$EVIDENCE/coordination/window.json" count="$EVIDENCE/coordination/count.md"
    local committed="$EVIDENCE/evidence/count.json" rerun="$RUN_SCRATCH/count-rerun.json"
    local reran="" differed=0 over differs key now then
    if [ ! -f "$EVIDENCE/coordination/count.py" ]; then
        differed=$((differed + 1))
        fail count-reruns "coordination/count.py, the walker that took the count, is missing, so the count is not taken again"
    elif jq -se 'length == 1 and (.[0] | type) == "object"' "$rerun" >/dev/null 2>&1; then
        reran="$rerun"
    else
        differed=$((differed + 1))
        fail count-reruns "coordination/count.py ran again and printed no one JSON object: $(head -c 160 "$rerun" 2>/dev/null | tr '\n' ' ')"
    fi
    if [ ! -f "$committed" ]; then
        differed=$((differed + 1))
        fail count-reruns "evidence/count.json, the count this tree commits, is missing, so nothing holds what coordination/count.py prints"
    elif ! jq -se 'length == 1 and (.[0] | type) == "object"' "$committed" >/dev/null 2>&1; then
        differed=$((differed + 1))
        fail count-reruns "evidence/count.json, the count this tree commits, is not one JSON object"
    elif [ -n "$reran" ]; then
        over="$(jq -r 'if (.window.from | type) == "string" and (.window.to | type) == "string" then "the window \(.window.from) to \(.window.to)" else empty end' "$reran" 2>/dev/null)"
        [ -n "$over" ] || over="a window it does not name"
        differs="$(jq -rn --slurpfile now "$reran" --slurpfile then "$committed" '
            def figures: if (.feed | type) == "object" then .feed |= del(.events_read) else . end;
            ($now[0] | figures) as $a | ($then[0] | figures) as $b
            | (($b | keys_unsorted) + (($a | keys_unsorted) - ($b | keys_unsorted)))[] as $k
            | select(($a | has($k)) != ($b | has($k)) or $a[$k] != $b[$k])
            | "\($k)\t\(if $a | has($k) then $a[$k] | tojson else "nothing" end)\t\(if $b | has($k) then $b[$k] | tojson else "nothing" end)"' 2>&1)" || {
            differed=$((differed + 1))
            fail count-reruns "the comparison of what coordination/count.py printed with evidence/count.json stopped before its end: $(printf '%s' "$differs" | tail -1 | head -c 240)"
            differs=""
        }
        while IFS=$'\t' read -r key now then; do
            [ -n "$key" ] || continue
            differed=$((differed + 1))
            case "$key" in
            classes | denominator_records_read | instances)
                fail count-reruns "coordination/count.py ran again over $over and printed $key $now, and evidence/count.json holds $key $then"
                ;;
            *)
                fail count-reruns "coordination/count.py ran again over $over and printed a count that differs from evidence/count.json at $key"
                ;;
            esac
        done <<<"$differs"
        [ "$differed" -eq 0 ] && pass count-reruns "coordination/count.py ran again over $over and printed evidence/count.json again, every figure equal but feed.events_read, the rows of the whole feed it read up to the run, which is no figure of the window: $(jq -r '.feed.events_read // "none"' "$reran" 2>/dev/null) now, $(jq -r '.feed.events_read // "none"' "$committed" 2>/dev/null) in evidence/count.json"
    fi

    if [ ! -s "$window" ]; then
        fail count-window "the count names no window"
        return
    fi
    local from to
    from="$(jq -r '.from // ""' "$window")"
    to="$(jq -r '.to // ""' "$window")"
    if printf '%s%s' "$from" "$to" | grep -qE '^[0-9]{4}-[0-9]{2}-[0-9]{2}T.*[0-9]{4}-[0-9]{2}-[0-9]{2}T'; then
        pass count-window "the window is two instants, $from to $to"
    else
        fail count-window "the window is not two instants: from='$from' to='$to'"
    fi

    local classes='across sessions|between an agent and a person|inside one tree'
    local missing=0 class
    IFS='|' read -ra names <<<"$classes"
    for class in "${names[@]}"; do
        grep -qF "$class" "$count" || {
            missing=$((missing + 1))
            fail count-class "the count does not carry the class '$class'"
        }
    done
    # The classes of the count taken again are the three keys count.py writes for them.
    if [ -z "$reran" ]; then
        missing=$((missing + 1))
        fail count-class "the count was not taken again into one JSON object, so the classes coordination/count.py prints are unread"
    elif ! jq -e '(.classes | type) == "object" and (.classes | keys) == ["across_sessions", "between_an_agent_and_a_person", "inside_one_tree"]' "$reran" >/dev/null 2>&1; then
        missing=$((missing + 1))
        fail count-class "the count coordination/count.py printed again carries the classes $(jq -c '.classes | if type == "object" then keys else . end' "$reran" 2>/dev/null), not exactly between_an_agent_and_a_person, inside_one_tree and across_sessions"
    fi
    [ "$missing" -eq 0 ] && pass count-class "the classes are the three the specification names, in count.md and in the count coordination/count.py printed again"

    # Each class is a row of the table: the class, what it counted, and the denominator,
    # which is the records read in the window.
    local without=0 zero_classes="" class_row instances denominator
    # Every row of the table is read, not only the three classes: a sample counted beside
    # them carries a denominator of its own and may read zero over it.
    for class in "${names[@]}"; do
        grep -qE "^\| *$class *\| *[0-9]+ *\| *[0-9]+ *\|" "$count" || {
            without=$((without + 1))
            fail count-denominator "the class '$class' carries no row of the table with a count and a denominator"
        }
    done
    while IFS= read -r class_row; do
        class="$(printf '%s' "$class_row" | awk -F'|' '{gsub(/^ +| +$/,"",$2); print $2}')"
        instances="$(printf '%s' "$class_row" | awk -F'|' '{gsub(/[^0-9]/,"",$3); print $3}')"
        denominator="$(printf '%s' "$class_row" | awk -F'|' '{gsub(/[^0-9]/,"",$4); print $4}')"
        if [ "$instances" -eq 0 ] && [ "$denominator" -gt 0 ]; then
            zero_classes="$zero_classes '$class'"
        fi
    done < <(grep -E "^\|[^|]+\| *[0-9]+ *\| *[0-9]+ *\|" "$count")
    # The count taken again carries its denominator, the comments and messages it read in
    # the window, a whole number above zero.
    if [ -z "$reran" ]; then
        without=$((without + 1))
        fail count-denominator "the count was not taken again into one JSON object, so the denominator coordination/count.py prints is unread"
    elif ! jq -e '.denominator_records_read | type == "number" and . > 0 and . == floor' "$reran" >/dev/null 2>&1; then
        without=$((without + 1))
        fail count-denominator "the count coordination/count.py printed again reads $(jq -c '.denominator_records_read' "$reran" 2>/dev/null) records, which is no whole number above zero"
    fi
    [ "$without" -eq 0 ] && pass count-denominator "every class carries its denominator, the records read in the window, and the count coordination/count.py printed again read $(jq -r '.denominator_records_read' "$reran") records"

    local judgement
    judgement="$(judgement_text)"
    if [ -z "$zero_classes" ]; then
        pass count-zero-premise "no class reads zero over a denominator above zero"
    elif printf '%s' "$judgement" | grep -qF 'premise that failed'; then
        pass count-zero-premise "the class$zero_classes reads zero and the front page states that premise as failed"
    else
        fail count-zero-premise "the class$zero_classes reads zero over a denominator above zero and the front page states no failed premise"
    fi
}

# ---------------------------------------------------------------- --shape

# The floor and the cap of candidate pages the specification fixes. Every assertion over
# the pages is held to the floor, so a judgement with fewer pages, or none, is no pass of
# them.
PAGE_FLOOR=3
PAGE_CAP=7

page_blocks() {
    # Prints each candidate page's heading and its body, separated by a form feed, from
    # the judgement judgement_load read.
    judgement_text | awk '/^### /{if(n)printf "\f"; n=1} n{print} END{if(n)printf "\f"}'
}

mode_shape() {
    local text
    judgement_load
    text="$(judgement_text)"
    local pages
    pages="$(printf '%s\n' "$text" | grep -c '^### ' || true)"
    if [ "${pages:-0}" -ge "$PAGE_FLOOR" ] && [ "${pages:-0}" -le "$PAGE_CAP" ]; then
        pass page-count "the judgement carries $pages candidate pages, between the floor of $PAGE_FLOOR and the cap of $PAGE_CAP"
    else
        fail page-count "the judgement carries ${pages:-0} candidate pages, and the floor is $PAGE_FLOOR and the cap is $PAGE_CAP"
    fi

    local front
    front="$(printf '%s\n' "$text" | sed -n '1,/^### /p')"
    local missing=0 part
    for part in "## The ranking" "## The one thing to do first" "## Where the authors' account and the checked reading differ" "## What was deliberately not read"; do
        printf '%s\n' "$front" | grep -qF "$part" || {
            missing=$((missing + 1))
            fail front-page "the front page has no section '$part'"
        }
    done
    [ "$missing" -eq 0 ] && pass front-page "the front page carries the ranking, the one thing to do first, the paragraph on where the two accounts differ, and what was not read"

    local parts=("**Question:**" "**Proposal:**" "**Verdict:**" "**Landing component:**" "**Cost:**" "**Deletes or replaces:**" "**Cost of reversing it:**" "**Default if nobody answers:**" "**Owner:**" "**Node model:**" "**Test:**")
    local bad_parts=0 bad_verdict=0 bad_commitment=0 page title verdict landing read_pages=0
    while IFS= read -r -d $'\f' page; do
        read_pages=$((read_pages + 1))
        title="$(printf '%s\n' "$page" | head -1 | sed 's/^### //')"
        for part in "${parts[@]}"; do
            printf '%s\n' "$page" | grep -qF "$part" || {
                bad_parts=$((bad_parts + 1))
                fail page-parts "the page '$title' is short of $part"
            }
        done
        # R9 names the landing component as Legion, Envoy or Dispatch, so the line's value,
        # after its label and with the whitespace around it and a trailing full stop trimmed,
        # is exactly one of the three; a page with no such line is short of it above.
        landing="$(printf '%s\n' "$page" | grep -m1 -F '**Landing component:**')" && {
            landing="$(printf '%s\n' "$landing" | sed 's/.*\*\*Landing component:\*\*//; s/^[[:space:]]*//; s/[[:space:]]*$//; s/\.$//')"
            case "$landing" in
            Legion | Envoy | Dispatch) ;;
            *)
                bad_parts=$((bad_parts + 1))
                fail page-parts "the page '$title' names the landing component '$landing', which is none of Legion, Envoy and Dispatch"
                ;;
            esac
        }
        verdict="$(printf '%s\n' "$page" | grep -m1 -F '**Verdict:**' | sed 's/.*\*\*Verdict:\*\*[[:space:]]*//' | tr -d '`' | awk '{print $1}')"
        case "$verdict" in
        vendor | depend | port | reject) ;;
        *)
            bad_verdict=$((bad_verdict + 1))
            fail page-verdict "the page '$title' carries the verdict word '$verdict', which is none of vendor, depend, port and reject"
            ;;
        esac
        local others=0 word
        for word in vendor depend port reject; do
            [ "$word" = "$verdict" ] && continue
            printf '%s\n' "$page" | grep -m1 -F '**Verdict:**' | grep -qw "$word" && others=$((others + 1))
        done
        [ "$others" -eq 0 ] || {
            bad_verdict=$((bad_verdict + 1))
            fail page-verdict "the page '$title' carries more than one verdict word"
        }
        case "$verdict" in
        depend)
            printf '%s\n' "$page" | grep -qiE 'upstream (owner|stops)' || {
                bad_commitment=$((bad_commitment + 1))
                fail verdict-commitment "the depend page '$title' names no upstream owner and what we do when upstream stops"
            }
            ;;
        port)
            printf '%s\n' "$page" | grep -qiE 'the change in our code' || {
                bad_commitment=$((bad_commitment + 1))
                fail verdict-commitment "the port page '$title' names no change in our code"
            }
            ;;
        reject)
            printf '%s\n' "$page" | grep -qiE 'rejected because|the reason it is rejected' || {
                bad_commitment=$((bad_commitment + 1))
                fail verdict-commitment "the reject page '$title' names no reason"
            }
            ;;
        esac
        printf '%s\n' "$page" | grep -qE 'INFRA-1098/spec v[0-9]+' && printf '%s\n' "$page" | grep -qE 'INFRA-1253/spec v[0-9]+' || {
            bad_parts=$((bad_parts + 1))
            fail page-parts "the page '$title' does not name the versions of the two node-model specifications it was read against"
        }
    done < <(page_blocks)
    if [ "$read_pages" -lt "$PAGE_FLOOR" ]; then
        bad_parts=$((bad_parts + 1)) bad_verdict=$((bad_verdict + 1)) bad_commitment=$((bad_commitment + 1))
        fail page-parts "the judgement carries $read_pages candidate pages, fewer than the floor of $PAGE_FLOOR, so its pages' parts are not judged"
        fail page-verdict "the judgement carries $read_pages candidate pages, fewer than the floor of $PAGE_FLOOR, so its verdicts are not judged"
        fail verdict-commitment "the judgement carries $read_pages candidate pages, fewer than the floor of $PAGE_FLOOR, so what its verdicts commit us to is not judged"
    fi
    [ "$bad_parts" -eq 0 ] && pass page-parts "each of the $read_pages candidate pages carries all eleven of its parts, and its landing component is Legion, Envoy or Dispatch"
    [ "$bad_verdict" -eq 0 ] && pass page-verdict "each of the $read_pages candidate pages carries exactly one verdict word"
    [ "$bad_commitment" -eq 0 ] && pass verdict-commitment "the verdict of each of the $read_pages candidate pages names what it commits us to"

    shape_fixed_pages "$text"
    shape_questions "$text"
    shape_triage "$text"
    shape_order
}

shape_fixed_pages() {
    local text="$1" bad=0
    # Each fixed page is found by the words its design is written in, and it answers that
    # design by name.
    local envoy definitions execution
    envoy="$(printf '%s\n' "$text" | awk '/^### /{p=0} /^### .*[Ee]nvoy/{p=1} p')"
    definitions="$(printf '%s\n' "$text" | awk '/^### /{p=0} /^### .*[Dd]efinitions/{p=1} p')"
    execution="$(printf '%s\n' "$text" | awk '/^### /{p=0} /^### .*[Ee]xecution record/{p=1} p')"
    if [ -z "$envoy" ] || ! printf '%s' "$envoy" | grep -qF 'quotes'; then
        bad=$((bad + 1))
        fail fixed-page "the page on structured metadata on an Envoy message does not answer the quotes field by name"
    fi
    if [ -z "$definitions" ] || ! printf '%s' "$definitions" | grep -qiE 'unit and its scale as values|unit and scale as values'; then
        bad=$((bad + 1))
        fail fixed-page "the page on one definitions record does not answer the typed definition carrying its unit and its scale as values"
    fi
    if [ -z "$execution" ] || ! printf '%s' "$execution" | grep -qF 'gen_ai.'; then
        bad=$((bad + 1))
        fail fixed-page "the page on an execution record does not answer the OpenTelemetry gen_ai names"
    fi
    [ "$bad" -eq 0 ] && pass fixed-page "the three pages the specification fixes exist and each answers by name the design already written for it"
}

shape_questions() {
    local text="$1" bad=0 number
    local listed
    listed="$(grep -c '^## [0-9]*\.' "$EVIDENCE/questions.md" || true)"
    [ "${listed:-0}" -gt 0 ] || {
        bad=$((bad + 1))
        fail question-list "the question list carries no question, so no page is held to one"
    }
    while IFS= read -r number; do
        grep -qE "^## $number\." "$EVIDENCE/questions.md" || {
            bad=$((bad + 1))
            fail question-list "a page answers question $number, which the question list does not carry"
        }
    done < <(printf '%s\n' "$text" | grep -oE '\*\*Question:\*\*[[:space:]]*Q[0-9]+' | grep -oE '[0-9]+')
    for number in $(seq 1 "${listed:-0}"); do
        printf '%s\n' "$text" | grep -qE "\*\*Question:\*\*[[:space:]]*Q$number\b" && continue
        printf '%s\n' "$text" | grep -qE "[Qq]uestion $number( |,|\.|:)" || {
            bad=$((bad + 1))
            fail question-list "question $number is answered by no page and named on no front page"
        }
    done
    [ "$bad" -eq 0 ] && pass question-list "every page's question is on the list of $listed, and every question is answered by a page or named on the front page"
}

shape_triage() {
    local text="$1" bad=0 name
    for name in mas-lab scope cognition-protocols ioc-project.github.io dot-github; do
        [ -s "$EVIDENCE/triage/$name.md" ] || {
            bad=$((bad + 1))
            fail triage-named "the triage of $name is missing"
            continue
        }
    done
    # Every repository the triage dropped is named on the front page with its reason.
    local front
    front="$(printf '%s\n' "$text" | sed -n '1,/^### /p')"
    for name in mas-lab scope cognition-protocols ioc-project.github.io dot-github; do
        printf '%s' "$front" | grep -qF "$name" || {
            bad=$((bad + 1))
            fail triage-named "the front page does not name the repository $name"
        }
    done
    [ "$bad" -eq 0 ] && pass triage-named "every repository of the organisation is triaged and named on the front page"
}

shape_order() {
    local questions_commit history
    history="$(history_ref)"
    questions_commit="$(git -C "$ROOT" log --diff-filter=A --format='%H' "$history" -- docs/research/internet-of-cognition/questions.md | tail -1)"
    if [ -z "$questions_commit" ]; then
        fail order-of-work "no commit of this history adds the question list"
        return
    fi
    local bad=0 commit path
    # The question list lands in a commit of its own: the commit that adds it adds no
    # other file of this directory. A tree that added the evidence beside it would pass
    # the ancestry rule and break the order the specification fixes.
    local beside
    beside="$(git -C "$ROOT" show --name-only --format= "$questions_commit" | grep '^docs/research/internet-of-cognition/' | grep -v 'questions.md$' || true)"
    if [ -n "$beside" ]; then
        bad=$((bad + 1))
        fail order-of-work "the commit that adds the question list adds $(printf '%s' "$beside" | tr '\n' ' ') beside it"
    fi
    # Each of the five deep-read directories is held to this order, and each holds at
    # least one file in this history: a directory with none is no pass of the order.
    local directory
    for directory in mas-lab triage tests coordination similarity; do
        [ -n "$(git -C "$ROOT" ls-tree -r --name-only "$history" -- "docs/research/internet-of-cognition/$directory" | head -1)" ] || {
            bad=$((bad + 1))
            fail order-of-work "this history holds no file of the deep-read directory $directory"
        }
    done
    while IFS= read -r path; do
        commit="$(git -C "$ROOT" log --diff-filter=A --format='%H' "$history" -- "$path" | tail -1)"
        [ -n "$commit" ] || continue
        [ "$commit" = "$questions_commit" ] && continue
        git -C "$ROOT" merge-base --is-ancestor "$questions_commit" "$commit" || {
            bad=$((bad + 1))
            fail order-of-work "the commit that adds $path does not descend from the commit that adds the question list"
        }
    done < <(git -C "$ROOT" ls-tree -r --name-only "$history" -- docs/research/internet-of-cognition/mas-lab docs/research/internet-of-cognition/triage docs/research/internet-of-cognition/tests docs/research/internet-of-cognition/coordination docs/research/internet-of-cognition/similarity)
    [ "$bad" -eq 0 ] && pass order-of-work "the commit that adds the question list is an ancestor of every commit that adds a file of the five deep-read directories"
}

# ---------------------------------------------------------------- --citations

# paths_at <repo root> <commit> <pathspec>
#
# The paths a citation names, at one commit. A citation writes a path, a directory, a
# glob such as `*.py`, or several of those separated by a comma or by the word "and".
# Each is read here, so a citation of a set of files resolves when the commit holds at
# least one path of every part it names.
paths_at() {
    local root="$1" commit="$2" spec="$3" part regex names
    names="$(git -C "$root" ls-tree -r --name-only "$commit" 2>/dev/null)"
    [ -n "$names" ] || return 1
    while IFS= read -r part; do
        part="$(printf '%s' "$part" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
        [ -n "$part" ] || continue
        part="${part%%:*}"
        part="${part%/}"
        # A part written in words rather than as a path names a set a reader reads, so
        # the gate passes over it and holds the citation to the parts that are paths.
        case "$part" in
        *' '*) continue ;;
        esac
        case "$part" in
        *'*'*)
            regex="$(printf '%s' "$part" | sed 's/[.]/\\./g; s/[*]/[^\/]*/g')"
            case "$part" in
            */*) printf '%s\n' "$names" | grep -qE "^$regex(/|$)" || return 1 ;;
            *) printf '%s\n' "$names" | grep -qE "(^|/)$regex(/|$)" || return 1 ;;
            esac
            ;;
        *)
            printf '%s\n' "$names" | grep -qE "^$(printf '%s' "$part" | sed 's/[^a-zA-Z0-9_/-]/\\&/g')(/|$)" || return 1
            ;;
        esac
    done < <(printf '%s\n' "$spec" | sed 's/ and /\n/g; s/,/\n/g')
    # A citation whose every part is written in words, such as "20 pyproject.toml files
    # under backend/", names a set a reader reads rather than a path, and the gate holds
    # it to no path: every part that is a path has already been read above.
    return 0
}

# flatten_text reads standard input and prints it with every run of space taken out,
# including the spaces a page writes as a no-break space. Both sides of a quotation are
# read this way, because taking the tags out of a page moves where its spaces fall.
flatten_text() {
    python3 -c 'import re,sys; sys.stdout.write(re.sub(r"\s+", "", sys.stdin.read()))'
}

# copy_text <path>
#
# The readable text of a committed copy. A page fetched from a content system is HTML, so
# its sentences are read with the tags taken out and the runs of space collapsed; any
# other copy is read as it stands.
copy_text() {
    case "$1" in
    *.html | *.htm)
        python3 - "$1" <<'PYTHON'
import html, re, sys
raw = open(sys.argv[1], encoding="utf-8", errors="replace").read()
# A script element is kept: a page rendered by a client framework carries its own
# sentences inside one, and that is where a quotation of such a page is found. The
# JSON escapes of that script are read first, so the tags written inside it as
# \\u003cstrong\\u003e are tags here too and come out with the rest.
raw = re.sub(r"\\+u([0-9a-fA-F]{4})", lambda m: chr(int(m.group(1), 16)), raw)
text = html.unescape(re.sub(r"(?s)<[^>]+>", " ", raw))
print(re.sub(r"\s+", " ", text))
PYTHON
        ;;
    *) cat "$1" ;;
    esac
}

# resolve_symbol <id> <repo root> <commit> <pathspec> <symbol>
#
# How a symbol is read, which is a reading of its own and so is written here. A symbol
# ending in a line or a line range resolves when the file holds that line. Otherwise the
# symbol is split on commas and on the word "and", and each part is looked for in the
# paths the citation names: as literal text, as a line, as a file name, as a prefixed
# name, as a path, as a regular expression where the part holds `|` or `*`, and as the
# last element of a dotted name. A part written in words that none of those finds names a
# part of the file a reader judges, and this gate does not read it; nor does it read any
# part of a symbol whose citation writes its file in words. Each such part is written to
# $RUN_SCRATCH/unchecked-parts, which --citations prints, one line per part with its row,
# so that a part nothing checked is never counted as checked. Any other part that no
# search finds fails the citation.
resolve_symbol() {
    local id="$1" root="$2" commit="$3" file="$4" symbol="$5" family="${6:-citation-our-code}" part
    # A symbol that is a line or a line range, written `name.py:88-97` or as the bare
    # range the record writes beside a file, resolves when the file holds that line.
    local line_ref
    line_ref="$(printf '%s' "$symbol" | grep -oE '(^|:)[0-9]+(-[0-9]+)?[^0-9]*$' | grep -oE '[0-9]+' | head -1)"
    if [ -n "$line_ref" ] && [ -z "${file##*[*/]}" ]; then
        line_ref=""
    fi
    if [ -n "$line_ref" ]; then
        if [ "$(git -C "$root" show "$commit:${file%%:*}" 2>/dev/null | wc -l)" -ge "$line_ref" ]; then
            return 0
        fi
        fail "$family" "$id cites $symbol, and $file holds no such line at $commit"
        return 1
    fi
    # A citation may name more than one file. Each part of the symbol is looked for in
    # every path the citation names, because the record writes the two together.
    local pathspec=()
    local fpart
    while IFS= read -r fpart; do
        fpart="$(printf '%s' "$fpart" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
        fpart="${fpart%%:*}"
        fpart="${fpart%/}"
        case "$fpart" in '' | *' '*) continue ;; esac
        # A glob reaches across directories here, as the citation writes it, so git is
        # told to read it as one.
        case "$fpart" in *'*'*) fpart=":(glob)$fpart" ;; esac
        pathspec+=("$fpart")
    done < <(printf '%s\n' "$file" | sed 's/ and /\n/g; s/,/\n/g')
    # A citation whose file is written in words, such as "15 pyproject.toml files", names
    # a set a reader reads rather than a path, and the gate holds its symbol to no file:
    # the paths it does name were already read by paths_at. Each part of its symbol is
    # reported as not checked.
    if [ "${#pathspec[@]}" -eq 0 ]; then
        while IFS= read -r part; do
            part="$(printf '%s' "$part" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
            [ -n "$part" ] && printf '%s\t%s\n' "$id" "$part" >>"$RUN_SCRATCH/unchecked-parts"
        done < <(printf '%s\n' "$symbol" | sed 's/ and /\n/g; s/,/\n/g')
        return 0
    fi
    while IFS= read -r part; do
        part="$(printf '%s' "$part" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')"
        [ -n "$part" ] || continue
        if git -C "$root" grep -qF -- "$part" "$commit" -- "${pathspec[@]}" 2>/dev/null; then
            continue
        fi
        # A part that is a line or a line range, which a citation writes beside the names
        # it lists, resolves when one of the files it names holds that line.
        case "$part" in
        [0-9]*)
            local part_line held=1 path
            part_line="$(printf '%s' "$part" | grep -oE '^[0-9]+')"
            for path in "${pathspec[@]}"; do
                # A glob names many files and no line, so a number beside one is a figure
                # of the prose rather than a line of a file.
                case "$path" in *'*'*) held=0 ;; esac
                [ "$(git -C "$root" show "$commit:$path" 2>/dev/null | wc -l)" -ge "$part_line" ] && held=0
            done
            [ "$held" -eq 0 ] && continue
            ;;
        esac
        # A part that is a file name resolves as a path under the paths the citation names.
        case "$part" in
        *.[A-Za-z]*)
            # ls-tree does not read the glob magic git grep takes, so the whole listing is
            # filtered here, as paths_at filters it.
            git -C "$root" ls-tree -r --name-only "$commit" 2>/dev/null |
                grep -qE "(^|/)$(printf '%s' "$part" | sed 's/[^a-zA-Z0-9_/-]/\\&/g')$" && continue
            ;;
        esac
        # A prefixed name, such as a Turtle term `mas:sessionId`, resolves on its local
        # part too: the prefix names a namespace and the file declares the local name.
        case "$part" in
        *:*)
            git -C "$root" grep -qF -- "${part##*:}" "$commit" -- "${pathspec[@]}" 2>/dev/null && continue
            ;;
        esac
        # A symbol that is itself a path resolves as a path does.
        case "$part" in
        */*)
            paths_at "$root" "$commit" "$part" && continue
            ;;
        esac
        case "$part" in
        *'|'* | *'*'*)
            git -C "$root" grep -qE -- "$part" "$commit" -- "${pathspec[@]}" 2>/dev/null && continue
            ;;
        esac
        case "$part" in
        *' '*)
            # A part of a file named in words, which a reader judges and this gate does
            # not: it is reported as not checked.
            printf '%s\t%s\n' "$id" "$part" >>"$RUN_SCRATCH/unchecked-parts"
            continue
            ;;
        esac
        # A dotted name is read by its last element too, since a citation writes
        # `Envelope.Validate` where the file declares `Validate` on a receiver.
        case "$part" in
        *.*)
            git -C "$root" grep -qF -- "${part##*.}" "$commit" -- "${pathspec[@]}" 2>/dev/null && continue
            ;;
        esac
        fail "$family" "$id cites the symbol $part, which $file does not hold at $commit"
        return 1
    done < <(printf '%s\n' "$symbol" | sed 's/ and /\n/g; s/,/\n/g')
    return 0
}


# Every dispatch:// reference is read by citations.py refs, the one definition of a
# reference and of the punctuation after one that is no part of it: its document, its
# block anchor after `#` and its version after `@`, so a bogus anchor or version is read
# as part of the reference rather than cut off.

# directory_references; every dispatch:// reference of this directory's pages and rows,
# once each. It fails when citations.py could not read them.
directory_references() {
    local files=() list="$RUN_SCRATCH/directory-references"
    mapfile -d '' -t files < <(find "$EVIDENCE" -type f \( -name '*.md' -o -name '*.jsonl' \) -print0)
    [ "${#files[@]}" -gt 0 ] || return 0
    python3 "$EVIDENCE/citations.py" refs --input "${files[@]}" >"$list" || return 1
    sort -u "$list"
}

# false_references
#
# A reference a row of claims.jsonl records as false is a reference the record under check
# wrote and Dispatch does not hold. It is that row's evidence rather than a citation of
# this directory, so --citations reads it there and not here. It fails when citations.py
# could not read them.
false_references() {
    local list="$RUN_SCRATCH/false-references"
    claims | jq -r 'select(.mark=="false") | [.claim, (.evidence|tostring), (.note // "")] | join(" ")' |
        python3 "$EVIDENCE/citations.py" refs --input - >"$list" || return 1
    sort -u "$list"
}

# dispatch_ref_read <ref>; asks Dispatch for the whole reference, its block anchor and its
# version included, and prints what dispatch prints. dispatch read --ref reads a block and
# refuses a block it does not hold. It refuses a version of a whole document, which
# dispatch doc-read reads instead, and the shorthand KEY/spec@vN these pages write is the
# version N of the issue's primary document. mode_citations runs it through live_read, so
# a reference Dispatch answers it does not hold fails citation-dispatch, and a Dispatch
# that does not answer ends the mode, exit 2, or exit 1 after a FAIL, by the precedence of
# the header.
dispatch_ref_read() {
    local ref="$1" rest
    case "$ref" in
    *'#'*) dispatch read --ref "$ref" ;;
    dispatch://*/spec@v*)
        rest="${ref#dispatch://}"
        dispatch doc-read --issue "${rest%%/*}" --version "${ref##*@v}"
        ;;
    *@v*) dispatch doc-read --ref "$ref" ;;
    *) dispatch read --ref "$ref" ;;
    esac
}


# report_live_pages
#
# What the Verification document asks of this mode beside the assertions: "where the live
# address answers, reports whether the live page still carries that sentence; a live page
# whose bytes have changed is reported as changed rather than failed, because a page
# served from a content system changes its bytes between any two fetches."
#
# So this is a report and no assertion: one line per published copy, naming its address
# and one of three readings. An address that does not answer is reported too, so the mode
# runs with no network, and a changed page fails nothing.
report_live_pages() {
    local address copy sentence live flat
    printf 'The live address of each published copy, read now. This is a report and no assertion: a page served from a content system changes its bytes between any two fetches.\n'
    while IFS=$'\t' read -r address copy; do
        [ -n "$address" ] && [ -n "$copy" ] || continue
        case "$copy" in */) continue ;; esac
        live="$RUN_SCRATCH/live-$(printf '%s' "$copy" | tr '/' '_')"
        if ! curl -sS -L --max-time 30 --max-filesize 20000000 -o "$live" "$address" 2>/dev/null || [ ! -s "$live" ]; then
            printf '  %s: unreachable, the address did not answer; the committed copy %s is what this reading rests on.\n' "$address" "$copy"
            continue
        fi
        if cmp -s "$live" "$EVIDENCE/$copy"; then
            printf '  %s: answered, and its bytes are the committed copy %s byte for byte.\n' "$address" "$copy"
            continue
        fi
        # The sentences this directory quotes from that copy, which is what a reader of a
        # changed page wants to know about.
        local missing=0 held=0
        while IFS= read -r sentence; do
            [ -n "$sentence" ] || continue
            flat="$(printf '%s' "$sentence" | flatten_text)"
            if copy_text "$live" | flatten_text | grep -qF -- "$flat"; then
                held=$((held + 1))
            else
                missing=$((missing + 1))
            fi
        done < <(claims | jq -r --arg c "$copy" 'select(.kind=="published") | select((.evidence.copy // "")==$c) | .evidence.quote // empty')
        if [ "$missing" -eq 0 ] && [ "$held" -eq 0 ]; then
            printf '  %s: answered and changed since it was fetched, and this reading quotes no sentence of it; the committed copy %s is what it rests on.\n' "$address" "$copy"
        elif [ "$missing" -eq 0 ]; then
            printf '  %s: answered and changed since it was fetched, and it still carries each of the %s sentences this reading quotes from %s.\n' "$address" "$held" "$copy"
        else
            printf '  %s: answered and changed since it was fetched, and it no longer carries %s of the %s sentences this reading quotes from %s, which the committed copy holds.\n' "$address" "$missing" "$((held + missing))" "$copy"
        fi
    done < <(jq -r '.published[] | select(.copy) | [.address, .copy] | @tsv' "$EVIDENCE/sources.json")
}

# report_unchecked_parts
#
# The symbol parts resolve_symbol did not read: a part written in words, as the name of a
# part of a file, that neither the literal search nor any other reading found, and every
# part of a symbol whose citation writes its file in words. This is a report and no
# assertion, one line per part with the row that cites it, and a count, so that a reader
# sees how much of the symbol column the citation assertions leave to them.
report_unchecked_parts() {
    local list="$RUN_SCRATCH/unchecked-parts" id part
    if [ ! -s "$list" ]; then
        printf 'No symbol part was left unchecked: every part is an identifier, a line, a prefixed name or a file name, or its text stands in the file.\n'
        return
    fi
    printf 'The symbol parts no search of the cited files read: a part written in words that no search found, or a part of a citation whose file is written in words. These are not checked: each names a part of a file, or of a set of files, a reader judges.\n'
    while IFS=$'\t' read -r id part; do
        printf '  not checked: %s cites the part "%s"\n' "$id" "$part"
    done <"$list"
    printf '%d symbol parts were not checked.\n' "$(wc -l <"$list" | tr -d ' ')"
}

mode_citations() {
    local commit bad_ours=0 bad_theirs=0 bad_published=0 bad_dispatch=0 bad_kinds=0 row id file symbol repo rcommit
    # The one parse of claims.jsonl comes first: a line that is no row fails citation-kinds,
    # and every read below reads only the rows it read.
    claims_judge citation-kinds || bad_kinds=1
    # The flattened text of each committed copy, read once: eight rows cite one page of
    # 400 KB, and reading it per row is the cost of the mode.
    local flat_dir="$RUN_SCRATCH/citations"
    mkdir -p "$flat_dir"
    commit="$(our_commit)"
    # The live inputs, read through live_read before any assertion over a citation judges:
    # the clone of each repository a project-code row cites, and Dispatch's answer to each
    # dispatch:// reference of this directory. A source that does not answer ends the mode,
    # exit 2, or exit 1 after a FAIL, by the precedence of the header, as after the one
    # parse's FAIL of citation-kinds above; a reference Dispatch answers it does not hold is
    # kept for citation-dispatch to fail.
    while IFS= read -r repo; do
        [ -n "$repo" ] && { clone_fetch "$repo" || true; }
    done < <(claims | jq -r 'select(.kind=="project-code") | .evidence.repo // empty' | sort -u)
    local ref refs="$RUN_SCRATCH/references" falses="$RUN_SCRATCH/false-reference-list"
    local unresolved="$RUN_SCRATCH/unresolved-references" refs_read=1 asked=0
    : >"$unresolved"
    if directory_references >"$refs" && false_references >"$falses"; then
        while IFS= read -r ref; do
            asked=$((asked + 1))
            live_read "Dispatch, the read of $ref" "$RUN_SCRATCH/reference.out" dispatch_ref_read "$ref" ||
                printf '%s\t%s\n' "$ref" "$(cat "$RUN_SCRATCH/reference.out" "$RUN_SCRATCH/reference.out.err" | head -1 | head -c 160)" >>"$unresolved"
        done < <(grep -vxF -f "$falses" "$refs")
    else
        refs_read=0
    fi
    while IFS= read -r row; do
        id="$(printf '%s' "$row" | jq -r '.id')"
        case "$(printf '%s' "$row" | jq -r '.kind')" in
        our-code)
            file="$(printf '%s' "$row" | jq -r '.evidence.file // ""')"
            symbol="$(printf '%s' "$row" | jq -r '.evidence.symbol // ""')"
            file="${file%%:*}"
            # A path, a directory or a glob: git reads all three as a pathspec, and the
            # citation resolves when the commit holds at least one path it names. A file
            # this pull request adds is read at the head instead, since the base has none.
            if [ -z "$file" ] || ! paths_at "$ROOT" "$commit" "$file"; then
                if [ -n "$file" ] && paths_at "$ROOT" HEAD "$file"; then
                    :
                else
                    bad_ours=$((bad_ours + 1))
                    fail citation-our-code "$id cites $file, which this repository does not hold at $commit"
                    continue
                fi
            fi
            if [ -n "$symbol" ] && [ "$symbol" != "null" ]; then
                resolve_symbol "$id" "$ROOT" "$commit" "$file" "$symbol" || bad_ours=$((bad_ours + 1))
            fi
            ;;
        project-code)
            repo="$(printf '%s' "$row" | jq -r '.evidence.repo // ""')"
            rcommit="$(printf '%s' "$row" | jq -r '.evidence.commit // ""')"
            file="$(printf '%s' "$row" | jq -r '.evidence.file // ""')"
            file="${file%%:*}"
            local root
            if ! jq -e --arg n "${repo##*/}" '.repositories[] | select(.name==$n) | .url and .commit' "$EVIDENCE/sources.json" >/dev/null 2>&1; then
                bad_theirs=$((bad_theirs + 1))
                fail citation-project-code "$id cites the repository $repo, which sources.json pins no clone of"
                continue
            fi
            root="$(clone_dir "$repo")"
            if ! paths_at "$root" "$rcommit" "$file"; then
                bad_theirs=$((bad_theirs + 1))
                fail citation-project-code "$id cites $repo $file at $rcommit, which the clone does not hold"
            else
                symbol="$(printf '%s' "$row" | jq -r '.evidence.symbol // ""')"
                if [ -n "$symbol" ] && [ "$symbol" != "null" ]; then
                    resolve_symbol "$id" "$root" "$rcommit" "$file" "$symbol" citation-project-code ||
                        bad_theirs=$((bad_theirs + 1))
                fi
            fi
            ;;
        published)
            local copy address sentence
            copy="$(printf '%s' "$row" | jq -r '.evidence.copy // ""')"
            address="$(printf '%s' "$row" | jq -r '.evidence.address // ""')"
            sentence="$(printf '%s' "$row" | jq -r '.evidence.quote // ""')"
            [ -n "$copy" ] || copy="$(jq -r --arg a "$address" '.published[] | select(.address==$a) | .copy' "$EVIDENCE/sources.json")"
            if [ -z "$copy" ] || [ ! -s "$EVIDENCE/$copy" ]; then
                bad_published=$((bad_published + 1))
                fail citation-published "$id cites $address and names no committed copy this directory holds"
                continue
            fi
            # The checksum sources.json recorded of that copy, against the copy as it
            # stands: a page is evidence of what it said when it was fetched, so a copy
            # whose bytes moved since is no longer the page the claim was read at.
            local recorded_sum read_sum
            recorded_sum="$(jq -r --arg c "$copy" '.published[] | select(.copy==$c) | .sha256 // empty' "$EVIDENCE/sources.json")"
            if [ -z "$recorded_sum" ]; then
                bad_published=$((bad_published + 1))
                fail citation-published "$id cites the copy $copy, and sources.json records no checksum of it"
                continue
            fi
            read_sum="$(sha256sum "$EVIDENCE/$copy" | cut -d' ' -f1)"
            if [ "$read_sum" != "$recorded_sum" ]; then
                bad_published=$((bad_published + 1))
                fail citation-published "the copy $copy hashes $read_sum, and sources.json records $recorded_sum"
                continue
            fi
            # The sentence is looked for with the spaces taken out of both sides, because
            # taking the tags out of a page moves where its spaces fall.
            if [ -n "$sentence" ]; then
                local flat="$flat_dir/$(printf '%s' "$copy" | tr '/' '_')"
                [ -s "$flat" ] || copy_text "$EVIDENCE/$copy" | flatten_text >"$flat"
                if ! grep -qF -- "$(printf '%s' "$sentence" | flatten_text)" "$flat"; then
                    bad_published=$((bad_published + 1))
                    fail citation-published "$id quotes a sentence the committed copy $copy does not hold"
                fi
            fi
            ;;
        esac
    done < <(claims)
    # None of these passes on no input: a kind of citation claims.jsonl holds no row of,
    # and a directory that names no dispatch:// reference, fail by name.
    local kind count
    for kind in our-code project-code published; do
        count="$(claims | jq -c --arg k "$kind" 'select(.kind==$k)' | wc -l)"
        [ "$count" -gt 0 ] && continue
        case "$kind" in
        our-code) bad_ours=$((bad_ours + 1)) ;;
        project-code) bad_theirs=$((bad_theirs + 1)) ;;
        published) bad_published=$((bad_published + 1)) ;;
        esac
        fail "citation-$kind" "claims.jsonl holds no $kind row, so no such citation is checked"
    done
    [ "$bad_ours" -eq 0 ] && pass citation-our-code "every citation into our own repository names a path that stands at $commit, and every symbol part that is an identifier, a line, a prefixed name or a file name stands in it"
    [ "$bad_theirs" -eq 0 ] && pass citation-project-code "every citation into one of the projects names a path the clone holds at its pinned commit, and every symbol part that is an identifier, a line, a prefixed name or a file name stands in it"
    [ "$bad_published" -eq 0 ] && pass citation-published "every citation of a published document names a committed copy that holds the sentence quoted"

    report_unchecked_parts
    report_live_pages

    if [ "$refs_read" -eq 0 ]; then
        bad_dispatch=$((bad_dispatch + 1))
        fail citation-dispatch "citations.py could not read the dispatch:// references of this directory"
    elif [ "$asked" -eq 0 ]; then
        bad_dispatch=$((bad_dispatch + 1))
        fail citation-dispatch "this directory names no dispatch:// reference, so none is resolved"
    fi
    local answer
    while IFS=$'\t' read -r ref answer; do
        bad_dispatch=$((bad_dispatch + 1))
        fail citation-dispatch "the reference $ref resolves to no node Dispatch holds: Dispatch answered '$answer'"
    done <"$unresolved"
    [ "$bad_dispatch" -eq 0 ] && pass citation-dispatch "each of the $asked dispatch:// references of this directory resolves to a node Dispatch holds"

    if ! python3 "$EVIDENCE/citations.py" --self-test >/dev/null 2>&1; then
        fail citation-kinds "citations.py --self-test does not pass, so the extraction rule reads less than it names"
    elif [ "$bad_kinds" -eq 0 ]; then
        pass citation-kinds "every line of claims.jsonl is a row the one parse reads, and the extraction rule reads a citation of every kind it names, a number written in words among them"
    fi
}

# ---------------------------------------------------------------- --landed

mode_landed() {
    local bad_proof=0 bad_refutation=0 page title commit command landed=0 history entry want_commit want_command named
    local read_pages=0 found="$RUN_SCRATCH/landed-titles"
    history="$(history_ref)"
    judgement_load
    : >"$found"
    while IFS= read -r -d $'\f' page; do
        read_pages=$((read_pages + 1))
        title="$(printf '%s\n' "$page" | head -1 | sed 's/^### //')"
        printf '%s\n' "$page" | grep -qE '\*\*Landed:\*\*' || continue
        landed=$((landed + 1))
        printf '%s\n' "$title" >>"$found"
        if printf '%s\n' "$page" | grep -qE '\*\*Landed:\*\*[[:space:]]*not made'; then
            # A step that is not made carries evidence in one of the five forms: a command
            # with its output, never a search that found nothing.
            printf '%s\n' "$page" | grep -qE '\*\*Refuted by:\*\*.*`.*`' || {
                bad_refutation=$((bad_refutation + 1))
                fail landed-refutation "the page '$title' says its step is not made and carries no command with output"
            }
            continue
        fi
        # The proof is the one this tree commits for the page's title, never the text of
        # the live page, which no reviewer of this pull request reads: the page must name
        # the committed commit and the committed command, and only the committed command
        # runs.
        entry="$(jq -c --arg t "$title" '[.landed // [] | .[] | select(.title == $t)] | first // empty' "$EVIDENCE/sources.json")"
        if [ -z "$entry" ]; then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the page '$title' carries a Landed line, and the landed list of sources.json commits no proof for that title"
            continue
        fi
        want_commit="$(printf '%s' "$entry" | jq -r '.commit // ""')"
        want_command="$(printf '%s' "$entry" | jq -r '.command // ""')"
        commit="$(printf '%s\n' "$page" | grep -m1 -oE '\*\*Landed:\*\*[[:space:]]*[0-9a-f]{7,40}' | grep -oE '[0-9a-f]{7,40}')"
        # The page may write the commit abbreviated; it names the committed one when git
        # reads the two as the same commit.
        named="$([ -n "$commit" ] && git -C "$ROOT" rev-parse --verify --quiet "$commit^{commit}" 2>/dev/null)"
        if [ -z "$named" ] || [ "$named" != "$(git -C "$ROOT" rev-parse --verify --quiet "$want_commit^{commit}" 2>/dev/null)" ]; then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the page '$title' names the commit '$commit', and sources.json commits '$want_commit' as the commit of that step"
            continue
        fi
        # A commit of this pull request is one this branch added (pr_commits): a commit of
        # the base branch, whether before the fork or brought in by a forward merge, is on
        # main and lands nothing here, so naming one is no proof of a step.
        if ! pr_commits | grep -qx "$named"; then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the page '$title' names the commit '$commit', which this pull request did not add: it is not on the pull request's own line of $history, past $(our_commit) and the base commits its forward merges brought in"
            continue
        fi
        command="$(printf '%s\n' "$page" | grep -m1 -oE '\*\*Proven by:\*\*[[:space:]]*`[^`]+`' | sed 's/.*`\(.*\)`/\1/')"
        if [ -z "$want_command" ] || [ "$command" != "$want_command" ]; then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the page '$title' names the proof command '$command', and sources.json commits '$want_command' as the proof of that step"
            continue
        fi
        if ! (cd "$ROOT" && bash -c "$want_command" >/dev/null 2>&1); then
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the committed proof of the page '$title', '$want_command', exits non-zero"
        fi
    done < <(page_blocks)
    # The expected set is committed: each step sources.json's landed list names is held to
    # a page of the judgement that carries its Landed line, so a judgement that lost a
    # Landed line, or a page, or every page, is no pass. A list with no step is no pass
    # either.
    local steps
    steps="$(jq -r '.landed // [] | .[].title' "$EVIDENCE/sources.json")"
    if [ -z "$steps" ]; then
        bad_proof=$((bad_proof + 1))
        fail landed-proof "sources.json's landed list names no step, so no landed step is held to a page"
    fi
    while IFS= read -r title; do
        [ -n "$title" ] || continue
        grep -qxF -- "$title" "$found" || {
            bad_proof=$((bad_proof + 1))
            fail landed-proof "the step '$title', which sources.json's landed list commits, has no page of the judgement that carries its Landed line"
        }
    done <<<"$steps"
    if [ "$read_pages" -lt "$PAGE_FLOOR" ]; then
        bad_refutation=$((bad_refutation + 1))
        fail landed-refutation "the judgement carries $read_pages candidate pages, fewer than the floor of $PAGE_FLOOR, so no page's refutation is judged"
    fi
    [ "$bad_proof" -eq 0 ] && pass landed-proof "each of the $(printf '%s\n' "$steps" | grep -c .) steps of sources.json's landed list has a page that carries its Landed line, each of the $landed landed pages names the commit and the command sources.json commits for it, the commit is one this pull request added, and the committed command passes"
    [ "$bad_refutation" -eq 0 ] && pass landed-refutation "each of the $read_pages candidate pages that says its step is not made carries a command with output"

    landed_order
    landed_no_refusal
    landed_third_change
}

landed_commit_of() {
    # shellcheck disable=SC2046
    git -C "$ROOT" log --no-walk --format='%H %s' $(pr_commits) | grep -m1 -F "$1" | cut -d' ' -f1
}

landed_order() {
    local rows rule
    rows="$(landed_commit_of 'two graders are recorded as two rows')"
    rule="$(landed_commit_of 'combines the reviewer')"
    if [ -z "$rows" ] || [ -z "$rule" ]; then
        fail landed-order "the two changes this issue commits to are not both in this pull request (rows='$rows' rule='$rule')"
        return
    fi
    if git -C "$ROOT" merge-base --is-ancestor "$rows" "$rule"; then
        pass landed-order "the commit of the shortfall rows is an ancestor of the commit of the combining rule"
    else
        fail landed-order "the commit of the shortfall rows is not an ancestor of the commit of the combining rule"
    fi
}

landed_no_refusal() {
    local touched
    touched="$(git -C "$ROOT" diff --name-only "$(pr_base)" "$(history_ref)" -- packages/daemon/internal/api packages/daemon/internal/workflow packages/envoy/internal/dispatch/api)"
    if [ -n "$touched" ]; then
        fail landed-no-refusal "a landed step touches a refusal path: $(printf '%s' "$touched" | tr '\n' ' ')"
        return
    fi
    local added
    added="$(git -C "$ROOT" diff "$(pr_base)" "$(history_ref)" -- '*.go' '*.ts' '*.sql' | grep -E '^\+' | grep -cE '"[A-Z][A-Z_]{4,}"' || true)"
    if [ "${added:-0}" -eq 0 ]; then
        pass landed-no-refusal "no landed step touches a refusal path of the daemon or of Dispatch, and none adds a refusal constant"
    else
        fail landed-no-refusal "a landed step adds what reads as a refusal constant ($added lines)"
    fi
}

landed_third_change() {
    # The third change the earlier reading proposed is a page with a cost and a default,
    # and the gate reads it by name rather than by the diff rule, because it could be
    # landed as prose in a skill and the diff rule alone would pass it.
    local hits
    hits="$(git -C "$ROOT" diff "$(pr_base)" "$(history_ref)" -- skills packages/daemon/internal/prompts |
        grep -E '^\+' | grep -ciE 'refus(e|ed|es|al).{0,80}(reviewer|review) completion|completion .{0,40}no pair result|REVIEWER_PAIR_REQUIRED' || true)"
    if [ "${hits:-0}" -eq 0 ]; then
        pass landed-third-change "the third change, a refusal of a reviewer completion with no pair result, is landed in no form"
    else
        fail landed-third-change "the third change is landed in this tree in $hits place(s), and it is a page with a cost and a default"
    fi
}

# ---------------------------------------------------------------- --published

mode_published() {
    local remote head work="$RUN_SCRATCH/published-mode"
    remote="$(jq -r '.our_repository.url' "$EVIDENCE/sources.json")"
    mkdir -p "$work"
    # The live inputs, read through live_read before any assertion judges: the three
    # Dispatch reads the delivery is judged by, then the default branch from GitHub,
    # fetched into a clone of this run's scratch and never into this repository's own
    # object store. A source that does not answer ends the mode, exit 2.
    live_read "Dispatch, the read of the comments of INFRA-3134" "$work/comments" dispatch_get '/api/v1/issues/INFRA-3134/comments?limit=200'
    live_read "Dispatch, the read of the messages of INFRA-3134" "$work/messages" dispatch_get '/api/v1/issues/INFRA-3134/messages?limit=200'
    live_read "Dispatch, the read of the comments of INFRA-1098" "$work/upstream" dispatch_get '/api/v1/issues/INFRA-1098/comments?limit=200'
    head="${IOC_PUBLISHED_COMMIT:-}"
    if [ -z "$head" ]; then
        live_read "GitHub, the read of the default branch of $remote" "$work/ls-remote" git ls-remote "$remote" refs/heads/main
        head="$(cut -f1 "$work/ls-remote")"
    fi
    local clone="$work/clone" missing=0 path
    if [ -n "$head" ]; then
        git init --quiet "$clone" 2>/dev/null || cannot_run environment " --published: git init of the scratch clone failed."
        live_read "GitHub, the fetch of $head from $remote" "$work/fetch.log" git -C "$clone" fetch --quiet --depth 1 --no-tags "$remote" "$head"
    fi
    if [ -z "$head" ]; then
        missing=$((missing + 1))
        fail published-tree "GitHub answered that $remote has no default branch main, so nothing of this directory is published"
    else
        for path in questions.md check.sh citations.py sources.json claims.jsonl README.md interview/transcript.md; do
            git -C "$clone" cat-file -e "$head:docs/research/internet-of-cognition/$path" 2>/dev/null || {
                missing=$((missing + 1))
                fail published-tree "the default branch at $head does not carry docs/research/internet-of-cognition/$path"
            }
        done
        # The files each landed step changed stand there as the step left them, each read
        # by the sentences its own step added and not by a word the file carried before.
        # Each landed entry of sources.json lists them as its `published` pairs, so a
        # change to a rule edits that one list; a list with no step is no pass.
        local title sentence
        [ "$(jq '.landed // [] | length' "$EVIDENCE/sources.json")" -gt 0 ] || {
            missing=$((missing + 1))
            fail published-tree "sources.json's landed list names no step, so no landed step is read on the default branch"
        }
        while IFS= read -r title; do
            missing=$((missing + 1))
            fail published-tree "the landed step '$title' lists no sentence in sources.json, so nothing of it is read on the default branch"
        done < <(jq -r '.landed[]? | select((.published // []) | length == 0) | .title' "$EVIDENCE/sources.json")
        while IFS=$'\t' read -r path sentence; do
            git -C "$clone" show "$head:$path" >"$work/published" 2>/dev/null || {
                missing=$((missing + 1))
                fail published-tree "the default branch at $head does not carry $path"
                continue
            }
            grep -qF -- "$sentence" "$work/published" || {
                missing=$((missing + 1))
                fail published-tree "the published $path does not carry the landed step: '$sentence'"
            }
        done < <(jq -r '.landed[]? | .published[]? | [.path, .sentence] | @tsv' "$EVIDENCE/sources.json")
    fi
    [ "$missing" -eq 0 ] && pass published-tree "the default branch at $head carries the evidence, the transcript, this script and the files each landed step changed"

    local delivery=0
    grep -qF 'notifications.role.legion-infra-controller' "$work/comments" || {
        delivery=$((delivery + 1))
        fail published-delivery "no comment of this issue records the publication of the ranked list to the project's controller"
    }
    grep -qiF 'ranking' "$work/messages" || {
        delivery=$((delivery + 1))
        fail published-delivery "no message on this issue carries the ranking to the issue's assignee"
    }
    grep -qF 'INFRA-3134' "$work/upstream" || {
        delivery=$((delivery + 1))
        fail published-delivery "no comment of dispatch://INFRA-1098 carries this issue's reading of the node model"
    }
    [ "$delivery" -eq 0 ] && pass published-delivery "the record of the delivery is on the issue: the message, the comment on dispatch://INFRA-1098 and the comment recording the publication"
    printf 'This mode reads the record of the publication to the controller, which is the comment above, and not the publication itself: an Envoy publication leaves no record a program can read.\n'
}

# ---------------------------------------------------------------- --self-test

# The self-test builds one small deliverable of its own under its run's scratch, proves the
# gate passes over it, then plants, one at a time, a bad case of every kind the gate reads
# and proves the gate fails and names the family. The planted list and the assertion table
# are one list: the table's fourth column is the fault, and a family whose fault is
# unplanted fails here. Each run of the gate over the fixture runs in the environment the
# header's "The fixture of --self-test" states.

fixture_build() {
    local dir="$1"
    local evidence="$dir/repo/docs/research/internet-of-cognition"
    mkdir -p "$evidence"/{mas-lab,triage,interview,tests,coordination,similarity,published,evidence}
    mkdir -p "$dir/repo/skills/legion-worker/references"
    mkdir -p "$dir/repo/packages/daemon/internal/api"
    cp "$EVIDENCE/citations.py" "$evidence/citations.py"
    cp "$EVIDENCE/check.sh" "$evidence/check.sh"
    # env.sh, whose functions the fixture's modes read: --citations clones a repository by
    # its rule, and --replay exports the fixture's tool caches by its table (prepare_env).
    cp "$EVIDENCE/env.sh" "$evidence/env.sh"
    # The hashed locks, which a planted install may read, and ioc-venv.in, which is none.
    cp "$EVIDENCE/evidence/ioc-venv.lock" "$EVIDENCE/evidence/ioc-venv.in" "$EVIDENCE/evidence/tm-constraints.txt" \
        "$EVIDENCE/evidence/build-constraints.txt" "$evidence/evidence/"

    cat >"$dir/repo/packages/daemon/internal/api/handoff.go" <<'GO'
package api

const TesterVerdictRequired = "TESTER_VERDICT_REQUIRED"
GO
    cat >"$dir/repo/skills/legion-worker/references/pr-body.md" <<'MD'
**Thermo:** the pair at the reviewed head, one row per half:
- `thermonuclear-deep-review`: <verdict>; <n> findings standing.
- `thermonuclear-code-quality`: <verdict>; <n> findings standing.
MD
    cat >"$dir/repo/skills/legion-worker/references/merge-gate.md" <<'MD'
The two halves of the pair decide one review between them: a `CHANGES` from either
`thermonuclear-deep-review` or `thermonuclear-code-quality` is a `REQUEST_CHANGES`.
MD

    cat >"$evidence/questions.md" <<'MD'
# The questions this reading answers

## 1. When a role writes a fact it read somewhere else, what carries the way back?

Raised by `packages/daemon/internal/api/handoff.go`.

## 2. What decides the round when the two graders disagree?

Raised by the merge gate.

## 3. A question no page answers.

Raised by nothing in particular.
MD

    local directory
    for directory in library-standard ctl runtime library-eval library-telemetry library-kg; do
        printf '# %s at COMMIT\n\nWhat it is [f-project-1].\n' "$directory" >"$evidence/mas-lab/$directory.md"
    done
    cat >>"$evidence/mas-lab/ctl.md" <<'MD'

## Question 1: can its specification format express Legion's phase workflow?

No, and the evidence is [f-project-1].
MD
    cat >>"$evidence/mas-lab/library-eval.md" <<'MD'

## Question 2: could its experiment harness measure whether a change to a role's prompt or skill made Legion better?

Not as it stands [f-project-1].
MD
    cat >>"$evidence/mas-lab/runtime.md" <<'MD'

## Question 3: what its boundary enforcement does that our refusals do not

It changes a call rather than refusing it [f-project-1].
MD
    for name in mas-lab scope cognition-protocols ioc-project.github.io dot-github; do
        printf '# %s\n\nTriaged [f-project-1].\n' "$name" >"$evidence/triage/$name.md"
    done
    printf '# The transcript\n\nHe said the third protocol is not released.\n' >"$evidence/interview/transcript.md"
    printf '# The rows\n\nThe three tiers [i-three-tiers].\n' >"$evidence/interview/claims.md"
    printf 'Input: dispatch://INFRA-3134\n\nIt ran [f-behaviour-1].\n' >"$evidence/tests/concord.md"
    printf 'Input: 9542c6fbbecd184ccee73523ab3defa39d6cf23a\n\nIt ran [f-behaviour-1].\n' >"$evidence/tests/scope.md"
    printf 'Input: dispatch://INFRA-3134\n\nIt ran [f-behaviour-1].\n' >"$evidence/tests/mas-lab.md"
    printf '{"from":"2026-10-29T00:00:00Z","to":"2026-11-12T00:00:00Z","project":"INFRA"}\n' >"$evidence/coordination/window.json"
    cat >"$evidence/coordination/count.md" <<'MD'
# The count

| class | instances | denominator |
| --- | --- | --- |
| across sessions | 10 | 100 |
| between an agent and a person | 5 | 100 |
| inside one tree | 20 | 100 |

Each class carries its denominator, the records read in the window.
MD
    # The fixture's walker of the count, which --replay runs again through live_read
    # (count-reruns): it prints one fixed JSON object, the count of count.md above, and
    # reads nothing, neither the network nor window.json, so no planted fault but its own
    # moves what it prints. evidence/count.json is what it prints, written as the
    # committed count is, by running it.
    cat >"$evidence/coordination/count.py" <<'PY'
import json

print(json.dumps({
    "window": {"from": "2026-10-29T00:00:00Z", "to": "2026-11-12T00:00:00Z", "project": "INFRA"},
    "feed": {"events_read": 1000, "first_event_id_in_window": 101, "last_event_id_in_window": 900},
    "denominator_records_read": 100,
    "instances": 40,
    "classes": {"between_an_agent_and_a_person": 5, "inside_one_tree": 20, "across_sessions": 10},
    "unclassed": {"no_cited_record": 5, "cited_record_not_held": 0, "cited_author_unknown": 0, "other": 0},
}, indent=1))
PY
    python3 "$evidence/coordination/count.py" >"$evidence/evidence/count.json"
    printf '# The table\n\nNine pairs [f-behaviour-1].\n' >"$evidence/similarity/table.md"
    printf 'the quoted sentence of a published page\n' >"$evidence/published/page.txt"
    printf '# The fixture\n' >"$evidence/README.md"
    mkdir -p "$evidence/records"
    cat >"$evidence/records/under-check.md" <<'MD'
# The fixture record under check

It claims something about `packages/daemon/internal/api/handoff.go`, and it reports
thirteen normative invariants.
MD

    cat >"$evidence/claims.jsonl" <<'JSONL'
{"id":"f-project-1","requirement":"r3","source":"mas-lab","kind":"project-code","claim":"The runtime declares a boundary gate.","mark":"verified","evidence":{"repo":"mas-lab","commit":"COMMIT","file":"README.md","symbol":"README"},"note":"read again"}
{"id":"f-our-1","requirement":"r1","source":"this tree's measurement","kind":"our-code","claim":"The completion refusal names a tester verdict.","mark":"verified","evidence":{"file":"packages/daemon/internal/api/handoff.go","symbol":"TesterVerdictRequired","commit":"OURS"},"note":"read again","checks":[{"row":"u-1","mark":"verified"}]}
{"id":"f-behaviour-1","requirement":"r7","source":"this tree's measurement","kind":"behaviour","claim":"The script prints its own name.","mark":"verified","evidence":{"surface":"cli"},"command":"printf 'ran once\\n'","expect":{"contains":"ran once"},"output":"ran once","note":"ran again"}
{"id":"f-absence-1","requirement":"r5","source":"this tree's measurement","kind":"absence","claim":"Nothing names a pair refusal.","mark":"verified","evidence":{"scope":"the fixture tree"},"command":"grep -r 'REVIEWER_PAIR_REQUIRED' skills || true","expect":{"at_most":"0"},"output":"","note":"searched again"}
{"id":"f-quotation-1","requirement":"r2","source":"the interview","kind":"quotation","claim":"The authors say the third protocol is not released.","mark":"unreleased","evidence":{"source":"the interview","quote":"the third protocol is not released","marked":"a claim of the project, not of its code"},"note":"their words"}
{"id":"f-published-1","requirement":"r2","source":"a published page","kind":"published","claim":"The page carries the sentence.","mark":"supported","evidence":{"address":"https://example.org/page","fetched":"2026-11-12","section":"body","quote":"the quoted sentence of a published page","copy":"published/page.txt"},"note":"read in the committed copy"}
{"id":"i-three-tiers","requirement":"r2","source":"the interview","kind":"quotation","claim":"The three tiers exist and the third is unreleased.","mark":"unreleased","evidence":{"source":"the interview","quote":"the third protocol is not released","marked":"a claim of the project, not of its code"},"note":"their words"}
{"id":"i-scope-triggers","requirement":"r2","source":"the interview","kind":"absence","claim":"Nothing triggers the protocols.","mark":"unsupported","evidence":{"scope":"the fixture tree"},"command":"grep -r 'trigger-accord' skills || true","expect":{"at_most":"0"},"output":"","note":"searched again"}
{"id":"i-skills-drop-in","requirement":"r2","source":"the interview","kind":"project-code","claim":"The protocols ship as skills.","mark":"supported","evidence":{"repo":"mas-lab","commit":"COMMIT","file":"README.md","symbol":"README"},"note":"read again"}
{"id":"i-measure-one","requirement":"r2","source":"the interview","kind":"quotation","claim":"One misalignment took accuracy from 99% to 98%.","mark":"not-externally-checkable","evidence":{"source":"the interview","quote":"the third protocol is not released","marked":"a claim of the project, not of its code"},"note":"no published document of theirs repeats it"}
{"id":"u-1","requirement":"r1","source":"the fixture record","kind":"our-code","claim":"The record cites the completion refusal file.","mark":"verified","evidence":{"file":"packages/daemon/internal/api/handoff.go","symbol":"TesterVerdictRequired","commit":"OURS"},"note":"read again at the commit"}
{"id":"u-2","requirement":"r1","source":"the fixture record","kind":"project-code","claim":"The record counts thirteen normative invariants.","mark":"verified","evidence":{"repo":"mas-lab","commit":"COMMIT","file":"README.md","symbol":"README"},"note":"counted again"}
JSONL

    cat >"$evidence/sources.json" <<JSON
{
  "clone_root_env": "IOC_CLONES",
  "repositories": [
    {"name": "mas-lab", "url": "https://github.com/ioc-project/mas-lab", "commit": "COMMIT"}
  ],
  "our_repository": {"name": "legion", "url": "$dir/repo", "commit": "OURS"},
  "records_under_check": [
    {"ref": "the fixture record", "label": "u", "copy": "records/under-check.md"}
  ],
  "published": [
    {"id": "page", "address": "https://example.org/page", "fetched": "2026-11-12", "copy": "published/page.txt", "sha256": "PAGE_SHA"}
  ],
  "landed": [
    {"title": "An execution record under the gen_ai. names", "commit": "LANDED_ROWS", "command": "true"}
  ]
}
JSON

    cat >"$dir/judgement.md" <<'MD'
# The fixture judgement

## The ranking

1. Structured metadata on an Envoy message — port — 2 engineer-days.

## The one thing to do first

The Envoy field.

## Where the authors' account and the checked reading differ

The account describes the problem and the reading describes the artifacts.

## What was deliberately not read

- mas-lab: read in part.
- scope: read in part.
- cognition-protocols: read in part.
- ioc-project.github.io: the documentation site, read only where a claim cites a page.
- dot-github: the organisation profile page, which holds no code.
- Question 3 is answered by no page: it is named here, and nothing of theirs answers it.

## Candidate pages

### Structured metadata on an Envoy message

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One optional field, `quotes: [{ref, token}]`.
- **Verdict:** port
- **Landing component:** Envoy
- **Cost:** 2 engineer-days
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** the Envoy tree.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- This is the change in our code it becomes.

### One definitions record

- **Question:** Q2 What decides the round when the two graders disagree?
- **Proposal:** A typed definition carrying its unit and its scale as values.
- **Verdict:** reject
- **Landing component:** Dispatch
- **Cost:** 4 engineer-days
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** the Dispatch tree.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because the measurement under it failed.

### An execution record under the gen_ai. names

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** Spans under the OpenTelemetry `gen_ai.` names.
- **Verdict:** port
- **Landing component:** Legion
- **Cost:** 6 engineer-days
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** the daemon tree.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- This is the change in our code it becomes.
- **Landed:** LANDED_ROWS
- **Proven by:** `true`
MD

    (
        cd "$dir/repo" || exit 1
        git init --quiet -b main .
        git config user.email fixture@example.org
        git config user.name fixture
        git add packages skills
        git commit --quiet -m "the base"
        printf '%s' "$(git rev-parse HEAD)" >"$dir/base"
        git add docs/research/internet-of-cognition/questions.md
        git commit --quiet -m "docs(research): the question list"
        git add docs
        git commit --quiet -m "docs(research): the evidence"
        git commit --quiet --allow-empty -m "feat(skills): each review round's two graders are recorded as two rows, not one verdict"
        printf '%s' "$(git rev-parse HEAD)" >"$dir/rows"
        git commit --quiet --allow-empty -m "feat(skills): the rule that combines the reviewer's two graders is written where the reviewer reads it"
    )
    local base rows mas_commit
    base="$(cat "$dir/base")"
    rows="$(cat "$dir/rows")"
    mas_commit="$(jq -r '.repositories[] | select(.name=="mas-lab") | .commit' "$EVIDENCE/sources.json")"
    sed -i "s/OURS/$base/g; s/COMMIT/$mas_commit/g" "$evidence/sources.json" "$evidence/claims.jsonl" "$evidence"/mas-lab/*.md
    sed -i "s/PAGE_SHA/$(sha256sum "$evidence/published/page.txt" | cut -d' ' -f1)/; s/LANDED_ROWS/$rows/" "$evidence/sources.json"
    # The commits above were made before those two names were resolved, so one more
    # commit leaves the fixture's working copy clean and a planted fault may check out.
    (cd "$dir/repo" && git add -A && git commit --quiet -m "docs(research): the pinned commits")
    sed -i "s/LANDED_ROWS/$rows/" "$dir/judgement.md"
}

fixture_run() {
    # fixture_run <dir> <mode>; runs the gate over the fixture <dir> once, prints the
    # directory it made for that run under <dir>, and ends with the run's status. The
    # directory holds the gate's own output (text), its ending record (ending, the file
    # IOC_ENDING names for this run alone) and the status it exited with (status), and a
    # case of check.test.sh and --self-test read the run by ending_read. The fixture's own
    # rows read its own repository alone, so it needs nothing env.sh prepares and skips it.
    # It runs in an environment of its own, as the header's "The fixture of --self-test"
    # states: FIXTURE_CARRIED and FIXTURE_INPUTS, each where it is set, the fixture's own
    # tree, judgement and head, the run's own record, and an IOC_CACHE of its own unless a
    # case names one; no other variable of the caller's.
    local dir="$1" mode="$2" name carried=() run status
    for name in $FIXTURE_CARRIED $FIXTURE_INPUTS; do
        [ -n "${!name+set}" ] && carried+=("$name=${!name}")
    done
    run="$(mktemp -d "$dir/run.XXXXXX")" || return 1
    env -i "${carried[@]}" \
        IOC_ROOT="$dir/repo" \
        IOC_EVIDENCE="$dir/repo/docs/research/internet-of-cognition" \
        IOC_JUDGEMENT="$dir/judgement.md" \
        IOC_NO_ENV=1 \
        IOC_PUBLISHED_COMMIT="$(git -C "$dir/repo" rev-parse HEAD 2>/dev/null)" \
        IOC_CACHE="${IOC_CACHE:-$dir/cache}" \
        IOC_ENDING="$run/ending" \
        bash "$dir/repo/docs/research/internet-of-cognition/check.sh" "$mode" >"$run/text" 2>&1
    status=$?
    printf '%s\n' "$status" >"$run/status"
    printf '%s\n' "$run"
    return "$status"
}

# gate_run <script> [<mode>]; runs a copy of the gate, or check.test.sh, once, with the
# caller's environment and not over a fixture, prints the directory it made for that run
# under this run's scratch, which holds what fixture_run's does, and ends with the run's
# status. --self-test runs check.test.sh and --admitted by it, and check.test.sh each run
# of a copy.
gate_run() {
    local run status
    mkdir -p "$RUN_SCRATCH/runs" && run="$(mktemp -d "$RUN_SCRATCH/runs/run.XXXXXX")" || return 1
    IOC_ENDING="$run/ending" bash "$1" ${2:+"$2"} >"$run/text" 2>&1
    status=$?
    printf '%s\n' "$status" >"$run/status"
    printf '%s\n' "$run"
    return "$status"
}

fixture_plant() {
    # fixture_plant <dir> <family>; breaks the copy and returns 0, or 1 when the family
    # has no planted fault.
    local dir="$1" family="$2"
    local evidence="$dir/repo/docs/research/internet-of-cognition"
    case "$family" in
    claim-form) sed -i 's/"kind":"project-code"/"kind":"a reading"/' "$evidence/claims.jsonl" ;;
    claim-mark) sed -i 's/"mark":"verified"/"mark":""/' "$evidence/claims.jsonl" ;;
    claim-marks-agree) sed -i 's/"checks":\[{"row":"u-1","mark":"verified"}\]/"checks":[{"row":"u-1","mark":"false"}]/' "$evidence/claims.jsonl" ;;
    predicate-form) sed -i 's/"expect":{"contains":"ran once"}/"expect":{"contains_a_sentence":"the output says it ran"}/' "$evidence/claims.jsonl" ;;
    install-pinned) sed -i "s/\"command\":\"printf 'ran once/\"command\":\"true || uvx pyshacl --version; printf 'ran once/" "$evidence/claims.jsonl" ;;
    replay-command) sed -i 's/"expect":{"contains":"ran once"}/"expect":{"contains":"ran twice"}/' "$evidence/claims.jsonl" ;;
    absence-still-empty) sed -i "s#grep -r 'REVIEWER_PAIR_REQUIRED' skills || true#grep -r 'thermonuclear' skills || true#" "$evidence/claims.jsonl" ;;
    quotation-marked) sed -i 's/"marked":"a claim of the project, not of its code"/"marked_as":"evidence of the code"/' "$evidence/claims.jsonl" ;;
    citation-row) sed -i '/"id":"u-1"/d' "$evidence/claims.jsonl" ;;
    mas-lab-directory) rm -f "$evidence/mas-lab/library-kg.md" ;;
    mas-lab-question) sed -i 's/\[f-project-1\]//' "$evidence/mas-lab/ctl.md" ;;
    interview-fixed-row) sed -i '/"id":"i-three-tiers"/d' "$evidence/claims.jsonl" ;;
    our-data-input) sed -i 's#^Input: dispatch://INFRA-3134#Input: tests/fixtures/golden-transcript.json#' "$evidence/tests/concord.md" ;;
    count-reruns) sed -i 's/"instances": 40,/"instances": 41,/' "$evidence/evidence/count.json" ;;
    count-window) rm -f "$evidence/coordination/window.json" ;;
    count-class) sed -i 's/| inside one tree |/| inside one pod |/' "$evidence/coordination/count.md" ;;
    count-denominator) sed -i 's/| across sessions | 10 | 100 |/| across sessions | 10 |/' "$evidence/coordination/count.md" ;;
    count-zero-premise) sed -i 's/| inside one tree | 20 | 100 |/| inside one tree | 0 | 100 |/' "$evidence/coordination/count.md" ;;
    page-count) cat >>"$dir/judgement.md" <<'MD'

### An eighth candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One more.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.

### A ninth candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One more again.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.

### A tenth candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One more and again.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.

### An eleventh candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** One more and again and again.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.

### A twelfth candidate page

- **Question:** Q1 When a role writes a fact it read somewhere else, what carries the way back?
- **Proposal:** The last one.
- **Verdict:** reject
- **Landing component:** Legion
- **Cost:** 1 engineer-day
- **Deletes or replaces:** nothing.
- **Cost of reversing it:** one commit.
- **Default if nobody answers:** nothing changes.
- **Owner:** nobody.
- **Node model:** it survives, read against dispatch://INFRA-1098/spec v97 and dispatch://INFRA-1253/spec v17.
- **Test:** `printf 'ran once\n'` → ran once.
- It is rejected because nothing asks for it.
MD
        ;;
    page-parts) sed -i '0,/- \*\*Cost of reversing it:\*\* one commit./{/- \*\*Cost of reversing it:\*\* one commit./d}' "$dir/judgement.md" ;;
    page-verdict) sed -i '0,/- \*\*Verdict:\*\* port/{s/- \*\*Verdict:\*\* port/- **Verdict:** port and reject/}' "$dir/judgement.md" ;;
    verdict-commitment) sed -i 's/- It is rejected because the measurement under it failed./- It is not for us./' "$dir/judgement.md" ;;
    fixed-page) sed -i 's/- \*\*Proposal:\*\* One optional field, `quotes: \[{ref, token}\]`./- **Proposal:** Something on the message./' "$dir/judgement.md" ;;
    front-page) sed -i "/^## Where the authors' account and the checked reading differ$/d" "$dir/judgement.md" ;;
    question-list) sed -i '0,/\*\*Question:\*\* Q1/{s/\*\*Question:\*\* Q1/**Question:** Q9/}' "$dir/judgement.md" ;;
    triage-named) sed -i '/^- dot-github: /d' "$dir/judgement.md" ;;
    order-of-work)
        (
            cd "$dir/repo" || exit 1
            # The branch is moved without a checkout, so the planted tree keeps every
            # file and only its history changes: one commit adds the question list and
            # the evidence together.
            git branch -f replanted "$(cat "$dir/base")"
            git symbolic-ref HEAD refs/heads/replanted
            git reset --quiet
            git add -A
            git commit --quiet -m "docs(research): the evidence and the question list in one commit, out of order"
        )
        ;;
    citation-our-code) sed -i 's/"symbol":"TesterVerdictRequired"/"symbol":"NameNoFileHolds"/' "$evidence/claims.jsonl" ;;
    citation-project-code) sed -i 's#"file":"README.md","symbol":"README"#"file":"README.md","symbol":"NoTextThatFileHolds"#' "$evidence/claims.jsonl" ;;
    citation-published) printf 'a byte the fetched page did not carry\n' >>"$evidence/published/page.txt" ;;
    citation-dispatch) printf '\nSee dispatch://INFRA-99999999/artifact/no-such-document.\n' >>"$evidence/tests/concord.md" ;;
    citation-kinds) sed -i 's/^    if first in TENS:/    if False:/; s/^    if first in WORD_NUMBERS:/    if False:/' "$evidence/citations.py" ;;
    landed-proof) sed -i 's/- \*\*Proven by:\*\* `true`/- **Proven by:** `false`/' "$dir/judgement.md" ;;
    landed-refutation)
        cat >>"$dir/judgement.md" <<'MD'
- **Landed:** not made
- **Refuted by:** a reading of the page with no command behind it.
MD
        ;;
    landed-order)
        (
            cd "$dir/repo" || exit 1
            # Two more commits with the same two subjects, in the other order, so the
            # newest row commit is a descendant of the newest rule commit.
            git commit --quiet --allow-empty -m "feat(skills): the rule that combines the reviewer's two graders is written where the reviewer reads it"
            git commit --quiet --allow-empty -m "feat(skills): each review round's two graders are recorded as two rows, not one verdict"
        )
        ;;
    landed-no-refusal)
        mkdir -p "$dir/repo/packages/daemon/internal/workflow"
        printf 'package workflow\n\nconst ReviewerPairRequired = "REVIEWER_PAIR_REQUIRED"\n' >"$dir/repo/packages/daemon/internal/workflow/review.go"
        (
            cd "$dir/repo" || exit 1
            git add packages
            git commit --quiet -m "a landed step that reaches a refusal path"
        )
        ;;
    landed-third-change)
        printf '\nA reviewer completion with no pair result recorded at the head is refused.\n' >>"$dir/repo/skills/legion-worker/references/merge-gate.md"
        (
            cd "$dir/repo" || exit 1
            git add skills
            git commit --quiet -m "the third change, landed as prose in a skill"
        )
        ;;
    *) return 1 ;;
    esac
    return 0
}

# self_test_ran <family> <the run> <its directory>; reads how a run the self-test made
# ended, by ending_read, which leaves RUN_* set for its caller. One that could not run
# judged nothing, so the self-test stops there, at <family>, naming the run and, last, the
# run's own last line, with the kind of that run's reason; finish then ends the self-test
# by the precedence of the header. Such a run is never read as an assertion that failed,
# and a run that crashed, one that wrote no record among them, is read as the failure it
# is by its caller.
self_test_ran() {
    ending_read "$3"
    [ "$RUN_ENDING" = cannot-run ] || return 0
    cannot_run "$(printf '%s' "$RUN_UNRUN" | head -1 | cut -f2)" " --self-test: $2 could not run, so the self-test stops at $1: $RUN_LINE"
}

mode_self_test() {
    local work="$RUN_SCRATCH/self-test" baseline
    mkdir -p "$work"
    fixture_build "$work/base"
    local mode modes="replay shape citations landed"
    local broken=0
    for mode in $modes; do
        baseline="$(fixture_run "$work/base" "--$mode")"
        self_test_ran self-test-baseline "--$mode over the fixture deliverable" "$baseline"
        if [ "$RUN_ENDING" != passed ]; then
            broken=$((broken + 1))
            fail "self-test-baseline" "the fixture deliverable does not pass --$mode, exit $RUN_STATUS: $(grep '^FAIL' "$baseline/text" | head -3 | tr '\n' ' ')$RUN_LINE"
        fi
    done
    [ "$broken" -eq 0 ] || return 0
    pass "self-test-baseline" "the fixture deliverable passes --$(printf '%s' "$modes" | sed 's/ /, --/g')"

    local family planted
    while IFS='|' read -r family mode asserts fault; do
        [ "$fault" = "-" ] && continue
        planted="$work/$family"
        rm -rf "$planted"
        mkdir -p "$planted"
        cp -r "$work/base/repo" "$planted/repo"
        cp "$work/base/judgement.md" "$planted/judgement.md"
        cp "$work/base/base" "$planted/base"
        cp "$work/base/rows" "$planted/rows"
        if ! fixture_plant "$planted" "$family"; then
            fail "$family" "the self-test plants no fault of this kind, so nothing proves the gate reads it"
            continue
        fi
        local out
        out="$(fixture_run "$planted" "--$mode")"
        self_test_ran "$family" "--$mode over the fixture with the planted fault of $family" "$out"
        if printf '%s' "$RUN_FAILED" | grep -qxF "$family"; then
            pass "$family" "the gate fails the planted fault and names it: $fault"
        else
            fail "$family" "the gate passed the planted fault ($fault)"
        fi
        rm -rf "$planted"
    done < <(assertion_table)

    # The faults the tester of this issue found the gate passing are cases of their own,
    # in check.test.sh beside this script, because each of them is a second fault of a
    # family this table already plants once. The self-test runs them here, so one run
    # proves both lists.
    if [ -r "$EVIDENCE/check.test.sh" ]; then
        local cases
        cases="$(gate_run "$EVIDENCE/check.test.sh")"
        self_test_ran gate-cases "check.test.sh" "$cases"
        if [ "$RUN_ENDING" = passed ]; then
            pass gate-cases "check.test.sh: $RUN_LINE"
        else
            fail gate-cases "check.test.sh does not pass, exit $RUN_STATUS: $(grep '^FAIL' "$cases/text" | head -3 | tr '\n' ' ')$RUN_LINE"
        fi
    else
        fail gate-cases "check.test.sh is missing, so the faults the tester found are proved by nothing"
    fi

    # The classifier admits exactly the forms the rows use, through its one table, and the
    # header's table is the one --admitted measures.
    local admitted
    admitted="$(gate_run "$EVIDENCE/check.sh" --admitted)"
    self_test_ran admitted-table "check.sh --admitted" "$admitted"
    if [ "$RUN_ENDING" = passed ]; then
        pass admitted-table "check.sh --admitted: every entry of ADMITTED is used by a row, no row is refused, and the header's table is the one measured"
    else
        fail admitted-table "check.sh --admitted does not pass, exit $RUN_STATUS: $(grep '^FAIL' "$admitted/text" | head -3 | tr '\n' ' ')$RUN_LINE"
    fi

    # --coverage names every family the gate holds, so a mode nothing runs is still read.
    local missing=0 coverage
    coverage="$(bash "$EVIDENCE/check.sh" --coverage)"
    while IFS='|' read -r family _mode _asserts _fault; do
        printf '%s\n' "$coverage" | grep -q "^$family " || {
            missing=$((missing + 1))
            fail coverage-complete "the coverage report does not name the family $family"
        }
    done < <(assertion_table)
    [ "$missing" -eq 0 ] && pass coverage-complete "the coverage report names every assertion family the gate holds"
    rm -rf "$work"
}

main() {
    local mode="${1:---help}"
    case "$mode" in
    --help | -h) usage ;;
    --coverage) coverage_report ;;
    --replay) run_mode replay ;;
    --shape) run_mode shape ;;
    --citations) run_mode citations ;;
    --landed) run_mode landed ;;
    --published) run_mode published ;;
    --admitted) run_mode admitted ;;
    --self-test) run_mode self-test ;;
    *)
        usage
        early_end gate "it has no mode $mode; its modes are the ones the header above names"
        ;;
    esac
}

# Sourced, as check.test.sh sources it for the fixture builder and runner, the script
# defines its functions and runs no mode.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
    main "$@"
fi
