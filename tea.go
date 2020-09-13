// Program tea copies its input to its output.
// Without further instructions, it behaves as cat(1).  The command-line may
// also specify patterns to match in the input, and external programs to invoke
// when those patterns are found.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"sync"
)

var (
	bufLimit = flag.Int("buf", 1<<16, "Match buffer size limit (bytes)")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %s [options] [regexp command args...]

Copy standard input to standard output. If a trigger consisting of a regexp
and command are given, each match of the regexp in the input triggers an
execution of the given command and arguments. Multiple triggers may be set,
separated by ";;". Note that the separator may need quotation to protect it
from the shell.

By default, matches are applied line-by-line, as in grep.
If a pattern sets the multi-line flag (?m), matches for that trigger may
span multiple lines, over a buffer of up to -buf bytes.

Pattern syntax is as defined by: https://pkg.go.dev/regexp/syntax

Submatches are interpolated into command arguments:

  $0   -- the entire match
  $1   -- the text of the first parenthesized submatch
  etc.

If the regular expression uses named capture groups like $(?P<name>...),
the argument may also use the syntax ${name}.

Options:
`, filepath.Base(os.Args[0]))

		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()

	out := []io.Writer{os.Stdout}
	for i, rule := range splitArgs(flag.Args()) {
		t, err := parseTrigger(rule)
		if err != nil {
			log.Fatalf("Parsing trigger %d: %v", i+1, err)
		}
		out = append(out, t)
		defer t.Close()
	}
	_, err := io.Copy(io.MultiWriter(out...), bufio.NewReader(os.Stdin))
	if err != nil {
		log.Printf("Copy failed: %v", err)
	}
}

// splitArgs partitions args into candidate trigger groups, separated by ";;"
// arguments. It returns an empty slice if there are no trigger groups.
func splitArgs(args []string) [][]string {
	var cmds [][]string
	var cur []string
	for _, arg := range args {
		if arg == ";;" {
			cmds = append(cmds, cur)
			cur = nil
		} else {
			cur = append(cur, arg)
		}
	}
	if len(cur) != 0 {
		cmds = append(cmds, cur)
	}
	return cmds
}

// hasMulti reports whether rt contains any subexpressions that allow
// multi-line matches. Since the regexp parser lowers top-level flags it is not
// sufficient to check only the root.
func hasMulti(rt *syntax.Regexp) bool {
	if rt.Flags != 0 && rt.Flags&syntax.OneLine == 0 {
		return true
	}
	for _, sub := range rt.Sub {
		if hasMulti(sub) {
			return true
		}
	}
	return false
}

// parseTrigger parses args as a trigger group consisting of a regexp pattern,
// a command, and optional arguments.
func parseTrigger(args []string) (*trigger, error) {
	switch len(args) {
	case 0:
		return nil, errors.New("missing regexp and command")
	case 1:
		return nil, errors.New("missing command")
	}

	// Parse the pattern and check its flags for multi-line support.
	rt, err := syntax.Parse(args[0], syntax.Perl) // as regexp.Compile
	if err != nil {
		return nil, fmt.Errorf("pattern: %v", err)
	}

	re := regexp.MustCompile(rt.String())
	return &trigger{
		re:    re,
		cmd:   args[1],
		args:  args[2:],
		multi: hasMulti(rt),
		buf:   bytes.NewBuffer(nil),
	}, nil
}

type trigger struct {
	re    *regexp.Regexp
	cmd   string
	args  []string
	multi bool
	wg    sync.WaitGroup

	mu  sync.Mutex // gates access to the buffer
	buf *bytes.Buffer
}

// hasMatch reports whether the buffer currently contains a match for the
// pattern, and if so returns the matching indices and the prefix of the buffer
// containing the match. The caller must hold t.mu.
//
// If closing == true, a line match will be attempted even if the buffer does
// not contain a newline.
func (t *trigger) hasMatch(closing bool) ([]int, string, bool) {
	if t.multi {
		// Check for a match of the regexp.
		m := t.re.FindSubmatchIndex(t.buf.Bytes())
		if m == nil {
			// Discard data in excess of the buffer size limit.
			if t.buf.Len() > *bufLimit {
				t.buf.Next(t.buf.Len() - *bufLimit)
			}
			return nil, "", false
		}
		return m, string(t.buf.Next(m[1])), true
	}

	// Scan ahead line-by-line, looking for a match.
	for t.buf.Len() > 0 {
		var line []byte

		if i := bytes.IndexByte(t.buf.Bytes(), '\n'); i >= 0 {
			line = t.buf.Next(i + 1)[:i]
		} else if closing {
			line = t.buf.Next(t.buf.Len())
		} else {
			break
		}
		m := t.re.FindSubmatchIndex(line)
		if m != nil {
			return m, string(line), true
		}

		// No match on this line, but see if there are more
	}
	return nil, "", false
}

// fire starts a subprocess to handle a pattern match with the given submatch
// indices m and content text.
func (t *trigger) fire(m []int, text string) {
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

// Write implements the io.Writer interface.  Data are copied into the internal
// buffer, and if this results in a match the trigger is fired in a goroutine.
func (t *trigger) Write(data []byte) (int, error) {
	t.mu.Lock()
	nw, err := t.buf.Write(data)
	for t.dispatch(false) { // not closing
	}
	t.mu.Unlock()
	return nw, err
}

// dispatch reports whether there is a match in the buffer, and if so
// dispatches a subprocess to handle it.  The caller must hold t.mu.
func (t *trigger) dispatch(closing bool) bool {
	m, text, ok := t.hasMatch(closing)
	if !ok {
		return false
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.fire(m, text)
	}()
	return true
}

// Close implements the io.Closer interface. It handles any remaining matches
// in the buffer, then waits for all subprocesses to exit.
func (t *trigger) Close() error {
	t.mu.Lock()
	for t.dispatch(true) { // closing
	}
	defer t.mu.Unlock()
	t.wg.Wait()
	return nil
}
