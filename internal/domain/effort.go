package domain

import (
	"strconv"
	"strings"
)

// EffortHeader is the work dtalgorand reports doing for one request (config
// EnableEffortHeader): an RFC 9651 dictionary of integers. It is sent on
// every answer written after the work began, and never on a busy 503, which
// is answered before any work.
const EffortHeader = "X-Algod-Effort"

// Effort is what a node reports in EffortHeader. A rounds simulation reports
// all of its rounds together.
type Effort struct {
	Opcodes    uint64 // app and logicsig budget consumed
	Lookups    uint64 // ledger object reads the ledger served
	History    uint64 // the lookups that fell through to the time-travel store
	Scanned    uint64 // entries visited by scans: history pages, ?round= lists
	TraceBytes uint64 // the exec trace's estimated size
	EvalUS     uint64 // wall time of the work, in microseconds; diagnostics only
}

// ParseEffort reads an EffortHeader value, the node's field lines joined
// with commas. Unknown keys and parameters are ignored; eval-us may be left
// out. The billed keys must each appear once with a non-negative integer:
// a header missing one is not an effort, so that a broken node is billed by
// time rather than for nothing. Members are split on commas, which would
// misread a string or byte sequence holding one; the node sends only
// integers, and an unknown member that is cut in two is still ignored.
func ParseEffort(v string) (Effort, bool) {
	var e Effort
	seen := map[string]bool{}
	for _, member := range strings.Split(v, ",") {
		member, _, _ = strings.Cut(member, ";") // parameters
		key, val, ok := strings.Cut(strings.TrimSpace(member), "=")
		f := e.field(key)
		if f == nil {
			continue
		}
		if !ok || seen[key] {
			return Effort{}, false // a boolean, or a key twice: RFC 9651 makes the latter invalid
		}
		seen[key] = true
		n, err := strconv.ParseUint(val, 10, 64)
		if err != nil {
			return Effort{}, false
		}
		*f = n
	}
	for _, k := range billedEffortKeys {
		if !seen[k] {
			return Effort{}, false
		}
	}
	return e, true
}

// billedEffortKeys are the keys a node always sends that cost credits.
var billedEffortKeys = []string{"opcodes", "lookups", "history", "scanned", "trace-bytes"}

func (e *Effort) field(key string) *uint64 {
	switch key {
	case "opcodes":
		return &e.Opcodes
	case "lookups":
		return &e.Lookups
	case "history":
		return &e.History
	case "scanned":
		return &e.Scanned
	case "trace-bytes":
		return &e.TraceBytes
	case "eval-us":
		return &e.EvalUS
	}
	return nil
}

// Add sums two efforts: one request's attempts on several nodes.
func (e Effort) Add(o Effort) Effort {
	return Effort{e.Opcodes + o.Opcodes, e.Lookups + o.Lookups, e.History + o.History,
		e.Scanned + o.Scanned, e.TraceBytes + o.TraceBytes, e.EvalUS + o.EvalUS}
}

// EffortRates converts reported work into credits: so many units of each
// kind cost one credit. A rate of 0 or less makes that kind free. History
// lookups are counted among the lookups as well, so HistoryPerCredit prices
// what a store read costs on top of a plain one.
type EffortRates struct {
	OpcodesPerCredit float64
	LookupsPerCredit float64
	HistoryPerCredit float64
	ScannedPerCredit float64
	TraceKBPerCredit float64
}

// DefaultEffortRates are placeholders, set so that a busy simulation costs
// about what the wall-time meter's default of 20 credits a second bills
// (one credit is ~50 ms of a node's time): ~150 ns per budget unit, a few
// µs per cached lookup or scanned entry, ~50 µs per store read, and a MiB of
// trace per credit.
var DefaultEffortRates = EffortRates{OpcodesPerCredit: 330000, LookupsPerCredit: 10000,
	HistoryPerCredit: 1000, ScannedPerCredit: 10000, TraceKBPerCredit: 1024}

// Credits is what effort e costs.
func (r EffortRates) Credits(e Effort) float64 {
	per := func(n uint64, rate float64) float64 {
		if rate <= 0 {
			return 0
		}
		return float64(n) / rate
	}
	return per(e.Opcodes, r.OpcodesPerCredit) + per(e.Lookups, r.LookupsPerCredit) +
		per(e.History, r.HistoryPerCredit) + per(e.Scanned, r.ScannedPerCredit) +
		per(e.TraceBytes, r.TraceKBPerCredit*1024)
}
