package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// withPrompt makes prompts interactive, reading from r, with ctx as the
// Ctrl-C context, for the test's duration.
func withPrompt(t *testing.T, r io.Reader, ctx context.Context) {
	t.Helper()
	stdin() // initialise the real reader first, so cleanup restores a usable one
	oldR, oldTTY, oldCtx, oldErr := stdinReader, isTTY, promptCtx, promptErr
	stdinReader, isTTY, promptCtx, promptErr = bufio.NewReader(r), func() bool { return true }, ctx, io.Discard
	t.Cleanup(func() { stdinReader, isTTY, promptCtx, promptErr = oldR, oldTTY, oldCtx, oldErr })
}

// runAsking runs a command that asks one question, as Execute runs commands.
func runAsking(t *testing.T) (answer string, err error) {
	t.Helper()
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error {
		answer = ask("question", "default")
		return nil
	}}
	cmd.SetArgs(nil)
	done := make(chan struct{})
	go func() { err = executeRoot(context.Background(), cmd); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt did not return")
	}
	return answer, err
}

// within runs fn and fails the test if it has not returned after 5s: a prompt
// that ignores Ctrl-C must fail the test quickly, not hang the package.
func within(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt did not return on Ctrl-C")
	}
}

func wantInterrupted(t *testing.T, err error) {
	t.Helper()
	var ee exitError
	if !errors.As(err, &ee) || ee.code != 130 || !errors.Is(err, errInterrupted) {
		t.Fatalf("got %v, want exit 130 interrupted", err)
	}
}

// #23: Ctrl-C at a prompt ends the command (exit 130) instead of waiting for
// input that never comes.
func TestCtrlCEndsAPrompt(t *testing.T) {
	pr, pw := io.Pipe() // nothing is ever typed
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	withPrompt(t, pr, ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := runAsking(t)
	wantInterrupted(t, err)
}

// Ctrl-D (the end of input) at a prompt ends the command too, rather than
// silently taking the default.
func TestEndOfInputEndsAPrompt(t *testing.T) {
	pr, pw := io.Pipe()
	pw.Close()
	withPrompt(t, pr, context.Background())
	answer, err := runAsking(t)
	if answer == "default" {
		t.Fatal("end of input took the default")
	}
	wantInterrupted(t, err)
}

// A last line without a newline is still an answer; a typed line is the answer.
func TestPromptAnswers(t *testing.T) {
	for in, want := range map[string]string{"abc": "abc", "xyz\n": "xyz", "\n": "default"} {
		pr, pw := io.Pipe()
		go func() { _, _ = io.WriteString(pw, in); pw.Close() }()
		withPrompt(t, pr, context.Background())
		answer, err := runAsking(t)
		if err != nil || answer != want {
			t.Errorf("%q: answer %q err %v, want %q", in, answer, err, want)
		}
	}
}

// Only an interrupted prompt is turned into exit 130; any other panic is not
// swallowed.
func TestRecoverInterruptedPassesOtherPanicsOn(t *testing.T) {
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("recovered %v, want the original panic", r)
		}
	}()
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { panic("boom") }}
	cmd.SetArgs(nil)
	_ = executeRoot(context.Background(), cmd)
}

// The password prompt (forge) ends on Ctrl-C as well; a completed read returns.
func TestCtrlCEndsASecretPrompt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	withPrompt(t, nil, ctx)
	block := make(chan struct{})
	defer close(block)
	time.AfterFunc(50*time.Millisecond, cancel)
	var err error
	within(t, func() {
		defer recoverInterrupted(&err)
		_, _ = readSecret(func() ([]byte, error) { <-block; return nil, nil })
	})
	wantInterrupted(t, err)

	withPrompt(t, nil, context.Background())
	b, rerr := readSecret(func() ([]byte, error) { return []byte("pw"), nil })
	if string(b) != "pw" || rerr != nil {
		t.Errorf("read %q %v", b, rerr)
	}
}

// Execute's real wiring: SIGINT cancels the context prompts wait on.
func TestSIGINTCancelsThePromptContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGINT to self on Windows")
	}
	old := promptCtx
	defer func() { promptCtx = old }()
	_, stop := signalContext()
	defer stop()
	p, _ := os.FindProcess(os.Getpid())
	if err := p.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-promptCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGINT did not cancel the prompt context")
	}
}

// The prompts that do not go through ask end on Ctrl-C too: confirm, and the
// forge password.
func TestCtrlCEndsConfirmAndTheForgePassword(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	withPrompt(t, pr, ctx)
	var err error
	within(t, func() {
		defer recoverInterrupted(&err)
		confirm(&cobra.Command{}, "delete?")
	})
	wantInterrupted(t, err)

	t.Setenv(envForgePassword, "")
	oldTerm, oldRead := forgeTerminal, forgeReadPassword
	block := make(chan struct{})
	defer func() { forgeTerminal, forgeReadPassword = oldTerm, oldRead; close(block) }()
	forgeTerminal = func() bool { return true }
	forgeReadPassword = func() ([]byte, error) { <-block; return nil, nil }
	err = nil
	within(t, func() {
		defer recoverInterrupted(&err)
		_, _ = forgePassword(io.Discard)
	})
	wantInterrupted(t, err)
}

// An interrupted prompt ends its line before "roksbnkargoctl: interrupted",
// whether by Ctrl-C or by the end of input.
func TestInterruptEndsThePromptLine(t *testing.T) {
	for name, setup := range map[string]func() (io.Reader, context.Context){
		"ctrl-c": func() (io.Reader, context.Context) {
			pr, _ := io.Pipe()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return pr, ctx
		},
		"eof": func() (io.Reader, context.Context) {
			pr, pw := io.Pipe()
			pw.Close()
			return pr, context.Background()
		},
	} {
		r, ctx := setup()
		withPrompt(t, r, ctx)
		var out bytes.Buffer
		promptErr = &out
		var err error
		within(t, func() {
			defer recoverInterrupted(&err)
			readLine()
		})
		wantInterrupted(t, err)
		if out.String() != "\n" {
			t.Errorf("%s: wrote %q before the error, want a newline", name, out.String())
		}
	}
}

// Ctrl-C at the password prompt restores the terminal state captured before
// the read: the abandoned read would otherwise leave echo off.
func TestInterruptedSecretRestoresTheTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	withPrompt(t, nil, ctx)
	captured := &term.State{}
	var restored *term.State
	oldGet, oldRestore := termGetState, termRestore
	defer func() { termGetState, termRestore = oldGet, oldRestore }()
	termGetState = func(int) (*term.State, error) { return captured, nil }
	termRestore = func(_ int, st *term.State) error { restored = st; return nil }
	block := make(chan struct{})
	defer close(block)
	time.AfterFunc(20*time.Millisecond, cancel)
	var err error
	within(t, func() {
		defer recoverInterrupted(&err)
		_, _ = readSecret(func() ([]byte, error) { <-block; return nil, nil })
	})
	wantInterrupted(t, err)
	if restored != captured {
		t.Error("the terminal state was not restored after an interrupted password read")
	}
}
