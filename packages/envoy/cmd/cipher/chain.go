// packages/envoy/cmd/cipher/chain.go
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
// source, and so is a person's sign-in this machine cannot renew that holds no live enrollment
// (signInPassedOver), which the walk also names on stderr when it takes a later source. A source
// that is present but broken, and any other refusal of the broker's, stops the command with that
// error: falling through would act under another identity than the one the caller set up, and
// hide what is wrong with it. With no source left, one refusal names each source and why it had
// nothing. No source needs a registered agent session.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/helper"
	"github.com/sjawhar/envoy/internal/cipherclient"
	"github.com/sjawhar/envoy/internal/redact"
)

// errSourceAbsent is what a source of the chain answers, through sourceAbsent, when it has nothing
// for the caller: the one answer the chain goes on after.
var errSourceAbsent = errors.New("this source has nothing for the caller")

// sourceAbsent is one source's absence and why, in words the chain's refusal prints as they
// stand. It is errSourceAbsent to errors.Is. told marks an absence the walk names on stderr when
// it takes a later source (passedOver): a source that was set up and cannot serve, where the
// caller would otherwise not know which identity the command acted as.
type sourceAbsent struct {
	reason string
	told   bool
}

func (e *sourceAbsent) Error() string        { return e.reason }
func (e *sourceAbsent) Is(target error) bool { return target == errSourceAbsent }

func absent(format string, args ...any) error {
	return &sourceAbsent{reason: fmt.Sprintf(format, args...)}
}

// passedOver is absent for a source that was set up and cannot serve: walkChain names it on
// stderr when it takes a later source.
func passedOver(format string, args ...any) error {
	return &sourceAbsent{reason: fmt.Sprintf(format, args...), told: true}
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

// signerChain is the chain for a process whose key directory is dir, in R4's order. passOver is
// whether its sign-in source passes over a sign-in this machine cannot renew (signInPassedOver):
// every walk but renew's, whose contract is to report the enrollment the broker no longer holds
// (exit 76) and never to act as another session in its place.
func signerChain(dir string, passOver bool) []signerSource {
	return []signerSource{keyDirSource(dir), signInSource(passOver), helperSource(), machineChainSource()}
}

// selectSigner is this process's signer: the first source of the chain that has one, after a
// machine session an older client left in dir is moved to the machine cache. A source passed
// over on the way is named on stderr (walkChain).
func selectSigner(dir string, stderr io.Writer, passOver bool) (string, Signer, error) {
	if err := migrateKeyDirMachineSession(dir); err != nil {
		return "", nil, err
	}
	type built struct {
		id     string
		signer Signer
	}
	_, got, err := walkChain(signerChain(dir, passOver), stderr, func(source signerSource) (built, error) {
		id, signer, err := source.build(context.Background())
		return built{id, signer}, err
	})
	return got.id, got.signer, err
}

// describeSource is the source selectSigner would sign with, and what `identity --source` says
// of it, registering nothing. A source passed over on the way is named on stderr, as
// selectSigner names it.
func describeSource(dir string, stderr io.Writer) (name, detail string, err error) {
	if err := migrateKeyDirMachineSession(dir); err != nil {
		return "", "", err
	}
	return walkChain(signerChain(dir, true), stderr, func(source signerSource) (string, error) {
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
// A sign-in this machine cannot renew is passed over as the chain passes it over
// (signInPassedOver), and says nothing on stderr: the reread signs only to read more.
func heldSigner() (proofSigner, error) {
	name, signer, err := walkChain(signerChain(keyDir(), true), nil, func(source signerSource) (proofSigner, error) {
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
// errNoBrokerIdentity, naming each source and why. A source passed over that told why
// (passedOver) is named on stderr, unless stderr is nil, in one line per such source before the
// walk ends: "agent-secrets: <why>; using the <source> source" where a later source serves, and
// "agent-secrets: <why>; tried the <source> source next" where a later source stops the walk, so
// the caller learns why the command did not act as that source even when the one it tried fails.
func walkChain[T any](chain []signerSource, stderr io.Writer, pick func(signerSource) (T, error)) (string, T, error) {
	var none T
	reasons := make([]string, 0, len(chain))
	var told []string
	tell := func(after string) {
		if stderr == nil {
			return
		}
		for _, reason := range told {
			fmt.Fprintf(stderr, "agent-secrets: %s; %s\n", reason, after)
		}
	}
	for _, source := range chain {
		got, err := pick(source)
		var why *sourceAbsent
		switch {
		case err == nil:
			tell("using the " + source.name + " source")
			return source.name, got, nil
		case errors.As(err, &why) && why.told:
			told = append(told, why.reason)
			reasons = append(reasons, source.name+": "+err.Error())
		case errors.Is(err, errSourceAbsent):
			reasons = append(reasons, source.name+": "+err.Error())
		default:
			tell("tried the " + source.name + " source next")
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

// signInSource is the sign-in of the person this process acts for (signInSigner). A box with
// sign-ins but no way to say which one this session is refuses here, naming them: a command must
// never read one colleague's secrets under another's name. A sign-in this machine cannot renew
// that holds no live enrollment has nothing for the caller (signInPassedOver).
func signInSource(passOver bool) signerSource {
	find := func(ctx context.Context) (cipherclient.SignInRecord, *ecdsa.PrivateKey, error) {
		store := cipherclient.SignIns(homeGetenv)
		records, err := store.List()
		if err != nil {
			return cipherclient.SignInRecord{}, nil, err
		}
		if len(records) == 0 {
			return cipherclient.SignInRecord{}, nil, absent("nobody has signed in on this machine with `%s` (%s holds no sign-in)", cipherclient.SignInCommand, cipherclient.SignInStoreName)
		}
		record, err := store.Choose(os.Getenv(cipherclient.PersonVar))
		if err != nil {
			return cipherclient.SignInRecord{}, nil, err
		}
		key, err := record.Key()
		if err != nil {
			return cipherclient.SignInRecord{}, nil, err
		}
		if !passOver {
			return record, key, nil
		}
		return record, key, signInPassedOver(ctx, record, key)
	}
	build := func(ctx context.Context) (string, Signer, error) {
		record, key, err := find(ctx)
		if err != nil {
			return "", nil, err
		}
		signer, err := signInSigner(record, key)
		if err != nil {
			return "", nil, err
		}
		return signer.enrollmentID, signer, nil
	}

	return signerSource{
		name:  "sign-in",
		build: build,
		describe: func(ctx context.Context) (string, error) {
			record, _, err := find(ctx)
			return record.Person, err
		},
		held: func() (proofSigner, error) {
			_, signer, err := build(context.Background())
			return signer, err
		},
	}
}

// signInPassedOver is why the chain passes over record's sign-in, whose box key is key, or nil
// where the sign-in serves. It passes over a sign-in this machine cannot renew (signInRenewal)
// that holds no live enrollment: its record names none, or the broker answers that the one it
// names is not live. Only for such a sign-in does it ask the broker, once: GET
// /v1/enrollments/self signed with the sign-in's key, which registers nothing. Any other answer
// but not live, a refusal or an unreachable broker, stops the chain, named as this check. A
// sign-in client this machine cannot read is no reason to pass a sign-in over: the sign-in serves
// as before, and its renewal reports that error when it runs.
func signInPassedOver(ctx context.Context, record cipherclient.SignInRecord, key *ecdsa.PrivateKey) error {
	_, renewal := signInRenewal(record)
	var unrenewable *unrenewableSignIn
	if !errors.As(renewal, &unrenewable) {
		return nil
	}
	if record.EnrollmentID != "" {
		base := brokerBaseURL()
		if base.Empty() {
			return nil
		}
		_, _, err := newClient(base).proofCall(ctx, &fileSigner{key: key, enrollmentID: record.EnrollmentID}, http.MethodGet, "/v1/enrollments/self", nil)
		if err != nil && !enrollmentNotLive(err) {
			return fmt.Errorf("ask the broker %s names whether %s's sign-in holds a live enrollment, since this machine cannot renew it (GET /v1/enrollments/self): %w",
				brokerURLVar, record.Person, err)
		}
		if err == nil {
			return nil
		}
	}
	return passedOver("%s's sign-in holds no live enrollment, and this machine cannot renew it: %s", record.Person, unrenewable.why)
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

// noLauncherCredential names each identity kind machineSource refuses, as its refusal names it:
// the kinds whose trade the broker mints no launcher credential for.
var noLauncherCredential = map[string]string{
	cipherclient.IdentityKubernetes:    "a Kubernetes identity",
	cipherclient.IdentityGitHubActions: "a GitHub Actions run",
}

// machineSource is the identity this machine proves at the broker's token exchange: the one
// AGENT_SECRETS_MACHINE_IDENTITY names, from the environment or the settings file, whose error
// stops the caller; else the one cipherclient.DetectMachineIdentity finds for the Google exchange
// audience the broker answers in GET /v1/settings. It answers nil, with every place it looked and
// why each had nothing, where this machine has none: a broker that names no exchange audience
// accepts no machine identity, and nothing is asked of the machine then. The use command, `agent-
// secrets login` and a sign-in's renewal all take the machine from here.
//
// AGENT_SECRETS_MACHINE_IDENTITY=kubernetes or github-actions is a usage error here, before
// anything is traded: the exchange mints a launcher credential for a Google subject and a person
// alone, so a pod's token or a workflow run's registers no session and acts as no sign-in's
// machine. The Legion controller and natsprobe, which read the same setting through
// cipherclient.MachineIdentity and decline the launcher credential, take both; the helper reads it
// too, and gets no launcher credential with either.
//
// Those settings are unauthenticated and choose the audience of the identity token this machine
// mints and hands over, so they are read only from an https broker, or a loopback one
// (requireTrustedBrokerURL), and a Google audience other than the one this machine kept is refused
// before anything is detected or minted (checkGoogleAudience). keep keeps the audience only once
// detection has found an identity to mint a token for it, and before any is minted
// (keepGoogleAudience): a machine with no identity mints nothing and keeps nothing, so a later
// audience or broker still gets the chain's no-source refusal. `identity --source` passes false
// and keeps nothing.
func machineSource(ctx context.Context, keep bool) (cipherclient.Source, []cipherclient.Skipped, error) {
	if kind := strings.TrimSpace(clientGetenv(cipherclient.IdentityVar)); kind != "" {
		if what, refused := noLauncherCredential[kind]; refused {
			return nil, nil, &settingError{fmt.Errorf("%s=%s is not one agent-secrets takes: the broker mints no launcher credential for %s, so no session could be registered under it; set %s or %s, or unset it to find this machine's identity from the broker's settings",
				cipherclient.IdentityVar, kind, what, cipherclient.IdentityMetadata, cipherclient.IdentityPool)}
		}
		identity, err := cipherclient.MachineIdentity(clientGetenv)
		return identity, nil, err
	}
	base := brokerBaseURL()
	if base.Empty() {
		return nil, []cipherclient.Skipped{{Source: "broker settings", Reason: brokerURLVar + " names no broker to read the exchange audience from"}}, nil
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
		return nil, []cipherclient.Skipped{{Source: "broker settings", Reason: fmt.Sprintf(
			"the broker at %s names no exchange audience in GET /v1/settings (a broker from before it published one, or one that accepts no machine identity)",
			base)}}, nil
	}
	if err := checkGoogleAudience(base, settings.GoogleExchangeAudience); err != nil {
		return nil, nil, err
	}
	identity, skipped, err := cipherclient.DetectMachineIdentity(ctx, settings.GoogleExchangeAudience)
	if err != nil || identity == nil || !keep {
		return identity, skipped, err
	}
	if err := keepGoogleAudience(base, settings.GoogleExchangeAudience); err != nil {
		return nil, nil, err
	}
	return identity, skipped, nil
}

// joinSkipped is each skipped place's reason, in the order they were looked at.
func joinSkipped(skipped []cipherclient.Skipped) string {
	reasons := make([]string, len(skipped))
	for i, s := range skipped {
		reasons[i] = s.Reason
	}
	return strings.Join(reasons, "; ")
}
