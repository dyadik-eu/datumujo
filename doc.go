// Package datumujo is an embedded database for Go, written with the
// standard library only.
//
// A database is one file with a log next to it. It has typed tables with
// a primary key, secondary and unique indexes, scans in both directions
// and counters. One write transaction runs at a time while readers go on. A
// commit is on stable storage when it returns, and a crash leaves the
// state of the last commit. Every page has a checksum, and a damaged page
// is an error, never data.
//
//	db, err := datumujo.Open("forge.db", datumujo.Options{})
//	err = db.Update(func(tx *datumujo.Tx) error {
//		return tx.Insert("issue", datumujo.Row{int64(1), int64(42), "open"})
//	})
//
// The design is in docs/design.md, the requirements in
// docs/requirements.md.
package datumujo
