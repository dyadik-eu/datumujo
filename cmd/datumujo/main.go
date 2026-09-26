// Command datumujo works on a database file.
//
//	datumujo check FILE
//	datumujo backup FILE COPY
//	datumujo restore COPY FILE
//
// check reads the whole file and prints each damaged page and each fault
// in its structure, then statistics. It exits 0 when the file is intact, 1
// on a finding, and 2 when it could not check.
//
// backup copies a database that no program has open. A program that has
// it open makes the copy with backup.Backup. restore makes a database
// from a copy. Neither replaces a file. Both exit 0 when the copy is made
// and the check accepts it, and 1 otherwise.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/dyadik-eu/datumujo/internal/backup"
	"github.com/dyadik-eu/datumujo/internal/check"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

func main() {
	os.Exit(run(vfs.OS{}, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = "usage: datumujo check FILE | backup FILE COPY | restore COPY FILE"

// run is the command without the process around it.
func run(fs vfs.FS, args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 2 && args[0] == "check":
		return runCheck(fs, args[1], stdout, stderr)
	case len(args) == 3 && (args[0] == "backup" || args[0] == "restore"):
		// A backup of a database that no program has open is a restore
		// under another name: both read the source read-only.
		if err := backup.Restore(fs, args[1], args[2]); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "%s written; the check accepts it\n", args[2])
		return 0
	}
	fmt.Fprintln(stderr, usage)
	return 2
}

func runCheck(fs vfs.FS, name string, stdout, stderr io.Writer) int {
	r, err := check.Run(fs, name)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	for _, f := range r.Findings {
		fmt.Fprintln(stdout, "fault:", f)
	}
	st := r.Stats
	fmt.Fprintf(stdout, "file: %d bytes, log: %d bytes\n", st.FileBytes, st.LogBytes)
	if !st.Opened {
		fmt.Fprintln(stdout, "the database could not be opened; nothing else was counted")
	} else {
		fmt.Fprintf(stdout, "pages: %d of %d bytes, %d free, %d in the catalog\n", st.Pages, st.PageSize, st.FreePages, st.CatalogPages)
		fmt.Fprintf(stdout, "schema version: %d\n", st.SchemaVersion)
		for _, t := range st.Tables {
			fmt.Fprintf(stdout, "table %s: %d rows, %d pages\n", t.Name, t.Rows, t.Pages)
			for _, ix := range t.Indexes {
				fmt.Fprintf(stdout, "  index %s: %d entries, %d pages\n", ix.Name, ix.Entries, ix.Pages)
			}
		}
	}
	if len(r.Findings) > 0 {
		fmt.Fprintf(stdout, "%d faults\n", len(r.Findings))
		return 1
	}
	fmt.Fprintln(stdout, "intact")
	return 0
}
