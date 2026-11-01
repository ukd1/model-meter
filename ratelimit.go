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

// blocked reports whether either key is over its limit, and how long the caller
// should wait before retrying.
func (l *loginLimiter) blocked(keys ...string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	window := l.window()
	retry := time.Duration(0)
	limited := false
	for _, key := range keys {
		tally, ok := l.entries[key]
		if !ok {
			continue
		}
		if age := now.Sub(tally.first); age >= window {
			delete(l.entries, key)
			continue
		}
		if tally.count >= l.limitFor(key) {
			if wait := window - now.Sub(tally.first); wait > retry {
				retry = wait
			}
			limited = true
		}
	}
	return retry, limited
}

// fail records a rejected attempt. Expired windows restart, and the map is
// pruned when it grows so a flood of distinct usernames cannot grow it forever.
func (l *loginLimiter) fail(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	window := l.window()
	if l.entries == nil {
		l.entries = make(map[string]*loginTally)
	}
	if len(l.entries) > 4096 {
		for key, tally := range l.entries {
			if now.Sub(tally.first) >= window {
				delete(l.entries, key)
			}
		}
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		tally, ok := l.entries[key]
		if !ok || now.Sub(tally.first) >= window {
			l.entries[key] = &loginTally{count: 1, first: now}
			continue
		}
		tally.count++
	}
}

// succeed clears the account counter so one legitimate sign-in is not punished
// by earlier typos. The peer counter is deliberately kept: clearing it would let
// an attacker reset their budget whenever they control one valid account.
func (l *loginLimiter) succeed(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range keys {
		delete(l.entries, key)
	}
}
