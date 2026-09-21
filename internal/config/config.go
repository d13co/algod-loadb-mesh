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
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/mnemonic"
	"github.com/algorand/go-algorand-sdk/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
)

// Config is the whole file.
type Config struct {
	Role            string  `yaml:"role"` // node | balancer
	Mode            string  `yaml:"mode"`
	Listen          Addrs   `yaml:"listen"`
	ClientToken     string  `yaml:"client_token"`
	ClientTokenFile string  `yaml:"client_token_file"`
	AdminToken      string  `yaml:"admin_token"`      // required for /loadb/*; also accepted as a client token
	AdminTokenFile  string  `yaml:"admin_token_file"` // default: algod.admin.token in local.data_dir
	Log             Log     `yaml:"log"`
	Local           Local   `yaml:"local"`
	Registry        Reg     `yaml:"registry"`
	Mesh            Mesh    `yaml:"mesh"`
	Tiers           Tiers   `yaml:"tiers"`
	Routing         Routing `yaml:"routing"`
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
	AlgodURL     string              `yaml:"algod_url"`    // empty: local node
	AlgodToken   string              `yaml:"algod_token"`  // empty: local token
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
	RetryBudget            *int          `yaml:"retry_budget"`
	UpstreamTimeout        time.Duration `yaml:"upstream_timeout"`
	PendingTTL             time.Duration `yaml:"pending_ttl"`
	DrainTimeout           time.Duration `yaml:"drain_timeout"`
	Breaker                Breaker       `yaml:"breaker"`
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
			// no default admin token: /loadb/* is closed unless auth is off entirely
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
		if e.HealthCheck == 0 {
			e.HealthCheck = time.Minute
		}
		if e.Tier == 0 {
			e.Tier = 100
		}
	}
	if c.Routing.ReturnHysteresisRounds == 0 {
		c.Routing.ReturnHysteresisRounds = 3
	}
	if c.Routing.LagGrace == 0 {
		c.Routing.LagGrace = 1500 * time.Millisecond
	}
	if c.Routing.RetryBudget == nil {
		v := 1
		c.Routing.RetryBudget = &v
	}
	if c.Routing.UpstreamTimeout == 0 {
		c.Routing.UpstreamTimeout = 60 * time.Second
	}
	if c.Local.WaitTimeout == 0 {
		c.Local.WaitTimeout = 20 * time.Second
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
	return nil
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
	sum := sha256.Sum256([]byte("algod-loadb-mesh-shared-secret:" + c.Mesh.SharedSecret))
	return sum[:], nil
}
