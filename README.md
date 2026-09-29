# datumujo

An embedded database for Go, written with the Go standard library only.
datumujo is Esperanto for "data container".

**Status: v0.2.0.** Stage 1 is a storage engine with a typed Go API: tables
with a primary key, secondary and unique indexes, scans and counters. One
writer runs while readers never wait, and there are backup and a check
command. Stage 2 adds a core subset of SQL, a driver for `database/sql`
and a shell. The API and the file format can still change before v1; the
roadmap says what v1.0 promises.

## Use

SQL through `database/sql`, with the driver of this module. The code
below runs as `Example` in `sqldriver/example_test.go`, and its output
is checked there.

```go
import (
	"database/sql"

	_ "github.com/dyadik-eu/datumujo/sqldriver"
)
```

```go
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
```

From the shell, `datumujo sql FILE` runs the SQL of its input:

```sh
datumujo sql forge.db "SELECT id, title FROM issue WHERE state = 'open'"
```

The typed Go API of stage 1 and the SQL of stage 2 work on the same
file. The SQL and its limits are in [docs/sql.md](docs/sql.md), and
each difference to SQLite is in
[docs/requirements.md](docs/requirements.md).

## Goals

- One database file, used in the process of the program. There is no server.
- Transactions that stay correct after a crash or a power loss.
- A checksum on every page. The database returns an error on damaged data.
  It never returns wrong data.
- No dependency outside the Go standard library, and no cgo.
- A documented, versioned file format.

The first user is a code forge, and its needs shaped the first stage. The
requirements are in
[docs/requirements.md](docs/requirements.md), the design in
[docs/design.md](docs/design.md), and the steps in
[docs/roadmap.md](docs/roadmap.md). The roadmap also says what v1.0
promises: a file format and an API that stay.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) describes how a change gets in.
Report a security problem or wrong data in private, as
[SECURITY.md](SECURITY.md) describes.

## Licence

[EUPL-1.2](LICENSE).
