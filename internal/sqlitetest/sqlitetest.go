// Package sqlitetest runs the sqlite3 program, the test oracle of P-4.
// Only tests use it. The oracle is a separate program, so the module
// keeps to the standard library (S-2).
package sqlitetest

import (
	"bytes"
	"os"
	"os/exec"
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
