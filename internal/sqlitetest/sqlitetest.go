// Package sqlitetest runs the sqlite3 program, the test oracle of P-4.
// Only tests use it. The oracle is a separate program, so the module
// keeps to the standard library (S-2).
package sqlitetest

import (
	"bytes"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Path returns the path of sqlite3. In CI a missing oracle fails the
// test; elsewhere the test skips.
func Path(t testing.TB) string {
	t.Helper()
	path, err := exec.LookPath("sqlite3")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("sqlite3 not found; CI must have the oracle (P-4)")
		}
		t.Skip("sqlite3 not found")
	}
	return path
}

// Run runs script in a database in memory and returns the output lines,
// with fields split at tabs. Output on stderr fails the test: every
// statement of the script must run.
func Run(t testing.TB, script string) [][]string {
	t.Helper()
	cmd := exec.Command(Path(t), "-batch", "-separator", "\t", ":memory:")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil || stderr.Len() > 0 {
		t.Fatalf("sqlite3: %v\n%s", err, stderr.String())
	}
	var lines [][]string
	for _, l := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if l != "" {
			lines = append(lines, strings.Split(l, "\t"))
		}
	}
	return lines
}

var nearLine = regexp.MustCompile(`near line (\d+): (.*)`)

// RunErrors runs script like Run, but a statement may fail. It returns
// the output lines and the errors by the line of the script they are
// on. A line on stderr that names no line of the script fails the test.
func RunErrors(t testing.TB, script string) ([][]string, map[int]string) {
	t.Helper()
	cmd := exec.Command(Path(t), "-batch", "-separator", "\t", ":memory:")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, _ := cmd.Output()
	errs := map[int]string{}
	last := -1
	for _, l := range strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n") {
		if l == "" {
			continue
		}
		m := nearLine.FindStringSubmatch(l)
		if m == nil {
			// Newer versions print the statement and a marker below the
			// error, indented. They belong to the error before.
			if last < 0 || !strings.HasPrefix(l, " ") {
				t.Fatalf("sqlite3: %s", l)
			}
			errs[last] += "\n" + l
			continue
		}
		last, _ = strconv.Atoi(m[1])
		errs[last] = m[2]
	}
	var lines [][]string
	for _, l := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if l != "" {
			lines = append(lines, strings.Split(l, "\t"))
		}
	}
	return lines, errs
}
