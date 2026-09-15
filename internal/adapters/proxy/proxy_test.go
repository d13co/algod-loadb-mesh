package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

func TestForwardStreamsAndSetsToken(t *testing.T) {
	var gotToken, gotHost string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotHost = r.Header.Get("X-Algo-API-Token"), r.Host
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Up", "1")
		w.WriteHeader(201)
		w.Write([]byte("echo:" + string(body)))
	}))
	defer up.Close()
	f := New(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v2/transactions?x=1", strings.NewReader("payload"))
	out := f.Forward(rec, req, ports.Target{BaseURL: up.URL, Token: "tok"})
	if out.Err != nil || out.Status != 201 || !out.HeadersSent {
		t.Fatalf("outcome %+v", out)
	}
	if rec.Code != 201 || rec.Body.String() != "echo:payload" || rec.Header().Get("X-Up") != "1" {
		t.Fatalf("response %d %q", rec.Code, rec.Body.String())
	}
	if gotToken != "tok" || gotHost != strings.TrimPrefix(up.URL, "http://") {
		t.Fatalf("token %q host %q", gotToken, gotHost)
	}
}

func TestForwardRetryableStatusIsNotStreamed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		w.Write([]byte("down"))
	}))
	defer up.Close()
	f := New(nil)
	rec := httptest.NewRecorder()
	out := f.Forward(rec, httptest.NewRequest("GET", "/v2/status", nil), ports.Target{BaseURL: up.URL})
	if out.Status != 503 || !out.Failed() || out.HeadersSent {
		t.Fatalf("outcome %+v", out)
	}
	if rec.Body.Len() != 0 {
		t.Fatal("503 body must not reach the client")
	}
	// A per-target retry status (404 for pending lookups) behaves the same.
	up404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	defer up404.Close()
	rec = httptest.NewRecorder()
	out = f.Forward(rec, httptest.NewRequest("GET", "/x", nil), ports.Target{BaseURL: up404.URL, RetryStatus: map[int]bool{404: true}})
	if out.Status != 404 || out.HeadersSent {
		t.Fatalf("outcome %+v", out)
	}
	rec = httptest.NewRecorder()
	out = f.Forward(rec, httptest.NewRequest("GET", "/x", nil), ports.Target{BaseURL: up404.URL})
	if out.Status != 404 || !out.HeadersSent || rec.Code != 404 {
		t.Fatalf("plain 404 must stream: %+v", out)
	}
}

func TestForwardTransportError(t *testing.T) {
	f := New(nil)
	rec := httptest.NewRecorder()
	out := f.Forward(rec, httptest.NewRequest("GET", "/v2/status", nil), ports.Target{BaseURL: "http://127.0.0.1:1"})
	if out.Err == nil || out.HeadersSent || out.Status != 0 {
		t.Fatalf("outcome %+v", out)
	}
}
