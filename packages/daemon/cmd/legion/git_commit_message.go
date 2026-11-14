package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// This file builds and checks a commit's message as git 2.47.3 does: prepare_to_commit and
// cmd_commit of builtin/commit.c, with the cleanup and -s of sequencer.c, strbuf.c and commit.c.

// commitCleanupMode is git's commit_msg_cleanup_mode.
type commitCleanupMode int

const (
	cleanupSpace commitCleanupMode = iota
	cleanupNone
	cleanupScissors
	cleanupAll
)

// commitCleanup is get_cleanup_mode (sequencer.c). default is strip when the editor runs and
// whitespace when it does not, and scissors is whitespace without the editor. ok is false for a
// mode git dies on.
func commitCleanup(arg commitString, useEditor bool) (commitCleanupMode, bool) {
	switch {
	case !arg.set || arg.value == "default":
		if useEditor {
			return cleanupAll, true
		}
		return cleanupSpace, true
	case arg.value == "verbatim":
		return cleanupNone, true
	case arg.value == "whitespace":
		return cleanupSpace, true
	case arg.value == "strip":
		return cleanupAll, true
	case arg.value == "scissors":
		if useEditor {
			return cleanupScissors, true
		}
		return cleanupSpace, true
	}
	return 0, false
}

// cutLine is the scissors line without its comment string (wt-status.c). A message that holds it
// is cut there when the cleanup is scissors or the commit is verbose (cleanup_message).
const cutLine = "------------------------ >8 ------------------------"

// commitBuild builds a commit's message as git does, to learn whether git accepts it and what
// git's trailer step makes of it. script is whether the git that runs the commit is a script.
type commitBuild struct {
	git    gitReader
	script bool
	opts   commitRead
	plan   commitPlan
}

// lines is each line of want that the message lacks, when the wrapper may add those lines, and
// nil when it adds none. stash is the source that the wrapper read and that git can no longer read
// (commitStash), or nil: git gets it whether a line is added or not. The wrapper may add the lines
// when two things hold. First, git alone accepts the message that prepare builds (accepts).
// Second, git's trailer step changes nothing but add the lines (onlyAdds).
//
// The editor is taken to leave the text as it is, which is exact for Oh My Pi's GIT_EDITOR=true.
// For a person's editor the answer is about the text before the edit. A read that fails gives
// nil. Under core.commentChar or core.commentString auto, git picks the comment character from
// the message (adjust_comment_line_char), and its status lines and cleanup use a character the
// helper's own runs do not know, so the wrapper adds none when the editor runs or the cleanup is
// strip. A configured trailer command makes the wrapper add none, and it reads nothing then: the
// trailer step would start the command, and a command that git alone does not run must not run
// here.
func (b commitBuild) lines(want []gitTrailer) (lines []gitTrailer, stash *commitStash, err error) {
	cfg, ok := b.git.commitConfig()
	if !ok || cfg.trailerCommand {
		return nil, nil, nil
	}
	cleanup := b.opts.cleanup
	if !b.opts.cleanupWritten {
		cleanup = cfg.cleanup
	}
	mode, ok := commitCleanup(cleanup, b.plan.useEditor)
	if !ok || cfg.autoComment && (b.plan.useEditor || mode == cleanupAll) {
		return nil, nil, nil
	}
	draft, ok, err := b.prepare(cfg, mode)
	if err != nil || !ok {
		return nil, draft.stash, err
	}
	message, ok := b.accepts(draft, mode)
	if !ok {
		return nil, draft.stash, nil
	}
	return b.onlyAdds(draft.text, message, want, mode), draft.stash, nil
}

// commitDraft is what prepare builds. text is the text git hands its trailer step. template is the
// template's text when templateRead is true: template_untouched compares the message with it.
// stash is the one source that prepare kept for git (commitStash), or nil.
type commitDraft struct {
	text         string
	template     string
	templateRead bool
	stash        *commitStash
}

// prepare builds the text git hands its trailer step: COMMIT_EDITMSG as prepare_to_commit writes
// it. That is the message from its sources, in prepare_to_commit's order, cleaned of spaces unless
// the cleanup is verbatim or the template is the source, then the -s line, and then, when the
// editor runs with commit.status on, a line feed and the status lines. The status lines are left
// out here, and the line feed git writes before them stands in for them. Each status line starts
// with the comment string, and the trailer step ignores the comment and blank lines at the end of
// its input (ignored_log_message_bytes, commit.c), so it finds the same trailer block, puts the
// lines at the same place, and leaves the lines after that place as they are. The cleanup takes
// the status lines away again, except under the whitespace and verbatim cleanups, where git's
// message keeps them, and git then accepts more than the message built here.
//
// prepare also reads the template where template_untouched reads it. It keeps at most one stash
// (readFile): a source that would need a second one is left unread. ok is false when a read fails
// or a source is left unread, and then no line is added. The error is a stash it could not write.
func (b commitBuild) prepare(cfg commitConfig, mode commitCleanupMode) (commitDraft, bool, error) {
	o, p := b.opts, b.plan
	var d commitDraft
	verbatim := mode == cleanupNone
	includeStatus := cfg.includeStatus
	if o.statusWritten {
		includeStatus = o.includeStatus
	}
	// readSource is readFile, and it keeps the stash readFile makes: once d holds a stash, a
	// source that would need another is declined.
	readSource := func(f commitFile, path string, stdin, written bool) (string, commitReadStatus, error) {
		read, status, stash, err := b.readFile(f, path, stdin, written && d.stash == nil)
		if stash != nil {
			d.stash = stash
		}
		return read, status, err
	}

	// The template: -t, else commit.template (git_commit_config), when no source dropped it.
	template, templateWritten := commitFile{}, true
	if p.templateOn {
		if o.templateWritten {
			template = o.template
		} else {
			value, found, err := b.git.config("commit.template", "--type=path")
			if err != nil {
				return d, false, nil
			}
			template, templateWritten = commitFile{set: found, value: value}, false
		}
	}

	mergeOn := !o.haveMessage && !o.file.set && !p.use.set && !o.fixup.set
	var paths gitPaths
	if mergeOn || o.file.set && o.file.value != "-" && !filepath.IsAbs(o.file.value) || template.set && !filepath.IsAbs(template.value) {
		var err error
		if paths, err = b.git.paths(); err != nil {
			return d, false, nil
		}
	}
	templatePath := paths.configPath(template.value)
	if templateWritten {
		templatePath = paths.cliPath(template.value)
	}

	// The commits git reads: use_message (parse_and_validate_options), then --squash's and
	// --fixup's (prepare_to_commit).
	squashIsUse := o.squash.set && p.use.set && p.use.value == o.squash.value
	var use, squash, fixup commitText
	for _, read := range []struct {
		on   bool
		name string
		text *commitText
	}{
		{p.use.set, p.use.value, &use},
		{o.squash.set && !squashIsUse, o.squash.value, &squash},
		{o.fixup.set, p.fixupCommit, &fixup},
	} {
		if read.on {
			var err error
			if *read.text, err = b.git.commitMessage(read.name); err != nil {
				return d, false, nil
			}
		}
	}

	// prepare_to_commit: the sources, in its order.
	text := ""
	switch {
	case squashIsUse:
		text = "squash! "
	case o.squash.set:
		text = "squash! " + squash.subject + "\n\n"
	}
	fromTemplate := false
	switch {
	case o.haveMessage && !o.fixup.set:
		text += o.message
	case o.file.set:
		path := "-"
		if o.file.value != "-" {
			path = paths.cliPath(o.file.value)
		}
		read, status, err := readSource(o.file, path, o.file.value == "-", true)
		if status != readDone {
			return d, false, err
		}
		text += read
	case p.use.set:
		text += skipBlankLines(use.message)
	case o.fixup.set:
		text += p.fixupPrefix + "! " + fixup.subject + "\n\n"
		switch {
		case p.fixupPrefix == "fixup":
			if o.haveMessage {
				text += o.message
			}
		case strings.HasPrefix(fixup.subject, "amend!"):
			// prepare_amend_commit: an amend! commit gives its body only, so its subject is not
			// written twice.
			text += fixup.body
		default:
			text += fixup.message
		}
	case fileExists(paths.merge) || fileExists(paths.squash):
		// SQUASH_MSG, which a `merge --squash` leaves, and then MERGE_MSG, each where stat finds
		// it.
		for _, name := range []string{paths.squash, paths.merge} {
			if !fileExists(name) {
				continue
			}
			read, err := os.ReadFile(name)
			if err != nil {
				return d, false, nil
			}
			text += string(read)
		}
	case template.set:
		// git reads the template twice here: as the message, and again in template_untouched. A
		// template that is no regular file gives git's second read nothing, and template_untouched
		// then refuses nothing. A stash would give that read the whole text again, and git would
		// refuse a commit it accepts alone. So such a template is left for git to read (written is
		// false), and no line is added.
		read, status, err := readSource(template, templatePath, false, false)
		if status != readDone {
			return d, false, err
		}
		text += read
		fromTemplate, d.template, d.templateRead = true, read, true
	}
	if template.set && !d.templateRead && !verbatim {
		// template_untouched reads the template again, the one read git makes of it here. Under
		// verbatim the wrapper never uses that text, and it reads nothing: template_untouched
		// returns before it opens the template for a message that is not empty, and for an empty
		// one git reads the template itself, once. A template it cannot read refuses nothing.
		read, status, err := readSource(template, templatePath, false, templateWritten)
		switch status {
		case readDeclined:
			return d, false, err
		case readDone:
			d.template, d.templateRead = read, true
		}
	}
	if !verbatim && !fromTemplate {
		text = stripSpace(text)
	}
	if o.signoff {
		var ok bool
		if text, ok = b.withSignoff(text, cfg.comment); !ok {
			return d, false, nil
		}
	}
	if p.useEditor && includeStatus {
		text += "\n"
	}
	d.text = text
	return d, true, nil
}

// accepts is the message git makes of draft, and whether git alone accepts it. The message is
// draft's text after git's trailer step with the caller's own --trailer values, which git runs
// only when the caller gives one, and then cleaned as cleanup_message cleans it: the cut, then
// strbuf_stripspace, with the comment string under strip only. cmd_commit refuses a message in
// three ways, which --allow-empty-message turns off. message_is_empty refuses a message whose
// every line is blank or starts with `Signed-off-by: `, and under verbatim accepts any byte.
// template_untouched refuses a message equal to the template once both are cleaned. The third
// refuses an `amend!` commit with an empty body. ok is false where git refuses the message, where
// a read fails, and where a message that git cuts (the cut line under scissors or a verbose
// commit) holds the cut line.
func (b commitBuild) accepts(draft commitDraft, mode commitCleanupMode) (string, bool) {
	o := b.opts
	verbatim := mode == cleanupNone
	first := draft.text
	if len(o.trailers) > 0 {
		var err error
		if first, err = b.git.interpretTrailers(draft.text, o.trailers); err != nil {
			return "", false
		}
	}
	// verbose is -1 until a -v option, and -1 counts as verbose here: git's commit.verbose may turn
	// verbose on, and the wrapper does not read it.
	if (mode == cleanupScissors || o.verbose != 0) && strings.Contains(first, cutLine) {
		return "", false
	}
	message, err := b.clean(mode, first)
	if err != nil {
		return "", false
	}
	if o.allowEmptyMessage {
		return message, true
	}
	if messageIsEmpty(message, verbatim) {
		return "", false
	}
	// template_untouched accepts any message under verbatim here, since message_is_empty refused
	// an empty one above.
	if draft.templateRead && draft.template != "" && !verbatim {
		cleanTemplate, err := b.clean(mode, draft.template)
		if err != nil {
			return "", false
		}
		rest := message
		if strings.HasPrefix(message, cleanTemplate) {
			rest = message[len(cleanTemplate):]
		}
		if restIsEmpty(rest) {
			return "", false
		}
	}
	if o.fixup.set && strings.HasPrefix(message, "amend! ") && messageIsEmpty(message[commitSubjectLength(message):], verbatim) {
		return "", false
	}
	return message, true
}

// onlyAdds is each line of want that message lacks, when git's trailer step would change nothing
// but add those lines, and nil otherwise. message is what accepts made of text, the text git hands
// its trailer step. A line with the same key, in any case, and the same value is one the message
// holds: git reads a message's trailers with `interpret-trailers --parse`. A message that holds
// every line of want needs none, and then nothing more is read.
//
// Each --trailer has git run `git interpret-trailers --in-place --no-divider` over COMMIT_EDITMSG
// (amend_file_with_trailers, trailer.c), which adds each trailer that trailer.<key>.command
// configures and writes an existing trailer block again in git's own form. So the helper runs that
// step over text with the caller's --trailer values and then the lacking lines, and the result is
// cleaned. The lines are added only when that result is message and then exactly those lines,
// right after it, or after one blank line when message has no trailer block.
func (b commitBuild) onlyAdds(text, message string, want []gitTrailer, mode commitCleanupMode) []gitTrailer {
	held, err := b.git.trailers(message)
	if err != nil {
		return nil
	}
	var lacking []gitTrailer
	for _, line := range want {
		if !slices.ContainsFunc(held, line.sameAs) {
			lacking = append(lacking, line)
		}
	}
	if len(lacking) == 0 {
		return nil
	}
	values := slices.Clone(b.opts.trailers)
	block := ""
	for _, line := range lacking {
		values = append(values, line.String())
		block += line.String() + "\n"
	}
	second, err := b.git.interpretTrailers(text, values)
	if err != nil {
		return nil
	}
	withLines, err := b.clean(mode, second)
	if err != nil {
		return nil
	}
	// A message that ends in the middle of a line gets no blank line from the step's line feed,
	// only the end of that line, so the lines would join its last paragraph.
	newBlock := len(held) == 0 && (message == "" || strings.HasSuffix(message, "\n")) && withLines == message+"\n"+block
	if withLines != message+block && !newBlock {
		return nil
	}
	return lacking
}

// clean is text cleaned as cleanup_message cleans it after the cut, under mode: as it is under
// verbatim, by strbuf_stripspace with the comment string under strip (`git stripspace
// --strip-comments`), and by strbuf_stripspace with none under the others.
func (b commitBuild) clean(mode commitCleanupMode, text string) (string, error) {
	switch mode {
	case cleanupNone:
		return text, nil
	case cleanupAll:
		return b.git.stripComments(text)
	}
	return stripSpace(text), nil
}

// commitReadStatus is how readFile ended.
type commitReadStatus int

const (
	// readDone is a read of the whole text.
	readDone commitReadStatus = iota
	// readFailed is a read that fails, as git's own read of the source fails.
	readFailed
	// readDeclined is a source left unread for git. No trailer is added then.
	readDeclined
)

// readFile reads what git reads for the file option f at path, standard input when stdin is true.
// It reads a regular file that names no descriptor (namesADescriptor) and leaves it for git to read
// again. git cannot read any other source twice, so readFile keeps it in a stash (commitStash) and
// returns the stash when written is true: an argument names the source, and git reads it once. It
// declines such a source when written is false: no argument can name a stash of it, git reads it
// twice, or a stash is kept already (commitBuild.prepare). It declines such a source but standard
// input when the git to run is a script, which may close the stash's descriptor. It declines every
// such source when --interactive, -p or --pathspec-from-file is given, since git reads standard
// input for each before the message. The error is a stash it could not write after it read the
// source, and then it returns no stash.
func (b commitBuild) readFile(f commitFile, path string, stdin, written bool) (string, commitReadStatus, *commitStash, error) {
	if !stdin {
		info, err := os.Stat(path)
		if err != nil {
			return "", readFailed, nil, nil
		}
		if info.Mode().IsRegular() && !namesADescriptor(path) {
			read, err := os.ReadFile(path)
			if err != nil {
				return "", readFailed, nil, nil
			}
			return string(read), readDone, nil, nil
		}
	}
	if !written || !stdin && b.script || b.opts.interactive || b.opts.patch || b.opts.pathspecFromFile.set {
		return "", readDeclined, nil, nil
	}
	file, err := os.CreateTemp("", "legion-git-message-*")
	if err != nil {
		return "", readDeclined, nil, nil
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		return "", readDeclined, nil, nil
	}
	var read []byte
	if stdin {
		read, err = io.ReadAll(os.Stdin)
	} else {
		read, err = os.ReadFile(path)
	}
	if err != nil {
		file.Close()
		return "", readFailed, nil, nil
	}
	if _, err := file.Write(read); err != nil {
		file.Close()
		return "", readDeclined, nil, fmt.Errorf("keep the text git can no longer read: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return "", readDeclined, nil, fmt.Errorf("rewind the text kept for git: %w", err)
	}
	if !stdin {
		if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, 0); err != nil {
			file.Close()
			return "", readDeclined, nil, fmt.Errorf("keep the text git can no longer read open for git: %w", err)
		}
	}
	return string(read), readDone, &commitStash{file: file, stdin: stdin, arg: f.arg, at: f.at}, nil
}

// withSignoff is text with the line -s adds, as append_signoff (sequencer.c) adds it with no
// flag, which prepare_to_commit does. The line is `Signed-off-by: <committer>` and goes before
// the comment and blank lines at the end of text (ignored_log_message_bytes, commit.c). It goes
// nowhere when it is the whole message or the last line of the message's trailer block, right
// after a trailer block, and after a blank line otherwise. comment is the comment string git
// reads then. ok is false when the committer cannot be read and where the wrapper does not place
// the line as git does: a text that holds the cut line, a `---` line (has_conforming_footer reads
// the trailer block up to it), or a cherry-pick line, and a message whose last line continues the
// line before it.
func (b commitBuild) withSignoff(text, comment string) (string, bool) {
	if strings.Contains(text, cutLine) {
		return "", false
	}
	committer, err := b.git.committer()
	if err != nil {
		return "", false
	}
	sob := "Signed-off-by: " + committer + "\n"
	ignore := ignoredLogMessageBytes(text, comment)
	if ignore == 0 && text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	at := len(text) - ignore
	body := text[:at]
	if body == sob {
		return text, true
	}
	for line := range strings.Lines(body) {
		if rest, ok := strings.CutPrefix(line, "---"); ok && rest != "" && gitIsSpace(rest[0]) {
			return "", false
		}
	}
	// has_conforming_footer: whether the message has a trailer block, and whether the line is its
	// last entry. Every block holds a line with a separator, which `--only-trailers` prints, but
	// one made of cherry-pick lines alone.
	block, err := b.git.output(body, "interpret-trailers", "--only-trailers", "--only-input")
	if err != nil || block == "" && strings.Contains(body, "(cherry picked from commit ") {
		return "", false
	}
	gap := ""
	if block != "" {
		last := body[strings.LastIndexByte(strings.TrimSuffix(body, "\n"), '\n')+1:]
		if gitIsSpace(last[0]) {
			return "", false
		}
		if last == sob {
			return text, true
		}
	} else {
		switch {
		case at == 0:
			gap = "\n\n"
		case at == 1 || body[at-2] != '\n':
			gap = "\n"
		}
	}
	return body + gap + sob + text[at:], true
}

// ignoredLogMessageBytes is ignored_log_message_bytes (commit.c) for a text that holds no cut
// line: the length of the run of comment lines, blank lines and an old `Conflicts:` block at its
// end. As in git, a run that starts on the first line counts from the line after it.
func ignoredLogMessageBytes(text, comment string) int {
	boc, bol := 0, 0
	conflicts := false
	for line := range strings.Lines(text) {
		switch {
		case strings.HasPrefix(line, comment) || line[0] == '\n':
			if boc == 0 {
				boc = bol
			}
		case strings.HasPrefix(line, "Conflicts:\n"):
			conflicts = true
			if boc == 0 {
				boc = bol
			}
		case conflicts && line[0] == '\t':
		case boc != 0:
			boc, conflicts = 0, false
		}
		bol += len(line)
	}
	if boc == 0 {
		return 0
	}
	return len(text) - boc
}

// stripSpace is strbuf_stripspace (strbuf.c) with no comment string, as the whitespace and
// scissors cleanups and prepare_to_commit run it: each line loses the spaces at its end, a run of
// blank lines becomes one blank line, the blank lines at the start and the end go, and the last
// line ends with a line feed.
func stripSpace(text string) string {
	var out strings.Builder
	out.Grow(len(text) + 1)
	empties := 0
	for line := range strings.Lines(text) {
		line = trimGitSpace(line)
		if line == "" {
			empties++
			continue
		}
		if empties > 0 && out.Len() > 0 {
			out.WriteByte('\n')
		}
		empties = 0
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// fileExists is whether stat finds path, as prepare_to_commit tests MERGE_MSG and SQUASH_MSG.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return path != "" && err == nil
}

// gitIsSpace is git's own isspace (sane_ctype, ctype.c): space, tab, line feed and carriage
// return.
func gitIsSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// oneLine is the first line of s with its line feed, as get_one_line counts it.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i+1]
	}
	return s
}

// trimGitSpace is line without the spaces at its end, as is_blank_line trims it.
func trimGitSpace(line string) string {
	for line != "" && gitIsSpace(line[len(line)-1]) {
		line = line[:len(line)-1]
	}
	return line
}

// skipBlankLines is s from its first line that is not blank (skip_blank_lines, commit.c).
func skipBlankLines(s string) string {
	for {
		line := oneLine(s)
		if line == "" || trimGitSpace(line) != "" {
			return s
		}
		s = s[len(line):]
	}
}

// commitSubjectLength is the length of the first paragraph of body, its line feeds included
// (commit_subject_length, commit.c).
func commitSubjectLength(body string) int {
	p := 0
	for p < len(body) && len(skipBlankLines(body[p:])) == len(body)-p {
		p += len(oneLine(body[p:]))
	}
	return p
}

// restIsEmpty is whether every line of s is blank or starts with `Signed-off-by: ` (rest_is_empty,
// sequencer.c).
func restIsEmpty(s string) bool {
	for s != "" {
		line := oneLine(s)
		s = s[len(line):]
		if strings.HasPrefix(line, "Signed-off-by: ") {
			continue
		}
		if trimGitSpace(line) != "" {
			return false
		}
	}
	return true
}

// messageIsEmpty is message_is_empty (sequencer.c): under verbatim any byte is a message.
func messageIsEmpty(message string, verbatim bool) bool {
	if verbatim && message != "" {
		return false
	}
	return restIsEmpty(message)
}
