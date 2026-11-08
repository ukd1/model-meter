package main

import (
	"strings"
	"sync"
	"time"
)

// Sign-in attempts are throttled in memory per account and per peer address.
// Both limits are needed: the account limit stops one host guessing a single
// password, and the peer limit stops one host spraying one password across many
// accounts. PBKDF2 at 600k iterations already costs an attacker real time; the
// limiter removes the remaining free online guesses.
//
// The zero value is ready to use, so App values built without a constructor are
// still protected. Counters are advisory and reset on restart, which is
// acceptable because the authoritative control remains the password hash.
type loginLimiter struct {
	mu      sync.Mutex
	win     time.Duration
	account int
	peer    int
	entries map[string]*loginTally
}

const (
	defaultLoginWindow = 15 * time.Minute
	defaultLoginUser   = 8
	defaultLoginPeer   = 30
	// Entries above this count are pruned of expired windows. Live entries are
	// bounded by distinct accounts and peers seen inside one window.
	loginPruneAt = 4096
)

type loginTally struct {
	count int
	first time.Time
}

func (l *loginLimiter) window() time.Duration {
	if l.win > 0 {
		return l.win
	}
	return defaultLoginWindow
}

// limitFor maps a namespaced key to its threshold. Any key without the peer
// prefix shares the stricter account limit.
func (l *loginLimiter) limitFor(key string) int {
	if strings.HasPrefix(key, "p:") {
		if l.peer > 0 {
			return l.peer
		}
		return defaultLoginPeer
	}
	if l.account > 0 {
		return l.account
	}
	return defaultLoginUser
}

// begin checks both budgets and, when the attempt is allowed, records it in the
// same critical section. Recording before the password is verified means a
// concurrent burst cannot slip an unbounded number of extra verifications past
// the limit, which a check-then-act sequence would allow.
func (l *loginLimiter) begin(account, peer string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	window := l.window()
	keys := make([]string, 0, 2)
	if account != "" {
		keys = append(keys, account)
	}
	// An unknown peer (for example a unix socket or a bare relay) is not shared
	// with other clients, so it must not collapse into one common bucket.
	if peer != "" {
		keys = append(keys, peer)
	}
	retry := time.Duration(0)
	for _, key := range keys {
		tally, ok := l.entries[key]
		if !ok {
			continue
		}
		age := now.Sub(tally.first)
		if age >= window {
			delete(l.entries, key)
			continue
		}
		if tally.count >= l.limitFor(key) {
			if wait := window - age; wait > retry {
				retry = wait
			}
		}
	}
	if retry > 0 {
		return retry, true
	}
	l.record(now, window, keys)
	return 0, false
}

// record charges an allowed attempt to every key. The caller holds the lock.
func (l *loginLimiter) record(now time.Time, window time.Duration, keys []string) {
	if l.entries == nil {
		l.entries = make(map[string]*loginTally)
	}
	if len(l.entries) > loginPruneAt {
		for key, tally := range l.entries {
			if now.Sub(tally.first) >= window {
				delete(l.entries, key)
			}
		}
	}
	for _, key := range keys {
		tally, ok := l.entries[key]
		if !ok || now.Sub(tally.first) >= window {
			l.entries[key] = &loginTally{count: 1, first: now}
			continue
		}
		tally.count++
	}
}

// succeed clears the account budget and refunds the peer charge, so ordinary
// successful sign-ins do not consume the shared per-peer allowance while a
// genuinely failing peer still spends it. Clearing the peer counter outright
// would let an attacker reset their budget whenever they control one account.
func (l *loginLimiter) succeed(account, peer string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if account != "" {
		delete(l.entries, account)
	}
	if tally, ok := l.entries[peer]; ok {
		if tally.count <= 1 {
			delete(l.entries, peer)
		} else {
			tally.count--
		}
	}
}
