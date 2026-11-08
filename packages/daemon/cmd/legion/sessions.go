package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/ompsessions"
	"github.com/sjawhar/legion/daemon/internal/runtime/sandbox"
)

// sessionsCommands is `legion sessions`, the operator's hand on Oh My Pi's session database
// (either database kind of runtime.kubernetes.session_store, postgres or cloudsql-iam), and the one
// list of its subcommands.
var sessionsCommands = map[string]command{
	"import": runSessionsImport,
}

func runSessions(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "sessions", sessionsCommands, args, stdout, stderr)
}

const sessionsImportUsage = "usage: legion sessions import --dsn-file <file> --claims <file|-> [--tree <KEY>] [--tree-volume <dir>]"

// sessionsInput is what `legion sessions import` reads first: --dsn-file and --claims, registered
// on its flag set by addSessionsInput.
type sessionsInput struct {
	dsnFile, claimsFile *string
}

func addSessionsInput(flags *flag.FlagSet) sessionsInput {
	return sessionsInput{
		dsnFile:    flags.String("dsn-file", "", "file holding the session database's postgres:// URL (required)"),
		claimsFile: flags.String("claims", "", "the claims `legion claims list --json` printed, or - for stdin (required)"),
	}
}

// open refuses a positional argument or a missing required flag, its own two, as a usage error,
// then reads the claims list and connects to the session database. code is the command's exit
// when conn is nil.
func (in sessionsInput) open(ctx context.Context, flags *flag.FlagSet, stderr io.Writer) (claims []importedClaim, conn *pgx.Conn, code int) {
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "legion sessions import: unexpected argument %q\n%s\n", flags.Arg(0), sessionsImportUsage)
		return nil, nil, 2
	}
	for _, pair := range [][2]string{{"dsn-file", *in.dsnFile}, {"claims", *in.claimsFile}} {
		if pair[1] == "" {
			fmt.Fprintf(stderr, "legion sessions import: --%s is required\n%s\n", pair[0], sessionsImportUsage)
			return nil, nil, 2
		}
	}
	claims, err := readImportedClaims(*in.claimsFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return nil, nil, 1
	}
	conn, err = ompsessions.Connect(ctx, *in.dsnFile)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return nil, nil, 1
	}
	return claims, conn, 0
}

// importedClaim is what `legion sessions import` reads of one claim `legion claims list --json`
// prints: its token, tree, recorded session and recorded session file. Every other member is the
// daemon's and ignored, so the list an earlier release prints reads alike.
type importedClaim struct {
	Token       string `json:"token"`
	Tree        string `json:"tree"`
	Session     string `json:"session"`
	SessionFile string `json:"sessionFile"`
}

// runSessionsImport is `legion sessions import`: the one-off copy of every recorded session file
// into the session table, before a deployment that kept its sessions as files on its tree volumes
// turns on a runtime.kubernetes.session_store that keeps sessions in a database, and before those
// volumes go. It reads the claims `legion claims list --json` printed (--claims, `-` for stdin),
// each claim of --tree alone when given, and copies each claim's session file into the table whose
// URL --dsn-file holds, under the path the claim recorded, which is the key Oh My Pi resumes the
// claim's session by (ompsessions.Import). The file is read at that path, or, with --tree-volume,
// from the tree volume mounted there (sandbox.SessionOnVolume). A closed claim whose resume path was
// cleared but whose session id remains is found under the mounted volume's sessions directory by
// that id. It prints one line per claim it copies, then one line for every claim of the whole list,
// whatever --tree says, whose recorded session the table does not hold (missingSessions), then a
// count of each outcome. It exits 1 when any claim's session could not be copied (a file it cannot
// read, several files for one closed claim's id, or a session the table already holds with other
// content) or a recorded session of the claims it was asked to copy is missing from the table; a
// missing one of another tree is reported and leaves the exit as it is. A rerun copies nothing
// twice: a session the table already holds byte for byte is reported as copied before.
func runSessionsImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("sessions import", sessionsImportUsage, stderr)
	input := addSessionsInput(flags)
	tree := flags.String("tree", "", "copy the sessions of this tree's claims alone")
	treeVolume := flags.String("tree-volume", "", "read each session from the tree volume mounted here rather than at its recorded path")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	claims, conn, code := input.open(ctx, flags, stderr)
	if conn == nil {
		return code
	}
	defer conn.Close(context.Background())

	selected := func(c importedClaim) bool { return *tree == "" || c.Tree == *tree }
	counts := map[string]int{}
	matched := 0
	for _, c := range claims {
		if !selected(c) {
			continue
		}
		matched++
		outcome, file, err := importClaimSession(ctx, conn, c, *treeVolume)
		if file == "" {
			fmt.Fprintf(stdout, "%s recorded no session: nothing to copy\n", c.Token)
			counts["nothing recorded"]++
			continue
		}
		var conflict *ompsessions.ConflictError
		var ambiguous *sessionFileAmbiguityError
		switch {
		case errors.As(err, &conflict), errors.As(err, &ambiguous):
			fmt.Fprintf(stdout, "%s refused %s: %v\n", c.Token, file, err)
			counts["refused"]++
		case err != nil:
			fmt.Fprintf(stdout, "%s failed %s: %v\n", c.Token, file, err)
			counts["failed"]++
		case outcome.Outcome == ompsessions.Copied:
			fmt.Fprintf(stdout, "%s copied %s (%s)\n", c.Token, file, outcome.Summary)
			counts["copied"]++
		default:
			fmt.Fprintf(stdout, "%s copied before %s (identical, %s)\n", c.Token, file, outcome.Summary)
			counts["copied before"]++
		}
	}

	if matched == 0 && *tree != "" {
		fmt.Fprintf(stderr, "legion sessions import: the claims list %s has no claim of %s\n", *input.claimsFile, *tree)
		return 1
	}
	missing, err := missingSessions(ctx, conn, claims)
	if err != nil {
		fmt.Fprintf(stderr, "legion sessions import: %v\n", err)
		return 1
	}
	missingHere := 0
	for _, c := range missing {
		fmt.Fprintf(stdout, "%s missing %s: the session table holds no such session (tree %s)\n", c.Token, c.SessionFile, c.Tree)
		if selected(c) {
			missingHere++
		}
	}
	counts["missing"] = len(missing)
	var summary []string
	for _, outcome := range []string{"copied", "copied before", "nothing recorded", "failed", "refused", "missing"} {
		summary = append(summary, fmt.Sprintf("%s %d", outcome, counts[outcome]))
	}
	fmt.Fprintf(stdout, "legion sessions import: %s\n", strings.Join(summary, ", "))
	if counts["failed"] > 0 || counts["refused"] > 0 || missingHere > 0 {
		return 1
	}
	return 0
}

// sessionFileAmbiguityError names the several files that could hold one closed
// claim's recorded session. Choosing one would make the import non-reproducible.
type sessionFileAmbiguityError struct {
	session string
	files   []string
}

func (e *sessionFileAmbiguityError) Error() string {
	return fmt.Sprintf("session %s has multiple files: %s", e.session, strings.Join(e.files, ", "))
}

// importClaimSession imports c's recorded file, or finds the one file of its
// retained session id when issue_close has already cleared the resume path.
func importClaimSession(ctx context.Context, conn *pgx.Conn, c importedClaim, treeVolume string) (importedSession, string, error) {
	if c.SessionFile != "" {
		outcome, err := importSession(ctx, conn, c.SessionFile, treeVolume)
		return outcome, c.SessionFile, err
	}
	if c.Session == "" {
		return importedSession{}, "", nil
	}
	volume := treeVolume
	if volume == "" {
		volume = sandbox.TreeRoot
	}
	source, err := sessionFileForID(c.Session, volume)
	if err != nil {
		return importedSession{}, "session " + c.Session, err
	}
	outcome, err := importSession(ctx, conn, source, volume)
	return outcome, source, err
}

// sessionFileForID finds the session a closed claim recorded, under the mounted
// tree volume or the default legacy volume root. Oh My Pi writes one session
// file per working-directory folder as `<timestamp>_<session>.jsonl`.
func sessionFileForID(session, treeVolume string) (string, error) {
	if !claudelaunch.IsSessionID(session) {
		return "", fmt.Errorf("recorded session %q is not a UUID", session)
	}
	root := treeVolume
	if root == "" {
		root = sandbox.TreeRoot
	}
	sessions := filepath.Join(root, sandbox.SessionsSubPath)
	files, err := filepath.Glob(filepath.Join(sessions, "*", "*_"+session+".jsonl"))
	if err != nil {
		return "", fmt.Errorf("find recorded session %s: %w", session, err)
	}
	for i, file := range files {
		cleaned := filepath.Clean(file)
		relative, err := filepath.Rel(sessions, cleaned)
		if err != nil || !filepath.IsLocal(relative) {
			return "", fmt.Errorf("found recorded session %s outside %s", session, sessions)
		}
		files[i] = cleaned
	}
	switch len(files) {
	case 0:
		return "", fmt.Errorf("recorded session %s was not found under %s", session, sessions)
	case 1:
		return sandbox.SessionFromVolume(files[0], root)
	default:
		return "", &sessionFileAmbiguityError{session: session, files: files}
	}
}

// missingSessions are the claims of the list, every tree's, that record a session the table does
// not hold: under SQL storage each would fail every launch until it is copied or marked lost.
func missingSessions(ctx context.Context, conn *pgx.Conn, claims []importedClaim) ([]importedClaim, error) {
	var missing []importedClaim
	for _, c := range claims {
		if c.SessionFile == "" {
			continue
		}
		_, found, err := ompsessions.Written(ctx, conn, c.SessionFile)
		if err != nil {
			return nil, err
		}
		if !found {
			missing = append(missing, c)
		}
	}
	return missing, nil
}

// readImportedClaims reads the claims list from file, or from stdin when file is `-`, in token
// order.
func readImportedClaims(file string) ([]importedClaim, error) {
	var body []byte
	var err error
	if file == "-" {
		body, err = io.ReadAll(os.Stdin)
	} else {
		body, err = os.ReadFile(file)
	}
	if err != nil {
		return nil, fmt.Errorf("read the claims list %s: %w", file, err)
	}
	var list struct {
		Claims *[]importedClaim `json:"claims"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("the claims list %s is not what `legion claims list --json` prints: %w", file, err)
	}
	if list.Claims == nil {
		return nil, fmt.Errorf("the claims list %s has no claims member, which `legion claims list --json` prints", file)
	}
	claims := *list.Claims
	slices.SortFunc(claims, func(a, b importedClaim) int { return strings.Compare(a.Token, b.Token) })
	return claims, nil
}

// importedSession is what importSession did with one session, and the content it read.
type importedSession struct {
	Outcome ompsessions.Outcome
	Summary ompsessions.Summary
}

// importSession copies the session recorded at file into the table: read at file itself, or from
// the tree volume mounted at treeVolume when one is given, with the file's modification time.
func importSession(ctx context.Context, conn *pgx.Conn, file, treeVolume string) (importedSession, error) {
	source, root := file, ""
	if treeVolume != "" {
		onVolume, err := sandbox.SessionOnVolume(file, treeVolume)
		if err != nil {
			return importedSession{}, err
		}
		source = onVolume
		root = treeVolume
	}
	content, info, err := readSessionFile(source, root)
	if err != nil {
		return importedSession{}, fmt.Errorf("read the session file: %w", err)
	}
	outcome, err := ompsessions.Import(ctx, conn, file, content, info.ModTime())
	if err != nil {
		return importedSession{}, err
	}
	return importedSession{Outcome: outcome, Summary: ompsessions.Summarize(content)}, nil
}

// ignoringEINTR calls fn until the error it answers with is not EINTR, and returns that error. A
// signal can interrupt any open or fstat of the walk, and the rooted-directory API this walk
// replaced retried each of its own calls the same way (src/os/root_unix.go).
func ignoringEINTR(fn func() error) error {
	for {
		err := fn()
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// openNoFollow opens name in the directory the descriptor parent holds, refusing a symbolic link
// at name with ELOOP, and retries the EINTR an open may answer with. Every component of
// openSessionFileBeneath's walk and its leaf are opened through it, so the flag set is written
// once and a later change to it cannot reach only one of them.
func openNoFollow(parent int, name string) (int, error) {
	var fd int
	err := ignoringEINTR(func() error {
		var err error
		fd, err = unix.Openat(parent, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		return err
	})
	return fd, err
}

// openSessionFileBeneath opens the session file at relative, the path below the sessions
// directory of the tree volume mounted at volume, one component at a time, and returns the leaf's
// open file for the caller to close.
//
// The walk's one open by a path is volume itself, the mount the operator named on --tree-volume:
// a link there is the operator's own, so it is followed. Every directory below it, the sessions
// directory first, was writable by every agent of a legion-v10.0.0 deployment, so any of them may
// be a link an agent left pointing off the volume; each is therefore opened from the descriptor
// of the one before it with O_NOFOLLOW and refused when it is a link.
//
// The flag set is O_RDONLY|O_NOFOLLOW rather than O_PATH, which is Linux's alone while legion is
// released for darwin/amd64 and darwin/arm64. Measured on 2026-11-08 under gVisor 4.19.0, the
// kernel every Legion pod runs: openat of a link to a directory with
// O_RDONLY|O_NOFOLLOW|O_NONBLOCK|O_CLOEXEC answers ELOOP, as it does under an ordinary Linux
// kernel, while Go's rooted-directory API follows that link out of its root. O_NONBLOCK stops the
// open of a named pipe from waiting for a writer: measured in this pod on 2026-11-08, a FIFO at
// the leaf is refused in 0.47 s with the flag, and the same open blocks without end without it.
//
// A component's kind comes from fstat of the handle just opened rather than from a second reading
// of the name, so a rename between the check and the open cannot change what was opened. Every
// open and every fstat runs through ignoringEINTR, keeping the behaviour of the loop Go's
// rooted-directory API wraps each of its own opens in (src/os/root_unix.go).
func openSessionFileBeneath(volume, relative string) (*os.File, error) {
	sessions := filepath.Join(volume, sandbox.SessionsSubPath)
	if !filepath.IsLocal(relative) {
		return nil, fmt.Errorf("%s is not under the sessions directory %s", filepath.Join(sessions, relative), sessions)
	}
	if relative == "." {
		return nil, fmt.Errorf("%s is the sessions directory itself, not a session file", sessions)
	}
	var parent int
	if err := ignoringEINTR(func() error {
		var err error
		parent, err = unix.Open(volume, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		return err
	}); err != nil {
		return nil, fmt.Errorf("the tree volume %s cannot be opened: %w", volume, err)
	}
	// The walk starts at the volume, so the sessions directory's own components come first.
	// relative is filepath.Rel's output, which is clean, and IsLocal has refused an absolute, an
	// empty and an escaping one, so splitting on the separator yields the components themselves.
	components := append(strings.Split(sandbox.SessionsSubPath, string(filepath.Separator)), strings.Split(relative, string(filepath.Separator))...)
	walked := volume
	for _, name := range components[:len(components)-1] {
		walked = filepath.Join(walked, name)
		directory, err := openNoFollow(parent, name)
		unix.Close(parent)
		if err != nil {
			switch {
			case errors.Is(err, unix.ELOOP):
				return nil, fmt.Errorf("%s is a symbolic link", walked)
			case errors.Is(err, unix.ENOENT):
				return nil, fmt.Errorf("%s is not on the tree volume", walked)
			}
			return nil, fmt.Errorf("%s cannot be opened: %w", walked, err)
		}
		var st unix.Stat_t
		if err := ignoringEINTR(func() error { return unix.Fstat(directory, &st) }); err != nil {
			unix.Close(directory)
			return nil, fmt.Errorf("%s cannot be opened: %w", walked, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			unix.Close(directory)
			return nil, fmt.Errorf("%s is not a directory", walked)
		}
		parent = directory
	}
	leaf := components[len(components)-1]
	path := filepath.Join(walked, leaf)
	file, err := openNoFollow(parent, leaf)
	unix.Close(parent)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%s is a symbolic link", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// The name carried on the handle is what a later Stat or read error prints, so it is the leaf's
	// own path on the tree volume.
	return os.NewFile(uintptr(file), path), nil
}

// readSessionFile reads one regular session file. When root names a mounted tree volume,
// openSessionFileBeneath walks source beneath that volume's sessions directory one descriptor at
// a time, rather than through Go's rooted-directory API, which under a gVisor kernel follows a
// directory symlink out of its root, so a path or symlink cannot escape between validation and
// the read.
func readSessionFile(source, root string) ([]byte, os.FileInfo, error) {
	var (
		opened *os.File
		err    error
	)
	if root == "" {
		opened, err = os.OpenFile(source, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, errors.New("refuses a symbolic link")
		}
	} else {
		// The path is taken relative to the sessions directory rather than to the volume: against
		// the volume, a source under `sessions/..` would look local and the walk would climb out.
		sessions := filepath.Join(root, sandbox.SessionsSubPath)
		var relative string
		relative, err = filepath.Rel(sessions, source)
		if err != nil {
			return nil, nil, fmt.Errorf("%s is not under the sessions directory %s", source, sessions)
		}
		opened, err = openSessionFileBeneath(root, relative)
	}
	if err != nil {
		return nil, nil, err
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.New("is not a regular file")
	}
	content, err := io.ReadAll(opened)
	if err != nil {
		return nil, nil, err
	}
	return content, info, nil
}
