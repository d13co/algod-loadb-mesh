package app

import (
	"sync"
	"time"
)

// txnPins remembers which node accepted a txn so pending lookups go there
// first. Two generations rotate every ttl, so a pin lives between ttl and
// 2*ttl and inserts never scan the table.
type txnPins struct {
	ttl time.Duration

	mu      sync.Mutex
	cur     map[string]string // txid -> upstream ID
	prev    map[string]string
	rotated time.Time
}

func newTxnPins(ttl time.Duration, now time.Time) *txnPins {
	return &txnPins{ttl: ttl, cur: map[string]string{}, prev: map[string]string{}, rotated: now}
}

// rotate drops generations older than ttl; the caller holds mu.
func (p *txnPins) rotate(now time.Time) {
	switch age := now.Sub(p.rotated); {
	case age >= 2*p.ttl:
		p.prev, p.cur, p.rotated = map[string]string{}, map[string]string{}, now
	case age >= p.ttl:
		p.prev, p.cur, p.rotated = p.cur, map[string]string{}, now
	}
}

func (p *txnPins) remember(txid, upstream string, now time.Time) {
	if txid == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rotate(now)
	p.cur[txid] = upstream
}

func (p *txnPins) recall(txid string, now time.Time) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rotate(now)
	if u, ok := p.cur[txid]; ok {
		return u, true
	}
	u, ok := p.prev[txid]
	return u, ok
}
