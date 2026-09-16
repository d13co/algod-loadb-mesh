// Package deploy embeds the example configurations so the binary can print
// them (`algod-loadb-mesh config example`).
package deploy

import _ "embed"

// ConfigExample is an agent config using the on-chain registry.
//
//go:embed config.example.yaml
var ConfigExample string

// ConfigStaticExample is an agent config with a static peer list.
//
//go:embed config.static.example.yaml
var ConfigStaticExample string

// ConfigBalancerExample is a balancer config: an agent with no algod.
//
//go:embed config.balancer.example.yaml
var ConfigBalancerExample string
