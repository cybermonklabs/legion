package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// instructionsCopies is the daemon's copy, under its state directory, of each instructions file
// its configuration names: the deployment's own (`instructions`) and each declared arm's, the
// `memory` block's among them. A launch's prompt ends with these copies (runtime.ComposePrompt),
// never with the operator's files, which a pod cannot read.
//
// Boot writes each copy from the file it read with its other refusals (writeInstructionsCopies),
// and every launch reads each file again first (refresh), so an edit of one reaches the next
// launch of a running daemon with no restart: a Kubernetes daemon mounts its ConfigMap whole, and
// the kubelet updates such a mount in place. A role already running keeps the prompt it launched
// with, and an edit opens no update round, since no pod records the text (the Sandbox runtime's
// drift reads none of it). The set of files is boot's: a file the configuration starts or stops
// naming is a change of `legion.yaml`, which still needs a restart.
type instructionsCopies struct {
	log *slog.Logger
	// mu holds one refresh at a time: launches build their specs at once, and each one compares
	// every file against its copy and may replace it.
	mu    sync.Mutex
	files []instructionsCopy
}

// instructionsCopy is one operator's file and the daemon's copy of it.
type instructionsCopy struct {
	// source is the operator's file, as the configuration names it.
	source string
	// read is source read with boot's refusal of it, and write the copy replaced whole, so a
	// launch composing its prompt beside a refresh reads the old copy or the new one, never half
	// of one (config.WriteDeploymentInstructions, config.WriteVariantInstructions).
	read  func() ([]byte, error)
	write func([]byte) error
	// contents is what the copy holds, and failure the refusal last logged for this file: "" while
	// the file reads and its copy is current.
	contents []byte
	failure  string
}

// readArmInstructions is the instructions file of variant, an arm of experiment, read and
// refused as boot reads it: the `memory` block's own arm (config.Variant.MemoryBlock) in the
// block's own words (config.ReadMemoryInstructions), which the documentation quotes, and every
// arm of the `experiments` key, `experiments.memory` included, in the experiments' words
// (config.ReadVariantInstructions).
func readArmInstructions(experiment string, variant config.Variant) ([]byte, error) {
	if variant.MemoryBlock {
		return config.ReadMemoryInstructions(variant.InstructionsPath)
	}
	return config.ReadVariantInstructions(experiment, variant.Name, variant.InstructionsPath)
}

// writeInstructionsCopies writes the copy of each instructions file boot read (readBoot) before
// the first pane opens, and returns the copies a launch refreshes with the path of the
// deployment's own, "" when the configuration names none. The deployment's copy is
// config.DeploymentInstructionsCopy; each arm's is under its
// experiment's own directory, since two names may hold `-` and neither may hold `/`, and the
// `memory` block's arm at the block's own `memory-instructions.md`
// (config.VariantInstructionsFile, where a launch's arms read the copies back from:
// experiment.Launch).
func writeInstructionsCopies(cfg config.Config, reads bootReads, log *slog.Logger) (*instructionsCopies, string, error) {
	copies := &instructionsCopies{log: log}
	keep := func(file instructionsCopy) error {
		if err := file.write(file.contents); err != nil {
			return err
		}
		copies.files = append(copies.files, file)
		return nil
	}
	deployment := ""
	if reads.instructions != nil {
		source := cfg.InstructionsPath
		if err := keep(instructionsCopy{
			source:   source,
			contents: reads.instructions,
			read:     func() ([]byte, error) { return config.ReadDeploymentInstructions(source) },
			write: func(contents []byte) error {
				return config.WriteDeploymentInstructions(contents, cfg.StateDir, cfg.Project)
			},
		}); err != nil {
			return nil, "", err
		}
		deployment = config.DeploymentInstructionsCopy(cfg.StateDir)
	}
	for _, arm := range reads.variantInstructions {
		experiment, variant := arm.experiment, arm.variant
		if err := keep(instructionsCopy{
			source:   variant.InstructionsPath,
			contents: arm.contents,
			read:     func() ([]byte, error) { return readArmInstructions(experiment, variant) },
			write: func(contents []byte) error {
				return config.WriteVariantInstructions(contents, cfg.StateDir, cfg.Project, experiment, variant)
			},
		}); err != nil {
			return nil, "", err
		}
	}
	return copies, deployment, nil
}

// refresh reads each operator's file again and replaces the copy of each one that changed, so the
// launch about to be built carries what the files hold now; SpawnSpec calls it first. Each edit it
// reads is logged once, at INFO, naming the file. A file that no longer reads, which boot would
// refuse (missing, unreadable or blank), keeps the copy last read: a mistake in an edit stops no
// launch, and the daemon says so once for each refusal, at WARN, naming the file and the refusal;
// a copy it cannot replace is the same, at ERROR. A file that reads again after a refusal is
// logged once at INFO, whether or not its text changed. A nil c, a test's specs literal,
// refreshes nothing.
func (c *instructionsCopies) refresh(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.files {
		file := &c.files[i]
		contents, err := file.read()
		switch {
		case err != nil:
			file.refused(ctx, c.log, slog.LevelWarn, "the file no longer reads", err)
		case bytes.Equal(contents, file.contents):
			if file.failure != "" {
				file.failure = ""
				c.log.InfoContext(ctx, "legion daemon reads an instructions file again, and it holds the text each launch carried meanwhile",
					"file", file.source, "bytes", len(contents))
			}
		default:
			if err := file.write(contents); err != nil {
				file.refused(ctx, c.log, slog.LevelError, "its copy could not be replaced", err)
				continue
			}
			file.contents, file.failure = contents, ""
			c.log.InfoContext(ctx, "legion daemon read an edited instructions file: each launch from now carries it, and each running role keeps the text it launched with",
				"file", file.source, "bytes", len(contents))
		}
	}
}

// refused logs, once for each refusal, that launches keep the copy last read because of err. A
// refusal is why with err's innermost cause, not err's whole text: a failed write of the copy names
// a temporary file whose name is new at each try (os.CreateTemp), and a refusal that stands is
// logged once.
func (f *instructionsCopy) refused(ctx context.Context, log *slog.Logger, level slog.Level, why string, err error) {
	cause := err
	for inner := errors.Unwrap(cause); inner != nil; inner = errors.Unwrap(cause) {
		cause = inner
	}
	if refusal := why + ": " + cause.Error(); refusal != f.failure {
		f.failure = refusal
		log.Log(ctx, level, "legion daemon launches with the instructions it last read: "+why, "file", f.source, "error", err)
	}
}
