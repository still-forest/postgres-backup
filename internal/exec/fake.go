package exec

import (
	"context"
	"fmt"
	"io"
	"sync"
)

// Stub is a canned response for one matched invocation.
type Stub struct {
	// Match is compared against Cmd.Path; empty matches any path.
	Match string
	// Stdout is written to the command's stdout before returning.
	Stdout string
	// Stderr is written to the command's stderr before returning.
	Stderr string
	// Err is returned from Run.
	Err error
	// Do runs after the canned output is written, for tests that need to
	// materialise a file that the command under test would have produced.
	Do func(cmd Cmd) error
}

// Fake is a Commander that records invocations and replays stubs.
// Stubs are consumed in order; any invocation past the end of the list
// succeeds silently.
type Fake struct {
	mu    sync.Mutex
	Stubs []Stub
	Calls []Cmd
}

// Run records cmd and applies the next matching stub.
func (f *Fake) Run(_ context.Context, cmd Cmd) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Calls = append(f.Calls, cmd)

	for i, s := range f.Stubs {
		if s.Match != "" && s.Match != cmd.Path {
			continue
		}
		f.Stubs = append(f.Stubs[:i:i], f.Stubs[i+1:]...)
		writeTo(cmd.Stdout, s.Stdout)
		writeTo(cmd.Stderr, s.Stderr)
		if s.Do != nil {
			if err := s.Do(cmd); err != nil {
				return err
			}
		}
		return s.Err
	}
	return nil
}

// CallsTo returns every recorded invocation of the named binary.
func (f *Fake) CallsTo(path string) []Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []Cmd
	for _, c := range f.Calls {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

// ArgsString renders a recorded invocation for assertion failure messages.
func ArgsString(c Cmd) string { return fmt.Sprintf("%s %v", c.Path, c.Args) }

func writeTo(w io.Writer, s string) {
	if w == nil || s == "" {
		return
	}
	_, _ = io.WriteString(w, s)
}
