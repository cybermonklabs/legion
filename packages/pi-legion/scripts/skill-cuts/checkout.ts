/**
 * Read-only calls, and their answers from a copy of a written case's checkout.
 *
 * **The read-only rule** (`readOnlyCall`, the one definition the replay's checkout rounds and the
 * behaviour report's inspection floors read): a call of one of READ_ONLY_TOOLS (Oh My Pi's `read`,
 * `grep`, `glob`, `find`, `ls`), or a `bash` call (`parseScript`) whose command is lists joined by
 * `;` or line ends of pipelines joined by `&&` or `||` of commands joined by pipes, each command a
 * simple one, a subshell `( … )` or a group `{ …; }`, every simple command among them, and every
 * one inside a command substitution (`$(…)`, or a backtick with no `\` in it), one of
 * READ_ONLY_PROGRAMS (`commandReadsOnly`): `cd`, `cat`, `head`, `tail`,
 * `ls`, `find` with none of FIND_ACTIONS (`-exec`, `-execdir`, `-ok`, `-okdir`, `-delete`,
 * `-fprint`, `-fprint0`, `-fprintf`, `-fls`, `-files0-from`), `grep`, `rg`, `wc`, `sed` with `-n`
 * and a script that only reads (`sedReadsOnly`), `nl`, `stat`, `file`, `tree`, `pwd`, `true`,
 * `echo`, `printf`
 * (its `-v NAME` or `-vNAME` naming a variable an assignment may set), `env` with no command
 * (ENV_PRINTS: options that print the environment, `-u NAME`, then assignments), `unset` (of no
 * variable of UNSET_PROTECTED: PATH, HOME, PWD, GIT_OPTIONAL_LOCKS, and the answers' git and jj
 * settings GIT_CONFIG_COUNT, GIT_CONFIG_KEY_n, GIT_CONFIG_VALUE_n, GIT_CONFIG_SYSTEM,
 * GIT_CONFIG_PARAMETERS and JJ_CONFIG), `export`, an assignment `NAME=value` alone or before a
 * command, `git` (with at most
 * the global options `-C <dir>`, `--no-pager` and `-P`) whose subcommand is one of READ_ONLY_GIT
 * (`log`, `show`, `diff`, `status`, `grep`, `ls-files`, `blame`, `rev-parse`), `worktree list`,
 * `remote` (alone, `-v`, `show`, `get-url`) or `branch` with no name (alone, `-a`, `-r`, `-v`,
 * `--list`, `--show-current`), or `jj` (`jjReadsOnly`) `status`, `st`, `log`, `show`, `diff`,
 * `root`, `file show`, `bookmark list` or `workspace list`. Every program's options are read by one
 * rule (`parseOptions`, as GNU's getopt_long reads them): a short option attached to its value
 * (`-vNAME`) or in a cluster (`-nO<cmd>`, `-ni`, `-bC`), and a long option by any start of its
 * name that git or getopt takes (`--in` for `--in-place`, `--open` for `--open-files-in-pager`).
 * So the call is not read-only that gives, by any of those spellings, an option that runs a
 * program, writes a file or reads one no word names: sed's `-i`/`--in-place` and `-f`/`--file`
 * (and a script with an `e`, `r`, `R`, `w` or `W` command, or an `s` with an `e` or `w` flag,
 * `sedScriptReadsOnly`); git's of GIT_RUNS (`--output`, `-O`, `--open-files-in-pager`,
 * `--ext-diff`, `--no-index`, `--show-signature`) or a format's `%G` (which runs gpg); and the
 * options of PROGRAM_OPTIONS: rg's `--pre`, `--pre-glob`, `-z`/`--search-zip` and
 * `--hostname-bin`, tree's `-o`, `-R`, `--fromfile` and `--fromtabfile`, wc's `--files0-from`
 * (each word of wc's with one known value, or a glob word, whose every text starts with no `-`,
 * read as a file operand), file's `-C`/`--compile`, `-m`/`--magic-file`, `-f`/`--files-from`,
 * `-p`/`--preserve-date`, `-z`, `-Z` and `--uncompress`, jj's `--tool`, `--config`,
 * `--config-toml` and `--config-file`; an option that reads one file its value names (grep's `-f`
 * and `--exclude-from`, rg's `-f` and `--ignore-file`, git's `grep -f`, `ls-files -X` and `blame
 * --contents`) is read so by the confinement, its value a path. The options are those of the
 * tools the sandbox runs, each checked against its own `--help` (GNU coreutils 9.7, GNU grep 3.11,
 * GNU findutils 4.10, GNU sed 4.9, git 2.47; no option of `cat`, `head`, `tail`, `ls`, `nl` or
 * `stat` reads a path no word names, and `echo`, `printf`, `true` and `pwd` are bash's builtins).
 * An assignment, `export` or `printf -v` sets no variable of PROTECTED_NAME (one that picks a
 * program, a library, a configuration or a directory: PATH, HOME, IFS, CDPATH, PAGER, every
 * GIT_*, JJ_*, LD_*, XDG_*, BASH*, …); each
 * affects only the call's own shell. A variable's expansion (`$NAME`, `${NAME}`) is one of a
 * variable the command assigns before it, or of HOME, PATH or PWD. A word of `find`, `rg`, `tree`,
 * `file`, `sed`, `git`, `jj` or `printf` (whose options may write, run a program or set a
 * variable) takes one known value (`walkScript`: a literal, or an expansion of a value the
 * command assigned, quoted or holding no blank or glob character); a word whose unquoted text
 * (`bareText`) holds a glob character (`*`, `?`, `[`) has none, since bash puts in its place the
 * names of the files it matches, one of which may be an option (`--open-files-in-pager=sh`,
 * `-vPATH`), and one in a word of `printf`, `env` or `export` or in an assignment's value makes
 * the call not read-only. So does a brace expansion (`{a,b}`, `{1..3}`) in the unquoted text of
 * any word of any command, or of a redirection's target, which bash makes into several words (a
 * quoted or escaped `}` or `,` neither ends nor splits one, as bash reads it: `{x\},-e1e\ id}` is
 * two words; `holdsBrace` finds one in one pass, in time linear in the word, since the rule runs
 * in the runner's own thread). A quoted word is a literal: a single-quoted one, and a
 * double-quoted one whose every `$` is escaped and that holds no backtick, expands to itself,
 * whatever `$`, `?`, `*`, `{…}` or `~` it holds. A redirection to a file anywhere makes the call
 * not read-only (a
 * redirection to `/dev/null`, a copy of one stream into another such as `2>&1`, and a read of a
 * file with `<` are none). So do a process substitution, an arithmetic expansion (`$((…))`, and
 * bash's old `$[…]`, which runs the substitutions a variable it reads holds) or command, any other
 * expansion (`$?`, `$1`, `${X:-y}`, `$'…'`), a command run in the background (`&`), a heredoc or
 * herestring, a comment, a program named by its path or an expansion, and any other command.
 *
 * **The answers** (`answerCall`): a written case's run (replay.ts's checkout rounds) answers a
 * read-only call from a copy of its context session's checkout (`CheckoutCopy`: the copy's
 * directory, and the session's checkout root and working directory it stands for), as the pinned
 * Oh My Pi (the tag v18.18.6-sami.20261119-162325 of cybermonklabs/oh-my-pi) gives it at its
 * default settings, every path of the session's (the copy's own never shows: a read's header,
 * grep's and glob's paths, and every path bash, git or jj prints, `pwd`, `git rev-parse
 * --show-toplevel`, `git worktree list`, `jj root` and error messages among them, names the
 * session's checkout root and working directory in place of the copy's), or says why it does not
 * (`Unanswered`). A call the copy answers in no form gives the run no verdict (replay.ts's
 * UnansweredRead): the run is made again, never judged on that call.
 *
 * **Confinement.** The calls are the model's, run on this machine, so none may read a file outside
 * the copy. cli.ts removes from each shared checkout, as it makes it, every symbolic link that
 * resolves outside it or not at all (`removeOutsideLinks`), so `cat` of a link, `grep -R`, `rg
 * --follow`, `find -L` and a `read` follow no link out; the runner therefore refuses no option
 * that follows links. Beside that, a `read`, `grep` or `glob` whose target resolves outside the
 * copy is not answered (`leavesCopy`), and a `bash` command is not answered when a word of it may
 * lead outside (`escapes`: a word whose expansion the runner cannot place inside the checkout,
 * `.git`, an absolute path off the root (one under `/proc` among them), a relative one that
 * leaves the root from any directory the shell may be in there, an option's value that does
 * (after a long option's `=`, or any rest of a short one's word after one of its letters, so
 * `grep -f..` too: `optionPaths`), a path that the kernel's walk of it in the copy takes out of
 * it, through a link that stays inside followed by `..` (`walksInCopy`), a glob word that may make
 * a word starting with `-` (read with no matcher, `globOptionFault`: one whose first part holds a
 * glob character, which bash expands in the directory the shell is in, `nullglob` off, where a
 * directory the shell may be in holds any name starting with `-`, as `wc *` beside a file named
 * `--files0-from=x` or `wc [[:punct:]]*` does, or is no directory the copy holds), a path of a
 * glob word whose glob part starts with `.` (bash matches a name's leading `.` only so, `dotglob`
 * off, so it may match `.`, `..` or `.git`) or comes before a `..` (the kernel walks the `..` from
 * what the glob matches, a link among them), a `cd` to a glob word, a `cd` that names no
 * directory, and, where the shell may be in a directory whose real path in the copy is not its
 * path as written, a word holding `..`), would show a value of
 * the runner's (`$PATH`; and, where the copy names no home of the session's, `CheckoutCopy.home`,
 * `$HOME` and a word of any command, `echo`'s among them, with an unquoted `~` at its start or
 * after its `=` or a `:`, where bash puts a home directory of the runner's: where it names one,
 * `$HOME`, `${HOME}` and each `~` bash expands, `homeTildes`, stand for that home, read as any
 * other path, inside the root or refused as outside it, and a `~user`, `~+` or `~-` is refused),
 * or runs jj where its repository may be one the checkout tracks (a `.jj` between the
 * directory jj starts from, its `-R`'s or the working directory, and the root, or at the root where
 * no colocation of the runner's made it: in the sandbox jj stops there at its configuration, which
 * it cannot move to the read-only home, and such a store may name a git repository outside the
 * copy). `$(git rev-parse --show-toplevel)` is the root only where no directory between the
 * working directory and the root holds a `HEAD` (git dies at a directory laid out as a bare
 * repository, and prints nothing). The copy is fixed and read-only, so a `cd` of one word, a
 * literal and no option, to a directory it holds inside the root, each part before a `..` a
 * directory too, as bash's own cd walks it (`cdLanding`), succeeds: where it surely runs (no `&&`
 * or `||` before it in its list), every later command of the call, until another `cd`, runs from
 * there alone (`Walk.cd`); any other `cd` (with an option or a second operand, to a path the copy
 * lacks, a file, a path outside the root, a word that is not a literal, or one that may not run)
 * may fail, so the shell may be where it was, where it leads, or where the kernel's walk of the
 * path as written ends (`cd -P`, and bash's own cd where its walk finds no directory). The kernel
 * walks a `..` from the directory the shell is in as it is real, the walk from its path as
 * written: so where the working directory's real path in the copy is not its path (the sandbox
 * enters it as it is real), or a `cd` may have gone through a symbolic link, a later word holding
 * `..` is not answered (`ShellState.linked`); nor, there, is a command that prints the working
 * directory's own path (`pwd`, `$PWD`, the environment `env` and `export` print, git's `rev-parse
 * --show-prefix`, `--show-cdup` and `--show-toplevel`, `jj root`), which bash, git and jj give
 * from the real directory, where the session showed the path as written.
 *
 * **The sandbox.** The rule decides which calls are answered; it does not make running them safe.
 * So each answered `bash` command, and each jj command of a copy's colocation, runs in a sandbox
 * of the copy (`runSandboxed`): new user, mount, network, process and host-name namespaces
 * (`unshare`); only the copy of the machine's disk, read-only, at CHECKOUT_MOUNT (`/mnt/checkout`),
 * the command's working directory the session's mapped into it, and the session's checkout root
 * mapped to that path in the command and back in its output (`sandboxedBash`); an empty, read-only
 * file system over each tree that holds the pod's credentials or tools (`hiddenPaths`: `/run`,
 * `/var/run`, `/home`, `/root`, `/legion`, `/tmp`, `/var/tmp`, `/dev/shm`, the operator's
 * configuration, `/opt`, the runner's home, and each directory a variable of the runner's
 * environment ending in `_FILE`, `_DIR` or `_HOME` names, the model gateway's key, the Dispatch
 * token, Envoy's and gh's configuration, the secrets broker's keys and the store's address among
 * them); no network; no capability (so nothing it runs can unmount what hides a directory); an
 * environment of only what it needs (SANDBOX_ENV: PATH, a fixed LANG, an empty HOME, no system or
 * global git configuration, GIT_CONFIG through GIT_CONFIG_COUNT: no fsmonitor, no hooks, a bare
 * repository used only where named, no transport, no pager, the index's stat data trusted though
 * the namespace shows each file's owner as root; GIT_OPTIONAL_LOCKS=0; jj's
 * SANDBOX_JJ_CONFIG, which names no external tool and no signing program; where the copy names
 * the session's home, the command's HOME is that home as the sandbox shows it, `sandboxHome`: its
 * place in the copy where it is the root or inside it, else an empty, read-only directory of the
 * sandbox's own with the copy bound again at the root's place under it, mapped back to the home
 * in the output; never one that shows a file of the copy where git reads the user's
 * configuration, HOME_GIT_CONFIG); a time limit (the
 * command's, from when its sandbox is made: the queue, below), past which every process it
 * started is ended, and an output cap. The sandbox's fresh `/proc` still
 * lists, in `/proc/self/mountinfo`, the mount points of the pod it was made from (the hidden ones
 * among them, each under its empty file system); no answered call reads it, as no answered path
 * is outside the root. An answer whose sandbox cannot be made is not given, nor one of a command
 * one of whose programs (each but bash's builtins) is on no directory of the sandbox's PATH,
 * whose answer would be bash's `command not found`, which the session did not see. The programs
 * the sandbox runs are the machine's own (on this pod GNU coreutils 9.7, GNU grep 3.11, GNU
 * findutils 4.10 and GNU sed 4.9), where Oh My Pi's bash tool may run built-in utilities of its
 * own (the Oh My Pi shell on this pod has `cat`, `head`, `ls`, `wc`, `grep`, `sed` and `find` as
 * shell builtins, uutils-like: `cat 0.8.0`, `grep (pi-uu-grep) 0.2.0`, `sed 0.1.1`), so an answer
 * of `ls` and the like (its layout, an error's wording) may differ from what the session saw.
 *
 * **The queue.** A sandbox keeps a CPU busy for as long as its command runs, and one runner
 * process makes many calls at once (the gateway's `--concurrency` bounds its model calls, not the
 * sandboxes a reply starts). Started together they slow each other: on a two-CPU pod, eight `git
 * status` commands run at once all reached the 30 s limit, and two at once took 10.9 s each. So a
 * runner process runs one sandbox at a time, each in the order it asked (`checkoutQueue`,
 * common.ts's `Limiter` of 1; the jj commands of a copy's colocation, `colocateJj`, queue too).
 * The pod runs at most two runner processes together, so at most two sandboxes run at once on its
 * two CPUs. The invariant: a sandbox has spawned no child and set no timer until it holds the
 * queue's place, and it keeps the place until its child has exited; so no limit counts its wait.
 * A command's limit (BASH_TIME_LIMIT_MS) counts from the sandbox's ready marker (SANDBOX_READY),
 * not from the spawn: the setup has the same limit from the spawn, so a sandbox that is never made
 * is ended too, but its setup takes none of the command's time. A sandbox past its limit is ended
 * and not answered, as before (`Unanswered`: the run is made again). The queue holds sandboxes
 * only: a `grep` or `glob` worker (`answerInWorker`) and a `read` are not queued. The measured
 * fault is `git status` commands under load; a `grep` or `glob` that runs past 30 s alone is a
 * separate timeout, which the queue does not change.
 *
 * - `read` (keys `path`, `i`): a whole read by path that resolves (cases.ts's `resolvedReadPath`,
 *   against the session's working directory, and a `~` path against the session's home where the
 *   copy names one, `CheckoutCopy.home`) to a file under the checkout's root, in the form
 *   arms.ts writes a whole read by path (`pathAnswer`, its rows as the arm's reads by path show
 *   them; its hashline header's path relative to the working directory, else `~/<rest>` under the
 *   session's home, as the pin's `shortenPath` shows it, read-format.ts lines 70-83, else
 *   absolute), or Oh My Pi's not-found result `Path '<path>' not found` for a file the copy lacks. A
 *   file that is no prose is answered so too: a UTF-8 text file that is no notebook, SVG or
 *   document markit converts. A whole read of such a file of 100 lines or more (newlines and one
 *   more, `read.summarize.minTotalLines`) is the pin's structural summary where the pin's
 *   tree-sitter parse, through its native addon, is clean and elides a span (arms.ts's
 *   `summaryAnswer`; read.ts lines 2519-2575, read-summary.ts's `trySummarize`): its rows, each
 *   elided span a `…` row or one `<a>-<b>:<head> … <tail>` row of a brace pair, and the footer
 *   `[…<n>ln elided; re-read needed ranges with <path>:<a>-<b>[,<c>-<d>]]` (`, e.g.` in place of
 *   ` with`, and the first two ranges, where more are elided), `<path>` relative to the session's
 *   working directory, else absolute; a file the pin does not summarize is answered as below.
 *   Whole reads page after 300 lines (`read.defaultLimit`) or at the byte budget, whichever
 *   comes first. A single line range (`:N-M`, `:N..M`, `:N+K`, `:N`, `:N-`) includes a preceding
 *   line and, for a range with an end, up to three following lines, subject to its byte budget
 *   and line cap (`pathRangeAnswer`). If lines remain, the notice is `[Showing lines <a>-<b>
 *   of <total>. Use :<b+1> to continue]`, with `(<size> limit)` after the total on a byte stop.
 *   Any column truncation goes in the same notice. Where the rows shown leave lines of the file
 *   out, the answer shows the pin's block context too (arms.ts's `windowRows`, block-context.ts):
 *   the boundary line of each block that opens on a shown row and closes after them, or closes on
 *   one and opens before them, as tree-sitter finds it through the pinned Oh My Pi's native addon
 *   (else, for a language the addon does not know or a file that does not parse, a scan of the
 *   brackets), in the rows' form, cut as a shown row is, with `…` rows across the gaps; the byte
 *   budget counts the shown rows alone. A range past the file's end gets the pin's notice, `Line
 *   <n> is beyond end of file (<lines> lines total). Use :1 to read from the start, or :<lines> to
 *   read the last line.` A read of a directory is answered with the pin's listing (read.ts lines
 *   3281-3358, workspace-tree.ts lines 57-90 and 166-314, crates/pi-natives/src/workspace.rs
 *   lines 160-190; `directoryListing`): `.`, then each entry to two levels below it (hidden ones
 *   too, `.DS_Store` left out, no `.gitignore` read), `- <name>` (a directory's `<name>/`)
 *   indented two blanks a level, a directory's own entries after it, each directory's newest
 *   first by the time of change the copy's own files hold (an lstat's, in whole milliseconds), a
 *   tie by name (`localeCompare`); a directory under it of more than 12 entries shows its 11
 *   newest, `- … <n> more` and its oldest; after each name, where its age is not 0 s, a file's
 *   size (`0B` for a symbolic link) and its age (`just now`, `<n>m ago`, `<n>h ago`, `<n>d ago`,
 *   `<n>w ago`, `<n>mo ago`); `(empty directory)` for none. The pin measures an age from the time
 *   of its call; the runner measures it from the time the copy was made (`CheckoutCopy.madeAt`,
 *   recorded once in the copy's marker by cli.ts), never from the time of its own call, so a
 *   listing reads the same in every arm and every run of a case. A symbolic link is listed as a
 *   file is; one cli.ts removed from the copy is not listed. Not
 *   answered: a read with another selector (`:raw`, a list, `:-N`, …), a ranged read of a
 *   directory, a listing of a copy that records no time it was made, one that would show `.git`
 *   or `.jj` (the copy's, not the session's), or past 50 KB, a read outside the root, under
 *   `.git` or, where the copy names no home of the session's, under `~`, a file past 4 MiB (the
 *   pin streams it, with a notice of its own and no block context), a read whose block context
 *   or summary needs
 *   a WebAssembly grammar not installed beside the addon (the pin downloads it first, and the
 *   session's rows came from it or, where the download failed, from the scan of the brackets, or
 *   the file was unsummarized), and a file the arms refuse for another reason (`pathFormFault`:
 *   not UTF-8 text, a git conflict block, an artifact spill, a summary's too).
 * - `grep` (keys `pattern`, `path`, `case`, `gitignore`, `skip`, `i`): GrepTool's output
 *   (packages/coding-agent/src/tools/grep.ts lines 651-1316, its native search
 *   crates/pi-walker/src/grep/). Its `path` (`.` when absent) is a path, or a list joined by `;`
 *   outside braces (unless the whole names a path the copy holds; `pathListEntries`, which
 *   `glob` reads too: path-utils.ts lines 599-772), whose parts that start with `!` and name no
 *   path of the copy are exclusions (grep.ts lines 465-529, crates/pi-walker/src/glob.rs lines
 *   95-125: one with no `/`, or that starts with `**`, matches at any depth; any other is
 *   anchored where it resolves, and kept only inside a searched directory; a file or directory
 *   one matches below a searched directory is left out) and the rest the paths searched (`.`
 *   where none is). Of several paths, those the copy holds are searched, each on its own in their
 *   order (a line two of them show kept once), or, where they are directories one of which holds
 *   the rest, as that one (path-utils.ts lines 1050-1133 and 1460-1591, grep.ts lines 826-918);
 *   the others are noted after the matches, `Skipped missing paths: <paths>`. A directory's files
 *   are its regular files in path order, `.git`, `.jj` and `node_modules` not entered and no link
 *   followed, a file past 4 MiB searched over its first 4 MiB after every other
 *   (crates/pi-walker/src/grep/mod.rs lines 1-19, run.rs lines 573-690); a `.gitignore` is not
 *   read (the copy holds the commit's tracked files). A file's matching lines (scan.rs lines
 *   221-385: none in a file with a NUL byte in its first 64 KiB; its search ends before a line to
 *   report that holds one), with one line of context before each and three after (settings.ts
 *   lines 488-520) that no other line reported holds, `...` between runs that do not touch, each
 *   line its trailing whitespace taken off and, past 512 bytes, cut (render.rs lines 1-139): a
 *   context line to its first 509 bytes and `...`; a match line the same where its first match
 *   ends in those, else to a window of 506 bytes centred on that match with `...` on each cut side
 *   (or `...` and the line's last 509 bytes), then ` [col <n>]`, the match's column. Each match
 *   line `*<n>:<line>` and each context line ` <n>:<line>` (`|` in place of `:` in a session that
 *   shows no hashline rows; packages/tui/src/tools/match-line-format.ts). A search of several
 *   files (a directory or a list) keeps 20 matches of a file and shows 20 files a page, from the
 *   file after the `skip` it is given, under a prefix-folded tree of `#` headers, each file's
 *   `#<tag>` beside its name in a hashline session (packages/tui/src/tools/grouped-file-output.ts
 *   lines 65-154, packages/utils/src/path-tree.ts); a search of one file keeps 200, under
 *   `[<path>#<tag>]` (grep.ts lines 83-96 and 985-1045). After them, each after a blank line
 *   (grep.ts lines 1221-1260): `Showing files <a>-<b> of <n>. Use skip=<b> for the next page, or
 *   narrow paths/pattern.` where more files match than the page shows (`<n>+` where a searched
 *   path's native search stopped at its 2,000 matches), the missing paths' note, and `More than 20
 *   matches in <files>; grep keeps the first 20 per file when searching several. Search one file
 *   alone for up to 200.` (its first five files, then `and <n> more files`) or `More than 200
 *   matches; grep keeps the first 200 in a file. Add a line range (`path:N-M`) or narrow the
 *   pattern for the rest.` where a file holds more matches than are kept. The match output stops
 *   at a line boundary within 50 KiB, reserving space for those trailers. On a byte stop the
 *   multi-file cap trailer names only files shown in full. The bracketed notice after the
 *   trailers gives `Showing lines 1-<b> of <total> (<bytes shown> limit). Use :<b+1> to continue`
 *   on a byte stop and `Some lines truncated to 512 bytes` if a match line was cut, joined with
 *   `. ` when both apply (packages/tui/src/tools/output-meta.ts). `No matches found` when none,
 *   the missing paths' note on the next line; `No more results (<n> files total; skip=<k> is past
 *   the end)` for a page past the last. Its paths are
 *   relative to the working directory when under it, else absolute (path-utils.ts lines
 *   512-531, `formatPathRelativeToCwd`, which shows no `~`: a path under the session's home is
 *   absolute, where a read's header shows `~/…`; a `~` path given resolves against the session's
 *   home where the copy names one); its regular expression is JavaScript's in Unicode mode,
 *   case-sensitive unless `case`
 *   is false, which backtracks where the pin's (Rust's regex) does not, so the answer is worked
 *   out in a worker thread (`answerInWorker`), never the runner's own, within BASH_TIME_LIMIT_MS
 *   (30 s), past which the worker is ended; and since JavaScriptCore stops a backtracking match
 *   after a count of its steps and gives no match where the pin finds one, a call one test of
 *   whose expression over a line (or of its exclusions' over a path) takes past REGEX_TEST_MS
 *   (100 ms) is not answered (`timedExec`). Each file's whole text is tested first, and its lines
 *   only where that test finds a match or takes past REGEX_TEST_MS (`grepScan`, where the
 *   invariant that loses no match is stated). A pattern that starts with one inline flag group
 *   `(?FLAGS)`, FLAGS one or more of `i`, `m` and `s` each once, which Rust's regex takes and
 *   JavaScript's does not, is read without it, its letters added to the expression's flags (`i`
 *   as `case` false adds it).
 *   Not answered: a glob or a selector in a searched path, a path outside the root, a path (or
 *   every path of a list) the copy lacks, a path named twice, an exclusion of a form the runner
 *   does not read (`dialectGlob`), a list joined by commas or blanks, a file named alone past
 *   4 MiB (which the pin streams), a pattern over lines or with a look-around, a back-reference,
 *   a named group in Rust's own form or a flag group other than that leading one (one elsewhere,
 *   a scoped `(?i:…)`, a negated one, one of another flag), one JavaScript does not take, a cut
 *   match line of a file that is not UTF-8, an output the pin spills to a session artifact, a
 *   test of its expression over a line (or of its exclusions' over a path) past 100 ms, and a
 *   search past the time limit.
 * - `glob` (keys `path`, `hidden`, `gitignore`, `limit`, `i`): GlobTool's output
 *   (packages/coding-agent/src/tools/glob.ts lines 197-375, path-utils.ts's `parseFindPattern`
 *   and `splitDelimitedPathEntry`): `path` (`.` when absent) a pattern, or a list of patterns
 *   joined by `;` (outside braces; unless the whole is a path of the checkout), each read from its
 *   first segment with a glob character (one that starts with a glob character is searched at
 *   every depth); a pattern with none is a directory, every file and directory under which
 *   matches, or a file, which answers itself. The matching files and directories (a directory's
 *   path ending in `/`; each path as grep's is, relative to the working directory or absolute),
 *   as a prefix-folded tree in their order (`formatGroupedPaths`), or `No files found
 *   matching pattern`; dotfiles left out when `hidden` is false; then, each on a line of its own,
 *   `Requested limit <n> clamped to the max of 200` for a limit above it and `Skipped missing
 *   paths: <patterns>` for the patterns of a list whose directory is not there. The order is the
 *   pin's (glob.ts lines 582, 600-605, 635 and 644-657; crates/pi-walker/src/glob.rs lines
 *   270-279 and 334-340): newest first by the time of change the copy's own files hold (an
 *   lstat's, in whole milliseconds; a file named alone, its followed stat's), each pattern's
 *   matches a tie by their path below its base in byte order, then the patterns' paths merged,
 *   each once, a tie kept in their order. Its matching
 *   (Bun.Glob's, of each path under its base) is worked out in a worker thread (`answerInWorker`),
 *   within BASH_TIME_LIMIT_MS, as grep's is. Not answered: a
 *   list joined by commas or blanks, a base outside the root, one pattern (or every pattern of a
 *   list) whose base is not there or is a file under a glob (the pin's error is not rebuilt),
 *   more matches than `limit` (200 when absent), and a search past the time limit.
 * - `bash` (keys `command`, `cwd`, `timeout`, `i`, and `pty`, `async`, `name` and `ready` where
 *   each asks for nothing, `bashOptionFault`): the command run in the sandbox of the copy
 *   (above; its working directory the session's mapped into the copy; the checkout's root in the
 *   command mapped to CHECKOUT_MOUNT, and that path and the copy's own, as given and as its real
 *   path, back to the root in the output; its HOME the session's home as the sandbox shows it,
 *   `sandboxHome`, where the copy names one), by `bash -c` with its standard error joined to its
 *   output, within BASH_TIME_LIMIT_MS of when its sandbox is made (the queue, above); its text
 *   as Oh My Pi's bash tool gives it (bash.ts lines
 *   765-922): the output as printed, `(no output, exit code 0)` or `(no output)` for none, `Wall
 *   time: <s> seconds` after it (`<s>` to two places) when the command took 1,000 ms or more
 *   (WALL_TIME_NOTICE_MIN_MS and `formatWallTimeNotice`, packages/tui/src/tools/bash.ts lines
 *   222-232; bash.ts lines 830-845), measured from its sandbox's ready marker, as its command
 *   starts, to its end (neither its wait in the queue nor its sandbox's making counts), and
 *   `Command exited with code <n>` after it, an error, when it failed. Oh My Pi's
 *   bash interceptor is off at its default settings (exec/settings.ts lines 170-180), so no
 *   command is blocked. Its git commands run on the copy's own history: the copy is the shared
 *   checkout of the commit (cli.ts's `sharedCheckout`: a fetch of the commit 50 commits deep with
 *   the blobs of each, checked out with a detached HEAD, its `origin` the repository's GitHub URL
 *   as a session's checkout names it), so `git log` shows at most 50 commits and then the shallow
 *   boundary, `git remote -v` shows `https://github.com/<repo>.git`, and `git status` names no
 *   branch (and shows each link removed as deleted); git runs with GIT_OPTIONAL_LOCKS=0, so `git
 *   status` writes no index, and with no transport, so a blob a copy lacks is not fetched (the
 *   answer is git's error). Its jj commands
 *   run on the copy too: where the context session ran jj, cli.ts made the copy a colocated jj
 *   repository (`colocateJj`), so they answer as in a jj workspace at the commit, each with
 *   `--ignore-working-copy` (the snapshot the colocation took, no lock taken, nothing written);
 *   where it ran none, as jj does in a checkout that is no jj repository (`There is no jj repo`),
 *   since the sandbox shows no directory above the copy. Not answered: `pty: true`, `async: true`,
 *   a service's `name`, a `ready` with a log or a port, and an option's value the pin's validation
 *   reads in a way the runner does not rebuild (`bashOptionFault`); a git or jj command where
 *   the copy is GitHub's tarball instead (the fetch failed: `CheckoutCopy.git` false), a jj
 *   command where the copy could not be made a jj repository (`CheckoutCopy.jj` a why) or the
 *   runner has no jj, a command a word of which may lead outside the copy (`escapes`), a jj
 *   command whose repository may be one the checkout tracks below its root, a jj command of a
 *   copy whose root holds a `.jj` no colocation of the runner's made, a command one of
 *   whose programs is on no directory of the sandbox's PATH (this pod's has no `rg`: bash would
 *   answer `command not found`, which the session did not see), a command whose sandbox cannot be
 *   made, and a run past the time limit. Nor is an output Oh My Pi shows
 *   with a notice that names the artifact the session saves the whole output to (bash.ts line 1097,
 *   `allocateOutputArtifact`), whose id is the session's own count of its artifacts, which the
 *   copy cannot know: one past 50 KB (`tools.artifactSpillThreshold`, which Oh My Pi elides), one
 *   with a line past 768 bytes (`tools.outputMaxColumns`: Oh My Pi cuts the line to its first 765
 *   bytes and `…`, streaming-output.ts lines 1153-1215, and notes `[Some lines truncated to 768
 *   bytes. Use artifact://<id>:raw:<n>-<n> …]`, output-meta.ts lines 365-431), and one of 1,000
 *   characters or more that Oh My Pi's minimizer may rewrite (crates/pi-shell/src/minimizer/
 *   plan.rs lines 1-32 and engine.rs lines 16-160: a command whose top level is simple commands
 *   joined by `&&` and `;`, one of them, unpiped, of MINIMIZED_PROGRAMS; a group or a subshell
 *   there, or a `||`, keeps the whole output as it is, though the runner, whose parse does not
 *   tell `||` from `&&`, refuses one with a `||` all the same), whose rewrite ends with
 *   `[raw output: artifact://<id>]` (exec/bash-executor.ts lines 918-947); the runner cannot
 *   tell, without porting each filter, where a filter leaves an output as it is.
 * - `find` and `ls`: no capture declares them, so they are not answered.
 */
import { isUtf8 } from "node:buffer";
import {
  closeSync,
  existsSync,
  fstatSync,
  lstatSync,
  openSync,
  readdirSync,
  readFileSync,
  readSync,
  realpathSync,
  rmSync,
  type Stats,
  statSync,
} from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { isMainThread, parentPort, Worker, workerData } from "node:worker_threads";
import {
  fileTag,
  formatBytes,
  outputNotice,
  type PathRows,
  pathAnswer,
  pathRangeAnswer,
  readRange,
  showingLines,
  shownPath,
} from "./arms";
import { type Case, insideRoot, resolvedReadPath, textLines } from "./cases";
import { bytesAt, eachChunk, firstLine, Limiter, timeLimit } from "./common";

/** The tools whose every call is read-only. */
export const READ_ONLY_TOOLS = ["read", "grep", "glob", "find", "ls"] as const;
/** The programs a read-only `bash` call's commands may run (see the file's header). */
const READ_ONLY_PROGRAMS = [
  "cd",
  "cat",
  "head",
  "tail",
  "ls",
  "find",
  "grep",
  "rg",
  "wc",
  "sed",
  "nl",
  "stat",
  "file",
  "tree",
  "git",
  "jj",
  "pwd",
  "true",
  "echo",
  "printf",
  "env",
  "unset",
  "export",
] as const;
/** The git subcommands a read-only `bash` call may run, beside `worktree list`, `remote` and
 *  `branch` with no name (`gitReadsOnly`). */
const READ_ONLY_GIT = [
  "log",
  "show",
  "diff",
  "status",
  "grep",
  "ls-files",
  "blame",
  "rev-parse",
] as const;
/** The options of `git branch` that name no branch to make, move or delete. */
const GIT_BRANCH_LISTS = /^(-[arvl]+|--all|--remotes|--verbose|--list|--show-current)$/;
/** The jj commands a read-only `bash` call may run: the words after `jj` and its options. */
const READ_ONLY_JJ = [
  "status",
  "st",
  "log",
  "show",
  "diff",
  "root",
  "file show",
  "bookmark list",
  "workspace list",
] as const;
/** jj's options that name the repository a command reads (`-R`, `--repository`), and its other
 *  global options that take a value, as `parseOptions` reads them. */
const JJ_REPOSITORY: OptionSpec = {
  values: "R",
  long: {
    repository: "required",
    "at-op": "required",
    "at-operation": "required",
    color: "required",
  },
};
/** The words of jj's options that take the next word as their value (JJ_REPOSITORY's, as written
 *  in full), as `jjReadsOnly` reads them. */
const JJ_VALUE_OPTIONS: ReadonlySet<string> = new Set([
  ...[...JJ_REPOSITORY.values].map((letter) => `-${letter}`),
  ...Object.entries(JJ_REPOSITORY.long).flatMap(([name, takes]) =>
    takes === "required" ? [`--${name}`] : []
  ),
]);
/** How a program reads its options (`parseOptions`, the one rule of every program's options):
 *  `flags`, its short options that take no value (read only where `strict`); `values`, those that
 *  take one (the rest of their word, else the next word); `optional`, those whose value is only the
 *  rest of their word (sed's `-i[SUFFIX]`); `long`, its long options, each with whether it takes a
 *  value (`required`: after its `=`, else the next word; `optional`: after its `=` only);
 *  `inOrder`, whether its first operand ends its options (bash's builtins, `env`); `strict`,
 *  whether a short option it does not have, or a long one that names none or several of its own,
 *  makes the words unread, as the program refuses them. A program that is not strict lists only
 *  the options the read-only rule must find, and those whose value it must not take for an option;
 *  any other is read as an option of no value, so no word it takes as a value escapes the check. */
interface OptionSpec {
  readonly flags?: string;
  readonly values: string;
  readonly optional?: string;
  readonly long: Readonly<Record<string, "none" | "required" | "optional">>;
  readonly inOrder?: true;
  readonly strict?: true;
}
/** sed's options (GNU sed 4.9): `-e`/`--expression` gives a script, `-f`/`--file` a script file,
 *  `-i`/`--in-place` edits the files given. */
const SED_OPTIONS: OptionSpec = {
  flags: "bnrsuzE",
  values: "efl",
  optional: "i",
  long: {
    expression: "required",
    file: "required",
    "in-place": "optional",
    "line-length": "required",
    "null-data": "none",
    "zero-terminated": "none",
    quiet: "none",
    silent: "none",
    "regexp-extended": "none",
    separate: "none",
    sandbox: "none",
    debug: "none",
    posix: "none",
    "follow-symlinks": "none",
    unbuffered: "none",
    binary: "none",
    help: "none",
    version: "none",
  },
  strict: true,
};
/** bash's `printf`: `-v NAME` (or `-vNAME`) sets the variable NAME to what it prints. */
const PRINTF_OPTIONS: OptionSpec = { values: "v", long: {}, inOrder: true, strict: true };
/** env's options (GNU coreutils 9): those of ENV_PRINTS print the environment; any other, with a
 *  command or not, is none a read-only call gives. */
const ENV_OPTIONS: OptionSpec = {
  flags: "iv0",
  values: "aCSu",
  long: {
    "ignore-environment": "none",
    null: "none",
    unset: "required",
    chdir: "required",
    "split-string": "required",
    argv0: "required",
    debug: "none",
    "block-signal": "optional",
    "default-signal": "optional",
    "ignore-signal": "optional",
    "list-signal-handling": "none",
    help: "none",
    version: "none",
  },
  inOrder: true,
  strict: true,
};
/** The options of env that print the environment, less the variables `-u` names. */
const ENV_PRINTS = ["i", "0", "u", "ignore-environment", "null", "unset"];
/** git's long options that run a program, write a file or read one no word names, with whether
 *  each takes a value, for their abbreviations (GIT_RUNS). */
const GIT_LONG: OptionSpec["long"] = {
  output: "required",
  "open-files-in-pager": "optional",
  "ext-diff": "none",
  "no-index": "none",
  "show-signature": "none",
};
/** The options of git's read-only subcommands that run a program, write a file or read one no
 *  word names: `--output=<file>` (log, show, diff), `-O` (`git grep -O[<pager>]` runs the pager on
 *  the files found; log, show and diff read an order file), `--open-files-in-pager`, `--ext-diff`
 *  (a diff program), `--no-index` (`git diff` of files outside the repository) and
 *  `--show-signature` (runs gpg); git takes any start of a long option's name that is no other's. */
const GIT_RUNS = ["O", ...Object.keys(GIT_LONG)];
/** The short options of each read-only git subcommand that take a value, so a letter of a value is
 *  no option: grep's pattern, file, context and count; log's, show's and diff's pickaxe (`-S`,
 *  `-G`), log's and show's count (`-n`), log's and blame's range (`-L`), blame's revisions file. */
const GIT_VALUES: Readonly<Record<string, string>> = {
  grep: "efABCm",
  log: "SGnL",
  show: "SGn",
  diff: "SG",
  blame: "LS",
};
/** The options of the other read-only programs that run a program, write a file, or read a file
 *  outside the checkout that no word of the command names (`runs`), and how each reads its options
 *  (`spec`): rg's preprocessor (`--pre`, `--pre-glob`), decompressors (`-z`, `--search-zip`) and
 *  host-name program (`--hostname-bin`); tree's output file (`-o`), its runs of itself that write
 *  one in each directory (`-R`) and its listings read from files (`--fromfile`, `--fromtabfile`,
 *  whose paths no word names); wc's list of files to count (`--files0-from`, GNU coreutils 9.7,
 *  any start of its name: `--files0`, `--f`); file's compiled magic (`-C`, `--compile`), magic
 *  files (`-m`, `--magic-file`, a list), list of files (`-f`, `--files-from`), access times (`-p`,
 *  `--preserve-date`) and decompressors (`-z`, `-Z`, `--uncompress`, `--uncompress-noreport`);
 *  jj's diff tool (`--tool`) and configurations (`--config`, `--config-toml`, `--config-file`,
 *  which may name a tool; `--to` is one of jj's own, not an abbreviation of `--tool`). An option
 *  that reads one file its value names (grep's and rg's `-f`, grep's `--exclude-from`, rg's
 *  `--ignore-file`, git's `ls-files -X` and `blame --contents`) is no such option: `escapes`
 *  reads its value as a path. */
const PROGRAM_OPTIONS: Readonly<
  Record<string, { readonly spec: OptionSpec; readonly runs: readonly string[] }>
> = {
  rg: {
    spec: {
      values: "ABCdeEfgjmMrtT",
      long: {
        pre: "required",
        "pre-glob": "required",
        "search-zip": "none",
        "hostname-bin": "required",
      },
    },
    runs: ["pre", "pre-glob", "z", "search-zip", "hostname-bin"],
  },
  tree: {
    spec: { values: "", long: { fromfile: "none", fromtabfile: "none" } },
    runs: ["o", "R", "fromfile", "fromtabfile"],
  },
  wc: {
    spec: { values: "", long: { "files0-from": "required", total: "required" } },
    runs: ["files0-from"],
  },
  file: {
    spec: {
      values: "efFmP",
      long: {
        compile: "none",
        "magic-file": "required",
        "files-from": "required",
        "preserve-date": "none",
        uncompress: "none",
        "uncompress-noreport": "none",
      },
    },
    runs: [
      "C",
      "m",
      "f",
      "p",
      "z",
      "Z",
      "compile",
      "magic-file",
      "files-from",
      "preserve-date",
      "uncompress",
      "uncompress-noreport",
    ],
  },
  jj: {
    spec: {
      values: "",
      long: {
        tool: "required",
        config: "required",
        "config-toml": "required",
        "config-file": "required",
        to: "required",
      },
    },
    runs: ["tool", "config", "config-toml", "config-file"],
  },
};
/** The expressions of `find` that run a command, write a file, or read its starting points from a
 *  file (`-files0-from`, GNU findutils 4.10, whose paths no word names). */
const FIND_ACTIONS: Readonly<Record<string, true>> = {
  "-exec": true,
  "-execdir": true,
  "-ok": true,
  "-okdir": true,
  "-delete": true,
  "-fprint": true,
  "-fprint0": true,
  "-fprintf": true,
  "-fls": true,
  "-files0-from": true,
};
/** A shell variable's name. */
const NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;
/** The variables an assignment, `export` or `printf -v` may not set: each picks a program, a
 *  library, a configuration or a directory the shell or a read-only program reads. */
const PROTECTED_NAME =
  /^(PATH|HOME|PWD|OLDPWD|CDPATH|IFS|ENV|SHELLOPTS|GLOBIGNORE|PS4|PROMPT_COMMAND|SHELL|TMPDIR|PAGER|MANPAGER|EDITOR|VISUAL|MAGIC|RIPGREP_CONFIG_PATH|GCONV_PATH|LOCPATH|NLSPATH|GLIBC_TUNABLES|TERMINFO|TERMINFO_DIRS|(BASH|GIT_|JJ_|LD_|DYLD_|XDG_|LESS)\w*)$/;
/** The variables `unset` may not unset: with no PATH the shell runs a program of the checkout's,
 *  and the others are the answers' own git and jj settings (SANDBOX_ENV: git's configuration
 *  through GIT_CONFIG_COUNT, jj's through JJ_CONFIG) and GIT_CONFIG_PARAMETERS, the configuration
 *  git's `-c` gives. */
const UNSET_PROTECTED =
  /^(PATH|HOME|PWD|GIT_OPTIONAL_LOCKS|GIT_CONFIG_(COUNT|KEY_\d+|VALUE_\d+|SYSTEM|PARAMETERS)|JJ_CONFIG)$/;
/** The shell's own variables a read-only command may expand without assigning them. */
const SHELL_OWN: Readonly<Record<string, true>> = { HOME: true, PATH: true, PWD: true };
/** The read-only programs that open no file their words name. */
const NO_PATH_PROGRAMS: Readonly<Record<string, true>> = {
  pwd: true,
  true: true,
  echo: true,
  printf: true,
  env: true,
  unset: true,
  export: true,
};
/** The read-only programs bash runs as builtins of its own, which no directory of PATH holds. */
const SHELL_BUILTINS: Readonly<Record<string, true>> = {
  cd: true,
  pwd: true,
  true: true,
  echo: true,
  printf: true,
  unset: true,
  export: true,
};
/** The most values a word may take that the confinement follows. */
const MAX_VALUES = 64;

/** A part of a word: literal text, a variable's expansion (`$NAME`, `${NAME}`), or a command
 *  substitution (`$(…)` or a backtick, its text `source`), each in quotes or not (`quoted`). */
type WordPart =
  | { readonly kind: "text"; readonly text: string; readonly quoted: boolean }
  | { readonly kind: "var"; readonly name: string; readonly quoted: boolean }
  | {
      readonly kind: "sub";
      readonly script: Script;
      readonly source: string;
      readonly quoted: boolean;
    };
/** A word of a command, its parts in order. */
type Word = readonly WordPart[];
/** An assignment `NAME=value`. */
interface Assignment {
  readonly name: string;
  readonly value: Word;
}
/** A redirection: its operator (after its stream's digits) and its target. */
interface Redirect {
  readonly op: string;
  readonly target: Word;
}
/** A simple command: the assignments before its words, its words, and its redirections. */
interface SimpleCommand {
  readonly kind: "simple";
  readonly assignments: readonly Assignment[];
  readonly words: readonly Word[];
  readonly redirects: readonly Redirect[];
}
/** A subshell `( … )` or a group `{ …; }`, and its redirections. */
interface Group {
  readonly kind: "group";
  readonly subshell: boolean;
  readonly script: Script;
  readonly redirects: readonly Redirect[];
}
/** A parsed command: lists joined by `;` or line ends, each of pipelines joined by `&&` or `||`,
 *  each of commands joined by `|`. */
type Script = readonly (readonly (readonly (SimpleCommand | Group)[])[])[];

/** Thrown inside `parseScript` at a shell form the read-only rule does not take. */
class Untaken extends Error {}

/** `command` parsed, or undefined when it holds a shell form the read-only rule does not take: a
 *  background `&`, a heredoc or herestring, a process substitution, a comment, an arithmetic
 *  expansion (`$((…))`, `$[…]`) or command, an expansion other than `$NAME`, `${NAME}`, `$(…)` and
 *  a backtick with no `\` in it, `;;`, `|&`, or a quote left open. */
function parseScript(command: string): Script | undefined {
  let at = 0;
  const fail = (): never => {
    throw new Untaken();
  };
  const peek = (offset = 0): string | undefined => command[at + offset];
  const skipBlanks = (newlines: boolean): void => {
    for (;;) {
      const ch = peek();
      if (ch === " " || ch === "\t" || (newlines && ch === "\n")) at++;
      else if (ch === "\\" && peek(1) === "\n") at += 2;
      else return;
    }
  };
  /** Whether the `}` at `at` is a word of its own, which closes a group where a command starts. */
  const closesGroup = (): boolean => {
    const next = peek(1);
    return peek() === "}" && (next === undefined || " \t\n;|&)<>".includes(next));
  };
  const list = (end: "" | ")" | "}"): Script => {
    const lists: (SimpleCommand | Group)[][][] = [];
    for (;;) {
      skipBlanks(true);
      const ch = peek();
      if (ch === undefined) {
        if (end !== "") fail();
        break;
      }
      if ((end === ")" && ch === ")") || (end === "}" && closesGroup())) break;
      lists.push(andOr());
      skipBlanks(false);
      const next = peek();
      if (next === ";") {
        if (peek(1) === ";" || peek(1) === "&") fail();
        at++;
      } else if (next === "\n") at++;
      else if (next !== undefined && next !== ")") fail();
    }
    if (lists.length === 0) fail();
    return lists;
  };
  const andOr = (): (SimpleCommand | Group)[][] => {
    const pipelines = [pipeline()];
    for (;;) {
      skipBlanks(false);
      const pair = `${peek() ?? ""}${peek(1) ?? ""}`;
      if (pair !== "&&" && pair !== "||") return pipelines;
      at += 2;
      skipBlanks(true);
      pipelines.push(pipeline());
    }
  };
  const pipeline = (): (SimpleCommand | Group)[] => {
    const commands = [commandAt()];
    for (;;) {
      skipBlanks(false);
      if (peek() !== "|" || peek(1) === "|") return commands;
      if (peek(1) === "&") fail();
      at++;
      skipBlanks(true);
      commands.push(commandAt());
    }
  };
  const commandAt = (): SimpleCommand | Group => {
    skipBlanks(false);
    const ch = peek();
    const subshell = ch === "(";
    if (subshell || (ch === "{" && " \t\n".includes(peek(1) ?? "x"))) {
      if (subshell && peek(1) === "(") fail();
      at++;
      const script = list(subshell ? ")" : "}");
      at++;
      const redirects: Redirect[] = [];
      for (;;) {
        skipBlanks(false);
        const redirect = redirectAt();
        if (redirect === undefined) break;
        redirects.push(redirect);
      }
      const next = peek();
      if (next !== undefined && !";\n|&)".includes(next)) fail();
      return { kind: "group", subshell, script, redirects };
    }
    return simpleAt();
  };
  const simpleAt = (): SimpleCommand => {
    const assignments: Assignment[] = [];
    const words: Word[] = [];
    const redirects: Redirect[] = [];
    for (;;) {
      skipBlanks(false);
      const ch = peek();
      if (ch === undefined || ";\n|)".includes(ch) || (ch === "&" && peek(1) !== ">")) break;
      const redirect = redirectAt();
      if (redirect !== undefined) {
        redirects.push(redirect);
        continue;
      }
      // A `(` here, a comment, or a `}` where a command starts.
      if (ch === "(" || ch === "#") fail();
      if (words.length === 0 && assignments.length === 0 && closesGroup()) fail();
      const word = wordAt();
      const assignment = words.length === 0 ? assignmentOf(word) : undefined;
      if (assignment === undefined) words.push(word);
      else assignments.push(assignment);
    }
    if (assignments.length === 0 && words.length === 0 && redirects.length === 0) fail();
    return { kind: "simple", assignments, words, redirects };
  };
  const redirectAt = (): Redirect | undefined => {
    const fd = /^\d*/.exec(command.slice(at))?.[0] ?? "";
    const start = at + fd.length;
    const ch = command[start];
    const next = command[start + 1];
    let op: string;
    if (ch === "&" && fd === "" && next === ">") op = command[start + 2] === ">" ? "&>>" : "&>";
    else if (ch === ">" || ch === "<") {
      if (next === "(") fail();
      op = ch;
      if (ch === ">" && (next === ">" || next === "|" || next === "&")) op += next;
      if (ch === "<" && (next === "<" || next === "&" || next === ">")) op += next;
      if (op === "<<") fail();
    } else return undefined;
    at = start + op.length;
    skipBlanks(false);
    const target = peek();
    if (target === undefined || " \t\n;&|()<>".includes(target)) fail();
    return { op: `${fd}${op}`, target: wordAt() };
  };
  const wordAt = (): Word => {
    const parts: WordPart[] = [];
    const text = (more: string, quoted: boolean): void => {
      const last = parts.at(-1);
      if (last?.kind === "text" && last.quoted === quoted) {
        parts[parts.length - 1] = { kind: "text", text: last.text + more, quoted };
      } else parts.push({ kind: "text", text: more, quoted });
    };
    const dollar = (quoted: boolean): void => {
      const next = peek(1);
      if (next === "(") {
        if (peek(2) === "(") fail();
        at += 2;
        const start = at;
        const script = list(")");
        parts.push({ kind: "sub", script, source: command.slice(start, at), quoted });
        at++;
        return;
      }
      if (next === "{") {
        const end = command.indexOf("}", at + 2);
        const name = end < 0 ? "" : command.slice(at + 2, end);
        if (!NAME.test(name)) fail();
        parts.push({ kind: "var", name, quoted });
        at = end + 1;
        return;
      }
      const name = /^[A-Za-z_][A-Za-z0-9_]*/.exec(command.slice(at + 1))?.[0];
      if (name !== undefined) {
        parts.push({ kind: "var", name, quoted });
        at += 1 + name.length;
        return;
      }
      // A special parameter, bash's old arithmetic expansion `$[…]` (which, as `$((…))`, runs the
      // substitutions a variable it reads holds), or (unquoted) `$'…'` or `$"…"`.
      if (next === "[") fail();
      if (next !== undefined && (/[0-9?#@*!$-]/.test(next) || (!quoted && `'"`.includes(next)))) {
        fail();
      }
      text("$", quoted);
      at++;
    };
    const backtick = (quoted: boolean): void => {
      const end = command.indexOf("`", at + 1);
      if (end < 0) fail();
      const source = command.slice(at + 1, end);
      const script = source.includes("\\") ? undefined : parseScript(source);
      if (script === undefined) fail();
      else parts.push({ kind: "sub", script, source, quoted });
      at = end + 1;
    };
    for (;;) {
      const ch = peek();
      if (ch === undefined || " \t\n;&|()<>".includes(ch)) break;
      if (ch === "'") {
        const end = command.indexOf("'", at + 1);
        if (end < 0) fail();
        text(command.slice(at + 1, end), true);
        at = end + 1;
      } else if (ch === '"') {
        at++;
        text("", true);
        for (;;) {
          const inner = peek();
          if (inner === undefined) fail();
          if (inner === '"') break;
          if (inner === "\\") {
            const escaped = peek(1);
            if (escaped === undefined) fail();
            else if ('"\\$`\n'.includes(escaped)) text(escaped === "\n" ? "" : escaped, true);
            else text(`\\${escaped}`, true);
            at += 2;
          } else if (inner === "$") dollar(true);
          else if (inner === "`") backtick(true);
          else {
            text(inner ?? "", true);
            at++;
          }
        }
        at++;
      } else if (ch === "\\") {
        const next = peek(1);
        if (next === undefined) fail();
        if (next !== "\n") text(next ?? "", true);
        at += 2;
      } else if (ch === "$") dollar(false);
      else if (ch === "`") backtick(false);
      else {
        text(ch, false);
        at++;
      }
    }
    if (parts.length === 0) fail();
    return parts;
  };
  try {
    return list("");
  } catch (error) {
    if (error instanceof Untaken) return undefined;
    throw error;
  }
}

/** The assignment `word` makes, `NAME=value` with its name and `=` unquoted, or undefined. */
function assignmentOf(word: Word): Assignment | undefined {
  const [first, ...rest] = word;
  if (first?.kind !== "text" || first.quoted) return undefined;
  const match = /^([A-Za-z_][A-Za-z0-9_]*)=/.exec(first.text);
  if (match?.[1] === undefined) return undefined;
  const after = first.text.slice(match[0].length);
  return {
    name: match[1],
    value: after === "" ? rest : [{ kind: "text", text: after, quoted: false }, ...rest],
  };
}

/** The text of `word`, its quotes taken off and each expansion as written (`$NAME`, `$(…)`). */
function wordText(word: Word): string {
  return word
    .map((part) =>
      part.kind === "text" ? part.text : part.kind === "var" ? `$${part.name}` : `$(${part.source})`
    )
    .join("");
}

/** The text of `word` when it expands nothing, else undefined. */
function literal(word: Word): string | undefined {
  return word.every((part) => part.kind === "text") ? wordText(word) : undefined;
}

/** The unquoted text of `word`, as bash reads it for a brace expansion, a glob or a `~`: each
 *  quoted part, variable and substitution one character that expands nothing (`\u0001`), so a
 *  quoted or escaped `}`, `,`, `*` or `~` neither ends, splits nor makes one. */
function bareText(word: Word): string {
  return word.map((part) => (part.kind === "text" && !part.quoted ? part.text : "\u0001")).join("");
}

/** Whether `word`'s unquoted text holds a glob character (`*`, `?`, `[`): bash puts in its place
 *  the names of the files it matches, one of which may start with `-`. */
function globbed(word: Word): boolean {
  return /[*?[]/.test(bareText(word));
}

/** Whether `bare`, a word's unquoted text (`bareText`), holds a brace expansion (`{a,b}`,
 *  `{1..3}`), which bash makes into several words: a `{`, then a `,` or a `..`, then a `}`, with no
 *  other `}` between them. One pass, in time linear in the text (a regular expression of it
 *  backtracks, cubic in a word of `{,` repeated). */
function holdsBrace(bare: string): boolean {
  let open = false;
  let split = false;
  for (let at = 0; at < bare.length; at++) {
    const ch = bare[at];
    if (ch === "{") open = true;
    else if (ch === "}") {
      if (open && split) return true;
      open = false;
      split = false;
    } else if (open && (ch === "," || (ch === "." && bare[at + 1] === "."))) split = true;
  }
  return false;
}

/** What a variable or a word may hold at a point of a command: each value it may take, or
 *  undefined when one of them is unknown. */
type Values = ReadonlySet<string> | undefined;

/** A command's shell at a point of it: each variable the command assigned, exported or unset
 *  before it, with what it may hold; every directory it may be in (the session's paths; none where
 *  the walk follows no directory); and whether one of those may be, as the kernel holds it,
 *  elsewhere than its path as written (`linked`: a working directory whose real path in the copy is
 *  not its path, or a `cd` that may have gone through a symbolic link, `Walk.cd`), where the kernel
 *  walks a later `..` from the real directory and the walk from the path. */
interface ShellState {
  readonly vars: Map<string, Values>;
  cwds: readonly string[];
  linked: boolean;
}

/** A walk of a command (`walkScript`): `check`, why one simple command at its point fails, or
 *  undefined; `subValues`, what a command substitution may print, as a word's value; `cd`, what a
 *  `cd` from the directory `from` (a session path) to `to` (a value of its word) does: `enters`,
 *  whether it surely succeeds, the shell then at `path.posix.resolve(from, to)`; `elsewhere`,
 *  another directory it may end at (a session path); and `linked`, whether the directory it may
 *  end at may be, as the kernel holds it, elsewhere than its path as written (absent, any `cd` may
 *  fail, and none goes through a link). */
interface Walk {
  readonly check: (command: SimpleCommand, state: ShellState) => string | undefined;
  readonly subValues: (script: Script, state: ShellState) => Values;
  readonly cd?: (
    from: string,
    to: string
  ) => { readonly enters: boolean; readonly linked: boolean; readonly elsewhere?: string };
}

/** `state` for a shell of its own: what that shell changes stays there. */
function forked(state: ShellState): ShellState {
  return { vars: new Map(state.vars), cwds: state.cwds, linked: state.linked };
}

/** What the variable `name` may hold at `state`: what the command gave it, or for `$PWD` each
 *  directory the shell may be in; undefined when unknown. */
function variableValues(name: string, state: ShellState): Values {
  if (state.vars.has(name)) return state.vars.get(name);
  return name === "PWD" && state.cwds.length > 0 ? new Set(state.cwds) : undefined;
}

/** Every value `word` may take at `state` (`walk.subValues` giving a substitution's), or undefined
 *  when one is unknown, or more than MAX_VALUES: an unquoted expansion the shell may split or match
 *  against files (empty, or holding a blank or a glob character) is unknown. */
function valuesOf(word: Word, state: ShellState, walk: Walk): Values {
  let values = new Set([""]);
  for (const part of word) {
    let held: Values;
    if (part.kind === "text") held = new Set([part.text]);
    else {
      held =
        part.kind === "var" ? variableValues(part.name, state) : walk.subValues(part.script, state);
      if (
        !part.quoted &&
        [...(held ?? [])].some((value) => value === "" || /[\s*?[]/.test(value))
      ) {
        held = undefined;
      }
    }
    if (held === undefined) return undefined;
    const next = new Set<string>();
    for (const before of values) for (const value of held) next.add(before + value);
    if (next.size > MAX_VALUES) return undefined;
    values = next;
  }
  return values;
}

/** The one value `word` takes at `state` (`valuesOf`), or undefined when it may take several or
 *  one unknown. */
function oneValue(word: Word, state: ShellState, walk: Walk): string | undefined {
  const held = valuesOf(word, state, walk);
  return held?.size === 1 ? [...held][0] : undefined;
}

/** Every word of `command`, in order: a simple command's words and its assignments' values, then
 *  each redirection's target. */
function commandWords(command: SimpleCommand | Group): Word[] {
  return [
    ...(command.kind === "simple"
      ? [...command.words, ...command.assignments.map((each) => each.value)]
      : []),
    ...command.redirects.map((each) => each.target),
  ];
}

/** Walks `script` from `state` (see the file's header): gives `walk.check` each simple command at
 *  its point (a group's redirections as a command of no words), each command substitution's
 *  before the command that holds it; and keeps in `state` what each command's assignments,
 *  `export`, `unset`, `printf -v` and `cd` change, only as possible where `maybe` (a pipeline
 *  after the first of an `&&` or `||` list), and only in the shell that runs it (a subshell, a
 *  substitution and each command of a pipeline of several run in one of their own). Returns the
 *  first why `walk.check` gives. */
function walkScript(
  script: Script,
  state: ShellState,
  maybe: boolean,
  walk: Walk
): string | undefined {
  for (const pipelines of script) {
    for (const [index, pipeline] of pipelines.entries()) {
      for (const command of pipeline) {
        const own = pipeline.length > 1 ? forked(state) : state;
        const why = walkCommand(command, own, maybe || index > 0, walk);
        if (why !== undefined) return why;
      }
    }
  }
  return undefined;
}

/** `walkScript` of one command. */
function walkCommand(
  command: SimpleCommand | Group,
  state: ShellState,
  maybe: boolean,
  walk: Walk
): string | undefined {
  for (const word of commandWords(command)) {
    for (const part of word) {
      if (part.kind !== "sub") continue;
      const why = walkScript(part.script, forked(state), false, walk);
      if (why !== undefined) return why;
    }
  }
  if (command.kind === "group") {
    const inner = command.subshell ? forked(state) : state;
    const why = walkScript(command.script, inner, maybe, walk);
    if (why !== undefined) return why;
    const redirects: SimpleCommand = {
      kind: "simple",
      assignments: [],
      words: [],
      redirects: command.redirects,
    };
    return walk.check(redirects, state);
  }
  const why = walk.check(command, state);
  if (why === undefined) changeState(command, state, maybe, walk);
  return why;
}

/** Keeps in `state` what the simple command `command` changes in its shell (see `walkScript`). */
function changeState(command: SimpleCommand, state: ShellState, maybe: boolean, walk: Walk): void {
  const set = (name: string, values: Values) => {
    const held = state.vars.get(name);
    if (!maybe) state.vars.set(name, values);
    else if (held === undefined || values === undefined) state.vars.set(name, undefined);
    else state.vars.set(name, new Set([...held, ...values]));
  };
  const [first, ...args] = command.words;
  if (first === undefined) {
    for (const { name, value } of command.assignments) set(name, valuesOf(value, state, walk));
    return;
  }
  const program = literal(first);
  const texts = args.map(literal);
  if (program === "export") {
    for (const word of args) {
      const assignment = assignmentOf(word);
      if (assignment !== undefined) set(assignment.name, valuesOf(assignment.value, state, walk));
    }
  } else if (program === "unset" && !texts.includes("-f")) {
    for (const name of texts) if (name !== undefined && NAME.test(name)) set(name, new Set([""]));
  } else if (program === "printf") {
    const name = printfTarget(args.map((word) => oneValue(word, state, walk)));
    if (name !== undefined && name !== "") set(name, undefined);
  } else if (program === "cd") {
    const operands = args.filter((word) => !(literal(word) ?? "").startsWith("-"));
    const [target] = operands;
    const values = target === undefined ? undefined : valuesOf(target, state, walk);
    if (target === undefined || values === undefined) return;
    const landings = state.cwds.flatMap((from) =>
      [...values].map((to) => ({
        dir: path.posix.resolve(from, to),
        ...(walk.cd?.(from, to) ?? { enters: false, linked: false }),
      }))
    );
    // bash's cd fails at an option it lacks, at a second operand, and at a path a part of which
    // before a `..`, or whose whole, is no directory: a cd of one word, a literal and no option, that
    // `walk.cd` enters from each directory surely moves the shell there where it surely runs. Any
    // other may fail, and the shell stay where it was, or end where the kernel's walk of the path
    // as written does (`cd -P`, or bash's own where its walk finds no directory).
    const certain =
      !maybe &&
      args.length === 1 &&
      literal(target) !== undefined &&
      landings.length > 0 &&
      landings.every((landing) => landing.enters);
    const moved = landings.flatMap((landing) =>
      certain || landing.elsewhere === undefined ? [landing.dir] : [landing.dir, landing.elsewhere]
    );
    state.cwds = [...new Set(certain ? moved : [...state.cwds, ...moved])];
    state.linked ||= landings.some((landing) => landing.linked);
  }
}

/** Whether a redirection `op` to `target` writes no file: a copy of one stream into another, a
 *  write to `/dev/null`, or a read of a file. */
function harmlessRedirect({ op, target }: { op: string; target: string }): boolean {
  if (/^\d*[<>]&$/.test(op)) return /^(\d+|-)$/.test(target);
  if (/^\d*<$/.test(op)) return target !== "";
  return target === "/dev/null";
}

/** One option a program's words give: every name it may be (a short option's letter; a long
 *  option's own name, or each name of the program's own it is the start of), and its value. */
interface GivenOption {
  readonly names: readonly string[];
  readonly value?: string;
}

/** The options and the operands of a program's words `words` (each undefined where its value is
 *  not known), read by `spec` as GNU's getopt_long reads them: the one rule of every program's
 *  options. A word `-abc` is a cluster of short options, each letter one, and a letter that takes
 *  a value takes the rest of the word (`-vNAME`, `-nO<cmd>`), else the next word; a word
 *  `--name[=value]` is the long option it names or, where none is that name, each of the
 *  program's own long options that start with it (git and getopt take any start of a name that
 *  is no other's); `--` ends the options of a program that is strict or reads them in order, while
 *  a program that is not strict is read on past it (a `--` may be a value the spec does not know
 *  of); a strict program's first operand ends its options where it reads them in order. Undefined
 *  where a word read as a possible option is not known, or the strict program refuses them. */
function parseOptions(
  words: readonly (string | undefined)[],
  spec: OptionSpec
): { options: GivenOption[]; operands: (string | undefined)[] } | undefined {
  const options: GivenOption[] = [];
  const operands: (string | undefined)[] = [];
  // The next word, as an option's value: undefined (unread) where there is none or it is unknown.
  const valueAfter = (at: number): { value: string } | "none" | undefined => {
    if (at + 1 >= words.length) return "none";
    const value = words[at + 1];
    return value === undefined ? undefined : { value };
  };
  for (let at = 0; at < words.length; at++) {
    const word = words[at];
    if (word === undefined) return undefined;
    if (word === "--") {
      if (spec.strict !== true && spec.inOrder !== true) continue;
      operands.push(...words.slice(at + 1));
      break;
    }
    if (word.startsWith("--")) {
      const equals = word.indexOf("=");
      const name = word.slice(2, equals < 0 ? undefined : equals);
      const names = Object.hasOwn(spec.long, name)
        ? [name]
        : Object.keys(spec.long).filter((each) => each.startsWith(name));
      if (spec.strict === true && names.length !== 1) return undefined;
      const takes = names.length === 1 ? spec.long[names[0] ?? ""] : "none";
      let value = equals < 0 ? undefined : word.slice(equals + 1);
      if (value !== undefined && takes === "none" && spec.strict === true) return undefined;
      if (value === undefined && takes === "required") {
        const next = valueAfter(at);
        if (next === undefined || (next === "none" && spec.strict === true)) return undefined;
        if (next !== "none") {
          value = next.value;
          at++;
        }
      }
      options.push({
        names: names.length === 0 ? [name] : names,
        ...(value === undefined ? {} : { value }),
      });
      continue;
    }
    if (word.startsWith("-") && word !== "-") {
      for (let letter = 1; letter < word.length; letter++) {
        const short = word[letter] ?? "";
        const rest = word.slice(letter + 1);
        if (spec.values.includes(short)) {
          let value: string | undefined = rest;
          if (rest === "") {
            const next = valueAfter(at);
            if (next === undefined || (next === "none" && spec.strict === true)) return undefined;
            value = next === "none" ? undefined : next.value;
            if (next !== "none") at++;
          }
          options.push({ names: [short], ...(value === undefined ? {} : { value }) });
          break;
        }
        if (spec.optional?.includes(short) === true) {
          options.push({ names: [short], ...(rest === "" ? {} : { value: rest }) });
          break;
        }
        if (spec.strict === true && !(spec.flags ?? "").includes(short)) return undefined;
        options.push({ names: [short] });
      }
      continue;
    }
    operands.push(word);
    if (spec.inOrder === true) {
      operands.push(...words.slice(at + 1));
      break;
    }
  }
  return { options, operands };
}

/** Whether `options` give one of `names`, under any name each may be. */
function givesAny(options: readonly GivenOption[], names: readonly string[]): boolean {
  return options.some((option) => option.names.some((name) => names.includes(name)));
}

/** The variable bash's `printf` sets with its words `values` (`-v NAME`, `-vNAME`), "" where it
 *  sets none, or undefined where its options are not read (PRINTF_OPTIONS). */
function printfTarget(values: readonly (string | undefined)[]): string | undefined {
  const parsed = parseOptions(values, PRINTF_OPTIONS);
  if (parsed === undefined) return undefined;
  return parsed.options.find((option) => option.names.includes("v"))?.value ?? "";
}

/** Whether the sed script `script` (GNU sed 4.9's grammar: commands joined by `;` or line ends, in
 *  `{ }` blocks, each after at most two addresses, a number, `$`, `first~step`, `/regex/` or
 *  `\cregexc` with its `I` and `M` flags, `+N` or `~N`, and `!`; `#` comments) runs no program and
 *  reads or writes no file: each command is one of `=`, `d`, `D`, `F`, `g`, `G`, `h`, `H`, `n`,
 *  `N`, `p`, `P`, `x`, `z`, `l`, `L`, `q`, `Q` (with a number), `:`, `b`, `t`, `T`, `v` (with a
 *  label), `a`, `i`, `c` (their text to the line's end, a `\` carrying it on), `y`, or `s` with no
 *  flag but `g`, `p`, `i`, `I`, `m`, `M` and a number; so no `e`, `r`, `R`, `w` or `W` command, and
 *  no `e` or `w` flag of `s`. A regex or a replacement ends at its delimiter, a `\` taking the
 *  character after it (GNU's match_slash); one a line end cuts is refused, as sed refuses it. */
function sedScriptReadsOnly(script: string): boolean {
  let at = 0;
  const peek = () => script[at];
  const blanks = () => {
    while (peek() === " " || peek() === "\t") at++;
  };
  const digits = () => {
    while (/[0-9]/.test(peek() ?? "")) at++;
  };
  // Past the next `delim` that no `\` takes.
  const through = (delim: string): boolean => {
    while (at < script.length) {
      const ch = script[at++];
      if (ch === "\n") return false;
      if (ch === "\\") at++;
      else if (ch === delim) return true;
    }
    return false;
  };
  // A delimiter after `s` or `y`, `\c` or `/` of an address.
  const delimiter = (): string | undefined => {
    const ch = script[at++];
    return ch === undefined || ch === "\n" || ch === "\\" ? undefined : ch;
  };
  const address = (second: boolean): "none" | "taken" | "refused" => {
    const ch = peek();
    if (ch !== undefined && /[0-9]/.test(ch)) {
      digits();
      if (peek() === "~") {
        at++;
        digits();
      }
      return "taken";
    }
    if (ch === "$") {
      at++;
      return "taken";
    }
    if (ch === "/" || ch === "\\") {
      at++;
      const delim = ch === "/" ? "/" : delimiter();
      if (delim === undefined || !through(delim)) return "refused";
      while (peek() === "I" || peek() === "M") at++;
      return "taken";
    }
    if (second && (ch === "+" || ch === "~")) {
      at++;
      if (!/[0-9]/.test(peek() ?? "")) return "refused";
      digits();
      return "taken";
    }
    return "none";
  };
  // The end of a command: blanks, then a `;` or a line end (taken), or a `}`, a `#` or the end.
  const ended = (): boolean => {
    blanks();
    const ch = peek();
    if (ch === ";" || ch === "\n") at++;
    return ch === undefined || ch === ";" || ch === "\n" || ch === "}" || ch === "#";
  };
  for (;;) {
    while (" \t\n;".includes(peek() ?? "x")) at++;
    if (peek() === undefined) return true;
    if (peek() === "#") {
      while (peek() !== undefined && peek() !== "\n") at++;
      continue;
    }
    const first = address(false);
    if (first === "refused") return false;
    blanks();
    if (first === "taken" && peek() === ",") {
      at++;
      blanks();
      if (address(true) !== "taken") return false;
    }
    blanks();
    while (peek() === "!") {
      at++;
      blanks();
    }
    const command = script[at++];
    switch (command) {
      case "{":
        continue;
      case "}":
      case "=":
      case "d":
      case "D":
      case "F":
      case "g":
      case "G":
      case "h":
      case "H":
      case "n":
      case "N":
      case "p":
      case "P":
      case "x":
      case "z":
        if (!ended()) return false;
        continue;
      case "l":
      case "L":
      case "q":
      case "Q":
        blanks();
        digits();
        if (!ended()) return false;
        continue;
      case ":":
      case "b":
      case "t":
      case "T":
      case "v":
        blanks();
        while (peek() !== undefined && !" \t\n;}".includes(peek() ?? "")) at++;
        if (!ended()) return false;
        continue;
      case "a":
      case "i":
      case "c":
        while (at < script.length && script[at] !== "\n") at += script[at] === "\\" ? 2 : 1;
        continue;
      case "s":
      case "y": {
        const delim = delimiter();
        if (delim === undefined || !through(delim) || !through(delim)) return false;
        if (command === "s") while (/[gpiImM0-9]/.test(peek() ?? "")) at++;
        if (!ended()) return false;
        continue;
      }
      default:
        return false;
    }
  }
}

/** Whether the sed command of `values` (the words after `sed`) only reads: `-n` (or `--quiet`,
 *  `--silent`), no `-i` in any spelling (`-i`, `-i.bak`, `-ni`, `--in-place`, `--in`, …), no `-f`
 *  or `--file` (a script the rule cannot read), and a script (its `-e` and `--expression` scripts
 *  joined by line ends, else its first operand) that `sedScriptReadsOnly` takes. */
function sedReadsOnly(values: readonly string[]): boolean {
  const parsed = parseOptions(values, SED_OPTIONS);
  if (parsed === undefined) return false;
  const { options, operands } = parsed;
  if (givesAny(options, ["i", "in-place", "f", "file"])) return false;
  if (!givesAny(options, ["n", "quiet", "silent"])) return false;
  const given = options.flatMap((option) =>
    option.names.includes("e") || option.names.includes("expression") ? [option.value ?? ""] : []
  );
  const script = given.length > 0 ? given.join("\n") : operands[0];
  return script !== undefined && sedScriptReadsOnly(script);
}

/** Whether the git command of `args` (the words after `git`) only reads (see the file's header). */
function gitReadsOnly(args: readonly string[]): boolean {
  let at = 0;
  for (; at < args.length; at++) {
    const arg = args[at] ?? "";
    if (arg === "-C") {
      at++;
      continue;
    }
    if (arg === "--no-pager" || arg === "-P") continue;
    break;
  }
  const subcommand = args[at];
  const rest = args.slice(at + 1);
  const parsed = parseOptions(rest, { values: GIT_VALUES[subcommand ?? ""] ?? "", long: GIT_LONG });
  // A format's `%G` (`%G?`, `%GS`, …) verifies a commit's signature, which runs gpg.
  if (parsed === undefined || givesAny(parsed.options, GIT_RUNS)) return false;
  if (rest.some((arg) => arg.includes("%G"))) return false;
  switch (subcommand) {
    case "worktree":
      return rest[0] === "list";
    case "remote": {
      const [verb] = rest.filter((arg) => arg !== "-v" && arg !== "--verbose");
      return verb === undefined || verb === "show" || verb === "get-url";
    }
    case "branch":
      return rest.every((arg) => GIT_BRANCH_LISTS.test(arg));
    default:
      return (READ_ONLY_GIT as readonly (string | undefined)[]).includes(subcommand);
  }
}

/** Whether the jj command of `args` (the words after `jj`) only reads (see the file's header). */
function jjReadsOnly(args: readonly string[]): boolean {
  if (!optionsRead("jj", args)) return false;
  const named: string[] = [];
  for (let at = 0; at < args.length; at++) {
    const arg = args[at] ?? "";
    if (JJ_VALUE_OPTIONS.has(arg)) at++;
    else if (!arg.startsWith("-")) named.push(arg);
  }
  const [first = "", second = ""] = named;
  const command = READ_ONLY_JJ.some((each) => each.startsWith(`${first} `))
    ? `${first} ${second}`
    : first;
  return (READ_ONLY_JJ as readonly string[]).includes(command);
}

/** Whether `program` (one of PROGRAM_OPTIONS) with the words `values` (undefined where unknown)
 *  gives none of its options that run a program, write a file or read one no word names. */
function optionsRead(program: string, values: readonly (string | undefined)[]): boolean {
  const options = PROGRAM_OPTIONS[program];
  if (options === undefined) return false;
  const parsed = parseOptions(values, options.spec);
  return parsed !== undefined && !givesAny(parsed.options, options.runs);
}

/** Whether `env`'s words `args` run no command: options of ENV_PRINTS (`-u`'s value a name), then
 *  assignments (after a first `-`, which is `-i`). */
function envReadsOnly(args: readonly Word[]): boolean {
  // An assignment is an operand whose value need not be known.
  const parsed = parseOptions(
    args.map((word) => (assignmentOf(word) === undefined ? literal(word) : "=")),
    ENV_OPTIONS
  );
  if (parsed === undefined) return false;
  for (const { names, value } of parsed.options) {
    if (names.length !== 1 || !ENV_PRINTS.includes(names[0] ?? "")) return false;
    if ((names[0] === "u" || names[0] === "unset") && !NAME.test(value ?? "")) return false;
  }
  return args
    .slice(args.length - parsed.operands.length)
    .every((word, n) => assignmentOf(word) !== undefined || (n === 0 && literal(word) === "-"));
}

/** Whether the read-only program `program` with `args` (each word, its one value, undefined where
 *  it has several or one unknown, and whether it is a glob word read as a file operand, `operand`)
 *  only reads (see the file's header). */
function commandReadsOnly(
  program: string,
  args: readonly { word: Word; value: string | undefined; operand: boolean }[]
): boolean {
  const values = args.map((arg) => arg.value);
  const known = (each: (string | undefined)[]): each is string[] =>
    each.every((value) => value !== undefined);
  const texts = args.map((arg) => literal(arg.word));
  const settable = (name: string | undefined) =>
    name !== undefined && NAME.test(name) && !PROTECTED_NAME.test(name);
  switch (program) {
    case "find":
      return known(values) && values.every((arg) => !Object.hasOwn(FIND_ACTIONS, arg));
    case "rg":
    case "tree":
    case "file":
      return known(values) && optionsRead(program, values);
    case "wc":
      return optionsRead(
        program,
        args.map((arg) => arg.value ?? (arg.operand ? "." : undefined))
      );
    case "git":
      return known(values) && gitReadsOnly(values);
    case "jj":
      return known(values) && jjReadsOnly(values);
    case "env":
      return envReadsOnly(args.map((arg) => arg.word));
    case "unset":
      return texts.every(
        (name) =>
          name === "-v" ||
          name === "-f" ||
          (name !== undefined && NAME.test(name) && !UNSET_PROTECTED.test(name))
      );
    case "export":
      return args.every(
        ({ word }) => literal(word) === "-p" || settable(assignmentOf(word)?.name ?? literal(word))
      );
    case "printf": {
      const name = printfTarget(values);
      return name === "" || settable(name);
    }
    case "sed":
      return known(values) && sedReadsOnly(values);
    default:
      return true;
  }
}

/** A walk that follows no command substitution's output. */
const READ_ONLY_WALK: Walk = {
  check: readOnlyFault,
  subValues: () => undefined,
};

/** Why the simple command `command` at `state` is not read-only (see the file's header), or
 *  undefined. */
function readOnlyFault(command: SimpleCommand, state: ShellState): string | undefined {
  // A word's one value, or undefined when it may take several or one unknown: a word whose
  // unquoted text holds a glob character takes the names of the files it matches.
  const values = (word: Word) =>
    globbed(word) ? undefined : oneValue(word, state, READ_ONLY_WALK);
  for (const part of commandWords(command).flat()) {
    if (part.kind === "var" && !state.vars.has(part.name) && !Object.hasOwn(SHELL_OWN, part.name)) {
      return `it expands $${part.name}, which it does not assign`;
    }
  }
  // bash makes of a word with a brace expansion several words, each of which may be an option or
  // a path no word shows as written (an assignment's value it does not expand so).
  for (const word of [...command.words, ...command.redirects.map((each) => each.target)]) {
    if (holdsBrace(bareText(word))) return `${wordText(word)} holds a brace expansion`;
  }
  for (const { name, value } of command.assignments) {
    if (PROTECTED_NAME.test(name)) return `it assigns ${name}`;
    if (globbed(value)) return `its assignment of ${name} holds a glob`;
  }
  for (const { op, target } of command.redirects) {
    const value = values(target);
    if (value === undefined || !harmlessRedirect({ op, target: value })) {
      return `it redirects to ${wordText(target)}`;
    }
  }
  const [first, ...rest] = command.words;
  if (first === undefined) return undefined;
  const program = literal(first);
  if (program === undefined || !(READ_ONLY_PROGRAMS as readonly string[]).includes(program)) {
    return `it runs ${wordText(first)}`;
  }
  if ((program === "printf" || program === "env" || program === "export") && rest.some(globbed)) {
    return `its ${program} command holds a glob, which may expand to an option or an assignment that a file's name makes`;
  }
  // A glob word whose every text starts with no `-` is a file operand, the names it matches
  // (which `escapes` reads in the copy) being no option either.
  const args = rest.map((word) => {
    const held = globbed(word) ? valuesOf(word, state, READ_ONLY_WALK) : undefined;
    const operand = held !== undefined && [...held].every((each) => !each.startsWith("-"));
    return { word, value: values(word), operand };
  });
  return commandReadsOnly(program, args) ? undefined : `its ${program} command may write`;
}

/** The parsed `bash` command `command` when it is read-only (see the file's header), else
 *  undefined. */
function readOnlyScript(command: unknown): Script | undefined {
  if (typeof command !== "string" || command.trim() === "") return undefined;
  const script = parseScript(command);
  if (script === undefined) return undefined;
  const fault = walkScript(
    script,
    { vars: new Map(), cwds: [], linked: false },
    false,
    READ_ONLY_WALK
  );
  return fault === undefined ? script : undefined;
}

/** The simple commands of `script`, those of its groups and command substitutions among them. */
function simpleCommands(script: Script): SimpleCommand[] {
  return script.flat(2).flatMap((command): SimpleCommand[] => {
    const subs = commandWords(command)
      .flat()
      .flatMap((part) => (part.kind === "sub" ? simpleCommands(part.script) : []));
    return [...(command.kind === "simple" ? [command] : simpleCommands(command.script)), ...subs];
  });
}

/** Whether the tool call `name` with `input` is read-only: the one rule of the replay's checkout
 *  rounds and the report's inspection floors (see the file's header). */
export function readOnlyCall(name: string | undefined, input: unknown): boolean {
  if ((READ_ONLY_TOOLS as readonly (string | undefined)[]).includes(name)) return true;
  if (name !== "bash" || typeof input !== "object" || input === null) return false;
  return readOnlyScript(Reflect.get(input, "command")) !== undefined;
}

// ---------------------------------------------------------------------------------------------
// Answers from a copy of the checkout.

/** A copy of a written case's checkout: `dir`, the copy on this machine; `root`, the checkout's
 *  directory on the session's machine (cases.ts's `checkout.root`); `cwd`, the session's working
 *  directory, inside `root`; `git`, whether the copy is a git checkout at the commit, whose history
 *  git commands read (else GitHub's tarball of the commit, which holds none); `jj`, where the
 *  context session ran jj (cli.ts), true when the copy was made a colocated jj repository
 *  (`colocateJj`), whose jj commands answer as in a jj workspace at the commit, else why it could
 *  not be, and no jj command is answered (absent, a jj command answers as jj does in a checkout
 *  that is no jj repository); `home`, the session's home directory (`sessionHome`), which a `~`
 *  or a `$HOME` of a call stands for and under which a read's header shows `~/…` (absent, such a
 *  call is not answered, and a header shows the absolute path); `madeAt`, when the copy was made
 *  (milliseconds since the epoch, from its marker, cli.ts's `sharedCheckout`), from which a
 *  directory listing measures each entry's age, so a listing reads the same in every arm and run
 *  (absent, no listing is answered); `answers`, the answers to its `read`, `grep` and `glob`
 *  calls, by tool, rows and input, each kept from when the call is first made (so the same call
 *  made again while its answer is still worked out waits for that one; an answer whose working out
 *  fails is dropped) (the copy is read-only once made, so each such answer depends on those alone;
 *  a `bash` answer is not kept, its wall time being its own run's), which one stage's runs share
 *  (cli.ts); absent, none is kept. */
export interface CheckoutCopy {
  readonly dir: string;
  readonly root: string;
  readonly cwd: string;
  readonly git: boolean;
  readonly jj?: true | Unanswered;
  readonly home?: string;
  readonly madeAt?: number;
  readonly answers?: Map<string, Promise<CheckoutAnswer | Unanswered>>;
}

/** The session's home directory, which a written case's checkout copy expands `~` and `$HOME` to
 *  (`CheckoutCopy.home`), from what the case records: `recorded`, the home its `pathCopy` names
 *  (build.ts's `sessionDirs`: the part of its session's transcript path before `/.omp/`, where
 *  Oh My Pi keeps its sessions under the home it resolves `~` against), else `/home/<user>` where
 *  its checkout's root `root` is under one (a written case records no `pathCopy`: build.ts writes
 *  none for it), else none. */
export function sessionHome(root: string, recorded: string | undefined): string | undefined {
  return recorded ?? /^\/home\/[^/]+(?=\/|$)/.exec(root)?.[0];
}

/** A call's answer: the text the tool gives, and whether it is an error. */
export interface CheckoutAnswer {
  readonly text: string;
  readonly isError: boolean;
}

/** Why a call is not answered. */
export interface Unanswered {
  readonly why: string;
}

/** Where an answer's sandbox (`runSandboxed`) mounts the copy, read-only: each command an answer
 *  runs sees the copy at this one path, which the answer maps to the session's checkout root and
 *  back (`sandboxedBash`). */
const CHECKOUT_MOUNT = "/mnt/checkout";
/** The sandbox's home directory: empty, on the sandbox's own read-only file system, so no file the
 *  repository holds (a `.gitconfig`, a `.jjconfig.toml`) is read as the user's configuration, even
 *  where the command unsets GIT_CONFIG_GLOBAL. */
const SANDBOX_HOME = "/mnt/home";
/** Where a sandbox shows the session's home directory (`CheckoutCopy.home`) when the copy does not
 *  hold it (`sandboxHome`): a directory of the sandbox's own read-only file system, empty but for
 *  the copy bound again at the checkout's root's place under it, which the command gets as its
 *  HOME and its output maps back to the session's home. */
const SESSION_HOME_MOUNT = "/mnt/session-home";
/** The files of a home directory git reads as the user's configuration where a command unsets
 *  GIT_CONFIG_GLOBAL (with XDG_CONFIG_HOME unset): no sandbox's HOME may show the copy's file at
 *  one of them (`sandboxHome`), which could name a program for git to run. */
const HOME_GIT_CONFIG = [".gitconfig", ".config/git/config"];
/** The sandbox's jj configuration (JJ_CONFIG, and `--config-file` on jj's command line, which
 *  outranks a repository's own): no pager, jj's default diff formatter, no signature shown by its
 *  templates, no file-system monitor, and no signing backend, each backend's program (jj 0.45's
 *  `signing.backends.gpg.program`, `.gpgsm.program`, `.ssh.program`) a path the sandbox does not
 *  hold: a template that verifies a signature (`signature.status()`) runs no gpg, gpgsm or
 *  ssh-keygen, whatever the sandbox's `/tmp`, so no jj command runs a program. */
const SANDBOX_JJ_CONFIG = "/mnt/jj.toml";
/** SANDBOX_JJ_CONFIG's text. */
const JJ_CONFIG_TEXT = `[ui]
paginate = "never"
diff-formatter = ":color-words"
show-cryptographic-signatures = false

[fsmonitor]
backend = "none"

[signing]
backend = "none"
backends.gpg.program = "/mnt/no-signing-program"
backends.gpgsm.program = "/mnt/no-signing-program"
backends.ssh.program = "/mnt/no-signing-program"
`;
/** git's configuration in each answer, through GIT_CONFIG_COUNT (which outranks every file of the
 *  repository's): no file-system monitor and no hooks, a bare repository used only where a command
 *  names it (so a directory of the checkout laid out as one, its `config` its own, is refused), no
 *  transport (so no lazy fetch of a blob from the commit's remote), no pager, and a stat check of
 *  the index's entries by mtime seconds, size and mode alone (`core.checkStat=minimal`,
 *  `core.trustctime=false`). The sandbox's user namespace shows each file of the copy as owned by
 *  uid and gid 0 where the index recorded the runner's, and the copy's ctime need not be the
 *  index's, so git's default check would call every file changed and compare its content again,
 *  on each call, since GIT_OPTIONAL_LOCKS=0 writes no refreshed index (a `git status` of a
 *  checkout of 115,965 files ran past BASH_TIME_LIMIT_MS so); the read-only bind keeps what the
 *  minimal check reads, and git compares the content where it differs, so no answer's text
 *  changes. */
const GIT_CONFIG: readonly (readonly [string, string])[] = [
  ["core.fsmonitor", "false"],
  ["core.hooksPath", "/dev/null"],
  ["safe.bareRepository", "explicit"],
  ["protocol.allow", "never"],
  ["core.pager", "cat"],
  ["core.checkStat", "minimal"],
  ["core.trustctime", "false"],
];
/** The environment an answer runs in, and the copy's colocation (`colocateJj`), the same for each:
 *  none of the runner's variables; its PATH the sandbox's jj wrapper (`/mnt/bin`, `runSandboxed`),
 *  then the system's; its home SANDBOX_HOME; a fixed LANG; git's system and global configuration
 *  none, GIT_CONFIG, its pagers `cat`, and GIT_OPTIONAL_LOCKS=0, so `git status` writes no index;
 *  jj's configuration SANDBOX_JJ_CONFIG. */
const SANDBOX_ENV: Readonly<Record<string, string>> = {
  PATH: "/mnt/bin:/usr/local/bin:/usr/bin:/bin",
  HOME: SANDBOX_HOME,
  LANG: "C.UTF-8",
  TERM: "dumb",
  GIT_CONFIG_NOSYSTEM: "1",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_PAGER: "cat",
  PAGER: "cat",
  GIT_OPTIONAL_LOCKS: "0",
  GIT_CONFIG_COUNT: String(GIT_CONFIG.length),
  ...Object.fromEntries(
    GIT_CONFIG.flatMap(([key, value], n) => [
      [`GIT_CONFIG_KEY_${n}`, key],
      [`GIT_CONFIG_VALUE_${n}`, value],
    ])
  ),
  JJ_CONFIG: SANDBOX_JJ_CONFIG,
};

/** The trees no answer sees, each under an empty, read-only file system: the pod's run-time
 *  mounts (`/run`, `/var/run`: the model gateway's key, the secrets broker's keys, gh's and the
 *  owners' GitHub configuration, the Dispatch token, the store's address), the home directories,
 *  the issue volume (`/legion`), the temporary directories, shared memory, the operator's
 *  configuration, and the pod's installed tools (`/opt`, which no read-only program needs, the
 *  runner's jj bound at /mnt/jj before it is hidden, and which holds files any user may write). */
const HIDDEN_TREES = [
  "/run",
  "/var/run",
  "/home",
  "/root",
  "/legion",
  "/tmp",
  "/var/tmp",
  "/dev/shm",
  "/etc/legion-operator",
  "/opt",
];
/** A variable of the runner's environment whose name says it names a file (`…_FILE`: the Dispatch
 *  and Envoy tokens, Envoy's configuration, the store's address, the private names' rules) or a
 *  directory (`…_DIR`, `…_HOME`: gh's configuration, the secrets broker's keys, the owners'
 *  tokens): each such directory, a file's own, is hidden too, wherever it is. */
const NAMES_A_PATH = /_(FILE|DIR|HOME)$/;
/** The paths the sandbox runs from: no directory that is one of them or holds one is hidden; a file
 *  a variable names in such a directory is hidden alone (where the kernel cannot mount over a file,
 *  as gVisor's cannot, the sandbox is then not made, and no command runs). */
const SANDBOX_NEEDS = [
  "/bin",
  "/sbin",
  "/lib",
  "/lib64",
  "/usr/bin",
  "/usr/sbin",
  "/usr/lib",
  "/usr/local/bin",
  "/etc",
  "/dev",
  "/proc",
  "/mnt",
];

/** What an answer's sandbox hides, from the runner's environment `env`: HIDDEN_TREES, the runner's
 *  home directory (HOME), and each directory or file a variable of NAMES_A_PATH names (none under
 *  /mnt, all of which the sandbox's own /mnt hides), each as its real path, those not there left
 *  out (nothing is there to hide), and none under another (whose hiding hides it), in path order
 *  (`paths`); and the runner's home as its real path (`home`), made again, empty, inside the one of
 *  `paths` that holds it (`homeTree`). */
function hiddenPaths(env: Readonly<Record<string, string | undefined>>): {
  readonly paths: string[];
  readonly home: string;
  readonly homeTree: string;
} {
  const real = (target: string): string | undefined => {
    try {
      return realpathSync(target);
    } catch {
      return undefined;
    }
  };
  const found = new Set<string>();
  const named = Object.entries(env).filter(([name]) => NAMES_A_PATH.test(name) || name === "HOME");
  for (const [name, value] of [...HIDDEN_TREES.map((tree) => ["", tree] as const), ...named]) {
    if (value === undefined || !path.isAbsolute(value)) continue;
    const file = name.endsWith("_FILE");
    const dir = real(file ? path.dirname(value) : value);
    if (dir === undefined || insideRoot("/mnt", dir) !== undefined) continue;
    if (dir !== "/" && !SANDBOX_NEEDS.some((need) => insideRoot(dir, need) !== undefined)) {
      found.add(dir);
    } else if (file) found.add(path.join(dir, path.basename(value)));
  }
  const sorted = [...found].sort();
  const paths = sorted.filter(
    (each, n) => !sorted.slice(0, n).some((other) => insideRoot(other, each) !== undefined)
  );
  const home = env.HOME === undefined || !path.isAbsolute(env.HOME) ? "" : (real(env.HOME) ?? "");
  const homeTree =
    home === "" ? "" : (paths.find((each) => insideRoot(each, home) !== undefined) ?? "");
  return { paths, home, homeTree };
}

/** `target` as a mount table names it: a blank, a tab, a line end and a backslash in octal. */
function mountName(target: string): string {
  return target.replace(/[\\ \t\n]/g, (ch) => `\\${ch.charCodeAt(0).toString(8).padStart(3, "0")}`);
}

/** The sandbox's making, in bash, run as the root of its own user namespace (`runSandboxed`): its
 *  own file system at /mnt; then, in one `mount -a` of a mount table of its own, the copy bound at
 *  CHECKOUT_MOUNT (read-only unless `rw`) and, where the command gets the session's home as
 *  SESSION_HOME_MOUNT (`sandboxHome`), bound again, read-only, at the checkout's root's place
 *  under it (`homebind`, its name in the table `homebindname`; the directories to it, or
 *  SESSION_HOME_MOUNT alone, `homedir`, made first), the directory of the runner's jj bound
 *  read-only at /mnt/jj, and an empty, read-only file system over each hidden directory and
 *  /dev/null over each hidden file (`hiddenPaths`, each with its name in the table; one the
 *  sandbox's user can neither enter nor read, as `/root`, needs none, since no command of it can
 *  either); the runner's home made again, empty, in the one that holds it, made read-only after;
 *  the jj wrapper at /mnt/bin/jj, SANDBOX_HOME and SANDBOX_JJ_CONFIG, then /mnt made read-only; a
 *  host name of its own; the working directory in the copy entered; each of the programs it is
 *  given (`programs`, blank-separated) looked for on PATH, the first it finds none of named on
 *  standard error (SANDBOX_LACKS) and no command run; then SANDBOX_READY, and the command, its
 *  standard error joined to its output, its HOME `sessionhome` where one is given, with no
 *  capability (setpriv), so it can neither unmount what hides a directory nor mount anything, nor
 *  gain one. A step that fails says which on standard error, and no command runs. */
const SANDBOX_SETUP = `copyfs=$1 mode=$2 cwd=$3 jjfs=$4 wrapper=$5 jjconfig=$6 home=$7 hometree=$8 command=$9 programs=\${10} sessionhome=\${11} homedir=\${12} homebind=\${13} homebindname=\${14}
shift 14
fail() { printf '%s\\n' "$1" >&2; exit 125; }
command -v setpriv >/dev/null || fail "find setpriv"
mount -t tmpfs -o mode=0755 checkout-sandbox /mnt || fail "mount a file system of its own at /mnt"
mkdir ${CHECKOUT_MOUNT} ${SANDBOX_HOME} /mnt/bin /mnt/jj || fail "make its directories"
if [ -n "$homedir" ]; then mkdir -p "$homedir" || fail "make the session's home"; fi
homemounted=
{
  if [ "$mode" = ro ]; then
    printf '%s ${CHECKOUT_MOUNT} none bind,ro 0 0\\n' "$copyfs"
  else
    printf '%s ${CHECKOUT_MOUNT} none bind 0 0\\n' "$copyfs"
  fi
  if [ -n "$homebind" ]; then printf '%s %s none bind,ro 0 0\\n' "$copyfs" "$homebindname"; fi
  if [ -n "$jjfs" ]; then printf '%s /mnt/jj none bind,ro 0 0\\n' "$jjfs"; fi
  while [ $# -gt 1 ]; do
    each=$1 target=$2
    shift 2
    if [ -d "$each" ]; then
      [ -x "$each" ] || continue
      if [ "$each" = "$hometree" ]; then
        homemounted=1
        printf 'checkout-sandbox-hidden %s tmpfs mode=0755 0 0\\n' "$target"
      else
        printf 'checkout-sandbox-hidden %s tmpfs ro,mode=0755 0 0\\n' "$target"
      fi
    elif [ -r "$each" ]; then
      printf '/dev/null %s none bind,ro 0 0\\n' "$target"
    fi
  done
} > /mnt/fstab || fail "write its mount table"
mount -a -T /mnt/fstab || fail "mount the copy and hide the directories that hold the pod's credentials"
: > /mnt/fstab
if [ -n "$homemounted" ]; then
  { mkdir -p "$home" && mount -o remount,ro "$hometree"; } || fail "make the runner's home again, empty"
fi
if [ -n "$jjfs" ]; then
  { printf '%s' "$wrapper" > /mnt/bin/jj && chmod 0755 /mnt/bin/jj; } || fail "write jj's wrapper"
fi
printf '%s' "$jjconfig" > ${SANDBOX_JJ_CONFIG} || fail "write jj's configuration"
mount -o remount,ro /mnt || fail "make its /mnt read-only"
hostname checkout 2>/dev/null
cd -P "${CHECKOUT_MOUNT}/$cwd" || fail "enter the working directory"
for program in $programs; do
  type -P "$program" >/dev/null || { printf 'lacks %s\\n' "$program" >&2; exit 125; }
done
unset PWD OLDPWD
if [ -n "$sessionhome" ]; then export HOME="$sessionhome"; fi
printf 'ready\\n' >&2
exec env -u SHLVL -u _ setpriv --bounding-set=-all --inh-caps=-all --no-new-privs -- bash --noprofile --norc -c "{ $command
} 2>&1" 2>/dev/null
`;

/** What SANDBOX_SETUP says, alone, on standard error once the sandbox is made, as its command
 *  starts: the command's wall time starts there. */
const SANDBOX_READY = "ready\n";
/** What SANDBOX_SETUP says, alone, on standard error where a program it is given is on no
 *  directory of the sandbox's PATH: the program's name follows. */
const SANDBOX_LACKS = /^lacks (.+)\n$/;

/** What a command run in the sandbox gave: its output, its exit code and its wall time. */
interface SandboxRun {
  readonly output: Buffer;
  readonly exitCode: number;
  readonly wallTimeMs: number;
}

/** The child a sandbox runs as, as `runSandboxed` reads it: `Bun.spawn`'s, or a stand-in's
 *  (`setSandboxSpawnForTests`). */
interface SandboxChild {
  readonly stdout: ReadableStream<Uint8Array>;
  readonly stderr: ReadableStream<Uint8Array>;
  readonly exited: Promise<number>;
  kill(signal: "SIGKILL"): void;
}

/** Starts the sandbox's child from `argv`, in SANDBOX_ENV, with no input and its output and error
 *  piped. */
type SandboxSpawn = (argv: string[]) => SandboxChild;

const spawnWithBun: SandboxSpawn = (argv) =>
  Bun.spawn(argv, { env: SANDBOX_ENV, stdin: "ignore", stdout: "pipe", stderr: "pipe" });
let spawnSandbox: SandboxSpawn = spawnWithBun;
/** The clock a command's wall time is read from (`runChild`), in milliseconds. */
const performanceClock = () => performance.now();
let sandboxClock: () => number = performanceClock;

/** Starts each sandbox's child with `spawn` in place of `Bun.spawn`, so a row of checkout.test.ts
 *  drives the queue and the limits with a child whose output and end it controls, and reads a
 *  command's wall time from `clock` where one is given (in place of `performance.now`), so a row
 *  sets it without waiting. Production callers never call this. */
export function setSandboxSpawnForTests(spawn: SandboxSpawn, clock?: () => number): void {
  spawnSandbox = spawn;
  if (clock !== undefined) sandboxClock = clock;
}

/** Puts `Bun.spawn` and `performance.now` back, as the file that set a stand-in must when it
 *  ends. */
export function resetSandboxSpawnForTests(): void {
  spawnSandbox = spawnWithBun;
  sandboxClock = performanceClock;
}

/** The one queue of a runner process's sandboxes (`runSandboxed`): each runs alone, in the order it
 *  asked (common.ts's `Limiter`; see the file's header, "The queue"). A sandbox spawns no child and
 *  sets no timer until it holds the place, and keeps it until its child has exited, so no limit
 *  counts the wait and the process's own sandboxes never compete with each other for CPU. */
const checkoutQueue = new Limiter(1);

/**
 * Runs `command` (bash's text, the copy at CHECKOUT_MOUNT in it) in a sandbox of the copy `dir`, as
 * each command that answers a call, and each jj command of the copy's colocation, runs: `unshare`'s
 * new user (the runner's user its root), mount, network, process and host-name namespaces, made by
 * SANDBOX_SETUP (the copy alone of the machine's disk mounted, at CHECKOUT_MOUNT, read-only unless
 * `writable`; an empty, read-only file system over each tree that holds the pod's credentials,
 * `hiddenPaths`; no network; no capability), from `cwd` (the working directory's path in the copy,
 * "" for its root), in SANDBOX_ENV, its jj the runner's (found on its PATH) through a wrapper that
 * gives it SANDBOX_JJ_CONFIG and, unless `writable`, `--ignore-working-copy` (so no jj answer takes
 * the working copy's lock or snapshots it: each answers from the snapshot the colocation took).
 * Not run where one of `programs` is on no directory of the sandbox's PATH. It waits first for its
 * place in the queue (`checkoutQueue`): until it has one, no child is spawned and no timer set.
 * Ended, with every process it started (SIGKILL; `--kill-child` ends the namespace's first
 * process, and the kernel the rest), once `limitMs` pass from the spawn without the sandbox's
 * ready marker (SANDBOX_READY), once `limitMs` pass from that marker, and once its output passes
 * OUTPUT_BYTES. Its output (standard error joined to it), exit code and wall time (from when the
 * sandbox is made and its command starts, at that marker); else why none: no `unshare`, a sandbox
 * not made, a program it lacks, the time limit or the output cap. With `home` (`sandboxHome`),
 * the command's HOME is `home.env`, the directory `home.dir` is made first, and the copy is bound
 * again at `home.bind`, where those are given.
 */
async function runSandboxed(
  dir: string,
  {
    command,
    cwd,
    writable,
    limitMs,
    programs,
    home,
  }: {
    command: string;
    cwd: string;
    writable: boolean;
    limitMs: number;
    programs: readonly string[];
    home?: SandboxHomeView;
  }
): Promise<SandboxRun | Unanswered> {
  const unshare = Bun.which("unshare");
  if (unshare === null) return { why: "the runner has no unshare, so it runs no command" };
  const jjFound = Bun.which("jj");
  const jj = jjFound === null ? undefined : realpathSync(jjFound);
  const wrapper =
    jj === undefined
      ? ""
      : `#!/bin/sh\nexec /mnt/jj/${path.basename(jj)}${writable ? "" : " --ignore-working-copy"} --config-file ${SANDBOX_JJ_CONFIG} "$@"\n`;
  const hidden = hiddenPaths(process.env);
  const argv = [
    unshare,
    "--user",
    "--map-root-user",
    "--mount",
    "--net",
    "--pid",
    "--fork",
    "--uts",
    "--mount-proc",
    "--kill-child",
    "--",
    "bash",
    "--noprofile",
    "--norc",
    "-c",
    SANDBOX_SETUP,
    "sandbox",
    mountName(realpathSync(dir)),
    writable ? "rw" : "ro",
    cwd,
    jj === undefined ? "" : mountName(path.dirname(jj)),
    wrapper,
    JJ_CONFIG_TEXT,
    hidden.home,
    hidden.homeTree,
    command,
    programs.join(" "),
    home?.env ?? "",
    home?.dir ?? "",
    home?.bind ?? "",
    home === undefined || home.bind === "" ? "" : mountName(home.bind),
    ...hidden.paths.flatMap((each) => [each, mountName(each)]),
  ];
  return checkoutQueue.run(() => runChild(argv, limitMs));
}

/** The sandbox `runSandboxed` made `argv` for, spawned and bounded once it holds its place in the
 *  queue: the limit of its setup starts at the spawn, and the command's at SANDBOX_READY. */
async function runChild(argv: string[], limitMs: number): Promise<SandboxRun | Unanswered> {
  const child = spawnSandbox(argv);
  const limit = timeLimit({ kill: () => child.kill("SIGKILL") }, limitMs);
  const chunks: Uint8Array[] = [];
  let bytes = 0;
  let over = false;
  let said = "";
  let ready: number | undefined;
  const decoder = new TextDecoder();
  await Promise.all([
    eachChunk(child.stderr, limit, (chunk) => {
      said += decoder.decode(chunk, { stream: true });
      if (ready === undefined && said.endsWith(SANDBOX_READY)) {
        ready = sandboxClock();
        // The command's limit starts here: the setup had the same limit from the spawn.
        if (!limit.timedOut()) limit.touch();
      }
    }),
    eachChunk(child.stdout, limit, (chunk) => {
      if (over) return;
      bytes += chunk.length;
      if (bytes > OUTPUT_BYTES) {
        over = true;
        child.kill("SIGKILL");
        return;
      }
      chunks.push(chunk);
    }),
  ]);
  const exitCode = await child.exited;
  const ended = sandboxClock();
  limit.clear();
  if (limit.timedOut()) return { why: `it ran past the ${limitMs / 1000} s limit` };
  if (over) return { why: `its output passes the ${OUTPUT_BYTES} bytes Oh My Pi shows whole` };
  const lacks = SANDBOX_LACKS.exec(said);
  if (lacks !== null) {
    return {
      why: `the sandbox has no ${lacks[1]} on its PATH, so bash would answer that it found no such command, which is not what the session saw`,
    };
  }
  if (ready === undefined || said !== SANDBOX_READY) {
    return {
      why: `the sandbox could not be made: ${firstLine(said.replace(SANDBOX_READY, "")) || "it started no command"}`,
    };
  }
  return { output: Buffer.concat(chunks), exitCode, wallTimeMs: ended - ready };
}

/** How a sandbox shows the session's home (`sandboxHome`): `env`, the HOME its command runs with;
 *  `dir`, a directory made for it ("" for none); `bind`, where the copy is bound again ("" for
 *  none). */
interface SandboxHomeView {
  readonly env: string;
  readonly dir: string;
  readonly bind: string;
}

/** What a sandbox of `copy` shows as the session's home (`CheckoutCopy.home`), the HOME its
 *  commands run with, so `~` and `$HOME` lead where the session's did: where the home is the
 *  checkout's root or inside it, its place in the copy at CHECKOUT_MOUNT; else SESSION_HOME_MOUNT,
 *  the copy bound again at the root's place under it where the root is inside the home (output
 *  maps SESSION_HOME_MOUNT back to the home). Undefined where the copy names no home (the command
 *  then runs with SANDBOX_HOME). Why not where that home would show a file of the copy at one of
 *  HOME_GIT_CONFIG, which git reads as the user's configuration where a command unsets
 *  GIT_CONFIG_GLOBAL: such a copy's commands run with SANDBOX_HOME, and one that expands `~` or
 *  `$HOME` is not answered (`escapes`). */
function sandboxHome(copy: CheckoutCopy): SandboxHomeView | Unanswered | undefined {
  const home = copy.home;
  if (home === undefined) return undefined;
  const held = insideRoot(copy.root, home);
  const under = insideRoot(home, copy.root);
  const config = HOME_GIT_CONFIG.find((file) => {
    if (held !== undefined) return existsSync(path.join(copy.dir, held, file));
    if (under === undefined || (file !== under && !file.startsWith(`${under}/`))) return false;
    return existsSync(path.join(copy.dir, file.slice(under.length + 1)));
  });
  if (config !== undefined) {
    return {
      why: `the session's home ${home} holds the checkout's own ${home}/${config}, git's configuration of the user, which may name a program for git to run, so no sandboxed command gets that home, and none that expands ~ or $HOME is answered`,
    };
  }
  if (held !== undefined) {
    return { env: held === "" ? CHECKOUT_MOUNT : `${CHECKOUT_MOUNT}/${held}`, dir: "", bind: "" };
  }
  if (under === undefined) return { env: SESSION_HOME_MOUNT, dir: SESSION_HOME_MOUNT, bind: "" };
  const bind = `${SESSION_HOME_MOUNT}/${under}`;
  return { env: SESSION_HOME_MOUNT, dir: bind, bind };
}

/** Runs the `bash` command `command` as each answered one runs (`runSandboxed`), from the session's
 *  working directory `cwd`: the session's checkout root in it mapped to CHECKOUT_MOUNT, its HOME
 *  the session's home as the sandbox shows it (`sandboxHome`; SANDBOX_HOME where the copy names
 *  none, or where that home may not be shown), and in its output that path, the copy's own (as
 *  given and as its real path) and SESSION_HOME_MOUNT mapped back to the root and the home; not
 *  run where one of `programs` (those the command runs that are no builtin of bash's) is on no
 *  directory of the sandbox's PATH. Its output, exit code and wall time, or why none. Exported for
 *  checkout.test.ts's negative control, which runs through it commands that the read-only rule
 *  refuses. */
export async function sandboxedBash(
  copy: CheckoutCopy,
  command: string,
  cwd = copy.cwd,
  programs: readonly string[] = []
): Promise<{ output: string; exitCode: number; wallTimeMs: number } | Unanswered> {
  const view = sandboxHome(copy);
  const home = view === undefined || "why" in view ? undefined : view;
  const ran = await runSandboxed(copy.dir, {
    command: command.split(copy.root).join(CHECKOUT_MOUNT),
    cwd: insideRoot(copy.root, cwd) ?? "",
    writable: false,
    limitMs: BASH_TIME_LIMIT_MS,
    programs,
    home,
  });
  if ("why" in ran) return ran;
  const real = realpathSync(copy.dir);
  const text = ran.output.toString("utf8");
  const output = [CHECKOUT_MOUNT, real, copy.dir].reduce(
    (each, own) => each.split(own).join(copy.root),
    home?.env === SESSION_HOME_MOUNT && copy.home !== undefined
      ? text.split(SESSION_HOME_MOUNT).join(copy.home)
      : text
  );
  return { output, exitCode: ran.exitCode, wallTimeMs: ran.wallTimeMs };
}

/** A word `jj` of a shell command, where a program's name stands. */
const JJ_WORD = /(^|[\s;&|(`])jj(?=$|[\s;&|)`])/;

/** Whether `messages`, a session's messages, hold a `bash` call that runs jj (a word `jj` of its
 *  command, JJ_WORD): that session's checkout was a jj workspace. build.ts records it on a written
 *  case, from its context session's whole transcript (cases.ts's `checkout.jj`); cli.ts reads a
 *  written case's own messages for it where the case has no such record. */
export function messagesRunJj(messages: Case["messages"]): boolean {
  return messages.some((message) =>
    (message.content ?? []).some((block) => {
      const input =
        block.type === "toolCall" && block.name === "bash" ? block.arguments : undefined;
      const command =
        typeof input === "object" && input !== null ? Reflect.get(input, "command") : "";
      return typeof command === "string" && JJ_WORD.test(command);
    })
  );
}

/** How `head` and `tail` read their options (GNU coreutils 9): `-n` and `-c` take a count. */
const HEAD_TAIL_OPTIONS: OptionSpec = {
  values: "nc",
  long: { lines: "required", bytes: "required" },
};

/** The files the `bash` command `command`, run from the session's directory `cwd`, reads with
 *  `cat`, `head`, `tail` or `sed -n`: each operand of such a command (its options read as
 *  `parseOptions` reads them; a `sed` script given as its first operand is none), as the absolute
 *  path it names from each directory the shell may be in there (a `cd` of one word, a literal and
 *  no option, where it surely runs, taken as done), each once, in order. None for a command the
 *  read-only rule's parser does not take, nor for an operand whose value the command does not show.
 *  build.ts's written build reads a context session's `bash` calls with it. */
export function bashFileReads(command: string, cwd: string): string[] {
  const script = parseScript(command);
  if (script === undefined) return [];
  const read = new Set<string>();
  const walk: Walk = {
    check: (simple, state) => {
      const [first, ...args] = simple.words;
      const program = first === undefined ? undefined : literal(first);
      const values = args.map((word) => oneValue(word, state, walk));
      let files: (string | undefined)[] = [];
      if (program === "cat") files = parseOptions(values, { values: "", long: {} })?.operands ?? [];
      else if (program === "head" || program === "tail") {
        files = parseOptions(values, HEAD_TAIL_OPTIONS)?.operands ?? [];
      } else if (program === "sed") {
        const parsed = parseOptions(values, SED_OPTIONS);
        if (parsed !== undefined && givesAny(parsed.options, ["n", "quiet", "silent"])) {
          const scripted = givesAny(parsed.options, ["e", "expression"]);
          files = scripted ? parsed.operands : parsed.operands.slice(1);
        }
      }
      for (const file of files) {
        if (file === undefined || file === "-") continue;
        for (const at of state.cwds) read.add(path.posix.resolve(at, file));
      }
      return undefined;
    },
    subValues: () => undefined,
    cd: () => ({ enters: true, linked: false }),
  };
  walkScript(script, { vars: new Map(), cwds: [cwd], linked: false }, false, walk);
  return [...read];
}

/** The longest one jj command of a copy's colocation may run: its snapshot reads every file of the
 *  checkout, as cli.ts's git checkout writes them (30 minutes there too). */
const JJ_TIME_LIMIT_MS = 1_800_000;

/** Makes the copy `dir` (a git checkout at its commit, its outside links removed) a colocated jj
 *  repository, as cli.ts makes the shared checkout of a session that ran jj: `jj git init
 *  --colocate`, then `jj status`, whose snapshot of the working copy (each link removed seen as
 *  deleted) is taken here, once, so that each answer's jj command (`--ignore-working-copy`) reads
 *  that snapshot and writes nothing. Each runs in the answers' sandbox (`runSandboxed`), the copy
 *  writable there, with the runner's jj. True when made; else why not, with the `.jj` it made
 *  removed. */
export async function colocateJj(dir: string): Promise<true | Unanswered> {
  if (Bun.which("jj") === null) return { why: "jj is not on the runner's PATH" };
  for (const command of ["jj git init --colocate", "jj status"]) {
    const ran = await runSandboxed(dir, {
      command,
      cwd: "",
      writable: true,
      limitMs: JJ_TIME_LIMIT_MS,
      programs: [],
    });
    if ("why" in ran || ran.exitCode !== 0) {
      rmSync(path.join(dir, ".jj"), { recursive: true, force: true });
      const said =
        "why" in ran
          ? ran.why
          : firstLine(ran.output.toString("utf8")).split(CHECKOUT_MOUNT).join(".");
      return { why: `${command} failed: ${said}` };
    }
  }
  return true;
}

/** The longest a `bash` call's command may run in the copy before it is ended, unanswered, from
 *  when its sandbox is made (and the longest its sandbox's setup may take, from its spawn): the
 *  wait for its place in the queue counts in neither. */
const BASH_TIME_LIMIT_MS = 30_000;
/** The bytes of an output and of a line a bash or grep output may hold to be answered whole: Oh My
 *  Pi elides an output past its spill threshold (`tools.artifactSpillThreshold`, 50 KB; grep's
 *  DEFAULT_MAX_BYTES, the same) and cuts a line past `tools.outputMaxColumns` (768 bytes, the
 *  sink's cap, streaming-output.ts lines 1153-1215), grep's lines past DEFAULT_MAX_COLUMN (512). */
const OUTPUT_BYTES = 50 * 1024;
const BASH_COLUMNS = 768;
const GREP_COLUMNS = 512;
/** Oh My Pi's minimizer passes an output shorter than this many characters through as it is
 *  (engine.rs lines 16-20). */
const MINIMIZE_CHARS = 1_000;
/** The read-only programs whose output Oh My Pi's minimizer may rewrite (filters/mod.rs lines
 *  42-60, and defs/jj.toml's pipeline for jj). */
const MINIMIZED_PROGRAMS: Readonly<Record<string, true>> = {
  ls: true,
  tree: true,
  find: true,
  grep: true,
  rg: true,
  wc: true,
  cat: true,
  stat: true,
  git: true,
  jj: true,
};
/** GrepTool's caps at the pin (grep.ts lines 83-96): files one page shows, matches kept in a file
 *  of several and of one, the matches it fetches from its native search of one root
 *  (INTERNAL_TOTAL_CAP), and its context lines. */
const GREP_FILE_LIMIT = 20;
const GREP_MULTI_FILE_MATCHES = 20;
const GREP_SINGLE_FILE_MATCHES = 200;
const GREP_TOTAL_MATCHES = 2_000;
const GREP_CONTEXT_BEFORE = 1;
const GREP_CONTEXT_AFTER = 3;
/** The native search's windows (crates/pi-walker/src/grep/mod.rs line 51, scan.rs lines 24-28): a
 *  file is searched over its first 4 MiB, and a NUL byte in its first 64 KiB makes it binary. */
const GREP_FILE_BYTES = 4 * 1024 * 1024;
const GREP_BINARY_WINDOW = 64 * 1024;
/** How long one test of a grep call's regular expression, or of its exclusions', may take.
 *  JavaScriptCore stops a backtracking match after a count of its steps (`(a+)+b` over 40 a's, then
 *  `xab`: about 0.7 s on this pod) and gives no match, where the pin's search (Rust's regex, which
 *  does not backtrack) finds one; a test past this may have stopped so: one of a line, or of a
 *  path by the exclusions, leaves the call not answered, so that the run is made again, never
 *  judged on that answer, and one of a file's whole text has its lines tested (`grepScan`). */
const REGEX_TEST_MS = 100;
/** GlobTool's limit when the call gives none, and the largest it takes (glob.ts lines 46-47). */
const GLOB_LIMIT = 200;
/** The read tool's directory listing (read.ts lines 3289-3358): its depth below the directory, and
 *  the entries a directory under it shows (the directory's own are not capped). */
const LISTING_DEPTH = 2;
const LISTING_CHILD_LIMIT = 12;

/** Why `input` names an option the copy does not answer, or undefined. */
function otherKeys(input: Record<string, unknown>, keys: readonly string[]): string | undefined {
  const other = Object.keys(input).filter((key) => key !== "i" && !keys.includes(key));
  return other.length === 0
    ? undefined
    : `its options ${other.join(", ")} are none the copy answers`;
}

/** Whether `value`, an optional field of a tool call's input, is one Oh My Pi's argument
 *  validation drops before the tool reads it: `null` or the text `null` (pi-ai's
 *  utils/validation.ts lines 950-970, and lines 1006-1022 for an unknown key of a nested object),
 *  or absent. */
function dropped(value: unknown): boolean {
  return value === undefined || value === null || value === "null";
}

/**
 * Why the `bash` call's `input` names an option the copy does not answer, or undefined (see the
 * file's header). At its default settings the pin's bash tool takes `command`, `timeout`, `cwd`,
 * `pty`, `async`, `name` and `ready` (bash.ts lines 441-454; `launch.enabled` and `async.enabled`
 * are on, tools/settings.ts lines 588-591 and 964-969), and reads each of the last four as absent
 * where it asks for nothing (bash.ts lines 472-498 and 1291-1311): one validation drops (`null`,
 * `"null"`; `""` too for `pty`, `async` and `ready`, whose schema takes no text), `pty` and
 * `async` false (only `true` is a terminal or a background job: lines 726, 1298, 1340, 1471-1479
 * and 1860-1863), a blank `name` (`blankToUndefined`), and a `ready` with no log and no port
 * (`normalizeReady`; a host or a timeout alone asks for no condition and adds no notice). Not
 * answered: `pty: true` and `async: true`, a service's `name`, a `ready` with a log or a port
 * (the pin adds `Ignored ready: service-only, and no service name was given.`), and any other
 * value, whose reading by validation the runner does not rebuild.
 */
function bashOptionFault(input: Record<string, unknown>): string | undefined {
  const other = otherKeys(input, ["command", "cwd", "timeout", "pty", "async", "name", "ready"]);
  if (other !== undefined) return other;
  const unread = (key: string, value: unknown) =>
    `its ${key} is ${JSON.stringify(value)}, a value whose reading by Oh My Pi's validation the runner does not rebuild`;
  for (const key of ["pty", "async"]) {
    const value = input[key];
    if (dropped(value) || value === "" || value === false) continue;
    if (value !== true) return unread(key, value);
    return key === "pty"
      ? "its pty: true runs the command on a terminal, which the copy does not answer"
      : "its async: true runs the command as a background job, whose answer names the job, which the copy does not answer";
  }
  const name = input.name;
  if (!dropped(name) && !(typeof name === "string" && name.trim() === "")) {
    return typeof name === "string"
      ? `its name ${JSON.stringify(name)} starts a service, which the copy does not answer`
      : unread("name", name);
  }
  const ready = input.ready;
  if (dropped(ready) || ready === "") return undefined;
  if (typeof ready !== "object" || Array.isArray(ready)) return unread("ready", ready);
  for (const [key, value] of Object.entries(ready as Record<string, unknown>)) {
    if (dropped(value)) continue;
    const kind =
      key === "log" || key === "host"
        ? "string"
        : key === "port" || key === "timeout"
          ? "number"
          : undefined;
    if (kind === undefined || typeof value !== kind) return unread(`ready.${key}`, value);
    if ((key === "log" && String(value).trim() !== "") || (key === "port" && value !== 0)) {
      return "its ready gives a log or a port and no service name, so Oh My Pi adds the notice Ignored ready: service-only, and no service name was given., which the runner does not rebuild";
    }
  }
  return undefined;
}

/** The path Oh My Pi's search tools show for the session's absolute path `full`: relative to the
 *  working directory when under it (`.` for it), else absolute (path-utils.ts lines 512-531). */
function relativeToCwd(copy: CheckoutCopy, full: string): string {
  if (full === copy.cwd) return ".";
  return full.startsWith(`${copy.cwd}/`) ? full.slice(copy.cwd.length + 1) : full;
}

/** The session's path of `inside`, a path inside the checkout ("" for its root): the reverse of
 *  `insideRoot(copy.root, …)`. */
function sessionPath(copy: CheckoutCopy, inside: string): string {
  return inside === "" ? copy.root : `${copy.root}/${inside}`;
}

/** What the copy holds at `inside`, a path inside the checkout (its links followed), or undefined
 *  where it holds nothing. */
function copyStat(copy: CheckoutCopy, inside: string): Stats | undefined {
  return statSync(path.join(copy.dir, inside), { throwIfNoEntry: false });
}

/** The session's absolute path that the search target `target` names, inside the root, and its
 *  path in the checkout; or why not. */
function searchTarget(
  copy: CheckoutCopy,
  target: string
): { full: string; inside: string } | Unanswered {
  if (/[;,]/.test(target)) return { why: `its path ${target} is a list` };
  if (target.startsWith("!") || /[*?[\]{}]/.test(target)) {
    return { why: `its path ${target} holds a glob` };
  }
  if (target.slice(target.lastIndexOf("/") + 1).includes(":")) {
    return { why: `its path ${target} holds a selector` };
  }
  const full = resolvedReadPath(target, { cwd: copy.cwd, home: copy.home });
  const inside = full === undefined ? undefined : insideRoot(copy.root, full);
  if (full === undefined || inside === undefined) {
    return { why: `its path ${target} is outside the session's checkout ${copy.root}` };
  }
  if (inside.split("/").includes(".git")) {
    return { why: `its path ${target} is under .git, which GitHub's tarball does not hold` };
  }
  return { full, inside };
}

/** The `read` call's answer (see the file's header). */
function readAnswer(
  copy: CheckoutCopy,
  input: Record<string, unknown>,
  rows: PathRows
): CheckoutAnswer | Unanswered {
  const other = otherKeys(input, ["path"]);
  if (other !== undefined) return { why: other };
  const target = input.path;
  if (typeof target !== "string") return { why: "it names no path" };
  const ranged = readRange(target);
  if (ranged !== undefined && ranged.range === undefined) {
    return {
      why: `${target} has the selector :${ranged.selector}, of a form the runner does not rebuild (it rebuilds one line range: :N-M, :N+K, :N)`,
    };
  }
  const read = ranged?.path ?? target;
  const full = resolvedReadPath(read, { cwd: copy.cwd, home: copy.home });
  const inside = full === undefined ? undefined : insideRoot(copy.root, full);
  if (full === undefined || inside === undefined) {
    return { why: `${target} is no file of the session's checkout ${copy.root}` };
  }
  if (inside.split("/").includes(".git")) {
    return { why: `${target} is under .git, which GitHub's tarball does not hold` };
  }
  if (leavesCopy(copy, inside)) {
    return { why: `${target} resolves, through a symbolic link, outside the checkout` };
  }
  if (copyStat(copy, inside)?.isDirectory()) {
    if (ranged !== undefined) {
      return {
        why: `${target} is a ranged read of a directory, whose sliced listing the runner does not rebuild`,
      };
    }
    return directoryListing(copy, inside, target);
  }
  if (read.endsWith("/")) {
    return { why: `${target} names a directory the session's checkout does not hold as one` };
  }
  const shown = shownPath(read, full, { cwd: copy.cwd, home: copy.home });
  if (ranged?.range !== undefined) {
    const answer = pathRangeAnswer(copy.dir, {
      file: inside,
      target: read,
      shown,
      rows,
      ...ranged.range,
    });
    return typeof answer === "string" ? { why: `${target}: ${answer}` } : answer;
  }
  const answer = pathAnswer(copy.dir, {
    file: inside,
    target,
    source: full,
    shown,
    rows,
    checkout: { listed: relativeToCwd(copy, full) },
  });
  if (typeof answer === "string") return { why: answer };
  return { text: answer.text, isError: answer.isError };
}

/** The time of change `mtimeNs` (an lstat's, in nanoseconds) of an entry of the copy, in whole
 *  milliseconds (floored), as the pin's native walk reports it (crates/pi-walker/src/fs/native/
 *  linux.rs lines 477-489, the lstat's mtime, a link not followed; crates/pi-natives/src/glob.rs
 *  lines 142-147, `millis`, nanoseconds divided by 1,000,000 rounded down). */
function walkMillis(mtimeNs: bigint): number {
  const whole = mtimeNs / 1_000_000n;
  return Number(mtimeNs < 0n && whole * 1_000_000n !== mtimeNs ? whole - 1n : whole);
}

/** An age of `seconds` as the pin shows it (packages/utils/src/format.ts lines 106-120): none for
 *  0, `just now` under a minute, else its months (of 30 days), weeks, days, hours or minutes, the
 *  first that is one or more, `<n><unit> ago`. */
function formatAge(seconds: number): string {
  if (seconds === 0) return "";
  const mins = Math.floor(seconds / 60);
  const hours = Math.floor(mins / 60);
  const days = Math.floor(hours / 24);
  const weeks = Math.floor(days / 7);
  const months = Math.floor(days / 30);
  if (months > 0) return `${months}mo ago`;
  if (weeks > 0) return `${weeks}w ago`;
  if (days > 0) return `${days}d ago`;
  if (hours > 0) return `${hours}h ago`;
  if (mins > 0) return `${mins}m ago`;
  return "just now";
}

/**
 * The read tool's listing of the directory `inside` of the copy, read as `target` (see the file's
 * header; read.ts lines 3289-3358, workspace-tree.ts lines 57-90 and 166-314): `.`, then each
 * entry to LISTING_DEPTH below it (hidden ones too, `.DS_Store` left out: workspace.rs lines
 * 170-191), each directory's entries after it, newest first by their time of change as the copy
 * holds it (`walkMillis`, a link's own), a tie by name (`byRecency`, `localeCompare`); a directory
 * under it of more than LISTING_CHILD_LIMIT entries shows its 11 newest, `- … <n> more` and its
 * oldest (`assembleTree`). Each line `- <name>` (`<name>/` for a directory) indented two blanks a
 * level, then, where its age is not 0 s, a file's size (`formatBytes` of a regular file's, `0B`
 * for a link's) and its age (`formatAge` of the whole seconds from its time of change to the time
 * the copy was made, `CheckoutCopy.madeAt`: the pin measures from the time of its call, which no
 * replay knows, so the copy's own fixed time makes the listing the same in every arm and run;
 * `formatLines`' compact form). `(empty directory)` for none.
 */
function directoryListing(
  copy: CheckoutCopy,
  inside: string,
  target: string
): CheckoutAnswer | Unanswered {
  const madeAt = copy.madeAt;
  if (madeAt === undefined) {
    return {
      why: `the listing of ${target} shows each entry's age, and the copy records no time it was made to measure it from`,
    };
  }
  interface Entry {
    readonly name: string;
    readonly dir: boolean;
    readonly size: number;
    readonly millis: number;
    readonly children: Entry[];
  }
  const list = (at: string, depth: number): Entry[] | Unanswered => {
    const entries: Entry[] = [];
    for (const name of readdirSync(path.join(copy.dir, at))) {
      if (name === ".DS_Store") continue;
      if (name === ".git" || name === ".jj") {
        return {
          why: `the listing of ${target} shows ${name}, which the copy holds as the runner made it, not as the session's checkout held it`,
        };
      }
      const below = at === "" ? name : `${at}/${name}`;
      const stats = lstatSync(path.join(copy.dir, below), { bigint: true });
      const dir = stats.isDirectory();
      let children: Entry[] = [];
      if (dir && depth < LISTING_DEPTH) {
        const listed = list(below, depth + 1);
        if ("why" in listed) return listed;
        children = listed;
      }
      entries.push({
        name,
        dir,
        size: stats.isFile() ? Number(stats.size) : 0,
        millis: walkMillis(stats.mtimeNs),
        children,
      });
    }
    return entries.sort((a, b) => b.millis - a.millis || a.name.localeCompare(b.name));
  };
  const top = list(inside, 1);
  if ("why" in top) return top;
  const lines = ["."];
  const render = (entries: readonly Entry[], depth: number): void => {
    const indent = "  ".repeat(depth);
    const shown = (entry: Entry) => {
      const label = `${indent}- ${entry.name}${entry.dir ? "/" : ""}`;
      const age = formatAge(Math.max(0, Math.floor((madeAt - entry.millis) / 1000)));
      const size = entry.dir ? "" : ` ${formatBytes(entry.size)}`;
      lines.push(age === "" ? label : `${label}${size} ${age}`);
      render(entry.children, depth + 1);
    };
    if (depth === 1 || entries.length <= LISTING_CHILD_LIMIT) {
      for (const entry of entries) shown(entry);
      return;
    }
    for (const entry of entries.slice(0, LISTING_CHILD_LIMIT - 1)) shown(entry);
    lines.push(`${indent}- … ${entries.length - LISTING_CHILD_LIMIT} more`);
    shown(entries.at(-1) as Entry);
  };
  render(top, 1);
  const text = lines.length <= 1 ? "(empty directory)" : lines.join("\n");
  if (Buffer.byteLength(text) > OUTPUT_BYTES) {
    return { why: `the listing of ${target} passes the ${OUTPUT_BYTES} bytes the read shows` };
  }
  return { text, isError: false };
}

/** Whether the file or directory `inside` of the copy, or the nearest of its parents that is
 *  there, resolves (its symbolic links followed) outside the copy's own directory. */
function leavesCopy(copy: CheckoutCopy, inside: string): boolean {
  const real = realpathSync(copy.dir);
  for (let at = path.join(copy.dir, inside); ; at = path.dirname(at)) {
    let resolved: string;
    try {
      resolved = realpathSync(at);
    } catch {
      if (at === copy.dir || at === path.dirname(at)) return false;
      continue;
    }
    return insideRoot(real, resolved) === undefined;
  }
}

/** Removes from `dir` (a copy of a checkout, cli.ts) every symbolic link whose target, resolved,
 *  is outside `dir`'s own real directory, or does not resolve (a dangling link, a loop), so that
 *  no command run in the copy (`cat`, `grep -R`, `rg --follow`, `find -L`, a `read`) reads a file
 *  outside it through one; a link that resolves inside the copy stays. Returns the path inside
 *  `dir` of each link removed, in path order. */
export function removeOutsideLinks(dir: string): string[] {
  const real = realpathSync(dir);
  const removed: string[] = [];
  const walk = (at: string) => {
    for (const entry of readdirSync(path.join(dir, at), { withFileTypes: true })) {
      const child = at === "" ? entry.name : `${at}/${entry.name}`;
      if (entry.isDirectory()) {
        walk(child);
        continue;
      }
      if (!entry.isSymbolicLink()) continue;
      let target: string | undefined;
      try {
        target = realpathSync(path.join(dir, child));
      } catch {
        target = undefined;
      }
      if (target !== undefined && insideRoot(real, target) !== undefined) continue;
      rmSync(path.join(dir, child), { force: true });
      removed.push(child);
    }
  };
  walk("");
  return removed.sort();
}

/** Every file and directory under `inside` (a directory of the checkout), each with its path
 *  inside the checkout and whether it is a directory, in path order; symbolic links, `.git` and
 *  `.jj` left out (the pin's walk skips `.git`, and a colocated `.jj` ignores all it holds). */
function entriesUnder(copy: CheckoutCopy, inside: string): { path: string; dir: boolean }[] {
  const found: { path: string; dir: boolean }[] = [];
  const walk = (at: string) => {
    for (const entry of readdirSync(path.join(copy.dir, at), { withFileTypes: true })) {
      if (entry.name === ".git" || entry.name === ".jj") continue;
      const child = at === "" ? entry.name : `${at}/${entry.name}`;
      if (entry.isDirectory()) {
        found.push({ path: child, dir: true });
        walk(child);
      } else if (entry.isFile()) found.push({ path: child, dir: false });
    }
  };
  walk(inside);
  return found.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
}

/** A prefix-folded tree of `paths` (pi-utils' `buildPathTree` and `walkPathTree`): each event a
 *  directory (its folded chain) or a file, with its depth. A path ending in `/` is a directory. */
function pathTreeEvents(
  paths: readonly string[]
): { kind: "dir" | "file"; depth: number; name: string; key: string }[] {
  interface Node {
    files: { name: string; key: string }[];
    fileNames: Set<string>;
    subdirs: { name: string; node: Node }[];
    dirIndex: Map<string, Node>;
  }
  const node = (): Node => ({ files: [], fileNames: new Set(), subdirs: [], dirIndex: new Map() });
  const root = node();
  for (const each of paths) {
    const isDir = each.endsWith("/");
    const trimmed = isDir ? each.slice(0, -1) : each;
    if (trimmed === "") continue;
    const segments = trimmed.split("/");
    const dirs = isDir ? segments.length : segments.length - 1;
    let at = root;
    for (let i = 0; i < dirs; i++) {
      const name = segments[i] ?? "";
      let child = at.dirIndex.get(name);
      if (child === undefined) {
        child = node();
        at.dirIndex.set(name, child);
        at.subdirs.push({ name, node: child });
      }
      at = child;
    }
    const name = segments.at(-1) ?? "";
    if (!isDir && !at.fileNames.has(name)) {
      at.fileNames.add(name);
      at.files.push({ name, key: each });
    }
  }
  const events: { kind: "dir" | "file"; depth: number; name: string; key: string }[] = [];
  const walk = (at: Node, depth: number) => {
    for (const file of at.files)
      events.push({ kind: "file", depth, name: file.name, key: file.key });
    for (const subdir of at.subdirs) {
      let dirNode = subdir.node;
      const parts = [subdir.name];
      while (dirNode.files.length === 0 && dirNode.subdirs.length === 1) {
        const only = dirNode.subdirs[0] as { name: string; node: Node };
        parts.push(only.name);
        dirNode = only.node;
      }
      events.push({ kind: "dir", depth, name: parts.join("/"), key: "" });
      walk(dirNode, depth + 1);
    }
  };
  walk(root, 0);
  return events;
}

/** The code units of the whitespace Rust's `trim_end` takes off a line's end (Unicode's
 *  White_Space), which grep's native search takes off each line it reports (render.rs lines
 *  9-19). */
const TRAILING_WHITESPACE: ReadonlySet<number> = new Set([
  0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x20, 0x85, 0xa0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004,
  0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000,
]);

/** `line` less the whitespace at its end, as grep's native search shows it. */
function trimLineEnd(line: string): string {
  let end = line.length;
  while (end > 0 && TRAILING_WHITESPACE.has(line.charCodeAt(end - 1))) end--;
  return line.slice(0, end);
}

/** The largest character boundary of the UTF-8 `bytes` at or before `at` (Rust's
 *  `floor_char_boundary`), and the smallest at or after it (`ceil_char_boundary`). */
function floorBoundary(bytes: Buffer, at: number): number {
  if (at >= bytes.length) return bytes.length;
  let boundary = at;
  while (boundary > 0 && ((bytes[boundary] ?? 0) & 0xc0) === 0x80) boundary--;
  return boundary;
}
function ceilBoundary(bytes: Buffer, at: number): number {
  let boundary = Math.min(at, bytes.length);
  while (boundary < bytes.length && ((bytes[boundary] ?? 0) & 0xc0) === 0x80) boundary++;
  return boundary;
}

/** A context line as grep shows it (render.rs lines 21-34): its trailing whitespace taken off, and
 *  past GREP_COLUMNS bytes cut to GREP_COLUMNS - 3 at a character boundary and `...`. */
function grepContextLine(line: string): string {
  const shown = trimLineEnd(line);
  const bytes = Buffer.from(shown, "utf8");
  if (bytes.length <= GREP_COLUMNS) return shown;
  return `${bytes.subarray(0, floorBoundary(bytes, GREP_COLUMNS - 3)).toString("utf8")}...`;
}

/** `regex.exec(text)`, or why not where that one test took past REGEX_TEST_MS (see there). */
function timedExec(regex: RegExp, text: string): RegExpExecArray | null | Unanswered {
  const started = performance.now();
  const found = regex.exec(text);
  if (performance.now() - started <= REGEX_TEST_MS) return found;
  return {
    why: `a test of its regular expression took past ${REGEX_TEST_MS} ms, after which JavaScript's engine may have stopped a backtracking match early, with no match where Oh My Pi's search finds one`,
  };
}

/** A matching line as grep shows it, with whether it was cut, or why not (render.rs lines 36-139,
 *  grep.ts line 1196): its trailing whitespace taken off; past GREP_COLUMNS bytes, the head that
 *  `grepContextLine` keeps where `found`, the line's first match (the test of the line that found
 *  it), ends in it, else a window of GREP_COLUMNS - 6 bytes centred on that match and marked
 *  `...` on each cut side (`...` and the line's last GREP_COLUMNS - 3 bytes where the window
 *  reaches its end), then ` [col <n>]`, the match's first character's column. `utf8`: whether the
 *  line's file is UTF-8, without which the native search's offsets in it are not the decoded
 *  text's. */
function grepMatchLine(
  line: string,
  found: RegExpExecArray,
  utf8: boolean
): { text: string; cut: boolean } | Unanswered {
  const shown = trimLineEnd(line);
  const bytes = Buffer.from(shown, "utf8");
  if (bytes.length <= GREP_COLUMNS) return { text: shown, cut: false };
  if (!utf8) {
    return {
      why: `it passes the ${GREP_COLUMNS} bytes grep shows in a file that is not UTF-8, where the runner does not place grep's window`,
    };
  }
  const headEnd = floorBoundary(bytes, GREP_COLUMNS - 3);
  const head = `${bytes.subarray(0, headEnd).toString("utf8")}...`;
  const byteAt = (index: number) =>
    floorBoundary(bytes, Math.min(Buffer.byteLength(line.slice(0, index)), bytes.length));
  const start = byteAt(found.index);
  const end = byteAt(found.index + found[0].length);
  const column = ` [col ${[...bytes.subarray(0, start).toString("utf8")].length + 1}]`;
  const body = GREP_COLUMNS - 6;
  if (end <= headEnd) return { text: `${head}${column}`, cut: true };
  const from = Math.max(0, start - Math.floor(Math.max(0, body - (end - start)) / 2));
  if (from + body >= bytes.length) {
    const tail = ceilBoundary(bytes, bytes.length - (GREP_COLUMNS - 3));
    return { text: `...${bytes.subarray(tail).toString("utf8")}${column}`, cut: true };
  }
  const at = ceilBoundary(bytes, from);
  const window = bytes.subarray(at, floorBoundary(bytes, at + body)).toString("utf8");
  return { text: `${at === 0 ? "" : "..."}${window}...${column}`, cut: true };
}

/** A line grep reports: its number and its text as shown. */
interface GrepLine {
  readonly n: number;
  readonly text: string;
}

/** A match grep reports: the file inside the checkout, the line's number, its text as shown
 *  (`grepMatchLine`) and whether it was cut, and its context lines. */
interface GrepMatch {
  readonly file: string;
  readonly n: number;
  readonly text: string;
  readonly cut: boolean;
  readonly before: GrepLine[];
  readonly after: GrepLine[];
}

/** The matches of `regex` in `bytes`, the text of `file` (or its first GREP_FILE_BYTES), at most
 *  `cap`, as grep's native search reports them (scan.rs lines 221-385, line mode): none where a
 *  NUL byte is in its first GREP_BINARY_WINDOW bytes; each with up to GREP_CONTEXT_BEFORE lines
 *  before it that no earlier report holds and up to GREP_CONTEXT_AFTER lines after it before the
 *  next matching line; ending before a line to report that holds a NUL byte. The whole text is
 *  tested first, and only where that test finds a match, or takes past REGEX_TEST_MS, is each
 *  line tested, once, a match line shown from that test's match. Why not, where a matching line
 *  is shown in a form the runner does not place (`grepMatchLine`), or a test of `regex` over a
 *  line took past REGEX_TEST_MS (`timedExec`). Exported for checkout.test.ts, whose rows give it
 *  a regular expression slow on a second test of a line, and one that records what it tests. */
export function grepScan(
  file: string,
  bytes: Buffer,
  regex: RegExp,
  cap: number
): GrepMatch[] | Unanswered {
  if (bytes.length === 0 || bytes.subarray(0, GREP_BINARY_WINDOW).includes(0)) return [];
  const text = bytes.toString("utf8");
  // Invariant: a match of a line is a match of the whole text under `regex`'s flags and `m`, so a
  // whole text with no match holds no matching line, and its lines are neither split nor tested
  // (splitting a 2 GB checkout's files into lines and testing each took about 5 s on this pod;
  // testing each whole text, about 1 s). The match's path runs over the same characters in the
  // whole text: a `^` or `$` it takes at the line's start or end stands after or before a newline
  // there (or at the text's start or end), and one beside a `\r`, `\u2028` or `\u2029` inside the
  // line holds under `m` in both; a `\b` or `\B` at the line's edge sees a newline beside it where
  // the line alone sees nothing, neither a word character; a back-reference repeats what that
  // path captured; and a look-around, the one form that could look past the line and fail there,
  // is refused (grepAnswer). `regex` has no `g` or `y`, so neither test reads its `lastIndex`. A
  // test past REGEX_TEST_MS may have stopped early with no match (see there), so the lines are
  // then tested as before.
  const whole = regex.flags.includes("m") ? regex : new RegExp(regex.source, `${regex.flags}m`);
  if (timedExec(whole, text) === null) return [];
  const utf8 = isUtf8(bytes);
  const lines = textLines(text);
  const out: GrepMatch[] = [];
  // The last line reported, and the match whose after-context lines are still owed.
  let reported = 0;
  let pending: { next: number; left: number; owner: GrepMatch } | undefined;
  const flush = (limit: number): boolean => {
    while (
      pending !== undefined &&
      pending.left > 0 &&
      pending.next < limit &&
      pending.next <= lines.length
    ) {
      const line = lines[pending.next - 1] ?? "";
      if (line.includes("\0")) return false;
      pending.owner.after.push({ n: pending.next, text: grepContextLine(line) });
      reported = pending.next;
      pending.next++;
      pending.left--;
    }
    return true;
  };
  for (const [index, line] of lines.entries()) {
    const n = index + 1;
    const found = timedExec(regex, line);
    if (found === null) continue;
    if ("why" in found) return { why: `line ${n} of ${file}: ${found.why}` };
    if (!flush(n) || out.length >= cap) return out;
    const before: GrepLine[] = [];
    for (let at = Math.max(reported + 1, n - GREP_CONTEXT_BEFORE); at < n; at++) {
      const text = lines[at - 1] ?? "";
      if (text.includes("\0")) return out;
      before.push({ n: at, text: grepContextLine(text) });
    }
    if (line.includes("\0")) return out;
    const shown = grepMatchLine(line, found, utf8);
    if ("why" in shown) return { why: `line ${n} of ${file}: ${shown.why}` };
    const match: GrepMatch = { file, n, ...shown, before, after: [] };
    out.push(match);
    pending = { next: n + 1, left: GREP_CONTEXT_AFTER, owner: match };
    reported = n;
  }
  flush(Number.POSITIVE_INFINITY);
  return out;
}

/** A glob of pi's dialect (crates/pi-walker/src/pattern.rs lines 1-17 and 209-374: globset's
 *  syntax, its separators literal) as the source of a regular expression over a path relative to
 *  a walk's root, for the forms the runner reads: literal characters, `?` and `*` (neither
 *  matching `/`), `**` at its start before a `/` or alone (any directories, none among them), at
 *  its end after a `/` (anything below) or between two `/` (any directories), elsewhere two `*`,
 *  and `{a,b}` groups that hold no group or `**` (an empty alternative left out). Undefined for
 *  any other: a class `[…]`, an escape `\`, a group nested or not closed. */
function dialectGlob(pattern: string): string | undefined {
  if (/[[\]\\]/.test(pattern)) return undefined;
  // The pattern's `{…}` groups, expanded into its alternatives.
  let alternatives = [""];
  for (let at = 0; at < pattern.length; ) {
    const open = pattern.indexOf("{", at);
    const close = pattern.indexOf("}", at);
    if (open < 0) {
      if (close >= 0) return undefined;
      const rest = pattern.slice(at);
      alternatives = alternatives.map((each) => each + rest);
      break;
    }
    const inner = close < 0 ? "" : pattern.slice(open + 1, close);
    if (close < open || inner.includes("{") || inner.includes("**")) return undefined;
    const options = inner.split(",").filter((option) => option !== "");
    const before = pattern.slice(at, open);
    alternatives = alternatives.flatMap((each) => options.map((option) => each + before + option));
    if (alternatives.length === 0 || alternatives.length > 256) return undefined;
    at = close + 1;
  }
  const sources: string[] = [];
  for (const alternative of alternatives) {
    type Token = { readonly lit: string } | "any" | "star" | "prefix" | "suffix" | "middle";
    const tokens: Token[] = [];
    // The character before the one read (parse_star's `prev`).
    let prev: string | undefined;
    for (let at = 0; at < alternative.length; ) {
      const ch = alternative[at] ?? "";
      if (ch !== "*") {
        const last = tokens.at(-1);
        if (ch === "?") tokens.push("any");
        else if (typeof last === "object") tokens[tokens.length - 1] = { lit: last.lit + ch };
        else tokens.push({ lit: ch });
        prev = ch;
        at++;
        continue;
      }
      if (alternative[at + 1] !== "*") {
        tokens.push("star");
        prev = ch;
        at++;
        continue;
      }
      const before = prev;
      at += 2;
      prev = "*";
      const next = alternative[at];
      if (tokens.length === 0 && (next === undefined || next === "/")) {
        tokens.push("prefix");
        if (next === "/") {
          at++;
          prev = "/";
        }
        continue;
      }
      if (tokens.length === 0 || before !== "/" || (next !== undefined && next !== "/")) {
        tokens.push("star", "star");
        continue;
      }
      if (next === "/") {
        at++;
        prev = "/";
      }
      // globset's pop_token: a literal loses its last character, its `/`; a leading `**/` or a
      // trailing `/**` stands; any other is replaced.
      const last = tokens.pop();
      if (last === "prefix" || last === "suffix") tokens.push(last);
      else {
        if (typeof last === "object" && last.lit.length > 1) {
          tokens.push({ lit: last.lit.slice(0, -1) });
        }
        tokens.push(next === undefined ? "suffix" : "middle");
      }
    }
    if (tokens.length === 1 && tokens[0] === "prefix") return "[\\s\\S]*";
    sources.push(
      tokens
        .map((token) =>
          typeof token === "object"
            ? token.lit.replace(/[\\^$.*+?()[\]{}|/]/g, "\\$&")
            : {
                any: "[^/]",
                star: "[^/]*",
                prefix: "(?:/?|[\\s\\S]*/)",
                suffix: "/[\\s\\S]*",
                middle: "(?:/|/[\\s\\S]*/)",
              }[token]
        )
        .join("")
    );
  }
  return sources.join("|");
}

/** The `!` exclusions `patterns` of a grep path (the `!` taken off) as its native search of the
 *  directory `root` of the checkout reads them (grep.ts lines 478-529, crates/pi-walker/src/glob.rs
 *  lines 95-125): each trimmed, its `\` read as `/` and its trailing `/` taken off; one with no
 *  `/`, or that starts with `**`, matches at any depth (one with no `/` as though `**` and a `/`
 *  came before it); any other is resolved against the session's working directory and kept,
 *  anchored, only where it is inside `root`. A regular expression that a path below `root`
 *  matches where it is left out, its directory not entered (none where none is left); why not
 *  where one is a glob of a form the runner does not read (`dialectGlob`). */
function grepExclusions(
  copy: CheckoutCopy,
  patterns: readonly string[],
  root: string
): { regex?: RegExp } | Unanswered {
  const rootFull = path.posix.resolve(sessionPath(copy, root));
  const sources: string[] = [];
  for (const given of patterns) {
    const pattern = given.trim().replace(/\\/g, "/").replace(/\/+$/, "");
    if (pattern === "") continue;
    let built: string;
    if (!pattern.includes("/") || pattern.startsWith("**")) {
      built = pattern.includes("/") ? pattern : `**/${pattern}`;
    } else {
      const target = resolvedReadPath(pattern, { cwd: copy.cwd });
      const relative =
        target === undefined ? "" : path.posix.relative(rootFull, path.posix.resolve(target));
      if (relative === "" || relative === ".." || relative.startsWith("../")) continue;
      built = relative;
    }
    const source = dialectGlob(built);
    if (source === undefined) {
      return { why: `its exclusion !${given} is a glob of a form the runner does not read` };
    }
    sources.push(source);
  }
  return sources.length === 0 ? {} : { regex: new RegExp(`^(?:${sources.join("|")})$`, "u") };
}

/** The files grep's native search reads under the directory `root` of the checkout (run.rs lines
 *  573-690), each its path inside the checkout: every regular file below it, no symbolic link
 *  followed, `.git`, `.jj` (which a colocated copy's own `.gitignore` leaves out) and
 *  `node_modules` not entered, and each file or directory `exclude` matches (its path below
 *  `root`) left out; in path order (`grepReads` puts a file past GREP_FILE_BYTES after all
 *  others). Why not, where a test of `exclude` took past REGEX_TEST_MS (`timedExec`). */
function grepFiles(
  copy: CheckoutCopy,
  root: string,
  exclude: RegExp | undefined
): string[] | Unanswered {
  const found: string[] = [];
  let slow: Unanswered | undefined;
  const walk = (at: string, below: string) => {
    for (const entry of readdirSync(path.join(copy.dir, at), { withFileTypes: true })) {
      if (slow !== undefined) return;
      if (entry.name === ".git" || entry.name === ".jj") continue;
      const inside = at === "" ? entry.name : `${at}/${entry.name}`;
      const under = below === "" ? entry.name : `${below}/${entry.name}`;
      const excluded = exclude === undefined ? null : timedExec(exclude, under);
      if (excluded !== null && "why" in excluded) {
        slow = { why: `its exclusions over ${under}: ${excluded.why}` };
        return;
      }
      if (excluded !== null) continue;
      if (entry.isDirectory()) {
        if (entry.name !== "node_modules") walk(inside, under);
      } else if (entry.isFile()) found.push(inside);
    }
  };
  walk(root, "");
  if (slow !== undefined) return slow;
  // Each path is `root`'s, then its path below it, so the paths' order is that of those below it.
  return found.sort();
}

/** The files `files` (each a path inside the checkout) as grep's native search reads them, in its
 *  order (run.rs lines 573-690): each of at most GREP_FILE_BYTES whole, in the order given, then
 *  each past it over its first GREP_FILE_BYTES (`cut`), in the order given. A file's size is read
 *  from the descriptor it is read through (`bytesWithin`), not by a stat of its path, a second
 *  lookup of it: on this pod a stat of each of the 115,940 files of a 2 GB checkout took 12.6 s,
 *  and reading them all about 15 s. */
function* grepReads(
  copy: CheckoutCopy,
  files: readonly string[]
): Generator<{ inside: string; bytes: Buffer; cut: boolean }> {
  const past: string[] = [];
  for (const inside of files) {
    const bytes = bytesWithin(path.join(copy.dir, inside), GREP_FILE_BYTES);
    if (bytes === undefined) past.push(inside);
    else yield { inside, bytes, cut: false };
  }
  for (const inside of past) {
    yield { inside, bytes: bytesAt(path.join(copy.dir, inside), 0, GREP_FILE_BYTES), cut: true };
  }
}

/** The bytes of the file `file`, or undefined where it holds more than `limit`: its size read from
 *  the descriptor opened to read it. */
function bytesWithin(file: string, limit: number): Buffer | undefined {
  const fd = openSync(file, "r");
  try {
    const size = fstatSync(fd).size;
    if (size > limit) return undefined;
    const bytes = Buffer.allocUnsafe(size);
    let at = 0;
    while (at < size) {
      const read = readSync(fd, bytes, at, size - at, at);
      if (read === 0) break;
      at += read;
    }
    return bytes.subarray(0, at);
  } finally {
    closeSync(fd);
  }
}

/** Where a `grep` call searches (grep.ts lines 673-746 and 826-918, path-utils.ts lines 1050-1133
 *  and 1460-1591; see the file's header): `roots`, each a file or a directory of the checkout, in
 *  the order the pin searches them; `multi`, whether the pin reads the call as a search of several
 *  files (a directory or a list); `missing`, the paths of a list the checkout does not hold;
 *  `exclusions`, its `!` parts. Or why not answered. */
function grepScope(
  copy: CheckoutCopy,
  given: unknown
):
  | {
      readonly roots: readonly { readonly inside: string; readonly dir: boolean }[];
      readonly multi: boolean;
      readonly missing: readonly string[];
      readonly exclusions: readonly string[];
    }
  | Unanswered {
  if (given !== undefined && typeof given !== "string") return { why: "its path is no text" };
  const entry = pathLikeInput(given ?? ".");
  if (entry === "") return { why: "its path is empty, which the pin refuses" };
  const parts = pathListEntries(copy, entry);
  if ("why" in parts) return parts;
  // A `!` part the checkout holds no path of is an exclusion (grep.ts lines 465-476).
  const exclusions = parts.flatMap((part) =>
    part.startsWith("!") && !copyHolds(copy, part) ? [part.slice(1)] : []
  );
  const searched = parts.filter((part) => !part.startsWith("!") || copyHolds(copy, part));
  const rootFull = path.posix.resolve(copy.root);
  const found: { entry: string; full: string; inside: string }[] = [];
  for (const each of searched.length > 0 ? searched : ["."]) {
    const where = searchTarget(copy, each);
    if ("why" in where) return where;
    if (leavesCopy(copy, where.inside)) {
      return { why: `its path ${each} resolves, through a symbolic link, outside the checkout` };
    }
    const full = path.posix.resolve(where.full);
    const inside = insideRoot(rootFull, full);
    if (inside === undefined) {
      return { why: `its path ${each} is outside the session's checkout ${copy.root}` };
    }
    found.push({ entry: each, full, inside });
  }
  // One path is searched as it is; of several, each the checkout holds, the others noted.
  const valid = found.length > 1 ? found.filter((each) => copyStat(copy, each.inside)) : found;
  const missing = found.filter((each) => !valid.includes(each)).map((each) => each.entry);
  const [only, ...more] = valid;
  if (only === undefined) {
    return {
      why: `its paths ${found.map((each) => each.entry).join(", ")} are not in the checkout, and the pin's error for them is not rebuilt`,
    };
  }
  if (more.length === 0) {
    const stats = copyStat(copy, only.inside);
    if (stats === undefined) {
      return {
        why: `its path ${only.entry} is not in the checkout, and the pin's error for it is not rebuilt`,
      };
    }
    const dir = stats.isDirectory();
    return { roots: [{ inside: only.inside, dir }], multi: dir, missing, exclusions };
  }
  if (new Set(valid.map((each) => each.entry)).size < valid.length) {
    return { why: "its path names one path twice, whose search the runner does not rebuild" };
  }
  // Several paths: each searched on its own, in order, unless they are directories one of which
  // holds the rest, searched as that one (resolveSearchPathItems, path-utils.ts lines 1050-1133).
  const items = valid.map((each) => ({
    ...each,
    dir: copyStat(copy, each.inside)?.isDirectory() === true,
  }));
  const split = items.map((each) => each.full.split("/"));
  const first = split[0] ?? [];
  let shared = 0;
  while (shared < first.length && split.every((each) => each[shared] === first[shared])) shared++;
  const common = first.slice(0, shared).join("/") || "/";
  const commonInside = insideRoot(rootFull, common);
  const fanOut =
    items.every((each) => !each.dir) ||
    items.some((each) => !each.dir) ||
    !items.some((each) => each.full === common);
  if (!fanOut && commonInside !== undefined) {
    return { roots: [{ inside: commonInside, dir: true }], multi: true, missing, exclusions };
  }
  return {
    roots: items.map((each) => ({ inside: each.inside, dir: each.dir })),
    multi: true,
    missing,
    exclusions,
  };
}

/** Grep's byte-only `truncateHead`: keep the longest prefix of complete lines that fits. */
function grepHead(text: string, budget: number): { text: string; lines: number; bytes: number } {
  let start = 0;
  let end = 0;
  let lines = 0;
  let bytes = 0;
  for (;;) {
    const newline = text.indexOf("\n", start);
    const next = newline === -1 ? text.length : newline;
    const cost = Buffer.byteLength(text.slice(start, next)) + (lines === 0 ? 0 : 1);
    if (bytes + cost > budget) break;
    bytes += cost;
    end = next;
    lines++;
    if (newline === -1) break;
    start = newline + 1;
  }
  return { text: text.slice(0, end), lines, bytes };
}

/** The `grep` call's answer (see the file's header). */
function grepAnswer(
  copy: CheckoutCopy,
  input: Record<string, unknown>,
  rows: PathRows
): CheckoutAnswer | Unanswered {
  const other = otherKeys(input, ["pattern", "path", "case", "gitignore", "skip"]);
  if (other !== undefined) return { why: other };
  const { pattern, case: caseSensitive } = input;
  if (typeof pattern !== "string" || pattern.trim() === "") return { why: "it names no pattern" };
  const given = input.skip ?? 0;
  if (typeof given !== "number" || !Number.isFinite(given) || Math.floor(given) < 0) {
    return { why: `its skip ${String(given)} is no number of files, which the pin refuses` };
  }
  const skip = Math.floor(given);
  if (pattern.includes("\n") || pattern.includes("\\n")) return { why: "its pattern spans lines" };
  // One leading inline flag group `(?FLAGS)`, FLAGS one or more of `i`, `m` and `s` each once,
  // which the pin's Rust regex takes and JavaScript's RegExp does not, is taken off and read as
  // JavaScript's flags; any other flag group stays in `source` and is not read.
  const leading = /^\(\?([ims]+)\)/.exec(pattern);
  const inline = leading?.[1] ?? "";
  const once = new Set(inline).size === inline.length;
  const source = leading !== null && once ? pattern.slice(leading[0].length) : pattern;
  const group = /\(\?[A-Za-z-]+[:)]/.exec(source);
  if (group !== null) {
    return {
      why: `its pattern holds the flag group \`${group[0]}\`, where the runner reads only one leading \`(?FLAGS)\` of i, m and s, each once`,
    };
  }
  if (/\(\?(<?[=!]|P<)|\\[1-9]/.test(source)) {
    return { why: "its pattern holds a look-around, a back-reference or a Rust named group" };
  }
  const flags = new Set(inline);
  if (caseSensitive === false) flags.add("i");
  let regex: RegExp;
  try {
    regex = new RegExp(source, `${[...flags].join("")}u`);
  } catch (error) {
    return { why: `its pattern is none JavaScript takes: ${String(error)}` };
  }
  const scope = grepScope(copy, input.path);
  if ("why" in scope) return scope;
  const cap = scope.multi ? GREP_MULTI_FILE_MATCHES : GREP_SINGLE_FILE_MATCHES;
  // Each root's matches (at most cap + 1 a file, GREP_TOTAL_MATCHES a root), a line two roots
  // share kept once (grep.ts lines 826-918).
  const matches: GrepMatch[] = [];
  const seen = new Set<string>();
  let totalCapReached = false;
  for (const root of scope.roots) {
    let files = [root.inside];
    if (root.dir) {
      const exclude = grepExclusions(copy, scope.exclusions, root.inside);
      if ("why" in exclude) return exclude;
      const found = grepFiles(copy, root.inside, exclude.regex);
      if ("why" in found) return found;
      files = found;
    }
    let emitted = 0;
    for (const file of grepReads(copy, files)) {
      if (!root.dir && file.cut) {
        return {
          why: `${file.inside} passes the ${GREP_FILE_BYTES} bytes grep reads into memory, past which it streams the file`,
        };
      }
      const found = grepScan(file.inside, file.bytes, regex, cap + 1);
      if ("why" in found) return found;
      const taken = found.slice(0, GREP_TOTAL_MATCHES - emitted);
      emitted += taken.length;
      for (const match of taken) {
        const key = `${match.file}\0${match.n}`;
        if (seen.has(key)) continue;
        seen.add(key);
        matches.push(match);
      }
      if (emitted >= GREP_TOTAL_MATCHES) {
        totalCapReached = true;
        break;
      }
    }
  }
  // Group by file in the order found, keeping the page's files (grep.ts lines 985-1045).
  const display = (inside: string) => relativeToCwd(copy, sessionPath(copy, inside));
  const groups = new Map<string, { count: number; list?: GrepMatch[] }>();
  const page: { shown: string; file: string; list: GrepMatch[] }[] = [];
  const capped: string[] = [];
  const pageStart = scope.multi ? skip : 0;
  const pageEnd = scope.multi ? skip + GREP_FILE_LIMIT : Number.POSITIVE_INFINITY;
  for (const match of matches) {
    const shown = display(match.file);
    const group = groups.get(shown);
    if (group === undefined) {
      const order = groups.size;
      const list = order >= pageStart && order < pageEnd ? [match] : undefined;
      if (list !== undefined) page.push({ shown, file: match.file, list });
      groups.set(shown, list === undefined ? { count: 1 } : { count: 1, list });
      continue;
    }
    group.count++;
    if (group.count > cap) {
      if (group.count === cap + 1 && group.list !== undefined) capped.push(shown);
      continue;
    }
    group.list?.push(match);
  }
  const totalFiles = groups.size;
  const totalLabel = `${totalFiles}${totalCapReached ? "+" : ""}`;
  const skipped = scope.multi ? Math.min(skip, totalFiles) : 0;
  const nextSkip = skipped + page.length;
  const pageNotice =
    scope.multi && totalFiles > skipped + GREP_FILE_LIMIT
      ? `Showing files ${skipped + 1}-${nextSkip} of ${totalLabel}. Use skip=${nextSkip} for the next page, or narrow paths/pattern.`
      : "";
  const missingNotice =
    scope.missing.length > 0 ? `Skipped missing paths: ${scope.missing.join(", ")}` : "";
  if (page.length === 0) {
    const pastEnd = scope.multi && skip > 0 && totalFiles > 0 && skipped >= totalFiles;
    const none = pastEnd
      ? `No more results (${totalLabel} files total; skip=${skip} is past the end)`
      : "No matches found";
    return { text: missingNotice === "" ? none : `${none}\n${missingNotice}`, isError: false };
  }
  // Each file's lines (grep.ts lines 1158-1217): a match's context before it, the match, its
  // context after it, `...` between runs that do not touch.
  const hashline = rows === "hashline";
  let linesCut = false;
  const sections = page.map(({ shown, file, list }) => {
    const lines: string[] = [];
    let last: number | undefined;
    const push = (n: number, text: string, isMatch: boolean) => {
      if (last !== undefined && n > last + 1) lines.push("...");
      lines.push(`${isMatch ? "*" : " "}${n}${hashline ? ":" : "|"}${text}`);
      last = n;
    };
    for (const match of list) {
      for (const line of match.before) push(line.n, line.text, false);
      push(match.n, match.text, true);
      linesCut ||= match.cut;
      for (const line of match.after) push(line.n, line.text, false);
    }
    // Only a session that shows hashline rows shows a file's tag.
    const tag = hashline ? fileTag(readFileSync(path.join(copy.dir, file), "utf8")) : "";
    return { shown, tag, lines };
  });
  const model: string[] = [];
  const modelEnds = new Map<string, number>();
  if (scope.multi) {
    const byShown = new Map(sections.map((section) => [section.shown, section]));
    let emitted = false;
    for (const event of pathTreeEvents(sections.map((section) => section.shown))) {
      const hashes = "#".repeat(event.depth + 1);
      if (emitted && (event.depth === 0 || event.kind === "dir")) model.push("");
      emitted = true;
      if (event.kind === "dir") {
        model.push(`${hashes} ${event.name}/`);
        continue;
      }
      const section = byShown.get(event.key);
      if (section === undefined) continue;
      model.push(`${hashes} ${event.name}${hashline ? `#${section.tag}` : ""}`, ...section.lines);
      modelEnds.set(event.key, model.length);
    }
  } else {
    const [section] = sections;
    if (section !== undefined) {
      if (hashline) model.push(`[${section.shown}#${section.tag}]`);
      model.push(...section.lines);
    }
  }
  // grep.ts reserves space for trailers before cutting the body. Only name a capped file
  // in multi-scope output when its entire section survived. Re-cut only if the trailer grows.
  const trailer = (files: readonly string[]): string => {
    const rest = files.length - 5;
    const names = `${files.slice(0, 5).join(", ")}${rest > 0 ? ` and ${rest} more ${rest === 1 ? "file" : "files"}` : ""}`;
    const capNotice =
      files.length === 0
        ? ""
        : scope.multi
          ? `More than ${cap} matches in ${names}; grep keeps the first ${cap} per file when searching several. Search one file alone for up to ${GREP_SINGLE_FILE_MATCHES}.`
          : `More than ${cap} matches; grep keeps the first ${cap} in a file. Add a line range (\`path:N-M\`) or narrow the pattern for the rest.`;
    return [pageNotice, missingNotice, capNotice]
      .filter((notice) => notice !== "")
      .map((notice) => `\n\n${notice}`)
      .join("");
  };
  const body = model.join("\n");
  const bytes = Buffer.byteLength(body);
  let suffix = trailer(capped);
  const column = linesCut ? `Some lines truncated to ${GREP_COLUMNS} bytes` : undefined;
  for (;;) {
    const reserved = Buffer.byteLength(suffix);
    if (bytes + reserved <= OUTPUT_BYTES) {
      return { text: `${body}${suffix}${outputNotice(column)}`, isError: false };
    }
    const kept = grepHead(body, OUTPUT_BYTES - reserved);
    if (scope.multi) {
      suffix = trailer(capped.filter((file) => (modelEnds.get(file) ?? Infinity) <= kept.lines));
    }
    const suffixBytes = Buffer.byteLength(suffix);
    if (suffixBytes > reserved) continue;
    if (kept.bytes + suffixBytes > OUTPUT_BYTES) {
      return {
        why: "grep saves the output as a session artifact, whose id the runner cannot rebuild",
      };
    }
    const total = body.split("\n").length;
    // grep.ts passes no maxBytes to the truncation metadata, so the notice names the kept bytes.
    const paging =
      kept.lines === 0
        ? `Showing line 1 (partial, 0B of ${formatBytes(bytes)}) of ${total}`
        : showingLines(1, kept.lines, total, kept.bytes);
    return { text: `${kept.text}${suffix}${outputNotice(paging, column)}`, isError: false };
  }
}

/** The key of a worker's data (`answerInWorker`) under which its call is. */
const WORKER_CALL = "skill-cuts checkout call";

/** The call a worker answers (`answerInWorker`): its tool, its copy (with no kept answers), its
 *  input and its rows. */
interface WorkerCall {
  readonly tool: "grep" | "glob";
  readonly copy: CheckoutCopy;
  readonly input: Record<string, unknown>;
  readonly rows: PathRows;
}

/** A `grep` or `glob` call's answer (`grepAnswer`, `globAnswer`), worked out in a worker thread of
 *  this module (its last lines), never in the runner's own thread, and unanswered once
 *  BASH_TIME_LIMIT_MS pass, the worker then ended, as a bash answer is: grep's regular expression
 *  is JavaScript's, which backtracks, so a pattern such as `(a+)+b` over many long lines may take
 *  longer than any run has, where the pin's search, Rust's regex, takes time linear in the line;
 *  and glob's matcher (Bun.Glob's, in time linear in the pattern) of each path under its base may
 *  take seconds for a long pattern in a large checkout (a 57 KB one, about 16 ms a path on this
 *  pod). A worker that fails throws its error, as the answer would have. */
function answerInWorker(
  tool: WorkerCall["tool"],
  copy: CheckoutCopy,
  input: Record<string, unknown>,
  rows: PathRows
): Promise<CheckoutAnswer | Unanswered> {
  const call: WorkerCall = {
    tool,
    copy: { dir: copy.dir, root: copy.root, cwd: copy.cwd, git: copy.git, home: copy.home },
    input,
    rows,
  };
  const worker = new Worker(fileURLToPath(import.meta.url), {
    workerData: { [WORKER_CALL]: call },
  });
  const { promise, resolve, reject } = Promise.withResolvers<CheckoutAnswer | Unanswered>();
  const end = () => {
    resolve({ why: `it ran past the ${BASH_TIME_LIMIT_MS / 1000} s limit` });
    void worker.terminate();
  };
  const limit = timeLimit({ kill: end }, BASH_TIME_LIMIT_MS);
  worker.once("message", (answer: CheckoutAnswer | Unanswered) => {
    limit.clear();
    resolve(answer);
    void worker.terminate();
  });
  worker.once("error", (error) => {
    limit.clear();
    reject(error);
  });
  worker.once("exit", (code) => {
    limit.clear();
    reject(new Error(`the ${tool} worker ended with code ${code} before it answered`));
  });
  return promise;
}

/** A tool's path-like input as the pin reads it (path-utils.ts's `normalizePathLikeInput`): trimmed,
 *  then one pair of outer double quotes taken off. */
function pathLikeInput(given: string): string {
  const trimmed = given.trim();
  return trimmed.length > 1 && trimmed.startsWith('"') && trimmed.endsWith('"')
    ? trimmed.slice(1, -1)
    : trimmed;
}

/** Whether the path `target` names, resolved against the session's working directory, is inside
 *  the root and there in the copy (a link as itself). */
function copyHolds(copy: CheckoutCopy, target: string): boolean {
  const full = resolvedReadPath(target, { cwd: copy.cwd, home: copy.home });
  const inside = full === undefined ? undefined : insideRoot(copy.root, full);
  return (
    inside !== undefined &&
    lstatSync(path.join(copy.dir, inside), { throwIfNoEntry: false }) !== undefined
  );
}

/** Whether `entry` holds a `,`, a `;` or a blank outside braces, where the pin may read it as a
 *  list of paths (path-utils.ts's `hasTopLevelPathDelimiter`). */
function hasListDelimiter(entry: string): boolean {
  let depth = 0;
  for (let at = 0; at < entry.length; at++) {
    const ch = entry[at] ?? "";
    if (ch === "\\") at++;
    else if (ch === "{") depth++;
    else if (ch === "}") depth = Math.max(0, depth - 1);
    else if (depth === 0 && /[,;\s]/.test(ch)) return true;
  }
  return false;
}

/** The paths of a `grep` or `glob` path `entry` (`pathLikeInput`'s), as the pin reads them
 *  (path-utils.ts's `splitDelimitedPathEntry`, lines 599-765, which glob.ts lines 212-225 and
 *  grep.ts lines 673-687 share): split at each `;` outside braces, each part trimmed, its outer
 *  double quotes taken off and the empty ones left out, unless the whole names a path the copy
 *  holds; `[entry]` when it holds no list delimiter; why not answered for a list the pin splits at
 *  commas or blanks only where its paths are there. */
function pathListEntries(copy: CheckoutCopy, entry: string): string[] | Unanswered {
  if (!hasListDelimiter(entry) || copyHolds(copy, entry)) return [entry];
  if (!entry.includes(";")) return { why: `its path ${entry} is a list of commas or blanks` };
  const parts: string[] = [];
  let depth = 0;
  let start = 0;
  for (let at = 0; at <= entry.length; at++) {
    const ch = entry[at];
    if (ch === "\\") at++;
    else if (ch === "{") depth++;
    else if (ch === "}") depth = Math.max(0, depth - 1);
    else if (ch === undefined || (depth === 0 && ch === ";")) {
      const part = pathLikeInput(entry.slice(start, at));
      if (part !== "") parts.push(part);
      start = at + 1;
    }
  }
  return parts.length === 0 ? { why: `its path ${entry} names no pattern` } : parts;
}

/** The `glob` call's answer (see the file's header). */
function globAnswer(
  copy: CheckoutCopy,
  input: Record<string, unknown>
): CheckoutAnswer | Unanswered {
  const other = otherKeys(input, ["path", "hidden", "gitignore", "limit"]);
  if (other !== undefined) return { why: other };
  const given = input.path ?? ".";
  if (typeof given !== "string") return { why: "its path is no text" };
  const entry = pathLikeInput(given);
  if (entry === "") return { why: "it names no pattern" };
  const entries = pathListEntries(copy, entry);
  if ("why" in entries) return entries;
  const requested = input.limit ?? GLOB_LIMIT;
  if (typeof requested !== "number" || !Number.isFinite(requested) || requested <= 0) {
    return { why: `its limit ${String(requested)} is no positive number` };
  }
  const limit = Math.min(GLOB_LIMIT, Math.max(1, Math.floor(requested)));
  // Each pattern's base, inside the checkout, and the glob under it (path-utils.ts's
  // `parseFindPattern`): a path with no glob character is its own base, every entry under it.
  const targets: { pattern: string; inside: string; glob: string; hasGlob: boolean }[] = [];
  for (const pattern of entries.map((each) => each.replace(/\\/g, "/"))) {
    if (/^\/+$/.test(pattern)) return { why: "it searches from /, which the pin refuses" };
    const segments = pattern.split("/");
    const first = segments.findIndex((segment) => /[*?[{]/.test(segment));
    const base = first < 0 ? pattern : segments.slice(0, first).join("/");
    let glob = first < 0 ? "**/*" : segments.slice(first).join("/");
    if (first === 0 && !glob.startsWith("**/")) glob = `**/${glob}`;
    const where = searchTarget(copy, base === "" ? "." : base);
    if ("why" in where) return where;
    if (leavesCopy(copy, where.inside)) {
      return { why: `its base ${base} resolves, through a symbolic link, outside the checkout` };
    }
    targets.push({ pattern, inside: where.inside, glob, hasGlob: first >= 0 });
  }
  // In a list, a path whose base is not there is skipped and noted (glob.ts lines 236-248).
  const missing =
    targets.length > 1 ? targets.filter((each) => copyStat(copy, each.inside) === undefined) : [];
  // Each path found, once, with the time of change the pin ranks it by (glob.ts lines 582 and
  // 600-605; crates/pi-walker/src/glob.rs lines 270-279 and 334-340): a target's matches newest
  // first, by the millisecond of each one's own time of change (`walkMillis`), a tie by its path
  // below the target's base in byte order; a file named alone at its own (followed) time of
  // change (glob.ts lines 506-510); the targets' in their order, each path where it is first.
  const found: { shown: string; millis: number }[] = [];
  const seen = new Set<string>();
  const add = (shown: string, millis: number) => {
    if (seen.has(shown)) return;
    seen.add(shown);
    found.push({ shown, millis });
  };
  const unique = new Set(
    targets.filter((each) => !missing.includes(each)).map((each) => each.pattern)
  );
  for (const target of targets) {
    if (missing.includes(target)) continue;
    const stats = copyStat(copy, target.inside);
    if (unique.size <= 1 && !stats?.isDirectory() && (target.hasGlob || !stats?.isFile())) {
      // The pin's error, `Path not found` or `Path is not a directory`, is not rebuilt.
      return {
        why: `its path ${target.pattern} is not found as a directory or a file, and the pin's error for it is not rebuilt`,
      };
    }
    const full = sessionPath(copy, target.inside);
    if (!target.hasGlob && stats?.isFile()) {
      add(relativeToCwd(copy, full), stats.mtimeMs);
      continue;
    }
    if (!stats?.isDirectory()) continue;
    const glob = new Bun.Glob(target.glob.replace(/\/+$/, ""));
    const directoryOnly = target.glob.endsWith("/");
    const matches: { shown: string; millis: number; walked: Buffer }[] = [];
    for (const each of entriesUnder(copy, target.inside)) {
      const below = target.inside === "" ? each.path : each.path.slice(target.inside.length + 1);
      if (input.hidden === false && below.split("/").some((part) => part.startsWith("."))) continue;
      if ((directoryOnly && !each.dir) || !glob.match(below)) continue;
      const { mtimeNs } = lstatSync(path.join(copy.dir, each.path), { bigint: true });
      matches.push({
        shown: `${relativeToCwd(copy, sessionPath(copy, each.path))}${each.dir ? "/" : ""}`,
        millis: walkMillis(mtimeNs),
        walked: Buffer.from(below),
      });
    }
    if (matches.length > limit) return { why: `it finds more than its limit of ${limit} paths` };
    matches.sort((a, b) => b.millis - a.millis || Buffer.compare(a.walked, b.walked));
    for (const match of matches) add(match.shown, match.millis);
  }
  if (missing.length === targets.length) {
    return {
      why: `its paths ${entries.join(", ")} are not found, and the pin's error for them is not rebuilt`,
    };
  }
  const notes = [
    ...(found.length > 0 && requested > GLOB_LIMIT
      ? [`Requested limit ${requested} clamped to the max of ${GLOB_LIMIT}`]
      : []),
    ...(missing.length > 0
      ? [`Skipped missing paths: ${missing.map((each) => each.pattern).join(", ")}`]
      : []),
  ];
  if (found.length === 0) {
    return { text: ["No files found matching pattern", ...notes].join("\n"), isError: false };
  }
  if (found.length > limit) return { why: `it finds more than its limit of ${limit} paths` };
  // The targets' paths merged newest first, a tie in the order found (glob.ts lines 644-657), as
  // a prefix-folded tree in that order (`formatGroupedPaths`).
  found.sort((a, b) => b.millis - a.millis);
  const lines = pathTreeEvents(found.map((each) => each.shown)).map((event) =>
    event.kind === "dir" ? `${"#".repeat(event.depth + 1)} ${event.name}/` : event.name
  );
  const text = lines.join("\n");
  return { text: notes.length === 0 ? text : `${text}\n\n${notes.join("\n")}`, isError: false };
}

/** The paths an option word `word` may name, as `escapes` reads them: a word that is no option is
 *  its own path; a long option's is its value after its `=` (with none, its value is the next
 *  word, read as a path on its own); a short option's, or a cluster's (`-nfFILE`), is each rest of
 *  the word after one of its letters, any of which may take the rest as its value (grep's
 *  `-f..`, rg's `-fFILE`, git's `ls-files -XFILE`). */
function optionPaths(word: string): string[] {
  if (!word.startsWith("-")) return [word];
  if (word.startsWith("--")) {
    const equals = word.indexOf("=");
    return equals < 0 ? [] : [word.slice(equals + 1)];
  }
  return Array.from({ length: Math.max(0, word.length - 2) }, (_, n) => word.slice(n + 2));
}

/** Where the path `rest` (relative, as written), taken from the session's directory `from` (inside
 *  the root), leads as the kernel walks it in the copy: from the copy's own directory at `from`,
 *  each part that is there has its symbolic links followed (realpath, one part at a time), `..` is
 *  the parent of where that got, and the first part that is not there ends the walk (the kernel
 *  goes no further, so the rest is read as written). `at`, where it got (a real path where each
 *  part is there, `there`); undefined where a step of it leaves the copy. So `l/../x`, through a
 *  link `l` of the checkout to its root, goes to the copy's parent, which the path read as written,
 *  `x`, does not show. */
function kernelWalk(
  copy: CheckoutCopy,
  from: string,
  rest: string
): { readonly at: string; readonly there: boolean } | undefined {
  const real = realpathSync(copy.dir);
  const parts = [...(insideRoot(copy.root, from) ?? "").split("/"), ...rest.split("/")];
  let at = real;
  for (const [n, part] of parts.entries()) {
    if (part === "" || part === ".") continue;
    if (part === "..") at = path.dirname(at);
    else {
      try {
        at = realpathSync(path.join(at, part));
      } catch {
        const written = path.resolve(at, ...parts.slice(n));
        return insideRoot(real, written) === undefined ? undefined : { at: written, there: false };
      }
    }
    if (insideRoot(real, at) === undefined) return undefined;
  }
  return { at, there: true };
}

/** Whether the path `rest` (relative, as written), taken from the session's directory `from`,
 *  stays in the copy as the kernel walks it there (`kernelWalk`). */
function walksInCopy(copy: CheckoutCopy, from: string, rest: string): boolean {
  return kernelWalk(copy, from, rest) !== undefined;
}

/** What a `cd` from the session's directory `from` to `to` (a value of its word) does in the copy
 *  (`Walk.cd`). bash's own cd (`-L`, its default) first makes the path logical from `from` (`..`
 *  the parent of the part before it) and goes there where each part before a `..`, and the whole,
 *  is a directory of the copy (links followed): then it surely succeeds (`enters`), at
 *  `path.posix.resolve(from, to)`. Else it goes, as `cd -P` always does, to the path as written,
 *  the kernel walking it (`kernelWalk`); where that ends at a directory other than the logical one,
 *  `elsewhere` is that directory's session path. `linked`: whether the directory it may end at, as
 *  the kernel holds it (the logical one's real path, or `elsewhere`), is not its path as written,
 *  through a symbolic link in it, so that the kernel walks a later `..` from elsewhere than the
 *  walk does; true too where it may leave the copy. */
function cdLanding(
  copy: CheckoutCopy,
  from: string,
  to: string
): { enters: boolean; linked: boolean; elsewhere?: string } {
  const [base, rest] = to.startsWith("/") ? [copy.root, insideRoot(copy.root, to)] : [from, to];
  const start = insideRoot(copy.root, base);
  const logical = insideRoot(copy.root, path.posix.resolve(from, to));
  const walked = rest === undefined ? undefined : kernelWalk(copy, base, rest);
  if (rest === undefined || start === undefined || logical === undefined || walked === undefined) {
    return { enters: false, linked: true };
  }
  const real = realpathSync(copy.dir);
  const local = (inside: string) => path.join(real, inside);
  const directory = (at: string) => statSync(at, { throwIfNoEntry: false })?.isDirectory() === true;
  const parts = start === "" ? [] : start.split("/");
  let canonical = true;
  for (const part of rest.split("/")) {
    if (part === "" || part === ".") continue;
    if (part !== "..") parts.push(part);
    else if (parts.length > 0 && directory(local(parts.join("/")))) parts.pop();
    else {
      canonical = false;
      break;
    }
  }
  const there = directory(local(logical));
  const landed = walked.there && directory(walked.at) ? insideRoot(real, walked.at) : undefined;
  const elsewhere =
    landed === undefined || landed === logical ? undefined : sessionPath(copy, landed);
  const linked =
    elsewhere !== undefined || (there && realpathSync(local(logical)) !== local(logical));
  return { enters: canonical && there, linked, ...(elsewhere === undefined ? {} : { elsewhere }) };
}

/** Whether the path `value`, of a word whose unquoted text holds a glob character, may walk out of
 *  where it is written to go, read with no matcher: a part with a glob character that starts with
 *  a `.` (bash matches a name's leading `.` only where the pattern starts with one, `dotglob` off,
 *  so such a part may match `.`, `..` where `globskipdots` is off, as before bash 5.2, `.git` or
 *  `.jj`), or one a `..` part follows (the kernel walks the `..` from the file the glob matches,
 *  which may be a link). */
function globLeadsOut(value: string): boolean {
  const parts = value.split("/");
  return parts.some(
    (part, n) => /[*?[]/.test(part) && (part.startsWith(".") || parts.slice(n + 1).includes(".."))
  );
}

/** Why the glob word `text`, of the values `values`, run from the session's directories `cwds` in
 *  the copy (which is fixed, so its names are the session's), may make a word starting with `-`,
 *  read with no matcher, or undefined: a value that starts with `-` (as it stays where nothing
 *  matches, `nullglob` off); a value whose first part holds a glob character, which bash expands
 *  in the directory the shell is in, where one of those holds any name starting with `-` (no
 *  pattern is matched against it, so no bracket expression is misread and no matcher runs in the
 *  runner's thread), or is no directory the copy holds, whose names the runner cannot read. */
function globOptionFault(
  copy: CheckoutCopy,
  text: string,
  values: Iterable<string>,
  cwds: readonly string[]
): string | undefined {
  const real = realpathSync(copy.dir);
  for (const value of values) {
    if (value.startsWith("-")) {
      return `${text} may make the word ${value}, which the program reads as an option`;
    }
    if (!/[*?[]/.test(value.split("/")[0] ?? "")) continue;
    for (const cwd of cwds) {
      const inside = insideRoot(copy.root, cwd);
      let names: string[] | undefined;
      try {
        names = inside === undefined ? undefined : readdirSync(path.join(real, inside));
      } catch {
        names = undefined;
      }
      if (names === undefined) {
        return `${text} expands in ${cwd}, which the copy does not hold as a directory, so the runner cannot read the names it may make`;
      }
      const found = names.find((name) => name.startsWith("-"));
      if (found !== undefined) {
        return `${text} expands in ${cwd}, which holds ${found}, so it may make the word ${found}, which the program reads as an option`;
      }
    }
  }
  return undefined;
}

/** Whether a directory on the walk up from the session's directory `dir` to the checkout's root
 *  (the root too where `withRoot`), as the kernel walks it in the copy (`dir`'s real path, then
 *  each parent), holds an entry `name`: where git (a `HEAD`, a repository's layout) or jj (a `.jj`)
 *  finds a repository the checkout tracks before the copy's own. True too where `dir` is no
 *  directory the copy holds inside the root, which the runner does not walk. */
function nestedUnder(copy: CheckoutCopy, dir: string, name: string, withRoot = false): boolean {
  const real = realpathSync(copy.dir);
  const inside = insideRoot(copy.root, dir);
  if (inside === undefined) return true;
  let at: string;
  try {
    at = realpathSync(path.join(real, inside));
  } catch {
    return true;
  }
  for (; ; at = path.dirname(at)) {
    if (insideRoot(real, at) === undefined) return true;
    if ((at !== real || withRoot) && existsSync(path.join(at, name))) return true;
    if (at === real) return false;
  }
}

/** Why the simple command `command` at `state` may reach a file outside the session's checkout or
 *  show a value of the runner's in place of the session's, or undefined (see `escapes`). */
function escapeFault(
  copy: CheckoutCopy,
  command: SimpleCommand,
  state: ShellState,
  walk: Walk
): string | undefined {
  const [first, ...args] = command.words;
  const words = commandWords(command);
  for (const word of words) {
    for (const part of word) {
      if (part.kind === "var" && !state.vars.has(part.name) && part.name !== "PWD") {
        return `${wordText(word)} expands $${part.name}, whose value is the runner's, not the session's`;
      }
    }
    // bash puts a home directory in place of an unquoted `~` at a word's start, or after its `=`
    // or a `:` (`~user` any user's, from the machine's /etc/passwd). Where the copy names the
    // session's home, `escapes` has read each such `~` as `$HOME` (`homeTildes`), and any left is
    // a literal; where it names none, that home would be the runner's, not the session's. A
    // quoted `~` is a literal.
    if (copy.home === undefined && /(^|[=:])~/.test(bareText(word))) {
      return `${wordText(word)} is under ~, a home directory of the runner's, not the session's`;
    }
  }
  const program = first === undefined ? undefined : literal(first);
  if (program === "cd" && args.every((word) => (literal(word) ?? "").startsWith("-"))) {
    return copy.home === undefined
      ? "its cd names no directory, so it goes to $HOME, which is the runner's"
      : `its cd names no directory, so it goes to $HOME, the session's home ${copy.home}, and the runner follows no such cd`;
  }
  const reaches = program !== undefined && !Object.hasOwn(NO_PATH_PROGRAMS, program);
  for (const word of [...(reaches ? args : []), ...command.redirects.map((each) => each.target)]) {
    const text = wordText(word);
    // A word whose unquoted text holds a glob character names the files it matches: a `cd` to one
    // is not followed, nor one that may make a word starting with `-` (an option a file's name
    // makes, `--files0-from=x`, `-n`: `globOptionFault`), nor a path whose glob may match a name
    // starting with `.`, or comes before a `..` (`globLeadsOut`).
    const glob = globbed(word);
    if (glob && program === "cd") {
      return `${text} holds a glob, and the runner does not follow a cd to what it matches`;
    }
    const values = valuesOf(word, state, walk);
    if (values === undefined) {
      return `${text} expands to a value the runner cannot place inside the session's checkout`;
    }
    const option = glob ? globOptionFault(copy, text, values, state.cwds) : undefined;
    if (option !== undefined) return option;
    for (const value of [...values].flatMap(optionPaths)) {
      if (value === "" || value === "/dev/null") continue;
      if (glob && globLeadsOut(value)) {
        return `${text} holds a glob that may match a name starting with . (., .., .git), or one before .., and the runner does not walk what it matches`;
      }
      if (value.split("/").includes(".git")) return `${text} names .git`;
      const fulls = value.startsWith("/")
        ? [path.posix.normalize(value)]
        : state.cwds.map((at) => path.posix.resolve(at, value));
      if (fulls.some((full) => insideRoot(copy.root, full) === undefined)) {
        return `${text} is outside the session's checkout ${copy.root}`;
      }
      const walked = value.startsWith("/")
        ? [{ from: copy.root, rest: insideRoot(copy.root, value) }]
        : state.cwds.map((from) => ({ from, rest: value }));
      if (walked.some(({ from, rest }) => rest === undefined || !walksInCopy(copy, from, rest))) {
        return `${text} leads, through a symbolic link of the checkout and .. after it, outside the checkout`;
      }
    }
  }
  // Each word's one value after the program, as the checks below read the words.
  const given = args.map((word) => oneValue(word, state, walk));
  // The kernel walks a `..` from the directory the shell is in as it is real, the walk from its
  // path as written: where the shell may be in a directory whose real path is not that path (a
  // working directory, or a `cd`, through a symbolic link: `cdLanding`), no word may hold a `..`.
  if (state.linked) {
    for (const word of words) {
      const text = wordText(word);
      if (
        text.includes("..") ||
        [...(valuesOf(word, state, walk) ?? [])].some((value) => value.includes(".."))
      ) {
        return `${text} holds .., which the kernel walks from the directory the shell is in as it is real, and the shell may be in one it reached through a symbolic link of the checkout, elsewhere than its path as written`;
      }
    }
    // There, too, what prints the working directory's own path may print its real one (the
    // sandbox enters it as it is real, so bash's `pwd` and `$PWD` are that path, and git and jj
    // start from it), where the session's printed the path as written: `pwd`, `$PWD`, the
    // environment `env` and `export` print (`PWD` in it), git's `rev-parse --show-prefix`,
    // `--show-cdup` and `--show-toplevel`, and `jj root`.
    if (
      program === "pwd" ||
      program === "env" ||
      (program === "export" &&
        given.every((value) => value === undefined || value.startsWith("-"))) ||
      (program === "git" &&
        given.some((value) =>
          ["--show-prefix", "--show-cdup", "--show-toplevel"].includes(value ?? "")
        )) ||
      (program === "jj" && given.includes("root")) ||
      words.some((word) => word.some((part) => part.kind === "var" && part.name === "PWD"))
    ) {
      return `its ${program ?? "command"} command may print the working directory's path, which the shell, in a directory it may have reached through a symbolic link of the checkout, gives as its real path, not the path as written that the session saw`;
    }
  }
  if (program === "jj") {
    // jj reads the repository whose `.jj` is in the directory its `-R` names, else in the nearest
    // of the working directory and its parents: one the checkout tracks below its root, or at it
    // where the runner's colocation did not make it (`bashAnswer` refuses every jj command of such a
    // copy), is not the copy's (in the sandbox jj stops at its configuration, which it cannot move
    // to the read-only home, and its store may name a git repository outside the copy).
    const parsed = parseOptions(given, JJ_REPOSITORY);
    if (parsed === undefined) return "its jj command's repository is not known";
    const named = parsed.options.flatMap((option) =>
      option.names.includes("R") || option.names.includes("repository") ? [option.value ?? ""] : []
    );
    const starts = state.cwds.flatMap((at) =>
      named.length === 0 ? [at] : named.map((each) => path.posix.resolve(at, each))
    );
    if (starts.some((dir) => nestedUnder(copy, dir, ".jj", copy.jj !== true))) {
      return "its jj command may read a jj repository the checkout holds below its root, not the copy's";
    }
  }
  return undefined;
}

/** What a `~` that bash expands to the home directory stands for once `homeTildes` reads it: a
 *  quoted `$HOME`, since bash neither splits nor globs what a tilde expands to. */
const HOME_PART: WordPart = { kind: "var", name: "HOME", quoted: true };

/**
 * `word` with each `~` that bash's tilde expansion puts the home directory in place of read as
 * HOME_PART (bash 5 in its default mode, as the sandbox runs it): an unquoted `~` at the word's
 * start, or, in an assignment's value (`assigned`) or after the `=` of a word that reads as one
 * (`NAME=…`: bash expands an argument or a redirection's target of that form too), at the value's
 * start or after an unquoted `:`, whose tilde-prefix (what follows up to the next unquoted `/`,
 * or there `:`, or the word's end) is empty. Any other `~` is a literal (`--opt=~/x`, `x:~/y`,
 * `"~"`, `\~`). Why not answered where a tilde-prefix is not empty: `~user` (the machine's user
 * of that name), `~+` and `~-` (the shell's directories), or one holding a quoted part, which
 * bash leaves as it is.
 */
function homeTildes(word: Word, assigned: boolean): { readonly word: Word } | Unanswered {
  // The word as units: each unquoted character, or one part that is no unquoted text.
  type Unit = { readonly ch: string } | { readonly part: WordPart };
  const units: Unit[] = word.flatMap((part): Unit[] =>
    part.kind === "text" && !part.quoted ? [...part.text].map((ch) => ({ ch })) : [{ part }]
  );
  const bare = (at: number): string | undefined => {
    const unit = units[at];
    return unit !== undefined && "ch" in unit ? unit.ch : undefined;
  };
  const named = assigned ? undefined : assignmentOf(word);
  // Where an assignment's value starts: tildes there and after each unquoted `:` expand too.
  const value = assigned ? 0 : named === undefined ? undefined : named.name.length + 1;
  const starts = [0];
  if (value !== undefined) {
    starts.push(value);
    for (let at = value; at < units.length; at++) if (bare(at) === ":") starts.push(at + 1);
  }
  let expanded = false;
  for (const start of starts) {
    if (bare(start) !== "~") continue;
    let end = start + 1;
    while (end < units.length && bare(end) !== "/" && !(value !== undefined && bare(end) === ":")) {
      end++;
    }
    if (end > start + 1) {
      return {
        why: `${wordText(word)} has a ~ with more after it (~user, ~+, ~-, or a quoted part), which bash reads as a user's home directory or one of the shell's own, or leaves as it is, a form the runner does not rebuild`,
      };
    }
    units[start] = { part: HOME_PART };
    expanded = true;
  }
  if (!expanded) return { word };
  const parts: WordPart[] = [];
  let text = "";
  for (const unit of units) {
    if ("ch" in unit) {
      text += unit.ch;
      continue;
    }
    if (text !== "") parts.push({ kind: "text", text, quoted: false });
    text = "";
    parts.push(unit.part);
  }
  if (text !== "") parts.push({ kind: "text", text, quoted: false });
  return { word: parts };
}

/** `script` with each word read by `homeTildes` (an assignment's value as one, every other word
 *  and each redirection's target as a word), in its groups and command substitutions too; or the
 *  first why it gives. */
function homeScript(script: Script): { readonly script: Script } | Unanswered {
  let why: string | undefined;
  const wordOf = (word: Word, assigned: boolean): Word => {
    const subbed = word.map((part) =>
      part.kind === "sub" ? { ...part, script: scriptOf(part.script) } : part
    );
    const read = homeTildes(subbed, assigned);
    if ("why" in read) {
      why ??= read.why;
      return subbed;
    }
    return read.word;
  };
  const commandOf = (command: SimpleCommand | Group): SimpleCommand | Group => {
    const redirects = command.redirects.map((each) => ({
      ...each,
      target: wordOf(each.target, false),
    }));
    if (command.kind === "group")
      return { ...command, script: scriptOf(command.script), redirects };
    return {
      ...command,
      assignments: command.assignments.map((each) => ({
        ...each,
        value: wordOf(each.value, true),
      })),
      words: command.words.map((word) => wordOf(word, false)),
      redirects,
    };
  };
  const scriptOf = (inner: Script): Script =>
    inner.map((pipelines) => pipelines.map((pipeline) => pipeline.map(commandOf)));
  const mapped = scriptOf(script);
  return why === undefined ? { script: mapped } : { why };
}

/** Whether a word of `script` (in its groups and command substitutions too) expands `$HOME`. */
function expandsHome(script: Script): boolean {
  return script
    .flat(2)
    .some(
      (command) =>
        (command.kind === "group" && expandsHome(command.script)) ||
        commandWords(command).some((word) =>
          word.some(
            (part) =>
              (part.kind === "var" && part.name === "HOME") ||
              (part.kind === "sub" && expandsHome(part.script))
          )
        )
    );
}

/** Why `script`, run from the session's working directory `cwd` (`local`, its path in the copy on
 *  this machine), may reach a file outside the session's checkout or show a value of the runner's,
 *  or undefined (see the file's header, **Confinement**). Each word a program may open (every word
 *  after the program but those of NO_PATH_PROGRAMS, and every redirection's target) is read at
 *  each value it may take (`walkScript`: a variable the command assigned, `$PWD` and `$(pwd)` as
 *  each directory the shell may be in, `$(git rev-parse --show-toplevel)` where git meets no
 *  repository of the checkout's own below the root first, and, in a colocated copy, `$(jj root)`;
 *  where the copy names the session's home, `$HOME` and each `~` bash expands, `homeTildes`, as
 *  that home). */
function escapes(
  copy: CheckoutCopy,
  cwd: string,
  local: string,
  script: Script
): string | undefined {
  const walk: Walk = {
    check: (command, state) => escapeFault(copy, command, state, walk),
    subValues: (sub, state) => {
      const [pipelines, ...more] = sub;
      const [commands, ...others] = pipelines ?? [];
      const [command, ...rest] = commands ?? [];
      if (more.length + others.length + rest.length > 0 || command?.kind !== "simple") {
        return undefined;
      }
      if (command.assignments.length + command.redirects.length > 0) return undefined;
      const words = command.words.map(literal);
      if (words.includes(undefined)) return undefined;
      switch (words.join(" ")) {
        case "pwd":
          return state.cwds.length > 0 ? new Set(state.cwds) : undefined;
        case "git rev-parse --show-toplevel":
          // git, walking up from the working directory, dies at a directory laid out as a bare
          // repository (a `HEAD`; `safe.bareRepository`), so the substitution is then empty.
          return copy.git && state.cwds.every((at) => !nestedUnder(copy, at, "HEAD"))
            ? new Set([copy.root])
            : undefined;
        case "jj root":
          return copy.jj === true ? new Set([copy.root]) : undefined;
        default:
          return undefined;
      }
    },
    cd: (from, to) => cdLanding(copy, from, to),
  };
  // The sandbox enters the working directory as it is real (`cd -P`): where that is not its path in
  // the copy (a symbolic link in it), the shell is elsewhere than the walk's path.
  let linked = true;
  try {
    linked = realpathSync(local) !== local;
  } catch {
    // No such directory: bashAnswer refuses the working directory.
  }
  const vars = new Map<string, Values>();
  let walked = script;
  if (copy.home !== undefined) {
    const read = homeScript(script);
    if ("why" in read) return read.why;
    const view = sandboxHome(copy);
    if (view !== undefined && "why" in view && expandsHome(read.script)) return view.why;
    walked = read.script;
    vars.set("HOME", new Set([copy.home]));
  }
  return walkScript(walked, { vars, cwds: [cwd], linked }, false, walk);
}

/** The `bash` call's answer (see the file's header). */
async function bashAnswer(
  copy: CheckoutCopy,
  input: Record<string, unknown>
): Promise<CheckoutAnswer | Unanswered> {
  const option = bashOptionFault(input);
  if (option !== undefined) return { why: option };
  const command = input.command;
  const script = readOnlyScript(command);
  if (typeof command !== "string" || script === undefined) {
    return { why: "its command is not read-only" };
  }
  const programs = simpleCommands(script).map((each) => literal(each.words[0] ?? []) ?? "");
  if (!copy.git && programs.some((program) => program === "git" || program === "jj")) {
    return {
      why: "the copy is GitHub's tarball of the commit, which holds no history: the fetch of the commit failed",
    };
  }
  if (programs.includes("jj")) {
    if (copy.jj !== undefined && copy.jj !== true) {
      return {
        why: `the copy could not be made a colocated jj repository, so no jj command is answered: ${copy.jj.why}`,
      };
    }
    // A `.jj` at the root that no colocation of the runner's made (the session ran no jj) is a
    // store the checkout tracks: jj would answer from it, where the session saw `There is no jj
    // repo`, and it may name a git repository outside the copy.
    if (
      copy.jj !== true &&
      lstatSync(path.join(realpathSync(copy.dir), ".jj"), { throwIfNoEntry: false }) !== undefined
    ) {
      return {
        why: "the checkout tracks a .jj at its root, a jj repository no colocation of the runner's made, so no jj command is answered",
      };
    }
    if (Bun.which("jj") === null) return { why: "jj is not on the runner's PATH" };
  }
  let cwd = copy.cwd;
  if (typeof input.cwd === "string") {
    const resolved = resolvedReadPath(input.cwd, { cwd: copy.cwd, home: copy.home });
    if (resolved === undefined || insideRoot(copy.root, resolved) === undefined) {
      return { why: `its working directory ${input.cwd} is outside the session's checkout` };
    }
    cwd = resolved;
  }
  const localCwd = path.join(realpathSync(copy.dir), insideRoot(copy.root, cwd) ?? "");
  const leads = escapes(copy, cwd, localCwd, script);
  if (leads !== undefined) return { why: leads };
  if (
    !statSync(localCwd, { throwIfNoEntry: false })?.isDirectory() ||
    !walksInCopy(copy, cwd, "")
  ) {
    return { why: `its working directory ${cwd} is no directory of the checkout` };
  }
  const lookedFor = programs.filter((each) => each !== "" && !Object.hasOwn(SHELL_BUILTINS, each));
  const ran = await sandboxedBash(copy, command, cwd, [...new Set(lookedFor)]);
  if ("why" in ran) return ran;
  const { output, exitCode, wallTimeMs } = ran;
  if (output.split("\n").some((line) => Buffer.byteLength(line) > BASH_COLUMNS)) {
    return {
      why: `a line of its output passes the ${BASH_COLUMNS} bytes Oh My Pi shows, and the notice of its cut names the artifact the session saves the output to, whose id the copy cannot know`,
    };
  }
  // The minimizer (crates/pi-shell/src/minimizer/plan.rs lines 1-32, engine.rs lines 33-125) may
  // rewrite a command's output, at 1,000 characters or more, only where its top level is simple
  // commands joined by `&&` and `;` and one of them, unpiped, runs a program a filter reads; a
  // group or a subshell there keeps the whole command as it is, and so does a `||`, which the
  // parsed script does not tell from an `&&`, so such a command is refused all the same.
  const top = script.flat();
  if (
    output.length >= MINIMIZE_CHARS &&
    top.every((pipeline) => pipeline.every((each) => each.kind === "simple")) &&
    top.some((pipeline) => {
      const [only, ...more] = pipeline;
      const program = only?.kind === "simple" ? literal(only.words[0] ?? []) : undefined;
      return (
        more.length === 0 && program !== undefined && Object.hasOwn(MINIMIZED_PROGRAMS, program)
      );
    })
  ) {
    return {
      why: "Oh My Pi's minimizer may rewrite its output, and a rewritten output ends with a notice naming the artifact the session saves the output to, whose id the copy cannot know",
    };
  }
  const lines = [
    output === "" ? (exitCode === 0 ? "(no output, exit code 0)" : "(no output)") : output,
  ];
  if (wallTimeMs >= 1000) lines.push("", `Wall time: ${(wallTimeMs / 1000).toFixed(2)} seconds`);
  if (exitCode !== 0) lines.push("", `Command exited with code ${exitCode}`);
  return { text: lines.join("\n"), isError: exitCode !== 0 };
}

/** The answer to the read-only call `name` with `input` from `copy` (see the file's header), its
 *  reads by path shown as `rows`, or why it is not answered. A `read`, `grep` or `glob` answer is
 *  kept in `copy.answers` from when the call is made, and given again to the same call. */
export async function answerCall(
  copy: CheckoutCopy,
  name: string | undefined,
  input: unknown,
  rows: PathRows
): Promise<CheckoutAnswer | Unanswered> {
  if (typeof input !== "object" || input === null || Array.isArray(input)) {
    return { why: "its input is no object" };
  }
  const fields = input as Record<string, unknown>;
  if (name === "read" || name === "grep" || name === "glob") {
    const key = `${name}\0${rows}\0${JSON.stringify(fields)}`;
    const held = copy.answers?.get(key);
    if (held !== undefined) return held;
    const answer =
      name === "read"
        ? Promise.resolve(readAnswer(copy, fields, rows))
        : answerInWorker(name, copy, fields, rows);
    copy.answers?.set(key, answer);
    answer.catch(() => copy.answers?.delete(key));
    return answer;
  }
  if (name === "bash") return bashAnswer(copy, fields);
  return { why: `no capture declares a ${name} tool, so the runner rebuilds none of its forms` };
}

// A worker (`answerInWorker`): this module, loaded in a worker thread whose data names a grep or
// glob call, answers it, once every constant above is set.
const workerCall = isMainThread
  ? undefined
  : (workerData as Record<string, WorkerCall> | null)?.[WORKER_CALL];
if (workerCall !== undefined) {
  const { tool, copy, input, rows } = workerCall;
  parentPort?.postMessage(
    tool === "grep" ? grepAnswer(copy, input, rows) : globAnswer(copy, input)
  );
}
