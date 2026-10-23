// Package domain holds the pure core of algod-loadb-mesh: value types and the
// functions that decide where a request goes. Nothing in this package does
// I/O, reads the clock or touches a socket; time and randomness are always
// passed in. Every function here is unit-testable with plain values.
package domain

import "fmt"

// ArchivalKind describes how a node's oldest servable round evolves.
type ArchivalKind string

const (
	ArchivalFull     ArchivalKind = "full"     // oldest_round is 0 forever
	ArchivalTrailing ArchivalKind = "trailing" // oldest_round = last_round - N
	ArchivalSince    ArchivalKind = "since"    // oldest_round fixed at N
	ArchivalNone     ArchivalKind = "none"     // only the recent window algod keeps anyway
)

// Archival is the (kind, parameter) pair from which OldestRound is derived.
type Archival struct {
	Kind ArchivalKind `json:"kind" yaml:"kind"`
	N    uint64       `json:"n,omitempty" yaml:"n,omitempty"`
}

// DefaultNonArchivalWindow is the number of recent rounds a non-archival algod
// keeps (MaxBalLookback-ish). v1 hard-coded 998; algod keeps 1000.
const DefaultNonArchivalWindow = 1000

// OldestRoundFor computes the earliest servable round for an archival mode at
// a given last round.
func OldestRoundFor(a Archival, lastRound uint64) uint64 {
	switch a.Kind {
	case ArchivalFull:
		return 0
	case ArchivalTrailing:
		if lastRound <= a.N {
			return 0
		}
		return lastRound - a.N
	case ArchivalSince:
		return a.N
	default:
		if lastRound <= DefaultNonArchivalWindow {
			return 0
		}
		return lastRound - DefaultNonArchivalWindow
	}
}

// Capabilities is what a node can do right now. It is produced by the local
// monitor and travels inside heartbeats.
type Capabilities struct {
	OldestRound     uint64   `json:"oldest_round"`
	Archival        Archival `json:"archival"`
	DeveloperAPI    bool     `json:"developer_api"`
	FollowMode      bool     `json:"follow_mode"`
	ExperimentalAPI bool     `json:"experimental_api"`
	// TimeTravel is a node that keeps every ledger object's value at every
	// round (the EnableTimeTravel fork), so it answers ?round= on state
	// endpoints and the /versions history endpoints at any past round, even
	// when Archival is off.
	TimeTravel bool `json:"time_travel,omitempty"`
	// Simulate is what the node allows in a simulate request, as dtalgorand
	// reports it at GET /v2/transactions/simulate/limits. nil is a stock
	// algod (or an agent too old to ask): one transaction group, no rounds
	// list, the upstream opcode budget limit and none of the fork's options.
	Simulate        *SimulateLimits `json:"simulate,omitempty"`
	AlgodVersion    string          `json:"algod_version,omitempty"`
	GenesisID       string          `json:"genesis_id,omitempty"`
	StorageEngine   string          `json:"storage_engine,omitempty"`
	MaxAcctLookback uint64          `json:"max_acct_lookback,omitempty"`
}

// CapabilityOverrides are manual corrections from config or the registry.
// A nil pointer means "no opinion".
type CapabilityOverrides struct {
	Archival     *Archival `json:"archival,omitempty" yaml:"archival,omitempty"`
	DeveloperAPI *bool     `json:"developer_api,omitempty" yaml:"developer_api,omitempty"`
	FollowMode   *bool     `json:"follow_mode,omitempty" yaml:"follow_mode,omitempty"`
	TimeTravel   *bool     `json:"time_travel,omitempty" yaml:"time_travel,omitempty"`
	// Simulate pins the simulate limits, for an upstream the agent cannot
	// ask (an external) or one that answers wrongly.
	Simulate *SimulateLimits `json:"simulate,omitempty" yaml:"simulate,omitempty"`
}

// Apply returns caps with the overrides applied, recomputing OldestRound.
func (o *CapabilityOverrides) Apply(c Capabilities, lastRound uint64) Capabilities {
	if o == nil {
		return c
	}
	if o.Archival != nil {
		c.Archival = *o.Archival
		c.OldestRound = OldestRoundFor(c.Archival, lastRound)
	}
	if o.DeveloperAPI != nil {
		c.DeveloperAPI = *o.DeveloperAPI
	}
	if o.FollowMode != nil {
		c.FollowMode = *o.FollowMode
	}
	if o.TimeTravel != nil {
		c.TimeTravel = *o.TimeTravel
	}
	if o.Simulate != nil {
		lim := *o.Simulate
		c.Simulate = &lim
	}
	return c
}

// SimulateLimits is what a dtalgorand node allows in one simulate request.
// The fields mirror GET /v2/transactions/simulate/limits; that response's
// time-travel and developer-api are already Capabilities of their own.
type SimulateLimits struct {
	MaxTxnGroups            uint64 `json:"max_txn_groups" yaml:"max_txn_groups"`
	MaxRounds               uint64 `json:"max_rounds" yaml:"max_rounds"` // 0: rounds is disabled
	MaxExtraOpcodeBudget    uint64 `json:"max_extra_opcode_budget" yaml:"max_extra_opcode_budget"`
	ResourceLimitMultiplier uint64 `json:"resource_limit_multiplier" yaml:"resource_limit_multiplier"`
	// MaxExecTraceBytes caps one request's exec trace (400 beyond), and
	// TimeLimitSeconds its run time (504, or per-round errors for a rounds
	// list); 0 is no limit, or a dtalgorand from before they existed.
	MaxExecTraceBytes uint64 `json:"max_exec_trace_bytes,omitempty" yaml:"max_exec_trace_bytes,omitempty"`
	TimeLimitSeconds  uint64 `json:"time_limit_seconds,omitempty" yaml:"time_limit_seconds,omitempty"`
}

// StockMaxExtraOpcodeBudget is the extra opcode budget limit of stock algod
// (simulation.MaxExtraOpcodeBudget).
const StockMaxExtraOpcodeBudget = 320000

// StockSimulateLimits is what a stock algod allows.
var StockSimulateLimits = SimulateLimits{MaxTxnGroups: 1, MaxExtraOpcodeBudget: StockMaxExtraOpcodeBudget, ResourceLimitMultiplier: 1}

// The ceilings dtalgorand clamps its simulate config to (simulation.
// MaxSimulateGroups, MaxSimulateRounds, MaxExtraOpcodeBudgetCeiling and
// MaxResourceLimitMultiplier).
const (
	maxSimulateGroups          = 256
	maxSimulateRounds          = 256
	maxExtraOpcodeBudget       = 1000 * StockMaxExtraOpcodeBudget
	maxResourceLimitMultiplier = 1024
)

// SimulateLimits is what the node allows in a simulate request, with the
// stock limits for a node that reports none. Values are read as dtalgorand
// reads its config: fewer than one group counts as one, a zero budget as the
// stock one, a multiplier below one as one, and each is clamped to the
// fork's ceiling, so that a hand-written override promises neither less nor
// more than a node can do.
func (c Capabilities) SimulateLimits() SimulateLimits {
	if c.Simulate == nil {
		return StockSimulateLimits
	}
	l := *c.Simulate
	l.MaxTxnGroups = min(max(l.MaxTxnGroups, 1), maxSimulateGroups)
	l.MaxRounds = min(l.MaxRounds, maxSimulateRounds)
	if l.MaxExtraOpcodeBudget == 0 {
		l.MaxExtraOpcodeBudget = StockMaxExtraOpcodeBudget
	}
	l.MaxExtraOpcodeBudget = min(l.MaxExtraOpcodeBudget, maxExtraOpcodeBudget)
	l.ResourceLimitMultiplier = min(max(l.ResourceLimitMultiplier, 1), maxResourceLimitMultiplier)
	return l
}

// AgentInfo is how other agents reach this node's agent on the control plane.
type AgentInfo struct {
	Addrs  []string `json:"addrs,omitempty"` // gossip addresses, one per mesh interface, preferred first
	Addr   string   `json:"addr,omitempty"`  // pre-multipath single address: read, never written
	PubKey []byte   `json:"pubkey"`          // ed25519 public key that signs heartbeats
}

// GossipAddrs is every address peers may send to: Addrs, then the legacy
// Addr when it is not already listed. Every consumer goes through it.
func (a AgentInfo) GossipAddrs() []string {
	out := append([]string(nil), a.Addrs...)
	if a.Addr != "" {
		for _, s := range out {
			if s == a.Addr {
				return out
			}
		}
		out = append(out, a.Addr)
	}
	return out
}

// Role is what an agent in the registry is.
type Role string

const (
	// RoleNode is an agent next to an algod; the empty string means the same,
	// so records written before roles existed are nodes.
	RoleNode Role = "node"
	// RoleBalancer is an agent with no algod of its own. It serves clients
	// from the fleet and receives heartbeats, but is never an upstream.
	RoleBalancer Role = "balancer"
)

// ParseRole validates a role string; empty is RoleNode.
func ParseRole(s string) (Role, error) {
	switch Role(s) {
	case "", RoleNode:
		return RoleNode, nil
	case RoleBalancer:
		return RoleBalancer, nil
	}
	return "", fmt.Errorf("unknown role %q (want node or balancer)", s)
}

// NodeRecord is the static description of an agent and, for nodes, its
// algod. It lives encrypted in the registry and changes rarely.
type NodeRecord struct {
	ID        string               `json:"id"`
	Role      Role                 `json:"role,omitempty"` // empty: node
	Network   string               `json:"network"`        // genesis id, e.g. mainnet-v1.0
	Endpoints []string             `json:"endpoints"`
	Token     string               `json:"token"`
	Agent     AgentInfo            `json:"agent"`
	Tier      int                  `json:"tier"`
	Tags      []string             `json:"tags,omitempty"`
	Declared  *CapabilityOverrides `json:"declared,omitempty"`
	Version   int                  `json:"version"`
	UpdatedAt uint64               `json:"updated_at"` // round when written
}

// Validate checks the invariants every record must satisfy before it is
// used for routing or written to the registry.
func (r NodeRecord) Validate() error {
	if r.ID == "" {
		return fmt.Errorf("node record: empty id")
	}
	if len(r.ID) > 48 {
		return fmt.Errorf("node record %q: id longer than 48 bytes", r.ID)
	}
	if r.Network == "" {
		return fmt.Errorf("node record %q: empty network", r.ID)
	}
	role, err := ParseRole(string(r.Role))
	if err != nil {
		return fmt.Errorf("node record %q: %w", r.ID, err)
	}
	switch {
	case role == RoleNode && len(r.Endpoints) == 0:
		return fmt.Errorf("node record %q: no endpoints", r.ID)
	case role == RoleBalancer && len(r.Agent.GossipAddrs()) == 0:
		return fmt.Errorf("balancer record %q: no agent address to send heartbeats to", r.ID)
	}
	if r.Tier < 0 {
		return fmt.Errorf("node record %q: negative tier", r.ID)
	}
	return nil
}

// IsBalancer reports whether the record is a balancer, which has no algod.
func (r NodeRecord) IsBalancer() bool { return r.Role == RoleBalancer }

// StaticEqual reports whether two records describe the same static facts,
// ignoring Version and UpdatedAt. Used to decide whether a write is needed.
func (r NodeRecord) StaticEqual(o NodeRecord) bool {
	if r.ID != o.ID || r.IsBalancer() != o.IsBalancer() || r.Network != o.Network || r.Token != o.Token || r.Tier != o.Tier {
		return false
	}
	// Order-sensitive like Endpoints: an upgraded node must republish when it
	// gains a second address.
	if !equalStrings(r.Agent.GossipAddrs(), o.Agent.GossipAddrs()) || string(r.Agent.PubKey) != string(o.Agent.PubKey) {
		return false
	}
	if !equalStrings(r.Endpoints, o.Endpoints) || !equalStrings(r.Tags, o.Tags) {
		return false
	}
	return equalOverrides(r.Declared, o.Declared)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalOverrides(a, b *CapabilityOverrides) bool {
	if a == nil || b == nil {
		return a == b
	}
	eqA := (a.Archival == nil && b.Archival == nil) || (a.Archival != nil && b.Archival != nil && *a.Archival == *b.Archival)
	eqD := (a.DeveloperAPI == nil && b.DeveloperAPI == nil) || (a.DeveloperAPI != nil && b.DeveloperAPI != nil && *a.DeveloperAPI == *b.DeveloperAPI)
	eqF := (a.FollowMode == nil && b.FollowMode == nil) || (a.FollowMode != nil && b.FollowMode != nil && *a.FollowMode == *b.FollowMode)
	eqT := (a.TimeTravel == nil && b.TimeTravel == nil) || (a.TimeTravel != nil && b.TimeTravel != nil && *a.TimeTravel == *b.TimeTravel)
	eqS := (a.Simulate == nil && b.Simulate == nil) || (a.Simulate != nil && b.Simulate != nil && *a.Simulate == *b.Simulate)
	return eqA && eqD && eqF && eqT && eqS
}

// Health is the liveness/sync state of an upstream as seen by an agent.
type Health int

const (
	HealthStarting Health = iota // nothing known yet
	HealthOffline                // unreachable or its agent went silent
	HealthOnline                 // reachable, sync state not (yet) judged
	HealthLagging                // reachable but behind the best known round
	HealthSynced                 // reachable and at the best known round
)

func (h Health) String() string {
	switch h {
	case HealthStarting:
		return "starting"
	case HealthOffline:
		return "offline"
	case HealthOnline:
		return "online"
	case HealthLagging:
		return "lagging"
	case HealthSynced:
		return "synced"
	}
	return fmt.Sprintf("health(%d)", int(h))
}

// MarshalText makes Health readable in JSON status output.
func (h Health) MarshalText() ([]byte, error) { return []byte(h.String()), nil }

// Reachable is true when requests may be sent to the upstream at all.
func (h Health) Reachable() bool {
	return h == HealthOnline || h == HealthLagging || h == HealthSynced
}

// Heartbeat is the per-round message an agent gossips about its own node.
type Heartbeat struct {
	NodeID    string       `json:"id"`
	Seq       uint64       `json:"seq"`
	LastRound uint64       `json:"round"`
	Online    bool         `json:"online"` // the agent can reach its algod
	Caps      Capabilities `json:"caps"`
	Draining  bool         `json:"draining,omitempty"`
}

// UpstreamKind distinguishes the three tiers of candidates.
type UpstreamKind int

const (
	KindLocal UpstreamKind = iota
	KindPeer
	KindExternal
)

func (k UpstreamKind) String() string {
	switch k {
	case KindLocal:
		return "local"
	case KindPeer:
		return "peer"
	case KindExternal:
		return "external"
	}
	return "unknown"
}

func (k UpstreamKind) MarshalText() ([]byte, error) { return []byte(k.String()), nil }

// Stats is the router's passive observation of an upstream, snapshotted for
// selection. Latency is a map from StatKey to an EWMA in milliseconds.
type Stats struct {
	LatencyMS   map[string]float64 `json:"latency_ms,omitempty"`
	ErrorRate   float64            `json:"error_rate"` // 0..1 over a recent window
	Inflight    int                `json:"inflight"`
	BreakerOpen bool               `json:"breaker_open"`
	Throttled   bool               `json:"throttled"`
	// Busy is a node that answered a heavy request (Heavy) with 503 and
	// Retry-After, its slots for them full: no heavy request goes to it
	// until then, while the rest still do.
	Busy bool `json:"busy,omitempty"`
}

// Upstream is one candidate the router chooses between. It is a value
// snapshot taken at request time; the router never mutates it.
type Upstream struct {
	ID        string       `json:"id"`
	Kind      UpstreamKind `json:"kind"`
	Tier      int          `json:"tier"`
	BaseURL   string       `json:"base_url"`
	Token     string       `json:"-"`
	Health    Health       `json:"health"`
	LastRound uint64       `json:"last_round"`
	Caps      Capabilities `json:"caps"`
	Draining  bool         `json:"draining,omitempty"`
	// Excluded is set by routing.exclude: the upstream is shown, but no
	// request is routed to it and its round does not count toward the
	// fleet's best.
	Excluded bool  `json:"excluded,omitempty"`
	Stats    Stats `json:"stats"`
	// Source says where the live facts come from: local, heartbeat, probe
	// (agent silent, node answered directly), check (external health check)
	// or none.
	Source string `json:"source,omitempty"`
	// Score is filled by the selector for status output only.
	Score float64 `json:"score,omitempty"`
}

// Mode selects the routing policy.
type Mode string

const (
	ModeFallback     Mode = "fallback"
	ModeLoadBalancer Mode = "loadbalancer"
)

// ParseMode validates a mode string.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeFallback, ModeLoadBalancer:
		return Mode(s), nil
	case "":
		return ModeFallback, nil
	}
	return "", fmt.Errorf("unknown mode %q (want fallback or loadbalancer)", s)
}

// UnmarshalText parses the String form, so status JSON round-trips.
func (h *Health) UnmarshalText(b []byte) error {
	for _, c := range []Health{HealthStarting, HealthOffline, HealthOnline, HealthLagging, HealthSynced} {
		if c.String() == string(b) {
			*h = c
			return nil
		}
	}
	return fmt.Errorf("unknown health %q", b)
}

// UnmarshalText parses the String form.
func (k *UpstreamKind) UnmarshalText(b []byte) error {
	for _, c := range []UpstreamKind{KindLocal, KindPeer, KindExternal} {
		if c.String() == string(b) {
			*k = c
			return nil
		}
	}
	return fmt.Errorf("unknown upstream kind %q", b)
}
