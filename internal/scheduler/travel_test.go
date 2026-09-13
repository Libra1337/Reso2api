package scheduler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// travelTestServer 构造带 mock 上游的调度器；限速间隔置 0 提速测试。
func travelTestServer(t *testing.T, handler http.HandlerFunc) (*Scheduler, *pool.Pool) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	prev, prevGap := travelAccountDelay, activityReportGap
	travelAccountDelay, activityReportGap = 0, 0
	t.Cleanup(func() { travelAccountDelay, activityReportGap = prev, prevGap })
	return New(Config{Pool: p, Upstream: up, Name: "workbuddy"}), p
}

func envelope(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"code":0,"msg":"ok","data":` + data + `}`))
}

// 无猫 -> 协议 + 领养闭环。
func TestTravelAdoptsWhenNoBuddy(t *testing.T) {
	var calls int64
	s, _ := travelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/buddy/info"):
			envelope(w, `{"buddy":null}`)
		case strings.HasSuffix(r.URL.Path, "/buddy/agreement"):
			envelope(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/buddy/first"):
			envelope(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			// 领养收益入账后的余额回读（refreshCredits）
			envelope(w, `{"Response":{"Data":{"Accounts":[{"PackageName":"pkg","CapacityRemain":2439,"CapacitySize":2439}]}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	s.RunTravelNow()
	// info+agreement+first+resource（领养后立即回读余额）
	if n := atomic.LoadInt64(&calls); n != 4 {
		t.Fatalf("calls=%d want 4 (info+agreement+first+resource)", n)
	}
}

// 有猫 + 到站 -> 领奖带 record_id。
func TestTravelClaimsArrived(t *testing.T) {
	var claimed map[string]any
	s, _ := travelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/buddy/info"):
			envelope(w, `{"buddy":{"id":1,"name":"cat"}}`)
		case strings.HasSuffix(r.URL.Path, "/travel/status"):
			envelope(w, `{"state":"arrived","record_id":42,"reward_credit":15}`)
		case strings.HasSuffix(r.URL.Path, "/travel/claim"):
			_ = json.NewDecoder(r.Body).Decode(&claimed)
			envelope(w, `{"reward_credit":15}`)
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			envelope(w, `{"Response":{"Data":{"Accounts":[{"PackageName":"pkg","CapacityRemain":2154,"CapacitySize":2154}]}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	s.RunTravelNow()
	if claimed["record_id"] != float64(42) {
		t.Fatalf("claim record_id=%v want 42", claimed["record_id"])
	}
}

// 活跃上报 5 连发：同会话 5 条独立 requestId；发满后回读 streak。
func TestActivityReportsFiveAndChecksStreak(t *testing.T) {
	var reports int64
	s, _ := travelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/report":
			atomic.AddInt64(&reports, 1)
			envelope(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/growth/streak"):
			envelope(w, `{"streak":{"days":3}}`)
		case strings.HasSuffix(r.URL.Path, "/buddy/info"):
			envelope(w, `{"buddy":{"id":1,"name":"cat"}}`)
		default:
			envelope(w, `{}`)
		}
	})
	s.RunActivityNow()
	if n := atomic.LoadInt64(&reports); n != 5 {
		t.Fatalf("reports=%d want 5", n)
	}
}

// bareUpstream 只实现 provider.Upstream 最小面（模拟 traework/qoder：无旅行能力）。
type bareUpstream struct{}

func (bareUpstream) RefreshToken(*auth.Auth) error { return nil }
func (bareUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, nil
}
func (bareUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (bareUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (bareUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (bareUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (bareUpstream) DailyCheckin(*auth.Auth) error               { return nil }
func (bareUpstream) Classify(int, string) provider.ErrKind       { return provider.ErrNone }
func (bareUpstream) Stream(http.ResponseWriter, io.Reader) error { return nil }
func (bareUpstream) Aggregate(io.Reader) (map[string]any, error) { return nil, nil }

// 无旅行能力的上游（traework/qoder）：RunTravelNow/RunActivityNow 静默跳过。
func TestTravelNoCapabilityNoop(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	s := New(Config{Pool: p, Upstream: bareUpstream{}, Name: "traework"})
	s.RunTravelNow()
	s.RunActivityNow()
}

// 一键领奖：只对到站账号领奖（idle/无猫零动作），领后刷新余额。
func TestRunClaimsNowOnlyArrived(t *testing.T) {
	var claimed map[string]any
	var statusCalls int64
	s, _ := travelTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/buddy/info"):
			envelope(w, `{"buddy":{"id":1,"name":"cat"}}`)
		case strings.HasSuffix(r.URL.Path, "/travel/status"):
			atomic.AddInt64(&statusCalls, 1)
			envelope(w, `{"state":"arrived","record_id":77,"reward_credit":9}`)
		case strings.HasSuffix(r.URL.Path, "/travel/claim"):
			_ = json.NewDecoder(r.Body).Decode(&claimed)
			envelope(w, `{"reward_credit":9}`)
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			envelope(w, `{"Response":{"Data":{"Accounts":[{"PackageName":"p","CapacityRemain":100}]}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	s.RunClaimsNow()
	if claimed["record_id"] != float64(77) {
		t.Fatalf("claim record_id=%v want 77", claimed["record_id"])
	}
	if n := atomic.LoadInt64(&statusCalls); n != 1 {
		t.Fatalf("status calls=%d want 1", n)
	}
}
