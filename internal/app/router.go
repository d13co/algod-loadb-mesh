package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// RouterOptions configures request routing.
type RouterOptions struct {
	Mode            domain.Mode
	ClientToken     string
	AdminToken      string // required for /loadb/* except health; also a valid client token
	SyncTolerance   uint64
	UpstreamTimeout time.Duration
	WaitTimeout     time.Duration
	PendingTTL      time.Duration
	RetryBudget     int
	MultiBroadcast  bool
	Weights         domain.Weights
	Version         string
}

func (o *RouterOptions) defaults() {
	if o.Mode == "" {
		o.Mode = domain.ModeFallback
	}
	if o.UpstreamTimeout == 0 {
		o.UpstreamTimeout = 60 * time.Second
	}
	if o.WaitTimeout == 0 {
		o.WaitTimeout = 20 * time.Second
	}
	if o.PendingTTL == 0 {
		o.PendingTTL = time.Minute
	}
}

// Router is the HTTP entry point: it classifies, selects, forwards and
// records. It knows nothing about sockets beyond net/http's handler API.
type Router struct {
	opts    RouterOptions
	dir     *Directory
	monitor *Monitor
	fwd     ports.Forwarder
	httpc   *http.Client
	stats   *StatsBook
	clock   ports.Clock
	log     ports.Logger
	metric  ports.Metrics
	rnd     domain.Rand
	metrics io.WriterTo // Prometheus text, may be nil

	inflight atomic.Int64
	draining atomic.Bool
	started  time.Time

	mu      sync.Mutex
	pending map[string]pendingEntry
}

type pendingEntry struct {
	upstream string
	at       time.Time
}

// NewRouter wires the router.
func NewRouter(o RouterOptions, dir *Directory, monitor *Monitor, fwd ports.Forwarder, httpc *http.Client,
	stats *StatsBook, clock ports.Clock, log ports.Logger, metric ports.Metrics, rnd domain.Rand, metricsText io.WriterTo) *Router {
	o.defaults()
	if httpc == nil {
		httpc = &http.Client{Timeout: o.UpstreamTimeout}
	}
	return &Router{opts: o, dir: dir, monitor: monitor, fwd: fwd, httpc: httpc, stats: stats, clock: clock,
		log: log, metric: metric, rnd: rnd, metrics: metricsText, pending: map[string]pendingEntry{}, started: clock.Now()}
}

// Draining marks the agent as shutting down; new requests are still served
// until the listener closes, but health reports 503 and peers are told.
func (r *Router) Draining(v bool) {
	r.draining.Store(v)
	r.dir.SetDraining(v)
}

// Inflight is the number of requests currently being served.
func (r *Router) Inflight() int64 { return r.inflight.Load() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeMessage(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"message": msg})
}

// ServeHTTP implements http.Handler.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.inflight.Add(1)
	defer r.inflight.Add(-1)
	start := r.clock.Now()
	class := domain.Classify(req.Method, req.URL.Path)
	sw := &statusWriter{ResponseWriter: w}
	defer r.logRequest(sw, req, class, start)
	r.serve(sw, req, class)
}

// logRequest writes the access log line: INFO, except health probes at DEBUG.
func (r *Router) logRequest(w *statusWriter, req *http.Request, class domain.RequestClass, start time.Time) {
	kv := []any{"method", req.Method, "path", req.URL.RequestURI(), "status", w.status, "bytes", w.bytes,
		"duration", r.clock.Now().Sub(start), "ip", clientIP(req)}
	if up := w.Header().Get("X-Algod-Loadb-Mesh-Upstream"); up != "" {
		kv = append(kv, "upstream", up)
	}
	if !class.Agent {
		kv = append(kv, "class", class.String())
	}
	if req.Context().Err() != nil {
		kv = append(kv, "client_gone", true)
	}
	if isHealth(req, class) {
		r.log.Debug("request", kv...)
		return
	}
	r.log.Info("request", kv...)
}

// authorize checks X-Algo-API-Token and answers 401/403 itself when the
// request may not proceed. /loadb/health is open; the rest of /loadb/* needs
// the admin token; everything else needs the client or the admin token. With
// neither token configured, auth is off.
func (r *Router) authorize(w http.ResponseWriter, req *http.Request, class domain.RequestClass) bool {
	tok := req.Header.Get("X-Algo-API-Token")
	admin := r.opts.AdminToken != "" && tokenEqual(tok, r.opts.AdminToken)
	switch {
	case admin, isHealth(req, class), r.opts.AdminToken == "" && r.opts.ClientToken == "":
		return true
	case class.Agent && r.opts.AdminToken == "":
		r.metric.Inc("loadb_requests_unauthorized")
		writeMessage(w, 403, "algod-loadb-mesh: /loadb endpoints need an admin token, and none is configured")
		return false
	case !class.Agent && (r.opts.ClientToken == "" || tokenEqual(tok, r.opts.ClientToken)):
		return true
	}
	r.metric.Inc("loadb_requests_unauthorized")
	writeMessage(w, 401, "Invalid API Token")
	return false
}

func tokenEqual(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func isHealth(req *http.Request, class domain.RequestClass) bool {
	return class.Agent && strings.TrimSuffix(req.URL.Path, "/") == "/loadb/health"
}

// clientIP is the leftmost X-Forwarded-For entry, then X-Real-IP, then the
// socket peer. The headers are client-controlled; they are only trustworthy
// behind a proxy that rewrites them.
func clientIP(req *http.Request) string {
	ip, _, _ := strings.Cut(req.Header.Get("X-Forwarded-For"), ",")
	if ip = strings.TrimSpace(ip); ip == "" {
		ip = strings.TrimSpace(req.Header.Get("X-Real-IP"))
	}
	if ip == "" {
		ip = req.RemoteAddr
		if host, _, err := net.SplitHostPort(ip); err == nil {
			ip = host
		}
	}
	return strings.TrimPrefix(ip, "::ffff:")
}

func (r *Router) serve(w http.ResponseWriter, req *http.Request, class domain.RequestClass) {
	if !r.authorize(w, req, class) {
		return
	}
	if class.Agent {
		r.serveAgent(w, req)
		return
	}
	cands, best := r.dir.Snapshot()
	w.Header().Set("X-Algod-Loadb-Mesh-Mode", string(r.opts.Mode))
	switch {
	case class.WaitAfter != nil:
		r.handleWait(w, req, class, cands, best)
	case class.Broadcast:
		r.handleBroadcast(w, req, class, cands, best)
	case class.PendingID != "":
		r.handlePending(w, req, class, cands, best)
	default:
		r.handleDefault(w, req, class, cands, best)
	}
}

func (r *Router) sel(cands []domain.Upstream, class domain.RequestClass, best uint64) (domain.Selection, bool) {
	return domain.Select(r.opts.Mode, cands, class, best, r.opts.SyncTolerance, r.opts.Weights, r.rnd)
}

// forward sends the request to one upstream and records the outcome.
func (r *Router) forward(w http.ResponseWriter, req *http.Request, u domain.Upstream, class domain.RequestClass, retry map[int]bool) ports.Outcome {
	w.Header().Set("X-Algod-Loadb-Mesh-Upstream", u.ID)
	w.Header().Set("X-Algod-Loadb-Mesh-Tier", fmt.Sprint(u.Tier))
	ctx, cancel := context.WithTimeout(req.Context(), r.opts.UpstreamTimeout)
	defer cancel()
	release := r.stats.Begin(u.ID)
	out := r.fwd.Forward(w, req.WithContext(ctx), ports.Target{BaseURL: u.BaseURL, Token: u.Token, RetryStatus: retry})
	release()
	if req.Context().Err() == nil { // client still there: the outcome is about the upstream
		r.stats.Record(u.ID, class.StatKey(), out)
	}
	r.metric.Inc("loadb_upstream_requests", "upstream", u.ID, "class", class.StatKey(), "failed", boolStr(out.Failed()))
	r.metric.Observe("loadb_upstream_latency_ms", float64(out.Duration)/float64(time.Millisecond), "upstream", u.ID, "class", class.StatKey())
	return out
}

func (r *Router) noUpstream(w http.ResponseWriter, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	r.metric.Inc("loadb_requests_unroutable", "class", class.StatKey())
	summary := make([]string, 0, len(cands))
	for _, u := range cands {
		summary = append(summary, fmt.Sprintf("%s(%s r=%d oldest=%d dev=%v follow=%v breaker=%v draining=%v)", u.ID, u.Health,
			u.LastRound, u.Caps.OldestRound, u.Caps.DeveloperAPI, u.Caps.FollowMode, u.Stats.BreakerOpen, u.Draining))
	}
	r.log.Warn("no eligible upstream", "class", class.String(), "best", best, "candidates", strings.Join(summary, " "))
	writeMessage(w, 503, "algod-loadb-mesh: no eligible upstream for this request")
}

func (r *Router) handleDefault(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	sel, ok := r.sel(cands, class, best)
	if !ok {
		r.noUpstream(w, class, cands, best)
		return
	}
	tries := append([]domain.Upstream{sel.Chosen}, sel.Alternates...)
	budget := 1
	if class.Idempotent {
		budget += r.opts.RetryBudget
	}
	if len(tries) > budget {
		tries = tries[:budget]
	}
	var last ports.Outcome
	for i, u := range tries {
		last = r.forward(w, req, u, class, nil)
		if !last.Failed() || last.HeadersSent || req.Context().Err() != nil {
			return
		}
		if i+1 < len(tries) {
			r.metric.Inc("loadb_retries")
			r.log.Debug("retrying on next upstream", "path", req.URL.Path, "failed", u.ID, "err", last.Err, "status", last.Status)
		}
	}
	r.answerFailure(w, req, last)
}

func (r *Router) answerFailure(w http.ResponseWriter, req *http.Request, out ports.Outcome) {
	if out.HeadersSent {
		return
	}
	switch {
	case req.Context().Err() != nil:
		return
	case out.Err != nil && errors.Is(out.Err, context.DeadlineExceeded):
		writeMessage(w, 504, "algod-loadb-mesh: upstream timeout")
	case out.Status >= 500 || out.Status == 429:
		writeMessage(w, 502, fmt.Sprintf("algod-loadb-mesh: upstream answered %d", out.Status))
	default:
		writeMessage(w, 502, "algod-loadb-mesh: upstream unreachable")
	}
}

// handleWait coalesces wait-for-block-after on the local monitor whenever the
// local node may serve it; otherwise it is an ordinary forwarded request.
func (r *Router) handleWait(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	local := cands[0]
	if local.Kind == domain.KindLocal && domain.Eligible(local, class, best, r.opts.SyncTolerance) {
		ctx, cancel := context.WithTimeout(req.Context(), r.opts.WaitTimeout)
		defer cancel()
		r.metric.Inc("loadb_wait_coalesced")
		body, err := r.monitor.WaitForBlockAfter(ctx, *class.WaitAfter)
		if req.Context().Err() != nil {
			return
		}
		if body == nil {
			writeMessage(w, 503, "algod-loadb-mesh: local node status unavailable")
			return
		}
		_ = err // on timeout algod itself answers with the current status; so do we
		w.Header().Set("X-Algod-Loadb-Mesh-Upstream", local.ID)
		w.Header().Set("X-Algod-Loadb-Mesh-Tier", fmt.Sprint(local.Tier))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write(body)
		return
	}
	r.handleDefault(w, req, class, cands, best)
}

func (r *Router) rememberTxn(txid, upstream string) {
	if txid == "" {
		return
	}
	now := r.clock.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.pending {
		if now.Sub(e.at) > r.opts.PendingTTL {
			delete(r.pending, id)
		}
	}
	r.pending[txid] = pendingEntry{upstream: upstream, at: now}
}

func (r *Router) recallTxn(txid string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.pending[txid]
	if !ok || r.clock.Now().Sub(e.at) > r.opts.PendingTTL {
		return "", false
	}
	return e.upstream, true
}

// handleBroadcast sends POST /v2/transactions to one eligible non-follower
// node (never retried after bytes went out) or, with multi_broadcast, to all
// of them at once, answering with the first success.
func (r *Router) handleBroadcast(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	sel, ok := r.sel(cands, class, best)
	if !ok {
		r.noUpstream(w, class, cands, best)
		return
	}
	if !r.opts.MultiBroadcast {
		cw := &captureWriter{ResponseWriter: w, limit: 8 << 10}
		out := r.forward(cw, req, sel.Chosen, class, nil)
		if out.Failed() && !out.HeadersSent {
			r.answerFailure(w, req, out)
			return
		}
		if out.Status == 200 {
			r.rememberTxn(txIDFrom(cw.buf.Bytes()), sel.Chosen.ID)
		}
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 4<<20))
	if err != nil {
		writeMessage(w, 400, "algod-loadb-mesh: cannot read body")
		return
	}
	targets := dedupeByURL(append([]domain.Upstream{sel.Chosen}, sel.Alternates...))
	type result struct {
		u    domain.Upstream
		resp *http.Response
		body []byte
		err  error
	}
	ctx, cancel := context.WithTimeout(req.Context(), r.opts.UpstreamTimeout)
	defer cancel()
	results := make(chan result, len(targets))
	for _, u := range targets {
		go func(u domain.Upstream) {
			start := r.clock.Now()
			rq, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(u.BaseURL, "/")+req.URL.RequestURI(), bytes.NewReader(body))
			rq.Header.Set("X-Algo-API-Token", u.Token)
			rq.Header.Set("Content-Type", req.Header.Get("Content-Type"))
			release := r.stats.Begin(u.ID)
			resp, err := r.httpc.Do(rq)
			release()
			res := result{u: u, resp: resp, err: err}
			if err == nil {
				res.body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
			}
			out := ports.Outcome{Err: err, Duration: r.clock.Now().Sub(start)}
			if resp != nil {
				out.Status = resp.StatusCode
			}
			r.stats.Record(u.ID, class.StatKey(), out)
			results <- res
		}(u)
	}
	var fallback *result
	for range targets {
		res := <-results
		if res.err == nil && res.resp.StatusCode == 200 {
			r.metric.Inc("loadb_multibroadcast", "outcome", "ok")
			r.rememberTxn(txIDFrom(res.body), res.u.ID)
			w.Header().Set("X-Algod-Loadb-Mesh-Upstream", res.u.ID)
			w.Header().Set("X-Algod-Loadb-Mesh-Tier", fmt.Sprint(res.u.Tier))
			w.Header().Set("Content-Type", res.resp.Header.Get("Content-Type"))
			w.WriteHeader(200)
			_, _ = w.Write(res.body)
			cancel() // other broadcasts are best effort
			return
		}
		if fallback == nil || (res.err == nil && fallback.err != nil) {
			res := res
			fallback = &res
		}
	}
	r.metric.Inc("loadb_multibroadcast", "outcome", "failed")
	if fallback.err != nil {
		writeMessage(w, 502, "algod-loadb-mesh: broadcast failed on every node: "+fallback.err.Error())
		return
	}
	w.Header().Set("X-Algod-Loadb-Mesh-Upstream", fallback.u.ID)
	w.Header().Set("Content-Type", fallback.resp.Header.Get("Content-Type"))
	w.WriteHeader(fallback.resp.StatusCode)
	_, _ = w.Write(fallback.body)
}

func dedupeByURL(us []domain.Upstream) []domain.Upstream {
	seen := map[string]bool{}
	out := us[:0]
	for _, u := range us {
		if !seen[u.BaseURL] {
			seen[u.BaseURL] = true
			out = append(out, u)
		}
	}
	return out
}

func txIDFrom(body []byte) string {
	var v struct {
		TxID string `json:"txId"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.TxID
}

// handlePending looks a pending transaction up on the node that received it
// first, then on the others by round, treating 404 as "try the next one".
func (r *Router) handlePending(w http.ResponseWriter, req *http.Request, class domain.RequestClass, cands []domain.Upstream, best uint64) {
	sel, ok := r.sel(cands, class, best)
	if !ok {
		r.noUpstream(w, class, cands, best)
		return
	}
	order := append([]domain.Upstream{sel.Chosen}, sel.Alternates...)
	// Highest round first, then the remembered node ahead of everything.
	sortByRoundDesc(order)
	if id, ok := r.recallTxn(class.PendingID); ok {
		for i, u := range order {
			if u.ID == id && i > 0 {
				copy(order[1:i+1], order[:i])
				order[0] = u
				break
			}
		}
	}
	notFound := map[int]bool{404: true}
	var last ports.Outcome
	for i, u := range order {
		retry := notFound
		if i == len(order)-1 {
			retry = nil // the final answer streams through, 404 included
		}
		last = r.forward(w, req, u, class, retry)
		if last.HeadersSent || req.Context().Err() != nil {
			return
		}
		if last.Status == 404 {
			continue
		}
		if !last.Failed() {
			return
		}
	}
	r.answerFailure(w, req, last)
}

func sortByRoundDesc(us []domain.Upstream) {
	for i := 1; i < len(us); i++ {
		for j := i; j > 0 && us[j].LastRound > us[j-1].LastRound; j-- {
			us[j], us[j-1] = us[j-1], us[j]
		}
	}
}

// serveAgent answers /loadb/* from the agent itself.
func (r *Router) serveAgent(w http.ResponseWriter, req *http.Request) {
	cands, best := r.dir.Snapshot()
	switch strings.TrimSuffix(req.URL.Path, "/") {
	case "/loadb/health":
		if r.draining.Load() {
			writeMessage(w, 503, "draining")
			return
		}
		for _, u := range cands {
			if domain.Eligible(u, domain.RequestClass{}, best, r.opts.SyncTolerance) {
				writeJSON(w, 200, map[string]any{"status": "ok", "best_round": best})
				return
			}
		}
		writeMessage(w, 503, "no eligible upstream")
	case "/loadb/status":
		local := r.monitor.State()
		writeJSON(w, 200, map[string]any{
			"version": r.opts.Version, "mode": r.opts.Mode, "draining": r.draining.Load(),
			"uptime_s": int(r.clock.Now().Sub(r.started).Seconds()), "inflight": r.inflight.Load(),
			"best_round": best, "local": local, "upstreams": cands,
		})
	case "/loadb/peers":
		writeJSON(w, 200, cands)
	case "/loadb/metrics":
		if r.metrics == nil {
			writeMessage(w, 404, "metrics disabled")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = r.metrics.WriteTo(w)
	default:
		writeMessage(w, 404, "unknown agent endpoint")
	}
}

// captureWriter tees the first `limit` bytes of a response for inspection.
type captureWriter struct {
	http.ResponseWriter
	buf   bytes.Buffer
	limit int
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.buf.Len() < c.limit {
		n := c.limit - c.buf.Len()
		if n > len(b) {
			n = len(b)
		}
		c.buf.Write(b[:n])
	}
	return c.ResponseWriter.Write(b)
}

func (c *captureWriter) Flush() {
	if fl, ok := c.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

// statusWriter records the status and body size sent to the client.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 && code >= 200 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusWriter) Flush() {
	if fl, ok := s.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
