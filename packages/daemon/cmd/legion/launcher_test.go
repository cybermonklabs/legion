package main

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

// syncBuffer is a bytes.Buffer the launcher writes from its role goroutines while a test reads it.
type syncBuffer = testlog.Buffer

// An issue pod's one container runs `legion launcher --roles-dir <dir> --role <r>…`, the command
// the daemon's pod builder renders: one connection per role, each with the token in that role's
// own directory, the pod's uid from POD_UID as the single-role launcher always took it, and a
// log whose lines each name their role.
func TestTheLauncherCommandServesEveryRoleOfARolesDirectory(t *testing.T) {
	rolesDir := t.TempDir()
	for _, role := range []string{"tester", "reviewer"} {
		for _, name := range []string{"launcher", "private"} {
			if err := os.MkdirAll(filepath.Join(rolesDir, role, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(rolesDir, role, "launcher", "LAUNCHER_TOKEN"), []byte("token-"+role+"\n"), 0o400); err != nil {
			t.Fatal(err)
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("POD_UID", "pod-7")
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr syncBuffer
	exited := make(chan int, 1)
	go func() {
		exited <- run(ctx, []string{
			"legion", "launcher", "--connect", "tcp://" + listener.Addr().String(), "--sandbox", "legion-acme-1",
			"--roles-dir", rolesDir, "--role", "tester", "--role", "reviewer", "--stop-grace", "2s",
		}, &stdout, &stderr)
	}()

	hellos := map[string]shimwire.LauncherHello{}
	for range 2 {
		conn, err := listener.Accept()
		if err != nil {
			t.Fatalf("accept: %v; the launcher's log: %s", err, stderr.String())
		}
		t.Cleanup(func() { _ = conn.Close() })
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		frame, err := shimwire.Decode(line)
		if err != nil {
			t.Fatal(err)
		}
		hello, ok := frame.(shimwire.LauncherHello)
		if !ok {
			t.Fatalf("first frame = %#v, want a launcher hello", frame)
		}
		hellos[hello.Role] = hello
		if err := shimwire.NewWriter(conn).WriteFrame(shimwire.LauncherHelloAck{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"tester", "reviewer"} {
		hello := hellos[role]
		if hello.Token != "token-"+role || hello.Sandbox != "legion-acme-1" || hello.PodUID != "pod-7" {
			t.Fatalf("role %s hello = %#v, want its own token, the sandbox and POD_UID's pod", role, hello)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr.String(), "tester: launcher: connected") || !strings.Contains(stderr.String(), "reviewer: launcher: connected") {
		if time.Now().After(deadline) {
			t.Fatalf("the launcher logged %q, want each role's acknowledged hello behind its role's name", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-exited:
		if code != 0 {
			t.Fatalf("exit %d at the container's stop; stderr: %s", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the launcher did not leave at the container's stop")
	}
}

// The two forms do not mix: a roles directory holds every role's own token and private
// directory, and the controller pod's single form names one role. Each mixture is a usage error
// before anything is dialled.
func TestTheLauncherCommandRefusesAMixOfItsTwoForms(t *testing.T) {
	token := filepath.Join(t.TempDir(), "LAUNCHER_TOKEN")
	if err := os.WriteFile(token, []byte("token"), 0o400); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"a roles directory and a token file", []string{"--roles-dir", t.TempDir(), "--role", "tester", "--token-file", token}, "no --token-file or --private-dir"},
		{"a roles directory and a private directory", []string{"--roles-dir", t.TempDir(), "--role", "tester", "--private-dir", t.TempDir()}, "no --token-file or --private-dir"},
		{"two roles without a roles directory", []string{"--token-file", token, "--role", "tester", "--role", "reviewer", "--private-dir", t.TempDir()}, "several roles need --roles-dir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A launcher that took the mixture would dial the unreachable daemon until its
			// context ends; the bound makes that a failure of this test, not a hung run.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			args := append([]string{"legion", "launcher", "--connect", "tcp://127.0.0.1:1", "--sandbox", "s", "--pod-uid", "p"}, tc.args...)
			if code := run(ctx, args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q; want 2 naming %q", code, stderr.String(), tc.want)
			}
		})
	}
}

// A word under `legion launcher` names a command of its or is a usage error naming the word, even
// when a flag the launcher does not have follows it: `launcher role-inix --generation 1` is a
// mistyped role-init, and naming --generation would send the reader to the wrong word.
func TestTheLauncherCommandRefusesAWordItDoesNotKnowByTheWord(t *testing.T) {
	for _, args := range [][]string{
		{"role-inix"},
		{"role-inix", "--generation", "1"},
		{"--connect", "tcp://127.0.0.1:1", "role-inix", "--status-fd", "3"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var stdout, stderr bytes.Buffer
		code := run(ctx, append([]string{"legion", "launcher"}, args...), &stdout, &stderr)
		cancel()
		if want := `legion launcher: unknown command "role-inix" for "legion launcher"`; code != 2 || !strings.Contains(stderr.String(), want) || strings.Contains(stderr.String(), "unknown flag") {
			t.Errorf("legion launcher %q = %d, stderr %q; want 2 naming the word: %q", args, code, stderr.String(), want)
		}
	}
}

// A service account to impersonate names a session database, so the launcher told one without the
// instance, the database and the login refuses before it dials anything. A launcher that did not
// know the flag would exit 2 on its usage, and one that parsed it and dropped it would take the
// configuration for the other stores and dial the unreachable daemon until its context ends.
func TestTheLauncherCommandRefusesAServiceAccountWithoutItsSessionDatabase(t *testing.T) {
	token := filepath.Join(t.TempDir(), "LAUNCHER_TOKEN")
	if err := os.WriteFile(token, []byte("token"), 0o400); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{
		"legion", "launcher", "--connect", "tcp://127.0.0.1:1", "--sandbox", "s", "--pod-uid", "p",
		"--token-file", token, "--role", "tester", "--private-dir", t.TempDir(),
		"--session-impersonate-service-account", "legion-sessions@example-project.iam.gserviceaccount.com",
	}, &stdout, &stderr)
	const want = "the session database is named in part: no --session-instance, no --session-database, no --session-user"
	if code != 1 || !strings.Contains(stderr.String(), want) {
		t.Fatalf("exit %d, stderr %q; want 1 naming %q", code, stderr.String(), want)
	}
}

// The metadata endpoint is a microVM pod's alone, in either of the launcher's forms, and named
// whole: both of its flags, with --microvm. Each other mixture is refused before anything is
// dialled or listened on, naming what it lacks, which proves each flag reaches the launcher: one
// it did not know would exit 2 on its usage, and one it dropped would change the refusal or dial
// the unreachable daemon until the context ends. The controller's form without --microvm is
// refused as an issue pod's is, so the metadata flags reach that form too. Its acceptance with
// --microvm is TestMetadataIsRefusedOutsideAMicroVMPodOrNamedInPart's (internal/launcher), through
// the configuration's own check: an accepted run here would start the launcher, which listens on
// the endpoint's fixed address and names it in this process's environment.
func TestTheLauncherCommandRefusesTheMetadataEndpointOutsideAMicroVMPod(t *testing.T) {
	token := filepath.Join(t.TempDir(), "LAUNCHER_TOKEN")
	if err := os.WriteFile(token, []byte("token"), 0o400); err != nil {
		t.Fatal(err)
	}
	const (
		pool     = "example-project.svc.id.goog"
		provider = "https://container.googleapis.com/v1/projects/example-project/locations/example-location/clusters/example-cluster"
	)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"the pool alone", []string{"--roles-dir", t.TempDir(), "--role", "tester", "--microvm", "--metadata-workload-pool", pool},
			"the metadata endpoint is named in part: no --metadata-identity-provider"},
		{"the identity provider alone", []string{"--roles-dir", t.TempDir(), "--role", "tester", "--microvm", "--metadata-identity-provider", provider},
			"the metadata endpoint is named in part: no --metadata-workload-pool"},
		{"a gVisor pod", []string{"--roles-dir", t.TempDir(), "--role", "tester", "--metadata-workload-pool", pool, "--metadata-identity-provider", provider},
			"the metadata endpoint serves a microVM's pod: it needs --microvm"},
		{"the controller's form in a gVisor pod", []string{"--token-file", token, "--role", "controller", "--private-dir", t.TempDir(), "--metadata-workload-pool", pool, "--metadata-identity-provider", provider},
			"the metadata endpoint serves a microVM's pod: it needs --microvm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var stdout, stderr bytes.Buffer
			args := append([]string{"legion", "launcher", "--connect", "tcp://127.0.0.1:1", "--sandbox", "s", "--pod-uid", "p"}, tc.args...)
			if code := run(ctx, args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("exit %d, stderr %q; want 1 naming %q", code, stderr.String(), tc.want)
			}
		})
	}
}

// `legion launcher role-init` is the launcher's own, hidden from its usage, and refuses to run
// anywhere but as the init of a role's new namespaces: run by hand it would otherwise mount a
// role's private view over the caller's own. Its help is read on stdout, where cobra prints it
// (newRoot), and on stderr. Every refusal, its usage's included, is a failure before the shim
// started, so it exits launcher.RoleInitSetupFailedExit, the one status the launcher reports as a
// setup failure, never the 2 a panic exits with.
func TestRoleInitIsHiddenAndRefusesToRunOutsideARolesNamespaces(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"legion", "launcher", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "Usage:") || strings.Contains(stdout.String()+stderr.String(), "role-init") {
		t.Fatalf("launcher --help: exit %d, stdout %q, stderr %q; want its usage on stdout without the hidden role-init", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"legion", "launcher", "role-init", "--roles-dir", "/r", "--role", "a", "--status-fd", "3"}, &stdout, &stderr); code != launcher.RoleInitSetupFailedExit {
		t.Fatalf("role-init with no shim argv: exit %d, %q; want its usage and exit %d", code, stderr.String(), launcher.RoleInitSetupFailedExit)
	}
	stderr.Reset()
	if code := run(context.Background(), []string{"legion", "launcher", "role-init", "--no-such-flag"}, &stdout, &stderr); code != launcher.RoleInitSetupFailedExit {
		t.Fatalf("role-init with a flag it does not know: exit %d, %q; want %d", code, stderr.String(), launcher.RoleInitSetupFailedExit)
	}
	stderr.Reset()
	code := run(context.Background(), []string{"legion", "launcher", "role-init", "--roles-dir", t.TempDir(), "--role", "a", "--generation", "1", "--status-fd", "3", "--", "/bin/true"}, &stdout, &stderr)
	if code != launcher.RoleInitSetupFailedExit || !strings.Contains(stderr.String(), "role-init: not the init of a PID namespace of its own") {
		t.Fatalf("role-init outside a role's namespaces: exit %d, %q; want a refusal with exit %d", code, stderr.String(), launcher.RoleInitSetupFailedExit)
	}
}

// `legion launcher volume-trim-delete` is the launcher's second hidden subcommand, the deleter
// one sweep of the pod's cache trimmer makes its removals through. It takes every directory it
// covers — the roles directory, the directory the container mounts the home's volumes under and
// the home, which the trimmer passes it as role-init is given them — and it refuses to run
// anywhere but as the init of a PID namespace of its own: run by hand it holds no descriptor of
// the volume, and a flag it does not know would end it with its usage and exit 2 instead of the
// refusal below. Its help is read on stdout, where cobra prints it (newRoot), and on stderr. The
// command line the launcher itself writes, with all four directories of the cover, is the case
// after this one.
func TestVolumeTrimDeleteIsHiddenAndTakesEveryDirectoryItCovers(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"legion", "launcher", "--help"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "Usage:") || strings.Contains(stdout.String()+stderr.String(), "volume-trim-delete") {
		t.Fatalf("launcher --help: exit %d, stdout %q, stderr %q; want its usage on stdout without the hidden volume-trim-delete", code, stdout.String(), stderr.String())
	}
	stderr.Reset()
	code = run(context.Background(), []string{
		"legion", "launcher", "volume-trim-delete",
		"--roles-dir", "/var/run/legion/roles", "--home-mounts", "/var/run/legion/home", "--home", "/home/legion",
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "volume-trim-delete: not the init of a PID namespace of its own") {
		t.Fatalf("volume-trim-delete outside its own namespaces: exit %d, %q; want exit 1 and that refusal, with every directory it covers taken as a flag", code, stderr.String())
	}
}

// The command line the launcher runs its deleter with is read by `legion launcher
// volume-trim-delete` whole: every flag the trimmer writes for the pod's cover
// (launcher.VolumeTrimDeleteArgv, which answers what trimDeleteLaunch builds) is one cobra reads
// for that command, so the words after `launcher volume-trim-delete` reach the refusal of a
// process that is not the init of a PID namespace of its own, exit 1. A flag the command does not
// register would end it with its usage and exit 2, and every sweep of the pod would then delete
// nothing. The launcher package reads the same command line back with RoleCoverFlags on a
// flag.FlagSet (TestTheDeletersCommandLineNamesEveryDirectoryItCovers); this case reads it
// through the command itself.
func TestTheLaunchersCommandLineForItsDeleterIsOneVolumeTrimDeleteReads(t *testing.T) {
	cover := launcher.RoleCover{
		RolesDir: "/var/run/legion/roles", Home: "/home/legion",
		HomeMounts: "/var/run/legion/home", FixedRoot: "/var/run/legion",
	}
	argv, err := launcher.VolumeTrimDeleteArgv(cover)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) < 3 || argv[1] != "launcher" || argv[2] != launcher.VolumeTrimDeleteSubcommand {
		t.Fatalf("the launcher runs its deleter as %q, want this binary's launcher %s", argv, launcher.VolumeTrimDeleteSubcommand)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "launcher", launcher.VolumeTrimDeleteSubcommand}, argv[3:]...), &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "volume-trim-delete: not the init of a PID namespace of its own") {
		t.Fatalf("legion launcher volume-trim-delete %q, the launcher's own command line: exit %d, %q; want exit 1 and the refusal of a process that is not the init of a PID namespace of its own, every flag read", argv[3:], code, stderr.String())
	}
}
