package ghconfig

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/testfifo"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

func twoOwnerBundle(acmeToken string) Bundle {
	expires := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	return Bundle{
		Default: "acme",
		Dirs: map[string]Rendered{
			"acme":    Render(acmeToken, "implement", expires),
			"sjawhar": Render("ghs_sjawhar_read", "implement", expires),
		},
	}
}

// WriteBundle writes each owner's two gh files under <root>/<owner>, reports the owners whose
// hosts.yml changed (all of them on the first write, sorted), then only the owner whose token
// turned, and removes every entry of root that is neither an owner of the bundle nor its pairs
// file: an owner the App's installations no longer hold, a file of a
// directory written before bundles, and anything else, such as a file naming a default owner, that
// is no owner's directory, since nothing under the root but the gh files and the pairs the
// wrappers route by is the daemon's.
func TestWriteBundleWritesOneDirectoryPerOwnerAndRemovesTheRest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gh")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{HostsFile: Hosts("ghs_stale"), ".default": "sjawhar"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write"))
	if err != nil {
		t.Fatalf("first WriteBundle: %v", err)
	}
	if want := []string{"acme", "sjawhar"}; !slices.Equal(changed, want) {
		t.Fatalf("first WriteBundle changed %q, want %q", changed, want)
	}
	for owner, token := range map[string]string{"acme": "ghs_acme_write", "sjawhar": "ghs_sjawhar_read"} {
		dir := Dir(root, owner)
		if dir != filepath.Join(root, owner) {
			t.Fatalf("Dir(%q) = %q, want %q", owner, dir, filepath.Join(root, owner))
		}
		hosts, err := os.ReadFile(filepath.Join(dir, HostsFile))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := TokenFromHosts(hosts); err != nil || got != token {
			t.Fatalf("%s's hosts.yml token = %q, %v, want %q", owner, got, err, token)
		}
		if config, err := os.ReadFile(filepath.Join(dir, ConfigFile)); err != nil || string(config) != Config {
			t.Fatalf("%s's config.yml = %q, %v, want %q", owner, config, err, Config)
		}
	}
	for _, stray := range []string{HostsFile, ".default"} {
		if _, err := os.Stat(filepath.Join(root, stray)); !os.IsNotExist(err) {
			t.Fatalf("the stray %s at the root survived the write: %v", stray, err)
		}
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("root = %v, %v, want a 0700 directory", info, err)
	}

	changed, err = WriteBundle(root, twoOwnerBundle("ghs_acme_turned"))
	if err != nil {
		t.Fatalf("second WriteBundle: %v", err)
	}
	if want := []string{"acme"}; !slices.Equal(changed, want) {
		t.Fatalf("second WriteBundle changed %q, want %q", changed, want)
	}

	alone := twoOwnerBundle("ghs_acme_turned")
	delete(alone.Dirs, "sjawhar")
	changed, err = WriteBundle(root, alone)
	if err != nil {
		t.Fatalf("third WriteBundle: %v", err)
	}
	if len(changed) != 0 {
		t.Fatalf("third WriteBundle changed %q, want nothing", changed)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if want := []string{"acme", PairsFile}; !slices.Equal(names, want) {
		t.Fatalf("root holds %q after sjawhar left the bundle, want %q: the owner and the bundle's pairs file", names, want)
	}
}

// A root a run of before INFRA-2543 wrote still holds the `<owner>/<repo>` directories that run
// cut a token per repository into, and no bundle holds one now: WriteBundle removes every one of
// them, so none is left holding a stale token, while the owner's own hosts.yml and config.yml
// stay where they are. A bundle that names such a directory is no bundle at all, and nothing of
// it is written: the shape is one the prune reads and nothing writes.
func TestWriteBundlePrunesTheRepositoryDirectoriesAnOlderRunLeft(t *testing.T) {
	expires := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "gh")
	for _, repo := range []string{"widgets", "gadgets"} {
		if _, err := Write(Dir(root, "acme/"+repo), Render("ghs_acme_"+repo, "implement", expires)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write")); err != nil {
		t.Fatalf("WriteBundle over a root an older run wrote: %v", err)
	}
	entries, err := os.ReadDir(Dir(root, "acme"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	if want := []string{ConfigFile, HostsFile}; !slices.Equal(names, want) {
		t.Fatalf("acme's directory holds %q, want %q: every repository directory an older run cut goes", names, want)
	}
	hosts, err := os.ReadFile(filepath.Join(Dir(root, "acme"), HostsFile))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := TokenFromHosts(hosts); err != nil || got != "ghs_acme_write" {
		t.Fatalf("acme's hosts.yml token = %q, %v, want the owner's own ghs_acme_write", got, err)
	}

	nested := twoOwnerBundle("ghs_acme_write")
	nested.Dirs["acme/widgets"] = Render("ghs_acme_widgets", "implement", expires)
	fresh := filepath.Join(t.TempDir(), "gh")
	if _, err := WriteBundle(fresh, nested); err == nil || !strings.Contains(err.Error(), `"acme/widgets"`) {
		t.Fatalf("WriteBundle of a bundle naming acme/widgets = %v, want the refusal naming it", err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("WriteBundle of a refused bundle left %s: %v", fresh, err)
	}
}

// IsOwner takes a lowercased GitHub login and nothing else: a bundle's every directory, its
// Default among them, is one owner's whole lease, so an `<owner>/<repo>` name is no directory.
// Its grammar is ghrepo's owner grammar lowercased, so a managed user's login, which ends in
// `_<shortcode>`, is one, and a name past GitHub's 39 characters is not; and every spelling of
// an owner is a directory here, lowercased, exactly where ghrepo parses a repository of it, so
// no issue the daemon admits on a parsed repository lacks a directory for its owner.
func TestIsOwnerTakesALowercasedLoginAndNothingNarrower(t *testing.T) {
	for name, want := range map[string]bool{
		"acme": true, "acme-labs": true, "a1": true, "mona-cat_octo": true, strings.Repeat("a", 39): true,
		"": false, "Acme": false, "-acme": false, "_acme": false, strings.Repeat("a", 40): false,
		"acme/widgets": false, "acme/": false, "/widgets": false, "../x": false, PairsFile: false,
	} {
		if got := IsOwner(name); got != want {
			t.Errorf("IsOwner(%q) = %t, want %t", name, got, want)
		}
	}
	for _, owner := range []string{
		"Mona-Cat_Octo", "ArtOfCode-", "hello--world", strings.Repeat("A", 39), strings.Repeat("A", 40), "_acme", "-acme",
	} {
		_, err := ghrepo.Parse("repo", owner+"/widgets")
		if parsed, got := err == nil, IsOwner(strings.ToLower(owner)); got != parsed {
			t.Errorf("IsOwner(%q) = %t where ghrepo parses %s/widgets %t: the two grammars must be one", strings.ToLower(owner), got, owner, parsed)
		}
	}
}

// RouteDir is the one rule a repository is routed to a directory by: the `to` owner of the pair
// whose `from` is the repository, without regard to case, and the owner's own directory for every
// other repository of that owner, the pair's `to` itself among them. A name the caller gives no
// repository for routes to the owner's own.
func TestRouteDirRoutesAMovedRepositoryToItsNewOwner(t *testing.T) {
	pairs := []ghrepo.Rename{ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion")}
	for _, tc := range []struct {
		owner, name string
		dir         string
		moved       bool
	}{
		{"sjawhar", "legion", "cybermonklabs", true},
		{"Sjawhar", "Legion", "cybermonklabs", true},
		{"sjawhar", "ygo", "sjawhar", false},
		{"cybermonklabs", "legion", "cybermonklabs", false},
		{"Acme", "widgets", "acme", false},
		{"sjawhar", "", "sjawhar", false},
	} {
		dir, pair, moved := RouteDir(pairs, tc.owner, tc.name)
		if dir != tc.dir || moved != tc.moved || (moved && pair != pairs[0]) {
			t.Errorf("RouteDir(%s/%s) = %q, %v, %t; want %q, moved %t", tc.owner, tc.name, dir, pair, moved, tc.dir, tc.moved)
		}
	}
	if dir, _, moved := RouteDir(nil, "sjawhar", "legion"); dir != "sjawhar" || moved {
		t.Errorf("RouteDir with no pair = %q, %t; want sjawhar's own directory", dir, moved)
	}
}

// Validate refuses a bundle whose default is missing, not lowercased, absent from its
// directories or naming a repository rather than an owner — GH_CONFIG_DIR always names an
// owner's directory — and one with a directory that is no lowercased GitHub login, an
// `<owner>/<repo>` directory among them, since a directory name is a path under the bundle's
// root and every directory is one owner's whole lease; WriteBundle writes nothing for a bundle
// Validate refuses.
func TestValidateRefusesABundleThatCannotNameItsDirectories(t *testing.T) {
	good := twoOwnerBundle("ghs_acme")
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate(two-owner bundle) = %v, want nil", err)
	}
	for _, tc := range []struct {
		name   string
		bundle func() Bundle
		want   string
	}{
		{"no default", func() Bundle { b := twoOwnerBundle("t"); b.Default = ""; return b }, "names no default owner"},
		{"default not lowercased", func() Bundle { b := twoOwnerBundle("t"); b.Default = "Acme"; return b }, `"Acme"`},
		{"unknown default", func() Bundle { b := twoOwnerBundle("t"); b.Default = "other"; return b }, `default owner "other"`},
		{"owner that walks out of root", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["../x"] = b.Dirs["sjawhar"]
			return b
		}, `"../x"`},
		{"owner not lowercased", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["Sjawhar"] = b.Dirs["sjawhar"]
			return b
		}, `"Sjawhar"`},
		{"a default naming a repository rather than an owner", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["acme/widgets"] = b.Dirs["sjawhar"]
			b.Default = "acme/widgets"
			return b
		}, `"acme/widgets"`},
		{"a repository directory under an owner", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["acme/widgets"] = b.Dirs["sjawhar"]
			return b
		}, `"acme/widgets"`},
		{"a repository name that walks out of its owner", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["acme/.."] = b.Dirs["sjawhar"]
			return b
		}, `"acme/.."`},
		{"a directory nested two deep", func() Bundle {
			b := twoOwnerBundle("t")
			b.Dirs["acme/widgets/deep"] = b.Dirs["sjawhar"]
			return b
		}, `"acme/widgets/deep"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bundle := tc.bundle()
			err := bundle.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tc.want)
			}
			root := filepath.Join(t.TempDir(), "gh")
			if _, err := WriteBundle(root, bundle); err == nil {
				t.Fatal("WriteBundle wrote a bundle Validate refuses")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("WriteBundle of a refused bundle left %s: %v", root, err)
			}
		})
	}
}

// DirNames is the bundle's directories, sorted, which the runtimes' log lines name.
func TestDirNamesIsSorted(t *testing.T) {
	if got, want := twoOwnerBundle("t").DirNames(), []string{"acme", "sjawhar"}; !slices.Equal(got, want) {
		t.Fatalf("DirNames() = %q, want %q", got, want)
	}
}

// The real gh's git-credential helper answers git from one owner's directory of a written bundle
// with that owner's token, the same check TestGhReadsTheRenderedFilesWithoutWriting makes of one
// directory.
func TestGhAnswersGitFromAnOwnersDirectory(t *testing.T) {
	gh := ghBinary(t)
	root := filepath.Join(t.TempDir(), "gh")
	if _, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write")); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for owner, token := range map[string]string{"acme": "ghs_acme_write", "sjawhar": "ghs_sjawhar_read"} {
		cmd := exec.Command(gh, "auth", "git-credential", "get")
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
			"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
			"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
			"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
			"GH_CONFIG_DIR=" + Dir(root, owner),
			"GH_TOKEN=",
			"GITHUB_TOKEN=",
			"GH_HOST=",
			"GH_NO_UPDATE_NOTIFIER=1",
		}
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\n\n")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("gh auth git-credential get for %s: %v", owner, err)
		}
		if !strings.Contains(string(out), "password="+token+"\n") {
			t.Fatalf("gh auth git-credential get for %s answered\n%s\nwant password=%s", owner, out, token)
		}
	}
}

// The bundle's pairs are the root's own file (PairsFile), one pair a line: WriteBundle writes it
// beside the owners' directories, rewrites it when the configured renames change, writes it empty
// for a bundle with none, and keeps it where it prunes every other entry of the root. ReadPairs
// reads back what RenderPairs wrote, a root with no such file holds no pair — a pod launched
// before the file existed — and a line that is no `<from> <to>` pair of repositories is an error
// rather than half a routing table.
func TestWriteBundleWritesThePairsTheWrappersRouteBy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gh")
	moved := ghrepo.MustRename("sjawhar/legion", "CyberMonkLabs/legion")
	bundle := twoOwnerBundle("ghs_acme_write")
	bundle.Pairs = []ghrepo.Rename{moved}
	if _, err := WriteBundle(root, bundle); err != nil {
		t.Fatalf("WriteBundle: %v", err)
	}
	written, err := os.ReadFile(filepath.Join(root, PairsFile))
	if err != nil || string(written) != "sjawhar/legion CyberMonkLabs/legion\n" {
		t.Fatalf("%s holds %q (%v), want the configured pair as `<from> <to>`", PairsFile, written, err)
	}
	pairs, err := ReadPairs(root)
	if err != nil || !slices.Equal(pairs, []ghrepo.Rename{moved}) {
		t.Fatalf("ReadPairs = %v, %v, want the pair the bundle holds", pairs, err)
	}

	// A stray file at the root goes; the pairs file stays, and a bundle that no longer routes by
	// a pair is written with an empty one rather than none, since a missing file says only that
	// nothing wrote it.
	if err := os.WriteFile(filepath.Join(root, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write")); err != nil {
		t.Fatalf("second WriteBundle: %v", err)
	}
	if written, err := os.ReadFile(filepath.Join(root, PairsFile)); err != nil || len(written) != 0 {
		t.Fatalf("%s holds %q (%v) for a bundle with no pair, want it empty", PairsFile, written, err)
	}
	if _, err := os.Stat(filepath.Join(root, "stray")); !os.IsNotExist(err) {
		t.Fatalf("the stray file at the root survived the write: %v", err)
	}
	if pairs, err := ReadPairs(root); err != nil || len(pairs) != 0 {
		t.Fatalf("ReadPairs over an empty %s = %v, %v, want no pair", PairsFile, pairs, err)
	}
	if pairs, err := ReadPairs(filepath.Join(t.TempDir(), "older-pod")); err != nil || len(pairs) != 0 {
		t.Fatalf("ReadPairs of a root with no %s = %v, %v, want no pair and no error", PairsFile, pairs, err)
	}
}

// ParsePairs reads the file WriteBundle wrote and refuses a line that is no pair, naming the line
// and its number: a caller routing a request by these pairs must fail rather than route by half of
// them. Validate refuses a pair naming no repository on a side, which no line could hold.
func TestParsePairsRefusesALineThatIsNoPair(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"sjawhar/legion\n", "is no `<from> <to>` rename pair"},
		{"sjawhar cybermonklabs/legion\n", "the repository a rename moved"},
		{"sjawhar/legion cybermonklabs\n", "the repository a rename moved it to"},
		{"sjawhar/legion cybermonklabs/legion\nacme\n", "line 2"},
	} {
		if _, err := ParsePairs([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParsePairs(%q) = %v, want an error containing %q", tc.text, err, tc.want)
		}
	}
	bundle := twoOwnerBundle("ghs_acme_write")
	bundle.Pairs = []ghrepo.Rename{{To: ghrepo.MustParse("cybermonklabs/legion")}}
	if err := bundle.Validate(); err == nil || !strings.Contains(err.Error(), "names no repository on one side") {
		t.Errorf("Validate of a bundle holding half a pair = %v, want a refusal naming it", err)
	}
}

// entryNames is the names of dir's entries, sorted, as os.ReadDir answers them.
func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// The root a runtime hands WriteBundle may sit under a state directory reached through a link, or
// be a link itself, and it is written and cleaned all the same. Before the second write the root's
// pairs file is a fifo, which replaceFile removes under the root resolved once, and the root is
// resolved once again before it is cleaned, so rolefile.RemoveTree, which refuses a parent that is
// a link, is handed the directory the link names both times. In both rows the pairs file is then a
// regular file holding the bundle's pairs, a stray file at the root and a `<owner>/<repo>`
// directory an older run left go, and the owners' own files stay. Without replaceFile's
// resolution the row whose root is itself a link fails the write ("write the gh bundle's
// renames.txt: … is a symlink"); without the cleaning's, that row's cleaning is refused ("… is a
// symlink"), and the stray file outlives the write. The fifo carries a held reader
// (testfifo.Plant), which must see no open of it for writing, and the second write's answer is
// waited for with no span of its own (testwait.Answer).
func TestWriteBundleCleansARootReachedThroughALink(t *testing.T) {
	for _, tc := range []struct {
		name string
		root func(t *testing.T) string
	}{
		{"under a state directory that is a symlink", func(t *testing.T) string {
			state := filepath.Join(t.TempDir(), "state")
			if err := os.Symlink(t.TempDir(), state); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(state, "gh")
		}},
		{"a root that is itself a link", func(t *testing.T) string {
			root := filepath.Join(t.TempDir(), "gh")
			if err := os.Symlink(t.TempDir(), root); err != nil {
				t.Fatal(err)
			}
			return root
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.root(t)
			bundle := twoOwnerBundle("ghs_acme_write")
			if _, err := WriteBundle(root, bundle); err != nil {
				t.Fatalf("first WriteBundle: %v", err)
			}
			if err := os.WriteFile(filepath.Join(root, "stray"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			expires := time.Date(2026, 10, 16, 18, 0, 0, 0, time.UTC)
			if _, err := Write(Dir(root, "acme/widgets"), Render("ghs_acme_widgets", "implement", expires)); err != nil {
				t.Fatal(err)
			}
			pairs := filepath.Join(root, PairsFile)
			if err := os.Remove(pairs); err != nil {
				t.Fatal(err)
			}
			fifo := testfifo.Plant(t, pairs, 0o600)
			answered := make(chan error, 1)
			go func() {
				_, err := WriteBundle(root, bundle)
				answered <- err
			}()
			if err := testwait.Answer(t, "the second WriteBundle over the fifo "+pairs, answered); err != nil {
				t.Fatalf("second WriteBundle: %v", err)
			}
			if shown := fifo.OpenedForWriting(t); shown != "" {
				t.Fatalf("the second WriteBundle opened the fifo at %s for writing, and the reader held on it was shown %s: it must replace the fifo without opening it so", pairs, shown)
			}
			info, err := os.Lstat(pairs)
			if err != nil {
				t.Fatal(err)
			}
			if !info.Mode().IsRegular() {
				t.Fatalf("%s is %v after the second WriteBundle, want a regular file", pairs, info.Mode())
			}
			if body, err := os.ReadFile(pairs); err != nil || string(body) != RenderPairs(bundle.Pairs) {
				t.Fatalf("%s holds %q (%v) after the second WriteBundle, want %q", pairs, body, err, RenderPairs(bundle.Pairs))
			}
			if got, want := entryNames(t, root), []string{"acme", PairsFile, "sjawhar"}; !slices.Equal(got, want) {
				t.Fatalf("root holds %q, want %q: the stray file goes", got, want)
			}
			if got, want := entryNames(t, Dir(root, "acme")), []string{ConfigFile, HostsFile}; !slices.Equal(got, want) {
				t.Fatalf("acme's directory holds %q, want %q: the repository directory an older run cut goes", got, want)
			}
			for owner, token := range map[string]string{"acme": "ghs_acme_write", "sjawhar": "ghs_sjawhar_read"} {
				hosts, err := os.ReadFile(filepath.Join(Dir(root, owner), HostsFile))
				if err != nil {
					t.Fatal(err)
				}
				if got, err := TokenFromHosts(hosts); err != nil || got != token {
					t.Fatalf("%s's hosts.yml token = %q, %v, want %q", owner, got, err, token)
				}
			}
		})
	}
}

// The pane's own user can write the bundle's root, so a link it plants there is unlinked and never
// followed. A link at the root to a directory outside it goes, and the file in what it names
// stays. A link under a stale `<owner>/<repo>` directory the pane made 0555, which the daemon,
// holding no capability, cannot unlink it from, is left as it is: WriteBundle answers a permission
// error naming that stale directory, and the directory the link names, and the file in it, are as
// they were. So is a link under a 0555 subdirectory of a directory the pane made at an owner's
// config.yml: replaceFile removes what is there before it writes the file, and removes it without
// following a link, so the write answers the unlink's permission error, naming the link, and the
// file in the directory the link names stays. os.RemoveAll, in the cleaning or in replaceFile,
// opens such an entry with O_DIRECTORY|O_NOFOLLOW, which a gVisor release follows a link through
// (rolefile.OpenDirectory), so in such a pod it deletes what the link names; Linux refuses that
// open, so only such a gVisor shows either control fail.
func TestWriteBundleRemovesAPlantedLinkWithoutFollowingIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root a 0555 directory does not stop the unlink, so the removal never opens the link")
	}
	outside := t.TempDir()
	precious := filepath.Join(outside, "precious")
	if err := os.WriteFile(precious, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept := func(t *testing.T) {
		t.Helper()
		if body, err := os.ReadFile(precious); err != nil || string(body) != "kept\n" {
			t.Fatalf("%s, in the directory the planted link names, is %q (%v), want it kept", precious, body, err)
		}
	}
	root := filepath.Join(t.TempDir(), "gh")
	if _, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write")); err != nil {
		t.Fatalf("first WriteBundle: %v", err)
	}

	stray := filepath.Join(root, "stray")
	if err := os.Symlink(outside, stray); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write")); err != nil {
		t.Fatalf("WriteBundle over a link at the root: %v", err)
	}
	if _, err := os.Lstat(stray); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the link %s at the root is still there (%v)", stray, err)
	}
	kept(t)

	stale := filepath.Join(Dir(root, "acme"), "widgets")
	if err := os.Mkdir(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(stale, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0o555); err != nil {
		t.Fatal(err)
	}
	// The temporary directory's own cleanup could not remove the stale directory at 0555.
	t.Cleanup(func() { _ = os.Chmod(stale, 0o755) })
	_, err := WriteBundle(root, twoOwnerBundle("ghs_acme_write"))
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "remove acme/widgets from the gh bundle") {
		t.Fatalf("WriteBundle over a link under a 0555 directory = %v, want the unlink refused naming acme/widgets", err)
	}
	kept(t)
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the planted link is %v (%v), want it left as it was", info, err)
	}

	// The stale directory goes, its link first, so that nothing but the config.yml below is left
	// for the write to refuse.
	if err := os.Chmod(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stale); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(Dir(root, "acme"), ConfigFile)
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(config, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	buried := filepath.Join(sub, "link")
	if err := os.Symlink(outside, buried); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	_, err = WriteBundle(root, twoOwnerBundle("ghs_acme_write"))
	// The file the link names is checked first, so that a removal which followed the link fails
	// the test naming the file it deleted.
	kept(t)
	named := filepath.Join("acme", ConfigFile, "sub", "link")
	if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), named) {
		t.Fatalf("WriteBundle over a link under a 0555 directory at acme's %s = %v, want the unlink refused naming %s", ConfigFile, err, named)
	}
	if info, err := os.Lstat(buried); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link planted under acme's %s is %v (%v), want it left as it was", ConfigFile, info, err)
	}
}

// WriteBundle is the one function both the tmux daemon and `legion controller start` write a
// role's gh bundle through, and the pane's own user can write each of the three files it writes:
// an owner's config.yml and hosts.yml, and the root's pairs file. Whatever the pane left at one
// of them, a fifo, a link to /dev/zero or a link to a file outside the root, is replaced by
// rename, never waited on or followed: WriteBundle answers, never having opened the fifo for
// writing, the path is then a regular file holding what the bundle renders for it, and the file a
// link named is as it was. A bundle written again over its own untouched files leaves every one
// of them as it was. A read with os.ReadFile waits on such a fifo for a writer for as long as none
// comes and reads /dev/zero without end, and a write with os.WriteFile waits on it for a reader
// and has the file a link names truncated.
//
// No clock decides the row. The fifo carries a held reader (testfifo.Plant), so a write's open of
// it returns at once and is shown to that reader, and WriteBundle's answer is waited for with no
// span of its own (testwait.Answer), so a wait in an open fails by name at the test binary's
// deadline. The row once read an answer later than a second as such a wait, and so failed a slow
// runner whose write had waited on nothing.
func TestWriteBundleReplacesAFifoOrALinkAtEachFileItWrites(t *testing.T) {
	bundle := twoOwnerBundle("ghs_acme_write")
	bundle.Pairs = []ghrepo.Rename{ghrepo.MustRename("sjawhar/legion", "CyberMonkLabs/legion")}
	files := []struct {
		name string // the file as the subtests name it
		rel  string // its path under the root
		want string // what the bundle renders for it
	}{
		{"acme's config.yml", filepath.Join("acme", ConfigFile), Config},
		{"acme's hosts.yml", filepath.Join("acme", HostsFile), Hosts("ghs_acme_write")},
		{"the root's pairs file", PairsFile, RenderPairs(bundle.Pairs)},
	}
	shapes := []struct {
		name string
		// plant puts the shape at path and answers the check of what it must leave as it was,
		// or nil.
		plant func(t *testing.T, path string) (kept func(t *testing.T))
	}{
		{"a fifo", func(t *testing.T, path string) func(t *testing.T) {
			fifo := testfifo.Plant(t, path, 0o600)
			return func(t *testing.T) {
				t.Helper()
				if shown := fifo.OpenedForWriting(t); shown != "" {
					t.Fatalf("WriteBundle opened the fifo at %s for writing, and the reader held on it was shown %s: it must replace the fifo without opening it so", path, shown)
				}
			}
		}},
		{"a link to dev zero", func(t *testing.T, path string) func(t *testing.T) {
			if err := os.Symlink("/dev/zero", path); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"a link to a file outside the root", func(t *testing.T, path string) func(t *testing.T) {
			precious := filepath.Join(t.TempDir(), "precious")
			if err := os.WriteFile(precious, []byte("precious\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(precious, path); err != nil {
				t.Fatal(err)
			}
			return func(t *testing.T) {
				t.Helper()
				if body, err := os.ReadFile(precious); err != nil || string(body) != "precious\n" {
					t.Fatalf("%s, the file the link named, is %q (%v), want it as it was", precious, body, err)
				}
			}
		}},
	}

	t.Run("an ordinary bundle is written as before", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "gh")
		if _, err := WriteBundle(root, bundle); err != nil {
			t.Fatalf("first WriteBundle: %v", err)
		}
		written := make([]fs.FileInfo, len(files))
		for i, file := range files {
			info, err := os.Lstat(filepath.Join(root, file.rel))
			if err != nil {
				t.Fatal(err)
			}
			written[i] = info
		}
		if changed, err := WriteBundle(root, bundle); err != nil || len(changed) != 0 {
			t.Fatalf("WriteBundle of the bundle the root holds = %q, %v, want no directory changed", changed, err)
		}
		for i, file := range files {
			path := filepath.Join(root, file.rel)
			if info, err := os.Lstat(path); err != nil || !os.SameFile(info, written[i]) {
				t.Fatalf("%s was replaced (%v), want the file the first write made left alone", path, err)
			}
			if body, err := os.ReadFile(path); err != nil || string(body) != file.want {
				t.Fatalf("%s holds %q (%v), want %q", path, body, err, file.want)
			}
		}
	})

	for _, file := range files {
		for _, shape := range shapes {
			t.Run(file.name+" is "+shape.name, func(t *testing.T) {
				root := filepath.Join(t.TempDir(), "gh")
				if _, err := WriteBundle(root, bundle); err != nil {
					t.Fatalf("first WriteBundle: %v", err)
				}
				path := filepath.Join(root, file.rel)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				kept := shape.plant(t, path)
				answered := make(chan error, 1)
				go func() {
					_, err := WriteBundle(root, bundle)
					answered <- err
				}()
				if err := testwait.Answer(t, "WriteBundle over "+path, answered); err != nil {
					t.Fatalf("WriteBundle over %s = %v, want it replaced", path, err)
				}
				if kept != nil {
					kept(t)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if !info.Mode().IsRegular() {
					t.Fatalf("%s is %v after WriteBundle, want a regular file", path, info.Mode())
				}
				if body, err := os.ReadFile(path); err != nil || string(body) != file.want {
					t.Fatalf("%s holds %q (%v) after WriteBundle, want %q", path, body, err, file.want)
				}
			})
		}
	}
}

// EmptyBundle leaves the gh directory of a controller whose gh acts as nobody an empty 0700
// directory, whatever an earlier run left there — a file, a directory holding a file, and a link
// to a directory outside it — even where the root is itself a link: it is resolved once, so the
// directory the link names is what is emptied, and the link left in it is unlinked without being
// followed, so the file in the directory it names stays. Without that resolution the removal is
// refused ("… is a symlink"), and the earlier run's files outlive it.
func TestEmptyBundleEmptiesARootThatIsALinkWithoutFollowingOne(t *testing.T) {
	outside := t.TempDir()
	precious := filepath.Join(outside, "precious")
	if err := os.WriteFile(precious, []byte("kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	named := t.TempDir()
	if err := os.Chmod(named, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(named, HostsFile), []byte(Hosts("ghs_stale")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(named, "acme"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(named, "acme", HostsFile), []byte(Hosts("ghs_acme_stale")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(named, "outside")); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "gh")
	if err := os.Symlink(named, root); err != nil {
		t.Fatal(err)
	}
	if err := EmptyBundle(root); err != nil {
		t.Fatalf("EmptyBundle(%s): %v", root, err)
	}
	if got := entryNames(t, named); len(got) != 0 {
		t.Fatalf("%s, which the root links to, holds %q, want nothing", named, got)
	}
	if info, err := os.Stat(named); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("%s is %v (%v), want a 0700 directory", named, info, err)
	}
	if body, err := os.ReadFile(precious); err != nil || string(body) != "kept\n" {
		t.Fatalf("%s, in the directory the planted link names, is %q (%v), want it kept", precious, body, err)
	}
}
