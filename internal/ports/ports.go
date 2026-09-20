// Package ports declares the interfaces the application services depend on.
// Adapters implement them; tests use fakes. Services never import adapters.
package ports

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/domain"
)

// Status is the subset of algod's /v2/status the agent uses.
type Status struct {
	LastRound         uint64 `json:"last-round"`
	CatchupTime       uint64 `json:"catchup-time"`
	TimeSinceLastMS   uint64 `json:"time-since-last-round"`
	CatchpointRunning bool   `json:"-"`
	Raw               []byte `json:"-"` // verbatim body, replayed to waiters
}

// Versions is the subset of /versions the agent uses.
type Versions struct {
	GenesisID string
	Build     string
}

// AlgodClient talks to one algod REST endpoint.
type AlgodClient interface {
	Status(ctx context.Context) (Status, error)
	// WaitForBlockAfter long-polls /v2/status/wait-for-block-after/{round}.
	WaitForBlockAfter(ctx context.Context, round uint64) (Status, error)
	// HasBlock reports whether the node can serve the given round (cheap probe).
	HasBlock(ctx context.Context, round uint64) (bool, error)
	Versions(ctx context.Context) (Versions, error)
	// BoxNames lists the box keys of an application.
	BoxNames(ctx context.Context, appID uint64) ([][]byte, error)
	// Box returns a box value; ErrNotFound when absent.
	Box(ctx context.Context, appID uint64, name []byte) ([]byte, error)
}

// AlgodClientFactory builds clients for arbitrary endpoints (peers, externals).
type AlgodClientFactory interface {
	NewAlgodClient(baseURL, token string) AlgodClient
}

// NodeConfig is what the local data directory reveals about the node.
type NodeConfig struct {
	Endpoint                string // http://host:port from algod.net
	Token                   string
	GenesisID               string
	Archival                bool
	MaxBlockHistoryLookback uint64
	EnableDeveloperAPI      bool
	EnableFollowMode        bool
	EnableExperimentalAPI   bool
	MaxAcctLookback         uint64
	StorageEngine           string
	RestWriteTimeoutSeconds int
	ModTime                 time.Time // config.json mtime, to detect edits
}

// NodeConfigReader reads the local node's configuration.
type NodeConfigReader interface {
	Read() (NodeConfig, error)
}

// Registry is the static fleet directory.
type Registry interface {
	// List returns every readable record and the round the read reflects.
	List(ctx context.Context) ([]domain.NodeRecord, uint64, error)
	Put(ctx context.Context, rec domain.NodeRecord) error
	Delete(ctx context.Context, id string) error
}

// RegistryCache persists the last good registry read for boot without algod.
type RegistryCache interface {
	Load() ([]domain.NodeRecord, error)
	Save([]domain.NodeRecord) error
}

// GossipMessage is one datagram from a peer. From is the source address the
// transport saw: the path the datagram arrived on and where a reply goes,
// never an identity.
type GossipMessage struct {
	From    string
	Payload []byte
}

// Gossip is the control-plane transport: fire-and-forget datagrams. Path
// choice is policy and lives in the application; the transport only sends.
type Gossip interface {
	// Send hands one datagram to the network. A nil error means it was sent,
	// not that it arrived: a black-holed peer still gets nil, liveness is the
	// receiver's judgement. ErrNoRoute means the address is unusable from this
	// host and the caller should try another.
	Send(ctx context.Context, addr string, payload []byte) error
	Receive() <-chan GossipMessage
	Close() error
}

// ErrNoRoute is returned by Gossip.Send for an address this host cannot
// reach: unparseable, or on a network it has no interface on.
var ErrNoRoute = errors.New("gossip: no route")

// Target is where the forwarder sends one request.
type Target struct {
	BaseURL string
	Token   string
	// RetryStatus lists upstream statuses that must not be streamed to the
	// client because the caller wants to try another upstream (e.g. 404 on
	// a pending-transaction lookup). 502/503/504 are always in this set.
	RetryStatus map[int]bool
}

// Outcome is what happened when forwarding.
type Outcome struct {
	Status      int           // upstream status, 0 on transport error
	Err         error         // transport or upstream-rejection error
	HeadersSent bool          // true once anything reached the client
	Duration    time.Duration // as measured by the forwarder
}

// Failed is true when the outcome should count against the upstream.
func (o Outcome) Failed() bool {
	return o.Err != nil || o.Status >= 500 || o.Status == 429
}

// Forwarder streams a client request to a target and the response back.
type Forwarder interface {
	Forward(w http.ResponseWriter, r *http.Request, t Target) Outcome
}

// Clock abstracts time for services.
type Clock interface {
	Now() time.Time
	// Sleep returns early with ctx.Err() when ctx is done.
	Sleep(ctx context.Context, d time.Duration) error
	// Tick delivers on a period until stop is called.
	Tick(d time.Duration) (ch <-chan time.Time, stop func())
}

// Metrics is the minimal instrumentation surface services use.
type Metrics interface {
	Inc(name string, labels ...string)
	Observe(name string, value float64, labels ...string)
	Gauge(name string, value float64, labels ...string)
}

// Logger is the structured logger services write to.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
	Debug(msg string, kv ...any)
}

// ErrNotFound is returned by Box when the box does not exist.
type notFound struct{}

func (notFound) Error() string { return "not found" }

// ErrNotFound is the sentinel adapters return for a missing box or block.
var ErrNotFound error = notFound{}
