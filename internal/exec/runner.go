// Package exec wraps os/exec behind an interface so that callers which shell out
// to pg_dump, pg_restore and psql can be unit-tested without those binaries.
package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
)

// Cmd describes a single external command invocation.
//
// Env holds extra environment entries layered on top of the parent process
// environment. Secrets belong here and never in Args: Args gets echoed into logs
// and shows up in the process table, Env does not.
type Cmd struct {
	Path   string
	Args   []string
	Env    []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Commander runs external commands.
type Commander interface {
	Run(ctx context.Context, cmd Cmd) error
}

// OS is a Commander backed by os/exec.
type OS struct{}

// Run executes cmd, inheriting the parent environment plus cmd.Env.
func (OS) Run(ctx context.Context, cmd Cmd) error {
	c := osexec.CommandContext(ctx, cmd.Path, cmd.Args...)
	c.Env = append(os.Environ(), cmd.Env...)
	c.Stdin = cmd.Stdin
	c.Stdout = cmd.Stdout
	c.Stderr = cmd.Stderr

	if err := c.Run(); err != nil {
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("%s exited %d: %w", cmd.Path, exitErr.ExitCode(), err)
		}
		return fmt.Errorf("%s: %w", cmd.Path, err)
	}
	return nil
}

// LookPath reports whether a binary is resolvable on PATH.
func LookPath(name string) (string, error) { return osexec.LookPath(name) }
