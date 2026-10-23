package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// errQuotaExceeded is the cause a simulation in flight is cancelled with once
// its client's allowance runs out.
var errQuotaExceeded = errors.New("rate limit: allowance exhausted during the request")

// costHeader tells the client what a request cost, in credits.
const costHeader = "X-Algod-Loadb-Mesh-Cost"

// loopbackAddr is the RemoteAddr of the agent's own registry reads, which go
// through the router in-process (agent.loopback). No network client has it:
// net/http sets RemoteAddr to host:port.
const loopbackAddr = "loopback"

// LimitToken is one configured API token and the plan it is limited by.
type LimitToken struct{ Name, Token, Plan string }

// LimiterOptions configures per-client rate limits.
type LimiterOptions struct {
	Plans         map[string]domain.Plan
	Tokens        []LimitToken
	AnonymousPlan string // the plan of requests without a token; "" refuses them
	Weights       domain.CostWeights
	// SimulatePerSecond is what a simulation costs per second while it runs,
	// on top of its base cost.
	SimulatePerSecond float64
	// Effort prices the work a mesh node reports (domain.EffortHeader). A
	// request it reports for is billed base + that, in place of the
	// running time; nil uses domain.DefaultEffortRates.
	Effort *domain.EffortRates
	// TrustedProxies are the peers whose X-Forwarded-For names the client.
	TrustedProxies []netip.Prefix
	IPv6Prefix     int    // anonymous IPv6 clients are keyed by this prefix; default 64
	MaxKeys        int    // clients tracked at most; new anonymous ones then share one allowance; default 200000
	StateFile      string // where the allowances survive a restart; "" keeps them in memory only
	Tick           time.Duration
	SaveEvery      time.Duration
}

func (o *LimiterOptions) defaults() {
	if o.Weights == nil {
		o.Weights = domain.DefaultCostWeights
	}
	if o.Effort == nil {
		r := domain.DefaultEffortRates
		o.Effort = &r
	}
	if o.IPv6Prefix == 0 {
		o.IPv6Prefix = 64
	}
	if o.MaxKeys == 0 {
		o.MaxKeys = 200000
	}
	if o.Tick == 0 {
		o.Tick = 250 * time.Millisecond
	}
	if o.SaveEvery == 0 {
		o.SaveEvery = time.Minute
	}
}

// Identity is who a request is billed to.
type Identity struct {
	Exempt bool   // the admin token, health probes, the agent's own reads
	Key    string // "token:<name>" or "ip:<address or IPv6 prefix>"
	Plan   string
}

// Limiter holds every client's allowance: two token buckets of credits per
// client, one refilled over a minute and one over a day. A request is
// charged its base cost up front and refused with 429 when either bucket
// lacks it. A simulation is also metered while it runs, and cancelled once
// what it has run up leaves either bucket overdrawn. When the node that
// answers reports the work it did, that is billed instead of the time.
type Limiter struct {
	opts   LimiterOptions
	plans  map[string]*domain.Plan
	tokens map[[32]byte]LimitToken // by the token's hash: a map lookup must not time the secret
	byName map[string]LimitToken
	clock  ports.Clock
	log    ports.Logger
	metric ports.Metrics

	saveMu   sync.Mutex
	mu       sync.Mutex
	keys     map[string]*limitKey
	active   map[*limitKey]struct{} // keys with a metered request in flight
	overflow *limitKey
}

// limitKey is one client's allowance.
type limitKey struct {
	id      string
	plan    *domain.Plan
	minute  domain.Bucket
	day     domain.Bucket
	flights map[*Ticket]struct{} // billed requests in flight
}

// NewLimiter builds a limiter. Nothing runs until Run.
func NewLimiter(o LimiterOptions, clock ports.Clock, log ports.Logger, metric ports.Metrics) *Limiter {
	o.defaults()
	l := &Limiter{opts: o, plans: map[string]*domain.Plan{}, tokens: map[[32]byte]LimitToken{}, byName: map[string]LimitToken{},
		clock: clock, log: log, metric: metric, keys: map[string]*limitKey{}, active: map[*limitKey]struct{}{}}
	for name, p := range o.Plans {
		p.Name = name
		l.plans[name] = &p
	}
	for _, t := range o.Tokens {
		l.tokens[sha256.Sum256([]byte(t.Token))] = t
		l.byName[t.Name] = t
	}
	return l
}

// Identify bills a request: a configured token to its plan, no token to the
// anonymous plan by client address. ok is false for a token not configured,
// or no token when there is no anonymous plan.
func (l *Limiter) Identify(req *http.Request, token string) (id Identity, ok bool) {
	if req.RemoteAddr == loopbackAddr {
		return Identity{Exempt: true}, true
	}
	if token == "" {
		if l.opts.AnonymousPlan == "" {
			return Identity{}, false
		}
		return Identity{Key: "ip:" + l.clientKey(req), Plan: l.opts.AnonymousPlan}, true
	}
	t, ok := l.tokens[sha256.Sum256([]byte(token))]
	if !ok {
		return Identity{}, false
	}
	return Identity{Key: "token:" + t.Name, Plan: t.Plan}, true
}

// clientKey is the client's address: the socket peer, or, when that is a
// trusted proxy, the last X-Forwarded-For entry it did not add itself. An
// IPv6 client is keyed by its prefix, since one host commonly holds a /64.
func (l *Limiter) clientKey(req *http.Request) string {
	addr, ok := parseAddr(req.RemoteAddr)
	if !ok {
		return req.RemoteAddr
	}
	if l.trusted(addr) {
		hops := strings.Split(strings.Join(req.Header.Values("X-Forwarded-For"), ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, ok := parseAddr(strings.TrimSpace(hops[i]))
			if !ok {
				break
			}
			addr = hop
			if !l.trusted(hop) {
				break
			}
		}
	}
	if addr.Is6() {
		if p, err := addr.Prefix(l.opts.IPv6Prefix); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

func (l *Limiter) trusted(a netip.Addr) bool {
	for _, p := range l.opts.TrustedProxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// parseAddr reads an address with or without a port, IPv4-mapped IPv6 as IPv4.
func parseAddr(s string) (netip.Addr, bool) {
	if host, _, err := net.SplitHostPort(s); err == nil {
		s = host
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap().WithZone(""), true
}

// Denial is a request refused before it runs, or a simulation cut short.
type Denial struct {
	Status     int
	Message    string
	RetryAfter time.Duration
	Header     http.Header // RateLimit headers, when there is an allowance to report
}

// Write answers the client with the denial.
func (d *Denial) Write(w http.ResponseWriter) {
	for k, vs := range d.Header {
		w.Header()[k] = vs
	}
	if d.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(ceilSeconds(d.RetryAfter), 10))
	}
	writeMessage(w, d.Status, d.Message)
}

// Admit charges a request's base cost to its client, best being the fleet's
// best round. It refuses with 403 a request the plan never allows, and with
// 429 one the client's allowance cannot cover now.
func (l *Limiter) Admit(id Identity, class domain.RequestClass, best uint64) (*Ticket, *Denial) {
	plan := l.plans[id.Plan]
	costKey := domain.CostKey(class, best)
	// A simulate body the router could not read cannot be held to the
	// caps; algod would reject it anyway. It is still charged below.
	unreadable := class.Simulate && class.SimNeeds == nil && plan.Simulate != (domain.SimulateCaps{})
	if class.Simulate && class.SimNeeds != nil {
		if msg := plan.Simulate.Check(*class.SimNeeds); msg != "" {
			l.metric.Inc("loadb_ratelimited", "plan", plan.Name, "class", costKey, "reason", "cap")
			return nil, &Denial{Status: http.StatusForbidden, Message: "algod-loadb-mesh: " + msg}
		}
	}
	cost := l.opts.Weights.Base(class, best)
	for _, w := range []struct {
		name string
		domain.Window
	}{{"minute", plan.Minute}, {"day", plan.Day}} {
		if w.Limited() && cost > w.Capacity {
			l.metric.Inc("loadb_ratelimited", "plan", plan.Name, "class", costKey, "reason", "cap")
			return nil, &Denial{Status: http.StatusForbidden,
				Message: fmt.Sprintf("algod-loadb-mesh: this request costs %s credits, more than this plan allows per %s (%s)", credits(cost), w.name, credits(w.Capacity))}
		}
	}
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.key(id, plan)
	k.refill(now)
	if wait, window := k.shortfall(cost); window != "" {
		l.metric.Inc("loadb_ratelimited", "plan", plan.Name, "class", costKey, "reason", window)
		return nil, &Denial{Status: http.StatusTooManyRequests, RetryAfter: wait, Header: k.header(),
			Message: fmt.Sprintf("algod-loadb-mesh: rate limit exceeded: this plan's %s allowance is used up", perWindow(window))}
	}
	k.charge(cost)
	if unreadable {
		l.metric.Inc("loadb_ratelimited", "plan", plan.Name, "class", costKey, "reason", "cap")
		return nil, &Denial{Status: http.StatusBadRequest, Header: k.header(), Message: "algod-loadb-mesh: cannot read the simulate request"}
	}
	t := &Ticket{l: l, k: k, costKey: costKey, base: cost, metered: class.Simulate, start: now}
	// Every request in flight keeps its key: one billed by effort is charged
	// at Done, and a key swept before then would lose the charge.
	k.flights[t] = struct{}{}
	if t.metered {
		l.active[k] = struct{}{}
	}
	return t, nil
}

// key finds or makes the client's allowance; l.mu is held.
func (l *Limiter) key(id Identity, plan *domain.Plan) *limitKey {
	if k, ok := l.keys[id.Key]; ok {
		return k
	}
	if strings.HasPrefix(id.Key, "ip:") && len(l.keys) >= l.opts.MaxKeys {
		if l.overflow == nil {
			l.overflow = newLimitKey("ip:overflow", plan)
			l.log.Warn("rate limit: too many clients tracked, new anonymous ones share one allowance", "max_keys", l.opts.MaxKeys)
		}
		return l.overflow
	}
	k := newLimitKey(id.Key, plan)
	l.keys[id.Key] = k
	return k
}

func newLimitKey(id string, plan *domain.Plan) *limitKey {
	return &limitKey{id: id, plan: plan, flights: map[*Ticket]struct{}{}}
}

func (k *limitKey) refill(now time.Time) {
	k.minute = k.plan.Minute.Refill(k.minute, now)
	k.day = k.plan.Day.Refill(k.day, now)
}

func (k *limitKey) charge(c float64) {
	if k.plan.Minute.Limited() {
		k.minute.Level -= c
	}
	if k.plan.Day.Limited() {
		k.day.Level -= c
	}
}

// owed is what the key's requests in flight have run up so far.
func (k *limitKey) owed() float64 {
	var sum float64
	for t := range k.flights {
		sum += t.extra
	}
	return sum
}

// metering is whether any of the key's requests in flight is metered.
func (k *limitKey) metering() bool {
	for t := range k.flights {
		if t.metered {
			return true
		}
	}
	return false
}

// shortfall is how long until both (refilled) buckets, less what is owed,
// hold cost, and the window that takes longest; "" when they hold it now.
func (k *limitKey) shortfall(cost float64) (time.Duration, string) {
	if cost <= 0 {
		return 0, "" // free whatever is owed
	}
	owed := k.owed()
	var wait time.Duration
	window := ""
	if d := k.plan.Minute.Wait(k.minute.Level-owed, cost); d > 0 {
		wait, window = d, "minute"
	}
	if d := k.plan.Day.Wait(k.day.Level-owed, cost); d > wait {
		wait, window = d, "day"
	}
	if window != "" && wait < time.Second {
		wait = time.Second
	}
	return wait, window
}

// overdrawn reports whether what is owed leaves either bucket below zero.
func (k *limitKey) overdrawn() bool {
	owed := k.owed()
	return k.plan.Minute.Limited() && k.minute.Level-owed < 0 || k.plan.Day.Limited() && k.day.Level-owed < 0
}

// header is the RateLimit-Policy and RateLimit headers (the IETF httpapi
// draft) for the key's limited windows: each window's allowance and period,
// and what is left of it and how long until it is full again.
func (k *limitKey) header() http.Header {
	h := http.Header{}
	owed := k.owed()
	var policy, state []string
	for _, w := range []struct {
		name string
		domain.Window
		b domain.Bucket
	}{{"minute", k.plan.Minute, k.minute}, {"day", k.plan.Day, k.day}} {
		if !w.Limited() {
			continue
		}
		left := w.b.Level - owed
		policy = append(policy, fmt.Sprintf("%q;q=%d;w=%d", w.name, int64(w.Capacity), int64(w.Period.Seconds())))
		state = append(state, fmt.Sprintf("%q;r=%d;t=%d", w.name, max(int64(math.Floor(left)), 0), ceilSeconds(w.UntilFull(left))))
	}
	if len(policy) > 0 {
		h.Set("RateLimit-Policy", strings.Join(policy, ", "))
		h.Set("RateLimit", strings.Join(state, ", "))
	}
	return h
}

// Ticket is one admitted request's account with its client.
type Ticket struct {
	l       *Limiter
	k       *limitKey
	costKey string
	base    float64
	metered bool
	start   time.Time
	// Guarded by l.mu:
	extra     float64       // what the meter ran up, or the reported effort costs; billed when the request ends
	frozen    bool          // extra is final: the response began, or the request was cancelled
	effort    domain.Effort // what mesh nodes reported doing for it, over every attempt
	reported  bool          // a mesh node reported its effort
	refunded  bool
	cancel    context.CancelCauseFunc
	cancelled bool
	done      bool
}

// Reported adds the work a mesh node reported doing for one attempt. The
// request is then billed by its effort rather than its running time; the
// meter still runs until the response, so that a client can be cut off
// while the work goes on.
func (t *Ticket) Reported(e domain.Effort) {
	t.l.mu.Lock()
	defer t.l.mu.Unlock()
	t.effort, t.reported = t.effort.Add(e), true
}

// settle makes extra what the request costs beyond its base by now: the
// reported effort, else the running time; l.mu is held.
func (t *Ticket) settle(now time.Time) {
	switch {
	case t.frozen:
	case t.reported:
		t.extra = t.l.opts.Effort.Credits(t.effort)
	case t.metered:
		t.extra = t.meter(now)
	}
}

// Track lets the limiter cancel the request once its client is overdrawn.
func (t *Ticket) Track(cancel context.CancelCauseFunc) {
	t.l.mu.Lock()
	t.cancel = cancel
	t.l.mu.Unlock()
}

// Responding is called as the response headers go out, with the status.
// The meter stops, as the work is done by the time a node answers, and the
// request is no longer cancelled. A busy answer (503 with Retry-After) that
// no node reported effort for did no work: it costs no more than the
// cheapest request. Responding adds the RateLimit headers and the cost to h.
func (t *Ticket) Responding(status int, h http.Header) {
	l := t.l
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if status == http.StatusServiceUnavailable && h.Get("Retry-After") != "" && !t.reported && !t.refunded {
		t.refund()
	}
	t.settle(now)
	t.frozen = true
	t.k.refill(now)
	for k, vs := range t.k.header() {
		h[k] = vs
	}
	h.Set(costHeader, credits(t.base+t.extra))
}

// Done bills what the meter ran up and ends the request's account.
func (t *Ticket) Done() {
	l := t.l
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.done {
		return
	}
	t.done = true
	t.settle(now)
	delete(t.k.flights, t)
	if !t.k.metering() {
		delete(l.active, t.k)
	}
	t.k.charge(t.extra)
	by := "base"
	switch {
	case t.reported:
		by = "effort"
	case t.metered && !t.refunded:
		by = "time"
	}
	l.metric.Observe("loadb_credits", t.base+t.extra, "plan", t.k.plan.Name, "class", t.costKey, "by", by)
}

// refund gives back the base cost of a request that did no work, down to
// the default weight, and stops its meter; l.mu is held.
func (t *Ticket) refund() {
	floor := t.l.opts.Weights["default"]
	if back := t.base - floor; back > 0 {
		t.k.charge(-back)
		t.base = floor
	}
	t.extra, t.frozen, t.refunded = 0, true, true
	t.l.metric.Inc("loadb_ratelimit_refunded", "plan", t.k.plan.Name, "class", t.costKey)
}

// Exhausted is the answer to a simulation cancelled for an exhausted
// allowance: 429, retry once the same request would be admitted again.
func (t *Ticket) Exhausted() *Denial {
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	t.k.refill(l.clock.Now())
	wait, _ := t.k.shortfall(t.base)
	return &Denial{Status: http.StatusTooManyRequests, RetryAfter: max(wait, time.Second), Header: t.k.header(),
		Message: "algod-loadb-mesh: rate limit exceeded: the allowance ran out during the simulation"}
}

// meter is what the request has run up by now; l.mu is held.
func (t *Ticket) meter(now time.Time) float64 {
	return max(now.Sub(t.start).Seconds(), 0) * t.l.opts.SimulatePerSecond
}

// tick runs the meter of every request in flight, and cancels those of a
// client it leaves overdrawn. Cancelling only some would not do: the work
// they did is billed all the same.
func (l *Limiter) tick() {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k := range l.active {
		for t := range k.flights {
			if t.metered && !t.frozen {
				t.extra = t.meter(now)
			}
		}
		k.refill(now)
		if !k.overdrawn() {
			continue
		}
		for t := range k.flights {
			if t.frozen || t.cancel == nil {
				continue
			}
			t.frozen, t.cancelled = true, true
			t.cancel(errQuotaExceeded)
			l.metric.Inc("loadb_ratelimit_cancelled", "plan", k.plan.Name)
			l.log.Debug("rate limit: simulation cancelled, allowance exhausted", "client", k.id, "billed", credits(t.base+t.extra))
		}
	}
}

// sweep forgets the clients whose allowance is full again: they owe
// nothing, so a fresh key would be the same.
func (l *Limiter) sweep() {
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, k := range l.keys {
		if len(k.flights) > 0 {
			continue
		}
		k.refill(now)
		if k.plan.Minute.Full(k.minute) && k.plan.Day.Full(k.day) {
			delete(l.keys, id)
		}
	}
	if k := l.overflow; k != nil && len(k.flights) == 0 && len(l.keys) < l.opts.MaxKeys {
		k.refill(now)
		if k.plan.Minute.Full(k.minute) && k.plan.Day.Full(k.day) {
			l.overflow = nil
		}
	}
	l.metric.Gauge("loadb_ratelimit_keys", float64(len(l.keys)))
}

// Run meters simulations in flight, and sweeps and saves the allowances,
// until ctx is done; then it saves them once more.
func (l *Limiter) Run(ctx context.Context) error {
	if err := l.load(); err != nil {
		l.log.Warn("rate limit: saved allowances not loaded", "file", l.opts.StateFile, "err", err)
	}
	tick, stopTick := l.clock.Tick(l.opts.Tick)
	defer stopTick()
	save, stopSave := l.clock.Tick(l.opts.SaveEvery)
	defer stopSave()
	for {
		select {
		case <-ctx.Done():
			l.saveLogged()
			return ctx.Err()
		case <-tick:
			l.tick()
		case <-save:
			l.sweep()
			l.saveLogged()
		}
	}
}

// limiterState is the state file: every allowance not full.
type limiterState struct {
	Saved time.Time                `json:"saved"`
	Keys  map[string]limiterBucket `json:"keys"`
}

type limiterBucket struct {
	Minute domain.Bucket `json:"minute"`
	Day    domain.Bucket `json:"day"`
}

// Save writes the allowances now: the composition root calls it once the
// last request has drained, after Run's own save on cancellation.
func (l *Limiter) Save() { l.saveLogged() }

func (l *Limiter) saveLogged() {
	if err := l.save(); err != nil {
		l.log.Warn("rate limit: allowances not saved", "file", l.opts.StateFile, "err", err)
	}
}

// save writes the allowances that are not full, atomically.
func (l *Limiter) save() error {
	if l.opts.StateFile == "" {
		return nil
	}
	l.saveMu.Lock() // Run and Agent.Serve's last save share the tmp file
	defer l.saveMu.Unlock()
	now := l.clock.Now()
	st := limiterState{Saved: now, Keys: map[string]limiterBucket{}}
	l.mu.Lock()
	for id, k := range l.keys {
		k.refill(now)
		// What simulations in flight have run up is owed already: saved
		// as spent, so that a crash before they end does not forgive it.
		owed := k.owed()
		minute, day := k.minute, k.day
		if k.plan.Minute.Limited() {
			minute.Level -= owed
		}
		if k.plan.Day.Limited() {
			day.Level -= owed
		}
		if !k.plan.Minute.Full(minute) || !k.plan.Day.Full(day) {
			st.Keys[id] = limiterBucket{Minute: minute, Day: day}
		}
	}
	l.mu.Unlock()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := l.opts.StateFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(l.opts.StateFile), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.opts.StateFile)
}

// load reads the saved allowances. A client whose token or plan is gone is
// dropped; one already seen since the start keeps the lower of the two.
func (l *Limiter) load() error {
	if l.opts.StateFile == "" {
		return nil
	}
	b, err := os.ReadFile(l.opts.StateFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var st limiterState
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	now := l.clock.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, s := range st.Keys {
		plan := l.planOf(id)
		if plan == nil {
			continue
		}
		saved := &limitKey{plan: plan, minute: s.Minute, day: s.Day}
		saved.refill(now)
		if k, ok := l.keys[id]; ok {
			k.refill(now)
			k.minute.Level = min(k.minute.Level, saved.minute.Level)
			k.day.Level = min(k.day.Level, saved.day.Level)
			continue
		}
		k := newLimitKey(id, plan)
		k.minute, k.day = saved.minute, saved.day
		l.keys[id] = k
	}
	return nil
}

// planOf is the plan a saved key is limited by now; nil when none.
func (l *Limiter) planOf(id string) *domain.Plan {
	switch {
	case strings.HasPrefix(id, "token:"):
		if t, ok := l.byName[strings.TrimPrefix(id, "token:")]; ok {
			return l.plans[t.Plan]
		}
	case strings.HasPrefix(id, "ip:") && id != "ip:overflow":
		return l.plans[l.opts.AnonymousPlan]
	}
	return nil
}

// credits renders a credit amount to two decimals at most.
func credits(v float64) string {
	return strconv.FormatFloat(math.Round(v*100)/100, 'f', -1, 64)
}

func ceilSeconds(d time.Duration) int64 { return int64(math.Ceil(d.Seconds())) }

func perWindow(window string) string {
	if window == "day" {
		return "24-hour"
	}
	return "per-minute"
}
