package domain

import (
	"net"
	"net/url"
)

// EndpointHosts returns, per advertised endpoint, its host normalised the way
// NormalizeAddr normalises (IPv4-mapped IPv6 becomes IPv4); "" for an
// endpoint whose host is not an IP (mem:x, hostnames). It is computed once
// per record so that EndpointFor never parses on the request path.
func EndpointHosts(endpoints []string) []string {
	hosts := make([]string, len(endpoints))
	for i, e := range endpoints {
		u, err := url.Parse(e)
		if err != nil {
			continue
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil {
			continue
		}
		if ip4 := ip.To4(); ip4 != nil {
			ip = ip4
		}
		hosts[i] = ip.String()
	}
	return hosts
}

// EndpointFor picks the endpoint whose precomputed host (EndpointHosts) is
// the host of pathAddr, the link's current heartbeat path in normalised
// host:port form, else endpoints[0]; "" when there are none. Matching is by
// host only: the path carries the gossip port, the endpoint the REST port, so
// two endpoints on one host always yield the first. hosts must be
// EndpointHosts(endpoints).
func EndpointFor(endpoints, hosts []string, pathAddr string) string {
	if len(endpoints) == 0 {
		return ""
	}
	if host, _, err := net.SplitHostPort(pathAddr); err == nil && host != "" {
		for i, h := range hosts {
			if h == host && i < len(endpoints) {
				return endpoints[i]
			}
		}
	}
	return endpoints[0]
}

// AlgodCovers reports whether an algod bound to algodNet (host:port verbatim
// from algod.net) already answers on addr, so the agent must not bind it: a
// wildcard covers its family (0.0.0.0 IPv4 only, [::] and a bare port both,
// as a dual-stack listener does), anything else covers only itself. Ports
// must be equal. A hostname counts as covered by 0.0.0.0 as well: whatever
// IPv4 it resolves to, the wildcard answers there, and skipping is the safe
// side of the port conflict.
func AlgodCovers(algodNet, addr string) bool {
	ah, ap, err := net.SplitHostPort(algodNet)
	if err != nil {
		return false
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil || p != ap {
		return false
	}
	ip := net.ParseIP(h)
	switch ah {
	case "", "::":
		return true
	case "0.0.0.0":
		return ip == nil || ip.To4() != nil
	}
	aip := net.ParseIP(ah)
	if aip == nil || ip == nil {
		return ah == h
	}
	return aip.Equal(ip)
}
