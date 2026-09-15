// Command algod-loadb-mesh is the mesh load balancer agent for algod.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/algorand/go-algorand-sdk/v2/crypto"
	"github.com/algorand/go-algorand-sdk/v2/mnemonic"

	"github.com/d13co/algod-loadb-mesh/deploy"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/algodhttp"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/datadir"
	"github.com/d13co/algod-loadb-mesh/internal/adapters/registryalgo"
	"github.com/d13co/algod-loadb-mesh/internal/agent"
	"github.com/d13co/algod-loadb-mesh/internal/app"
	"github.com/d13co/algod-loadb-mesh/internal/config"
	"github.com/d13co/algod-loadb-mesh/internal/devfleet"
	"github.com/d13co/algod-loadb-mesh/internal/domain"
)

const usage = `algod-loadb-mesh - mesh load balancer for algod

usage:
  algod-loadb-mesh run        -config FILE          run the agent
  algod-loadb-mesh config     example [-static] [-o FILE] [-force]  print an example config
  algod-loadb-mesh check-node -data-dir DIR [-probe] report the local node's capabilities
  algod-loadb-mesh registry   gen-key               create a sync account (mnemonic + address)
  algod-loadb-mesh registry   init  -config FILE    deploy the registry app with the sync account
  algod-loadb-mesh registry   list  -config FILE [-show-tokens]
  algod-loadb-mesh registry   add   -config FILE -id ID -network NET -endpoint URL -token T -agent ADDR [-tier N]
  algod-loadb-mesh registry   rm    -config FILE -id ID
  algod-loadb-mesh dev        [-nodes N] [-mode M]  run a fake fleet in-process
  algod-loadb-mesh version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "config":
		err = configCmd(os.Args[2:])
	case "check-node":
		err = checkNodeCmd(os.Args[2:])
	case "registry":
		err = registryCmd(os.Args[2:])
	case "dev":
		err = devCmd(os.Args[2:])
	case "version":
		fmt.Println("algod-loadb-mesh", agent.Version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", "/etc/algod-loadb-mesh/config.yaml", "config file")
	_ = fs.Parse(args)
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	deps, err := agent.FromConfig(cfg)
	if err != nil {
		return err
	}
	a, err := agent.New(deps)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	return a.Serve(ctx)
}

func configCmd(args []string) error {
	if len(args) == 0 || args[0] != "example" {
		return fmt.Errorf("config: subcommand required (example)")
	}
	fs := flag.NewFlagSet("config example", flag.ExitOnError)
	static := fs.Bool("static", false, "static peer list instead of the on-chain registry")
	out := fs.String("o", "", "write to FILE instead of stdout")
	force := fs.Bool("force", false, "overwrite FILE if it exists")
	_ = fs.Parse(args[1:])
	text := deploy.ConfigExample
	if *static {
		text = deploy.ConfigStaticExample
	}
	if *out == "" {
		_, err := fmt.Print(text)
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	// 0600: the file will hold tokens once filled in.
	f, err := os.OpenFile(*out, flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists (use -force to overwrite)", *out)
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "wrote", *out)
	return nil
}

func checkNodeCmd(args []string) error {
	fs := flag.NewFlagSet("check-node", flag.ExitOnError)
	dir := fs.String("data-dir", "/var/lib/algorand", "algod data directory")
	probe := fs.Bool("probe", false, "also measure the oldest servable round (log2(rounds) requests)")
	_ = fs.Parse(args)
	nc, err := datadir.Reader{Dir: *dir}.Read()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := algodhttp.New(nc.Endpoint, nc.Token, nil)
	st, err := client.Status(ctx)
	if err != nil {
		return fmt.Errorf("node not reachable at %s: %w", nc.Endpoint, err)
	}
	report := map[string]any{"endpoint": nc.Endpoint, "genesis_id": nc.GenesisID, "last_round": st.LastRound,
		"archival": nc.Archival, "max_block_history_lookback": nc.MaxBlockHistoryLookback,
		"developer_api": nc.EnableDeveloperAPI, "follow_mode": nc.EnableFollowMode,
		"experimental_api": nc.EnableExperimentalAPI, "storage_engine": nc.StorageEngine}
	var arch domain.Archival
	switch {
	case nc.Archival:
		arch = domain.Archival{Kind: domain.ArchivalFull}
	case nc.MaxBlockHistoryLookback > 0:
		arch = domain.Archival{Kind: domain.ArchivalTrailing, N: nc.MaxBlockHistoryLookback}
	default:
		arch = domain.Archival{Kind: domain.ArchivalNone}
	}
	report["configured_oldest_round"] = domain.OldestRoundFor(arch, st.LastRound)
	if v, err := client.Versions(ctx); err == nil {
		report["algod_version"] = v.Build
	}
	if *probe {
		oldest, err := app.ProbeOldestRound(ctx, client, st.LastRound)
		if err != nil {
			return err
		}
		report["measured_oldest_round"] = oldest
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func registryCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("registry: subcommand required (gen-key, init, list, add, rm)")
	}
	sub, rest := args[0], args[1:]
	if sub == "gen-key" {
		acct := crypto.GenerateAccount()
		mn, err := mnemonic.FromPrivateKey(acct.PrivateKey)
		if err != nil {
			return err
		}
		fmt.Printf("address:  %s\nmnemonic: %s\n\nFund the address (a few ALGO) and put the mnemonic in registry.sync_key_file.\nIf you rekey an existing account to this key instead, also set registry.sync_address.\n", acct.Address, mn)
		return nil
	}
	fs := flag.NewFlagSet("registry "+sub, flag.ExitOnError)
	path := fs.String("config", "/etc/algod-loadb-mesh/config.yaml", "config file")
	id := fs.String("id", "", "node id")
	network := fs.String("network", "", "genesis id, e.g. mainnet-v1.0")
	endpoint := fs.String("endpoint", "", "algod REST base URL (repeatable with commas)")
	token := fs.String("token", "", "algod API token")
	agentAddr := fs.String("agent", "", "agent gossip address host:port")
	tier := fs.Int("tier", 1, "tier")
	showTokens := fs.Bool("show-tokens", false, "print algod tokens")
	_ = fs.Parse(rest)

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	seed, err := cfg.SyncSeed()
	if err != nil {
		return err
	}
	if seed == nil {
		return fmt.Errorf("registry.sync_key is required")
	}
	url, tok := cfg.Registry.AlgodURL, cfg.Registry.AlgodToken
	if url == "" {
		nc, err := datadir.Reader{Dir: cfg.Local.DataDir}.Read()
		if err != nil {
			return fmt.Errorf("registry.algod_url not set and local node unavailable: %w", err)
		}
		url, tok = nc.Endpoint, nc.Token
	}
	reader := algodhttp.New(url, tok, nil)
	reg, err := registryalgo.New(cfg.Registry.AppID, seed, cfg.Registry.SyncAddress, reader, url, tok, agent.Logger(cfg))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if sub != "list" {
		if err := reg.CheckAuth(ctx); err != nil {
			return err
		}
	}
	switch sub {
	case "init":
		appID, err := reg.Create(ctx)
		if err != nil {
			return err
		}
		acct := reg.Sender()
		if reg.Signer() != acct {
			acct += ", rekeyed to " + reg.Signer()
		}
		fmt.Printf("registry app created: %d (sync account %s)\nSet registry.app_id: %d on every host.\n", appID, acct, appID)
	case "list":
		recs, round, err := reg.List(ctx)
		if err != nil {
			return err
		}
		for i := range recs {
			if !*showTokens {
				recs[i].Token = mask(recs[i].Token)
			}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"round": round, "records": recs})
	case "add":
		if *id == "" || *network == "" || *endpoint == "" {
			return fmt.Errorf("-id, -network and -endpoint are required")
		}
		material, _ := cfg.KeyMaterial()
		key, err := agent.AgentKey(material, *id)
		if err != nil {
			return err
		}
		rec := domain.NodeRecord{ID: *id, Network: *network, Endpoints: strings.Split(*endpoint, ","), Token: *token,
			Agent: domain.AgentInfo{Addr: *agentAddr, PubKey: []byte(key.Public().(ed25519.PublicKey))}, Tier: *tier, Version: 1}
		if err := reg.Put(ctx, rec); err != nil {
			return err
		}
		fmt.Println("added", *id)
	case "rm":
		if *id == "" {
			return fmt.Errorf("-id is required")
		}
		if err := reg.Delete(ctx, *id); err != nil {
			return err
		}
		fmt.Println("removed", *id)
	default:
		return fmt.Errorf("registry: unknown subcommand %q", sub)
	}
	return nil
}

func mask(s string) string {
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func devCmd(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	n := fs.Int("nodes", 3, "number of fake nodes")
	mode := fs.String("mode", "fallback", "fallback | loadbalancer")
	roundTime := fs.Duration("round-time", 2800*time.Millisecond, "fake block time")
	_ = fs.Parse(args)
	var specs []devfleet.NodeSpec
	for i := 0; i < *n; i++ {
		// dev1 is archival, the last node has the developer API, the rest
		// are plain nodes keeping the last 1000 rounds.
		spec := devfleet.NodeSpec{ID: fmt.Sprintf("dev%d", i+1), StartRound: 5000, Archival: i == 0, DeveloperAPI: i == *n-1, Tier: 1}
		if i > 0 {
			spec.OldestRound = 4000
		}
		specs = append(specs, spec)
	}
	ctx, cancel := signalContext()
	defer cancel()
	cfg := config.Config{Log: config.Log{Level: "debug"}}
	f, err := devfleet.Start(ctx, devfleet.Options{Nodes: specs, Mode: *mode, Log: agent.Logger(cfg),
		KeepAlive: 2 * time.Second, SuspectAfter: 10 * time.Second, ProbeInterval: 5 * time.Second, RegistryRefresh: 5 * time.Second,
		ReturnHysteresis: 3})
	if err != nil {
		return err
	}
	defer f.Close()
	for i, s := range f.Servers {
		fmt.Printf("agent %-6s %s   (fake algod %s, token %s)\n", f.Nodes[i].ID(), s.URL, f.Nodes[i].URL(), f.Nodes[i].Token())
	}
	fmt.Println("rounds advance every", *roundTime, "- try: curl", f.Servers[0].URL+"/loadb/status")
	t := time.NewTicker(*roundTime)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			f.Advance(1)
		}
	}
}
