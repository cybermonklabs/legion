package daemon

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// textsKey is the key every folder of the tests below stands under: textsArm's arm of the
// experiment `shorter`, in bootTextsConfig's project.
const textsKey = "projects.TEXTS.experiments.shorter.variants.folder.texts"

// legionDigest is the digest of Legion's own text at path, a path of a folder of texts
// (prompts.Texts), as a manifest records it.
func legionDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := fs.ReadFile(prompts.Texts(), path)
	if err != nil {
		t.Fatalf("read Legion's %s: %v", path, err)
	}
	return texts.Digest(body)
}

// textsFolder writes a folder of texts in a directory of the test's own and answers its path:
// files by their paths in it, and, where manifest is not nil, a manifest.json recording its
// digests (texts.WriteManifest).
func textsFolder(t *testing.T, files, manifest map[string]string) string {
	t.Helper()
	folder := filepath.Join(t.TempDir(), "texts")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range files {
		file := filepath.Join(folder, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if manifest != nil {
		if err := texts.WriteManifest(folder, manifest); err != nil {
			t.Fatal(err)
		}
	}
	return folder
}

// textsArm is the experiment `shorter`, reaching roles (nil for every issue role), whose arm
// `folder` names folder beside a control that names nothing.
func textsArm(roles []claim.Role, folder string) config.Experiment {
	return config.Experiment{Name: "shorter", Mode: config.ExperimentRandom, Unit: config.ExperimentTree, Roles: roles,
		Variants: []config.Variant{{Name: "folder", Weight: 1, TextsPath: folder}, {Name: "control", Weight: 1}}}
}

// bootTextsConfig is a tmux configuration of the one project TEXTS declaring experiment, with the
// operator bearer's file: what readBoot reads before it reaches the folders. A boot that refuses
// no folder goes on to the host's git, jj and gh, as every daemon test's boot does (TestMain
// names its gh).
func bootTextsConfig(t *testing.T, experiment config.Experiment) config.Config {
	t.Helper()
	token := filepath.Join(t.TempDir(), "operator-token")
	if err := os.WriteFile(token, []byte(testOperatorToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		Project:           "TEXTS",
		OperatorTokenFile: token,
		Runtime:           config.Runtime{Name: "tmux"},
		Projects:          map[string]config.Project{"TEXTS": {Experiments: []config.Experiment{experiment}}},
	}
}

// textsBootRefusal is readBoot's refusal of cfg, as `legion config check` (CheckStart) and `legion
// start` (prepare) both read it, with no host Oh My Pi, which no folder needs.
func textsBootRefusal(cfg config.Config) error {
	_, err := readBoot(cfg, os.LookupEnv, os.Getenv, false, slog.New(slog.DiscardHandler))
	return err
}

// wantTextsFault holds err to the refusal of a folder: the arm's key, then the texts.Fault naming
// folder, file ("" for the folder itself) and cause.
func wantTextsFault(t *testing.T, err error, folder, file string, cause texts.Cause) {
	t.Helper()
	var fault *texts.Fault
	switch {
	case err == nil:
		t.Fatalf("the boot read the folder %s and refused nothing, want %s", folder, cause)
	case !errors.As(err, &fault):
		t.Fatalf("the boot's refusal = %v, want a texts.Fault (%s)", err, cause)
	case fault.Cause != cause || fault.Path != file || fault.Dir != folder:
		t.Fatalf("the boot's refusal = %v, want %s at the file %q of %s", err, cause, file, folder)
	case !strings.HasPrefix(err.Error(), textsKey+": texts folder "+folder+": "):
		t.Fatalf("the boot's refusal = %q, want it to name the arm's key and the folder first", err)
	}
}

// The boot refuses each folder texts.Read refuses, under the arm's key, naming the folder, the
// file where the fault is one file's, and the cause: a path that does not exist, a file in place
// of the folder, a file it cannot read, no manifest, a manifest that is no JSON or holds a key of
// its own or a digest of another form, a file the manifest does not list, one it lists that the
// folder lacks, a path outside the four trees or one that is not clean, and a link that leaves the
// folder. Under kubernetes it refuses too a file a volume that projects items does not project,
// the manifest among them, since a role's pod would not see it, and takes the folder whose every
// file is an item, at the mount or deeper under it.
func TestBootRefusesEachBadTextsFolder(t *testing.T) {
	t.Parallel()
	reviewer := map[string]string{"roles/reviewer.md": "A reviewer of this folder.\n"}
	for _, tc := range []struct {
		name   string
		folder func(t *testing.T) string
		file   string
		cause  texts.Cause
	}{
		{name: "a path that does not exist", cause: texts.Gone,
			folder: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") }},
		{name: "a file in place of the folder", cause: texts.Unreadable,
			folder: func(t *testing.T) string {
				file := filepath.Join(t.TempDir(), "texts")
				if err := os.WriteFile(file, []byte("not a folder\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return file
			}},
		{name: "a file it cannot read", file: "roles/reviewer.md", cause: texts.Unreadable,
			folder: func(t *testing.T) string {
				if os.Geteuid() == 0 {
					t.Skip("root reads a file of mode 000, so this row can show nothing as root")
				}
				folder := textsFolder(t, reviewer, map[string]string{"roles/reviewer.md": legionDigest(t, "roles/reviewer.md")})
				if err := os.Chmod(filepath.Join(folder, "roles", "reviewer.md"), 0); err != nil {
					t.Fatal(err)
				}
				return folder
			}},
		{name: "no manifest", file: texts.ManifestName, cause: texts.NoManifest,
			folder: func(t *testing.T) string { return textsFolder(t, reviewer, nil) }},
		{name: "a manifest that is no JSON", file: texts.ManifestName, cause: texts.ManifestUnreadable,
			folder: func(t *testing.T) string {
				return textsFolder(t, map[string]string{"roles/reviewer.md": "A reviewer.\n", texts.ManifestName: "{\"files\": "}, nil)
			}},
		{name: "a manifest with a key of its own", file: texts.ManifestName, cause: texts.ManifestUnreadable,
			folder: func(t *testing.T) string {
				return textsFolder(t, map[string]string{texts.ManifestName: `{"files": {}, "version": 1}`}, nil)
			}},
		{name: "a digest of another form", file: texts.ManifestName, cause: texts.ManifestUnreadable,
			folder: func(t *testing.T) string {
				return textsFolder(t, reviewer, map[string]string{"roles/reviewer.md": "sha256:abc"})
			}},
		{name: "a file the manifest does not list", file: "roles/planner.md", cause: texts.Unlisted,
			folder: func(t *testing.T) string {
				return textsFolder(t, map[string]string{"roles/reviewer.md": "A reviewer.\n", "roles/planner.md": "A planner.\n"},
					map[string]string{"roles/reviewer.md": legionDigest(t, "roles/reviewer.md")})
			}},
		{name: "a file the manifest lists and the folder lacks", file: "go/reviewer.md", cause: texts.Missing,
			folder: func(t *testing.T) string {
				return textsFolder(t, reviewer, map[string]string{
					"roles/reviewer.md": legionDigest(t, "roles/reviewer.md"), "go/reviewer.md": legionDigest(t, "go/reviewer.md")})
			}},
		{name: "a path outside the four trees", file: "notes.md", cause: texts.BadPath,
			folder: func(t *testing.T) string {
				return textsFolder(t, map[string]string{"roles/reviewer.md": "A reviewer.\n", "notes.md": "Notes.\n"},
					map[string]string{"roles/reviewer.md": legionDigest(t, "roles/reviewer.md")})
			}},
		{name: "a path in the manifest that is not clean", file: "roles/../go/reviewer.md", cause: texts.BadPath,
			folder: func(t *testing.T) string {
				return textsFolder(t, reviewer, map[string]string{
					"roles/reviewer.md": legionDigest(t, "roles/reviewer.md"), "roles/../go/reviewer.md": legionDigest(t, "go/reviewer.md")})
			}},
		{name: "a link that leaves the folder", file: "roles/reviewer.md", cause: texts.LinkEscapes,
			folder: func(t *testing.T) string {
				outside := filepath.Join(t.TempDir(), "reviewer.md")
				if err := os.WriteFile(outside, []byte("A reviewer outside the folder.\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				folder := textsFolder(t, nil, map[string]string{"roles/reviewer.md": legionDigest(t, "roles/reviewer.md")})
				if err := os.MkdirAll(filepath.Join(folder, "roles"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(folder, "roles", "reviewer.md")); err != nil {
					t.Fatal(err)
				}
				return folder
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			folder := tc.folder(t)
			err := textsBootRefusal(bootTextsConfig(t, textsArm([]claim.Role{claim.RoleReviewer}, folder)))
			wantTextsFault(t, err, folder, tc.file, tc.cause)
		})
	}

	t.Run("under kubernetes, a file no projected item carries", func(t *testing.T) {
		t.Parallel()
		files := map[string]string{"roles/reviewer.md": "A reviewer.\n", "go/reviewer.md": "A reviewer's daemon part.\n"}
		folder := textsFolder(t, files, map[string]string{
			"roles/reviewer.md": legionDigest(t, "roles/reviewer.md"), "go/reviewer.md": legionDigest(t, "go/reviewer.md")})
		experiment := textsArm([]claim.Role{claim.RoleReviewer}, folder)
		// mounted is the configuration whose ConfigMap volume projects items at mountPath, each
		// item's key its path with `/` as `-`, a key holding no `/`.
		mounted := func(mountPath string, items ...string) config.Config {
			cfg := bootTextsConfig(t, experiment)
			projected := make([]corev1.KeyToPath, 0, len(items))
			for _, item := range items {
				projected = append(projected, corev1.KeyToPath{Key: strings.ReplaceAll(item, "/", "-"), Path: item})
			}
			cfg.Runtime = config.Runtime{Name: "kubernetes", Kubernetes: &config.Kubernetes{Pod: config.PodConfig{
				Volumes: []corev1.Volume{{Name: "operator-texts", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "legion-operator-texts"}, Items: projected}}}},
				VolumeMounts: []corev1.VolumeMount{{Name: "operator-texts", MountPath: mountPath, ReadOnly: true}},
			}}}
			return cfg
		}
		// The folder's arm is read as readBoot reads every declared one; readBoot itself would
		// first hold the pod block to the sandbox's refusals (checkOperatorConfig), which refuse
		// a mount under a directory every Legion pod mounts for itself, as a test's own
		// temporary directory may be.
		read := func(cfg config.Config) error {
			declared := cfg.ExperimentsDeclared()
			return readArmTexts(cfg, declared[1], declared[1].Variants[0])
		}
		for _, row := range []struct {
			name  string
			items []string
			want  string
		}{
			{"a role part", []string{"manifest.json", "roles/reviewer.md"},
				textsKey + " entry " + folder + "/go/reviewer.md is not among the items volume operator-texts projects at " + folder + " (manifest.json, roles/reviewer.md): add an item with path go/reviewer.md"},
			{"the manifest", []string{"go/reviewer.md", "roles/reviewer.md"},
				textsKey + " entry " + folder + "/manifest.json is not among the items volume operator-texts projects at " + folder + " (go/reviewer.md, roles/reviewer.md): add an item with path manifest.json"},
		} {
			if err := read(mounted(folder, row.items...)); err == nil || err.Error() != row.want {
				t.Errorf("%s no item projects: the boot's read = %v, want %q", row.name, err, row.want)
			}
		}
		if err := read(mounted(folder, "manifest.json", "roles/reviewer.md", "go/reviewer.md")); err != nil {
			t.Errorf("every file an item at the folder's mount: the boot's read = %v, want none", err)
		}
		if err := read(mounted(filepath.Dir(folder), "texts/manifest.json", "texts/roles/reviewer.md", "texts/go/reviewer.md")); err != nil {
			t.Errorf("every file an item of a mount above the folder: the boot's read = %v, want none", err)
		}
	})
}

// A role part at a path of Legion's that no launch of a role the experiment reaches reads is
// refused, naming the file: the planner's part under an experiment of the reviewer alone, the
// part only Claude Code roles are told while every role runs on Oh My Pi, the controller's prompt,
// which no experiment reaches, and a part no launch reads at all. The read looks past the digest,
// so a part that is both unread and recorded at another digest is refused for being unread.
func TestBootRefusesAPartNoReachedRoleReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		roles  []claim.Role
		file   string
		digest func(t *testing.T) string
	}{
		{name: "the planner's part under an experiment of the reviewer", roles: []claim.Role{claim.RoleReviewer}, file: "roles/planner.md"},
		{name: "the Claude Code roles' mechanics", file: "go/claude-mechanics.md"},
		{name: "the controller's prompt", file: "roles/controller-root.md"},
		{name: "a part no launch reads", file: "roles/core/oracle.md"},
		{name: "the planner's part recorded at another digest", roles: []claim.Role{claim.RoleReviewer}, file: "roles/planner.md",
			digest: func(*testing.T) string { return texts.Digest([]byte("the planner's part of another release\n")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			digest := legionDigest(t, tc.file)
			if tc.digest != nil {
				digest = tc.digest(t)
			}
			folder := textsFolder(t,
				map[string]string{"roles/reviewer.md": "A reviewer.\n", tc.file: "A part of this folder.\n"},
				map[string]string{"roles/reviewer.md": legionDigest(t, "roles/reviewer.md"), tc.file: digest})
			err := textsBootRefusal(bootTextsConfig(t, textsArm(tc.roles, folder)))
			wantTextsFault(t, err, folder, tc.file, texts.Unread)
		})
	}
}

// A folder cut from another release of Legion's texts starts the daemon: a role part recorded at
// a digest that is not Legion's, a path where Legion has no text, and an agent recorded at any
// digest are each a launch's to fall back from, so a new release never stops a deployment whose
// folders were cut from the one before it.
func TestBootAcceptsAStaleFolder(t *testing.T) {
	t.Parallel()
	folder := textsFolder(t,
		map[string]string{
			"roles/reviewer.md":       "A reviewer of another release.\n",
			"roles/no-such-part.md":   "A part Legion has never had.\n",
			"agents/goal-red-team.md": "---\nname: goal-red-team\n---\nA red team of another release.\n",
		},
		map[string]string{
			"roles/reviewer.md":       texts.Digest([]byte("the reviewer's part of another release\n")),
			"roles/no-such-part.md":   texts.Digest([]byte("A part Legion has never had.\n")),
			"agents/goal-red-team.md": texts.Digest([]byte("the red team of another release\n")),
		})
	if err := textsBootRefusal(bootTextsConfig(t, textsArm([]claim.Role{claim.RoleReviewer}, folder))); err != nil {
		t.Fatalf("the boot over a stale folder = %v, want no refusal", err)
	}
}
