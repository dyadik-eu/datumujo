package sqldriver_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/dyadik-eu/datumujo/sqldriver"
)

// Example opens a database through database/sql, writes in a
// transaction and reads the rows back.
func Example() {
	dir, _ := os.MkdirTemp("", "datumujo")
	defer os.RemoveAll(dir)
	db, err := sql.Open("datumujo", filepath.Join(dir, "forge.db"))
	if err != nil {
		panic(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE issue (id INTEGER PRIMARY KEY, title TEXT NOT NULL, state TEXT NOT NULL)`); err != nil {
		panic(err)
	}
	tx, err := db.Begin()
	if err != nil {
		panic(err)
	}
	for _, title := range []string{"crash on start", "typo in README"} {
		if _, err := tx.Exec("INSERT INTO issue (title, state) VALUES (?, 'open')", title); err != nil {
			panic(err)
		}
	}
	if err := tx.Commit(); err != nil {
		panic(err)
	}
	rows, err := db.Query("SELECT id, title FROM issue WHERE state = ? ORDER BY id", "open")
	if err != nil {
		panic(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var title string
		if err := rows.Scan(&id, &title); err != nil {
			panic(err)
		}
		fmt.Println(id, title)
	}
	// Output:
	// 1 crash on start
	// 2 typo in README
}

// TestReadmeExample: the code in README.md is the body of Example, so
// the code there has run.
func TestReadmeExample(t *testing.T) {
	readme, err := os.ReadFile("../README.md")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("example_test.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	body := s[strings.Index(s, "\tdir, _ := os.MkdirTemp"):strings.Index(s, "\t// Output:")]
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		lines = append(lines, strings.TrimPrefix(l, "\t"))
	}
	want := "```go\n" + strings.Join(lines, "\n") + "\n```"
	if !strings.Contains(string(readme), want) {
		t.Errorf("README.md does not hold the body of Example:\n%s", want)
	}
}
