// Package proxy implements ports.Forwarder over httputil.ReverseProxy:
// streaming both ways, no buffering, request bodies passed through untouched.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Forwarder holds the shared transport.
type Forwarder struct {
	transport http.RoundTripper
	// RetryableStatus lists upstream statuses treated as failures before any
	// byte is sent to the client, so the router may try another upstream.
	RetryableStatus map[int]bool
}

// New builds a forwarder. rt may be nil for a default transport.
func New(rt http.RoundTripper) *Forwarder {
	if rt == nil {
		rt = &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 0, // the router bounds requests with its context
			ForceAttemptHTTP2:     false,
		}
	}
	return &Forwarder{transport: rt, RetryableStatus: map[int]bool{502: true, 503: true, 504: true}}
}

type upstreamRejected struct{ status int }

func (e upstreamRejected) Error() string { return fmt.Sprintf("upstream status %d", e.status) }

// Forward implements ports.Forwarder.
func (f *Forwarder) Forward(w http.ResponseWriter, r *http.Request, t ports.Target) ports.Outcome {
	start := time.Now()
	target, err := url.Parse(t.BaseURL)
	if err != nil {
		return ports.Outcome{Err: err, Duration: time.Since(start)}
	}
	tw := &trackingWriter{ResponseWriter: w}
	out := ports.Outcome{}
	rp := &httputil.ReverseProxy{
		Transport: f.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Header.Set("X-Algo-API-Token", t.Token)
			pr.Out.Header.Del("X-Forwarded-For")
		},
		ModifyResponse: func(resp *http.Response) error {
			out.Status = resp.StatusCode
			if f.RetryableStatus[resp.StatusCode] || t.RetryStatus[resp.StatusCode] {
				resp.Body.Close()
				return upstreamRejected{resp.StatusCode}
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			out.Err = err
			// Leave the response untouched: the router decides whether to retry
			// elsewhere or to answer the client itself.
		},
		FlushInterval: -1, // stream immediately (wait-for-block, big blocks)
	}
	rp.ServeHTTP(tw, r)
	out.HeadersSent = tw.wrote
	out.Duration = time.Since(start)
	if out.Err != nil {
		var rej upstreamRejected
		if errors.As(out.Err, &rej) {
			out.Status = rej.status
		} else if !out.HeadersSent {
			out.Status = 0
		}
	}
	return out
}

// IsTimeout reports whether the outcome was a client/context timeout rather
// than an upstream failure.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	return strings.Contains(err.Error(), "context canceled")
}

type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

// Flush lets ReverseProxy stream through us.
func (t *trackingWriter) Flush() {
	if fl, ok := t.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// Unwrap supports http.ResponseController.
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
