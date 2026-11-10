package leakguard

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// GhRequest is one gh call, as the caller is about to make it.
type GhRequest struct {
	// Args is the whole argv the caller would hand gh, gh's own name aside.
	Args []string
	// Stdin is what the caller would hand gh on its standard input, nil for nothing. It is read
	// only where the shared check asks for it, and what is read comes back from GhCall for the
	// caller to hand gh in its place.
	Stdin io.Reader
	// Repo is the destination repository, as "owner/name". Empty where the caller knows no
	// destination, which the guard reads as public (Guard.scanThrough).
	Repo string
}

// whatGhCall is what a refusal and a setup failure of this shape call the write. Both halves of
// the check — the first call and the one that follows it with the buffer — report the one call.
const whatGhCall = "the gh call"

// GhCall checks a gh call before it is made, in the shared check's gh-guard shape, and answers
// the standard input it read.
//
// The check reads a call that takes its body on standard input in two goes: the first call exits
// 4, meaning "this call reads standard input; buffer it and ask me again", and the second call is
// handed the buffer. The buffer is a regular file, never a pipe: the check reads it twice, and
// the check itself refuses a pipe for that reason. The bytes come back so the caller can hand
// them to gh, because by then this package has read the caller's standard input and the caller
// cannot read it again; a nil answer means standard input was never read and the caller's own is
// still whole.
func (g *Guard) GhCall(ctx context.Context, req GhRequest) ([]byte, error) {
	check, err := g.Resolve()
	if err != nil {
		return nil, &SetupFailure{What: whatGhCall, Why: whyNoCheck, Err: err}
	}
	argv := append([]string{check, "gh-guard"}, req.Args...)
	status, printed, err := g.run(ctx, child{argv: argv})
	if err != nil {
		return nil, &SetupFailure{What: whatGhCall, Why: "the shared public leak check could not be run", Err: err}
	}
	var buffered []byte
	if status == checkWantsStdin {
		buffered, status, printed, err = g.askAgain(ctx, argv, req)
		if err != nil {
			return buffered, err
		}
	}
	if err := readStatus(whatGhCall, status, printed); err != nil {
		return buffered, err
	}
	public, err := g.scanThrough(ctx, whatGhCall, req.Repo)
	if err != nil {
		return buffered, err
	}
	if !public {
		return buffered, nil
	}
	records := []Record{{Label: "the gh call's arguments", Text: strings.Join(req.Args, " ")}}
	if len(buffered) > 0 {
		records = append(records, Record{Label: "the gh call's standard input", Text: string(buffered)})
	}
	return buffered, g.rules(ctx, whatGhCall, records)
}

// askAgain buffers the caller's standard input to a regular file and asks the check again, with
// that file as a descriptor of the child's and PUBLIC_LEAK_CHECK_STDIN naming it. The file is the
// test's and the pod's own temporary directory's, and it is gone before this returns: the bytes
// go back to the caller in memory, which is where they came from.
func (g *Guard) askAgain(ctx context.Context, argv []string, req GhRequest) ([]byte, int, string, error) {
	buffered := []byte{}
	if req.Stdin != nil {
		read, err := io.ReadAll(req.Stdin)
		if err != nil {
			return nil, 0, "", &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be read", Err: err}
		}
		buffered = read
	}
	file, err := os.CreateTemp("", "leakguard-stdin-")
	if err != nil {
		return buffered, 0, "", &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be buffered", Err: err}
	}
	defer func() {
		file.Close()
		os.Remove(file.Name())
	}()
	if _, err := file.Write(buffered); err != nil {
		return buffered, 0, "", &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be buffered", Err: err}
	}
	// The child's descriptor is a dup of this one and shares its offset, so the file is rewound
	// here and not by whoever reads it.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return buffered, 0, "", &SetupFailure{What: whatGhCall, Why: "the call's standard input could not be buffered", Err: err}
	}
	// ExtraFiles' first entry is the child's descriptor 3, which is the one named here.
	const buffer = 3
	status, printed, err := g.run(ctx, child{
		argv:  argv,
		files: []*os.File{file},
		env: []string{
			fmt.Sprintf("PUBLIC_LEAK_CHECK_STDIN=/dev/fd/%d", buffer),
			// The check's own advice on exit 4 is for a caller that cannot buffer. This one
			// just did, so the advice is turned off.
			"PUBLIC_LEAK_CHECK_STDIN_BUFFERS=1",
		},
	})
	if err != nil {
		return buffered, 0, "", &SetupFailure{What: whatGhCall, Why: "the shared public leak check could not be run", Err: err}
	}
	if status == checkWantsStdin {
		return buffered, 0, printed, &SetupFailure{
			What: whatGhCall,
			Why:  "the shared public leak check asked for standard input again after it was given",
			Err:  printedError(printed),
		}
	}
	return buffered, status, printed, nil
}
