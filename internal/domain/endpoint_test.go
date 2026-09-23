package domain

import (
	"reflect"
	"testing"
)

func TestEndpointHosts(t *testing.T) {
	got := EndpointHosts([]string{"http://10.112.0.5:8080", "http://[fd00::5]:8080", "http://[::ffff:10.114.0.5]:8080", "mem:a", "http://node.example:8080", "::bad"})
	want := []string{"10.112.0.5", "fd00::5", "10.114.0.5", "", "", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestEndpointFor(t *testing.T) {
	eps := []string{"http://10.112.0.5:8080", "http://10.114.0.5:8080"}
	hosts := EndpointHosts(eps)
	cases := []struct{ path, want string }{
		{"10.114.0.5:4001", eps[1]},          // matching host wins regardless of position
		{"10.112.0.5:4001", eps[0]},          // first also matches by host
		{"[::ffff:10.114.0.5]:4001", eps[0]}, // path addresses are already normalised; a raw mapped form does not match
		{"10.9.9.9:4001", eps[0]},            // no match: first
		{"mem:x", eps[0]},                    // not host:port: first
		{"", eps[0]},                         // no path: first
	}
	for _, c := range cases {
		if got := EndpointFor(eps, hosts, c.path); got != c.want {
			t.Errorf("path %q: got %q want %q", c.path, got, c.want)
		}
	}
	if got := EndpointFor(nil, nil, "10.114.0.5:4001"); got != "" {
		t.Errorf("empty: got %q", got)
	}
	// Host-only matching: two endpoints on one host always yield the first.
	same := []string{"http://10.114.0.5:8080", "http://10.114.0.5:8081"}
	if got := EndpointFor(same, EndpointHosts(same), "10.114.0.5:4001"); got != same[0] {
		t.Errorf("same host: got %q", got)
	}
	// A normalised path matches an endpoint given in mapped form.
	mapped := []string{"http://10.112.0.5:8080", "http://[::ffff:10.114.0.5]:8080"}
	if got := EndpointFor(mapped, EndpointHosts(mapped), "10.114.0.5:4001"); got != mapped[1] {
		t.Errorf("mapped endpoint: got %q", got)
	}
}

func TestAlgodCovers(t *testing.T) {
	cases := []struct {
		algod, addr string
		want        bool
	}{
		{"10.114.0.5:8080", "10.114.0.5:8080", true},
		{"0.0.0.0:8080", "10.114.0.5:8080", true},
		{"[::]:8080", "10.114.0.5:8080", true},
		{"[::]:8080", "[fd00::5]:8080", true},
		{":8080", "[fd00::5]:8080", true},
		{"0.0.0.0:8080", "[fd00::5]:8080", false},
		{"0.0.0.0:8080", "10.114.0.5:8081", false},
		{"127.0.0.1:8080", "10.114.0.5:8080", false},
		{"127.0.0.1:8080", "127.0.0.1:8080", true},
		{"[::ffff:10.114.0.5]:8080", "10.114.0.5:8080", true},
		{"10.114.0.5:8080", "[::ffff:10.114.0.5]:8080", true},
		{"garbage", "10.114.0.5:8080", false},
		{"0.0.0.0:8080", "garbage", false},
		{"0.0.0.0:8080", "node.local:8080", true},
		{"[::]:8080", "node.local:8080", true},
		{"10.114.0.5:8080", "node.local:8080", false},
	}
	for _, c := range cases {
		if got := AlgodCovers(c.algod, c.addr); got != c.want {
			t.Errorf("AlgodCovers(%q, %q) = %v want %v", c.algod, c.addr, got, c.want)
		}
	}
}
