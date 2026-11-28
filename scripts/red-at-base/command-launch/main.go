// Command command-launch starts the command of one cmd line of scripts/red-at-base.sh, as that
// script's header says: it enters the line's directory, starts the command there with no shell,
// writes to the start record why the start failed (nothing when it started), and exits as the
// command did. It observes Start, not a command's exit status or an interpreter's arguments. It
// changes directory before PATH lookup, and permits relative PATH entries as a direct exec does.
// The script builds it from a copy in its own directory under TMPDIR when its list holds a cmd
// line.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func recordStart(path, failure string) {
	if err := os.WriteFile(path, []byte(failure), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "command-launch: recording process start:", err)
		os.Exit(2)
	}
}

func workingDirectory() string {
	directory, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "command-launch: reading working directory:", err)
		os.Exit(2)
	}
	return directory
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: command-launch <start record> <directory> <command> [<argument>...]")
		os.Exit(2)
	}
	mark, directory, name := os.Args[1], os.Args[2], os.Args[3]
	previousDirectory := workingDirectory()
	if err := os.Chdir(directory); err != nil {
		recordStart(mark, fmt.Sprintf("could not enter %s: %v", directory, err))
		os.Exit(127)
	}
	currentDirectory := workingDirectory()
	command := exec.Command(name, os.Args[4:]...)
	if errors.Is(command.Err, exec.ErrDot) {
		command.Err = nil
	}
	command.Env = append(os.Environ(), "PWD="+currentDirectory, "OLDPWD="+previousDirectory)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := command.Start(); err != nil {
		recordStart(mark, fmt.Sprintf("the exec of %s failed: %v", name, err))
		os.Exit(127)
	}
	recordStart(mark, "")
	if err := command.Wait(); err != nil {
		if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			os.Exit(128 + int(status.Signal()))
		}
		os.Exit(command.ProcessState.ExitCode())
	}
}
