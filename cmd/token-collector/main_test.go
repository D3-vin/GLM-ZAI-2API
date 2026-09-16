package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func countTokens(t *testing.T, db *sql.DB) (int, int) {
	t.Helper()
	var rows, uniq int
	if err := db.QueryRow("SELECT COUNT(*) FROM tokens").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(DISTINCT token) FROM tokens").Scan(&uniq); err != nil {
		t.Fatal(err)
	}
	return rows, uniq
}

func TestEnsureTokenIndexHealsDuplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE tokens (id INTEGER PRIMARY KEY, token TEXT);"); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"a", "b", "a", "b", "c"} {
		if _, err := db.Exec("INSERT INTO tokens (token) VALUES (?);", tok); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureTokenIndex(db); err != nil {
		t.Fatalf("ensureTokenIndex on polluted DB: %v", err)
	}
	if rows, uniq := countTokens(t, db); rows != 3 || uniq != 3 {
		t.Fatalf("after heal rows=%d uniq=%d, want 3/3", rows, uniq)
	}
	if err := ensureTokenIndex(db); err != nil {
		t.Fatalf("ensureTokenIndex is not idempotent: %v", err)
	}
	if _, err := db.Exec("INSERT OR IGNORE INTO tokens (token) VALUES (?);", "a"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := countTokens(t, db); rows != 3 {
		t.Fatalf("INSERT OR IGNORE did not dedupe: rows=%d", rows)
	}
}
