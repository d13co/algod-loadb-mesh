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
	Ahead      Reply   // algod's reply when Round is past its latest; zero when unknown
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
	// SimNeeds is what a simulate request's body asks of the node that runs
	// it. The router fills it in once it has read the body (ApplySimulate);
	// it is nil before that and for every other request.
	SimNeeds *SimulateNeeds
	// RoundHigh is the latest round a simulate request's "rounds" lists: a
	// node must have reached it too, while Round, the earliest, is the one
	// its window must cover. 0 when the request names at most one round.
	RoundHigh uint64
	// SimulateLimits marks GET /v2/transactions/simulate/limits, which the
	// router answers for the fleet (dtalgorand).
	SimulateLimits bool
}

// SimulateNeeds is what a simulate request needs of the node that runs it.
// It is the router's preference, not algod's rule: when no node fits, the
// router relaxes it (Relaxed) and sends the request on, so that a node
// answers it, with its own error if need be, as a single algod would.
type SimulateNeeds struct {
	Groups            int    // len(txn-groups)
	Rounds            int    // len(rounds), duplicates included
	ExtraOpcodeBudget uint64 // extra-opcode-budget
	// Fork marks an option only dtalgorand has: parallel-groups, a validity
	// window mode or a protocol. Groups and Rounds beyond stock algod's are
	// told by the limits instead.
	Fork bool
	// DeveloperAPI marks an execution trace or protocol future, which algod
	// simulates only with EnableDeveloperAPI.
	DeveloperAPI bool
	// RoundsSpan marks Round and RoundHigh as taken from rounds, which a
	// node answers round by round: one it cannot simulate is an error in
	// that round's result, not in the response.
	RoundsSpan bool
	// Latest marks a rounds list with a 0 entry, which the node resolves to
	// its own last round: the node must be in sync, even though it holds
	// the past rounds the list names too.
	Latest bool
}

// Fits reports whether a node with capabilities c accepts the request.
func (n SimulateNeeds) Fits(c Capabilities) bool {
	if n.Fork && c.Simulate == nil {
		return false
	}
	if n.DeveloperAPI && !c.DeveloperAPI {
		return false
	}
	l := c.SimulateLimits()
	return uint64(n.Groups) <= l.MaxTxnGroups && uint64(n.Rounds) <= l.MaxRounds && n.ExtraOpcodeBudget <= l.MaxExtraOpcodeBudget
}

// SimulateBody is the part of a simulate request body routing reads. The
// tags are algod's for both msgpack and JSON; every other field is skipped.
type SimulateBody struct {
	TxnGroups         []struct{} `codec:"txn-groups" json:"txn-groups"`
	Round             uint64     `codec:"round" json:"round"`
	Rounds            []uint64   `codec:"rounds" json:"rounds"`
	ExtraOpcodeBudget int64      `codec:"extra-opcode-budget" json:"extra-opcode-budget"`
	ExecTraceConfig   struct {
		Enable bool `codec:"enable" json:"enable"`
	} `codec:"exec-trace-config" json:"exec-trace-config"`
	ParallelGroups        bool   `codec:"parallel-groups" json:"parallel-groups"`
	SkipValidityWindow    bool   `codec:"skip-validity-window" json:"skip-validity-window"`
	RewriteValidityWindow bool   `codec:"rewrite-validity-window" json:"rewrite-validity-window"`
	Protocol              string `codec:"protocol" json:"protocol"`
}

// protocolFuture is the protocol name only a node with the developer API
// simulates under (protocol.ConsensusFuture).
const protocolFuture = "future"

// ApplySimulate refines a simulate request's class with what its body asks.
// A round other than 0 (which means the latest) needs a node holding it:
// a plain node within its account-state lookback, a time-travel node at any
// round. A rounds list needs a node holding every round it names other than
// 0, so its earliest and latest bound the node's window.
func (c *RequestClass) ApplySimulate(b SimulateBody) {
	n := SimulateNeeds{Groups: len(b.TxnGroups), Rounds: len(b.Rounds),
		Fork:         b.ParallelGroups || b.SkipValidityWindow || b.RewriteValidityWindow || b.Protocol != "",
		DeveloperAPI: b.ExecTraceConfig.Enable || b.Protocol == protocolFuture}
	if b.ExtraOpcodeBudget > 0 {
		n.ExtraOpcodeBudget = uint64(b.ExtraOpcodeBudget)
	}
	setRound := func(r uint64) {
		c.Round = &r
		c.RoundWindow = WindowState
		c.HistoryAnyRound = true
	}
	switch {
	case b.Round != 0:
		// With rounds as well the node rejects the request; routing it by
		// round gets that answer from a node that would otherwise serve it.
		setRound(b.Round)
	case len(b.Rounds) > 0:
		var lo, hi uint64
		for _, r := range b.Rounds {
			if r == 0 {
				n.Latest = true // the node's own latest round
				continue
			}
			if lo == 0 || r < lo {
				lo = r
			}
			hi = max(hi, r)
		}
		if hi > 0 {
			setRound(lo)
			c.RoundHigh = hi
			n.RoundsSpan = true
		}
	}
	c.SimNeeds = &n
}

// Relaxed lists the classes selection falls back to, in order, when no
// upstream is eligible for c. A simulate request with a rounds list first
// drops the rounds, which the node reports one by one as it cannot simulate
// them, then the rest of its needs, so that a node rejects the request with
// its own message rather than the router with a 503. A single round is kept:
// a node that lacks it would answer the latest state or an error.
func (c RequestClass) Relaxed() []RequestClass {
	if c.SimNeeds == nil {
		return nil
	}
	var out []RequestClass
	if c.SimNeeds.RoundsSpan {
		c.Round, c.RoundHigh, c.RoundWindow, c.HistoryAnyRound = nil, 0, WindowBlock, false
		out = append(out, c)
	}
	c.SimNeeds = nil
	return append(out, c)
}

// Reply is a status and message the router answers with in algod's place.
type Reply struct {
	Status  int
	Message string
}

// String renders the class for logs.
func (c RequestClass) String() string {
	var parts []string
	if c.Round != nil {
		parts = append(parts, fmt.Sprintf("round=%d", *c.Round))
	}
	if c.RoundHigh != 0 {
		parts = append(parts, fmt.Sprintf("round_high=%d", c.RoundHigh))
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
	if n := c.SimNeeds; n != nil {
		if n.Groups > 1 {
			parts = append(parts, fmt.Sprintf("sim_groups=%d", n.Groups))
		}
		if n.Rounds > 0 {
			parts = append(parts, fmt.Sprintf("sim_rounds=%d", n.Rounds))
		}
		if n.ExtraOpcodeBudget > StockMaxExtraOpcodeBudget {
			parts = append(parts, fmt.Sprintf("sim_budget=%d", n.ExtraOpcodeBudget))
		}
		if n.Fork {
			parts = append(parts, "sim_fork")
		}
		if n.DeveloperAPI {
			parts = append(parts, "sim_dev")
		}
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
	case c.Simulate:
		// One simulation can run for seconds (many groups, many rounds):
		// it must not skew the latency of the requests it would share with.
		return "simulate"
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
	case "blocks", "deltas", "stateproofs":
		if r, ok := parseRound(seg(2)); ok {
			c.Round = &r
			const ledger = "failed to retrieve information from the ledger"
			sub := strings.Join(segs[3:], "/")
			switch {
			case method != "GET":
				// algod has no such route; Ahead stays unset.
			case seg(1) == "stateproofs" && sub == "", seg(1) == "blocks" && sub == "lightheader/proof":
				c.Ahead = Reply{500, "given round is greater than the latest round"}
			case seg(1) == "blocks" && len(segs) == 6 && seg(3) == "transactions" && seg(5) == "proof":
				c.Ahead = Reply{500, ledger}
			case seg(1) == "blocks" && (sub == "" || sub == "hash" || sub == "txids" || sub == "logs"),
				seg(1) == "deltas" && sub == "":
				c.Ahead = Reply{404, ledger}
			}
			if seg(1) == "deltas" && sub == "" {
				// A plain node serves a delta only from its account-state
				// lookback; a time-travel node re-evaluates one whose block
				// is still stored, so it stays on the block window.
				c.RoundWindow = WindowDelta
			}
		}
		if seg(1) == "blocks" && q.Get("header-only") == "true" {
			// A time-travel node answers a header from its history at any
			// round; a plain node needs the stored block (WindowBlock).
			c.HistoryAnyRound = true
		}
		if seg(1) == "deltas" && seg(2) == "txn" {
			// /v2/deltas/txn/group/{id}: round unknown, any synced node.
			c.Round = nil
		}
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
		case seg(2) == "simulate" && seg(3) == "limits":
			// dtalgorand's limits: a GET, answered for the fleet by the router.
			c.SimulateLimits = method == "GET"
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
