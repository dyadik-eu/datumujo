// Command datumujo works on a database file.
//
//	datumujo check FILE
//	datumujo backup FILE COPY
//	datumujo restore COPY FILE
//	datumujo sql FILE [SQL]
//
// check reads the whole file and prints each damaged page and each fault
// in its structure, then statistics. It exits 0 when the file is intact, 1
// on a finding, and 2 when it could not check.
//
// backup copies a database that no program has open. A program that has
// it open makes the copy with backup.Backup. restore makes a database
// from a copy. Neither replaces a file. Both exit 0 when the copy is made
// and the check accepts it, and 1 otherwise.
//
// sql runs the SQL text of its argument, or else of its input, on the
// database in FILE. It creates the file if it does not exist (L-12).
// For each SELECT it prints a line with the names of the columns, then
// one line per row, the values separated by a tab. Three kinds of text
// print as a SQL literal in quotes. They are a text with a tab or a line
// break, the text NULL, and a text that starts with a quote.
//
// The first statement that fails stops the text; a transaction
// that is still open rolls back. It exits 0 when all statements ran, 1
// when one failed, and 2 when it could not open the file. It works on
// the file system of the operating system.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dyadik-eu/datumujo"
	"github.com/dyadik-eu/datumujo/internal/backup"
	"github.com/dyadik-eu/datumujo/internal/check"
	"github.com/dyadik-eu/datumujo/internal/sqlexec"
	"github.com/dyadik-eu/datumujo/internal/vfs"
)

func main() {
	os.Exit(run(vfs.OS{}, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

const usage = "usage: datumujo check FILE | backup FILE COPY | restore COPY FILE | sql FILE [SQL]"

// run is the command without the process around it.
func run(fs vfs.FS, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch {
	case (len(args) == 2 || len(args) == 3) && args[0] == "sql":
		text := ""
		if len(args) == 3 {
			text = args[2]
		} else {
			b, err := io.ReadAll(stdin)
			if err != nil {
				fmt.Fprintln(stderr, err)
				return 2
			}
			text = string(b)
		}
		return runSQL(args[1], text, stdout, stderr)
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

// runSQL runs a SQL text on the database in the file path.
func runSQL(path, text string, stdout, stderr io.Writer) int {
	db, err := datumujo.Open(path, datumujo.Options{})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer db.Close()
	s := db.Session()
	defer s.Close()
	_, err = s.Script(text, func(rows *datumujo.SQLRows) error {
		var names []string
		for _, c := range rows.Columns() {
			names = append(names, c.Name)
		}
		fmt.Fprintln(stdout, strings.Join(names, "\t"))
		for rows.Next() {
			var vals []string
			for _, v := range rows.Row() {
				vals = append(vals, cell(v))
			}
			fmt.Fprintln(stdout, strings.Join(vals, "\t"))
		}
		return rows.Err()
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// cell prints a value for a line of the output. A text that the line
// could not show as it is prints as a SQL literal.
func cell(v any) string {
	if s, ok := v.(string); ok && (strings.ContainsAny(s, "\t\n\r") || s == "NULL" || strings.HasPrefix(s, "'")) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return sqlexec.Text(v)
}
