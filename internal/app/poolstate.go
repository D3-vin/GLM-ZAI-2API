// poolstate.go — persistent per-account status for the Z.AI token pool.
//
// zaitoken.go keeps the pool as an in-memory round-robin (zaiTokenPool), so a
// process restart wipes every account verdict: accounts banned mid-life
// (USER_BLOCKED / 401) are re-probed as if fresh, and the init scan re-burns
// guest device tokens for dead accounts on every boot. This layer persists
// alive/dead verdicts to the existing tokens.sqlite DB (table token_status,
// created on open) so the pool keeps its memory across restarts.
//
// Design rules (mirror classifyAuthCheck's offline tolerance):
//   - only DEFINITIVE verdicts are persisted: alive (2xx+id) and dead
//     (401/403/410). Ambiguous network/parse results are never written.
//   - state is advisory: a missing row or a read error degrades to the old
//     offline-tolerant behavior (probing decides), so a broken state DB can
//     never brick the service.
//   - keys are FNV-64a hex fingerprints — full JWTs are secret material and
//     must not sit in a second store next to the pool file.
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// poolStateTTL bounds how long an alive verdict is trusted without a re-probe
// (accounts die: bans, 401 rotation). The auths/ probe is a cheap
// authenticated GET, so re-verifying after an hour is cheap insurance.
const poolStateTTL = time.Hour

// poolDeadGrace bounds how long a definitive dead verdict silences the
// account before the pool re-probes it on purpose (platform bans can lift).
const poolDeadGrace = 10 * time.Minute

var (
	poolStateMu  sync.Mutex
	poolStateDB  *sql.DB
	poolStateRow = poolState{} // last verified state of the in-use account
	poolStateErr = false       // set on DB failure → callers fall back to legacy
)

type poolState struct {
	alive            bool
	aliveExpiryUnix  int64
	deadUntilUnix    int64
	failCount        int
	lastError        string
	lastVerifiedUnix int64
}

const stateSelect = `SELECT alive, alive_expiry, dead_until, fail_count, last_error, updated_at FROM token_status WHERE key = ?`

// accountStateKey is a stable, non-reversible token fingerprint.
func accountStateKey(token string) string {
	h := fnv.New64a()
	h.Write([]byte(token))
	return fmt.Sprintf("a%x", h.Sum64())
}

// openPoolState binds the persistent pool state to the token DB. It reuses the
// TokenStore's *sql.DB handle — opening a SECOND connection to tokens.sqlite
// would contend for SQLite's single write lock and make captcha verification
// fail under load (modernc/sqlite defaults to busy_timeout=0).
func openPoolState(ts *TokenStore) {
	poolStateMu.Lock()
	defer poolStateMu.Unlock()
	if poolStateDB != nil || ts == nil || ts.db == nil {
		return
	}
	poolStateDB = ts.db
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := poolStateDB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS token_status (
		key TEXT PRIMARY KEY,
		alive INTEGER NOT NULL DEFAULT 0,
		alive_expiry INTEGER NOT NULL DEFAULT 0,
		dead_until INTEGER NOT NULL DEFAULT 0,
		fail_count INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL DEFAULT 0
	)`)
	cancel()
	if err != nil {
		poolStateDB = nil
		log.Printf("[PoolState] schema failed: %v (state disabled)", err)
		return
	}
	log.Printf("[PoolState] pool state persistence enabled (table token_status, shared connection)")
}

// recordVerdict persists a definitive auths/ verdict and refreshes the in-use
// row. Ambiguous results must NOT be recorded (offline tolerance).
func recordVerdict(token, verdict string) {
	poolStateMu.Lock()
	defer poolStateMu.Unlock()
	if poolStateErr || poolStateDB == nil || token == "" {
		return
	}
	now := time.Now().Unix()
	var alive, failCount int
	var aliveExpiry, deadUntil int64
	switch verdict {
	case "alive":
		alive = 1
		aliveExpiry = now + int64(poolStateTTL.Seconds())
		failCount = 0
	case "dead":
		alive = 0
		deadUntil = now + int64(poolDeadGrace.Seconds())
		failCount = poolStateRow.failCount + 1
	default:
		return // ambiguous: leave previous state untouched
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := poolStateDB.ExecContext(ctx,
		`INSERT INTO token_status (key, alive, alive_expiry, dead_until, fail_count, last_error, updated_at)
		 VALUES (?, ?, ?, ?, ?, '', ?)
		 ON CONFLICT(key) DO UPDATE SET
			alive = excluded.alive,
			alive_expiry = excluded.alive_expiry,
			dead_until = excluded.dead_until,
			fail_count = excluded.fail_count,
			updated_at = excluded.updated_at`,
		accountStateKey(token), alive, aliveExpiry, deadUntil, failCount, now)
	if err != nil {
		log.Printf("[PoolState] write failed: %v (state disabled)", err)
		poolStateErr = true
		return
	}
	loadRowLocked(token)
}

// loadRowLocked refreshes the in-use row from disk. Caller must hold poolStateMu.
func loadRowLocked(token string) {
	if poolStateErr || poolStateDB == nil || token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := poolStateDB.QueryRowContext(ctx, stateSelect, accountStateKey(token)).
		Scan(&poolStateRow.alive, &poolStateRow.aliveExpiryUnix, &poolStateRow.deadUntilUnix, &poolStateRow.failCount, &poolStateRow.lastError, &poolStateRow.lastVerifiedUnix); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			poolStateRow = poolState{}
			return
		}
		log.Printf("[PoolState] read failed: %v (state disabled)", err)
		poolStateErr = true
		return
	}
	if poolStateRow.alive && poolStateRow.aliveExpiryUnix <= time.Now().Unix() {
		poolStateRow.alive = false // verdict expired → re-probe on next init
	}
}

// loadRow hydrates the in-use row from disk (call after setHardcodedAccount).
func loadRow(token string) {
	poolStateMu.Lock()
	defer poolStateMu.Unlock()
	loadRowLocked(token)
}

// poolAccountUsable reports whether the persistent state still considers the
// token usable (alive verdict unexpired, or no verdict yet). Advisory: on
// error it returns true so the legacy probing path still runs.
func poolAccountUsable(token string) bool {
	poolStateMu.Lock()
	defer poolStateMu.Unlock()
	if poolStateErr || poolStateDB == nil || token == "" {
		return true
	}
	now := time.Now().Unix()
	var alive, aliveExpiry, deadUntil int
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := poolStateDB.QueryRowContext(ctx, stateSelect, accountStateKey(token)).
		Scan(&alive, &aliveExpiry, &deadUntil, new(int), new(string), new(int64)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true // no verdict yet → legacy probing decides
		}
		return true // read error → offline tolerance
	}
	if alive == 1 && int64(aliveExpiry) > now {
		return true
	}
	if int64(deadUntil) > now {
		return false // definitive dead, still in grace window
	}
	return true // dead but grace expired → re-probe
}

// poolDeadSilencedUntil reports the earliest time the given token may be
// probed again after a definitive dead verdict (zero = no silence).
func poolDeadSilencedUntil(token string) time.Time {
	poolStateMu.Lock()
	defer poolStateMu.Unlock()
	if poolStateErr || poolStateDB == nil || token == "" {
		return time.Time{}
	}
	var deadUntil int
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := poolStateDB.QueryRowContext(ctx, stateSelect, accountStateKey(token)).
		Scan(new(int), new(int), &deadUntil, new(int), new(string), new(int64)); err != nil {
		return time.Time{}
	}
	if int64(deadUntil) > time.Now().Unix() {
		return time.Unix(int64(deadUntil), 0)
	}
	return time.Time{}
}
