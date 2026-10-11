package domain

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// RoundWindow names the range in which a plain (non-time-travel) node can
// answer a request for one specific round.
type RoundWindow int

const (
	// WindowBlock is the block store: OldestRound..LastRound. Full blocks,
	// hashes, state proofs and block headers live here.
	WindowBlock RoundWindow = iota
	// WindowState is the account-state lookback: LastRound-MaxAcctLookback..
	// LastRound. Box listings and simulations at a round live here; the
	// block window overstates them by up to ~1000 rounds.
	WindowState
	// WindowDelta is the account-state lookback without its oldest round:
	// the ledger answers state at its oldest round from disk, but keeps a
	// delta only for the rounds after it. State deltas live here.
	WindowDelta
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
	// TimeTravel marks a request only a time-travel node can answer: state at
	// a past round (?round=) or an object history (/versions). A stock node
	// ignores round on those endpoints and would answer the latest state, so
	// they must never reach one without the history.
	TimeTravel bool
	// RoundWindow is the window a plain node must cover the round with. It is
	// irrelevant to a time-travel node, which answers the round from its
	// history for every round at or before its last when HistoryAnyRound is
	// set, and from its (stored) block window when not.
	RoundWindow RoundWindow
	// HistoryAnyRound marks a round a time-travel node serves from its
	// history for any round at or below its last, whatever RoundWindow says:
	// block headers, box listings and simulations at a round. Requests that
	// still need the stored block (full blocks, state deltas) leave it clear,
	// so a time-travel node serves deltas only within its block window.
	HistoryAnyRound bool
	// Simulate marks POST /v2/transactions/simulate, whose body may carry a
	// round the router refines the class with before it selects.
	Simulate bool
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
	if c.TimeTravel {
		parts = append(parts, "time_travel")
	}
	if c.Round != nil {
		switch c.RoundWindow {
		case WindowState:
			parts = append(parts, "state_window")
		case WindowDelta:
			parts = append(parts, "delta_window")
		}
	}
	if c.HistoryAnyRound {
		parts = append(parts, "history_any_round")
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
	case c.TimeTravel:
		// A history read, whether ?round= or /versions, is its own bucket.
		return "timetravel"
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

// Classify derives the RequestClass from method, path and raw query. It is a
// pure function of its inputs and is the only place route knowledge lives.
func Classify(method, path, rawQuery string) RequestClass {
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
	q := queryValues(rawQuery)
	roundParam := func() (uint64, bool) {
		s := q.Get("round")
		if s == "" {
			return 0, false
		}
		return parseRound(s)
	}
	// Every .../versions endpoint is the time-travel object history, and is
	// registered only when the node has EnableTimeTravel.
	if seg(len(segs)-1) == "versions" {
		c.TimeTravel = true
		return c
	}
	switch seg(1) {
	case "blocks":
		if r, ok := parseRound(seg(2)); ok {
			c.Round = &r
		}
		if q.Get("header-only") == "true" {
			// A time-travel node answers a header from its history at any
			// round; a plain node needs the stored block (WindowBlock).
			c.HistoryAnyRound = true
		}
	case "deltas", "stateproofs":
		if r, ok := parseRound(seg(2)); ok {
			c.Round = &r
			if seg(1) == "deltas" {
				// A plain node serves a delta only from its account-state
				// lookback; a time-travel node re-evaluates one whose block
				// is still stored, so it stays on the block window.
				c.RoundWindow = WindowDelta
			}
		}
		// /v2/deltas/txn/group/{id} and the other non-round delta paths
		// concern no specific round: any synced node.
	case "accounts", "assets", "applications", "ledger":
		// Of the ledger group only supply is proxied; the rest is admin API.
		if seg(1) == "ledger" && seg(2) != "supply" {
			c.LocalOnly = true
			return c
		}
		r, ok := roundParam()
		switch {
		case seg(1) == "applications" && seg(3) == "boxes":
			// Stock algod takes round on a box listing, within its
			// account-state lookback; a time-travel node serves any round.
			if ok {
				c.Round = &r
				c.RoundWindow = WindowState
				c.HistoryAnyRound = true
			}
		case seg(1) == "applications" && seg(3) == "box":
			// Stock algod ignores round here: only the history answers it.
			if ok {
				c.Round = &r
				c.RoundWindow = WindowState
				c.HistoryAnyRound = true
				c.TimeTravel = true
			}
		default:
			if ok {
				c.Round = &r
				c.RoundWindow = WindowState
				c.HistoryAnyRound = true
				c.TimeTravel = true
			}
		}
		if seg(1) == "ledger" && !ok {
			// Supply at the latest round is still the local node's to answer.
			c.LocalOnly = true
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
			c.Simulate = true
		case seg(2) == "params":
			// suggested params: any synced node
		}
	case "participation", "shutdown", "catchup", "devmode":
		c.LocalOnly = true // admin API is never proxied to other nodes
	}
	return c
}

// queryValues parses a raw query string. url.ParseQuery returns every valid
// pair alongside the first error, so a malformed pair must not discard a
// well-formed round (which would let the request reach a plain node and be
// answered with the latest state).
func queryValues(rawQuery string) url.Values {
	if rawQuery == "" {
		return nil
	}
	v, _ := url.ParseQuery(rawQuery)
	return v
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
