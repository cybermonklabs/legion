//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// unitVariables is every AGENT_SECRETS_* variable besides the broker URL that install writes
// into the unit, in the order the unit lists them. path marks the ones whose value is a
// filesystem path, which must be absolute because the unit's helper runs in another working
// directory than install does; the rest are an identity's own words — a kind, an audience, a
// pool, an account, an issuer, a client, a person, a session name — which have no directory to
// be resolved from. Each is copied from install's own environment, except the sign-in directory,
// which is resolved there (unitValue).
//
// The identity and sign-in half is not optional decoration: a helper started without
// AGENT_SECRETS_MACHINE_IDENTITY proves no identity of its own and falls back to the
// device-grant machine login, and one started without the key directory and the sign-in client
// enrols every session under this machine rather than under the person who ran
// `agent-secrets login` on it.
var unitVariables = []struct {
	name string
	path bool
}{
	{name: "AGENT_SECRETS_HELPER_SOCK", path: true},
	{name: "AGENT_SECRETS_OPERATOR_FILE", path: true},
	{name: "AGENT_SECRETS_MACHINE_FILE", path: true},
	{name: "AGENT_SECRETS_KEY_DIR", path: true},
	{name: brokerclient.SignInDirVar, path: true},
	{name: brokerclient.IdentityVar},
	{name: brokerclient.MachineTokenFileVar, path: true},
	{name: brokerclient.MachineAudienceVar},
	{name: brokerclient.PoolAudienceVar},
	{name: brokerclient.PoolAccountVar},
	{name: brokerclient.SignInIssuerVar},
	{name: brokerclient.SignInClientIDVar},
	{name: brokerclient.SignInClientSecretFileVar, path: true},
	{name: brokerclient.PersonVar},
	{name: brokerclient.SessionVar},
}

// unitValue is what the unit says `name` is: this process's own value for it, and for the
// sign-in directory the directory this process's `agent-secrets login` would write to, resolved
// here (brokerclient.SignIns) rather than copied. The store falls back to
// $XDG_STATE_HOME/agent-secrets, and the unit carries neither this shell's
// AGENT_SECRETS_SIGNIN_DIR nor its XDG_STATE_HOME, so a box whose XDG_STATE_HOME is not the
// default would have `login` write one directory and the unit's helper read another — and every
// session on it would silently enrol as the machine rather than as the person signed in.
func unitValue(name string, getenv func(string) string) string {
	if name == brokerclient.SignInDirVar {
		return brokerclient.SignIns(getenv).Dir
	}
	return getenv(name)
}

// install writes the helper's systemd user unit for the binary at exe (symlinks resolved), the
// broker URL from --url or AGENT_SECRETS_URL, and every other AGENT_SECRETS_* variable of its
// environment the unit's helper needs (unitVariables): the socket install probes, the operator
// and machine files the caller named, the key directory, the sign-in directory whose records
// say whose authority a session enrols under — always written, and resolved rather than copied
// (unitValue) — and this machine's own identity at the broker's token exchange with
// the sign-in client a person logs in with. Without those last the installed helper falls back
// to the device-grant machine login, which a broker with BROKER_MACHINE_LOGIN_URL unset does
// not serve, and ignores every `agent-secrets login` on the box. It works in
// three steps. First the preflight: a helper
// answering on its socket while the unit is not active was not started by it, and starting the
// unit would put a second helper on that socket, so install refuses, listing that helper's
// sessions, and writes and starts nothing, whether or not the unit file already matches. Then
// it writes the unit when its bytes differ, and runs `systemctl --user daemon-reload` and
// `enable --now` when it wrote it or the unit is not active. Last the restart: a helper that was
// answering before is restarted when restartWithSessions is set, whether or not the unit
// changed; otherwise only when the unit changed and `drain` finds it has no sessions (the
// helper then refuses new ones). A restart ends the sessions' grants, so with sessions it prints
// them and why it did not restart, and returns nil. The restart is bounded (restartTimeout).
// After a drain install reads the unit's main process before and after the restart, and when
// the restart did not replace it — the same main process, none, or a read that failed, as after
// a restart that failed or outlived its bound — install sends `undrain`, so the helper takes
// sessions again, and returns the restart's error, or one naming the unchanged process after a
// restart systemd answered success. A helper older
// than `drain` cannot refuse a register between the check and the restart, so without
// restartWithSessions it is never restarted: install prints its sessions and that it predates
// drain, and returns nil. An unchanged, active unit without restartWithSessions changes nothing.
func install(args []string, getenv func(string) string, exe string, stdout io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	url := fs.String("url", "", "the broker's base URL (default $AGENT_SECRETS_URL)")
	restartWithSessions := fs.Bool("restart-with-sessions", false, "restart the helper even when sessions are registered")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		*url = getenv("AGENT_SECRETS_URL")
	}
	// Every trailing slash, the one rule the clients read the address by, and before the check
	// below: a URL of nothing but slashes is no broker URL at all and is refused here rather
	// than written into the unit as an empty value.
	*url = brokerclient.WithoutTrailingSlashes(*url)
	if *url == "" {
		return errors.New("install needs the broker's URL: pass --url URL or set AGENT_SECRETS_URL")
	}
	const needsEscaping = " \t\r\n\"'\\%$;"
	if strings.ContainsAny(*url, needsEscaping) {
		return fmt.Errorf("broker URL %q holds characters a unit's Environment= line would need escaped", redact.URL(*url))
	}
	environment := "Environment=AGENT_SECRETS_URL=" + *url + "\n"
	for _, v := range unitVariables {
		value := strings.TrimSpace(unitValue(v.name, getenv))
		if value == "" {
			continue
		}
		if strings.ContainsAny(value, needsEscaping) {
			return fmt.Errorf("%s %q holds characters a unit's Environment= line would need escaped", v.name, value)
		}
		// The unit's helper runs in another working directory than install does.
		if v.path && !filepath.IsAbs(value) {
			return fmt.Errorf("%s %q is a relative path, which the unit's helper would resolve from another directory; set it to an absolute path", v.name, value)
		}
		environment += "Environment=" + v.name + "=" + value + "\n"
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve this binary's path: %w", err)
	}
	// ExecStart splits on whitespace and expands % specifiers and $ variables.
	if strings.ContainsAny(resolved, needsEscaping) {
		return fmt.Errorf("this binary's path %q holds characters ExecStart would split or expand; install it at a plain path", resolved)
	}
	unit := []byte(`[Unit]
Description=agent-secrets helper: pins agent sessions by pidfd and signs their secrets-broker proofs
[Service]
ExecStart=` + resolved + ` serve
` + environment + `# A SIGTERM from anything but systemd ends the helper with exit 0, so restart on any exit.
Restart=always
RestartSec=10
[Install]
WantedBy=default.target
`)

	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	running, err := helper.Call(cfg.Socket, helper.Request{Op: "sessions"}, 2*time.Second)
	wasRunning := err == nil
	active := unitActive()
	if wasRunning && !active {
		for _, s := range running.Sessions {
			fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\n", s.PID, s.RuntimeID, s.EnrollmentID, s.State)
		}
		return fmt.Errorf("an agent-secrets-helper not run by %s answers at %s (its sessions are listed above); stop it, then re-run install", helper.UnitName, cfg.Socket)
	}
	path := helper.UnitPath(getenv)
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	changed := !bytes.Equal(old, unit) || errors.Is(err, os.ErrNotExist)
	if changed {
		if err := writeFileAtomic(path, unit); err != nil {
			return err
		}
	} else if err := os.Chmod(path, 0o600); err != nil {
		// A unit an older install wrote 0644 holds the same URL, credential included.
		return err
	}
	if changed || !active {
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		if err := systemctl("enable", "--now", helper.UnitName); err != nil {
			return err
		}
	}
	if !wasRunning || (!changed && !*restartWithSessions) {
		return nil
	}
	drained := false
	if !*restartWithSessions {
		var sessions []helper.SessionInfo
		sessions, drained, err = drainHelper(cfg.Socket)
		if err != nil {
			return err
		}
		for _, s := range sessions {
			fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\n", s.PID, s.RuntimeID, s.EnrollmentID, s.State)
		}
		if !drained {
			fmt.Fprintln(stdout, "not restarted: the running agent-secrets-helper predates drain, so a session could start between the check and the restart. Re-run with --restart-with-sessions at a quiet moment.")
			return nil
		}
		if n := len(sessions); n > 0 {
			fmt.Fprintf(stdout, "not restarted: %d sessions are registered; a restart ends their grants. Re-run with --restart-with-sessions.\n", n)
			return nil
		}
	}
	if !drained {
		return restartHelper()
	}
	// The unit's main process before and after the restart says whether the restart replaced the
	// helper install drained. When it did not, or install cannot tell, install undoes the drain:
	// the helper would otherwise refuse every registration until its drain bound passes, and an
	// undrain that reaches a new helper changes nothing. It does not while a job for the unit is
	// still queued, as after a restart that outlived restartTimeout, whose job systemd keeps when
	// its client is killed: a session that registered after an undrain would lose its grants when
	// that job stops the helper, which is what the drain is for. The drain then ends with that
	// restart, or at the helper's own bound (helper.DefaultDrainBound).
	before, beforeErr := mainPID()
	restartErr := restartHelper()
	after, afterErr := mainPID()
	var notReplaced string
	switch {
	case beforeErr != nil:
		notReplaced = "its main process could not be read before the restart: " + beforeErr.Error()
	case afterErr != nil:
		notReplaced = "its main process could not be read after the restart: " + afterErr.Error()
	case after == 0:
		notReplaced = "the unit runs no main process after the restart"
	case after == before:
		notReplaced = fmt.Sprintf("the unit's main process is still %d", after)
	default:
		return restartErr
	}
	var queued string
	switch job, err := queuedJob(); {
	case err != nil:
		queued = "whether a job for the unit is still queued could not be read: " + err.Error()
	case job != "":
		queued = "job " + job + " for the unit is still queued"
	}
	if queued != "" {
		fmt.Fprintf(stdout, "the helper is left draining (refusing new sessions): %s, and an undrain now would let a session register on the helper that job may stop; the drain ends with the restart, or by itself within %s of the drain\n", queued, helper.DefaultDrainBound)
		if restartErr != nil {
			return fmt.Errorf("%w; %s, so install did not undrain the helper", restartErr, queued)
		}
		return fmt.Errorf("systemctl --user restart %s answered success, but %s and %s, so install did not undrain the helper", helper.UnitName, notReplaced, queued)
	}
	if resp, uerr := helper.Call(cfg.Socket, helper.Request{Op: "undrain"}, 2*time.Second); uerr != nil || !resp.OK {
		fmt.Fprintf(stdout, "the helper may still be draining (refusing new sessions): undrain failed: %v %s; it ends the drain itself within %s of the drain\n", uerr, resp.Error, helper.DefaultDrainBound)
	}
	if restartErr != nil {
		return restartErr
	}
	return fmt.Errorf("systemctl --user restart %s answered success, but %s, so the helper install drained was not restarted; install sent it undrain", helper.UnitName, notReplaced)
}

// restartTimeout bounds install's `systemctl --user restart` of the helper
// (helper.InstallRestartTimeout, which says why 30 s). A variable only so a test can shorten it.
var restartTimeout = helper.InstallRestartTimeout

// restartHelper runs `systemctl --user restart` on the helper's unit, and stops waiting for it at
// restartTimeout.
func restartHelper() error {
	ctx, cancel := context.WithTimeout(context.Background(), restartTimeout)
	defer cancel()
	err := systemctlContext(ctx, "restart", helper.UnitName)
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("systemctl --user restart %s did not finish within %s", helper.UnitName, restartTimeout)
	}
	return err
}

// systemctlReadTimeout bounds each read of the unit's state, one D-Bus property read that answers
// at once from a working user manager. Three of them (the main process before and after the
// restart, and its queued job) and the 2 s undrain fit, with InstallRestartTimeout, inside
// helper.DefaultDrainBound.
const systemctlReadTimeout = 5 * time.Second

// unitProperty is the helper unit's property as `systemctl --user show --value` prints it,
// trimmed.
func unitProperty(property string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), systemctlReadTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property="+property, "--value", helper.UnitName).Output()
	if err != nil {
		return "", fmt.Errorf("systemctl --user show --property=%s %s: %w", property, helper.UnitName, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// mainPID is the helper unit's main process as `systemctl --user show` names it, 0 while the
// unit runs none.
func mainPID() (int, error) {
	value, err := unitProperty("MainPID")
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("systemctl --user show --property=MainPID %s answered %q, which is no process id", helper.UnitName, value)
	}
	return pid, nil
}

// queuedJob is the id of the job systemd holds for the helper unit, a restart or any other, and
// "" while it holds none: `systemctl --user show --property=Job --value` prints the job's id, or
// nothing when there is no job.
func queuedJob() (string, error) {
	value, err := unitProperty("Job")
	if err != nil {
		return "", err
	}
	if _, err := strconv.ParseUint(value, 10, 32); value != "" && err != nil {
		return "", fmt.Errorf("systemctl --user show --property=Job %s answered %q, which is no job id", helper.UnitName, value)
	}
	return value, nil
}

// unitActive reports whether `systemctl --user is-active` names the helper's unit active.
func unitActive() bool {
	out, err := exec.Command("systemctl", "--user", "is-active", helper.UnitName).Output()
	return err == nil && strings.TrimSpace(string(out)) == "active"
}

// drainHelper asks the helper at sock to drain: it answers its sessions and, with none, refuses
// every later register until it exits, so none can start between the answer and the restart; a
// helper of this release also ends that drain itself once helper.DefaultDrainBound passes with it
// still running. A helper older than the drain op answers it as an unknown op; drainHelper then
// answers that helper's `sessions` and drained false.
func drainHelper(sock string) (sessions []helper.SessionInfo, drained bool, err error) {
	resp, err := helper.Call(sock, helper.Request{Op: "drain"}, 2*time.Second)
	drained = true
	if err == nil && resp.Code == helper.CodeBadRequest && strings.HasPrefix(resp.Error, "unknown op") {
		drained = false
		resp, err = helper.Call(sock, helper.Request{Op: "sessions"}, 2*time.Second)
	}
	if err != nil {
		return nil, false, fmt.Errorf("agent-secrets-helper stopped answering at %s: %w", sock, err)
	}
	if !resp.OK {
		return nil, false, fmt.Errorf("agent-secrets-helper: %s: %s", resp.Code, resp.Error)
	}
	return resp.Sessions, drained, nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// 0600: the unit's Environment= line holds AGENT_SECRETS_URL, and with it any credential
	// that URL carries.
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func systemctl(args ...string) error {
	return systemctlContext(context.Background(), args...)
}

// systemctlContext runs `systemctl --user args...` until it exits or ctx ends, whichever is first.
func systemctlContext(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}
