package cli

import (
	"bufio"
	"fmt"
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

func isTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

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
	line, _ := stdin().ReadString('\n')
	line = strings.TrimSpace(line)
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
