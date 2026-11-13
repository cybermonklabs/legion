package rescue_test

// The generated matrix of the differential (differential_test.go). The rows there are the shapes
// reviews found, one at a time; this test builds its cases from the dimensions one path of a
// worktree jj holds varies along, rather than from what a reader thought of, and holds the two
// reads to the same rules over each of them.
//
// The dimensions:
//
//   - The index state of the path in the worktree's index copy, 13 of them: absent; a regular
//     entry; an intent-to-add entry, a file new in @ that a role's `jj status` snapshotted; an
//     entry marked assume-unchanged; one marked skip-worktree; a gitlink; an entry forced into
//     the index with `git add -f` under an ignore rule (the workspace's .gitignore holds
//     `/forced.*/p`); a gitlink marked assume-unchanged; a gitlink marked skip-worktree; a
//     symlink, at mode 120000; an executable file, at mode 100755; a gitlink staged at another
//     commit than HEAD, a bump `git add` staged after a commit inside the repository; and a
//     gitlink HEAD holds that `git rm --cached` took out of the index.
//   - The disk state at the path, 15 of them: absent; the same content as the record; new
//     content of the same size, written in a later second than the record and its mtime then set
//     back to the record's, as an archive tool or `touch -r` leaves it; new content of the same
//     size whose mtime the write moved, the rewrite a role makes, which must be equal; new
//     content of another size; the execute bit set (chmod +x) and cleared (chmod -x), the mode
//     alone changed; new content of another size at a path an ignore rule covers (the
//     workspace's .gitignore holds `/*.ignored/p`, written after every jj command and every
//     record, so a record stands as it is made and the rule then covers it), which is a new
//     untracked file the rule covers where the path has no record; a directory holding a file; a
//     directory holding a .git with a commit; a directory holding a .git without one; a directory
//     holding a .jj; a symlink to a file; a symlink to a directory; and the path's parent
//     replaced by a symlink to a directory that holds the path.
//   - The worktree's git HEAD: born or unborn.
//   - The worktree's index file: present or missing.
//
// HEAD and the index file belong to a workspace, so each of their four combinations is a disk of
// its own (matrixWorkspace), and each index state with a disk state is a cell of it, at the
// sub-path <index state>.<disk state>/p, such as absent.dir-jj/p or gitlink.parent-link/p. The
// cell's name is that sub-path's directory, and every failure names it.
//
// What a record is. Under a born HEAD, the regular, assume-unchanged, skip-worktree, symlink and
// executable entries are what a role's jj committed, which HEAD holds (`jj commit`), and the
// three gitlinks are embedded repositories committed with plain git, so HEAD holds them too; the
// intent-to-add and forced entries are in no commit. Under an unborn HEAD nothing is committed:
// those entries are staged with `git add` after the last jj command, so jj holds no record of
// them before the route's own snapshot. A disk state is read against the entry's record: the
// committed or staged file or symlink, the file jj snapshotted for intent-to-add, the file forced
// into the index, and for a gitlink the commit it names. A symlink's record names a path that is
// not there, so new content of either size is a symlink named otherwise. The marks and the
// forced entry are written after every jj command, since a jj command rewrites the index
// (buildMatrix).
//
// Which combinations collapse, or cannot be built, and so are not built twice (built):
//
//   - absent with absent: no record and nothing on disk, so nothing to build; both trees hold
//     nothing there.
//   - absent with the same content, new content of either size and either mtime, or the
//     execute bit cleared: a path with no record has no content to be the same as or to differ
//     from, and a new file has no execute bit to clear, so these are one cell, absent.same, a new
//     file. absent.chmod+x, a new executable file, and absent.ignored, a new file the rule
//     covers, are built.
//   - A gitlink, marked or not, with new content: a gitlink's content is the commit it names, so
//     new content of the same size is a commit made inside its repository, one cell,
//     <gitlink>.new-same-size, whichever its mtime; a gitlink with a directory holding a .git
//     with a commit is the same build, a repository at the gitlink's path whose HEAD is not the
//     commit recorded. New content of another size is a regular file in the repository's place,
//     <gitlink>.new-other-size, and <gitlink>.ignored is that file under the rule. A gitlink has
//     no execute bit of its own, so chmod +x and chmod -x are <gitlink>.same.
//   - A symlink with the execute bit set or cleared: git records no mode bits of a symlink, and
//     chmod changes the mode of what it names, so both are symlink.same.
//   - The execute bit set on what has it already, or cleared on what lacks it: chmod +x of the
//     executable entry is executable.same, and chmod -x of every other file entry is its same.
//   - forced with the rule: the forced entry's path is under an ignore rule already, so
//     forced.ignored is forced.new-other-size.
//   - A missing index file: the copy is read from HEAD's tree under a born HEAD and is nothing
//     under an unborn one (rescue.snapshotTree). An assume-unchanged or skip-worktree mark lives in
//     the index file, so it goes with it: those cells are the regular cells, and the marked
//     gitlinks' cells the gitlink cells, and each is built once. The intent-to-add entry goes from
//     the copy too, but jj's @ still holds the file, which is a state of its own, and is built.
//     The forced entry goes, leaving an untracked path an ignore rule covers, and its record goes
//     with it: the same content, new content of either size or mtime and the execute bit set are
//     one cell, forced.same, and forced.absent is nothing at all, as absent with absent is.
//   - A missing index file under an unborn HEAD: the regular, symlink, executable and gitlink
//     entries were only staged, and jj holds no record of them, so each is the absent cell of its
//     disk state, and is built once.
//   - The staged gitlink bump and the gitlink `git rm --cached` took out are index states only a
//     born HEAD with an index file can hold: elsewhere each is the plain gitlink's cell, so they
//     are built in that combination alone, crossed with the gitlink's disk states.
//
// That leaves 161 cells under a born HEAD with an index file, 84 with none, 139 under an unborn
// HEAD with an index file, and 32 with none: 416 cells.
//
// What the matrix does not cover, and the rows do: two paths of one cell in different states,
// such as a record under another record; git's content conversion and its configuration; sparse
// patterns and jj's size limit; a conflicted or stale working copy; an unreadable directory;
// names git refuses or that are not valid UTF-8. What it does not cover, and no row does either:
// an unreadable tracked file, which today's read fails on and the route reads clean, the one
// named exception to failing where that read fails; special files (a fifo, a socket, a device);
// a parent replaced by a regular file; a .git that is a gitfile rather than a directory;
// unmerged index stages; and a symlink or executable record crossed with the assume-unchanged or
// skip-worktree mark, with intent-to-add or with the forced entry.
//
// What the matrix does not prove, of the cells it builds:
//
//   - Under an unborn HEAD both reads start from `read-tree --empty` (rescue.snapshotTree,
//     rescue.indexTree), so neither reads a record or a mark of the index file, nor whether there
//     is one, and jj holds no record of a path git alone staged. So the 139 cells under an unborn
//     HEAD with an index file give both reads the input of another cell: the 102 whose record git
//     alone staged (regular, assume-unchanged, skip-worktree, the three gitlinks, symlink and
//     executable) that of a path with no record in their disk state's shape, which for most of
//     them is the absent cell of that disk state, and the 37 absent, intent-to-add and forced
//     cells that of the same cell with no index file, or of forced.same or absent with absent
//     where that collapses. They pass as those cells pass, and prove nothing of a staged record
//     or a mark that those cells do not.
//   - The split in halves (matrixRun.classify) finds a cell that fails in a read alone. A failure
//     that needs two cells together, the one in each half, fails the workspace of both and
//     neither half, and the halves are then compared as trees that both reads answer: the
//     search cannot find it, and the matrix does not hold it to the rule a failure is held to.
//
// How the cells are run. Today's read and the route each fail the whole workspace when any one
// path fails, so a failing cell would hide every other cell of its workspace. So cells are
// classified by what each read answers, a tree or a failure, measured, and never assumed
// (matrixRun.classify): the cells of a combination are built in one jj workspace and read both
// ways, and where both reads answer a tree, that is the class of every cell in it, since a path
// that fails would have failed the workspace. Where either read fails, the workspace's cells are
// split in two halves, each built in a workspace of its own and classified the same way, until a
// cell that fails is in a workspace alone. So each cell that fails in a read alone is classified
// by measurement without a workspace for every cell, and a failure that needs two cells together
// is not found, as the bullet above says: measured on 2026-11-13 over the 218 cells of the seven
// index states and eleven disk states before these, one workspace per cell ran the matrix in 289
// s and this in 134 s, since 16 of the 218 cells fail in a read, and they sit side by side
// (cells).
//
// A cell that fails in either read is held to the rule a failure is held to, in its own
// workspace (failedAsRuled). Where today's read fails, the route must fail too, and a cell whose
// route answers a tree there fails as a loss (failureLost). Where both fail, the route's failure
// must name the cell's own path, or a path under it, as the route writes a path (namesPath): a
// route that fails there for another reason is no agreement with today's read, and fails. Where
// the route fails alone, the cell fails unless matrixDeclared declares that refusal with its
// reason, as the rows declare the unreadable directory's (R4); no cell declares one today. Every
// cell of a workspace whose reads both answer a tree is held there, by its entries at its path's
// directory and under it, to the rules of the rows in both directions (heldEntriesAsRuled,
// entriesHeldAndAbsent), under what the cell declares (matrixDeclared). The paths of no cell,
// .gitignore and base.txt, are held the same way.
//
// A cell whose trees differ in a way no declaration covers fails, naming the cell, and so does a
// cell where today's read fails and the route answers a tree. Neither is decided here.
// matrixDeclared holds only differences an existing kind already covers by its reason, each with
// that reason, and every one of them carries a reason that is not empty (unreasoned).

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sjawhar/legion/daemon/internal/rescue/rescuetest"
)

// matrixIndex is the index state of a cell's path in the worktree's index copy.
type matrixIndex int

const (
	indexAbsent matrixIndex = iota
	indexRegular
	indexIntentToAdd
	indexAssumeUnchanged
	indexSkipWorktree
	indexGitlink
	indexForced
	indexGitlinkAssumeUnchanged
	indexGitlinkSkipWorktree
	indexSymlink
	indexExecutable
	indexGitlinkBumped
	indexGitlinkRmCached
	matrixIndexes
)

var matrixIndexNames = [matrixIndexes]string{"absent", "regular", "intent-to-add", "assume-unchanged", "skip-worktree", "gitlink", "forced", "gitlink-assume-unchanged", "gitlink-skip-worktree", "symlink", "executable", "gitlink-bumped", "gitlink-rm-cached"}

// gitlink is whether the index state's record is a gitlink: marked or not, bumped, or the one
// HEAD holds that `git rm --cached` took out of the index.
func (i matrixIndex) gitlink() bool {
	return i == indexGitlink || i == indexGitlinkAssumeUnchanged || i == indexGitlinkSkipWorktree || i == indexGitlinkBumped || i == indexGitlinkRmCached
}

// headAlone is whether the index state is one only an index file under a born HEAD holds: a
// gitlink staged at another commit than HEAD's, or one HEAD holds that the index does not. Under
// an unborn HEAD there is no HEAD to differ from, and with no index file the copy is HEAD's tree,
// so each is the gitlink cell there.
func (i matrixIndex) headAlone() bool {
	return i == indexGitlinkBumped || i == indexGitlinkRmCached
}

// marked is whether the index state marks its record assume-unchanged or skip-worktree.
func (i matrixIndex) marked() bool {
	return i == indexAssumeUnchanged || i == indexSkipWorktree || i == indexGitlinkAssumeUnchanged || i == indexGitlinkSkipWorktree
}

// staged is whether the index state's record is one a role commits under a born HEAD and git
// alone stages under an unborn one: every state but absent, intent-to-add and forced.
func (i matrixIndex) staged() bool {
	return i != indexAbsent && i != indexIntentToAdd && i != indexForced
}

// matrixDisk is the disk state at a cell's path.
type matrixDisk int

const (
	diskAbsent matrixDisk = iota
	diskSame
	diskNewSameSize
	diskNewSameSizeFresh
	diskNewOtherSize
	diskPlusX
	diskMinusX
	diskIgnored
	diskDirWithFile
	diskDirWithGitCommit
	diskDirWithGitNoCommit
	diskDirWithJJ
	diskLinkToFile
	diskLinkToDir
	diskParentLink
	matrixDisks
)

var matrixDiskNames = [matrixDisks]string{"absent", "same", "new-same-size", "new-same-size-fresh", "new-other-size", "chmod+x", "chmod-x", "ignored", "dir-file", "dir-git-commit", "dir-git-no-commit", "dir-jj", "link-file", "link-dir", "parent-link"}

// matrixCell is one index state with one disk state, at the path p under a directory of the
// cell's name.
type matrixCell struct {
	index matrixIndex
	disk  matrixDisk
}

// dir is the cell's name, the directory its path is in.
func (c matrixCell) dir() string { return matrixIndexNames[c.index] + "." + matrixDiskNames[c.disk] }

// path is the cell's path.
func (c matrixCell) path() string { return c.dir() + "/p" }

// key is the cell in w as matrixDeclared names it.
func (c matrixCell) key(w matrixWorkspace) string { return w.String() + "/" + c.dir() }

// record is the body of the file the cell's record holds.
func (c matrixCell) record() string { return recordOf(c.path()) }

// linkTarget is what the cell's symlink record names, a path that is not there.
func (c matrixCell) linkTarget() string { return "the target of " + c.path() }

// recordOf is the body of the file the record at path holds.
func recordOf(path string) string { return "the record of " + path + "\n" }

// matrixWorkspace is one combination of the worktree's git HEAD and its index file.
type matrixWorkspace struct{ born, indexFile bool }

var matrixWorkspaces = []matrixWorkspace{{true, true}, {true, false}, {false, true}, {false, false}}

func (w matrixWorkspace) String() string {
	head, index := "unborn HEAD", "no index file"
	if w.born {
		head = "born HEAD"
	}
	if w.indexFile {
		index = "an index file"
	}
	return head + " with " + index
}

// built is whether the cell is built in w, and false where it collapses into another cell or
// cannot be built (the comment at the head of this file).
func (w matrixWorkspace) built(c matrixCell) bool {
	newContent := c.disk == diskNewSameSize || c.disk == diskNewSameSizeFresh || c.disk == diskNewOtherSize
	modeAlone := c.disk == diskPlusX || c.disk == diskMinusX
	switch {
	case c.index == indexAbsent && (c.disk == diskAbsent || newContent || c.disk == diskMinusX):
		return false
	case c.index.gitlink() && (c.disk == diskNewSameSizeFresh || c.disk == diskDirWithGitCommit || modeAlone):
		return false
	case c.index == indexSymlink && modeAlone:
		return false
	case c.index == indexExecutable && c.disk == diskPlusX:
		return false
	case c.index != indexExecutable && c.disk == diskMinusX:
		return false
	case c.index == indexForced && c.disk == diskIgnored:
		return false
	case c.index.headAlone() && !(w.born && w.indexFile):
		return false
	case w.indexFile:
		return true
	case c.index.marked():
		return false
	case c.index == indexForced && (c.disk == diskAbsent || newContent || c.disk == diskPlusX):
		return false
	case !w.born && c.index.staged():
		return false
	}
	return true
}

// cells is every cell w builds, by disk state and then by index state, so that the cells of one
// disk state are side by side when a workspace of them is split (matrixRun.classify).
func (w matrixWorkspace) cells() []matrixCell {
	var cells []matrixCell
	for disk := range matrixDisks {
		for index := range matrixIndexes {
			if c := (matrixCell{index, disk}); w.built(c) {
				cells = append(cells, c)
			}
		}
	}
	return cells
}

// The reasons of matrixDeclared, each the reason of a row of the differential that declares the
// same kind for the same cause, or that cause over a symlink.
const (
	// The row of a file jj snapshotted, rewritten at the same size with its mtime set back to
	// the one jj recorded.
	mtimeSetBack = "an mtime set back on purpose to the one jj recorded hides a same-size rewrite from jj's stat rule, so the route keeps the bytes jj read before and loses the new bytes the git read carried; the role's own jj commit misses it the same way, and the git read carried it only because the stat-less index makes git hash every file"
	// The same cause over a symlink jj snapshotted, pointed at a name as long as its old one.
	linkMtimeSetBack = "a symlink pointed at a name as long as the one jj read, with its own mtime set back on purpose to the one jj recorded, as tar and rsync -t set a symlink's mtime with lutimes, is a same-size rewrite jj's stat rule passes over, so the route keeps the target jj read before and loses the new target the git read carried; the role's own jj commit misses it the same way, and the git read carried it only because the stat-less index makes git read every symlink"
)

// matrixDeclaration is what matrixDeclared declares of one cell, each path named relative to the
// cell's directory.
type matrixDeclaration struct {
	// contentLost, reformed, lost and gained are a row's kinds (differentialRow), each path with
	// its reason.
	contentLost, reformed, lost, gained map[string]string
	// refused is why the route may fail the workspace at the cell where today's read answers a
	// tree, a refusal under R4, and "" where it may not. No cell declares one today.
	refused string
}

// kinds is the declaration's kinds as a row of the differential holds them, under the paths it
// names them by.
func (d matrixDeclaration) kinds() differentialRow {
	return differentialRow{contentLost: d.contentLost, reformed: d.reformed, lost: d.lost, gained: d.gained}
}

// matrixDeclared is the cells whose two trees differ in a way an existing kind covers by its
// reason, or whose route may fail alone, by the combination and the cell's name (key), each
// path named relative to the cell's directory, with that reason. Every other cell must answer
// the same entries in both trees, or fail in both reads naming its own path.
var matrixDeclared = map[string]matrixDeclaration{
	"born HEAD with an index file/regular.new-same-size":    {contentLost: map[string]string{"p": mtimeSetBack}},
	"born HEAD with no index file/regular.new-same-size":    {contentLost: map[string]string{"p": mtimeSetBack}},
	"born HEAD with an index file/executable.new-same-size": {contentLost: map[string]string{"p": mtimeSetBack}},
	"born HEAD with no index file/executable.new-same-size": {contentLost: map[string]string{"p": mtimeSetBack}},
	"born HEAD with an index file/symlink.new-same-size":    {contentLost: map[string]string{"p": linkMtimeSetBack}},
	"born HEAD with no index file/symlink.new-same-size":    {contentLost: map[string]string{"p": linkMtimeSetBack}},
	"born HEAD with no index file/intent-to-add.ignored":    {gained: map[string]string{"p": trackedThenIgnored}},
	"unborn HEAD with an index file/intent-to-add.ignored":  {gained: map[string]string{"p": trackedThenIgnored}},
	"unborn HEAD with no index file/intent-to-add.ignored":  {gained: map[string]string{"p": trackedThenIgnored}},
}

// declared is what matrixDeclared declares of the cell's kinds in w, its paths under the cell's
// directory.
func (c matrixCell) declared(w matrixWorkspace) differentialRow {
	rel, ok := matrixDeclared[c.key(w)]
	if !ok {
		return differentialRow{}
	}
	under := func(paths map[string]string) map[string]string {
		if paths == nil {
			return nil
		}
		full := make(map[string]string, len(paths))
		for path, reason := range paths {
			full[c.dir()+"/"+path] = reason
		}
		return full
	}
	return differentialRow{
		name:        w.String() + ", " + c.dir(),
		contentLost: under(rel.contentLost),
		reformed:    under(rel.reformed),
		lost:        under(rel.lost),
		gained:      under(rel.gained),
	}
}

// buildMatrix adds a jj workspace of d's clone named name, in w's shape, holding each of cells at
// its path, and answers a disk whose Workspace is it. targets is the directory outside every
// worktree that the cells' symlinks name (matrixTargets).
//
// The order is what the cells stand on. The workspace stands on the base commit under a born HEAD
// and on the root commit under an unborn one. Under a born HEAD the gitlinks are committed first,
// with plain git, and then the regular, assume-unchanged, skip-worktree, symlink and executable
// records with jj, so jj has imported every commit before the files change. Then the
// intent-to-add files are written and snapshotted by `jj status`, the last jj command. Then git
// alone writes the index: the staged entries under an unborn HEAD, the forced entries, the bumped
// gitlinks, each staged with `git add` at a commit made inside its repository after HEAD's, the
// gitlinks `git rm --cached` takes out, and the marks. Then the rule `/*.ignored/p` joins
// .gitignore, so every record stands before it covers them. Each cell's index entry is checked
// (requireCellIndex) before its disk state is made (becomes), and last, where w has no index
// file, the index file is removed.
//
// Nothing a read keeps may hang on how fast the build ran, and git's stat rule would make it so
// twice; measured on this build, forced.new-same-size under a born HEAD with an index file was
// equal in one run and differed in the next. An entry git writes for a file whose mtime is not
// older than the index file is racily clean, and git reads that file again whatever its stat data
// says, so every record's mtime is set a minute back once it is written, before any index entry is
// made of it. And git compares a file's ctime to the second, so a same-size rewrite whose mtime is
// set back waits for the second after the record's ctime (becomes).
func buildMatrix(t *testing.T, d *rescuetest.Disk, w matrixWorkspace, name, targets string, cells []matrixCell) *rescuetest.Disk {
	t.Helper()
	ws := *d
	ws.Workspace = filepath.Join(filepath.Dir(d.Workspace), name)
	add := []string{"jj", "workspace", "add", "--name", name}
	if !w.born {
		add = append(add, "--revision", "root()")
	}
	d.Run(d.Clone, append(add, ws.Workspace)...)
	of := func(states ...matrixIndex) []matrixCell {
		var held []matrixCell
		for _, c := range cells {
			if slices.Contains(states, c.index) {
				held = append(held, c)
			}
		}
		return held
	}
	paths := func(cells []matrixCell) []string {
		held := make([]string, 0, len(cells))
		for _, c := range cells {
			held = append(held, c.path())
		}
		return held
	}
	// write makes the record of each of cells at its path, a file, an executable file or a
	// symlink, with its mtime set a minute back, and answers their paths.
	write := func(cells []matrixCell) []string {
		back := time.Now().Add(-time.Minute)
		for _, c := range cells {
			at := filepath.Join(ws.Workspace, c.path())
			switch c.index {
			case indexSymlink:
				symlink(t, c.linkTarget(), at)
			case indexExecutable:
				ws.Write(ws.Workspace, c.path(), c.record())
				if err := os.Chmod(at, 0o755); err != nil {
					t.Fatal(err)
				}
			default:
				ws.Write(ws.Workspace, c.path(), c.record())
			}
			setTimes(t, at, back)
		}
		return paths(cells)
	}
	links := paths(of(indexGitlink, indexGitlinkAssumeUnchanged, indexGitlinkSkipWorktree, indexGitlinkBumped, indexGitlinkRmCached))
	committed := of(indexRegular, indexAssumeUnchanged, indexSkipWorktree, indexSymlink, indexExecutable)
	ws.Write(ws.Workspace, ".gitignore", "/forced.*/p\n")
	if w.born {
		if len(links) > 0 {
			for _, link := range links {
				embeddedRepository(&ws, link)
			}
			ws.Git(append([]string{"add", "--"}, links...)...)
			ws.Git("commit", "--quiet", "-m", "the gitlinks, committed with plain git")
		}
		write(committed)
		ws.JJ("commit", "-m", "the records")
	}
	if fresh := of(indexIntentToAdd); len(fresh) > 0 {
		intentToAdd(t, &ws, write(fresh)...)
	}
	if !w.born {
		staged := write(committed)
		for _, link := range links {
			embeddedRepository(&ws, link)
			staged = append(staged, link)
		}
		if len(staged) > 0 {
			ws.Git(append([]string{"add", "--"}, staged...)...)
		}
	}
	if forced := of(indexForced); len(forced) > 0 {
		ws.Git(append([]string{"add", "-f", "--"}, write(forced)...)...)
	}
	if bumped := of(indexGitlinkBumped); len(bumped) > 0 {
		for _, c := range bumped {
			at := filepath.Join(ws.Workspace, c.path())
			ws.Write(at, "bump.txt", "the commit a bump stages\n")
			ws.Run(at, "git", "add", "bump.txt")
			ws.Run(at, "git", "commit", "--quiet", "-m", "the commit a bump stages")
		}
		ws.Git(append([]string{"add", "--"}, paths(bumped)...)...)
	}
	if uncached := paths(of(indexGitlinkRmCached)); len(uncached) > 0 {
		ws.Git(append([]string{"rm", "--quiet", "--cached", "--"}, uncached...)...)
	}
	for flag, states := range map[string][]matrixIndex{
		"--assume-unchanged": {indexAssumeUnchanged, indexGitlinkAssumeUnchanged},
		"--skip-worktree":    {indexSkipWorktree, indexGitlinkSkipWorktree},
	} {
		if marked := paths(of(states...)); len(marked) > 0 {
			ws.Git(append([]string{"update-index", flag, "--"}, marked...)...)
		}
	}
	ws.Write(ws.Workspace, ".gitignore", "/forced.*/p\n/*.ignored/p\n")
	for _, c := range cells {
		requireCellIndex(t, &ws, c)
		becomes(t, &ws, targets, name, c)
	}
	if !w.indexFile {
		if err := os.Remove(strings.TrimSpace(gitRead(t, &ws, ws.Workspace, nil, "rev-parse", "--path-format=absolute", "--git-path", "index"))); err != nil {
			t.Fatal(err)
		}
	}
	if head := headOf(t, &ws, ws.Workspace); (head != "") != w.born {
		t.Fatalf("the workspace %s has its git HEAD at %q, want it %s", name, head, w)
	}
	return &ws
}

// setTimes sets the access and modification times of the file or symlink at to when, and of a
// symlink not those of what it names.
func setTimes(t *testing.T, at string, when time.Time) {
	t.Helper()
	ts := unix.NsecToTimespec(when.UnixNano())
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, at, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatalf("setting the times of %s: %v", at, err)
	}
}

// requireCellIndex fails the test unless the workspace's index holds at the cell's path the entry
// its index state names: no entry where it is absent, and no entry where HEAD holds a gitlink
// there for one `git rm --cached` took out; a gitlink, which `ls-files -v` tags h where it is
// marked assume-unchanged and S where it is marked skip-worktree, and which is at another commit
// than the one HEAD holds there for a bumped one; a file entry it tags h or S the same way; a
// symlink at 120000; an executable file at 100755; the empty blob of intent-to-add; and
// otherwise a file's blob at 100644, for a regular entry and a forced one.
func requireCellIndex(t *testing.T, ws *rescuetest.Disk, c matrixCell) {
	t.Helper()
	// headLink is the commit of the gitlink HEAD holds at the cell's path, and "" where HEAD is
	// unborn or holds none there.
	headLink := ""
	if out, code, _ := gitExit(t, ws, ws.Workspace, nil, "", "ls-tree", "-z", "HEAD", "--", c.path()); code == 0 {
		for _, line := range nulSplit(out) {
			meta, _, _ := strings.Cut(line, "\t")
			if fields := strings.Fields(meta); len(fields) == 3 && fields[0] == gitlinkMode {
				headLink = fields[2]
			}
		}
	}
	records := nulSplit(gitRead(t, ws, ws.Workspace, nil, "ls-files", "-s", "-v", "-z", "--", c.path()))
	held := "absent"
	if headLink != "" {
		held = "gitlink-rm-cached"
	}
	switch {
	case len(records) > 1:
		held = fmt.Sprintf("%q", records)
	case len(records) == 1:
		meta, _, _ := strings.Cut(records[0], "\t")
		switch fields := strings.Fields(meta); {
		case len(fields) != 4:
			held = fmt.Sprintf("%q", records[0])
		case fields[1] == gitlinkMode && fields[0] == "h":
			held = "gitlink-assume-unchanged"
		case fields[1] == gitlinkMode && fields[0] == "S":
			held = "gitlink-skip-worktree"
		case fields[1] == gitlinkMode && fields[0] == "H" && headLink != "" && headLink != fields[2]:
			held = "gitlink-bumped"
		case fields[1] == gitlinkMode && fields[0] == "H":
			held = "gitlink"
		case fields[0] == "h" && fields[1] == "100644":
			held = "assume-unchanged"
		case fields[0] == "S" && fields[1] == "100644":
			held = "skip-worktree"
		case fields[0] == "H" && fields[1] == "120000":
			held = "symlink"
		case fields[0] == "H" && fields[1] == "100755":
			held = "executable"
		case fields[0] == "H" && fields[1] == "100644" && fields[2] == "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391":
			held = "intent-to-add"
		case fields[0] == "H" && fields[1] == "100644":
			held = "regular"
		default:
			held = fmt.Sprintf("%q", records[0])
		}
	}
	want := matrixIndexNames[c.index]
	if c.index == indexForced {
		want = "regular"
	}
	if held != want {
		t.Fatalf("the cell %s: the index holds %s at %s, want the %s entry the cell stands on", c.dir(), held, c.path(), want)
	}
}

// matrixTargets makes the directory outside every worktree of d that the cells' symlinks name, and
// answers it: file.txt, a file; dir, a directory holding k.txt; and, made by becomes, the
// directory each parent replaced by a symlink names.
func matrixTargets(d *rescuetest.Disk) string {
	targets := filepath.Join(filepath.Dir(d.Remote), "targets")
	d.Write(targets, "file.txt", "the file a cell's symlink names\n")
	d.Write(targets, "dir/k.txt", "a file of the directory a cell's symlink names\n")
	return targets
}

// becomes makes the cell's disk state at its path in the workspace named workspace, after its
// record. A path whose index state is absent has no record, so it is first written as a new file.
func becomes(t *testing.T, ws *rescuetest.Disk, targets, workspace string, c matrixCell) {
	t.Helper()
	at := filepath.Join(ws.Workspace, c.path())
	if c.index == indexAbsent {
		ws.Write(ws.Workspace, c.path(), c.record())
	}
	switch c.disk {
	case diskAbsent:
		removeAll(t, at)
	case diskSame:
	case diskNewSameSize, diskNewSameSizeFresh:
		setBack := c.disk == diskNewSameSize
		if c.index.gitlink() {
			ws.Run(at, "git", "add", "loose.txt")
			ws.Run(at, "git", "commit", "--quiet", "-m", "a commit made inside the repository")
			break
		}
		if setBack {
			// git compares ctime to the second, so a rewrite in the record's second whose mtime is
			// set back reads as unchanged to it.
			pastCtimeSecond(t, at)
		}
		if c.index == indexSymlink {
			relinked(t, at, strings.ToUpper(c.linkTarget()), setBack)
			break
		}
		rewrittenAtTheSameSize(t, ws, c.path(), strings.ToUpper(c.record()), setBack)
	case diskNewOtherSize, diskIgnored:
		switch {
		case c.index.gitlink():
			removeAll(t, at)
			ws.Write(ws.Workspace, c.path(), c.record())
		case c.index == indexSymlink:
			removeAll(t, at)
			symlink(t, c.linkTarget()+", and more", at)
		default:
			ws.Write(ws.Workspace, c.path(), c.record()+"and a line more\n")
		}
	case diskPlusX:
		if err := os.Chmod(at, 0o755); err != nil {
			t.Fatal(err)
		}
	case diskMinusX:
		if err := os.Chmod(at, 0o644); err != nil {
			t.Fatal(err)
		}
	case diskDirWithFile:
		removeAll(t, at)
		ws.Write(ws.Workspace, c.path()+"/k.txt", "a file of the directory at the cell's path\n")
	case diskDirWithGitCommit:
		removeAll(t, at)
		becomesAGitRepository(ws, c.path())
	case diskDirWithGitNoCommit:
		removeAll(t, at)
		ws.Run(ws.Workspace, "git", "init", "--quiet", "--initial-branch=main", c.path())
		ws.Write(ws.Workspace, c.path()+"/work.txt", "work in a repository with no commit\n")
	case diskDirWithJJ:
		removeAll(t, at)
		ws.Write(ws.Workspace, c.path()+"/.jj/f", "the nested jj's own file\n")
		ws.Write(ws.Workspace, c.path()+"/k.txt", "a file beside the nested .jj\n")
	case diskLinkToFile:
		removeAll(t, at)
		symlink(t, filepath.Join(targets, "file.txt"), at)
	case diskLinkToDir:
		removeAll(t, at)
		symlink(t, filepath.Join(targets, "dir"), at)
	case diskParentLink:
		parent := filepath.Join(ws.Workspace, c.dir())
		moved := filepath.Join(targets, workspace, c.dir())
		if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(parent, moved); err != nil {
			t.Fatal(err)
		}
		symlink(t, moved, parent)
	}
}

// relinked points the symlink at at to target, which is as long as what it named, as a role
// names another path with it. With setBack its mtime is then set back to the one it had, the one
// jj recorded, as rewrittenAtTheSameSize sets a file's; without it the mtime is left where the
// new symlink has it, and the cell fails where it did not move, since the cell then stands on
// nothing.
func relinked(t *testing.T, at, target string, setBack bool) {
	t.Helper()
	was, err := os.Lstat(at)
	if err != nil {
		t.Fatal(err)
	}
	named, err := os.Readlink(at)
	if err != nil {
		t.Fatal(err)
	}
	if len(named) != len(target) {
		t.Fatalf("%s names %q, want a name as long as %q, which it is pointed at", at, named, target)
	}
	removeAll(t, at)
	symlink(t, target, at)
	if setBack {
		setTimes(t, at, was.ModTime())
	}
	now, err := os.Lstat(at)
	if err != nil {
		t.Fatal(err)
	}
	if moved := !now.ModTime().Equal(was.ModTime()); moved == setBack {
		t.Fatalf("%s has the mtime %s once pointed at %q, from %s before: the cell stands on its mtime being set back (%v)", at, now.ModTime(), target, was.ModTime(), setBack)
	}
}

// pastCtimeSecond waits until the clock is past the second of the ctime of the file at, so that a
// rewrite of it now has a ctime git tells from the one an index entry made of it before holds.
// git compares a ctime to the second: measured on this build, a file rewritten at its size with
// its mtime set back read as unchanged to `git diff-files` in the second of its `git add`, three
// times out of three, and as changed 1.2 s later, three times out of three. The wait is a second
// at the most, and none where the clock has passed that second already.
func pastCtimeSecond(t *testing.T, at string) {
	t.Helper()
	info, err := os.Lstat(at)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("the stat of %s holds no ctime: %T", at, info.Sys())
	}
	if next := time.Unix(stat.Ctim.Sec+1, 0); time.Now().Before(next) {
		time.Sleep(time.Until(next))
	}
}

// under is the entries at the cell's directory and below it.
func under(entries map[string]entry, c matrixCell) map[string]entry {
	held := map[string]entry{}
	for path, e := range entries {
		if path == c.dir() || strings.HasPrefix(path, c.dir()+"/") {
			held[path] = e
		}
	}
	return held
}

// listing is entries as a failure message says them, sorted by path.
func listing(entries map[string]entry) string {
	if len(entries) == 0 {
		return "nothing"
	}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for i, path := range paths {
		paths[i] = fmt.Sprintf("%q at mode %s blob %s", path, entries[path].mode, entries[path].object)
	}
	return strings.Join(paths, ", ")
}

// The outcomes of a cell: equal where both trees hold the same entries under it, and those of a
// cell that fails in a read alone (failedAsRuled).
const (
	equal              = "equal"
	bothFail           = "both fail"
	routeFailsDeclared = "route fails alone, declared"
	undecided          = "undecided"
)

// failedAsRuled holds a cell that fails in either read, in the workspace ws it was built alone in,
// to the rule a failure is held to, and answers its outcome. Where today's read fails and the
// route answers a tree, the cell fails as a loss. Where both fail, the route's failure must name
// the cell's path or a path under it (namesPath), or the cell fails. Where the route fails alone,
// the cell fails unless its declaration says why the route may refuse it there (refused). What
// each read said is logged.
func failedAsRuled(t *testing.T, w matrixWorkspace, c matrixCell, ws *rescuetest.Disk, r reads) string {
	t.Helper()
	cell := w.String() + ", " + c.dir()
	if lost := failureLost(r); lost != "" {
		t.Errorf("the cell %s: %s; the jj route's tree holds under the cell %s", cell, lost, listing(under(entriesOf(t, ws, r.route), c)))
		return undecided
	}
	if r.todayErr != nil {
		if !namesPath(r.routeErr, c.path()) {
			t.Errorf("the cell %s: both reads fail the workspace, and the jj route's failure names neither %q nor a path under it, so it may fail for another reason than today's read does, which is no agreement with it. Today's read: %v. The jj route: %v", cell, c.path(), r.todayErr, r.routeErr)
			return undecided
		}
		t.Logf("the cell %s: both reads fail the workspace. Today's read: %v. The jj route: %v", cell, r.todayErr, r.routeErr)
		return bothFail
	}
	held := listing(under(entriesOf(t, ws, r.today), c))
	why := matrixDeclared[c.key(w)].refused
	if strings.TrimSpace(why) == "" {
		t.Errorf("the cell %s: the jj route fails the workspace where today's read answers a tree, and matrixDeclared declares no refusal of the cell with its reason. Today's read's tree holds under the cell %s. The jj route: %v", cell, held, r.routeErr)
		return undecided
	}
	t.Logf("the cell %s: the jj route fails the workspace where today's read answers a tree, a refusal matrixDeclared declares (%s). Today's read's tree holds under the cell %s. The jj route: %v", cell, why, held, r.routeErr)
	return routeFailsDeclared
}

// declaredKinds is the kinds a cell's declaration uses, as its outcome says them.
func declaredKinds(row differentialRow) string {
	var kinds []string
	for _, kind := range []struct {
		word     string
		declared map[string]string
	}{{"contentLost", row.contentLost}, {"reformed", row.reformed}, {"lost", row.lost}, {"gained", row.gained}} {
		if len(kind.declared) > 0 {
			kinds = append(kinds, kind.word)
		}
	}
	if len(kinds) == 0 {
		return equal
	}
	return "declared " + strings.Join(kinds, " and ")
}

// matrixRun is the run of one combination's cells: the disk they are built on, the directory
// their symlinks name (matrixTargets), the count of workspaces made so far, and each cell's
// outcome, by outcome.
type matrixRun struct {
	w        matrixWorkspace
	d        *rescuetest.Disk
	targets  string
	made     int
	outcomes map[string][]string
}

// classify builds cells in one jj workspace and reads it both ways. Where both reads answer a
// tree, every cell of it is held to the rules there (compare). Where either read fails, a cell
// alone in the workspace is held to the rule a failure is held to (failedAsRuled), and more than
// one cell are split in two halves, each classified the same way in a workspace of its own.
func (m *matrixRun) classify(t *testing.T, cells []matrixCell) {
	t.Helper()
	m.made++
	ws := buildMatrix(t, m.d, m.w, fmt.Sprintf("cells-%03d", m.made), m.targets, cells)
	r := readBoth(t, ws, headOf(t, ws, ws.Workspace))
	switch {
	case r.todayErr == nil && r.routeErr == nil:
		m.compare(t, ws, r, cells)
	case len(cells) > 1:
		m.classify(t, cells[:len(cells)/2])
		m.classify(t, cells[len(cells)/2:])
	default:
		c := cells[0]
		outcome := undecided
		t.Run(c.dir(), func(t *testing.T) {
			outcome = failedAsRuled(t, m.w, c, ws, r)
			declaration := matrixDeclared[c.key(m.w)]
			if declaredKinds(declaration.kinds()) != equal {
				t.Errorf("matrixDeclared declares kinds of the cell %s, %s, which fails in a read alone and has no two trees to declare a difference of", m.w, c.dir())
				outcome = undecided
			}
			if declaration.refused != "" && r.todayErr != nil {
				t.Errorf("matrixDeclared declares a refusal of the cell %s, %s (%s), where today's read fails too: the allowance no longer describes the route", m.w, c.dir(), declaration.refused)
				outcome = undecided
			}
		})
		m.outcomes[outcome] = append(m.outcomes[outcome], m.w.String()+", "+c.dir())
	}
}

// compare holds every cell of the workspace ws, whose two reads r both answer a tree, to the
// rules of the rows by its own entries, and the paths of no cell the same way.
func (m *matrixRun) compare(t *testing.T, ws *rescuetest.Disk, r reads, cells []matrixCell) {
	t.Helper()
	was, now := entriesOf(t, ws, r.today), entriesOf(t, ws, r.route)
	t.Run("the paths of no cell in "+filepath.Base(ws.Workspace), func(t *testing.T) {
		outside := func(entries map[string]entry) map[string]entry {
			left := maps.Clone(entries)
			for _, c := range cells {
				for path := range under(entries, c) {
					delete(left, path)
				}
			}
			return left
		}
		heldEntriesAsRuled(t, ws, outside(was), outside(now), conflictedPaths(t, ws), differentialRow{})
	})
	for _, c := range cells {
		row := c.declared(m.w)
		outcome := declaredKinds(row)
		if !t.Run(c.dir(), func(t *testing.T) {
			cellWas, cellNow := under(was, c), under(now, c)
			heldEntriesAsRuled(t, ws, cellWas, cellNow, nil, row)
			entriesHeldAndAbsent(t, row, map[string]map[string]entry{"git read's": cellWas, "jj route's": cellNow})
			if why := matrixDeclared[c.key(m.w)].refused; why != "" {
				t.Errorf("matrixDeclared declares a refusal of the cell %s, %s (%s), and both reads answer a tree: the allowance no longer describes the route", m.w, c.dir(), why)
			}
			if t.Failed() {
				t.Logf("the cell %s, %s: the git read's tree holds under the cell %s; the jj route's holds %s", m.w, c.dir(), listing(cellWas), listing(cellNow))
			}
		}) {
			outcome = undecided
		}
		m.outcomes[outcome] = append(m.outcomes[outcome], m.w.String()+", "+c.dir())
	}
}

// Every cell of the generated matrix that today's read and the jj route both answer a tree for
// holds the same entries in both trees, but for what an existing kind covers by its reason; every
// cell whose today's read fails has its route fail too, naming the cell's path; and no cell's
// route fails alone but where matrixDeclared says why. The comment at the head of this file says
// what the cells are, which collapse, what the matrix does not prove, and how they are run.
func TestTheJJRouteKeepsWhatTheWholeWorktreeGitReadKeepsInEveryCellOfTheMatrix(t *testing.T) {
	names := map[string]bool{}
	for _, w := range matrixWorkspaces {
		cells := w.cells()
		t.Logf("%s: %d cells", w, len(cells))
		for _, c := range cells {
			names[c.key(w)] = true
		}
	}
	for name, declaration := range matrixDeclared {
		if !names[name] {
			t.Errorf("matrixDeclared declares %q, which is no cell the matrix builds", name)
		}
		for _, path := range unreasoned(declaration.kinds()) {
			t.Errorf("matrixDeclared declares %s in %q with no reason, want the reason the two trees may differ there", path, name)
		}
	}
	outcomes := map[string][]string{}
	for _, w := range matrixWorkspaces {
		t.Run(w.String(), func(t *testing.T) {
			d := rescuetest.New(t, issue)
			run := &matrixRun{w: w, d: d, targets: matrixTargets(d), outcomes: outcomes}
			run.classify(t, w.cells())
		})
	}
	total := 0
	kinds := make([]string, 0, len(outcomes))
	for outcome, cells := range outcomes {
		total += len(cells)
		kinds = append(kinds, outcome)
	}
	slices.Sort(kinds)
	for _, outcome := range kinds {
		t.Logf("%s: %d cells", outcome, len(outcomes[outcome]))
		if outcome != equal {
			for _, cell := range outcomes[outcome] {
				t.Logf("  %s: %s", outcome, cell)
			}
		}
	}
	t.Logf("%d cells in all", total)
}
