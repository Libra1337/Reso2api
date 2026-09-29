package upstream

import (
	"net/http"
	"testing"
)

func TestShardedTransportStablePerUID(t *testing.T) {
	st := newShardedTransport(4)
	seen := map[*http.Transport]bool{}
	for _, uid := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"} {
		p := st.pick(uid)
		if st.pick(uid) != p {
			t.Fatalf("uid %s not stable", uid)
		}
		seen[p] = true
	}
	if len(seen) < 2 {
		t.Fatalf("uids not spread across shards: %d", len(seen))
	}
	if st.pick("") != st.shards[0] {
		t.Fatalf("empty uid should use shard 0")
	}
}

func TestStreamTransportHealthCheck(t *testing.T) {
	tr := newStreamTransport()
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 || tr.HTTP2.PingTimeout <= 0 {
		t.Fatalf("http2 ping health check must be enabled")
	}
	if tr.ResponseHeaderTimeout != streamResponseHeaderTimeout || tr.Proxy == nil {
		t.Fatalf("unexpected transport config")
	}
}
