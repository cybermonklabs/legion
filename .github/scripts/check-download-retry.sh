#!/usr/bin/env bash
# Every download this repository's CI makes retries, and one rule holds that retry
# (dispatch://INFRA-1545, R2). This check is the enforcement half: nothing else refuses a fetch
# added with no retry, a retry below the floor, an attempt with no time bound, a second copy of
# the retry loop, or a fetch whose failure the pipeline around it swallows.
#
# THE FLOOR (R1): at least 5 attempts, the last starting at least 120 seconds after the first, and
# each attempt bounded in time, so an attempt against a host that accepts the connection and then
# sends nothing stops and the next one starts. Three mechanisms reach it, and a site invents no
# fourth:
#
#   1. `retry_fetch <what> <bound> -- <command...>`, the one helper, in
#      .github/scripts/ci-retry.sh: five attempts, sleeps of 10, 20, 40 and 80 seconds, each
#      attempt under `timeout <bound>`. A workflow `run:`, a composite action's `run:` and a
#      script source that file and call it.
#   2. curl's own options, `--retry 7 --retry-all-errors --max-time <bound>` with `--fail` —
#      measured on curl 8.14.1: eight attempts whose last starts 127 s after the first, with
#      `--max-time` reset for each one, so it bounds every attempt and not the whole transfer.
#      `--fail` is load-bearing and usually arrives as the `f` of `-fsSL`: without it curl exits 0
#      on a persistent 504 and writes the error body into the output file, which the checksum or
#      the unpack after it then reads. A curl that reads the status itself (`-w '%{http_code}'`
#      and a test of what it printed) answers for `--fail` and passes.
#
#      Two more options decide when that last attempt starts, and a curl carrying either is held
#      to the floor's 120 s by the ladder this check computes rather than by its `--retry` count
#      alone. The rule is curl's own, read in its documentation at curl 8.14.1
#      (docs/cmdline-opts/retry.md, retry-delay.md and retry-max-time.md of the curl-8_14_1 tag):
#      curl "first waits one second and then for all forthcoming retries it doubles the waiting
#      time until it reaches 10 minutes", `--retry-delay <n>` replaces that doubling with a fixed
#      wait of n seconds before every retry (and `--retry-delay 0` asks for the default back-off
#      again), and `--retry-max-time <n>` stops the ladder: a retry is made only while the timer
#      reset at the first attempt "has not reached this given limit", so the last attempt starts
#      no more than one wait past n (and `--retry-max-time 0` sets no limit at all). The ladder
#      this check computes is the sum of those waits with the attempts themselves taking no time,
#      which is the longest one the options allow; a curl whose longest ladder is under 120 s is
#      refused with the figure. `--retry 7 --retry-delay 1` is 7 s and `--retry 7
#      --retry-max-time 5` is 7 s, and both pass every other test of this floor. No site in the
#      tree writes either option today. Rule 8 reads this same ladder when it prices the curl's
#      attempts against the step's budget, so a curl's floor and its arithmetic are one
#      computation rather than two that can drift apart.
#   3. inside a Dockerfile, one canonical inline `retry()` definition, the same text in every
#      network `RUN` of the three files. A Dockerfile carries the loop inline because it can hold
#      no new file above a layer whose order its own comments set on purpose, and a COPY of a
#      helper there would throw that layer's cache away on every edit of the helper.
#
# WHAT IT READS: every file under .github/workflows/, every action.yml (or action.yaml) under
# .github/actions/, every *.sh under .github/scripts/ but the `.test.sh` harnesses, and the three
# Dockerfiles a CI job builds (DOCKERFILES below; a missing one fails this check, so a rename is
# seen). Files are discovered by walking the tree, not by asking git, so an unsnapshotted change
# cannot read green locally. Workflows and actions are parsed as YAML, never grepped, and only a
# step's `run:` is read. This check reads its own file like any other script, which is why the
# comments here write no heredoc operator of their own: an operator no line of this file closes
# would blank every line after it for this file's own reading.
#
# Rule 8 reads one step across more than one of those files: a step that runs a script under
# .github/scripts/ carries that script's own retried calls, and those of every script it calls
# in turn. SCRIPT_CALL finds a call by the path it writes, scripts_called reads one body's calls
# with the line each is written on, calls_of folds the loops and the functions around that line
# into how many times the body runs it, and script_arithmetic walks them under a `seen` set,
# which counts a file two of a step's paths
# reach once and stops a cycle. Three budgets of this tree were a one-line `run:` of a script
# call and nothing else — ci.yaml's `pr-title` and `ci-ok` steps and pr-title.yaml's, 1200 s,
# 600 s and 900 s of retries under a 20-, a 10- and a 15-minute budget — and a rule 8 that
# stopped at the `run:` saw the arithmetic of none of them.
#
# Three limits are left in that reading set, and each is a script this check cannot name. A call
# that does not write the script's path — one that builds the path as it runs, or runs a
# variable holding the whole command — is a call SCRIPT_CALL does not find: the script is still
# read as a file of its own, so rules 1 to 6 hold it, but its arithmetic reaches no step's
# budget. A call inside a heredoc whose consumer this check cannot name is another: a body
# belongs to the command it is fed to, and runs_heredoc reads that command off the line the
# operator sits on, so the body of a `bash` heredoc is shell the step runs and the calls in it
# are the step's own, while `cat`, a redirect into a file, `jq` and a `python3` heredoc take
# their body as data, where a path written in it is a path something prints rather than runs,
# and a body read as data is blanked before any rule reads the step (without_heredocs).
# What the command word does not say, this check does not read: a consumer
# written as a variable (a heredoc fed to `"$shell"`), a shell behind a program reduce_words
# does not strip (an `ssh host bash`, and a wrapper written as a path, where a bare `env bash`
# is read as the shell it runs and `/usr/bin/env bash` is not), and an interpreter whose script
# argument hides behind a long option taking a value all read as data, so a call written in one
# of those bodies reaches no step's budget. Four shapes of that reading are worth naming,
# because each is a fetch the step really runs and this check really passes, measured in
# scratch trees by review round 3 of dispatch://INFRA-1545 and in none of this tree: a body fed
# to `cat` and piped into a shell, where the consumer word is `cat` and the pipeline is what
# runs it; a shell given an option whose value stands where its script would, such as a `bash
# -O extglob`, which leaves the body data to this check and shell to the runner; an unquoted
# body whose text holds a command substitution of a script path, which the step's own shell
# expands before the consumer ever reads it, so the call is the step's own whatever the
# consumer is, and a path in such a body is no path merely printed — that reading holds for a
# quoted body alone; and an interpreter whose command sits in the value of a `-c` written after
# another option, such as a `bash -o pipefail -c`, where the reading stops at the option's
# value and the fetch inside names no site at all. And a script outside
# .github/scripts/ is not read at all, since that one directory is
# the whole set: a `run:` that calls the repository's own scripts/<name>.sh or a
# packages/<package>/scripts/<name>.sh — ci.yaml and the release workflows each hold such calls
# — is read no further than its own words, so a download written inside one of those files is
# held to no floor here and sits in no budget. Moving it under .github/scripts/ is what brings
# it back under both rules, and a new file there is read without anyone remembering to list it.
# The harnesses left out are named at TEST_HARNESS below, with the two network calls they hold
# today and why a retry at either would destroy what the test measures.
#
# THE VERBS. A fetch is one of these commands, in command position, after the wrappers below are
# stripped: curl, wget, apt-get/apt (update, install, download, upgrade, dist-upgrade, source,
# build-dep), pip install, uv (sync, add, pip, tool, python, lock, export), bun install, bun add,
# npm/pnpm/yarn (install, ci, add), playwright install, go mod download, go install,
# mise (install, use), crane (copy, digest, config, pull, export, manifest, blob), docker pull,
# pulumi plugin install, and gh — every subcommand of it but the local ones named next.
#
# gh is read the other way round from the rest, by what it is not. `gh api` and `gh release
# download` were the whole of its table once, and an unretried `gh run list`, `gh pr view`, `gh
# run view`, `gh cache delete`, `gh workflow run` or `gh search code` was therefore invisible
# here: every one of them is the same HTTPS call to the same host as a `gh api`, carrying the
# same token, and the difference between the two sets was a subcommand name and nothing else. So
# the set is gh's own command list minus what reaches no network, read off `gh --help` and off
# each command's own help: GH_LOCAL below — `alias`, `completion`, `config`, `extension`, `help`
# and `version`, which answer out of the local config, the local extension directory or the
# binary — and the four local actions of `gh auth`, `git-credential`, `setup-git`, `switch` and
# `token`, which read the stored token or write the local git config, where `gh auth login`,
# `logout`, `refresh` and `status` talk to the host. Anything else gh runs is a fetch, so a
# subcommand nobody has listed is held to the floor rather than passing unseen. A verb is named
# `gh <subcommand> <action>` for the subcommands that take an action of their own (GH_GROUPS:
# `gh run list`, `gh release view`), and `gh <subcommand>` for the three that take an argument
# instead (`gh api repos/owner/name`, `gh browse`, `gh status`). A gh call that writes is a site
# like a gh call that reads: `gh api --method POST` has always been one here, and the retry is
# for the transport, which does not know the method.
#
# `mise x`, `mise exec`, `mise up` and
# `mise upgrade` are deliberately not among the verbs; rule 7 below says why, and what it does
# with one instead. `bunx`, `npx`, `pnpx`, `sh -c`, `bash -c` and `eval` are transparent: the
# verb is what they run, so a `bunx playwright install` is the Playwright download and a `bunx
# biome check` is not a fetch at all (the binary it runs came from the workspace install, which
# is itself a site).
#
# The wrappers stripped before the command is read are an assignment, a shell keyword, `sudo`,
# `env`, `exec`, `command`, `nice`, `nohup`, `time`, `timeout <seconds>`, `retry_fetch <what>
# <bound> --` and a Dockerfile's `retry <bound> <what>`. The last three are also what says a
# fetch is retried and bounded. A command that answers about itself (`--version`, `--help`) reads
# nothing from the network and is no fetch, whatever its verb.
#
# Two verbs are deliberately outside the subject, because R1's subject is a tool, a toolchain or a
# dependency: `git clone` and `git fetch`, which move this repository's own refs rather than
# anything a job installs, and which no site in the tree retries. `playwright install-deps` is
# named under the exceptions below.
#
# THE RULES.
#
#   1. In a workflow `run:`, an action.yml `run:` or a *.sh under .github/scripts/, a fetch must
#      be a `retry_fetch` call or, for curl, carry curl's own options at the floor.
#   2. A loop (`while`, `until`, `for`) whose body holds a fetch that is not a `retry_fetch` call
#      is a copy of the helper and is refused as one, whether or not that fetch would pass rule 1
#      on its own: R2's rule is that one place holds the attempts. A loop reaches a fetch two
#      ways, and both count: the fetch is written in the body, or the body is a function the same
#      block calls with a fetch among the call's words — `attempts 2 "apt-get update" sudo timeout
#      90 apt-get update`, the shape the shared helper was made from.
#   3. A Dockerfile `RUN` that fetches opens with the canonical `retry()` text (CANONICAL below),
#      compared with its backslash continuations joined and each run of blank space collapsed to
#      one space, which is the one liberty the tree's own two shapes force: a `RUN --mount=...`
#      indents the definition under the flag. Each fetch of that `RUN` is then wrapped in
#      `retry <bound> <what>`, or is a curl carrying curl's own options. A `RUN` that defines a
#      retry loop of its own text is refused. A Dockerfile is judged against the floor alone:
#      rule 2 does not read one, because the canonical definition is itself a loop and because a
#      `RUN` iterating a list is how a Dockerfile writes what a script would write as a function.
#   4. An attempt with no time bound is refused: a curl with no `--max-time`, or a retried fetch
#      of any other verb that no `timeout` and no helper bounds.
#   5. An `apt-get update` carries `-o APT::Update::Error-Mode=any`. This is apt's `--fail`, and
#      the reason is curl's: a fetch that reports success having fetched nothing. `apt-get update`
#      exits 0 when every index fails to download — it prints `W: Failed to fetch … 504 Gateway
#      Timeout` for each one and then `W: Some index files failed to download. They have been
#      ignored, or old ones used instead.` — so a retry around it succeeds on its first attempt
#      having read no list, and the `apt-get install` after it fails with `E: Package '…' has no
#      installation candidate` against a list that will never name the package. That is a failure
#      no further attempt can cure, which is what cost run 42375256920's `download-retry-images`
#      job. The option turns each of those warnings into an error, so the command exits non-zero
#      and the retry retries. It has existed since apt 2.1.10; the images here run apt 2.6
#      (bookworm) and 2.9 (trixie), and the runner's own on Ubuntu 24.04.
#
#      Every `apt-get update` this check holds to the floor carries it, retried or not. The two
#      sets are the same once the tree is green, since rule 1 and rule 3 retry every one of them;
#      scoping the rule to the retried ones would differ only in handing an author who writes a
#      bare `apt-get update` one refusal now and a second one after they add the retry. The sites
#      an exception below removes are outside this rule as they are outside the rest.
#   6. In Python, inside the heredoc of a *.sh under .github/scripts/, a read through
#      `urllib.request` goes through check-ci-images.sh's `read_retrying`, which carries the floor
#      in that check's own idiom — five attempts, waits of 10, 20, 40 and 80 seconds, each bounded
#      by the read's own `timeout=30`. One definition, no copies, as the shell has.
#   7. A `mise x`, `mise exec`, `mise up` or `mise upgrade` carries no retry, anywhere: not a
#      `retry_fetch` call and not a Dockerfile `retry` call. None of the four fails when it
#      cannot resolve the tool. `mise x` warns (`Failed to resolve tool version list for … HTTP
#      host https://api.github.com:443 is unavailable`) and then runs whatever binary of that
#      name is already on PATH — a different build, at a version nobody asked for — and exits 0;
#      `mise up` prints `All tools are up to date` having read nothing, and exits 0. Measured on
#      mise 2026.8.12, the build packages/daemon/docker/worker.Dockerfile pins, against a proxy
#      faulting every host. So a retry around one of them waits for a failure that never comes,
#      and the cure is structural: retry `mise install <tool>`, which exits 1 on the failed fetch
#      on that same build, and read the path afterwards from what that install put on disk.
#
#      That is also why the four are not fetch verbs above, and why a bare one is on no list. A
#      `mise x` that follows the install of the tool it names resolves off disk: with every host
#      faulting and a decoy binary earlier on PATH it still printed the installed path, silently
#      and exit 0. A `mise x` that follows no install fetches only as a side effect of the
#      resolution that install should have done. Holding either to the floor would be demanding
#      the one shape this rule refuses, so the check reads these four only to refuse a retry.
#
#   8. A step that holds a retried fetch carries its own `timeout-minutes`, and that budget
#      gives strictly more time than the attempts the step allows: 60 x <minutes> is strictly
#      greater than
#      that step's arithmetic, which is 5 x <bound> + 150 s for each `retry_fetch` call and,
#      for each curl making its own attempts, one attempt per `--retry` and one more at
#      `--max-time` with curl's own waits between them — the same ladder rule 1 holds that curl
#      to, computed once in curl_ladder and read by both rules, so `--retry 7` is
#      8 x <bound> + 127 s and `--retry 10` is 11 x <bound> + 1023 s. A curl outside rule 1's
#      floor is read at the floor, which is the ladder it makes once that refusal is answered.
#      Those figures are summed over the step's own
#      calls, over those of every script under .github/scripts/ the step runs and every script
#      that one runs in turn, and, for a `uses:` step of a local composite action, over that
#      action's calls and the scripts and actions it reaches the same way. Equal is
#      not enough: the last attempt's own error and the runner's kill then land at the same
#      instant, and the retry can lose that race. Measured in run 42386541065's job
#      128445139635, where a `gh api` read bounded at 30 s sat under `timeout-minutes: 5`, its
#      own arithmetic of 5 x 30 + 150 = 300 s against a budget of 300 s: the helper's `::error::`
#      landed at 17:22:16.7444966Z and the runner's kill at 17:22:16.7483230Z, four milliseconds
#      apart. Lose that race and the step reports GitHub's bare `The action '…' has timed out
#      after 5 minutes.` instead of naming the site and its attempts, which is the loud failure
#      the retry exists to give. GitHub's granularity is the minute, so a whole minute is the
#      smallest margin that can be written.
#
#      A step holding an arithmetic above zero and writing no `timeout-minutes` at all is
#      refused with the figure it needs. A budget that does not exist is judged by nothing, and
#      nothing was what judged ci.yaml's `airlock-lean` at 61e220f36: the step running
#      .github/actions/install-elan carried that action's curl of the elan tarball,
#      8 x 60 + 127 = 607 s, wrote no budget of its own, and the job's twenty minutes carried
#      no term for it either, so an exhausted ladder there would have ended the job on GitHub's
#      bare timeout line. This check read that step's cost and compared it with nothing,
#      because only a step that wrote a budget made an entry to compare. Every step of a
#      workflow makes one now. A composite action's step makes none: GitHub takes no
#      `timeout-minutes` on one, so the budget that holds an action's calls is the `uses:` step
#      that runs it, which is where the figure is asked for.
#
#      The job's own `timeout-minutes` is held above that same arithmetic, because a job
#      ceiling under one of its steps' attempts cuts the step before the step's own budget can
#      name the site, and the step's margin then buys nothing. A job writing no
#      `timeout-minutes` is held at GitHub's own default of 360 minutes (JOB_DEFAULT_MINUTES).
#      This is the rule ci.yaml's Budgets note already writes for a reader — a job carries its
#      measured duration plus the arithmetic of its slowest retried step — read here as the
#      half a check can make, which is the arithmetic and not the measurement.
#
#      How many times a step runs one call, and what this check can see of it. Rule 8
#      multiplies a call by the loops and the function calls it resolves, which are the three
#      kinds listed next; where a loop it did read writes no count it refuses the site rather
#      than guessing; and every shape it reads as neither a loop nor a call leaves the call
#      counted once. It is no shell parser, so the list of what it counts once, below the
#      three, is as much this rule as the list of what it multiplies.
#
#        * each loop around the call, and each loop around that loop. A call in a three-pass
#          loop inside a two-pass loop runs six times, and reading the inner loop alone would
#          count two of those six.
#        * each call of the function the call sits in, and the loops around those calls. A
#          page read written once in a function that a seven-pass loop calls is seven ladders
#          and not one. check-pr-text.sh reads like that instance and is not it: its page read
#          sits in read_list's `while :`, a loop this check cannot count, so the fold stops
#          there and never reaches the `for ((i = 0; i < ${#lists[@]}; i += 2))` that runs
#          read_list once per list. The three lists are carried by hand instead, in that
#          script's DeclaredArithmetic entry below, which writes the 3 x in its own sum.
#        * the loop a step wraps around the script it runs. The script's whole arithmetic is
#          the step's once per pass, as a call written in the `run:` would be.
#
#      Two written calls of one function are two, branches or not, as two fetches written in
#      one `run:` are two: this check reads no branch, and the figure it sums is the one a step
#      could wait for. The pass count is read where the loop writes it, and only in the three
#      shapes this check is sure of: a `for x in 1 2 3 4 5 6 7`, whose list is literal words, a
#      `for x in $(seq 1 7)`, and a `for ((i = 0; i < <bound>; i += <step>))` whose bound is a
#      number or `${#name[@]}` for an array the same body writes out as words. dequeued.yaml's
#      timeline read is one `retry_fetch` call of 300 s written inside a seven-pass poll, so
#      the step waits 2100 s for it and not 300 s. The
#      loop's own waits between the passes are added where they are written as a `sleep <n>` of
#      that loop's body — the poll's six 10 s waits above — and a loop whose wait is computed
#      rather than written adds none, so the figure there is the attempts alone and the passes
#      between them cost nothing in it.
#
#      A loop whose pass count is not written — a `while read` over a list the run makes, a
#      `while :` over the pages of a route — is a count this check does not make, and no pass
#      number stands in for one it did not make. What answers such a site is a
#      DeclaredArithmetic entry of DECLARED_LOOPS below: it writes the seconds that site
#      contributes to the budget of the step holding it, and its `why` writes that figure out
#      as a sum, so a reader checks the arithmetic rather than taking the number. The figure
#      may not fall below one call of the site, which is what one_call computes: a site costs
#      at least one call of itself, whatever the run finds for it to do, and a figure under
#      that floor is refused naming the site and the floor.
#
#      WHAT IT COUNTS ONCE, which is the other half of this rule and the half a reader is owed
#      plainly. A shape this check reads as neither a loop nor a call of a function is no
#      refusal: the call is counted once, the budget is judged against that one figure, and a
#      step whose real cost is several ladders passes. Each shape below was driven at
#      398ee1e88c83 as a step at `timeout-minutes: 6` around a 300 s call, in a scratch tree,
#      and none of them is in this tree: review round 3 of dispatch://INFRA-1545 read every
#      step and every script of it with this check's own parser and found no instance.
#
#        * two loops written on one line: a loop is keyed by the line it sits on, so the
#          second overwrites the first and the call is multiplied by the wrong one of the two
#          (a five-pass loop's call, with a three-pass loop after it on its line, is 900 s and
#          not 1500 s). A loop opened on the line of an `if`, as in
#          `if true; then for a in 1 2; do`, is not read either, and its call counts once.
#        * a call of a function that this check finds only where the function's name is the
#          first word of its command: a function called as the condition of an `if` inside a
#          loop, one called behind an assignment prefix such as `GH_HOST=x`, and one called
#          inside a one-line `for x in a b c; do fetch; done`. A function written in the
#          `function name { … }` or `function name() { … }` form is not read as a function at
#          all. In each case the retried call inside the function is counted once.
#        * a function that calls itself: counted once, because a reading that cannot see the
#          second call cannot price it.
#        * a script run through another program rather than by its own path, such as an
#          `xargs -n1 .github/scripts/x.sh`.
#        * a `for ((i = 0; i < ${#name[@]}; i++))` over an array the body writes as words this
#          check takes literally: `($var)` and `(a{1,2,3})` are one word each, so the loop is
#          one pass; and an element assignment such as `name[2]=c` is not read, so the count is
#          the words written at the array's definition alone.
#        * one script that two of a step's paths reach, counted once, because the `seen` set
#          is shared across a step's scripts. That set exists to stop a cycle, and this is the
#          same mechanism read from the other side.
#
#      What is read correctly, though a reader may expect otherwise: a retried call written
#      directly, not through a function, as the condition of an `if` inside a loop or behind an
#      assignment prefix such as `GH_HOST=x`, and a loop written on its own line inside an
#      `if`, are each multiplied by their loop. A
#      `for x in` over `"${name[@]}"`, `$var` or a brace expansion, and a
#      `for ((…; i < ${#name[@]}; …))` over an array `mapfile` fills, are loops whose passes
#      this check cannot read, and are refused, as the paragraph below that begins "A step
#      whose budget reaches a loop" says.
#
#      One shape goes the other way and over-counts: a script call and a loop written on one
#      line are read as the loop holding the call, which buys a budget a margin it does not
#      need and refuses nothing that is right.
#
#      None of this asks for a wider reading. A parser that learns some of those shapes and
#      stays silent on the rest is this same defect one layer in and harder to see, where a
#      list a reader can check is worth what it says and no more.
#
#      A step whose budget reaches a loop this check read but whose passes are not written,
#      and for which no DeclaredArithmetic entry writes the seconds, is refused in place of
#      being judged, and the refusal names the file, the line and the verb, the loop line it
#      could not read, and what answers it. That refusal is the default for the loops this
#      check reads, and for those alone. This rule has been wrong here three times, each fix
#      teaching the reading one more loop shape while leaving the default on pass; a loop read
#      with no count is refused now, and a declaration carries its own arithmetic or it is not
#      one.
#
#      An entry matching no such site fails this check, as a stale exception does, so a
#      declaration cannot outlive its loop; and an entry whose file, step and verb match sites
#      in two loops fails as well, because it writes one figure and the second loop would take
#      it without anybody writing it one. A loop in a script no step runs is nobody's
#      arithmetic and is left alone, and so is a Dockerfile's, whose RUN lines sit in no step's
#      budget; rule 2 is what reads a loop wherever it sits.
#
#   9. In a step whose shell leaves `pipefail` off, a fetch that is not the last command of its
#      pipeline is refused. A workflow `run:` that names no shell runs under GitHub's own
#      default, `/usr/bin/bash -e {0}`: errexit on, pipefail off, and a pipeline's status is
#      then its last command's alone. So `curl -fsSL <url> | tar -xz` exits 0 when curl got a
#      504 and tar unpacked nothing, and a `retry_fetch` around that pipeline reads that 0 and
#      never retries: the site reads retried here and is unprotected in fact. It is the class of
#      curl without `--fail` (rule 1), of `apt-get update` without its error mode (rule 5) and
#      of `mise x` running a decoy (rule 7) — a command that reports success having fetched
#      nothing. The cure is to read the fetch's own status: write the pipeline as two commands,
#      with the fetch's output in a file or a variable, or give the step a shell that sets
#      `-o pipefail`.
#
#      Which steps the rule reaches is read rather than assumed, because the answer is each
#      step's own: its `shell:`, else its job's `defaults.run.shell`, else the workflow's, and a
#      value of `bash` — which GitHub expands to `bash --noprofile --norc -eo pipefail {0}` — or
#      a custom command line writing `pipefail` itself takes that step out of the rule. So it
#      reaches every workflow `run:` of this tree but main-bypass-check.yaml's one step, which
#      names `shell: bash` on purpose and whose own test holds it to that shell, and it reaches
#      no composite action's step: GitHub requires one to name a shell, and all eleven here name
#      `bash`. A *.sh under .github/scripts/ is outside it as well, each setting its own `-o
#      pipefail`, and so is a Dockerfile RUN, which names no step's shell and whose pipelines
#      are rule 3's business. Measured in run 42386541065's job logs: 63 steps at
#      `/usr/bin/bash -e {0}`.
#
#      The scripts are outside it for a reason that is a limit as much as a scoping, and the
#      limit is written here rather than left for a reader to find: this rule asks its question
#      of a step's shell, and a script sets its own flags, so a piped fetch inside a *.sh under
#      .github/scripts/ is a hole this rule has no step to read. The hole is unreachable today,
#      and that is a measurement rather than a habit: all 70 scripts under .github/scripts/ set
#      `pipefail`, so `grep -L pipefail .github/scripts/*.sh` prints nothing. A script that
#      stops setting it would carry the fault this rule refuses in a step, and nothing here
#      would say so. What closes the hole is reading each script's own `set` line, and the day a
#      script leaves `pipefail` off is the day to write that reading.
#
#      That reading is the rule rather than a grep over the workflows, and the difference is not
#      academic: a rule refusing every workflow `run:` would be one line and would refuse
#      main-bypass-check.yaml's step, the one site in the tree that named a pipefail shell on
#      purpose. A scope taken from "no step sets a shell here" punishes the careful site and
#      passes the careless one the day either changes, so the question is asked of each step.
#
#      No site in the tree is refused by this rule today, and three lines look as though they
#      would be. Both pipeline spellings count: `|`, and `|&`, which is bash's pipe of standard
#      error beside standard output and loses the fetch's status the same way. `||` is neither
#      of them but a logical OR, and it sets nothing — which is what ci.yaml's `cancel-started`
#      step and dequeued.yaml's `cancel-dequeued` write, each ending a line with `gh api
#      --silent --method POST … ||`, and what the scanner tells from a `|` by the character
#      after it. release-envoy-listener.yaml's `if ! legacy_source=$(gh api …)` is the third
#      line, and it reads that fetch's own status, which is what this rule asks for.
#
#      A site this rule refuses that another rule refuses too is one `::error` and not two. The
#      sentence is appended to that site's own refusal, so a reader counts one refused site
#      rather than two annotations on one line of one file.
#
# THE EXCEPTIONS, which are R2's four and no others. Each is a site this repository refuses to
# retry on purpose. Each one written out as an entry of EXEMPTIONS below must match something: an
# entry that matches nothing fails this check, so an exception cannot outlive the site it was
# written for. (b) and (d) are read from the tree rather than listed, for the reasons they give.
#
#   a. A `ghcr.io` pull of a package this repository publishes. A failed pull there is a fault of
#      this repository's own package, and it must be seen rather than retried over. R2 named two
#      sites, .github/actions/pull-test-images and the `envoy-client` job's pull; T7's
#      `download-retry-images` job added a third, the pull of the base images of the three
#      Dockerfiles, which is the same package and the same reasoning.
#   b. A `uses:` reference, and anything inside a third-party action. This needs no entry: only a
#      step's `run:` is read, and no third-party action's source is in this tree. A composite
#      action cannot wrap a `uses:` step in a loop, which is why R4 answers the Bun runtime with a
#      restored cache rather than with a retry.
#   c. The apt phase of .github/scripts/playwright-install.sh, which dispatch://INFRA-1443 owns:
#      its own `attempts()` loop, which switches origin after two attempts where the shared
#      helper cannot, and the four calls that hand it `apt-get`, are exempt from rules 1 and 2
#      together, and from rule 5 with them, an exemption taking the whole verdict. That phase was
#      the Playwright step of ci.yaml's `dispatch` job until cybermonklabs/legion#2891 moved the
#      Dispatch browser suite into .github/workflows/dispatch-browser.yaml and wrote its install
#      as that script, which both of that workflow's install steps call; the exception moved with
#      it. Those calls carry `-o APT::Update::Error-Mode=any` all the same: rule 5's hole is a
#      hole in that loop too, and one option is the whole fix. `playwright install-deps` is that
#      same apt phase written as Playwright writes it, and is outside the verb list above for the
#      same reason; the browser download beside it, `playwright install`, is a site like any
#      other.
#   d. The credentialed publish jobs of release.yaml, release-envoy-listener.yaml and
#      release-envoy-infra.yaml, and every *.sh under .github/scripts/ such a job runs.
#      .github/scripts/check-release-credentials.sh pins the command set such a job may run, and
#      a retry wrapper there is a change to that pinned shape. A job is
#      credentialed by that check's own definition, WRITE_SCOPES: it holds `contents`, `id-token`,
#      `attestations` or `actions` at `write`, or names the deploy key. Their build jobs, which
#      hold no credential, are in scope like any other job.
#
#      The scripts are the second half, and they are read from the tree rather than listed: the
#      roots are the scripts a credentialed job's steps call, and the closure adds the scripts
#      those call in turn, which is the reading rule 8 already makes of a step. One script is in
#      that set today, .github/scripts/release-push.sh, with .github/scripts/release-bump.sh,
#      which it calls and which fetches nothing. release-push.sh's own fetches are the gh calls
#      of its `restart`, `release` and `release-section` commands — `gh run list`, `gh workflow
#      run`, `gh release view`, `gh release create`, `gh release upload` and `gh release edit` —
#      and nothing in the tree but those publish jobs runs one of them.
#
#      Retrying them means sourcing .github/scripts/ci-retry.sh into the publish job, and that is
#      what check-release-credentials.sh refuses: its rule 1(a) lets a credentialed job run no
#      dependency code, and its dependency_command() names as dependency code any command whose
#      path holds a `scripts/` component other than release-push.sh itself (`if
#      DEPENDENCY_PATH.search(path) and path != RELEASE_PUSH`). A `.` of ci-retry.sh is read
#      through that check's INTERPRETERS to exactly that path. Measured: sourcing the helper into
#      release.yaml's `publish` step 'pi-envoy: Create GitHub release' makes that check print
#      `job 'publish' holds a credential, but step '…' runs '.github/scripts/ci-retry.sh';
#      dependency code runs only in a job that holds none` and exit 1. Writing the `.` inside
#      release-push.sh instead would hide the same code from that check rather than keep it out
#      of the job, so these sites are left unretried.
#
# TWO SCOPINGS OF THE SUBJECT, which are not exceptions to it: neither site is a download of a
# tool, a toolchain or a dependency, so R1 never reached them.
#
#   * A fetch of 127.0.0.1, localhost or ::1 is a readiness poll of a process the step itself
#     started, not a download. Its loop is a wait for that process, and rule 2 leaves it alone for
#     the same reason.
#   * A call whose own failure is consumed as the answer rather than as a fault — `x=$(cmd …
#     2>/dev/null || true)`, whose result is then tested — is a probe. The one site in the tree is
#     mirror-ci-images.sh's `crane digest` of the package's own tag: that refusal is the signal to
#     copy, and five attempts there would sleep 150 s for every row the package lacks, which are
#     exactly the rows the copy after it then writes. The copy itself, and the digest read that
#     confirms it, do retry.
#
# One more scoping the tree states twice, at .github/scripts/playwright-install.sh and at
# e2e.yaml's package step: an `apt-get install` with no `-d` that follows an `apt-get install -d`
# in the same `run:` unpacks an archive already on disk and takes no network, so the download
# before it is the half that retries. An `apt-get` carrying a simulate flag (`-s`, `--simulate`,
# `--dry-run`, `--just-print`, `--no-act`, `--recon`) reads the lists on disk and fetches nothing
# either.
#
# `--list` prints the sites this check holds to the floor, one per line, as four tab-separated
# columns: the file, the line, the verb, and the step's name where the site is a step (`-` where
# it is not), sorted by file and then by line. Nothing else goes to standard output in that mode,
# so `wc -l` is the count and `cut -f1,2` is the file and line. dispatch://INFRA-1545's production
# measurement (R5, T10) reads this list at the merged head: it is the machine-readable subject of
# "a step R1 names". A site an exception or a scoping above removes is not on the list, by the
# same reasoning that leaves it unretried.
#
# Run from anywhere: .github/scripts/check-download-retry.sh
#                    .github/scripts/check-download-retry.sh --list
# CI runs it in ci.yaml's lint job, and its tests in ci.yaml's test job.
set -euo pipefail

cd "$(dirname "$0")/../.."

case "$#:${1:-}" in
  0: | 1:--list) ;;
  *)
    echo "::error file=.github/scripts/check-download-retry.sh::this check takes no argument, or \`--list\` to print the sites it holds to the floor. Run \`.github/scripts/check-download-retry.sh\` or \`.github/scripts/check-download-retry.sh --list\`." >&2
    exit 2
    ;;
esac
export DOWNLOAD_RETRY_MODE="${1:-}"

# The walk is workflow_paths.py's, beside this script in the repository it checks, which
# PYTHONPATH names: the files this check reads are the files the other gates read.
# PYTHONDONTWRITEBYTECODE keeps the import from leaving a __pycache__ in the tree.
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts python3 <<'PY'
import os
import re
import signal
import sys
from pathlib import Path

import yaml

from workflow_paths import walk

LIST_MODE = os.environ.get("DOWNLOAD_RETRY_MODE") == "--list"


class LineLoader(yaml.SafeLoader):
    """SafeLoader that records each mapping's line, so a problem names where to edit."""


class Mapping(dict):
    """A YAML mapping that remembers where each key was written."""

    lines: dict[str, int]


def _mapping_with_line(loader, node):
    mapping = Mapping(loader.construct_mapping(node, deep=False))
    mapping.lines = {
        key.value: key.start_mark.line + 1 for key, _ in node.value if hasattr(key, "value")
    }
    return mapping


LineLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping_with_line)

# The three Dockerfiles a CI job builds. Named rather than discovered: a Dockerfile elsewhere in
# the tree is not built by CI, and a rename of one of these must fail here rather than pass by
# matching nothing.
DOCKERFILES = (
    "packages/daemon/docker/worker.Dockerfile",
    "packages/envoy/docker/Dockerfile",
    "packages/envoy/infra/host/heldportwrapper/Dockerfile",
)
WORKFLOWS = Path(".github/workflows")
ACTIONS = Path(".github/actions")
SCRIPTS = Path(".github/scripts")
HELPER = ".github/scripts/ci-retry.sh"
# This file, named by the problems it raises about its own lists.
SELF = ".github/scripts/check-download-retry.sh"
# The `.test.sh` harnesses beside the checks are not read. A harness is not a job's download: the
# two network calls in the tree today are ci-fault-proxy.test.sh's `run_curl`, which drives one
# curl at the fault proxy precisely to watch a single unretried attempt, and
# release-push.test.sh's `gh api meta`, which compares the pinned host keys with what GitHub
# publishes and prints SKIP when the read fails. A retry at either would destroy what the test
# measures, and neither brings a tool, a toolchain or a dependency into a job.
#
# The widened gh table above does not add a third. The other gh words a harness writes reach no
# host: release-push.test.sh and check-private-names.test.sh each write a stub `gh` into the
# test's own bin directory and put that directory first on PATH, so the words are the stub's own
# `case` arms and the calls the script under test makes land on it; check-ci-filters.test.sh's
# `gh run cancel` is a line of a workflow fixture it writes for the checker to read, which
# nothing runs. A retry around any of them would wrap a shell function or a string.
TEST_HARNESS = ".test.sh"
# What names a script in a call: a word ending in `/<name>.sh`, wherever on the line it sits —
# `.github/scripts/x.sh`, `"$script_dir/x.sh"`, `"$GITHUB_ACTION_PATH/../../scripts/x.sh"`,
# `tag=$(.github/scripts/x.sh)`, `. .github/scripts/ci-retry.sh`. Only the name is read, and only
# a name SCRIPTS holds is followed: every script a CI job runs lives in that one directory, and
# the path a call writes in front of the name is one of half a dozen ways of reaching the same
# file. A `.test.sh` harness matches nothing here, since the name this reads holds no dot.
SCRIPT_CALL = re.compile(r"/([a-z0-9-]+\.sh)")
# The canonical inline retry of a Dockerfile RUN, already in the shape the comparison uses: its
# continuations joined and each run of blank space one space.
CANONICAL = (
    'retry() { b=$1; what=$2; shift 2; i=1; while :; do timeout "$b" "$@" && return 0; s=$?; '
    '[ "$i" -ge 5 ] && { echo "$what failed after $i attempts (last exit $s)" >&2; return "$s"; }; '
    'sleep "$(( 10 << (i - 1) ))"; i=$((i + 1)); done; }'
)
RETRY_FLOOR = 7
ATTEMPT_FLOOR = 5
# R1's floor on the ladder itself: the last attempt starts at least this many seconds after the
# first. The count alone does not say it, because `--retry-delay` and `--retry-max-time` each
# rewrite the waits between curl's own attempts (curl_ladder below).
LADDER_FLOOR = 120
# The wait curl takes before its first retry, and the ceiling its doubling reaches, read in
# curl's documentation at curl 8.14.1 (docs/cmdline-opts/retry.md): "it first waits one second
# and then for all forthcoming retries it doubles the waiting time until it reaches 10 minutes".
CURL_FIRST_WAIT = 1
CURL_WAIT_CEILING = 600
# Rule 8's arithmetic, the worst case of one retried call of the shared helper: it sleeps 10,
# 20, 40 and 80 seconds between its five attempts. What curl waits between its own attempts is
# no constant of this file, because `--retry-delay` and `--retry-max-time` each rewrite those
# waits; curl_ladder reads them off the call and computes the ladder from there.
HELPER_WAIT = 150
# What a job allows a step when the job writes no `timeout-minutes` of its own, read off
# GitHub's own documentation ("Workflow syntax for GitHub Actions", `jobs.<job_id>.timeout-
# minutes`): six hours. Rule 8 holds a step's arithmetic under its job's budget as well as
# under its own, and this is the budget a job that writes none is holding it under.
JOB_DEFAULT_MINUTES = 360
# What a `timeout` argument's suffix multiplies its number by, as timeout(1) reads it.
SUFFIX = {"": 1, "s": 1, "m": 60, "h": 3600, "d": 86400}
# The one Python function a `urllib.request` read of a script under .github/scripts/ goes through,
# and the one file that defines it.
PYTHON_READER = "read_retrying"
PYTHON_READER_FILE = ".github/scripts/check-ci-images.sh"
PYTHON_READ = re.compile(r"urllib\.request\.urlopen\s*\(|(?<![\w.])urlopen\s*\(")
PYTHON_DEF = re.compile(r"^(\s*)def\s+(\w+)\s*\(")

LOOPBACK = ("127.0.0.1", "localhost", "::1")
# What a word sheds to become the name of the program it runs.
PROGRAM = re.compile(r"^(\$\{?GITHUB_WORKSPACE\}?/)?(\.\.?/)*")
ASSIGNMENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*\+?=")
EXPRESSION = re.compile(r"\$\{\{.*?\}\}")
FUNCTION_WORD = re.compile(r"^[A-Za-z_][\w:.-]*\(\)$")
# A word of a `for` list that is itself and nothing else: no expansion, no glob, no quoting. A
# list of these is a pass count rule 8 can read off the loop (passes_of below).
LITERAL_WORD = re.compile(r"[A-Za-z0-9_@%+:,./=-]+")
# A `for ((i = 0; i < <bound>; i += <step>))`, the third shape rule 8 reads a pass count off: the
# counter is set to a number, tested against a bound, and raised by a number or by one. The bound
# is a number or `${#name[@]}`, the length of an array the body writes out (array_words below).
ARITHMETIC_FOR = re.compile(
    r"for\s*\(\(\s*([A-Za-z_]\w*)\s*=\s*(\d+)\s*;"
    r"\s*\1\s*(<=?)\s*([^;]+?)\s*;"
    r"\s*(?:\1\s*\+=\s*(\d+)|\1\s*\+\+|\+\+\s*\1)\s*\)\)"
)
ARRAY_LENGTH = re.compile(r"\$\{#([A-Za-z_]\w*)\[[@*]\]\}")
# What an array this check counts the words of may not hold, because each of these makes one
# written word into a number of words only the run knows.
UNCOUNTABLE_WORD = re.compile(r"\$\(|`|\[[@*]\]|[*?]")
# Transparent in command position, taking no argument of their own.
PASSTHROUGH = {"sudo", "exec", "command", "builtin", "nice", "ionice", "nohup", "time", "env",
               "stdbuf", "setsid", "doas", "xvfb-run"}
# Transparent, but the command they run is inside the script string behind `-c`. These are also
# the shells runs_heredoc reads a heredoc body as commands for, since a shell runs what it reads
# on its standard input.
INTERPRETERS = {"bash", "sh", "dash", "zsh"}
# Transparent, and the verb is what they run: a package runner resolves a binary the workspace's
# own install already provides.
RUNNERS = {"bunx", "npx", "pnpx", "pnpm-dlx"}
KEYWORDS = {"if", "then", "else", "elif", "fi", "do", "done", "while", "until", "for", "in",
            "case", "esac", "select", "function", "!", "[[", "]]", "{", "}"}
LOOP_WORDS = {"while", "until", "for", "select"}
APT_SUBCOMMANDS = {"update", "install", "download", "upgrade", "dist-upgrade", "full-upgrade",
                   "source", "build-dep", "reinstall"}
APT_SIMULATE = {"-s", "--simulate", "--just-print", "--dry-run", "--no-act", "--recon"}
APT_DOWNLOAD_ONLY = {"-d", "--download-only"}
# What an `apt-get update` option has to say for rule 5 to be satisfied, the value of an `-o`.
APT_ERROR_MODE = re.compile(r"apt::update::error-mode\s*=\s*any", re.IGNORECASE)
# A command that answers about itself reads nothing from the network, whatever its verb.
INFORMATIONAL = {"--version", "--help", "--usage"}
# The mise subcommands that fetch and say so: both exit 1 when the fetch fails.
MISE_SUBCOMMANDS = {"install", "use"}
# Rule 7's four: each exits 0 when it cannot resolve the tool, so none of them is a fetch verb
# and a retry around one protects nothing. The check reads them only to refuse that retry.
MISE_SILENT = {"x", "exec", "up", "upgrade"}
CRANE_SUBCOMMANDS = {"copy", "cp", "digest", "config", "pull", "export", "manifest", "blob"}
UV_SUBCOMMANDS = {"sync", "add", "pip", "tool", "python", "lock", "export"}
NPM_SUBCOMMANDS = {"install", "ci", "i", "install-test", "add"}
BUN_SUBCOMMANDS = {"install", "add", "i"}
# gh. Every subcommand of it but these reaches api.github.com over HTTPS, so the table is written
# the other way round from the rest: what is named here is what fetches nothing, and anything
# else gh runs is a fetch. GH_LOCAL answers out of the local config, the local extension
# directory or the binary itself: `gh config get`, `gh alias list`, `gh completion`, `gh
# extension list`, `gh help` and `gh version`. `gh auth` is half of each — `git-credential`,
# `setup-git`, `switch` and `token` read the stored token or write the local git config, while
# `login`, `logout`, `refresh` and `status` talk to the host — so its local actions are named
# apart. GH_GROUPS is the set of subcommands that take an action word of their own, which is
# gh's own grouping (`gh run list`, `gh release view`, `gh cache delete`): a verb of one of them
# is named with that action, and `gh api`, `gh browse` and `gh status`, which take an argument
# instead, are named by the subcommand alone.
GH_LOCAL = {"alias", "completion", "config", "extension", "help", "version"}
GH_AUTH_LOCAL = {"git-credential", "setup-git", "switch", "token"}
GH_GROUPS = {"accessibility", "attestation", "auth", "cache", "codespace", "gist", "gpg-key",
             "issue", "label", "org", "preview", "project", "release", "repo", "ruleset", "run",
             "search", "secret", "ssh-key", "variable", "workflow"}
# The write scopes check-release-credentials.sh counts as a credential (its WRITE_SCOPES), and the
# deploy key it reads beside them.
WRITE_SCOPES = ("contents", "id-token", "attestations", "actions")
DEPLOY_KEY = re.compile(r"secrets\s*(\.\s*DEPLOY_KEY\b|\[\s*['\"]DEPLOY_KEY['\"]\s*\])",
                        re.IGNORECASE)
RELEASE_WORKFLOWS = (
    ".github/workflows/release.yaml",
    ".github/workflows/release-envoy-listener.yaml",
    ".github/workflows/release-envoy-infra.yaml",
)

problems: list[str] = []


def problem(path: str, line: int, message: str) -> None:
    problems.append(f"::error file={path},line={line}::{message}")


# ---------------------------------------------------------------- reading the shell


def runs_heredoc(before: str) -> bool:
    """Whether the command a heredoc is fed to runs its body as shell. `before` is the line in
    front of the operator, where that command word sits: the last simple command of it, read with
    the Scanner below and stripped of the wrappers reduce_words strips, so a `sudo bash`, a
    `timeout 60 /bin/bash` and a bare `sh` are one answer. A shell of INTERPRETERS runs what it
    reads on its standard input, so a body of one is the commands the step itself runs; every
    other consumer — `cat`, a redirect into a file, `jq`, a `read`, the `python3 <<'PY'` rule 6
    reads as Python — takes its body as data.

    Two shapes of an interpreter take it as data as well, and both are the shell's own rule: a
    `-c <script>` runs the word behind the option and leaves the body on that script's standard
    input, and a script file argument does the same. `-s` is what overrides the second, since
    `bash -s a b` reads the script from standard input and hands the words after it to it as
    arguments, and a `-` written for that argument is standard input spelled out."""
    commands = Scanner(1).scan(before)
    words = reduce_words(commands[-1].words)[0] if commands else []
    if not words or PROGRAM.sub("", words[0]).rsplit("/", 1)[-1] not in INTERPRETERS:
        return False
    stdin = False
    rest = words[1:]
    while rest:
        word = rest.pop(0)
        if word == "--":
            # Past the separator the next word is the script argument, if the command writes one.
            return stdin or not rest or rest[0] == "-"
        if not word or word[0] not in "-+" or word == "-":
            return stdin or word == "-"
        flags = word[1:] if word[1:2] != "-" else ""
        if "c" in flags:
            return False
        if "s" in flags:
            stdin = True
        if flags[-1:] == "o" and rest:
            # `-o pipefail`, `+o posix`, `-euo pipefail`: the word after is that option's value
            # and no script of its own.
            rest.pop(0)
    return True


def without_heredocs(text: str) -> tuple[str, list[tuple[int, str]]]:
    """The text with every heredoc body its consumer reads as data blanked, and those bodies as
    (line, text) pairs. A body belongs to the command it is fed to: a shell runs what it reads on
    its standard input, so such a body stays in the text as the commands it is and the shell rules
    read it where it is written; every other consumer takes it as data, so the shell rules never
    read one, and the Python a `python3 <<'PY'` runs is read by rule 6 instead, off the bodies
    this returns. A heredoc inside a body a shell runs is read the same way, which is what the
    recursion below is for: a `cat <<'INNER'` written inside a `bash <<'OUTER'` is still data."""
    out: list[str] = []
    bodies: list[tuple[int, str]] = []
    lines = text.split("\n")
    index = 0
    while index < len(lines):
        line = lines[index]
        out.append(line)
        index += 1
        for match in re.finditer(r"<<-?\s*(['\"]?)([A-Za-z_][A-Za-z0-9_]*)\1", line):
            terminator = match.group(2)
            first = index + 1
            body: list[str] = []
            while index < len(lines) and lines[index].strip() != terminator:
                body.append(lines[index])
                index += 1
            if body and runs_heredoc(line[:match.start()]):
                inner, nested = without_heredocs("\n".join(body))
                out.extend(inner.split("\n"))
                bodies.extend((first + number - 1, body_line) for number, body_line in nested)
            else:
                out.extend("" for _ in body)
                bodies.extend((first + offset, body_line)
                              for offset, body_line in enumerate(body))
            if index < len(lines):
                out.append(lines[index])
                index += 1
    return "\n".join(out), bodies


def probe_spans(text: str) -> list[tuple[int, int]]:
    """The character spans of each `x=$(… )` whose failure is consumed as the answer: its own
    error silenced and `|| true` inside the substitution or right after it."""
    spans = []
    for match in re.finditer(r"[A-Za-z_][A-Za-z0-9_]*=\$\(", text):
        depth = 1
        index = match.end()
        while index < len(text) and depth:
            if text[index] == "(":
                depth += 1
            elif text[index] == ")":
                depth -= 1
            index += 1
        inner = text[match.end():index - 1]
        consumed = re.search(r"\|\|\s*true\s*$", inner.strip()) or re.match(
            r"\s*\|\|\s*true\b", text[index:index + 40]
        )
        if consumed and ("2>/dev/null" in inner or "2>&1" in inner):
            spans.append((match.start(), index))
    return spans


class Command:
    """One simple command of a shell body: its words, the line it starts on, the character offset
    it starts at, the loops it sits in — all of them, outermost first, with the innermost one
    beside them — and whether a `|` is what ended it, which says its status is not the
    pipeline's and so is rule 9's question."""

    def __init__(self, words: list[str], line: int, offset: int) -> None:
        self.words = words
        self.line = line
        self.offset = offset
        self.loops: list[int] = []
        self.loop: int | None = None
        self.piped = False


class Scanner:
    """One quote-aware pass over a shell body, collecting each simple command with its line. The
    shape is .github/scripts/check-release-credentials.sh's Scanner, which reads a `run:` the same
    way; this one keeps the keywords, so a loop and a function definition are visible, and carries
    each command's line and offset, so a problem names where to edit."""

    def __init__(self, first_line: int) -> None:
        self.commands: list[Command] = []
        self.words: list[str] = []
        self.word = ""
        self.started = False
        self.at_word_start = True
        self.redirect_target = False
        self.line = first_line
        self.word_line = first_line
        self.word_offset = 0
        self.command_line = first_line
        self.command_offset = 0
        self.open_command = False

    def add(self, char: str, offset: int) -> None:
        if not self.started:
            self.word_line = self.line
            self.word_offset = offset
        if not self.open_command:
            self.command_line = self.line
            self.command_offset = offset
            self.open_command = True
        self.word += char
        self.started = True
        self.at_word_start = False

    def end_word(self) -> None:
        brace = False
        if self.started and not self.redirect_target:
            self.words.append(self.word)
            # `{` and `}` are reserved words, so `fetch() { curl …; }` holds a command of its own
            # after the brace: without this the curl would read as an argument of `fetch()`.
            brace = self.word in ("{", "}")
        if self.started:
            self.redirect_target = False
        self.word = ""
        self.started = False
        self.at_word_start = True
        if brace:
            self.end_command()

    def end_command(self, piped: bool = False) -> None:
        self.end_word()
        if self.words:
            command = Command(self.words, self.command_line, self.command_offset)
            command.piped = piped
            self.commands.append(command)
        self.words = []
        self.redirect_target = False
        self.open_command = False

    def scan(self, text: str) -> list[Command]:
        quote = None
        open_subs: list[tuple[str, str | None]] = []
        i = 0
        while i < len(text):
            char = text[i]
            after = text[i + 1:i + 2]
            if char == "\n":
                self.line += 1
            if quote == "'":
                if char == "'":
                    quote = None
                else:
                    self.add(char, i)
            elif char == "`":
                self.end_command()
                if open_subs and open_subs[-1][0] == "tick":
                    quote = open_subs.pop()[1]
                else:
                    open_subs.append(("tick", quote))
                    quote = None
            elif char == "$" and after == "(":
                self.add(char, i)
                self.end_command()
                open_subs.append(("paren", quote))
                quote = None
                i += 1
            elif quote == '"':
                if char == '"':
                    quote = None
                elif char == "\\" and after in ("$", "`", '"', "\\"):
                    self.add(after, i)
                    i += 1
                elif char == "\\" and after == "\n":
                    self.line += 1
                    i += 1
                else:
                    self.add(char, i)
            elif char == "#" and self.at_word_start:
                end = text.find("\n", i)
                i = len(text) if end < 0 else end
                continue
            elif char == "\\" and after == "\n":
                self.line += 1
                i += 1
            elif char == "\\" and after:
                self.add(after, i)
                i += 1
            elif char in "'\"":
                quote = char
                self.started = True
                self.at_word_start = False
                if not self.open_command:
                    self.command_line = self.line
                    self.command_offset = i
                    self.open_command = True
            elif char == "(" and after == ")" and self.started:
                # `name()` opens a function; the parentheses belong to the word, not to a subshell.
                self.add("(", i)
                self.add(")", i)
                i += 1
            elif char == "(" or (char in "<>" and after == "("):
                if char != "(":
                    i += 1
                self.end_command()
                open_subs.append(("paren", None))
            elif char == ")":
                self.end_command()
                if open_subs and open_subs[-1][0] == "paren":
                    quote = open_subs.pop()[1]
            elif char in "<>":
                self.end_word()
                self.redirect_target = True
                while text[i + 1:i + 2] in ("<", ">", "&", "|"):
                    i += 1
            elif char in ";&|\n":
                # A `|` that ends this command pipes its output into the next one, and the
                # pipeline's status is that next one's where pipefail is off (rule 9). `|&`
                # pipes standard error with it and is as much a pipeline; the `&` of it then
                # ends a command with no words, which adds none. `||` is a logical OR and no
                # pipeline at all, and its second `|` ends a wordless command the same way.
                self.end_command(char == "|" and after != "|")
            elif char in " \t":
                self.end_word()
            else:
                self.add(char, i)
            i += 1
        self.end_command()
        return self.commands


class Loops:
    """What rule 8 reads of one shell body: each loop by its own line, with the line its `done`
    sits on, the passes it makes and the seconds it waits between two of them; the line of the
    loop in each function that holds one — the loop a call of that function reaches; and where
    each function this body defines is written and where it is called, which is how many times
    a call inside it runs. A pass count is None where the loop does not write one."""

    def __init__(self) -> None:
        self.function_loop: dict[str, int] = {}
        self.passes: dict[int, int | None] = {}
        self.sleep: dict[int, int] = {}
        self.extent: dict[int, int] = {}
        self.function_span: dict[str, tuple[int, int]] = {}
        self.function_calls: dict[str, list[int]] = {}

    def enclosing(self, line: int) -> list[int]:
        """The loops around one line of this body, outermost first. A command carries its own
        stack; this answers for a line read off the text, which is how a script call is found."""
        return sorted(start for start, end in self.extent.items() if start <= line <= end)

    def function_of(self, line: int) -> str | None:
        """The function whose body holds this line, the innermost where more than one does."""
        found: str | None = None
        for name, (start, end) in self.function_span.items():
            if start <= line <= end and (found is None or start > self.function_span[found][0]):
                found = name
        return found


def array_words(text: str, name: str) -> int | None:
    """The words an array this body writes out holds — `lists=(a b c)` is three. None where the
    body writes that name anywhere else than one `name=(…)`, where the parentheses do not close,
    or where one written word can become more than one at run time: a `$(…)`, a backtick, another
    array's `[@]`, a glob. The count is read rather than the words, so a `"$x/y"` is the one word
    it is."""
    opens = list(re.finditer(rf"(?m)^\s*{re.escape(name)}\+?=\(", text))
    if len(opens) != 1:
        return None
    index = opens[0].end()
    depth = 0
    quote: str | None = None
    while index < len(text):
        char = text[index]
        if quote:
            if char == quote:
                quote = None
        elif char in "'\"":
            quote = char
        elif char == "(":
            depth += 1
        elif char == ")":
            if depth == 0:
                break
            depth -= 1
        index += 1
    else:
        return None
    inner = text[opens[0].end():index]
    if UNCOUNTABLE_WORD.search(inner):
        return None
    return sum(len(command.words) for command in Scanner(1).scan(inner))


def passes_of(text: str, command: Command) -> int | None:
    """The passes a loop makes, where the loop writes a count this check is sure of: a `for x in
    1 2 3 4 5 6 7`, whose list is literal words; a `for x in $(seq 1 7)`, whose list is a count
    of them; and a `for ((i = 0; i < <bound>; i += <step>))`, whose bound is a number or the
    length of an array this body writes out. Everything else — a `while`, an `until`, a `for`
    over a variable or over the lines a command printed — writes no count, and reads as None
    rather than as one pass."""
    if arithmetic := ARITHMETIC_FOR.match(text[command.offset:]):
        first = int(arithmetic.group(2))
        written = arithmetic.group(4).strip()
        step = int(arithmetic.group(5)) if arithmetic.group(5) else 1
        if step <= 0:
            return None
        if re.fullmatch(r"\d+", written):
            bound = int(written)
        elif length := ARRAY_LENGTH.fullmatch(written):
            counted = array_words(text, length.group(1))
            if counted is None:
                return None
            bound = counted
        else:
            return None
        if arithmetic.group(3) == "<=":
            bound += 1
        return max(0, -(-(bound - first) // step))
    match = re.match(r"for\s+[A-Za-z_]\w*\s+in\s+([^\n;]*)", text[command.offset:])
    if not match:
        return None
    listed = match.group(1).strip()
    if seq := re.fullmatch(r"\$\(\s*seq\s+([\d\s]+)\)", listed):
        # `seq <last>` counts from 1, and `seq <first> <last>` from <first>. A step argument, or
        # a bound that is not a number, is a shape this check does not read.
        numbers = [int(number) for number in seq.group(1).split()]
        if len(numbers) == 1:
            return numbers[0]
        if len(numbers) == 2:
            return numbers[1] - numbers[0] + 1 if numbers[1] >= numbers[0] else 0
        return None
    words = listed.split()
    if words and all(LITERAL_WORD.fullmatch(word) for word in words):
        return len(words)
    return None


def structure(commands: list[Command], text: str) -> Loops:
    """Marks each command with the loops it sits in, and returns what rule 8 reads of them: each
    loop's extent, passes and own wait between two passes, the loop each function holds, and
    where each function this body defines is written and called."""
    loops: list[int] = []
    function: str | None = None
    function_depth = 0
    function_line = 0
    last = 0
    read = Loops()
    for command in commands:
        first = command.words[0]
        if first == "done" and loops:
            read.extent[loops.pop()] = command.line
        command.loops = list(loops)
        command.loop = loops[-1] if loops else None
        last = max(last, command.line)
        if (command.loop is not None and first == "sleep" and len(command.words) == 2
                and re.fullmatch(r"\d+", command.words[1])):
            # The loop's own wait between two passes, where it is written as a number. A
            # `sleep "$(( 10 << (i - 1) ))"` is computed rather than written and adds nothing,
            # and where a loop writes more than one the longest is what a pass can wait.
            seconds = int(command.words[1])
            read.sleep[command.loop] = max(read.sleep.get(command.loop, 0), seconds)
        if function is not None:
            if command.loop is not None and function not in read.function_loop:
                read.function_loop[function] = command.loop
            function_depth += command.words.count("{") - command.words.count("}")
            if function_depth <= 0:
                read.function_span[function] = (function_line, command.line)
                function = None
        elif FUNCTION_WORD.fullmatch(first):
            function = first[:-2]
            function_line = command.line
            function_depth = command.words.count("{") - command.words.count("}")
        if first in LOOP_WORDS:
            loops.append(command.line)
            read.passes[command.line] = passes_of(text, command)
    # A loop or a function whose closing word this body never writes runs to the end of it.
    for line in loops:
        read.extent[line] = last
    if function is not None:
        read.function_span[function] = (function_line, last)
    for command in commands:
        span = read.function_span.get(command.words[0])
        if span and not span[0] <= command.line <= span[1]:
            read.function_calls.setdefault(command.words[0], []).append(command.line)
    return read


def reduce_words(words: list[str]) -> tuple[list[str], str | None, bool, int | None]:
    """The words of the command that actually runs, the retry wrapping it (`retry_fetch`, the one
    helper, or `retry`, a Dockerfile's inline definition), whether something bounds it, and the
    seconds of that bound where it is written as a number — rule 8's `<bound>`."""
    retry = None
    bounded = False
    bound: int | None = None
    words = list(words)
    while words:
        first = words[0]
        if ASSIGNMENT.match(first) or first in PASSTHROUGH or first in KEYWORDS:
            words.pop(0)
            continue
        if first == "timeout":
            words.pop(0)
            while words and words[0].startswith("-"):
                words.pop(0)
                if words and re.fullmatch(r"\d+", words[0]):
                    break
            if words and (match := re.fullmatch(r"(\d+)([smhd]?)", words[0])):
                words.pop(0)
                bounded = True
                if bound is None:
                    bound = int(match.group(1)) * SUFFIX[match.group(2)]
            continue
        if first == "retry_fetch":
            retry = "retry_fetch"
            words.pop(0)
            # `<what> <bound> --`: the bound is the one of those words that is a count of
            # seconds, and a call written without one bounds no attempt.
            for _ in range(3):
                if not words:
                    break
                word = words.pop(0)
                if re.fullmatch(r"[1-9]\d*", word):
                    bounded = True
                    if bound is None:
                        bound = int(word)
                if word == "--":
                    break
            continue
        if first == "retry" and len(words) > 2 and re.fullmatch(r"\d+", words[1]):
            retry = "retry"
            bounded = True
            if bound is None:
                bound = int(words[1])
            del words[:3]
            continue
        break
    return words, retry, bounded, bound


def subcommand(words: list[str]) -> str | None:
    """The first word that is not an option."""
    return next((word for word in words if not word.startswith("-")), None)


def gh_verb_of(rest: list[str]) -> str | None:
    """The verb a `gh` call runs, named `gh <subcommand>` or `gh <subcommand> <action>`, or None
    where that subcommand reads and writes nothing over the network. Every gh subcommand but
    GH_LOCAL and the four local `gh auth` actions reaches api.github.com over HTTPS, which is the
    read `gh api` already makes, so the default here is that a gh call is a fetch: a subcommand
    nobody listed is held to the floor rather than passing unseen."""
    words = [word for word in rest if not word.startswith("-")]
    sub = words[0] if words else None
    if sub is None or sub in GH_LOCAL:
        return None
    action = words[1] if len(words) > 1 else None
    if sub == "auth" and action in GH_AUTH_LOCAL:
        return None
    if sub not in GH_GROUPS:
        # `api`, `browse` and `status` take no action word: the word after them is an argument
        # (`gh api repos/owner/name`), and naming it in the verb would name every route.
        return f"gh {sub}"
    return f"gh {sub} {action}" if action else f"gh {sub}"


def verb_of(words: list[str]) -> str | None:
    """The fetch verb these words run, named as the list and the messages name it, or None."""
    if not words:
        return None
    # Its own options, which stop at a `--`: past that separator the words belong to a command
    # this one runs (`mise x "$pin" -- omp --version` resolves the tool and then asks it, and
    # that trailing `--version` says nothing about this command).
    own = words[1:words.index("--")] if "--" in words[1:] else words[1:]
    if any(word in INFORMATIONAL for word in own):
        return None
    name = PROGRAM.sub("", words[0]).rsplit("/", 1)[-1]
    rest = words[1:]
    if name in RUNNERS:
        return verb_of(rest)
    if name in ("curl", "wget") and any(word in ("-V", "-h") for word in rest):
        return None
    if name == "curl":
        return "curl"
    if name == "wget":
        return "wget"
    if name in ("apt-get", "apt", "aptitude"):
        sub = subcommand(rest)
        if sub not in APT_SUBCOMMANDS or any(word in APT_SIMULATE for word in rest):
            return None
        return f"apt-get {sub}"
    if name in ("pip", "pip3") and subcommand(rest) == "install":
        return "pip install"
    if name == "uv":
        sub = subcommand(rest)
        return f"uv {sub}" if sub in UV_SUBCOMMANDS else None
    if name == "bun":
        sub = subcommand(rest)
        return f"bun {sub}" if sub in BUN_SUBCOMMANDS else None
    if name in ("npm", "pnpm", "yarn"):
        sub = subcommand(rest)
        return f"{name} {sub}" if sub in NPM_SUBCOMMANDS else None
    if name == "playwright":
        # `install-deps` is the apt phase, which dispatch://INFRA-1443 owns: it is not this one.
        return "playwright install" if rest and rest[0] == "install" else None
    if name == "go":
        while len(rest) > 1 and rest[0] == "-C":
            del rest[:2]
        if rest[:2] == ["mod", "download"]:
            return "go mod download"
        if rest[:1] == ["install"]:
            return "go install"
        return None
    if name == "mise":
        sub = subcommand(rest)
        return f"mise {sub}" if sub in MISE_SUBCOMMANDS else None
    if name == "crane":
        sub = subcommand(rest)
        return f"crane {sub}" if sub in CRANE_SUBCOMMANDS else None
    if name == "docker":
        return "docker pull" if subcommand(rest) == "pull" else None
    if name == "gh":
        return gh_verb_of(rest)
    if name == "pulumi" and rest[:2] == ["plugin", "install"]:
        return "pulumi plugin install"
    return None


def silent_verb_of(words: list[str]) -> str | None:
    """The rule 7 mise subcommand these words run, or None. These are no fetch verbs: each exits
    0 when it cannot resolve the tool, so what the check does with one is refuse a retry."""
    if not words:
        return None
    own = words[1:words.index("--")] if "--" in words[1:] else words[1:]
    if any(word in INFORMATIONAL for word in own):
        return None
    if PROGRAM.sub("", words[0]).rsplit("/", 1)[-1] != "mise":
        return None
    sub = subcommand(words[1:])
    return f"mise {sub}" if sub in MISE_SILENT else None


def verb_in(words: list[str]) -> tuple[str, list[str]] | None:
    """The first fetch verb among these words, wherever it sits: what a call hands a loop that
    runs `"$@"`."""
    for index in range(len(words)):
        verb = verb_of(words[index:])
        if verb:
            return verb, words[index:]
    return None


def curl_options(words: list[str]) -> tuple[int | None, bool, bool, bool]:
    """What a curl carries: its `--retry` count, whether it passes `--retry-all-errors`, whether
    it bounds each attempt with `--max-time`, and whether a persistent error fails it (`--fail`,
    the `f` of a short cluster, or a read of the status it wrote out)."""
    retries = None
    all_errors = False
    bounded = False
    fails = "%{http_code}" in " ".join(words)
    index = 1
    while index < len(words):
        word = words[index]
        value = None
        if "=" in word and word.startswith("--"):
            word, _, value = word.partition("=")
        if word == "--retry":
            if value is None and index + 1 < len(words):
                value = words[index + 1]
            if value and value.isdigit():
                retries = int(value)
        elif word == "--retry-all-errors":
            all_errors = True
        elif word in ("--max-time", "-m"):
            bounded = True
        elif word in ("--fail", "--fail-with-body", "--fail-early"):
            fails = True
        elif re.fullmatch(r"-[A-Za-z]*f[A-Za-z]*", word):
            fails = True
        index += 1
    return retries, all_errors, bounded, fails


def curl_bound(words: list[str]) -> int | None:
    """The seconds a curl gives each of its own attempts, its `--max-time`, where that option
    carries a number: rule 8's `<bound>` for a curl that makes the attempts itself."""
    for index, word in enumerate(words):
        value = None
        if word.startswith("--max-time="):
            value = word.partition("=")[2]
        elif word in ("--max-time", "-m") and index + 1 < len(words):
            value = words[index + 1]
        if value and re.fullmatch(r"\d+", value):
            return int(value)
    return None


def curl_waits(words: list[str]) -> tuple[int | None, int | None, bool]:
    """What this curl writes for `--retry-delay` and `--retry-max-time`, in seconds, and whether
    every value it writes for them is a number this check can read. A value that is not — a
    variable, an expression — leaves the ladder unreadable rather than default, since the one
    thing this check must not do with these two options is assume."""
    delay: int | None = None
    max_time: int | None = None
    readable = True
    for index, word in enumerate(words):
        value = None
        if "=" in word and word.startswith("--"):
            word, _, value = word.partition("=")
        if word not in ("--retry-delay", "--retry-max-time"):
            continue
        if value is None and index + 1 < len(words):
            value = words[index + 1]
        if not value or not re.fullmatch(r"\d+", value):
            readable = False
            continue
        if word == "--retry-delay":
            delay = int(value)
        else:
            max_time = int(value)
    return delay, max_time, readable


def curl_ladder(retries: int, delay: int | None, max_time: int | None) -> int:
    """The seconds between the first of curl's own attempts and the last, as curl makes them —
    R1's floor is that this is at least LADDER_FLOOR. The rule is curl's own, at curl 8.14.1:
    the wait before the first retry is one second and doubles for each of the rest up to ten
    minutes; `--retry-delay <n>` replaces that with a fixed n, and `--retry-delay 0` asks for
    the default back-off again; `--retry-max-time <n>` lets a retry start only while the timer
    reset at the first attempt has not reached n, and `--retry-max-time 0` sets no limit. The
    attempts themselves are taken as instant, which makes this the longest ladder the options
    allow: the real one is shorter, because every second an attempt spends is a second of that
    timer."""
    if delay:
        waits = [delay] * retries
    else:
        waits = [min(CURL_FIRST_WAIT << index, CURL_WAIT_CEILING) for index in range(retries)]
    elapsed = 0
    for wait in waits:
        if max_time and elapsed >= max_time:
            break
        elapsed += wait
    return elapsed


def apt_error_mode(words: list[str]) -> bool:
    """Whether this `apt-get update` fails when an index read fails, rather than warning and
    exiting 0: `-o APT::Update::Error-Mode=any`, in any spelling apt takes for an option. Option
    names are matched without regard to case, as apt reads them."""
    for index, word in enumerate(words):
        lowered = word.lower()
        value = None
        if lowered in ("-o", "--option"):
            value = words[index + 1] if index + 1 < len(words) else None
        elif lowered.startswith("-o"):
            value = word[2:].lstrip("=")
        elif lowered.startswith("--option="):
            value = word.partition("=")[2]
        if value and APT_ERROR_MODE.fullmatch(value.strip()):
            return True
    return False


class Site:
    """One network fetch: where it is, what verb it uses, what retries it, what bounds each of
    its attempts, and what it is in. `piped` is rule 9's: the fetch sits left of a `|` in a
    shell that leaves pipefail off, so the pipeline's status is its last command's and this
    fetch's failure is not read. `silent` marks rule 7's shape instead: a `mise x`, `mise
    exec`, `mise up` or `mise upgrade` with a retry around it, which is no fetch to hold to the
    floor but a retry to refuse. `loops` is rule 8's: every loop this call sits in, outermost
    first, and `loop` is the innermost of them, which is rule 2's. `factor` is how many times
    the body runs this call once those loops, the function holding it and the loops around that
    function's own calls are all read; `wait` is what those loops sleep between their passes;
    and `blocked` names the one loop whose passes this check could neither read nor find
    declared, which is the case it refuses rather than counts."""

    def __init__(self, path: str, line: int, verb: str, step: str, words: list[str],
                 retry: str | None, bounded: bool, loops: list[int], bound: int | None = None,
                 silent: bool = False, piped: bool = False) -> None:
        self.path = path
        self.line = line
        self.verb = verb
        self.step = step
        self.words = words
        self.retry = retry
        self.bounded = bounded
        self.loops = loops
        self.loop = loops[-1] if loops else None
        self.bound = bound
        self.silent = silent
        self.piped = piped
        self.factor: int | None = 1
        self.wait = 0
        self.blocked: int | None = None


class Call:
    """One script a shell body runs: the line the call is written on, how many times that body
    runs it, what the loops around it sleep between their passes, and the loop whose passes this
    check could not read where `factor` is None because of one."""

    def __init__(self, line: int, factor: int | None, wait: int, loop: int | None) -> None:
        self.line = line
        self.factor = factor
        self.wait = wait
        self.loop = loop

    def beside(self, other: "Call") -> "Call":
        """This call and another of the same script in one body: the body runs it both times, so
        the counts add, and one call this check cannot count leaves the pair uncounted."""
        if self.factor is None:
            return self
        if other.factor is None:
            return other
        return Call(self.line, self.factor + other.factor, self.wait + other.wait, None)


class Uncounted:
    """One thing a step waits for that this check can put no figure on: a retried call, or a
    script the body runs, inside a loop whose passes it can neither read nor find declared.
    `what` is the verb, or the script's path; `one` is what one of them costs, left None for a
    script, whose arithmetic is read when the refusal is written."""

    def __init__(self, path: str, line: int, what: str, loop: int | None,
                 one: int | None) -> None:
        self.path = path
        self.line = line
        self.what = what
        self.loop = loop
        self.one = one


def one_call(site: Site) -> int:
    """The wall time one call of this site takes to exhaust its attempts. Five at the helper's
    own bound with its sleeps of 10, 20, 40 and 80 s between them, or, where curl makes the
    attempts itself, one per `--retry` and one more at curl's `--max-time` with the ladder
    curl_ladder computes between them — the same ladder rule 1 holds that curl to, so the two
    rules read one figure. A curl outside that floor is read at the floor, which is the ladder
    it will make once rule 1's refusal is answered. A call whose bound is not written as a
    number adds nothing: there is no arithmetic to do, and rule 4 is what refuses an unbounded
    attempt."""
    if site.bound is None or site.silent:
        return 0
    if site.retry == "retry_fetch":
        return ATTEMPT_FLOOR * site.bound + HELPER_WAIT
    if site.verb == "curl" and site.retry is None:
        retries, _, _, _ = curl_options(site.words)
        delay, max_time, readable = curl_waits(site.words)
        if retries is None or retries < RETRY_FLOOR or not readable:
            retries, delay, max_time = RETRY_FLOOR, None, None
        return (retries + 1) * site.bound + curl_ladder(retries, delay, max_time)
    return 0


def declared_arithmetic(site: Site) -> "DeclaredArithmetic | None":
    """The declaration that carries this site's own arithmetic, where one is written."""
    return next((entry for entry in DECLARED_LOOPS if entry.covers(site)), None)


def around(loops: Loops, lines: list[int]) -> tuple[int | None, int, int | None]:
    """Folding a cost at one position outwards through the loops around it: how many times they
    run it, what they sleep between their passes, and the loop whose passes could not be read
    where there is one. Each loop multiplies what is inside it and adds its own waits —
    `passes * inside + (passes - 1) * sleep` — so a call in a loop inside a loop is multiplied
    by both. A loop whose count this check never made stops the fold rather than taking a
    figure from somewhere else: that site is refused below, or answered in seconds by a
    DeclaredArithmetic entry, because a pass count nobody read is a pass count nobody checks."""
    factor = 1
    wait = 0
    for line in reversed(lines):
        passes = loops.passes.get(line)
        if passes is None:
            return None, 0, line
        wait = passes * wait + max(passes - 1, 0) * loops.sleep.get(line, 0)
        factor *= passes
    return factor, wait, None


def times_run(loops: Loops, line: int, stack: list[int] | None = None,
              seen: frozenset[str] = frozenset()) -> tuple[int | None, int, int | None]:
    """How many times a body runs what is written at `line`, what the loops around it sleep, and
    the loop that stopped the reading where one did. The loops around the line are the first
    half; the second is the function holding it, which runs once for each of its own calls, each
    of those multiplied the same way again. `seen` stops a function that reaches itself, which
    the shell would run until something failed and which must not hang this check."""
    factor, wait, blocked = around(loops, loops.enclosing(line) if stack is None else stack)
    if factor is None:
        return None, 0, blocked
    holder = loops.function_of(line)
    calls = loops.function_calls.get(holder, ()) if holder is not None else ()
    # A function this body defines and never calls is counted once, as a command of the body
    # itself is: its call is written in whatever sources this file.
    if holder is None or holder in seen or not calls:
        return factor, wait, None
    outer = 0
    outer_wait = 0
    for call in calls:
        times, waited, blocked = times_run(loops, call, None, seen | {holder})
        if times is None:
            return None, 0, blocked
        outer += times
        outer_wait += waited
    return outer * factor, outer * wait + outer_wait, None


def arithmetic_of(site: Site) -> int | None:
    """Rule 8's arithmetic for one site: one call of it for each time the step runs it, with
    the waits of the loops that run it. Where one of those loops makes a count this check
    cannot read, the figure is the DeclaredArithmetic entry's own seconds, and None where no
    such entry covers the site — which is a site refused below rather than a budget
    undercounted here."""
    one = one_call(site)
    if not one:
        return 0
    if site.factor is not None:
        return site.factor * one + site.wait
    entry = declared_arithmetic(site)
    if entry is None:
        return None
    entry.used += 1
    if site.blocked is not None:
        entry.loops.add(site.blocked)
    if entry.seconds < one:
        entry.below[site.line] = one
    return entry.seconds


def carried(call: Call, cost: int | None) -> int | None:
    """What a script costs the body that calls it: its own arithmetic once for each time that
    body runs it, with the waits of the loops around the call where there is anything to wait
    for."""
    if cost is None or call.factor is None:
        return None
    return call.factor * cost + (call.wait if cost else 0)


def added(total: int | None, more: int | None) -> int | None:
    """Two arithmetics summed, where either may be the unreadable one: a step holding a site
    this check cannot count has no figure at all, and a figure short by one loop would be worse
    than none."""
    return None if total is None or more is None else total + more


def scripts_called(text: str, first_line: int = 1) -> dict[str, list[int]]:
    """The scripts under .github/scripts/ a shell body runs, and the lines their calls are
    written on, whose own retried calls rule 8 counts as this body's: a step that runs a script
    waits for every attempt that script makes, once for each pass of the loop the step wraps
    around the call.

    Read off the text by the path each call writes, with backslash continuations joined and
    comment lines left out, rather than off the commands this file scans elsewhere: a call
    reaches a script in command position, inside a `$(…)`, as the value of an assignment or
    behind a `source` or `.`, and the path is the one thing all of those write. A name SCRIPTS
    does not hold is nothing this check reads.

    A heredoc body a shell runs is read here with the rest of the text, and one its consumer
    takes as data is left out with the comments, which is the reading without_heredocs makes:
    a `bash <<EOF` body is the step's own commands, so a script it calls is a script the step
    waits for, and the paths a `cat`, a `jq` or a `python3 <<'PY'` body spells are paths
    something prints rather than runs — check-ci-images.sh's `MIRROR_SCRIPT`, which names the
    script a reader is told to run, and this file's own EXEMPTIONS and DECLARED_LOOPS, which
    name the files their entries cover. Counting those as calls gave the lint step that runs
    this check 15000 s of other scripts' arithmetic, and the ci-images job's two check steps
    mirror-ci-images.sh's 3000 s, none of which those steps wait for."""
    found: dict[str, list[int]] = {}
    lines = without_heredocs(text)[0].split("\n")
    index = 0
    while index < len(lines):
        line = lines[index]
        start = first_line + index
        while line.endswith("\\") and index + 1 < len(lines):
            index += 1
            line = line[:-1] + " " + lines[index]
        index += 1
        if line.lstrip().startswith("#"):
            continue
        for name in SCRIPT_CALL.findall(line):
            path = str(SCRIPTS / name)
            if path in script_files and start not in found.setdefault(path, []):
                found[path].append(start)
    return found


def calls_of(text: str, first_line: int, loops: Loops) -> dict[str, Call]:
    """The scripts a shell body runs, each with how many times that body runs it: the loops
    around the call, and the calls of the function the call sits in."""
    found: dict[str, Call] = {}
    for path, lines in scripts_called(text, first_line).items():
        for line in lines:
            factor, wait, blocked = times_run(loops, line)
            call = Call(line, factor, wait, blocked)
            found[path] = call if path not in found else found[path].beside(call)
    return found


def merge_calls(into: dict[str, Call], more: dict[str, Call]) -> None:
    """The calls of a second body folded into the first's: a composite action whose two steps
    each run one script runs it twice, and a `uses:` of that action waits for both."""
    for path, call in more.items():
        into[path] = call if path not in into else into[path].beside(call)


def blocked_calls(path: str, calls: dict[str, Call]) -> list[Uncounted]:
    """The scripts one body runs inside a loop whose passes this check cannot read: whoever runs
    that body waits for the whole of each script once per pass, and the passes are unread."""
    return [Uncounted(path, call.line, target, call.loop, None)
            for target, call in sorted(calls.items()) if call.factor is None]


def commands_of(body: str, first_line: int) -> tuple[list[Command], Loops,
                                                     list[tuple[int, int]]]:
    """The simple commands of a shell body, what rule 8 reads of its loops, and the probe
    spans."""
    text, _ = without_heredocs(EXPRESSION.sub("EXPR", body))
    commands = Scanner(first_line).scan(text)
    return commands, structure(commands, text), probe_spans(text)


def sites_of(path: str, body: str, first_line: int, step: str,
             pipefail: bool = True) -> tuple[list[Site], Loops]:
    """Every fetch of one shell body, and what rule 8 read of that body's loops. `pipefail` is
    whether the shell that runs it sets `-o pipefail`: where it does, a pipeline carries the
    failure of every command in it and rule 9 has nothing to refuse. That is the default,
    because a *.sh and a Dockerfile RUN are read with no step's `shell:` to ask about and each
    sets its own flags."""
    commands, loops, probes = commands_of(body, first_line)
    found: list[Site] = []
    for command in commands:
        if any(start <= command.offset < end for start, end in probes):
            continue
        found += sites_of_command(path, command, loops, step)
    if pipefail:
        for site in found:
            site.piped = False
    found = unpacks_dropped(found)
    # A site whose loops write no count leaves whatever step runs it without a figure here:
    # arithmetic_of asks the DeclaredArithmetic entries for its seconds, and uncounted_of below
    # hands the step's budget the ones no entry answers.
    for site in found:
        site.factor, site.wait, site.blocked = times_run(loops, site.line, site.loops)
    return found, loops


def uncounted_of(found: list[Site]) -> list[Uncounted]:
    """The sites of one body this check can put no figure on: a retried call inside a loop
    whose passes it cannot read, and for which no DeclaredArithmetic
    entry writes the seconds either. A step whose budget reaches one of these is refused in
    the loop's place, and a loop no budget reaches costs nobody anything and is left alone —
    rule 2 is what reads a loop wherever it sits."""
    return [Uncounted(site.path, site.line, site.verb, site.blocked, one_call(site))
            for site in found
            if site.factor is None and one_call(site) and site.path not in DOCKERFILES
            and declared_arithmetic(site) is None]


def unpacks_dropped(found: list[Site]) -> list[Site]:
    """The sites with the apt unpack phases taken out. An `apt-get install` with no `-d` that
    follows an `apt-get install -d` in the same body installs an archive already on disk and
    takes no network: the download before it is the half that retries, which is what
    .github/scripts/playwright-install.sh and e2e.yaml's package step each say where they do
    it."""
    downloaded = False
    kept = []
    for site in found:
        if site.verb == "apt-get install":
            only = any(word in APT_DOWNLOAD_ONLY for word in site.words)
            if downloaded and not only:
                continue
            downloaded = downloaded or only
        kept.append(site)
    return kept


def sites_of_command(path: str, command: Command, loops: Loops,
                     step: str, retry: str | None = None, bounded: bool = False,
                     depth: int = 0, bound: int | None = None) -> list[Site]:
    words, wrapped, wrapped_bound, wrapped_seconds = reduce_words(command.words)
    retry = retry or wrapped
    bounded = bounded or wrapped_bound
    if bound is None:
        bound = wrapped_seconds
    if not words or depth > 3:
        return []
    name = PROGRAM.sub("", words[0]).rsplit("/", 1)[-1]
    if name in INTERPRETERS or name == "eval":
        script = None
        if name == "eval":
            script = " ".join(words[1:])
        else:
            rest = words[1:]
            while rest and rest[0].startswith(("-", "+")):
                flag = rest.pop(0)
                if not flag.startswith("--") and "c" in flag:
                    script = rest[0] if rest else None
                    break
        if script is None:
            return []
        inner = Scanner(command.line).scan(script)
        found = []
        for sub in inner:
            sub.line = command.line
            sub.offset = command.offset
            # A `bash -c '…' | tar` pipes the interpreter, so a fetch inside it loses its own
            # status the same way a fetch written in the pipeline would.
            sub.piped = sub.piped or command.piped
            found += sites_of_command(path, sub, loops, step, retry, bounded, depth + 1,
                                      bound)
        return found
    if name in loops.function_loop:
        # A loop that runs what it is handed: the verb among the call's words is that loop's fetch.
        held = verb_in(words[1:])
        if not held:
            return []
        verb, rest = held
        _, _, inner_bound, inner_seconds = reduce_words(rest)
        return [Site(path, command.line, verb, step, rest, retry, bounded or inner_bound,
                     command.loops + [loops.function_loop[name]],
                     bound if bound is not None else inner_seconds, piped=command.piped)]
    silent = silent_verb_of(words)
    if silent:
        # Rule 7. A bare one is no fetch this check holds to anything; a retried one is the
        # shape the rule refuses, and it is recorded so the verdict can name it.
        if not retry:
            return []
        return [Site(path, command.line, silent, step, words, retry, bounded, command.loops,
                     bound, silent=True, piped=command.piped)]
    verb = verb_of(words)
    if not verb:
        return []
    if verb in ("curl", "wget") and loopback_only(words):
        return []
    if verb == "curl" and retry is None:
        # curl makes the attempts itself, and `--max-time` is what bounds each one of them.
        bound = curl_bound(words) or bound
    return [Site(path, command.line, verb, step, words, retry, bounded, command.loops, bound,
                 piped=command.piped)]


def loopback_only(words: list[str]) -> bool:
    """Whether every address this fetch names is the loopback: a readiness poll of a process the
    step itself started, which is no download."""
    urls = [word for word in words if "://" in word]
    if not urls:
        return False
    return all(any(host in url for host in LOOPBACK) for url in urls)


# ---------------------------------------------------------------- the exceptions


class Exemption:
    """One site R2 excepts, by the file, the job and the step it sits in and the verbs it covers.
    `why` is what a reader of a stale entry is told the entry was for."""

    def __init__(self, path: str, job: str | None, step: str | None,
                 verbs: tuple[str, ...] | None, why: str) -> None:
        self.path = path
        self.job = job
        self.step = step
        self.verbs = verbs
        self.why = why
        self.used = 0

    def covers(self, site: Site, job: str | None) -> bool:
        return (
            site.path == self.path
            and (self.job is None or self.job == job)
            and (self.step is None or self.step == site.step)
            and (self.verbs is None or site.verb in self.verbs)
        )


GHCR_WHY = ("a ghcr.io pull of a package this repository publishes: a failed pull is a fault of "
            "this repository's own package and must be seen, not retried over")
EXEMPTIONS = [
    Exemption(".github/actions/pull-test-images/action.yml", None, None,
              ("docker pull",), GHCR_WHY),
    Exemption(".github/workflows/ci.yaml", "envoy-client",
              "Pre-pull the NATS image the credential tests run", ("docker pull",), GHCR_WHY),
    Exemption(".github/workflows/ci.yaml", "download-retry-images",
              "Pull every base image the three builds read", ("docker pull",), GHCR_WHY),
    # A script site is read with no job and no step, so this entry names neither: the call at the
    # `for path in scripts:` loop below passes job=None and step=None.
    Exemption(".github/scripts/playwright-install.sh", None, None,
              ("apt-get update", "apt-get install"),
              "the apt phase of this script is dispatch://INFRA-1443's: its own attempts() loop "
              "switches origin after two attempts, which the shared helper cannot do"),
]


# ------------------------------------ what a reader declares where this check's reading stops


class DeclaredArithmetic:
    """One figure a reader writes down where rule 8's own reading of a loop stops, matched to a
    site by the file, the step and the verb it names. It answers for a site whose pass count
    this check cannot resolve — a `while read` over a list the run makes, a `while :` over the
    pages of a route — and it answers in seconds rather than in passes, because a loop this
    check could not read is a loop nobody has priced, and a pass number written over it is an
    excuse rather than a declaration. It writes the seconds that site contributes to the budget
    of the step holding it, and `why` writes that figure out as a sum, so a reader checks the
    arithmetic rather than taking the number on trust. This check uses it in place of the
    arithmetic it could not compute.

    An entry matching nothing fails this check, as a stale exception does, so a declaration
    cannot outlive what it was written for; an entry two loops of one file match fails as well,
    because it writes one figure and the second loop would take it without anybody writing it
    one; and a figure below one call of the site is refused with that floor, because a site
    costs at least one call of itself, whatever the run finds for it to do."""

    def __init__(self, path: str, step: str | None, verb: str, seconds: int, why: str) -> None:
        self.path = path
        self.step = step
        self.verb = verb
        self.seconds = seconds
        self.why = why
        self.used = 0
        self.loops: set[int] = set()
        # The sites whose one call costs more than this entry's whole figure, each with that
        # floor. Each of them is this entry refused by name.
        self.below: dict[int, int] = {}

    def covers(self, site: Site) -> bool:
        return (
            site.path == self.path
            and (self.step is None or self.step == site.step)
            and site.verb == self.verb
        )

    def names(self) -> str:
        """How a refusal names this entry."""
        return self.path + (f" step '{self.step}'" if self.step else "") + f" `{self.verb}`"

    def figure(self) -> str:
        return f"{self.seconds} s"


# Every loop in this tree that rule 8 cannot read is one whose passes are as many as the run
# finds work: a `while read` over a list the run wrote, a `while :` over the pages of a route.
# Each one's figure is the passes its own prose argues for at one call of that site, and the sum
# is written out beside it so that a reader checks it rather than takes it. What every entry of
# one pass says, each in its own words: the loop runs the call once for each thing the run found
# to do, the budget is written for the one the common run finds, and a run where every pass
# exhausts its five attempts is not the transient fault a retry exists to survive but an outage
# of the host, which ci.yaml's Budgets note leaves to the job's own ceiling rather than to the
# step's.
DECLARED_LOOPS: list[DeclaredArithmetic] = [
    DeclaredArithmetic(".github/workflows/ci.yaml",
                       "Force-cancel the runs of the groups the queue removed", "gh api", 300,
                       "one force-cancel per group the queue removed, and the budget is "
                       "written for the one a common run removes: 1 x (5 x 30 + 150) = 300 s. "
                       "That step's own comment derives its budget from that one ladder beside "
                       "about 3 s a call over the at most 21 calls the 20 groups the queue "
                       "builds at once can need"),
    DeclaredArithmetic(".github/workflows/dequeued.yaml",
                       "Cancel the removed group's ci.yaml run", "gh api", 300,
                       "one force-cancel per ci.yaml merge-group run still going at the "
                       "removed group's head, and the queue starts one run for a group: a "
                       "re-run is another attempt of that run and not a second one. "
                       "1 x (5 x 30 + 150) = 300 s"),
    DeclaredArithmetic(".github/workflows/main-bypass-check.yaml",
                       "Start ci.yaml when this push's head was never tested",
                       "gh workflow run", 300,
                       "one dispatch. .github/scripts/main-bypass-check.sh's header says it "
                       "prints one line on standard output, `dispatch <sha>`, naming the head "
                       "when it reached main untested, so the loop over its acts runs this "
                       "call once. 1 x (5 x 30 + 150) = 300 s"),
    # A script site is read with no job and no step, so these entries name no step: the call at
    # the `for path in scripts:` loop below passes step="-".
    DeclaredArithmetic(".github/scripts/check-pr-text.sh", None, "gh api", 900,
                       "the page read of one list route, over the three lists this script "
                       "reads. read_list's loop reads pages until a short one, which is no "
                       "count this check can make, and a pull request carrying more than 100 "
                       "reviews or more than 100 comments reads another page at the "
                       "per_page=100 the call asks for; the budgets reaching it are written "
                       "for the one page the common pull request answers. The "
                       "`for ((i = 0; i < ${#lists[@]}; i += 2))` under it runs read_list "
                       "once for each of three lists, so three lists at one page each is "
                       "3 x 1 x (5 x 30 + 150) = 900 s. The two reads this script writes "
                       "outside that loop are counted where they are written and are no part "
                       "of this figure"),
    DeclaredArithmetic(".github/scripts/ci-ok-reconcile.sh", None, "gh api", 900,
                       "one PATCH per other ci-ok on the head that needs one, and three is "
                       "what the ci-ok step's budget is written for: 3 x (5 x 30 + 150) = "
                       "900 s. A merge-group run posts one ci-ok per job whose own result "
                       "decides the group (ci-ok-report.sh), so a run that fails in three jobs "
                       "leaves three posts on the head; the two posted ci-oks that script's "
                       "header measures on open heads are one per head. This one is written "
                       "high on purpose: the script's refusals only warn, and a step the "
                       "runner cuts fails the ci-ok job, which turns a passing required check "
                       "red"),
    DeclaredArithmetic(".github/scripts/mirror-ci-images.sh", None, "crane copy", 1650,
                       "one copy per row of .github/ci-images.yaml the package does not hold, "
                       "and the ci-images step's budget is written for the rows a run copies, "
                       "at the under 75 s a row that file's own comment derives: "
                       "1 x (5 x 300 + 150) = 1650 s"),
    DeclaredArithmetic(".github/scripts/mirror-ci-images.sh", None, "crane digest", 450,
                       "the digest read that confirms the copy above it, once per row copied: "
                       "1 x (5 x 60 + 150) = 450 s"),
    DeclaredArithmetic(".github/scripts/mirror-ci-images.sh", None, "crane config", 900,
                       "the Go check of one row, once per tagged library/golang row, of which "
                       ".github/ci-images.yaml holds one. The read sits "
                       "in image_go(), which check_go() calls, and the loop runs check_go once "
                       "for the row the package already holds and once for the row it is about "
                       "to copy — two written calls of which a pass runs one, so the figure is "
                       "twice what a row costs: 2 x (5 x 60 + 150) = 900 s"),
]


def credentialed_jobs(path: str, document: Mapping) -> set[str]:
    """The jobs of a release workflow that hold a credential, by check-release-credentials.sh's
    own definition: a write scope of WRITE_SCOPES, or the deploy key."""
    if path not in RELEASE_WORKFLOWS:
        return set()
    found = set()
    for name, job in (document.get("jobs") or {}).items():
        if not isinstance(job, Mapping):
            continue
        permissions = job.get("permissions")
        scopes = permissions if isinstance(permissions, Mapping) else {}
        # repr, not a YAML dump: the loader's mappings are a subclass no dumper represents, and
        # what is wanted is only whether the job's text names the key anywhere in it.
        if any(scopes.get(scope) == "write" for scope in WRITE_SCOPES) or DEPLOY_KEY.search(
            repr(job)
        ):
            found.add(name)
    return found


# ---------------------------------------------------------------- the reading set

files = walk()
workflows = [
    path for path in files
    if Path(path).parent == WORKFLOWS and Path(path).suffix in {".yml", ".yaml"}
]
actions = [
    path for path in files
    if ACTIONS in Path(path).parents and Path(path).name in {"action.yml", "action.yaml"}
]
scripts = [
    path for path in files
    if Path(path).parent == SCRIPTS
    and path.endswith(".sh")
    and not path.endswith(TEST_HARNESS)
]
for dockerfile in DOCKERFILES:
    if dockerfile not in files:
        problem(SELF, 1,
                f"{dockerfile} is one of the three Dockerfiles a CI job builds and this check "
                "reads, and the tree does not hold it. Point DOCKERFILES at where it moved, or "
                "drop it if CI no longer builds it.")

sites: list[Site] = []
exempted: list[Site] = []
# The scripts a credentialed publish job's steps call, and then the closure of that set: the
# loops below fill the first and the walk between them turns it into the second.
credentialed_roots: set[str] = set()
credentialed_scripts: set[str] = set()


class Budget:
    """One step of a workflow, with everything rule 8 compares: where the step is written, what
    it is called, the minutes its own `timeout-minutes` gives (None where it writes none, which
    is itself refused for a step holding a retried call), the minutes its job gives and where
    that is written, the arithmetic of the calls the step's own `run:` holds, the local
    composite action a `uses:` step runs, whose calls are the step's too, and the scripts the
    step's `run:` invokes, each with how many times the step runs it, whose calls are the
    step's too for the same reason. A cost of None is a step holding a site this check cannot
    count, and `uncounted` is what made it so, which the refusal names in place of a
    comparison.

    A composite action's steps get no entry here: GitHub takes no `timeout-minutes` on one, so
    the budget that holds an action's calls is the `uses:` step that runs it."""

    def __init__(self, path: str, line: int, step: str, minutes: int | None, cost: int | None,
                 uses: str | None, scripts: dict[str, Call], uncounted: list[Uncounted],
                 job: str | None, job_minutes: int | None, job_line: int) -> None:
        self.path = path
        self.line = line
        self.step = step
        self.minutes = minutes
        self.cost = cost
        self.uses = uses
        self.scripts = scripts
        self.uncounted = uncounted
        self.job = job
        self.job_minutes = job_minutes
        self.job_line = job_line


budgets: list[Budget] = []
# The arithmetic of each composite action's own `run:` steps, and the local actions and the
# scripts each one calls in turn: a `uses:` step of one carries the whole of it.
action_cost: dict[str, int | None] = {}
action_uses: dict[str, list[str]] = {}
action_scripts: dict[str, dict[str, Call]] = {}
# What rule 8 can put no figure on in each composite action and in each script: a site, or a
# script the body runs, inside a loop whose passes it cannot read. A step reaching one of them
# is told about that loop instead of being judged against a sum that is missing it.
action_uncounted: dict[str, list[Uncounted]] = {}
action_by_directory = {str(Path(path).parent): path for path in actions}
action_files = set(actions)
# The arithmetic of each script's own calls, and the scripts it calls in turn: whoever runs one
# of them waits for the whole of it, once for each time it runs it.
script_cost: dict[str, int | None] = {}
script_calls: dict[str, dict[str, Call]] = {}
script_uncounted: dict[str, list[Uncounted]] = {}
script_files = set(scripts)


def local_action(uses: object) -> str | None:
    """The action.yml a `uses:` names, where it is one of this repository's own composite
    actions. A third-party reference and a called workflow are neither of them read: exception
    (b) above, and a called workflow's jobs carry their own budgets."""
    if not isinstance(uses, str) or not uses.startswith("./"):
        return None
    return action_by_directory.get(uses[2:].rstrip("/"))


def defaults_shell(holder: object) -> object:
    """The `defaults.run.shell` a workflow or one of its jobs sets, or None."""
    if not isinstance(holder, Mapping):
        return None
    defaults = holder.get("defaults")
    run = defaults.get("run") if isinstance(defaults, Mapping) else None
    return run.get("shell") if isinstance(run, Mapping) else None


def sets_pipefail(shell: object) -> bool:
    """Whether a step's shell sets `-o pipefail`, which is rule 9's question. `bash` is the one
    keyword GitHub expands to a pipefail shell, `bash --noprofile --norc -eo pipefail {0}`; a
    custom command line writes the option itself; and everything else — `sh`, `pwsh`, `python`,
    a shell named by a path, and a step that names none at all — leaves it off."""
    if not isinstance(shell, str):
        return False
    return shell.strip() == "bash" or "pipefail" in shell


def written_minutes(holder: object) -> int | None:
    """The `timeout-minutes` a step or a job writes, where it writes a whole number of them."""
    if not isinstance(holder, Mapping):
        return None
    minutes = holder.get("timeout-minutes")
    return minutes if isinstance(minutes, int) and not isinstance(minutes, bool) else None


def steps_of(path: str, document: Mapping) -> list[tuple[str | None, str, Mapping, object,
                                                         object]]:
    """The steps of a workflow's jobs and of a composite action's `runs`, each with the job it
    sits in (None in an action), the name a problem calls it by, the shell it runs under — its
    own `shell:`, else its job's `defaults.run.shell`, else the workflow's — and the mapping of
    the job holding it, whose own `timeout-minutes` rule 8 holds above the step's arithmetic
    too."""
    found = []
    holders: list[tuple[str | None, object, object, object]] = []
    fallback = defaults_shell(document)
    runs = document.get("runs")
    if isinstance(runs, Mapping):
        holders.append((None, runs.get("steps"), fallback, None))
    for name, job in (document.get("jobs") or {}).items():
        if isinstance(job, Mapping):
            holders.append((name, job.get("steps"), defaults_shell(job) or fallback, job))
    for job, steps, shell, holder in holders:
        if not isinstance(steps, list):
            continue
        for step in steps:
            if not isinstance(step, Mapping):
                continue
            name = step.get("name") or step.get("id") or "-"
            found.append((job, str(name), step, step.get("shell") or shell, holder))
    return found


for path in workflows + actions:
    try:
        document = yaml.load(Path(path).read_text(), Loader=LineLoader)
    except yaml.YAMLError as error:
        problem(path, 1, f"this file is not valid YAML, so its downloads cannot be read: {error}")
        continue
    if not isinstance(document, Mapping):
        continue
    credentialed = credentialed_jobs(path, document)
    for job, name, step, shell, holder in steps_of(path, document):
        cost: int | None = 0
        called: dict[str, Call] = {}
        blocked: list[Uncounted] = []
        if isinstance(step.get("run"), str):
            first_line = step.lines.get("run", 0)
            found, loops_read = sites_of(path, step["run"], first_line, name,
                                         sets_pipefail(shell))
            called = calls_of(step["run"], first_line, loops_read)
            blocked = uncounted_of(found) + blocked_calls(path, called)
            for site in found:
                cost = added(cost, arithmetic_of(site))
                if job in credentialed:
                    exempted.append(site)
                    continue
                covering = next((entry for entry in EXEMPTIONS if entry.covers(site, job)), None)
                if covering:
                    covering.used += 1
                    exempted.append(site)
                    continue
                sites.append(site)
        if job in credentialed:
            credentialed_roots.update(called)
        uses = local_action(step.get("uses"))
        if path in action_files:
            action_cost[path] = added(action_cost.get(path, 0), cost)
            merge_calls(action_scripts.setdefault(path, {}), called)
            action_uncounted.setdefault(path, []).extend(blocked)
            if uses:
                action_uses.setdefault(path, []).append(uses)
        # Every step of a workflow gets an entry, whether or not it writes a budget: a step
        # that holds a retried call and writes none is refused below, which is the shape that
        # let .github/actions/install-elan's 607 s ladder under ci.yaml's `airlock-lean` reach
        # no comparison at all. A composite action's step gets none, because GitHub takes no
        # `timeout-minutes` there and the `uses:` step that runs the action is its budget.
        if path not in action_files:
            minutes = written_minutes(step)
            job_minutes = written_minutes(holder)
            budgets.append(Budget(
                path,
                step.lines.get("timeout-minutes", min(step.lines.values(), default=0)),
                name, minutes, cost, uses, called, blocked, job, job_minutes,
                holder.lines.get("timeout-minutes", min(holder.lines.values(), default=0))
                if isinstance(holder, Mapping) else 0))

# Exception (d) for a script: a *.sh under .github/scripts/ that a credentialed publish job runs
# can carry no retry either, because the retry is `. .github/scripts/ci-retry.sh` and that source
# runs inside the credentialed job, where check-release-credentials.sh's rule 1(a) allows no
# dependency code — a command whose path holds a `scripts/` component other than release-push.sh
# is dependency code by its own dependency_command(). Read from the tree, like (b): the roots are
# the scripts such a job's steps call, and the closure below adds the scripts those call in turn,
# the same reading rule 8 makes of a step. It follows a call it can see rather than the branch a
# job takes, so it reaches a script a credentialed job could run; the sites it covers are named
# in exception (d) of this file's header.
while credentialed_roots:
    path = credentialed_roots.pop()
    if path in credentialed_scripts:
        continue
    credentialed_scripts.add(path)
    credentialed_roots.update(scripts_called(Path(path).read_text()))

for path in scripts:
    text = Path(path).read_text()
    found, loops_read = sites_of(path, text, 1, "-")
    script_calls[path] = calls_of(text, 1, loops_read)
    script_uncounted[path] = uncounted_of(found) + blocked_calls(path, script_calls[path])
    for site in found:
        script_cost[path] = added(script_cost.get(path, 0), arithmetic_of(site))
        if path in credentialed_scripts:
            exempted.append(site)
            continue
        covering = next((entry for entry in EXEMPTIONS if entry.covers(site, None)), None)
        if covering:
            covering.used += 1
            exempted.append(site)
            continue
        sites.append(site)
    # Rule 6: a `urllib.request` read inside this script's Python goes through the one reader.
    _, heredocs = without_heredocs(EXPRESSION.sub("EXPR", text))
    indents: list[tuple[int, str]] = []
    for line, body in heredocs:
        if match := PYTHON_DEF.match(body):
            indent = len(match.group(1))
            indents = [entry for entry in indents if entry[0] < indent]
            indents.append((indent, match.group(2)))
        elif body.strip() and not body.startswith((" ", "\t")):
            indents = []
        if not PYTHON_READ.search(body):
            continue
        inside = indents[-1][1] if indents else None
        if inside == PYTHON_READER and path == PYTHON_READER_FILE:
            continue
        problem(path, line,
                f"this read goes straight to urllib.request. Every Python read of a CI script "
                f"goes through {PYTHON_READER_FILE}'s {PYTHON_READER}(), which carries R1's floor "
                f"in that check's idiom: {ATTEMPT_FLOOR} attempts, waits of 10, 20, 40 and 80 "
                f"seconds, each bounded by the read's own timeout. One definition, no copies.")

# ---------------------------------------------------------------- the Dockerfiles


def instructions(text: str) -> list[tuple[int, str, str]]:
    """Each instruction of a Dockerfile as (line, keyword, the text after the keyword) with its
    backslash continuations and the comment lines between them kept, so a scan of that text reads
    the same lines the build runs."""
    found = []
    lines = text.split("\n")
    index = 0
    while index < len(lines):
        line = lines[index]
        start = index + 1
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            index += 1
            continue
        body = [line]
        while body[-1].rstrip().endswith("\\") and index + 1 < len(lines):
            index += 1
            body.append(lines[index])
        index += 1
        whole = "\n".join(body)
        keyword, _, rest = whole.partition(" ")
        found.append((start, keyword.upper(), rest))
    return found


def joined(text: str) -> str:
    """One line, continuations joined and each run of blank space one space: the shape the
    canonical retry is compared in."""
    return " ".join(text.replace("\\\n", " ").split())


for path in DOCKERFILES:
    if path not in files:
        continue
    for line, keyword, rest in instructions(Path(path).read_text()):
        if keyword != "RUN":
            continue
        while match := re.match(r"\s*--(mount|network|security)=\S+", rest):
            rest = rest[match.end():]
        found, _ = sites_of(path, rest, line, "-")
        if not found:
            continue
        sites += found
        flat = joined(rest)
        opens = flat.startswith(CANONICAL)
        if "retry()" in flat and not opens:
            problem(path, line,
                    "this RUN defines a retry loop of its own text. Every network RUN of the three "
                    "Dockerfiles opens with one canonical definition, byte for byte, so a reader "
                    "sees one loop and not a family of them. The text is in "
                    ".github/scripts/check-download-retry.sh's CANONICAL.")
        elif not opens and any(site.retry == "retry" for site in found):
            problem(path, line,
                    "this RUN calls `retry` and does not open with the canonical definition of it. "
                    "The text is in .github/scripts/check-download-retry.sh's CANONICAL.")

# ---------------------------------------------------------------- the verdict


def where_of(site: Site) -> str:
    """How a refusal names a site: its verb, and the step it sits in where it is a step's."""
    return f"`{site.verb}`" + (f" in step '{site.step}'" if site.step != "-" else "")


for site in sites:
    where = where_of(site)
    dockerfile = site.path in DOCKERFILES
    if site.silent:
        problem(site.path, site.line,
                f"{where} is retried, and that retry protects nothing: `mise x`, `mise exec`, "
                "`mise up` and `mise upgrade` exit 0 when they cannot resolve the tool. `mise x` "
                "warns (`Failed to resolve tool version list for …`) and then runs whatever "
                "binary of that name is already on PATH — a different build, at a version nobody "
                "asked for — and `mise up` says `All tools are up to date` having read nothing. "
                "So the retry waits for a failure that never comes, and the first attempt "
                "succeeds on a tool it never fetched. The cure is `mise install <tool>` under "
                "the retry, which exits 1 on the failed fetch, with the resolution after it: "
                "once the install has succeeded, a bare `mise x <tool> -- sh -c 'command -v "
                "<binary>'` reads the path off disk and fetches nothing.")
        continue
    if site.verb == "curl":
        retries, all_errors, bounded, fails = curl_options(site.words)
        if site.retry:
            if not site.bounded:
                problem(site.path, site.line,
                        f"{where} is retried and no attempt of it is bounded in time. A host that "
                        "accepts the connection and then sends nothing would hold one attempt for "
                        "the job's whole budget. Pass `--max-time <seconds>`.")
        elif retries is None or retries < RETRY_FLOOR or not all_errors or not bounded or not fails:
            missing = []
            if retries is None:
                missing.append(f"`--retry {RETRY_FLOOR}` (it passes none)")
            elif retries < RETRY_FLOOR:
                missing.append(f"`--retry {RETRY_FLOOR}` (it passes `--retry {retries}`, whose "
                               "last attempt starts well inside 120 s: curl's waits double from "
                               f"one second, so only {RETRY_FLOOR} reaches 127 s)")
            if not all_errors:
                missing.append("`--retry-all-errors` (without it a connection reset is not a retry)")
            if not bounded:
                missing.append("`--max-time <seconds>`, which curl resets for each attempt")
            if not fails:
                missing.append("`--fail`, usually the `f` of `-fsSL` (without it curl exits 0 on a "
                               "persistent 504 and writes the error body to the output)")
            problem(site.path, site.line,
                    f"{where} fetches without the floor: it lacks " + ", ".join(missing) +
                    ". Pass curl's own options, or wrap the call in `retry_fetch` from "
                    f"{HELPER}.")
        else:
            delay, max_time, readable = curl_waits(site.words)
            ladder = curl_ladder(retries, delay, max_time)
            if not readable:
                problem(site.path, site.line,
                        f"{where} writes `--retry-delay` or `--retry-max-time` with a value "
                        "this check cannot read, so it cannot say when the last of curl's "
                        "attempts starts. Both options rewrite the waits between them — a "
                        "fixed wait, and a limit that stops the ladder — and R1's floor is "
                        f"that the last attempt starts at least {LADDER_FLOOR} s after the "
                        "first. Write the seconds as a number, or drop the option and take "
                        "curl's own doubling waits.")
            elif ladder < LADDER_FLOOR:
                wrote = []
                if delay is not None:
                    wrote.append(f"`--retry-delay {delay}`, which replaces curl's doubling "
                                 "waits with a fixed one")
                if max_time is not None:
                    wrote.append(f"`--retry-max-time {max_time}`, which stops the ladder once "
                                 "the timer reset at the first attempt reaches it")
                problem(site.path, site.line,
                        f"{where} carries curl's own attempts and its last one starts "
                        f"{ladder} s after the first, inside R1's floor of {LADDER_FLOOR} s: "
                        "it writes " + " and ".join(wrote) + ". A retry whose ladder is over "
                        "in seconds does not outlast the fault it exists to survive — the "
                        "measured incidents were hosts refusing for about a minute. Drop the "
                        f"option and take curl's own waits, which reach 127 s at `--retry "
                        f"{RETRY_FLOOR}`, or write one that reaches {LADDER_FLOOR} s.")
        continue
    if site.verb == "apt-get update" and not apt_error_mode(site.words):
        problem(site.path, site.line,
                f"{where} passes no `-o APT::Update::Error-Mode=any`, so a failed index read "
                "leaves it reporting success. apt treats every index it could not fetch as a "
                "warning (`W: Failed to fetch … 504 Gateway Timeout`, then `W: Some index files "
                "failed to download. They have been ignored, or old ones used instead.`) and "
                "exits 0, so the retry around it stops after one attempt having read no list, "
                "and the `apt-get install` after it fails with `has no installation candidate` "
                "against a package list that will never name the package — which no further "
                "attempt can cure. The option makes each of those warnings an error, so the "
                "command exits non-zero and the retry has something to retry. It is apt's "
                "`--fail`.")
    if dockerfile:
        if site.retry != "retry":
            problem(site.path, site.line,
                    f"{where} fetches inside a Dockerfile RUN with no retry around it. Wrap it in "
                    "the canonical inline loop this file's network RUN lines open with: "
                    "`retry <bound in seconds> \"<what>\" <command...>`.")
        continue
    if site.retry != "retry_fetch":
        problem(site.path, site.line,
                f"{where} fetches with no retry. Source {HELPER} and call "
                "`retry_fetch \"<what>\" <bound in seconds> -- <command...>`: five attempts, the "
                "last starting 150 s after the first, each under `timeout <bound>`.")
        continue
    if not site.bounded:
        problem(site.path, site.line,
                f"{where} is retried and no attempt of it is bounded in time. Give `retry_fetch` "
                "a per-attempt bound in seconds.")

# Rule 9: a fetch left of a `|` in a step whose shell leaves pipefail off. The pipeline's status
# is its last command's there, so the fetch's own failure is the one thing the step never reads.
for site in sites:
    if not site.piped or site.silent:
        continue
    refusal = (
        f"{where_of(site)} is not the last command of its pipeline, and this step's shell "
        "leaves `pipefail` off: a workflow `run:` that names no shell runs under GitHub's "
        "`/usr/bin/bash -e {0}`, where a pipeline's status is its last command's alone. So "
        "`curl … | tar -xz` exits 0 when the fetch got a 504 and the unpack read nothing, and "
        "a `retry_fetch` around that pipeline reads that 0 and never retries: the site reads "
        "retried and is unprotected in fact. Read the fetch's own status: write the pipeline "
        "as two commands, with the fetch's output in a file or a variable, or give the step "
        "`shell: bash`, which GitHub expands to `bash --noprofile --norc -eo pipefail {0}`."
    )
    # One site, one annotation: a fetch this rule refuses is often refused by rule 1 as well,
    # and two `::error` lines at one line of one file read as two sites rather than as one
    # site's two faults.
    opening = f"::error file={site.path},line={site.line}::"
    for index, line in enumerate(problems):
        if line.startswith(opening):
            problems[index] = f"{line} {refusal}"
            break
    else:
        problem(site.path, site.line, refusal)

# Rule 2: a loop that holds a fetch is a copy of the one helper. Not in a Dockerfile, whose
# canonical definition is itself a loop and which carries no helper file to copy.
seen_loops: set[tuple[str, int]] = set()
for site in sites:
    if site.loop is None or site.retry == "retry_fetch" or site.path in DOCKERFILES:
        continue
    if (site.path, site.loop) in seen_loops:
        continue
    seen_loops.add((site.path, site.loop))
    problem(site.path, site.loop,
            f"this loop retries `{site.verb}` itself, which makes it a second copy of the retry "
            f"{HELPER} holds. One rule holds the attempts (dispatch://INFRA-1545, R2): delete the "
            "loop and call `retry_fetch \"<what>\" <bound in seconds> -- <command...>`.")

# Rule 8: a retried step's own budget strictly exceeds the attempts it allows, so the last
# attempt's own error reaches the log before the runner's kill does. A call is counted once for
# each time the step runs it — every pass of every loop around it, every call of the function
# holding it, and the loops around those calls — and a step reaching a loop whose passes this
# check can neither read nor find declared is told about that loop in place of a comparison: a
# sum short by one of its loops would send the step's author to the wrong line.


def script_arithmetic(path: str, seen: set[str]) -> int | None:
    """The arithmetic a call of this script carries: its own calls and those of every script it
    calls in turn, each once for every time it runs it. `seen` is what the step has counted
    already, so a script two of its calls reach is counted once, and a cycle — which the shell
    would run until something failed, but which must not hang this check — stops. None where
    one of those calls is a site this check cannot count."""
    if path in seen:
        return 0
    seen.add(path)
    total = script_cost.get(path, 0)
    for target, call in sorted(script_calls.get(path, {}).items()):
        total = added(total, carried(call, script_arithmetic(target, seen)))
    return total


def action_arithmetic(path: str, seen: set[str]) -> int | None:
    """The arithmetic a `uses:` of this composite action carries: its own calls, the scripts its
    steps run, and every local action it calls in turn, which carries its own the same way.
    `seen` stops a cycle, which GitHub would refuse to run but which must not hang this check."""
    if path in seen:
        return 0
    seen.add(path)
    total = action_cost.get(path, 0)
    for target, call in sorted(action_scripts.get(path, {}).items()):
        total = added(total, carried(call, script_arithmetic(target, seen)))
    for target in action_uses.get(path, ()):
        total = added(total, action_arithmetic(target, seen))
    return total


for budget in budgets:
    # One `seen` for the whole step: the script its `run:` calls, the action it uses and that
    # action's own scripts are one reading set, and a file two of those paths reach is one file
    # the step runs once.
    seen: set[str] = set()
    cost = budget.cost
    for target, call in sorted(budget.scripts.items()):
        cost = added(cost, carried(call, script_arithmetic(target, seen)))
    if budget.uses:
        cost = added(cost, action_arithmetic(budget.uses, seen))
    blocking = list(budget.uncounted)
    for target in sorted(seen):
        blocking += script_uncounted.get(target, []) + action_uncounted.get(target, [])
    carries = (f"carries `timeout-minutes: {budget.minutes}`" if budget.minutes is not None
               else "carries no `timeout-minutes`")
    if blocking:
        named = "; ".join(
            f"`{item.what}` at {item.path}:{item.line}, inside the loop at line {item.loop}"
            for item in blocking
        )
        # The figure and the advice are written for the first of them, which is the step's own
        # uncounted site wherever it has one: uncounted_of's verb sites come before
        # blocked_calls's scripts. A retried call carries what one call of it costs; a script
        # carries None, and what one run of it costs is read here rather than when the call was
        # found, because the scripts are read after the steps that run them. The two take
        # different advice, because a declaration names a site by the verb it retries and a
        # script call writes a path where that verb would be.
        script_call = blocking[0].one is None
        one = (script_arithmetic(blocking[0].what, set()) or 0 if script_call
               else blocking[0].one)
        waiting = ("One run of that script takes" if script_call
                   else "One call there takes")
        instead = (
            "take the call out of the loop, or write the loop inside that script, where the "
            "call it retries is a site a declaration can name. No entry of DECLARED_LOOPS "
            "answers this one: an entry matches a site by its file, its step and the verb it "
            "retries, and a call of a script writes a path where that verb would be, so an "
            "entry pointed at it matches no retried call and fails this check in its own "
            "right."
            if script_call else
            "where the passes are as many as the run finds work, declare that site in "
            "DECLARED_LOOPS in .github/scripts/check-download-retry.sh: a DeclaredArithmetic "
            "entry there writes the seconds the site contributes to this budget and the sum "
            "they come from.")
        problem(budget.path, budget.line,
                f"step '{budget.step}' {carries} and this "
                "check can put no figure on it: it runs a retried call inside a loop whose "
                f"passes it cannot read — {named}. {waiting} "
                f"{one} s to exhaust its attempts and the step waits for "
                "that once per pass, so a budget written for one pass is a step the runner "
                "cuts in the middle of the retry it exists to allow. Write the count where "
                "this check reads it — a `for x in 1 2 3 4 5 6 7`, a `for x in "
                f"$(seq 1 7)`, or a `for ((i = 0; i < 7; i++))` — or {instead}")
        continue
    if cost is None or not cost:
        continue
    if budget.minutes is None:
        problem(budget.path, budget.line,
                f"step '{budget.step}' holds {cost} s of retries and carries no "
                "`timeout-minutes` of its own, so nothing holds that arithmetic above "
                "anything: this rule compares a step's attempts with the budget that step "
                "writes, and a step writing none is judged by nothing while the job's ceiling "
                "cuts it with GitHub's bare `The action '…' has timed out after N minutes.` "
                "That is how .github/actions/install-elan's 8 x 60 + 127 = 607 s ladder sat "
                "under ci.yaml's `airlock-lean` with no term for it and nothing said so. "
                "GitHub's granularity is the minute, so write "
                f"`timeout-minutes: {cost // 60 + 1}` or more on this step.")
        continue
    if 60 * budget.minutes <= cost:
        followed = sorted(target for target in seen if target in script_files)
        through = (f" That arithmetic is the step's own and that of {', '.join(followed)}, "
                   "which it runs: a step waits for every attempt the scripts it calls make."
                   if followed else "")
        problem(budget.path, budget.line,
                f"step '{budget.step}' carries `timeout-minutes: {budget.minutes}`, "
                f"{60 * budget.minutes} s, and its own retries take {cost} s to exhaust, which "
                f"that budget does not strictly exceed.{through} A budget its own retries reach "
                "is a race between the last attempt's error and the runner's kill, and the "
                "retry can lose it: "
                "in run 42386541065's job 128445139635 a `gh api` read bounded at 30 s under "
                "`timeout-minutes: 5` — 5 x 30 + 150 = 300 s against 300 s — printed "
                "`attempt 5 of 5 reached its 30s bound` at 17:22:16.7444966Z and was killed at "
                "17:22:16.7483230Z, four milliseconds later. Lose that race and the step "
                "reports GitHub's bare `The action '…' has timed out after N minutes.` rather "
                "than naming the site and its attempts, which is the loud failure the retry "
                "exists to give. GitHub's granularity is the minute, so write "
                f"`timeout-minutes: {cost // 60 + 1}` or more.")
        continue
    allowed = JOB_DEFAULT_MINUTES if budget.job_minutes is None else budget.job_minutes
    if 60 * allowed <= cost:
        job_carries = (
            f"job '{budget.job}' carries `timeout-minutes: {budget.job_minutes}`, "
            f"{60 * budget.job_minutes} s" if budget.job_minutes is not None
            else f"job '{budget.job}' writes no `timeout-minutes`, so GitHub gives it "
                 f"{JOB_DEFAULT_MINUTES} minutes, {60 * JOB_DEFAULT_MINUTES} s")
        problem(budget.path, budget.job_line,
                f"{job_carries}, and its step '{budget.step}' holds retries taking {cost} s to "
                "exhaust, which that budget does not strictly exceed. A job whose ceiling sits "
                "under one of its own steps' arithmetic cuts that step before the step's own "
                "budget can name the site that failed, so the step's `timeout-minutes` buys "
                "nothing: a job carries its measured duration plus the arithmetic of its "
                "slowest retried step, which is the rule ci.yaml's Budgets note writes out. "
                f"Write `timeout-minutes: {cost // 60 + 1}` or more on this job.")

for entry in EXEMPTIONS:
    if entry.used:
        continue
    problem(SELF, 1,
            f"the exception for {entry.path}"
            + (f" job '{entry.job}'" if entry.job else "")
            + (f" step '{entry.step}'" if entry.step else "")
            + " matches no fetch in the tree. An exception outliving its site is an exception "
            "nobody reads again: drop it, or point it at where that site moved. It was written "
            f"for {entry.why}.")

for entry in DECLARED_LOOPS:
    if len(entry.loops) > 1:
        problem(SELF, 1,
                f"the declared loop for {entry.names()} covers the loops at lines "
                + ", ".join(str(line) for line in sorted(entry.loops))
                + ", and it writes one figure. A loop's passes are that loop's own, so an "
                "entry that two of them match hands the second a figure nobody wrote for it: "
                "one of those loops is counting something else. Write the count where this "
                "check reads it off the loop, or give the two loops steps or verbs that tell "
                f"them apart. This entry said {entry.figure()}, because {entry.why}.")
        continue
    if entry.below:
        line, floor = sorted(entry.below.items())[0]
        problem(SELF, 1,
                f"the declared arithmetic for {entry.names()} writes {entry.figure()}, and "
                f"one call of that site at {entry.path}:{line} takes {floor} s to exhaust its "
                "attempts. A site costs at least one call of itself, whatever the run finds "
                "for it to do, so a figure under that floor is a budget written for a step "
                f"the runner cuts inside the first ladder. Write {floor} s or more, or price "
                f"the attempts the site actually makes. It said {entry.figure()}, because "
                f"{entry.why}.")
        continue
    if entry.used:
        continue
    problem(SELF, 1,
            f"the declared loop for {entry.names()} matches no retried call in a loop whose "
            "passes this check cannot read. A declaration outliving its loop is a figure "
            "nobody checks again: drop it, or point it at where that loop moved. It said the "
            f"budgets reaching it are written for {entry.figure()}, because {entry.why}.")

if LIST_MODE and not problems:
    # A reader of this list pipes it (`| head`, `| cut`), so a closed pipe ends this the way any
    # other tool ends: on the signal, with no traceback.
    signal.signal(signal.SIGPIPE, signal.SIG_DFL)
    for site in sorted(sites, key=lambda site: (site.path, site.line)):
        print(f"{site.path}\t{site.line}\t{site.verb}\t{site.step}")
    print(f"{len(sites)} site(s)", file=sys.stderr)
    raise SystemExit(0)

if problems:
    for line in problems:
        print(line)
    print(f"{len(problems)} download(s) outside the one retry rule", file=sys.stderr)
    raise SystemExit(1)

print(
    f"every network fetch in CI retries at the floor: {len(sites)} site(s) over "
    f"{len(workflows)} workflow(s), {len(actions)} composite action(s), {len(scripts)} script(s) "
    f"and {len(DOCKERFILES)} Dockerfile(s), with {len(exempted)} site(s) the exceptions name"
)
PY
