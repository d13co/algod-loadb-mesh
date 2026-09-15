// Package datadir reads the local algod data directory: algod.net, algod.token,
// genesis.json and config.json. Only the config keys the agent uses are
// parsed; defaults match go-algorand's config/defaults for those keys.
package datadir

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Reader implements ports.NodeConfigReader for a data directory.
type Reader struct {
	Dir string
}

// defaults for the keys we parse (go-algorand config v34).
var defaults = configFile{
	Archival:                false,
	MaxBlockHistoryLookback: 0,
	EnableDeveloperAPI:      false,
	EnableFollowMode:        false,
	EnableExperimentalAPI:   false,
	MaxAcctLookback:         8,
	StorageEngine:           "sqlite",
	RestWriteTimeoutSeconds: 120,
	EndpointAddress:         "127.0.0.1:0",
}

type configFile struct {
	Archival                bool   `json:"Archival"`
	MaxBlockHistoryLookback uint64 `json:"MaxBlockHistoryLookback"`
	EnableDeveloperAPI      bool   `json:"EnableDeveloperAPI"`
	EnableFollowMode        bool   `json:"EnableFollowMode"`
	EnableExperimentalAPI   bool   `json:"EnableExperimentalAPI"`
	MaxAcctLookback         uint64 `json:"MaxAcctLookback"`
	StorageEngine           string `json:"StorageEngine"`
	RestWriteTimeoutSeconds int    `json:"RestWriteTimeoutSeconds"`
	EndpointAddress         string `json:"EndpointAddress"`
}

// Read implements ports.NodeConfigReader.
func (r Reader) Read() (ports.NodeConfig, error) {
	var nc ports.NodeConfig
	if r.Dir == "" {
		return nc, errors.New("datadir: empty directory")
	}
	netAddr, err := readTrimmed(filepath.Join(r.Dir, "algod.net"))
	if err != nil {
		return nc, fmt.Errorf("datadir: %w (is algod running?)", err)
	}
	nc.Endpoint = endpointURL(netAddr)
	if nc.Token, err = readTrimmed(filepath.Join(r.Dir, "algod.token")); err != nil {
		return nc, fmt.Errorf("datadir: %w", err)
	}
	gen, err := os.ReadFile(filepath.Join(r.Dir, "genesis.json"))
	if err != nil {
		return nc, fmt.Errorf("datadir: %w", err)
	}
	var g struct {
		Network string `json:"network"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(gen, &g); err != nil {
		return nc, fmt.Errorf("datadir: genesis.json: %w", err)
	}
	nc.GenesisID = g.Network + "-" + g.ID

	cf := defaults
	cfgPath := filepath.Join(r.Dir, "config.json")
	if st, err := os.Stat(cfgPath); err == nil {
		nc.ModTime = st.ModTime()
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			return nc, fmt.Errorf("datadir: %w", err)
		}
		if err := json.Unmarshal(raw, &cf); err != nil {
			return nc, fmt.Errorf("datadir: config.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nc, fmt.Errorf("datadir: %w", err)
	}
	nc.Archival = cf.Archival
	nc.MaxBlockHistoryLookback = cf.MaxBlockHistoryLookback
	nc.EnableDeveloperAPI = cf.EnableDeveloperAPI
	nc.EnableFollowMode = cf.EnableFollowMode
	nc.EnableExperimentalAPI = cf.EnableExperimentalAPI
	nc.MaxAcctLookback = cf.MaxAcctLookback
	nc.StorageEngine = cf.StorageEngine
	nc.RestWriteTimeoutSeconds = cf.RestWriteTimeoutSeconds
	return nc, nil
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// endpointURL turns algod.net's "host:port" into a base URL. algod writes
// "0.0.0.0:port" or "[::]:port" when listening on all interfaces; loopback is
// the right way for the co-located agent to reach it.
func endpointURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	host, port, ok := strings.Cut(addr, ":")
	if i := strings.LastIndex(addr, ":"); i > 0 {
		host, port, ok = addr[:i], addr[i+1:], true
	}
	if !ok {
		return "http://" + addr
	}
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + port
}
