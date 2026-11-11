package capabilities

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/podsafety"
)

// overlaysVariable is Oh My Pi's list of settings overlays.
const overlaysVariable = "PI_CONFIG_FILES"

// OverlayEntries are the overlays a PI_CONFIG_FILES value names, in order, its empty entries left
// out as Oh My Pi leaves them out (`a.yml:` and `a.yml::b.yml` start Oh My Pi on the files alone).
func OverlayEntries(value string) []string {
	return slices.DeleteFunc(strings.Split(value, ":"), func(entry string) bool { return entry == "" })
}

// CheckOverlays is whether img's Oh My Pi starts on overlays, a role's whole PI_CONFIG_FILES in
// order, with each of own, the role's own overlays among them, also read alone: refused names the
// first overlay it would not start on and why — a file this process cannot read
// (podsafety.CheckOverlay), or `<Launch> config get retry.modelFallback --json` exiting non-zero
// under PI_CONFIG_FILES set to that file alone, then to the whole list, with Oh My Pi's own error
// line — and is "" when it starts. err joins the runs that never reached an exit of their own (Oh
// My Pi would not start at all, or timed out), which say nothing of the overlays: such a run does
// not stop the check, every later run still goes, and a refusal found after one is still refused;
// a caller reports err rather than refuse on it. img.Env's own PI_CONFIG_FILES is replaced on each
// run.
func CheckOverlays(ctx context.Context, img Image, overlays, own []string) (refused string, err error) {
	for _, file := range overlays {
		if err := podsafety.CheckOverlay(file); err != nil {
			return fmt.Sprintf("%s cannot be read: %v", file, err), nil
		}
	}
	run := func(list []string) (string, error) {
		const args = "config get retry.modelFallback --json"
		r := img.withOverlays(list).launch(ctx, args)
		switch {
		case r.err != nil:
			return "", fmt.Errorf("%s %s under PI_CONFIG_FILES %s: %w", img.Launch, args, strings.Join(list, ":"), r.err)
		case r.exit != 0:
			return errorLine(r), nil
		}
		return "", nil
	}
	var unanswered []error
	for _, file := range own {
		why, err := run([]string{file})
		if why != "" {
			return fmt.Sprintf("Oh My Pi refuses %s: %s", file, why), errors.Join(unanswered...)
		}
		if err != nil {
			unanswered = append(unanswered, err)
		}
	}
	why, err := run(overlays)
	if err != nil {
		unanswered = append(unanswered, err)
	}
	if why != "" {
		why = fmt.Sprintf("Oh My Pi refuses PI_CONFIG_FILES %s: %s", strings.Join(overlays, ":"), why)
	}
	return why, errors.Join(unanswered...)
}

// OverlayReads is what a reader of whole PI_CONFIG_FILES values has already asked Oh My Pi
// through CheckOverlays, so that it asks each question once: every whole value it has read, by
// the value's `:`-joined form, and every overlay it has read alone. Oh My Pi answers one file
// the same whichever value that file stands in, and one value the same whichever role holds it,
// so a value two roles or two arms produce alike is read once, and a role's own files are read
// alone once however many arms stand them in. A deployment that declares arms would otherwise
// pay for the thing it added at every value of every role.
//
// The zero value has read nothing, and both readers of these values hold one of their own: the
// boot gate, which reads each value of each role (internal/daemon's checkPaneRoleOverlays), and
// the image probe, which reads the values the manifest passed it (cmd/legion's
// checkRoleOverlays). Values are offered in the caller's own order, so the first role and the
// first value holding what Oh My Pi refuses is still the one its refusal names.
type OverlayReads struct {
	values map[string]struct{}
	alone  map[string]struct{}
}

// Unread takes the next whole value: alone is the overlays of it that no read has read on their
// own yet, which is the list CheckOverlays reads alone, and unread is false for a whole value
// read before, which its caller passes over.
func (r *OverlayReads) Unread(value []string) (alone []string, unread bool) {
	if r.values == nil {
		r.values, r.alone = map[string]struct{}{}, map[string]struct{}{}
	}
	joined := strings.Join(value, ":")
	if _, read := r.values[joined]; read {
		return nil, false
	}
	r.values[joined] = struct{}{}
	alone = make([]string, 0, len(value))
	for _, file := range value {
		if _, read := r.alone[file]; read {
			continue
		}
		r.alone[file] = struct{}{}
		alone = append(alone, file)
	}
	return alone, true
}

// withOverlays is img with PI_CONFIG_FILES set to list alone.
func (img Image) withOverlays(list []string) Image {
	env := slices.DeleteFunc(slices.Clone(img.Env), func(pair string) bool { return strings.HasPrefix(pair, overlaysVariable+"=") })
	img.Env = append(env, overlaysVariable+"="+strings.Join(list, ":"))
	return img
}

// errorLine is Oh My Pi's own reason for a failed run: the last line of its output beginning
// `error:` (Oh My Pi prints a source excerpt before it, which a cut of the whole output would keep
// in its place), else the run's failure as it printed it.
func errorLine(r ran) string {
	lines := strings.Split(r.stderr+"\n"+r.stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "error:") {
			return oneLine(line)
		}
	}
	return r.failure()
}
