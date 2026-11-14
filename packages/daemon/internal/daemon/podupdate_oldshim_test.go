package daemon

// The push check through a shim built before the issue's one rescue branch: the fake pod runtime
// stands in for that shim, which reads the request as it decodes the frame and reports by its own
// rule, over a remote that holds the rescue branches of the pods the issue had before.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sjawhar/envoy/dispatchbody"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/rescue"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// olderShimRequest is the push check's request as a shim built before the issue's one rescue
// branch decodes the frame: shimwire.RescueUnpushed's four fields at 9542c6fbbe, with no target.
type olderShimRequest struct {
	ID                 string `json:"id"`
	Branch             string `json:"branch"`
	WorkspaceTimeoutMs int    `json:"workspaceTimeoutMs"`
	TimeoutMs          int    `json:"timeoutMs"`
}

// olderShimWorkspace is the one workspace on the disk of the older shim's pod.
const olderShimWorkspace = "/legion/workspaces/sjawhar/legion/legion-484"

// olderShimRescue is that shim's answer to request over a disk whose one workspace's remote holds
// remote (branch name to commit). The request crosses as stream.Conn sends it: the frame built
// from it, refused as Validate refuses it, then encoded, and decoded into the shim's four fields.
// The report follows that shim's rule (internal/rescue's check at 9542c6fbbe): every remote branch
// whose name starts with `<branch>-`, in ls-remote's order, plus each branch it pushed. The
// workspace holds nothing its remote lacks, so the shim pushes nothing and the second half is
// empty.
func olderShimRescue(request runtime.RescueRequest, remote map[string]string) (olderShimRequest, []runtime.WorkspaceRescue, error) {
	frame := shimwire.RescueUnpushed{ID: "older-shim", Branch: request.Branch, Target: request.Target,
		WorkspaceTimeoutMs: int(request.WorkspaceTimeout.Milliseconds()), TimeoutMs: int(request.Timeout.Milliseconds())}
	if err := frame.Validate(); err != nil {
		return olderShimRequest{}, nil, err
	}
	line, err := json.Marshal(frame)
	if err != nil {
		return olderShimRequest{}, nil, err
	}
	var read olderShimRequest
	if err := json.Unmarshal(line, &read); err != nil {
		return olderShimRequest{}, nil, err
	}
	w := shimwire.RescueWorkspace{Dir: olderShimWorkspace, Status: rescue.Clean}
	for _, name := range slices.Sorted(maps.Keys(remote)) {
		if strings.HasPrefix(name, read.Branch+"-") {
			w.Branches = append(w.Branches, shimwire.RescueBranch{Name: name, Commit: remote[name]})
		}
	}
	if len(w.Branches) > 0 {
		w.Status = rescue.Rescued
	}
	return read, rescue.Report([]shimwire.RescueWorkspace{w}), nil
}

// A pod whose shim predates the issue's one rescue branch reports its own pod's rescue branch and
// no other. Such a shim reads only the request's branch, and reports every remote branch whose
// name starts with `<branch>-` as its own, while the issue's remote holds the rescue branches of
// every pod the issue had before. So the request keeps the pod's own prefix in branch and names
// the one branch in target, which that shim ignores, and the report and the issue's message name
// the pod's own branch alone. With the one branch's name in branch, the shim would report every
// earlier pod's branches, and the message would tell the issue to merge work that is not this
// pod's.
func TestPodUpdateRescueThroughAnOlderShimNamesOnlyItsOwnPodsBranch(t *testing.T) {
	own := podUpdateRescueBranch + "-1"
	earlier := []string{
		"legion/" + podUpdateIssue + "-rescue-" + podUpdatePreviousPod[:8] + "-1",
		"legion/" + podUpdateIssue + "-rescue-" + podUpdatePreviousPod[:8] + "-2",
		"legion/" + podUpdateIssue + "-rescue-2b7e4c19-1",
		"legion/" + podUpdateIssue + "-rescue-6d05a8f3-1",
	}
	remote := map[string]string{"main": fmt.Sprintf("%040x", 1), claim.IssueBranch(podUpdateIssue): fmt.Sprintf("%040x", 2)}
	for i, name := range append([]string{own}, earlier...) {
		remote[name] = fmt.Sprintf("%040x", 16+i)
	}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.rt.RescueWith(func(_ context.Context, _ runtime.Known, request runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
		_, report, err := olderShimRescue(request, remote)
		return report, err
	})
	rig.answeredRound(claim.RoleImplementer)

	rig.awaitReplacement(1, "once the implementer answered and its older shim's check passed")
	checks := rig.rt.CallsOf("RescueUnpushed")
	if len(checks) != 1 {
		t.Fatalf("the runtime was asked for %d push checks, want one for the one replacement", len(checks))
	}
	// The stand-in is a function of the request alone, so this is the read and the report it
	// answered the daemon with.
	read, report, err := olderShimRescue(checks[0].Rescue, remote)
	if err != nil {
		t.Fatalf("the older shim refused the request %+v: %v", checks[0].Rescue, err)
	}
	var reported []string
	for _, w := range report {
		for _, b := range w.Branches {
			reported = append(reported, b.Name)
		}
	}
	messages := rig.messages()
	told := strings.Join(messages, "\n")
	var extraReported, extraTold []string
	for _, name := range earlier {
		if slices.Contains(reported, name) {
			extraReported = append(extraReported, name)
		}
		if strings.Contains(told, "`"+name+"`") {
			extraTold = append(extraTold, name)
		}
	}
	if len(extraReported) > 0 || len(extraTold) > 0 {
		t.Errorf("the older shim reported the earlier pods' rescue branches %v and the issue's message named %v, want neither: the shim read branch %q as the prefix of its own",
			extraReported, extraTold, read.Branch)
	}
	if !slices.Equal(reported, []string{own}) {
		t.Errorf("the older shim reported %v, want its own pod's %s alone", reported, own)
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "`"+own+"` (commit "+remote[own]+")") {
		t.Errorf("the issue was told %q, want one message naming %s at %s", messages, own, remote[own])
	}
	target := "legion/" + podUpdateIssue + "-rescue"
	if got := checks[0].Rescue; got.Branch != podUpdateRescueBranch || got.Target != target {
		t.Errorf("the push check was asked for branch %q and target %q, want the pod's own prefix %q and the issue's one branch %q",
			got.Branch, got.Target, podUpdateRescueBranch, target)
	}
	if read.Branch != podUpdateRescueBranch {
		t.Errorf("the older shim read branch %q, want the pod's own prefix %q", read.Branch, podUpdateRescueBranch)
	}
}

// An older shim's report of many per-pod branches is told in a summary that Dispatch takes in one
// post, and the message links the full list. Such a shim reports no commits, so the message
// names how many branches it reported and the first 20 of them with their commits, and the
// document it links lists all 217.
func TestPodUpdateRescueThroughAnOlderShimOfManyBranchesFitsOnePostAndLinksThemAll(t *testing.T) {
	remote := map[string]string{"main": fmt.Sprintf("%040x", 1), claim.IssueBranch(podUpdateIssue): fmt.Sprintf("%040x", 2)}
	var own []string
	for n := 1; n <= 217; n++ {
		name := fmt.Sprintf("%s-%d", podUpdateRescueBranch, n)
		own = append(own, name)
		remote[name] = fmt.Sprintf("%040x", 1000+n)
	}
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.rt.RescueWith(func(_ context.Context, _ runtime.Known, request runtime.RescueRequest) ([]runtime.WorkspaceRescue, error) {
		_, report, err := olderShimRescue(request, remote)
		return report, err
	})
	rig.answeredRound(claim.RoleImplementer)

	rig.awaitReplacement(1, "once the implementer answered and its older shim's check passed")
	post := rig.rescuePost()
	if units := utf8.RuneCountInString(post.Body); units >= dispatchbody.BlockUnits {
		t.Fatalf("the rescue message is %d characters, want fewer than one Dispatch block's %d", units, dispatchbody.BlockUnits)
	}
	if pairs := strings.Count(post.Body, "` (commit "); pairs != rescueListed {
		t.Errorf("the rescue message names %d branches with their commits, want the first %d:\n%s", pairs, rescueListed, post.Body)
	}
	for _, want := range []string{"217 rescue branches", "and 197 more", "Merge each of these branches into the issue branch, or delete the branch"} {
		if !strings.Contains(post.Body, want) {
			t.Errorf("the rescue message does not say %q:\n%s", want, post.Body)
		}
	}
	var missing []string
	for _, name := range own {
		if !strings.Contains(post.Artifact.Content, "repository: sjawhar/legion\nbranch: "+name+"\ntip: "+remote[name]+"\n") {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the message's document lacks %d of the 217 branches: %v\n%s", len(missing), missing, post.Artifact.Content)
	}
}
