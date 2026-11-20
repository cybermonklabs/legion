// packages/envoy/cmd/agent-secrets/chain.go
//
// Where every session form takes this process's broker identity from: the first of four sources
// that has one for it, in this order, as the AWS SDK takes a credential from the first provider
// in its chain that holds one.
//
//  1. key-dir: the key.pem and enrollment a launcher put in the key directory (a box, a pod).
//  2. sign-in: a person's `agent-secrets login` on this machine.
//  3. helper: agent-secrets-helper, for a caller inside a session it registered.
//  4. machine: the machine's own cloud identity, found with no setting (machineSource), whose
//     session the machine cache keeps.
//
// A source that has nothing for the caller answers errSourceAbsent with its reason and the next
// is tried; a helper that answers that the caller is in no session it registered is such a
// source. A source that is present but broken, and any refusal of the broker's, stops the
// command with that error: falling through would act under another identity than the one the
// caller set up, and hide what is wrong with it. With no source left, one refusal names each
// source and why it had nothing. No source needs a registered agent session.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/envoy/internal/broker/helper"
	"github.com/sjawhar/envoy/internal/brokerclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// errSourceAbsent is what a source of the chain answers, through sourceAbsent, when it has nothing
// for the caller: the one answer the chain goes on after.
var errSourceAbsent = errors.New("this source has nothing for the caller")

// sourceAbsent is one source's absence and why, in words the chain's refusal prints as they
// stand. It is errSourceAbsent to errors.Is.
type sourceAbsent struct{ reason string }

func (e *sourceAbsent) Error() string        { return e.reason }
func (e *sourceAbsent) Is(target error) bool { return target == errSourceAbsent }

func absent(format string, args ...any) error {
	return &sourceAbsent{reason: fmt.Sprintf(format, args...)}
}

// signerSource is one source of the chain. build answers its signer, registering a session where
// the source is one that registers its own (a sign-in that has none yet, the machine); describe
// answers what `identity --source` prints after the name, found the way build finds it and
// registering nothing; held answers the signer for an identity the source holds now, with no wait
// and no machine trade (heldSigner).
type signerSource struct {
	name     string
	build    func(ctx context.Context) (enrollmentID string, signer Signer, err error)
	describe func(ctx context.Context) (string, error)
	held     func() (proofSigner, error)
}

// signerChain is the chain for a process whose key directory is dir, in R4's order.
func signerChain(dir string) []signerSource {
	return []signerSource{keyDirSource(dir), signInSource(), helperSource(), machineChainSource()}
}

// selectSigner is this process's signer: the first source of the chain that has one, after a
// machine session an older client left in dir is moved to the machine cache.
func selectSigner(dir string) (string, Signer, error) {
	if err := migrateKeyDirMachineSession(dir); err != nil {
		return "", nil, err
	}
	type built struct {
		id     string
		signer Signer
	}
	_, got, err := walkChain(signerChain(dir), func(source signerSource) (built, error) {
		id, signer, err := source.build(context.Background())
		return built{id, signer}, err
	})
	return got.id, got.signer, err
}

// describeSource is the source selectSigner would sign with, and what `identity --source` says
// of it, registering nothing.
func describeSource(dir string) (name, detail string, err error) {
	if err := migrateKeyDirMachineSession(dir); err != nil {
		return "", "", err
	}
	return walkChain(signerChain(dir), func(source signerSource) (string, error) {
		return source.describe(context.Background())
	})
}

// heldSigner is a broker identity this process already holds, for a call that signing only adds
// to: a secret form's reread, which the broker answers in full only for a session, and which goes
// unsigned all the same where this finds none. It walks the chain in its order and takes the first
// source's held signer (signerSource.held): what each source holds now, with no wait on a launcher
// still enrolling or a machine registration in progress, and no machine trade. A source with
// nothing for the caller is passed over; one that is present but not usable now (a launcher still
// enrolling, a broken key or record, a stopped helper) stops the walk with its error, named by its
// source, as the chain stops, so the reread never signs as another identity than the one the
// caller set up. It moves no machine session an older client left in the key directory, since
// that move takes the machine cache's exclusive lock (migrateKeyDirMachineSession): such a key
// directory is signed with as it stands. With no source holding anything it names each source and
// why.
//
// A person's sign-in is the one held signer that may register again: its lease lapses after
// BROKER_LEASE_SECONDS and nothing renews it, so its signer keeps signInSigner's register, and the
// reread's call (doProofBuilt) registers the session again once when the broker says the
// enrollment is not live. That costs one trade of the person's own sign-in, with this machine's
// identity as its actor, and one enrollment; it waits on nothing and registers no machine session.
func heldSigner() (proofSigner, error) {
	name, signer, err := walkChain(signerChain(keyDir()), func(source signerSource) (proofSigner, error) {
		return source.held()
	})
	if err != nil && !errors.Is(err, errNoBrokerIdentity) {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return signer, err
}

// walkChain is the one walk of the chain: it asks each source in its order for what pick reads
// of it, and returns the first source's name with what it found. A source with nothing for the
// caller (errSourceAbsent) is passed over, and its reason kept; any other error stops the walk
// with that source's name and the error, as the chain stops. With no source left it is
// errNoBrokerIdentity, naming each source and why.
func walkChain[T any](chain []signerSource, pick func(signerSource) (T, error)) (string, T, error) {
	var none T
	reasons := make([]string, 0, len(chain))
	for _, source := range chain {
		got, err := pick(source)
		switch {
		case err == nil:
			return source.name, got, nil
		case errors.Is(err, errSourceAbsent):
			reasons = append(reasons, source.name+": "+err.Error())
		default:
			return source.name, none, err
		}
	}
	return "", none, fmt.Errorf("%w: %s", errNoBrokerIdentity, strings.Join(reasons, "; "))
}

// errNoBrokerIdentity is the chain's refusal when no source has a broker identity, from every
// walk of it (walkChain: selectSigner, describeSource and heldSigner alike); the error wrapping
// it names each source and why.
var errNoBrokerIdentity = errors.New("no broker identity for this process")

// keyDirSource is a launcher's key and enrollment in dir. Its session is the launcher's to make
// again, so its signer registers nothing. A fresh enrollment.pending with no key.pem beside it is
// a launcher still enrolling this process, which is present: the chain stops there rather than
// sign as a weaker identity while the launcher is slow (buildSigner has already waited for it).
// dir holds key.pem, so no line names it or a file in it by its path: each names the setting
// (keyDirName), with the system's reason where a file call failed.
func keyDirSource(dir string) signerSource {
	build := func(context.Context) (string, Signer, error) {
		keyPath := filepath.Join(dir, keyFile)
		if !exists(keyPath) {
			if freshMarker(dir) {
				return "", nil, fmt.Errorf("%s holds %s and no key.pem yet: the launcher that wrote it is still enrolling this process", keyDirName(), pendingMarker)
			}
			return "", nil, absent("no key.pem in %s (a launcher puts one there for a box or a pod)", keyDirName())
		}
		key, err := loadKey(keyPath, keyDirFile(keyFile))
		if err != nil {
			return "", nil, err
		}
		id, err := readEnrollment(filepath.Join(dir, enrollmentFile), keyDirFile("the enrollment file"))
		if err != nil {
			if reason, rerr := readTrimmed(filepath.Join(dir, enrollmentErrorFile)); rerr == nil {
				return "", nil, fmt.Errorf("%w (%s: %s)", err, enrollmentErrorFile, reason)
			}
			return "", nil, err
		}
		return id, &fileSigner{key: key, enrollmentID: id}, nil
	}
	return signerSource{name: "key-dir", build: build, describe: func(ctx context.Context) (string, error) {
		if _, _, err := build(ctx); err != nil {
			return "", err
		}
		return keyDirName(), nil
	}, held: func() (proofSigner, error) {
		_, signer, err := build(context.Background())
		return signer, err
	}}
}

// readEnrollment is the enrollment id the enrollment file at path holds, which every refusal names
// as file (the key dir's or the machine cache's) and never by its path: a file it cannot read
// prints the system's reason alone (redact.File), and one that holds no id says so.
func readEnrollment(path, file string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", file, redact.File(err))
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("%s is empty", file)
	}
	return id, nil
}

// signInSource is the sign-in of the person this process acts for (signInSigner).
func signInSource() signerSource {
	noSignIn := func() error {
		return absent("nobody has signed in on this machine with `%s` (%s holds no sign-in)", brokerclient.SignInCommand, brokerclient.SignInStoreName)
	}
	build := func(context.Context) (string, Signer, error) {
		signer, err := signInSigner()
		if err != nil {
			return "", nil, err
		}
		if signer == nil {
			return "", nil, noSignIn()
		}
		return signer.enrollmentID, signer, nil
	}

	return signerSource{
		name:  "sign-in",
		build: build,
		describe: func(context.Context) (string, error) {
			store := brokerclient.SignIns(homeGetenv)
			records, err := store.List()
			if err != nil {
				return "", err
			}
			if len(records) == 0 {
				return "", noSignIn()
			}
			record, err := store.Choose(os.Getenv(brokerclient.PersonVar))
			if err != nil {
				return "", err
			}
			if _, err := record.Key(); err != nil {
				return "", err
			}
			return record.Person, nil
		},
		held: func() (proofSigner, error) {
			_, signer, err := build(context.Background())
			return signer, err
		},
	}
}

// helperSource is agent-secrets-helper, asked once whether the caller is inside a session it
// registered (probeHelper). Its signer is built only once the helper said so: a socket alone
// says nothing about the caller, since every process of the machine inherits it. A socket that is
// absent where AGENT_SECRETS_HELPER_SOCK names it, or where the helper's unit is installed, is a
// stopped helper (a clean stop removes the socket), as bare `identity` reads it: present but
// broken, so a process in one of its sessions stops rather than act as the machine.
func helperSource() signerSource {
	find := func() (string, error) {
		answer := probeHelper()
		switch {
		case !answer.there:
			if _, named := helperSocket(); named {
				return "", helperFailure(answer.sock, errors.New("the socket AGENT_SECRETS_HELPER_SOCK names is absent, so the helper is stopped; start it, or unset AGENT_SECRETS_HELPER_SOCK for a process in none of its sessions"), false)
			}
			if helperInstalled() {
				return "", helperFailure(answer.sock, fmt.Errorf("the socket is absent and the helper is installed (%s), so the helper is stopped; start it: systemctl --user start %s", helper.UnitPath(homeGetenv), helper.UnitName), false)
			}
			return "", absent("the helper socket %s is absent and no helper is installed here (AGENT_SECRETS_HELPER_SOCK; `agent-secrets register` starts a session the helper signs for)", answer.sock)
		case answer.err != nil:
			return "", helperFailure(answer.sock, answer.err, false)
		case answer.resp.OK, answer.resp.Code == helper.CodeNotEnrolled:
			return answer.sock, nil
		case answer.resp.Code == helper.CodeNotASession:
			return "", absent("this process is in no session the helper at %s registered", answer.sock)
		default:
			return "", helperFailure(answer.sock, helperRefusal(answer.resp), true)
		}
	}
	build := func(context.Context) (string, Signer, error) {
		sock, err := find()
		if err != nil {
			return "", nil, err
		}
		return "", &helperSigner{sock: sock}, nil
	}

	return signerSource{
		name:     "helper",
		build:    build,
		describe: func(context.Context) (string, error) { return find() },
		held: func() (proofSigner, error) {
			_, signer, err := build(context.Background())
			return signer, err
		},
	}
}

// machineChainSource is the machine's own identity: the session the machine cache holds, read
// with no network call whether or not AGENT_SECRETS_MACHINE_IDENTITY is set, else the identity
// machineSource finds, traded and registered into the cache (registerMachine).
func machineChainSource() signerSource {
	return signerSource{
		name: "machine",
		build: func(ctx context.Context) (string, Signer, error) {
			signer, err := readMachineCache()
			if err == nil && signer == nil {
				signer, err = registerMachine(ctx, "")
			}
			if err != nil {
				return "", nil, err
			}
			return signer.enrollmentID, signer, nil
		},
		describe: func(ctx context.Context) (string, error) {
			cached, err := readMachineCache()
			if err != nil {
				return "", err
			}
			if cached != nil {
				return "the cached enrollment in " + machineCacheName, nil
			}
			identity, skipped, err := machineSource(ctx, false)
			if err != nil {
				return "", err
			}
			if identity == nil {
				return "", absent("%s", joinSkipped(skipped))
			}
			return identity.Describe(), nil
		},
		held: func() (proofSigner, error) {
			signer, err := readMachineCacheNow()
			if err != nil {
				return nil, err
			}
			return signer, nil
		},
	}
}

// machineSource is the identity this machine proves at the broker's token exchange: the one
// AGENT_SECRETS_MACHINE_IDENTITY names, from the environment or the settings file, whose error
// stops the caller; else the one brokerclient.DetectMachineIdentity finds for the Google exchange
// audience the broker answers in GET /v1/settings. It answers nil, with every place it looked and
// why each had nothing, where this machine has none: a broker that names no exchange audience
// accepts no machine identity, and nothing is asked of the machine then. The use command, `agent-
// secrets login` and a sign-in's renewal all take the machine from here.
//
// AGENT_SECRETS_MACHINE_IDENTITY=kubernetes is a usage error here, before anything is traded: the
// exchange mints a launcher credential for a Google subject and a person alone, so a pod's token
// registers no session and acts as no sign-in's machine. The helper and the Legion controller,
// which read the same setting through brokerclient.MachineIdentity, are unchanged.
//
// Those settings are unauthenticated and choose the audience of the identity token this machine
// mints and hands over, so they are read only from an https broker, or a loopback one
// (requireTrustedBrokerURL), and a Google audience other than the one this machine kept is refused
// before anything is detected or minted (checkGoogleAudience). keep keeps the audience only once
// detection has found an identity to mint a token for it, and before any is minted
// (keepGoogleAudience): a machine with no identity mints nothing and keeps nothing, so a later
// audience or broker still gets the chain's no-source refusal. `identity --source` passes false
// and keeps nothing.
func machineSource(ctx context.Context, keep bool) (brokerclient.Source, []brokerclient.Skipped, error) {
	if kind := strings.TrimSpace(clientGetenv(brokerclient.IdentityVar)); kind != "" {
		if kind == brokerclient.IdentityKubernetes {
			return nil, nil, &settingError{fmt.Errorf("%s=%s is not one agent-secrets takes: the broker mints no launcher credential for a Kubernetes identity, so no session could be registered under it; set %s or %s, or unset it to find this machine's identity from the broker's settings",
				brokerclient.IdentityVar, kind, brokerclient.IdentityMetadata, brokerclient.IdentityPool)}
		}
		identity, err := brokerclient.MachineIdentity(clientGetenv)
		return identity, nil, err
	}
	base := brokerBaseURL()
	if base.Empty() {
		return nil, []brokerclient.Skipped{{Source: "broker settings", Reason: brokerURLVar + " names no broker to read the exchange audience from"}}, nil
	}
	if err := requireTrustedBrokerURL(base); err != nil {
		return nil, nil, &settingError{err}
	}
	settings, err := newClient(base).Settings(ctx)
	var refused *apiError
	if errors.As(err, &refused) && refused.Status == http.StatusNotFound {
		// A broker that serves no settings at all publishes no exchange audience either.
		settings, err = Settings{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read the broker's exchange audience (GET /v1/settings) to find this machine's identity: %w", err)
	}
	if settings.GoogleExchangeAudience == "" {
		return nil, []brokerclient.Skipped{{Source: "broker settings", Reason: fmt.Sprintf(
			"the broker at %s names no exchange audience in GET /v1/settings (a broker from before it published one, or one that accepts no machine identity)",
			base)}}, nil
	}
	if err := checkGoogleAudience(base, settings.GoogleExchangeAudience); err != nil {
		return nil, nil, err
	}
	identity, skipped, err := brokerclient.DetectMachineIdentity(ctx, settings.GoogleExchangeAudience)
	if err != nil || identity == nil || !keep {
		return identity, skipped, err
	}
	if err := keepGoogleAudience(base, settings.GoogleExchangeAudience); err != nil {
		return nil, nil, err
	}
	return identity, skipped, nil
}

// joinSkipped is each skipped place's reason, in the order they were looked at.
func joinSkipped(skipped []brokerclient.Skipped) string {
	reasons := make([]string, len(skipped))
	for i, s := range skipped {
		reasons[i] = s.Reason
	}
	return strings.Join(reasons, "; ")
}
