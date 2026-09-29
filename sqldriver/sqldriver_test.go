package sqldriver_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dyadik-eu/datumujo"
	"github.com/dyadik-eu/datumujo/sqldriver"
)

func open(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("datumujo", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDriver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	db := open(t, path)
	if _, err := db.Exec(`CREATE TABLE repo (id INTEGER PRIMARY KEY, name TEXT NOT NULL, stars INTEGER,
		score REAL, public BOOLEAN, logo BLOB, created TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE UNIQUE INDEX by_name ON repo (name)"); err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 9, 29, 8, 30, 0, 0, time.UTC)
	res, err := db.Exec("INSERT INTO repo (name, stars, score, public, logo, created) VALUES (?, ?, ?, ?, ?, ?)",
		"alpha", 5, float32(2.5), true, []byte{1, 2}, created)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := res.LastInsertId(); id != 1 {
		t.Errorf("LastInsertId %d", id)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("RowsAffected %d", n)
	}
	if _, err := db.Exec("INSERT INTO repo (name) VALUES (?2), (?1)", "gamma", "beta"); err != nil {
		t.Fatal(err)
	}
	var (
		name     string
		stars    sql.NullInt64
		score    float64
		public   bool
		logo     []byte
		created2 time.Time
	)
	if err := db.QueryRow("SELECT name, stars, score, public, logo, created FROM repo WHERE id = ?", 1).
		Scan(&name, &stars, &score, &public, &logo, &created2); err != nil {
		t.Fatal(err)
	}
	if name != "alpha" || stars.Int64 != 5 || score != 2.5 || !public || string(logo) != "\x01\x02" || !created2.Equal(created) {
		t.Errorf("row: %v %v %v %v %v %v", name, stars, score, public, logo, created2)
	}
	rows, err := db.Query("SELECT id, name FROM repo ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.ColumnTypes()
	if cols[0].DatabaseTypeName() != "INTEGER" || cols[1].DatabaseTypeName() != "TEXT" {
		t.Errorf("types %s %s", cols[0].DatabaseTypeName(), cols[1].DatabaseTypeName())
	}
	var got []string
	for rows.Next() {
		var id int64
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d %s", id, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if strings.Join(got, "|") != "1 alpha|2 beta|3 gamma" {
		t.Errorf("rows %v", got)
	}
	// The error of the engine reaches the program through database/sql.
	_, err = db.Exec("INSERT INTO repo (name) VALUES ('alpha')")
	var se *datumujo.SQLError
	if !errors.Is(err, datumujo.ErrUnique) || !errors.As(err, &se) {
		t.Errorf("unique conflict: %v", err)
	}
	if _, err := db.Exec("SELECT 1 FROM repo WHERE id = :id", sql.Named("id", 1)); err == nil || !strings.Contains(err.Error(), "named parameter") {
		t.Errorf("named parameter: %v", err)
	}
	if _, err := db.Query("DELETE FROM repo"); err == nil {
		t.Error("DELETE through Query: no error")
	}
	// An error in a later row ends the rows with Err.
	rows, err = db.Query("SELECT 10 / (2 - id) FROM repo ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("error in the second row: %v", err)
	}
	rows.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := db.ExecContext(ctx, "DELETE FROM repo"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled context: %v", err)
	}
}

func TestTransactions(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "db"))
	if _, err := db.Exec("CREATE TABLE t (n INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	count := func(q *sql.DB) int {
		var n int
		if err := q.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := tx.Exec("INSERT INTO t VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	// A failed statement changes nothing, and the transaction goes on.
	if _, err := tx.Exec("INSERT INTO t VALUES (4), (1)"); !errors.Is(err, datumujo.ErrExists) {
		t.Errorf("duplicate key in a Tx: %v", err)
	}
	var inTx int
	if err := tx.QueryRow("SELECT count(*) FROM t").Scan(&inTx); err != nil || inTx != 3 {
		t.Errorf("the Tx sees %d rows, %v", inTx, err)
	}
	// Another connection reads the last commit and does not wait.
	if n := count(db); n != 0 {
		t.Errorf("before commit, another connection sees %d rows", n)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := count(db); n != 3 {
		t.Errorf("after commit: %d rows", n)
	}
	tx, _ = db.Begin()
	tx.Exec("DELETE FROM t")
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := count(db); n != 3 {
		t.Errorf("after rollback: %d rows", n)
	}
	// A read-only transaction reads its snapshot and does not write.
	ro, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO t VALUES (9)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ro.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 3 {
		t.Errorf("snapshot: %d %v", n, err)
	}
	if _, err := ro.Exec("INSERT INTO t VALUES (10)"); !errors.Is(err, datumujo.ErrSession) {
		t.Errorf("write in a read-only Tx: %v", err)
	}
	ro.Rollback()
}

// TestSQLTransactionText runs BEGIN, COMMIT and ROLLBACK as SQL text on
// one connection.
func TestSQLTransactionText(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "db"))
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, "CREATE TABLE t (n INTEGER); BEGIN; INSERT INTO t VALUES (1); ROLLBACK; BEGIN; INSERT INTO t VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, "BEGIN"); !errors.Is(err, datumujo.ErrSession) {
		t.Errorf("BEGIN in a transaction: %v", err)
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); !errors.Is(err, datumujo.ErrSession) {
		t.Errorf("COMMIT outside a transaction: %v", err)
	}
	// A transaction that the text leaves open ends when the connection
	// goes back to the pool: the next user does not get it.
	if _, err := c.ExecContext(ctx, "BEGIN; INSERT INTO t VALUES (3)"); err != nil {
		t.Fatal(err)
	}
	c.Close()
	var got []int
	rows, err := db.Query("SELECT n FROM t ORDER BY n")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int
		rows.Scan(&n)
		got = append(got, n)
	}
	rows.Close()
	if fmt.Sprint(got) != "[2]" {
		t.Errorf("rows %v, want [2]", got)
	}
	// The writer is free again: a write does not wait for ever.
	done := make(chan error, 1)
	go func() {
		_, err := db.Exec("INSERT INTO t VALUES (4)")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write waits: the transaction of the closed connection is still open")
	}
}

// TestSharedFile: two sql.DB of one file share the open database, and
// the last Close releases the file.
func TestSharedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	a, err := sql.Open("datumujo", path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sql.Open("datumujo", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec("CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec("INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("second sql.DB: %v", err)
	}
	a.Close()
	if _, err := b.Exec("INSERT INTO t VALUES (2)"); err != nil {
		t.Fatalf("after the first Close: %v", err)
	}
	if _, err := datumujo.Open(path, datumujo.Options{}); !errors.Is(err, datumujo.ErrLocked) {
		t.Errorf("open while b uses the file: %v, want ErrLocked", err)
	}
	b.Close()
	db, err := datumujo.Open(path, datumujo.Options{})
	if err != nil {
		t.Fatalf("open after the last Close: %v", err)
	}
	db.Close()
}

// TestConnector uses a database that the program has open.
func TestConnector(t *testing.T) {
	d, err := datumujo.Open(filepath.Join(t.TempDir(), "db"), datumujo.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	db := sql.OpenDB(sqldriver.NewConnector(d))
	if _, err := db.Exec("CREATE TABLE t (n INTEGER)"); err != nil {
		t.Fatal(err)
	}
	// A transaction that a connection leaves open ends when database/sql
	// closes the connection.
	ctx := context.Background()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ExecContext(ctx, "BEGIN; INSERT INTO t VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	c.Close()
	// The database stays open for the program, and the writer is free.
	done := make(chan error, 1)
	go func() {
		_, err := d.Exec("INSERT INTO t VALUES (1)")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("after sql.DB.Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write waits: the closed connection kept its transaction")
	}
	var n int64
	if err := d.Read(func(v *datumujo.View) error {
		rows, err := v.Query("SELECT count(*) FROM t WHERE n = 7")
		if err != nil {
			return err
		}
		defer rows.Close()
		rows.Next()
		n = rows.Row()[0].(int64)
		return nil
	}); err != nil || n != 0 {
		t.Errorf("the INSERT of the closed connection: %d rows, %v", n, err)
	}
}

// TestPool runs writers and readers on a pool of four connections at
// once. Run with -race.
func TestPool(t *testing.T) {
	db := open(t, filepath.Join(t.TempDir(), "db"))
	db.SetMaxOpenConns(4)
	if _, err := db.Exec("CREATE TABLE t (id INTEGER PRIMARY KEY, w INTEGER)"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				tx, err := db.Begin()
				if err != nil {
					errs <- err
					return
				}
				if _, err := tx.Exec("INSERT INTO t (w) VALUES (?)", w); err != nil {
					tx.Rollback()
					errs <- err
					return
				}
				if err := tx.Commit(); err != nil {
					errs <- err
					return
				}
			}
		}(w)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				var n, s int64
				if err := db.QueryRow("SELECT count(*), coalesce(sum(w), 0) FROM t").Scan(&n, &s); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil || n != 200 {
		t.Errorf("%d rows, %v; want 200", n, err)
	}
}
