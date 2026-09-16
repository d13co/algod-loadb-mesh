package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// RequestClass is everything routing needs to know about a client request.
type RequestClass struct {
	Round      *uint64 // request concerns one specific round
	NeedsDev   bool    // /v2/teal/* requires EnableDeveloperAPI
	Broadcast  bool    // POST /v2/transactions
	PendingID  string  // /v2/transactions/pending/{txid}
	WaitAfter  *uint64 // /v2/status/wait-for-block-after/{r}
	LocalOnly  bool    // must be answered by the local node (or the agent)
	Agent      bool    // /loadb/* served by the agent itself
	Idempotent bool    // safe to retry on another upstream before bytes were sent
}

// String renders the class for logs.
func (c RequestClass) String() string {
	var parts []string
	if c.Round != nil {
		parts = append(parts, fmt.Sprintf("round=%d", *c.Round))
	}
	if c.WaitAfter != nil {
		parts = append(parts, fmt.Sprintf("wait_after=%d", *c.WaitAfter))
	}
	if c.NeedsDev {
		parts = append(parts, "dev")
	}
	if c.Broadcast {
		parts = append(parts, "broadcast")
	}
	if c.PendingID != "" {
		parts = append(parts, "pending="+c.PendingID)
	}
	if c.LocalOnly {
		parts = append(parts, "local_only")
	}
	if c.Idempotent {
		parts = append(parts, "idempotent")
	}
	return strings.Join(parts, ",")
}

// StatKey buckets requests for per-class latency tracking.
func (c RequestClass) StatKey() string {
	switch {
	case c.Round != nil:
		return "round"
	case c.Broadcast:
		return "broadcast"
	case c.PendingID != "":
		return "pending"
	case c.WaitAfter != nil:
		return "wait"
	case c.NeedsDev:
		return "teal"
	}
	return "default"
}

// Classify derives the RequestClass from method and path. It is a pure
// function of its inputs and is the only place route knowledge lives.
func Classify(method, path string) RequestClass {
	var c RequestClass
	method = strings.ToUpper(method)
	c.Idempotent = method == "GET" || method == "HEAD"
	p := strings.TrimSuffix(path, "/")
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	seg := func(i int) string {
		if i < len(segs) {
			return segs[i]
		}
		return ""
	}

	if seg(0) == "loadb" {
		c.Agent = true
		c.LocalOnly = true
		return c
	}
	switch p {
	case "/health", "/ready", "/metrics", "/swagger.json", "/versions", "/genesis":
		c.LocalOnly = true
		return c
	}
	if seg(0) != "v2" {
		// Unknown top-level: keep it local, it is probably admin or a probe.
		c.LocalOnly = true
		return c
	}
	switch seg(1) {
	case "blocks", "deltas", "stateproofs":
		if r, ok := parseRound(seg(2)); ok {
			c.Round = &r
		}
		if seg(1) == "deltas" && seg(2) == "txn" {
			// /v2/deltas/txn/group/{id}: round unknown, any synced node.
			c.Round = nil
		}
	case "status":
		if seg(2) == "wait-for-block-after" {
			if r, ok := parseRound(seg(3)); ok {
				c.WaitAfter = &r
			}
		}
	case "teal":
		c.NeedsDev = true
		c.Idempotent = true // compile/dryrun/disassemble are side-effect free
	case "transactions":
		switch {
		case seg(2) == "" && method == "POST":
			c.Broadcast = true
			c.Idempotent = false
		case seg(2) == "pending" && seg(3) != "":
			c.PendingID = seg(3)
		case seg(2) == "pending":
			c.LocalOnly = true // pool dump is only meaningful per node
		case seg(2) == "simulate":
			c.Idempotent = true
		case seg(2) == "params":
			// suggested params: any synced node
		}
	case "participation", "shutdown", "catchup", "ledger", "devmode":
		c.LocalOnly = true // admin API is never proxied to other nodes
	}
	return c
}

func parseRound(s string) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	r, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return r, true
}
