// Package config loads and validates the agent's YAML configuration.
package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/mnemonic"
	"github.com/algorand/go-algorand-sdk/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/d13co/algod-mesh/internal/domain"
)

// Config is the whole file.
type Config struct {
	Role            string    `yaml:"role"` // node | balancer
	Mode            string    `yaml:"mode"`
	Listen          Addrs     `yaml:"listen"`
	ClientToken     string    `yaml:"client_token"`
	ClientTokenFile string    `yaml:"client_token_file"`
	AdminToken      string    `yaml:"admin_token"`      // required for /mesh/*; also accepted as a client token
	AdminTokenFile  string    `yaml:"admin_token_file"` // default: algod.admin.token in local.data_dir
	Log             Log       `yaml:"log"`
	Local           Local     `yaml:"local"`
	Registry        Reg       `yaml:"registry"`
	Mesh            Mesh      `yaml:"mesh"`
	Tiers           Tiers     `yaml:"tiers"`
	Routing         Routing   `yaml:"routing"`
	RateLimit       RateLimit `yaml:"ratelimit"`
}

// RateLimit limits each client in credits, per minute and per day. A request
// costs its kind's weight; a simulation its weight per group and round, plus
// simulate_per_second while it runs, and it is cancelled once the client's
// allowance runs out. When the node that answers reports the work it did
// (dtalgorand's EnableEffortHeader), the request costs its weight plus that
// work at the effort rates instead of its running time. A client without a
// token is limited by address under the anonymous plan; a listed token by
// its plan; any other token gets 401.
type RateLimit struct {
	Enabled        bool     `yaml:"enabled"`
	StateFile      string   `yaml:"state_file"`      // the allowances survive restarts here; default: memory only
	TrustedProxies []string `yaml:"trusted_proxies"` // CIDRs or addresses whose X-Forwarded-For names the client
	IPv6Prefix     int      `yaml:"ipv6_prefix"`     // default 64
	MaxKeys        int      `yaml:"max_keys"`        // default 200000
	// AnonymousPlan is the plan of requests without a token; "" refuses
	// them. Default: free, when there is such a plan.
	AnonymousPlan     *string             `yaml:"anonymous_plan"`
	SimulatePerSecond *float64            `yaml:"simulate_per_second"` // default 20
	Weights           map[string]float64  `yaml:"weights"`             // by cost key; unset keys keep their defaults
	Effort            map[string]float64  `yaml:"effort"`              // units of reported work per credit, by EffortKeys; unset keys keep their defaults, 0 makes that work free
	Plans             map[string]RatePlan `yaml:"plans"`
	Tokens            []RateToken         `yaml:"tokens"`
}

// EffortKeys are the keys of ratelimit.effort.
var EffortKeys = []string{"opcodes_per_credit", "lookups_per_credit", "history_per_credit", "scanned_per_credit", "trace_kb_per_credit"}

// EffortRates is rates as a ratelimit.effort map.
func EffortRates(r domain.EffortRates) map[string]float64 {
	return map[string]float64{"opcodes_per_credit": r.OpcodesPerCredit, "lookups_per_credit": r.LookupsPerCredit,
		"history_per_credit": r.HistoryPerCredit, "scanned_per_credit": r.ScannedPerCredit, "trace_kb_per_credit": r.TraceKBPerCredit}
}

// EffortRatesOf is the domain rates of a finished ratelimit.effort map.
func (r RateLimit) EffortRatesOf() domain.EffortRates {
	e := r.Effort
	return domain.EffortRates{OpcodesPerCredit: e["opcodes_per_credit"], LookupsPerCredit: e["lookups_per_credit"],
		HistoryPerCredit: e["history_per_credit"], ScannedPerCredit: e["scanned_per_credit"], TraceKBPerCredit: e["trace_kb_per_credit"]}
}

// RatePlan is one plan's allowance: credits per minute and per day, 0 for
// no limit, and what one simulate request may ask for.
type RatePlan struct {
	PerMinute float64      `yaml:"per_minute"`
	PerDay    float64      `yaml:"per_day"`
	Simulate  SimulateCaps `yaml:"simulate"`
}

// SimulateCaps bounds one simulate request; 0 is no cap, and the options are
// allowed unless set false.
type SimulateCaps struct {
	MaxGroups            int    `yaml:"max_groups"`
	MaxRounds            int    `yaml:"max_rounds"`
	MaxExtraOpcodeBudget uint64 `yaml:"max_extra_opcode_budget"`
	UnnamedResources     *bool  `yaml:"unnamed_resources"`
	ExecTrace            *bool  `yaml:"exec_trace"`
}

// RateToken is one API token clients may send, and its plan.
type RateToken struct {
	Name      string `yaml:"name"` // shown in logs, metrics and the state file
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
	Plan      string `yaml:"plan"`
}

// TrustedPrefixes parses trusted_proxies; Finish has validated them.
func (r RateLimit) TrustedPrefixes() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range r.TrustedProxies {
		if p, err := parsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// parsePrefix reads a CIDR, or one address as a prefix of its own.
func parsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap()
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// finish applies the defaults and validates; adminToken is resolved.
func (r *RateLimit) finish(clientToken, adminToken string) error {
	if !r.Enabled {
		return nil
	}
	if clientToken != "" {
		return errors.New("ratelimit: client_token and client_token_file must be unset: list the tokens in ratelimit.tokens, and clients without one get ratelimit.anonymous_plan")
	}
	if len(r.Plans) == 0 {
		return errors.New("ratelimit.plans: at least one plan is required")
	}
	for name, p := range r.Plans {
		switch {
		case name == "":
			return errors.New("ratelimit.plans: a plan needs a name")
		case !nonNegative(p.PerMinute) || !nonNegative(p.PerDay):
			return fmt.Errorf("ratelimit.plans.%s: per_minute and per_day must not be negative (0 is unlimited)", name)
		case p.Simulate.MaxGroups < 0 || p.Simulate.MaxRounds < 0:
			return fmt.Errorf("ratelimit.plans.%s.simulate: caps must not be negative", name)
		}
	}
	weights := map[string]float64{}
	for k, v := range domain.DefaultCostWeights {
		weights[k] = v
	}
	for k, v := range r.Weights {
		if _, ok := weights[k]; !ok {
			return fmt.Errorf("ratelimit.weights.%s: unknown; want one of %s", k, strings.Join(domain.CostKeys, ", "))
		}
		if !nonNegative(v) {
			return fmt.Errorf("ratelimit.weights.%s: want a number, 0 or more", k)
		}
		weights[k] = v
	}
	r.Weights = weights
	effort := EffortRates(domain.DefaultEffortRates)
	for k, v := range r.Effort {
		if _, ok := effort[k]; !ok {
			return fmt.Errorf("ratelimit.effort.%s: unknown; want one of %s", k, strings.Join(EffortKeys, ", "))
		}
		if !nonNegative(v) {
			return fmt.Errorf("ratelimit.effort.%s: want a number, 0 or more", k)
		}
		effort[k] = v
	}
	r.Effort = effort
	if r.SimulatePerSecond == nil {
		v := 20.0
		r.SimulatePerSecond = &v
	}
	if !nonNegative(*r.SimulatePerSecond) {
		return errors.New("ratelimit.simulate_per_second must not be negative")
	}
	if r.IPv6Prefix == 0 {
		r.IPv6Prefix = 64
	}
	if r.IPv6Prefix < 1 || r.IPv6Prefix > 128 {
		return errors.New("ratelimit.ipv6_prefix: want 1 to 128")
	}
	if r.MaxKeys < 0 {
		return errors.New("ratelimit.max_keys must not be negative")
	}
	if r.MaxKeys == 0 {
		r.MaxKeys = 200000
	}
	if r.AnonymousPlan == nil {
		v := ""
		if _, ok := r.Plans["free"]; ok {
			v = "free"
		}
		r.AnonymousPlan = &v
	}
	if a := *r.AnonymousPlan; a != "" {
		if _, ok := r.Plans[a]; !ok {
			return fmt.Errorf("ratelimit.anonymous_plan %q: no such plan", a)
		}
	}
	for _, s := range r.TrustedProxies {
		if _, err := parsePrefix(s); err != nil {
			return fmt.Errorf("ratelimit.trusted_proxies %q: %w", s, err)
		}
	}
	names, values := map[string]bool{}, map[string]string{}
	for i := range r.Tokens {
		t := &r.Tokens[i]
		switch {
		case t.Name == "":
			return fmt.Errorf("ratelimit.tokens[%d]: name is required", i)
		case names[t.Name]:
			return fmt.Errorf("ratelimit.tokens[%d]: name %q is used twice", i, t.Name)
		case t.Token == "" && t.TokenFile == "":
			return fmt.Errorf("ratelimit.tokens %s: set one of token and token_file", t.Name)
		}
		names[t.Name] = true
		if _, ok := r.Plans[t.Plan]; !ok {
			return fmt.Errorf("ratelimit.tokens %s: plan %q: no such plan", t.Name, t.Plan)
		}
		if t.TokenFile != "" {
			v, err := readSecret(t.TokenFile)
			if err != nil {
				return fmt.Errorf("ratelimit.tokens %s: token_file: %w", t.Name, err)
			}
			// Finish runs more than once on one config: a token read
			// before is the file's, anything else is set twice.
			if t.Token != "" && t.Token != v {
				return fmt.Errorf("ratelimit.tokens %s: set one of token and token_file", t.Name)
			}
			t.Token = v
		}
		switch {
		case t.Token == "":
			return fmt.Errorf("ratelimit.tokens %s: the token is empty", t.Name)
		case t.Token == adminToken:
			return fmt.Errorf("ratelimit.tokens %s: the token is the admin token", t.Name)
		case values[t.Token] != "":
			return fmt.Errorf("ratelimit.tokens %s: the same token as %s", t.Name, values[t.Token])
		}
		values[t.Token] = t.Name
	}
	return nil
}

// Addrs is a list of addresses to listen on. YAML accepts one address, a
// comma-separated string, or a sequence:
//
//	listen: 10.112.0.44:4000
//	listen: 10.112.0.44:4000, 10.114.0.44:4000
//	listen:
//	  - 10.112.0.44:4000
//	  - 10.114.0.44:4000
type Addrs []string

// UnmarshalYAML accepts a scalar or a sequence.
func (a *Addrs) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var s string
		if err := n.Decode(&s); err != nil {
			return err
		}
		*a = strings.Split(s, ",")
		return nil
	}
	var ss []string
	if err := n.Decode(&ss); err != nil {
		return err
	}
	*a = ss
	return nil
}

func (a Addrs) String() string { return strings.Join(a, ", ") }

// resolve trims, defaults and deduplicates the list, and rejects an address
// that could not be bound: one without a port, or one on a port a wildcard
// address already covers (the second bind would fail at startup).
func (a Addrs) resolve(what, def string) (Addrs, error) {
	out := make(Addrs, 0, len(a))
	seen := map[string]bool{}
	wild := map[string]string{} // port -> the wildcard address on it
	for _, s := range a {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w (want host:port, [::1]:port for IPv6)", what, s, err)
		}
		if port == "" {
			return nil, fmt.Errorf("%s %q: no port", what, s)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		if wildcardHost(host) {
			if prev, ok := wild[port]; ok {
				return nil, fmt.Errorf("%s %s: %s already listens on port %s of every interface", what, s, prev, port)
			}
			wild[port] = s
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return Addrs{def}, nil
	}
	for _, s := range out {
		host, port, _ := net.SplitHostPort(s)
		if prev, ok := wild[port]; ok && !wildcardHost(host) && wildcardCovers(prev, host) {
			return nil, fmt.Errorf("%s %s: %s already listens on port %s of every interface", what, s, prev, port)
		}
	}
	return out, nil
}

// wildcardCovers reports whether binding the wildcard address wild takes
// the port away from host: 0.0.0.0 is IPv4 only, so a specific IPv6 address
// beside it binds fine; [::] (dual-stack) and a bare port cover both.
func wildcardCovers(wild, host string) bool {
	wh, _, _ := net.SplitHostPort(wild)
	if wh != "0.0.0.0" {
		return true
	}
	ip := net.ParseIP(host)
	return ip == nil || ip.To4() != nil
}

// resolvePeers trims and deduplicates a list of addresses others send to: a
// port is required and a wildcard host is rejected, since 0.0.0.0 means
// nothing as a destination.
func (a Addrs) resolvePeers(what string) (Addrs, error) {
	return a.resolveSpecific(what, "peers cannot send to a wildcard address")
}

// resolveSpecific is resolvePeers with its own reason for refusing a
// wildcard host.
func (a Addrs) resolveSpecific(what, noWildcard string) (Addrs, error) {
	out := make(Addrs, 0, len(a))
	seen := map[string]bool{}
	for _, s := range a {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w (want host:port, [::1]:port for IPv6)", what, s, err)
		}
		if port == "" {
			return nil, fmt.Errorf("%s %q: no port", what, s)
		}
		if wildcardHost(host) {
			return nil, fmt.Errorf("%s %q: %s", what, s, noWildcard)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// Public lists the addresses that are neither private, loopback, link-local
// nor wildcard: ones the public internet could reach. Hostnames are not
// judged.
func (a Addrs) Public() Addrs {
	var out Addrs
	for _, s := range a {
		host, _, err := net.SplitHostPort(s)
		if err != nil {
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsUnspecified() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Wildcard lists the addresses that mean every interface.
func (a Addrs) Wildcard() Addrs {
	var out Addrs
	for _, s := range a {
		if host, _, err := net.SplitHostPort(s); err == nil && wildcardHost(host) {
			out = append(out, s)
		}
	}
	return out
}

// wildcardHost reports whether a listen host means every interface.
func wildcardHost(h string) bool { return h == "" || h == "0.0.0.0" || h == "::" }

// notIn lists the addresses whose host:port none of the endpoint URLs
// carries, both sides normalised as domain.NormalizeAddr does.
func (a Addrs) notIn(endpoints []string) Addrs {
	have := map[string]bool{}
	for _, e := range endpoints {
		if u, err := url.Parse(withScheme(e)); err == nil && u.Host != "" {
			have[domain.NormalizeAddr(u.Host)] = true
		}
	}
	var out Addrs
	for _, s := range a {
		if !have[domain.NormalizeAddr(s)] {
			out = append(out, s)
		}
	}
	return out
}

type Log struct {
	Level string `yaml:"level"`
	JSON  bool   `yaml:"json"`
}

type Local struct {
	ID                 string                      `yaml:"id"`
	DataDir            string                      `yaml:"data_dir"`
	Network            string                      `yaml:"network"` // balancer: genesis id of the fleet it serves
	AdvertiseEndpoints []string                    `yaml:"advertise_endpoints"`
	Passthrough        Addrs                       `yaml:"passthrough"` // also bind these for algod and splice TCP through to it
	Tier               int                         `yaml:"tier"`
	Tags               []string                    `yaml:"tags"`
	Overrides          *domain.CapabilityOverrides `yaml:"overrides"`
	WaitTimeout        time.Duration               `yaml:"wait_for_block_timeout"`
	VerifyInterval     time.Duration               `yaml:"verify_interval"`
}

type Reg struct {
	Type         string              `yaml:"type"` // algorand | file | memory | static
	AppID        uint64              `yaml:"app_id"`
	SyncKey      string              `yaml:"sync_key"`
	SyncKeyFile  string              `yaml:"sync_key_file"`
	SyncAddress  string              `yaml:"sync_address"` // rekeyed sync account; empty: the sync key's address
	AlgodURL     string              `yaml:"algod_url"`    // empty: through this agent's router
	AlgodToken   string              `yaml:"algod_token"`  // token for AlgodURL
	Refresh      time.Duration       `yaml:"refresh"`
	AutoRegister *bool               `yaml:"auto_register"`
	Cache        string              `yaml:"cache"`
	File         string              `yaml:"file"`
	Static       []domain.NodeRecord `yaml:"static"`
}

type Mesh struct {
	Listen            Addrs                   `yaml:"listen"`        // UDP sockets, one per mesh interface
	Advertise         Addrs                   `yaml:"advertise"`     // what the registry tells peers, preferred first
	SharedSecret      string                  `yaml:"shared_secret"` // key material when there is no sync key
	SuspectAfter      time.Duration           `yaml:"suspect_after"`
	DownAfter         time.Duration           `yaml:"down_after"`
	ProbeInterval     time.Duration           `yaml:"probe_interval"`
	KeepAlive         time.Duration           `yaml:"keepalive"`
	PathProbeInterval time.Duration           `yaml:"path_probe_interval"` // periodic ping of every path to a peer
	PathTimeout       time.Duration           `yaml:"path_timeout"`        // an unanswered ping is a loss after this
	PeerOverrides     map[string]PeerOverride `yaml:"peer_overrides"`
}

type PeerOverride struct {
	Tier *int `yaml:"tier"`
}

type Tiers struct {
	External []External `yaml:"external"`
}

type External struct {
	Name         string                      `yaml:"name"`
	URL          string                      `yaml:"url"`
	Token        string                      `yaml:"token"`
	Tier         int                         `yaml:"tier"`
	Capabilities *domain.CapabilityOverrides `yaml:"capabilities"`
	HealthCheck  time.Duration               `yaml:"health_check"`
}

type Routing struct {
	SyncTolerance          uint64        `yaml:"sync_tolerance"`
	LagGrace               time.Duration `yaml:"lag_grace"` // one round behind is tolerated this long
	ReturnHysteresisRounds int           `yaml:"return_hysteresis_rounds"`
	MultiBroadcast         bool          `yaml:"multi_broadcast"`
	RetryBudget            *int          `yaml:"retry_budget"`     // cap on further mesh upstreams tried after a failure; unset: every eligible one
	UpstreamTimeout        time.Duration `yaml:"upstream_timeout"` // bound on one attempt at one upstream
	RequestTimeout         time.Duration `yaml:"request_timeout"`  // bound on one request across every upstream tried; default 2*upstream_timeout
	PendingTTL             time.Duration `yaml:"pending_ttl"`
	DrainTimeout           time.Duration `yaml:"drain_timeout"`
	Breaker                Breaker       `yaml:"breaker"`
	Exclude                []string      `yaml:"exclude"` // peer node ids and external names never routed to
}

type Breaker struct {
	Threshold int           `yaml:"threshold"`
	OpenFor   time.Duration `yaml:"open_for"`
}

// Load reads, defaults and validates a config file.
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if c, err = Decode(b); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Finish(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Decode parses YAML strictly (unknown keys fail) without defaults or
// validation; Finish does those.
func Decode(b []byte) (Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return c, err
	}
	return c, nil
}

// Finish applies defaults, resolves file-backed secrets and validates.
func (c *Config) Finish() error {
	role, err := domain.ParseRole(c.Role)
	if err != nil {
		return err
	}
	c.Role = string(role)
	balancer := role == domain.RoleBalancer
	if _, err := domain.ParseMode(c.Mode); err != nil {
		return err
	}
	if c.Mode == "" && balancer {
		c.Mode = string(domain.ModeLoadBalancer)
	}
	if c.Mode == "" {
		c.Mode = string(domain.ModeFallback)
	}
	listen, err := c.Listen.resolve("listen", "127.0.0.1:4000")
	if err != nil {
		return err
	}
	c.Listen = listen
	if c.ClientToken == "" && c.ClientTokenFile != "" {
		t, err := readSecret(c.ClientTokenFile)
		if err != nil {
			return fmt.Errorf("client_token_file: %w", err)
		}
		c.ClientToken = t
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Local.ID == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return errors.New("local.id is required (hostname unavailable)")
		}
		c.Local.ID = h
	}
	switch {
	case balancer && c.Local.Network == "":
		return errors.New("local.network (the genesis id, e.g. mainnet-v1.0) is required for role balancer")
	case !balancer && c.Local.Network != "":
		return errors.New("local.network is only for role balancer; a node's network comes from its genesis")
	case !balancer && c.Local.DataDir == "":
		return errors.New("local.data_dir is required")
	}
	// URLs without a scheme are a common slip (host:port copied from
	// algod.net) and cannot be proxied; assume plain http.
	for i, u := range c.Local.AdvertiseEndpoints {
		c.Local.AdvertiseEndpoints[i] = withScheme(u)
	}
	c.Registry.AlgodURL = withScheme(c.Registry.AlgodURL)
	for i := range c.Registry.Static {
		for j, u := range c.Registry.Static[i].Endpoints {
			c.Registry.Static[i].Endpoints[j] = withScheme(u)
		}
	}
	for i := range c.Tiers.External {
		c.Tiers.External[i].URL = withScheme(c.Tiers.External[i].URL)
	}
	// A balancer has no data dir, so no default admin token file.
	if c.AdminToken == "" && (c.AdminTokenFile != "" || c.Local.DataDir != "") {
		path, explicit := c.AdminTokenFile, c.AdminTokenFile != ""
		if !explicit {
			path = filepath.Join(c.Local.DataDir, "algod.admin.token")
		}
		t, err := readSecret(path)
		switch {
		case err == nil:
			c.AdminToken = t
		case !explicit && (errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)):
			// no default admin token: /mesh/* is closed unless auth is off entirely
		default:
			return fmt.Errorf("admin_token_file: %w", err)
		}
	}
	if c.Registry.Type == "" {
		switch {
		case c.Registry.AppID != 0:
			c.Registry.Type = "algorand"
		case c.Registry.File != "":
			c.Registry.Type = "file"
		default:
			c.Registry.Type = "static"
		}
	}
	switch c.Registry.Type {
	case "algorand":
		if c.Registry.AppID == 0 {
			return errors.New("registry.app_id is required for type algorand")
		}
		if c.Registry.SyncKey == "" && c.Registry.SyncKeyFile == "" {
			return errors.New("registry.sync_key or sync_key_file is required for type algorand")
		}
		if balancer && c.Registry.AlgodURL == "" {
			return errors.New("registry.algod_url is required for role balancer, which has no local node to read the registry through")
		}
		if c.Registry.SyncAddress != "" {
			if _, err := types.DecodeAddress(c.Registry.SyncAddress); err != nil {
				return fmt.Errorf("registry.sync_address: %w", err)
			}
		}
	case "file":
		if c.Registry.File == "" {
			return errors.New("registry.file is required for type file")
		}
	case "memory":
		// process-local registry: dev fleet and single-node setups
	case "static":
		for _, r := range c.Registry.Static {
			if err := r.Validate(); err != nil {
				return fmt.Errorf("registry.static: %w", err)
			}
		}
	default:
		return fmt.Errorf("registry.type %q: want algorand, file, memory or static", c.Registry.Type)
	}
	if c.Registry.SyncKey == "" && c.Registry.SyncKeyFile != "" {
		k, err := readSecret(c.Registry.SyncKeyFile)
		if err != nil {
			return fmt.Errorf("registry.sync_key_file: %w", err)
		}
		c.Registry.SyncKey = k
	}
	if c.Registry.Refresh == 0 {
		c.Registry.Refresh = 5 * time.Minute
	}
	if c.Registry.AutoRegister == nil {
		v := c.Registry.Type != "static"
		c.Registry.AutoRegister = &v
	}
	if *c.Registry.AutoRegister && c.Registry.Type != "static" && !balancer && len(c.Local.AdvertiseEndpoints) == 0 {
		return errors.New("local.advertise_endpoints is required when registry.auto_register is on")
	}
	// The wildcard-plus-specific rule is exactly UDP's EADDRINUSE case.
	meshListen, err := c.Mesh.Listen.resolve("mesh.listen", "0.0.0.0:4001")
	if err != nil {
		return err
	}
	c.Mesh.Listen = meshListen
	advertise, err := c.Mesh.Advertise.resolvePeers("mesh.advertise")
	if err != nil {
		return err
	}
	c.Mesh.Advertise = advertise
	if len(c.Mesh.Advertise) == 0 && *c.Registry.AutoRegister && c.Registry.Type != "static" {
		return errors.New("mesh.advertise (the addresses peers send heartbeats to) is required for auto_register")
	}
	passthrough, err := c.Local.Passthrough.resolveSpecific("local.passthrough", "a wildcard would take the port from algod")
	if err != nil {
		return err
	}
	c.Local.Passthrough = passthrough
	if balancer && len(c.Local.Passthrough) > 0 {
		return errors.New("local.passthrough: a balancer has no algod to pass through to")
	}
	// The pass-through must not fight the client listeners for a port:
	// ports first, wildcardCovers ignores them on purpose.
	for _, pt := range c.Local.Passthrough {
		ph, pp, _ := net.SplitHostPort(pt)
		for _, l := range c.Listen {
			lh, lp, _ := net.SplitHostPort(l)
			if lp != pp {
				continue
			}
			if l == pt || (wildcardHost(lh) && wildcardCovers(l, ph)) {
				return fmt.Errorf("local.passthrough %s: listen %s already binds it", pt, l)
			}
		}
	}
	if c.Mesh.PathProbeInterval == 0 {
		c.Mesh.PathProbeInterval = 30 * time.Second
	}
	if c.Mesh.PathTimeout == 0 {
		c.Mesh.PathTimeout = 2 * time.Second
	}
	for i := range c.Tiers.External {
		e := &c.Tiers.External[i]
		if e.Name == "" || e.URL == "" {
			return fmt.Errorf("tiers.external[%d]: name and url are required", i)
		}
		if e.HealthCheck < 0 {
			return fmt.Errorf("tiers.external[%d]: health_check must not be negative", i)
		}
		if e.HealthCheck == 0 {
			e.HealthCheck = time.Minute
		}
		if e.Tier == 0 {
			e.Tier = 100
		}
	}
	exclude := make([]string, 0, len(c.Routing.Exclude))
	excluded := map[string]bool{}
	for _, id := range c.Routing.Exclude {
		id = strings.TrimSpace(id)
		switch {
		case id == "" || excluded[id]:
			continue
		case id == c.Local.ID:
			return fmt.Errorf("routing.exclude %q is this agent's own node (local.id)", id)
		}
		excluded[id] = true
		exclude = append(exclude, id)
	}
	c.Routing.Exclude = exclude
	if c.Routing.ReturnHysteresisRounds == 0 {
		c.Routing.ReturnHysteresisRounds = 3
	}
	if c.Routing.LagGrace == 0 {
		c.Routing.LagGrace = 1500 * time.Millisecond
	}
	if c.Routing.RetryBudget != nil && *c.Routing.RetryBudget < 0 {
		return errors.New("routing.retry_budget must not be negative")
	}
	if c.Routing.UpstreamTimeout == 0 {
		c.Routing.UpstreamTimeout = 60 * time.Second
	}
	if c.Routing.RequestTimeout < 0 {
		return errors.New("routing.request_timeout must not be negative")
	}
	if c.Routing.RequestTimeout == 0 {
		c.Routing.RequestTimeout = 2 * c.Routing.UpstreamTimeout
	}
	if c.Local.WaitTimeout == 0 {
		c.Local.WaitTimeout = 60 * time.Second
	}
	if c.Routing.PendingTTL == 0 {
		c.Routing.PendingTTL = 10 * time.Second
	}
	if c.Routing.DrainTimeout == 0 {
		c.Routing.DrainTimeout = 10 * time.Second
	}
	if c.Routing.Breaker.Threshold == 0 {
		c.Routing.Breaker.Threshold = 5
	}
	if c.Routing.Breaker.OpenFor == 0 {
		c.Routing.Breaker.OpenFor = 10 * time.Second
	}
	return c.RateLimit.finish(c.ClientToken, c.AdminToken)
}

// Balancer reports whether the agent runs without a local node.
func (c *Config) Balancer() bool { return c.Role == string(domain.RoleBalancer) }

// Warnings lists deployment properties worth a look that are not errors: a
// single-homed public host must still work, but gossip is meant to stay off
// the public internet.
func (c *Config) Warnings() []string {
	var out []string
	if w := c.Mesh.Listen.Wildcard(); len(w) > 0 {
		out = append(out, fmt.Sprintf("mesh.listen %s answers on every interface, public ones included; list the mesh addresses to keep gossip off the internet", w))
	}
	if p := c.Mesh.Listen.Public(); len(p) > 0 {
		out = append(out, fmt.Sprintf("mesh.listen %s is a public address", p))
	}
	if p := c.Mesh.Advertise.Public(); len(p) > 0 {
		out = append(out, fmt.Sprintf("mesh.advertise %s is a public address: peers will send heartbeats over the internet", p))
	}
	if p := c.Local.Passthrough.Public(); len(p) > 0 {
		out = append(out, fmt.Sprintf("local.passthrough %s is a public address: it exposes algod to the internet", p))
	}
	if missing := c.Local.Passthrough.notIn(c.Local.AdvertiseEndpoints); len(missing) > 0 {
		out = append(out, fmt.Sprintf("local.passthrough %s is not in local.advertise_endpoints: peers will never use it", missing))
	}
	// The same defaults as app.DirectoryOptions. A path is deaf once the
	// peer has missed three keepalives, and it is the peer's silence ping at
	// suspect_after that carries that news; sent earlier, it says nothing.
	keepAlive, suspectAfter := c.Mesh.KeepAlive, c.Mesh.SuspectAfter
	if keepAlive == 0 {
		keepAlive = 5 * time.Second
	}
	if suspectAfter == 0 {
		suspectAfter = 15 * time.Second
	}
	if suspectAfter < 3*keepAlive {
		out = append(out, fmt.Sprintf("mesh.suspect_after %s is under three keepalives (%s): a silent peer's ping cannot mark a one-way path deaf, so that failover waits for path_probe_interval", suspectAfter, 3*keepAlive))
	}
	return out
}

// withScheme prefixes http:// to a non-empty URL that has no scheme.
func withScheme(u string) string {
	u = strings.TrimSpace(u)
	if u == "" || strings.Contains(u, "://") {
		return u
	}
	return "http://" + u
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// SyncSeed parses registry.sync_key: a 25-word mnemonic, or base64/hex of a
// 32-byte seed or 64-byte ed25519 private key.
func (c *Config) SyncSeed() ([]byte, error) {
	k := strings.TrimSpace(c.Registry.SyncKey)
	if k == "" {
		return nil, nil
	}
	if strings.Count(k, " ") >= 24 {
		sk, err := mnemonic.ToPrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("sync_key mnemonic: %w", err)
		}
		return sk[:32], nil
	}
	if b, err := hex.DecodeString(k); err == nil && (len(b) == 32 || len(b) == 64) {
		return b[:32], nil
	}
	if b, err := base64.StdEncoding.DecodeString(k); err == nil && (len(b) == 32 || len(b) == 64) {
		return b[:32], nil
	}
	return nil, errors.New("sync_key: want a 25-word mnemonic or base64/hex of 32 or 64 bytes")
}

// KeyMaterial is what agent signing keys are derived from: the sync seed
// when there is one, otherwise the mesh shared secret.
func (c *Config) KeyMaterial() ([]byte, error) {
	if seed, err := c.SyncSeed(); err != nil || seed != nil {
		return seed, err
	}
	// The prefix carries the project's old name on purpose: it is a frozen
	// protocol constant (see keymaterial_test.go) that every agent key depends on.
	sum := sha256.Sum256([]byte("algod-loadb-mesh-shared-secret:" + c.Mesh.SharedSecret))
	return sum[:], nil
}

// nonNegative is whether v is a finite number, 0 or more: YAML reads .nan
// and .inf, and either would make a bucket admit everything or nothing.
func nonNegative(v float64) bool { return v >= 0 && !math.IsInf(v, 1) }
