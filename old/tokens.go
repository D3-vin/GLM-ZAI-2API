// tokens.go — SQLite-backed device-token store for captcha verification.
// Replaces the globalDB + dbMu pattern from the original token-helper.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	_ "modernc.org/sqlite"
)

type TokenStore struct {
	mu sync.Mutex
	db *sql.DB
}

func OpenTokenStore(dbPath string) (*TokenStore, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("token database not found: %s", dbPath)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &TokenStore{db: db}, nil
}

func (ts *TokenStore) Close() error {
	if ts.db != nil {
		return ts.db.Close()
	}
	return nil
}

// Count returns the number of tokens currently in the store.
// Returns 0 if the store is nil or the query fails.
func (ts *TokenStore) Count() int {
	if ts == nil || ts.db == nil {
		return 0
	}
	var n int
	_ = ts.db.QueryRow("SELECT COUNT(*) FROM tokens;").Scan(&n)
	return n
}

func (ts *TokenStore) getNext() (string, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	var token string
	err := ts.db.QueryRow("SELECT token FROM tokens ORDER BY id LIMIT 1;").Scan(&token)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			captchaLog("No device tokens available in table 'tokens'")
		} else {
			captchaLog("Failed to query token: " + err.Error())
		}
		return "", false
	}
	return token, true
}

func (ts *TokenStore) remove(token string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	_, err := ts.db.Exec("DELETE FROM tokens WHERE token = ?;", token)
	if err != nil {
		captchaLog("Failed to delete consumed token: " + err.Error())
	}
}
