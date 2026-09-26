// Command datumujo works on a database file.
//
//	datumujo check FILE
//
// check reads the whole file and prints each damaged page and each fault
// in its structure, then statistics. It exits 0 when the file is intact, 1
// on a finding, and 2 when it could not check.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/dyadik-eu/datumujo/internal/check"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

func main() {
	os.Exit(run(vfs.OS{}, os.Args[1:], os.Stdout, os.Stderr))
}

const usage = "usage: datumujo check FILE"

// run is the command without the process around it.
func run(fs vfs.FS, args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "check" {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	r, err := check.Run(fs, args[1])
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
