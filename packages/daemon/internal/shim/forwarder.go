package shim

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

// forwarderRestartDelay is the wait before a forwarder that exited is started again.
const forwarderRestartDelay = 2 * time.Second

// forwarderStopWait is how long the shim waits, once its loop has ended, for its forwarder to
// revoke its grants and exit on the SIGTERM it is sent, before it is killed: inside the role's
// stop grace, which the daemon requires to be at least nine seconds of a pod's.
const forwarderStopWait = 4 * time.Second

// startForwarder runs the role's GitHub forwarder (Config.GitHubForwarder, `legion forwarder`) as a
// child of the shim, once the role's key exists: the forwarder asks the broker for the role's grant
// with that key, once the daemon has enrolled it. It is restarted after forwarderRestartDelay
// whenever it exits, until the stop begins (s.commands ends, with the shim's own stop, the agent's
// SIGTERM or the loop's end): at the stop's start role-init signals every process of the role,
// the forwarder among them, which revokes its grants and exits, and a new one then would ask for
// a fresh grant while the role goes away. A forwarder still running at the stop keeps running
// until the loop ends, which sends it SIGTERM and gives it forwarderStopWait to revoke the grants
// it holds; the shim's Run waits for it. It runs in the shim's own environment, where the runtime
// set AGENT_SECRETS_URL and AGENT_SECRETS_KEY_DIR, and its lines go to the shim's stderr, beside
// the renewer's. One forwarder per shim: a later call starts no second one.
func (s *shim) startForwarder() {
	argv := s.cfg.GitHubForwarder
	if len(argv) == 0 {
		return
	}
	s.mu.Lock()
	if s.forwarding {
		s.mu.Unlock()
		return
	}
	s.forwarding = true
	s.mu.Unlock()
	s.forwarders.Add(1)
	go func() {
		defer s.forwarders.Done()
		for s.commands.Err() == nil {
			cmd := exec.CommandContext(s.loop, argv[0], argv[1:]...)
			cmd.Env = s.cfg.Env
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
			cmd.WaitDelay = forwarderStopWait
			// Through the shim's guard, as the renewer starts: a shim that reaps orphans
			// (Config.ReapOrphans) must not take the forwarder's exit before cmd.Wait does.
			pid, err := s.children.Start(cmd)
			if err != nil {
				s.log.Printf("[worker-shim] forwarder could not start: %v; retrying in %s", err, forwarderRestartDelay)
			} else {
				s.log.Printf("[worker-shim] forwarder started (pid %d)", pid)
				err := cmd.Wait()
				s.children.Done(pid)
				if s.commands.Err() != nil {
					s.log.Printf("[worker-shim] forwarder exited (%v) during the stop; not starting it again", err)
					return
				}
				s.log.Printf("[worker-shim] forwarder exited (%v); restarting in %s", err, forwarderRestartDelay)
			}
			select {
			case <-s.clock.After(forwarderRestartDelay):
			case <-s.commands.Done():
				return
			}
		}
	}()
}
