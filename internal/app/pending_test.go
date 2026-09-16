package app

import (
	"errors"
	"net/http"
	"testing"

	"github.com/algorand/go-algorand-sdk/v2/encoding/msgpack"
)

func TestPendingRank(t *testing.T) {
	answer := func(status int, ct string, body []byte) fanResult {
		return fanResult{resp: &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {ct}}}, body: body}
	}
	mp := func(v map[string]any) []byte { return msgpack.Encode(v) }
	cases := []struct {
		name string
		res  fanResult
		want int
	}{
		{"transport error", fanResult{err: errors.New("boom")}, pendingFailed},
		{"503", answer(503, "application/json", nil), pendingFailed},
		{"400", answer(400, "application/json", []byte(`{"message":"bad"}`)), pendingOther},
		{"404", answer(404, "application/json", []byte(`{"message":"not found"}`)), pendingNotFound},
		{"json in pool", answer(200, "application/json", []byte(`{"pool-error":"","txn":{}}`)), pendingInPool},
		{"json pool error", answer(200, "application/json", []byte(`{"pool-error":"overspend","txn":{}}`)), pendingPoolError},
		{"json confirmed", answer(200, "application/json", []byte(`{"confirmed-round":12,"pool-error":"","txn":{}}`)), pendingConfirmed},
		{"msgpack pool error", answer(200, "application/msgpack", mp(map[string]any{"pool-error": "overspend", "txn": map[string]any{}})), pendingPoolError},
		{"msgpack confirmed", answer(200, "application/msgpack", mp(map[string]any{"confirmed-round": uint64(12), "pool-error": "", "txn": map[string]any{}})), pendingConfirmed},
		{"unreadable 200", answer(200, "application/json", []byte(`nope`)), pendingInPool},
	}
	for _, c := range cases {
		if got := pendingRank(c.res); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, pendingAnswer[got], pendingAnswer[c.want])
		}
	}
}
