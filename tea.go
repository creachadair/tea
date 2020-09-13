// Program tea copies its input to its output.
// Without further instructions, it behaves as cat(1).  The command-line may
// also specify patterns to match in the input, and external programs to invoke
// when those patterns are found.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
)

var (
	bufLimit = flag.Int("bufsize", 1<<16, "Match buffer size limit (bytes)")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %s [options] [regexp command args...]

Copy standard input to standard output. If a regexp and command are
given, each match of the regexp in the input triggers execution of the
given command and arguments.

Regular expression syntax: https://pkg.go.dev/regexp/syntax

Options:
`, filepath.Base(os.Args[0]))

		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()

	var out io.Writer = os.Stdout
	if flag.NArg() > 0 {
		t, err := parseTrigger(flag.Args())
		if err != nil {
			log.Fatalf("Parsing trigger: %v", err)
		}
		out = io.MultiWriter(os.Stdout, t)
		defer t.Close()
	}

	_, err := io.Copy(out, bufio.NewReader(os.Stdin))
	if err != nil {
		log.Printf("Copy failed: %v", err)
	}
}

func parseTrigger(args []string) (*trigger, error) {
	if len(args) < 2 {
		return nil, errors.New("missing regexp or command")
	}
	re, err := regexp.Compile(args[0])
	if err != nil {
		return nil, fmt.Errorf("parsing pattern: %v", err)
	}

	return &trigger{
		re:   re,
		cmd:  args[1],
		args: args[2:],
		buf:  bytes.NewBuffer(nil),
	}, nil
}

type trigger struct {
	ctx   context.Context
	re    *regexp.Regexp
	cmd   string
	args  []string
	multi bool

	mu  sync.Mutex
	buf *bytes.Buffer
}

func (t *trigger) fire() {
	t.mu.Lock()

	// Check for a match of the regexp.
	m := t.re.FindSubmatchIndex(t.buf.Bytes())
	if m == nil {
		// Discard data in excess of the buffer size limit.
		if t.buf.Len() > *bufLimit {
			t.buf.Next(t.buf.Len() - *bufLimit)
		}
		t.mu.Unlock()
		return // no match
	}
	text := string(t.buf.Next(m[1]))
	t.mu.Unlock()

	// Substitute any submatches into the command line.
	var args []string
	for _, arg := range t.args {
		repl := t.re.ExpandString(nil, arg, text, m)
		args = append(args, string(repl))
	}
	cmd := exec.Command(t.cmd, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Printf("Error: executing %q: %v", t.cmd, err)
	}
}

func (t *trigger) Write(data []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	nw, err := t.buf.Write(data)
	if nw != 0 {
		go t.fire()
	}
	return nw, err
}

func (t *trigger) Close() error { t.fire(); return nil }
