package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/term"
)

var (
	stdinOnce   sync.Once
	stdinReader *bufio.Reader
)

func stdin() *bufio.Reader {
	stdinOnce.Do(func() { stdinReader = bufio.NewReader(os.Stdin) })
	return stdinReader
}

// isTTY reports whether prompts can be shown; tests substitute it.
var isTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// promptCtx is cancelled by Ctrl-C (Execute's signal context). Execute catches
// SIGINT for every command, which turns off Go's exit-on-Ctrl-C, so a prompt
// blocked on stdin would otherwise ignore it (#23).
var promptCtx = context.Background()

// interrupted unwinds a command from inside a prompt: Ctrl-C, or the end of
// input (Ctrl-D). Execute recovers it and exits 130. A panic rather than an
// error because ask is called from some forty places that cannot fail; the
// unwinding still runs every deferred cleanup on the way out.
type interrupted struct{}

// Seams for tests: where the interrupt newline goes, and the terminal state
// calls readSecret uses to restore echo (a test has no terminal).
var (
	promptErr    io.Writer = os.Stderr
	termGetState           = term.GetState
	termRestore            = term.Restore
)

// errInterrupted is what an interrupted command returns.
var errInterrupted = errors.New("interrupted")

// readLine reads one line from stdin, unwinding with interrupted on Ctrl-C or
// at the end of input. The reader goroutine left blocked on Ctrl-C does not
// matter: the command is ending.
func readLine() string {
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	in := stdin() // taken now: the goroutine may outlive this prompt
	go func() {
		s, err := in.ReadString('\n')
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil && r.s == "" {
			// End of input: end the prompt's line before the error message.
			fmt.Fprintln(promptErr)
			panic(interrupted{})
		}
		return r.s
	case <-promptCtx.Done():
		fmt.Fprintln(promptErr)
		panic(interrupted{})
	}
}

// readSecret is readLine without echo, via read (term.ReadPassword on the
// real terminal). Ctrl-D does not end it: x/term's password read treats an
// empty read as nothing typed and waits on; Ctrl-C does. On Ctrl-C the terminal's echo is restored before unwinding:
// the abandoned read would otherwise leave it off.
func readSecret(read func() ([]byte, error)) ([]byte, error) {
	fd := int(os.Stdin.Fd())
	state, stateErr := termGetState(fd)
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b, err := read()
		ch <- res{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-promptCtx.Done():
		if stateErr == nil {
			_ = termRestore(fd, state)
		}
		fmt.Fprintln(promptErr)
		panic(interrupted{})
	}
}

// recoverInterrupted turns an interrupted unwind into errInterrupted; any
// other panic continues. Use as: defer recoverInterrupted(&err).
func recoverInterrupted(err *error) {
	if r := recover(); r != nil {
		if _, ok := r.(interrupted); !ok {
			panic(r)
		}
		*err = exitError{code: 130, err: errInterrupted}
	}
}

// ask prompts for a string. Without a terminal it returns the default, so a
// non-interactive run never blocks.
func ask(label, def string) string {
	if !isTTY() {
		return def
	}
	if def != "" {
		fmt.Fprintf(os.Stderr, "%s [%s]: ", label, def)
	} else {
		fmt.Fprintf(os.Stderr, "%s: ", label)
	}
	line := strings.TrimSpace(readLine())
	if line == "" {
		return def
	}
	return line
}

func askInt(label string, def int) int {
	for {
		v := ask(label, strconv.Itoa(def))
		n, err := strconv.Atoi(v)
		if err == nil && n > 0 {
			return n
		}
		fmt.Fprintln(os.Stderr, "  enter a positive number")
		if !isTTY() {
			return def
		}
	}
}

// choose offers numbered options and returns the chosen value.
func choose(label string, options []string, def string) string {
	if !isTTY() || len(options) == 0 {
		return def
	}
	fmt.Fprintln(os.Stderr, label+":")
	for i, o := range options {
		mark := " "
		if o == def {
			mark = "*"
		}
		fmt.Fprintf(os.Stderr, "  %s %d) %s\n", mark, i+1, o)
	}
	for {
		v := ask("  choice", def)
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(options) {
			return options[n-1]
		}
		for _, o := range options {
			if o == v {
				return o
			}
		}
		fmt.Fprintln(os.Stderr, "  pick a number from the list")
	}
}

func askYesNo(label string, def bool) bool {
	d := "n"
	if def {
		d = "y"
	}
	v := strings.ToLower(ask(label+" (y/n)", d))
	return v == "y" || v == "yes"
}
