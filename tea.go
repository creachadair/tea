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
	"sync"
)

var (
	bufLimit    = flag.Int("buf", 1<<16, "Match buffer size limit (bytes)")
	doMultiLine = flag.Bool("m", false, "Allow matches to span multiple lines")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: %s [options] [regexp command args...]

Copy standard input to standard output. If a regexp and command are
given, each match of the regexp in the input triggers execution of the
given command and arguments.

Regular expression syntax: https://pkg.go.dev/regexp/syntax

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
	re   *regexp.Regexp
	cmd  string
	args []string

	mu  sync.Mutex
	wg  sync.WaitGroup
	buf *bytes.Buffer
}

func (t *trigger) hasMatch(closing bool) ([]int, string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if *doMultiLine {
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

func (t *trigger) fire(closing bool) {
	m, text, ok := t.hasMatch(closing)
	if !ok {
		return
	}

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
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.fire(false)
		}()
	}
	return nw, err
}

func (t *trigger) Close() error {
	t.wg.Wait()
	t.fire(true)
	return nil
}
