package engine

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type fakeHost struct {
	mu                            sync.Mutex
	values                        map[string][]byte
	accounts                      map[int64]*pluginv1.ResolveOutboundIdentityResponse
	schedulable                   map[int64]bool
	gets, sets, deletes, resolves int
}

func testHost(ids ...int64) *fakeHost {
	h := &fakeHost{values: map[string][]byte{}, accounts: map[int64]*pluginv1.ResolveOutboundIdentityResponse{}, schedulable: map[int64]bool{}}
	for _, id := range ids {
		h.accounts[id] = &pluginv1.ResolveOutboundIdentityResponse{Found: true, AccountId: id, Platform: "openai", AccountType: "oauth", Token: fmt.Sprintf("test-token-%d", id), Headers: map[string]*pluginv1.HeaderValues{"Chatgpt-Account-Id": {Values: []string{fmt.Sprint(id)}}}}
		h.schedulable[id] = true
	}
	return h
}
func (h *fakeHost) KVGet(_ context.Context, r *pluginv1.KVGetRequest, _ ...grpc.CallOption) (*pluginv1.KVGetResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gets++
	b, ok := h.values[r.Namespace+"/"+r.Key]
	return &pluginv1.KVGetResponse{Found: ok, Value: append([]byte(nil), b...)}, nil
}
func (h *fakeHost) KVSet(_ context.Context, r *pluginv1.KVSetRequest, _ ...grpc.CallOption) (*pluginv1.KVSetResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sets++
	h.values[r.Namespace+"/"+r.Key] = append([]byte(nil), r.Value...)
	return &pluginv1.KVSetResponse{}, nil
}
func (h *fakeHost) KVDelete(_ context.Context, r *pluginv1.KVDeleteRequest, _ ...grpc.CallOption) (*pluginv1.KVDeleteResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deletes++
	delete(h.values, r.Namespace+"/"+r.Key)
	return &pluginv1.KVDeleteResponse{}, nil
}
func (h *fakeHost) KVList(context.Context, *pluginv1.KVListRequest, ...grpc.CallOption) (*pluginv1.KVListResponse, error) {
	return &pluginv1.KVListResponse{}, nil
}
func (h *fakeHost) ListAccounts(context.Context, *pluginv1.ListAccountsRequest, ...grpc.CallOption) (*pluginv1.ListAccountsResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := &pluginv1.ListAccountsResponse{}
	for id := range h.accounts {
		r.AccountIds = append(r.AccountIds, id)
		r.Accounts = append(r.Accounts, &pluginv1.AccountInfo{Id: id, Platform: "openai", AccountType: "oauth", Schedulable: h.schedulable[id]})
	}
	return r, nil
}
func (h *fakeHost) ResolveOutboundIdentity(_ context.Context, r *pluginv1.ResolveOutboundIdentityRequest, _ ...grpc.CallOption) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resolves++
	original := h.accounts[r.AccountId]
	if original == nil {
		return &pluginv1.ResolveOutboundIdentityResponse{}, nil
	}
	return proto.Clone(original).(*pluginv1.ResolveOutboundIdentityResponse), nil
}
func testState(n int) string { return "gAAAAA" + strings.Repeat("A", n-6) }
func testFernetState(n int, issuedAt time.Time) string {
	raw := make([]byte, n*6/8)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issuedAt.Unix()))
	return base64.RawURLEncoding.EncodeToString(raw)
}
func setTargetRouteCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "lb-test"})
	http.SetCookie(w, &http.Cookie{Name: "__oailb", Value: "unified-88"})
}
func completed(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":%q}}\n\n", model)
}
func completedWithText(w http.ResponseWriter, model, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"item_id\":\"msg-test\",\"text\":%q}\n\n", text)
	completed(w, model)
}
func testConfig(proxy string, ids ...int64) Config {
	c := DefaultConfig()
	// Unit tests exercise both retired and latest paths directly. Production
	// ParseConfig always migrates this field to cookie + 780.
	c.TicketMode = ticketModeLegacy
	c.AllowState780 = false
	c.MintConcurrency = 1
	c.Enabled = true
	c.DynamicProxyURL = proxy
	c.MaxAttempts = 1
	for _, id := range ids {
		c.Accounts = append(c.Accounts, AccountConfig{AccountID: id, Enabled: true, EgressMode: egressModeSub2, Plan: "pro", Models: []string{"gpt-6-astra"}})
	}
	return c
}
func testEngine(t *testing.T, h *fakeHost, url string) *Engine {
	t.Helper()
	e := newEngine(h, url, 5*time.Millisecond)
	e.mu.Lock()
	e.warmup = 0
	e.mu.Unlock()
	t.Cleanup(e.Close)
	return e
}
func apply(t *testing.T, e *Engine, c Config) {
	t.Helper()
	r := e.applyConfig(c)
	if !r.Applied {
		t.Fatalf("apply: %+v", r)
	}
}
func waitFor(t *testing.T, e *Engine, state string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		s := e.snapshotLocked(time.Now())
		e.mu.Unlock()
		if len(s.Tickets) > 0 && s.Tickets[0].State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r, _ := e.Health(context.Background(), &pluginv1.HealthRequest{})
	t.Fatalf("waiting %s: %s", state, r.StatusJson)
}
func testStart(id int64) *pluginv1.ForwardRequestStart {
	return &pluginv1.ForwardRequestStart{AccountId: id, Platform: "openai", AccountType: "oauth", Headers: map[string]*pluginv1.HeaderValues{"Chatgpt-Account-Id": {Values: []string{fmt.Sprint(id)}}}}
}

func TestConfigStrictIsolation(t *testing.T) {
	c, err := ParseConfig([]byte(`{}`))
	if err != nil || c.Enabled || c.TTLMinutes != 180 || c.RefreshBeforeSeconds != 120 || c.PreferPreviousIP ||
		c.GatewayPolicy != gatewayPolicyAllow || c.TargetGateway != "unified-15,unified-88,unified-180" ||
		!c.RouteCookieReuse || !c.MintFingerprintConvergence || c.QualityProbeEnabled ||
		c.QualityProbeMode != qualityProbeOff || c.QualityProbePrompt != "" || c.QualityProbeAccept != "" ||
		c.TicketMode != ticketModeCookie || !c.AllowState780 || c.MintConcurrency != 3 || len(c.Accounts) != 0 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	bad := []string{`{"unknown":true}`, `null`, `{"ttl_minutes":181}`, `{"ttl_minutes":5,"refresh_before_minutes":5}`, `{"ttl_minutes":1,"refresh_before_seconds":60}`, `{"max_attempts":33}`, `{"mint_concurrency":4}`, `{"proxy_generator_ttl_minutes":181}`, `{"target_gateway":"unified-abc"}`, `{"target_gateway":"https://gateway.example"}`, `{"target_gateway":"any,unified-88"}`, `{"gateway_policy":"blocked"}`, `{"gateway_policy":"allow","target_gateway":"any"}`, `{"gateway_policy":"deny","target_gateway":""}`, `{"enabled":true,"accounts":[{"account_id":1,"enabled":true}]}`, `{"accounts":[{"account_id":1},{"account_id":1}]}`, `{"accounts":[{"account_id":1,"models":["gpt-6-astra","gpt-6-astra"]}]}`, `{"dynamic_proxy_url":"file:///tmp/a"}`, `{"dynamic_proxy_url":"http://host/secret?token=x"}`, `{"accounts":[{"account_id":1,"plan":"wrong"}]}`, `{"accounts":[{"account_id":1,"email":"not-an-email"}]}`, `{"accounts":[{"account_id":1,"name":"bad\nname"}]}`, `{"accounts":[{"account_id":1,"egress_mode":"plugin"}]}`, `{"accounts":[{"account_id":1,"egress_mode":"plugin","sticky_proxy_url":"socks5h://user-{random}:pass@proxy.example:1080"}]}`, `{"accounts":[{"account_id":1,"egress_mode":"other","sticky_proxy_url":"socks5h://proxy.example:1080"}]}`, `{"quality_probe_enabled":true}`, `{"quality_probe_enabled":true,"quality_probe_prompt":"probe"}`, `{"quality_probe_enabled":true,"quality_probe_prompt":"probe","quality_probe_accept":",17"}`, `{"quality_probe_accept":"1,2,3,4,5,6,7,8,9"}`}
	for _, raw := range bad {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("accepted invalid config %s", raw)
		}
	}
	c2, _ := ParseConfig([]byte(`{"accounts":[{"account_id":1}]}`))
	c2.Accounts[0].Models[0] = "gpt-other"
	c3, _ := ParseConfig([]byte(`{"accounts":[{"account_id":1}]}`))
	if c3.Accounts[0].Models[0] != "gpt-6-astra" {
		t.Fatal("default slices shared")
	}
	meta, err := ParseConfig([]byte(`{"accounts":[{"account_id":7,"name":" Example ","email":"owner@example.com","expires_at":"2026-12-31 23:59","quota":"$12.50 / $20.00"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := meta.Accounts[0]
	if got.Name != "Example" || got.Email != "owner@example.com" || got.ExpiresAt != "2026-12-31 23:59" || got.Quota != "$12.50 / $20.00" {
		t.Fatalf("display metadata not normalized: %+v", got)
	}
	plugin, err := ParseConfig([]byte(`{"enabled":true,"upstream_proxy_url":"socks5://first.example:1081","proxy_generator_url":"https://generator.example/gen","accounts":[{"account_id":9,"enabled":true,"egress_mode":"plugin","sticky_proxy_url":"socks5h://user-session-123:pass@us.example:10000"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if plugin.Accounts[0].EgressMode != egressModePlugin || plugin.DynamicProxyURL != "" {
		t.Fatalf("plugin egress configuration not normalized: %+v", plugin)
	}
	seconds, err := ParseConfig([]byte(`{"ttl_minutes":5,"refresh_before_seconds":30}`))
	if err != nil || seconds.RefreshBeforeSeconds != 30 {
		t.Fatalf("seconds renewal horizon not parsed: %+v %v", seconds, err)
	}
	legacy, err := ParseConfig([]byte(`{"ttl_minutes":5,"refresh_before_minutes":1}`))
	if err != nil || legacy.RefreshBeforeSeconds != 60 || legacy.RefreshBeforeMinutes != 0 {
		t.Fatalf("legacy renewal horizon not migrated: %+v %v", legacy, err)
	}
	prefer, err := ParseConfig([]byte(`{"prefer_previous_ip":true}`))
	if err != nil || !prefer.PreferPreviousIP {
		t.Fatalf("previous egress preference not parsed: %+v %v", prefer, err)
	}
	gateway, err := ParseConfig([]byte(`{"target_gateway":"88, 180,15,unified_88"}`))
	if err != nil || gateway.GatewayPolicy != gatewayPolicyAllow || gateway.TargetGateway != "unified-15,unified-88,unified-180" {
		t.Fatalf("target gateways were not normalized: %+v %v", gateway, err)
	}
	anyGateway, err := ParseConfig([]byte(`{"target_gateway":"any"}`))
	if err != nil || anyGateway.GatewayPolicy != gatewayPolicyAny || anyGateway.TargetGateway != "" {
		t.Fatalf("any target gateway was not normalized: %+v %v", anyGateway, err)
	}
	denyGateway, err := ParseConfig([]byte(`{"gateway_policy":"deny","target_gateway":"88,180"}`))
	if err != nil || denyGateway.GatewayPolicy != gatewayPolicyDeny || denyGateway.TargetGateway != "unified-88,unified-180" {
		t.Fatalf("deny gateway policy was not normalized: %+v %v", denyGateway, err)
	}
	anyPolicy, err := ParseConfig([]byte(`{"gateway_policy":"any","target_gateway":"unified-88"}`))
	if err != nil || anyPolicy.GatewayPolicy != gatewayPolicyAny || anyPolicy.TargetGateway != "" {
		t.Fatalf("explicit any gateway policy was not normalized: %+v %v", anyPolicy, err)
	}
	quality, err := ParseConfig([]byte(`{"quality_probe_enabled":true,"quality_probe_prompt":"probe","quality_probe_accept":" 17, iphone 17 ,17"}`))
	if err != nil || !quality.QualityProbeEnabled || quality.QualityProbeMode != qualityProbeStrict || quality.QualityProbePrompt != "probe" || quality.QualityProbeAccept != "17,iphone 17" {
		t.Fatalf("quality probe config was not normalized: %+v %v", quality, err)
	}
	fallback, err := ParseConfig([]byte(`{"ticket_mode":"legacy","allow_state_780":false,"quality_probe_mode":"strict_fallback","quality_probe_prompt":"probe","quality_probe_accept":"17","mint_concurrency":2}`))
	if err != nil || fallback.TicketMode != ticketModeCookie || !fallback.AllowState780 || fallback.QualityProbeMode != qualityProbeStrictFallback || fallback.MintConcurrency != 2 {
		t.Fatalf("latest-mode migration was not normalized: %+v %v", fallback, err)
	}
}

func TestPausedAccountsAreNotScheduled(t *testing.T) {
	h := testHost(42)
	h.schedulable[42] = false
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	e := testEngine(t, h, proxy.URL)
	apply(t, e, testConfig(proxy.URL, 42))
	time.Sleep(150 * time.Millisecond)
	if requests.Load() != 0 || h.resolves != 0 {
		t.Fatalf("paused account was scheduled: requests=%d resolves=%d", requests.Load(), h.resolves)
	}
}

func TestEffectiveRefreshBeforeSeconds(t *testing.T) {
	c := DefaultConfig()
	a := AccountConfig{EgressMode: egressModeSub2}
	if got := effectiveRefreshBefore(c, a); got != 120*time.Second {
		t.Fatalf("default renewal horizon = %s", got)
	}
	c.RefreshBeforeSeconds = 30
	if got := effectiveRefreshBefore(c, a); got != 30*time.Second {
		t.Fatalf("configured renewal horizon = %s", got)
	}
	c.ProxyGeneratorTTLMinutes = 1
	a.EgressMode = egressModeGenerator
	c.RefreshBeforeSeconds = 120
	if got := effectiveRefreshBefore(c, a); got != 59*time.Second {
		t.Fatalf("generator renewal horizon was not clamped = %s", got)
	}
}
func TestCollectFixedProxyValidationAndPersistence(t *testing.T) {
	h := testHost(42)
	var dynamic, fixed atomic.Int32
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dynamic.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-token-42" || r.Header.Get(StateHeader) != "" {
			t.Errorf("dynamic identity or STATE incorrect: authorization=%q state=%q", r.Header.Get("Authorization"), r.Header.Get(StateHeader))
		}
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixed.Add(1)
		if r.Header.Get(StateHeader) != testState(292) || r.Header.Get("Authorization") != "Bearer test-token-42" {
			t.Error("fixed validation lost identity/state")
		}
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()
	e := testEngine(t, h, business.URL)
	c := testConfig(pool.URL, 42)
	apply(t, e, c)
	waitFor(t, e, "ready")
	if dynamic.Load() != 1 || fixed.Load() != 1 {
		t.Fatalf("requests dynamic=%d fixed=%d", dynamic.Load(), fixed.Load())
	}
	r, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || r == nil {
		t.Fatalf("ticket missing %v", err)
	}
	if r, err = e.ticketForRequest(context.Background(), testStart(99), "gpt-6-astra"); err != nil || r != nil {
		t.Fatal("unlisted account not isolated")
	}
	if r, err = e.ticketForRequest(context.Background(), testStart(42), "gpt-5.6-sol"); err != nil || r != nil {
		t.Fatal("untargeted model not isolated")
	}
	start := testStart(42)
	start.ProxyUrl = "http://127.0.0.1:9"
	if _, err = e.ticketForRequest(context.Background(), start, "gpt-6-astra"); err == nil {
		t.Fatal("ticket reused on changed fixed proxy")
	}
	start = testStart(42)
	start.Headers["Chatgpt-Account-Id"].Values = []string{"another-account"}
	if _, err = e.ticketForRequest(context.Background(), start, "gpt-6-astra"); err == nil {
		t.Fatal("ticket reused on changed account identity")
	}
	health, _ := e.Health(context.Background(), &pluginv1.HealthRequest{})
	if strings.Contains(health.StatusJson, testState(292)) || strings.Contains(health.StatusJson, "test-token") {
		t.Fatal("secret in health")
	}
	h.mu.Lock()
	for _, value := range h.values {
		if strings.Contains(string(value), "test-token") || strings.Contains(string(value), pool.URL) {
			t.Error("credentials stored with ticket")
		}
	}
	h.mu.Unlock()
	apply(t, e, c)
	if dynamic.Load() != 1 {
		t.Fatal("identical config started duplicate harvest")
	}
	// Recreate engine using the same host KV. Restoration revalidates on fixed IP
	// and does not request another dynamic IP.
	e.Close()
	e2 := testEngine(t, h, business.URL)
	apply(t, e2, c)
	waitFor(t, e2, "ready")
	if dynamic.Load() != 1 || fixed.Load() < 2 {
		t.Fatalf("restart did not revalidate KV ticket: dynamic=%d fixed=%d", dynamic.Load(), fixed.Load())
	}
}

func TestQualityProbeGatesNewTicket(t *testing.T) {
	for _, tc := range []struct {
		name, text, wantState string
		convergence           bool
	}{
		{"matched", "iPhone 17", "ready", true},
		{"matched without fingerprint convergence", "iPhone 17", "ready", false},
		{"mismatch", "iPhone 16", "cooldown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testHost(42)
			pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(StateHeader, testState(292))
				completed(w, "gpt-6-astra")
			}))
			defer pool.Close()
			var businessCalls atomic.Int32
			business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := businessCalls.Add(1)
				if r.Header.Get(StateHeader) != testState(292) {
					t.Error("business request lost STATE")
				}
				if call == 1 {
					completed(w, "gpt-6-astra")
					return
				}
				var payload struct {
					Instructions string `json:"instructions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode quality payload: %v", err)
				}
				if strings.Contains(strings.ToLower(payload.Instructions), "pong") {
					t.Errorf("quality probe was forced to pong: %q", payload.Instructions)
				}
				completedWithText(w, "gpt-6-astra", tc.text)
			}))
			defer business.Close()

			e := testEngine(t, h, business.URL)
			c := testConfig(pool.URL, 42)
			c.QualityProbeEnabled = true
			c.QualityProbePrompt = "quality prompt"
			c.QualityProbeAccept = "17"
			c.MintFingerprintConvergence = tc.convergence
			apply(t, e, c)
			waitFor(t, e, tc.wantState)
			if businessCalls.Load() != 2 {
				t.Fatalf("business calls = %d; want 2", businessCalls.Load())
			}
			if tc.wantState == "cooldown" {
				e.mu.Lock()
				defer e.mu.Unlock()
				if !strings.Contains(e.records[keyFor(42, "gpt-6-astra")].LastError, "quality_mismatch") {
					t.Fatalf("last error = %q", e.records[keyFor(42, "gpt-6-astra")].LastError)
				}
			}
		})
	}
}

func TestQualityStrictFallbackPublishesProvisionalTicket(t *testing.T) {
	h := testHost(42)
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"203.0.113.31"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"203.0.113.31"}`))
			return
		}
		setTargetRouteCookies(w)
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "capture"})
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer capture.Close()

	var businessCalls atomic.Int32
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"198.51.100.31"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"198.51.100.31"}`))
			return
		}
		call := businessCalls.Add(1)
		setTargetRouteCookies(w)
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "business"})
		w.Header().Set(StateHeader, testState(780))
		if call == 2 {
			completedWithText(w, "gpt-6-astra", "iPhone 16")
			return
		}
		if call >= 4 {
			completedWithText(w, "gpt-6-astra", "iPhone 17")
			return
		}
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()

	e := testEngine(t, h, business.URL)
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(capture.URL, 42)
	c.TicketMode = ticketModeCookie
	c.AllowState780 = true
	c.MintConcurrency = 1
	c.CookieCaptureMode = captureModeSOCKS5
	c.CookieCaptureProxyURL = capture.URL
	c.CookieBusinessProxyURL = business.URL
	c.RouteCookieReuse = false
	c.QualityProbeEnabled = true
	c.QualityProbeMode = qualityProbeStrictFallback
	c.QualityProbePrompt = "quality prompt"
	c.QualityProbeAccept = "17"
	c.AttemptIntervalSeconds = 1
	apply(t, e, c)
	waitFor(t, e, "ready_provisional")

	e.mu.Lock()
	current := e.tickets[keyFor(42, "gpt-6-astra")]
	e.mu.Unlock()
	if current == nil || current.QualityStatus != ticketQualityProvisional {
		t.Fatalf("provisional ticket not published: %+v", current)
	}
	if businessCalls.Load() < 2 {
		t.Fatalf("quality probe did not run: business calls=%d", businessCalls.Load())
	}
	waitFor(t, e, "ready")
	e.mu.Lock()
	current = e.tickets[keyFor(42, "gpt-6-astra")]
	e.mu.Unlock()
	if current == nil || current.QualityStatus != ticketQualityVerified {
		t.Fatalf("provisional ticket was not upgraded in the background: %+v", current)
	}
}

func TestQualityStrictFallbackKeepsProvisionalAliveAfterWorkerStops(t *testing.T) {
	h := testHost(42)
	var captureCalls atomic.Int32
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"203.0.113.71"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"203.0.113.71"}`))
			return
		}
		if captureCalls.Add(1) == 1 {
			setTargetRouteCookies(w)
			http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "capture"})
			w.Header().Set(StateHeader, testState(780))
			completed(w, "gpt-6-astra")
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer capture.Close()

	var businessCalls atomic.Int32
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"198.51.100.71"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"198.51.100.71"}`))
			return
		}
		call := businessCalls.Add(1)
		setTargetRouteCookies(w)
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "business"})
		w.Header().Set(StateHeader, testState(780))
		if call == 2 {
			completedWithText(w, "gpt-6-astra", "iPhone 16")
			return
		}
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()

	e := testEngine(t, h, business.URL)
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := DefaultConfig()
	c.Enabled = true
	c.TicketMode = ticketModeCookie
	c.AllowState780 = true
	c.MintConcurrency = 2
	c.MaxAttempts = 2
	c.CookieCaptureMode = captureModeSOCKS5
	c.CookieCaptureProxyURL = capture.URL
	c.CookieBusinessProxyURL = business.URL
	c.RouteCookieReuse = false
	c.QualityProbeMode = qualityProbeStrictFallback
	c.QualityProbePrompt = "quality prompt"
	c.QualityProbeAccept = "17"
	c.AttemptIntervalSeconds = 1
	c.Accounts = []AccountConfig{{AccountID: 42, Enabled: true, EgressMode: egressModeSub2, Plan: "pro", Models: []string{"gpt-6-astra"}}}
	apply(t, e, c)
	waitFor(t, e, "ready_provisional")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		current := e.tickets[keyFor(42, "gpt-6-astra")]
		record := e.records[keyFor(42, "gpt-6-astra")]
		valid := validTicket(current, e.config, e.config.Accounts[0], "gpt-6-astra", time.Now())
		cooldown := record != nil && record.CooldownUntil.After(time.Now())
		e.mu.Unlock()
		if valid && current.QualityStatus == ticketQualityProvisional && !cooldown {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("provisional ticket entered cooldown after a concurrent upstream stop")
}

func TestCaptureCandidatesParallelizesSharedAttemptBudget(t *testing.T) {
	h := testHost(42)
	release := make(chan struct{})
	var concurrent atomic.Int32
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"203.0.113.41"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"203.0.113.41"}`))
			return
		}
		current := concurrent.Add(1)
		if current == 3 {
			close(release)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		concurrent.Add(-1)
		setTargetRouteCookies(w)
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer capture.Close()

	e := newEngine(h, capture.URL, time.Second)
	defer e.Close()
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := DefaultConfig()
	c.Enabled = true
	c.ProxyGeneratorBlockedCountries = []string{"HK"}
	c.TicketMode = ticketModeCookie
	c.AllowState780 = true
	c.MintConcurrency = 3
	c.CookieCaptureMode = captureModeSOCKS5
	c.CookieCaptureProxyURL = capture.URL
	c.RouteCookieReuse = false
	a := AccountConfig{AccountID: 42, Enabled: true, EgressMode: egressModeSub2, Plan: "pro", Models: []string{"gpt-6-astra"}}

	results, cancel := e.captureCandidates(context.Background(), h, c, a, "gpt-6-astra", keyFor(42, "gpt-6-astra"), 1, 3, false, false)
	defer cancel()
	select {
	case <-release:
	case <-time.After(3 * time.Second):
		t.Fatal("three capture workers did not run concurrently")
	}
	count := 0
	for range results {
		count++
	}
	if count != 3 {
		t.Fatalf("capture results=%d; want 3", count)
	}
}

func TestCaptureCancellationStopsOtherWorkers(t *testing.T) {
	h := testHost(42)
	var probeOrder atomic.Int32
	var allStarted sync.Once
	allStartedCh := make(chan struct{})
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"203.0.113.51"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"203.0.113.51"}`))
			return
		}
		order := probeOrder.Add(1)
		if order == 3 {
			allStarted.Do(func() { close(allStartedCh) })
		}
		if order > 1 {
			time.Sleep(time.Second)
			return
		}
		select {
		case <-allStartedCh:
		case <-r.Context().Done():
			return
		}
		setTargetRouteCookies(w)
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer capture.Close()

	e := testEngine(t, h, capture.URL)
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := DefaultConfig()
	c.Enabled = true
	c.TicketMode = ticketModeCookie
	c.AllowState780 = true
	c.MintConcurrency = 3
	c.MaxAttempts = 3
	c.CookieCaptureMode = captureModeSOCKS5
	c.CookieCaptureProxyURL = capture.URL
	c.CookieBusinessProxyURL = ""
	c.RouteCookieReuse = false
	c.Accounts = []AccountConfig{{AccountID: 42, Enabled: true, EgressMode: egressModeSub2, Plan: "pro", Models: []string{"gpt-6-astra"}}}

	results, cancel := e.captureCandidates(context.Background(), h, c, c.Accounts[0], "gpt-6-astra", keyFor(42, "gpt-6-astra"), 1, 3, false, false)
	select {
	case result := <-results:
		if result.Candidate == "" {
			t.Fatal("first capture did not return a candidate")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first capture did not complete")
	}
	cancel()
	closed := make(chan struct{})
	go func() {
		for range results {
		}
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(750 * time.Millisecond):
		t.Fatal("other capture workers did not stop after cancellation")
	}
}

func TestPluginEgressUsesAccountStickyProxyForCaptureAndValidation(t *testing.T) {
	h := testHost(42)
	var dynamicHits atomic.Int32
	dynamic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dynamicHits.Add(1)
		http.Error(w, "dynamic pool must not be used", http.StatusBadGateway)
	}))
	defer dynamic.Close()
	var stickyHits atomic.Int32
	sticky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stickyHits.Add(1)
		if r.Header.Get("Authorization") != "Bearer test-token-42" {
			t.Error("sticky proxy lost account authorization")
		}
		if stickyHits.Load() == 1 {
			if r.Header.Get(StateHeader) != "" {
				t.Error("capture unexpectedly carried STATE")
			}
			w.Header().Set(StateHeader, testState(292))
		} else if r.Header.Get(StateHeader) != testState(292) {
			t.Error("fixed validation lost STATE")
		}
		completed(w, "gpt-6-astra")
	}))
	defer sticky.Close()
	stickyURL := "http://sticky-user:sticky-secret@" + strings.TrimPrefix(sticky.URL, "http://")

	e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
	c := testConfig(dynamic.URL, 42)
	c.Accounts[0].EgressMode = egressModePlugin
	c.Accounts[0].StickyProxyURL = stickyURL
	apply(t, e, c)
	waitFor(t, e, "ready")
	if dynamicHits.Load() != 0 || stickyHits.Load() != 2 {
		t.Fatalf("egress routing dynamic=%d sticky=%d", dynamicHits.Load(), stickyHits.Load())
	}
	r, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || r == nil || r.TargetProxyURL != stickyURL || r.UpstreamProxyURL != "" {
		t.Fatalf("ticket did not retain account sticky proxy: %+v %v", r, err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, value := range h.values {
		if strings.Contains(string(value), "sticky-secret") || strings.Contains(string(value), stickyURL) {
			t.Fatal("proxy credentials were persisted with ticket")
		}
	}
}

func TestPlanLengthMismatchAndRoutingFailure(t *testing.T) {
	for _, tc := range []struct {
		name, plan, actual string
		length             int
		want               string
	}{
		{"team332", "team", "gpt-6-astra", 332, "ready"}, {"teamReject292", "team", "gpt-6-astra", 292, "cooldown"}, {"proReject332", "pro", "gpt-6-astra", 332, "cooldown"}, {"lengthAloneInsufficient", "pro", "gpt-5.6-luna", 292, "cooldown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := testHost(42)
			pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(StateHeader, testState(tc.length))
				completed(w, tc.actual)
			}))
			defer pool.Close()
			business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { completed(w, "gpt-6-astra") }))
			defer business.Close()
			e := testEngine(t, h, business.URL)
			c := testConfig(pool.URL, 42)
			c.Accounts[0].Plan = tc.plan
			apply(t, e, c)
			waitFor(t, e, tc.want)
		})
	}
}

func TestState780CompatibilityRequiresOptInAndSameEgressValidation(t *testing.T) {
	h := testHost(42)
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setTargetRouteCookies(w)
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "capture"})
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()
	e := testEngine(t, h, business.URL)
	c := testConfig(pool.URL, 42)
	c.AllowState780 = true
	c.TargetGateway = "unified-88"
	apply(t, e, c)
	waitFor(t, e, "ready")

	off := testEngine(t, testHost(42), business.URL)
	disabled := testConfig(pool.URL, 42)
	apply(t, off, disabled)
	waitFor(t, off, "cooldown")
}

func TestState780Rejects312DuringSameEgressValidation(t *testing.T) {
	h := testHost(42)
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setTargetRouteCookies(w)
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "capture"})
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(StateHeader, testState(312))
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()
	e := testEngine(t, h, business.URL)
	c := testConfig(pool.URL, 42)
	c.AllowState780 = true
	apply(t, e, c)
	waitFor(t, e, "cooldown")
}

func TestState780RejectsOffTargetGatewayBeforeBusinessValidation(t *testing.T) {
	h := testHost(42)
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "lb-test"})
		http.SetCookie(w, &http.Cookie{Name: "__oailb", Value: "unified-15"})
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	var businessCalls atomic.Int32
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		businessCalls.Add(1)
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()

	e := testEngine(t, h, business.URL)
	c := testConfig(pool.URL, 42)
	c.AllowState780 = true
	c.TargetGateway = "unified-88"
	apply(t, e, c)
	waitFor(t, e, "cooldown")
	if businessCalls.Load() != 0 {
		t.Fatalf("off-target 780 reached business validation %d times", businessCalls.Load())
	}
}

func TestState780DenyPolicyAllowsUnlistedGateway(t *testing.T) {
	h := testHost(42)
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "__cflb", Value: "lb-test"})
		http.SetCookie(w, &http.Cookie{Name: "__oailb", Value: "unified-15"})
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	var businessCalls atomic.Int32
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		businessCalls.Add(1)
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()

	e := testEngine(t, h, business.URL)
	c := testConfig(pool.URL, 42)
	c.AllowState780 = true
	c.GatewayPolicy = gatewayPolicyDeny
	c.TargetGateway = "unified-88"
	apply(t, e, c)
	waitFor(t, e, "ready")
	if businessCalls.Load() == 0 {
		t.Fatal("unlisted 780 gateway did not reach business validation under deny policy")
	}
}

func TestState780AnyPolicyStillRejectsMissingRouteCookies(t *testing.T) {
	for name, routeCookies := range map[string]map[string]string{
		"missing __cflb":  {"__cf_bm": "present", "__oailb": "unified-88"},
		"missing __oailb": {"__cf_bm": "present", "__cflb": "lb"},
	} {
		t.Run(name, func(t *testing.T) {
			h := testHost(42)
			pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for cookieName, value := range routeCookies {
					http.SetCookie(w, &http.Cookie{Name: cookieName, Value: value})
				}
				w.Header().Set(StateHeader, testState(780))
				completed(w, "gpt-6-astra")
			}))
			defer pool.Close()
			var businessCalls atomic.Int32
			business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				businessCalls.Add(1)
				completed(w, "gpt-6-astra")
			}))
			defer business.Close()

			e := testEngine(t, h, business.URL)
			c := testConfig(pool.URL, 42)
			c.AllowState780 = true
			c.GatewayPolicy = gatewayPolicyAny
			c.TargetGateway = ""
			apply(t, e, c)
			waitFor(t, e, "cooldown")
			if businessCalls.Load() != 0 {
				t.Fatalf("780 without a complete route pair reached business validation %d times", businessCalls.Load())
			}
		})
	}
}

func TestCookieValidationPersistsRollingStateAndCookie(t *testing.T) {
	h := testHost(42)
	captureState := testState(780)
	validatedState := testState(779) + "B"
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"203.0.113.1"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"203.0.113.1"}`))
			return
		}
		setTargetRouteCookies(w)
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "capture"})
		w.Header().Set(StateHeader, captureState)
		completed(w, "gpt-6-astra")
	}))
	defer capture.Close()

	var validationCalls atomic.Int32
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"198.51.100.2"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"198.51.100.2"}`))
			return
		}
		if r.Header.Get(StateHeader) != captureState {
			t.Errorf("validation state = %q; want capture state", r.Header.Get(StateHeader))
		}
		validationCalls.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "__cf_bm", Value: "validated"})
		w.Header().Set(StateHeader, validatedState)
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()

	e := testEngine(t, h, business.URL)
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(capture.URL, 42)
	c.TicketMode = ticketModeCookie
	c.CookieCaptureMode = captureModeSOCKS5
	c.CookieCaptureProxyURL = capture.URL
	c.CookieBusinessProxyURL = business.URL
	c.AllowState780 = true
	apply(t, e, c)
	waitFor(t, e, "ready")
	if validationCalls.Load() == 0 {
		t.Fatal("business egress validation did not run")
	}

	e.mu.Lock()
	got := e.tickets[keyFor(42, "gpt-6-astra")]
	var gotState, gotCookie string
	if got != nil {
		gotState = got.State
		gotCookie = got.Cookies["__cf_bm"]
	}
	e.mu.Unlock()
	if gotState != validatedState {
		t.Fatalf("stored state = %q; want validation state", gotState)
	}
	if gotCookie != "validated" {
		t.Fatalf("stored __cf_bm = %q; want validation cookie", gotCookie)
	}
}

func TestAuthAndRateLimitStopRound(t *testing.T) {
	for _, code := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			h := testHost(42)
			var calls atomic.Int32
			pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(code) }))
			defer pool.Close()
			e := testEngine(t, h, pool.URL)
			c := testConfig(pool.URL, 42)
			c.MaxAttempts = 8
			apply(t, e, c)
			waitFor(t, e, "cooldown")
			if calls.Load() != 1 {
				t.Fatalf("did not stop: %d", calls.Load())
			}
		})
	}
}
func TestFailedRenewalRetainsTicketAndLateWatchdogCannotRevokeNew(t *testing.T) {
	h := testHost(42)
	var fail atomic.Bool
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(429)
			return
		}
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { completed(w, "gpt-6-astra") }))
	defer business.Close()
	e := testEngine(t, h, business.URL)
	c := testConfig(pool.URL, 42)
	apply(t, e, c)
	waitFor(t, e, "ready")
	old, _ := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	fail.Store(true)
	e.mu.Lock()
	current := e.tickets[keyFor(42, "gpt-6-astra")]
	current.ExpiresAt = time.Now().Add(5 * time.Minute)
	e.mu.Unlock()
	e.notify()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		r := e.records[old.Key]
		failed := r != nil && r.LastError == "upstream_rate_limited"
		e.mu.Unlock()
		if failed {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || got.Version != old.Version {
		t.Fatal("failed renewal discarded still-valid ticket")
	}
	e.mu.Lock()
	replacement := *e.tickets[old.Key]
	replacement.Version = "new-version"
	e.tickets[old.Key] = &replacement
	e.mu.Unlock()
	e.invalidate(old, "model_mismatch")
	got, err = e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || got.Version != "new-version" {
		t.Fatal("late response revoked a new ticket")
	}
	e.invalidate(got, "state_312")
	if _, err = e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra"); err == nil {
		t.Fatal("current rejected ticket still available")
	}
}

func TestRenewalKeepsOldTicketAvailable(t *testing.T) {
	h := testHost(42)
	release := make(chan struct{})
	var blockRenewal atomic.Bool
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if blockRenewal.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { completed(w, "gpt-6-astra") }))
	defer business.Close()
	e := testEngine(t, h, business.URL)
	apply(t, e, testConfig(pool.URL, 42))
	waitFor(t, e, "ready")
	old, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || old == nil {
		t.Fatalf("initial ticket unavailable: %v", err)
	}
	blockRenewal.Store(true)
	e.mu.Lock()
	current := e.tickets[old.Key]
	current.ExpiresAt = time.Now().Add(5 * time.Second)
	e.mu.Unlock()
	e.notify()
	waitFor(t, e, "renewing")
	got, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || got == nil || got.Version != old.Version {
		t.Fatalf("renewal blocked the still-valid ticket: %+v %v", got, err)
	}
	close(release)
}
func TestApplyCancellationAndPassiveHealth(t *testing.T) {
	h := testHost(42)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	e := testEngine(t, h, pool.URL)
	c := testConfig(pool.URL, 42)
	apply(t, e, c)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("harvest not started")
	}
	c.Enabled = false
	apply(t, e, c)
	close(release)
	time.Sleep(25 * time.Millisecond)
	e.mu.Lock()
	if len(e.tickets) != 0 {
		t.Error("old config job committed after disable")
	}
	e.mu.Unlock()
	h.mu.Lock()
	before := h.resolves
	h.mu.Unlock()
	for i := 0; i < 10; i++ {
		e.Health(context.Background(), &pluginv1.HealthRequest{})
	}
	h.mu.Lock()
	after := h.resolves
	h.mu.Unlock()
	if before != after {
		t.Fatal("Health caused credential reads")
	}
	if r, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra"); r != nil || err != nil {
		t.Fatal("disabled account did not pass through")
	}
}
func TestProxySessionRotation(t *testing.T) {
	raw := "socks5h://example-region-Rand-sid-old-t-5:fake@us.1024proxy.io:3000"
	a, _ := rotateProxy(raw)
	b, _ := rotateProxy(raw)
	if a == b || strings.Contains(a, "sid-old") {
		t.Fatal("1024 session not rotated")
	}
	a, err := rotateProxy("http://user-{sid}:fake-{random}@127.0.0.1:3128")
	if err != nil || strings.Contains(a, "{") {
		t.Fatal("template not expanded")
	}
}

func TestFixedProxyChangeTriggersCollectionBeforeExpiry(t *testing.T) {
	h := testHost(42)
	var dynamic atomic.Int32
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dynamic.Add(1)
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { completed(w, "gpt-6-astra") }))
	defer business.Close()
	newFixed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(StateHeader) != testState(292) {
			t.Error("new fixed proxy validation lacks state")
		}
		completed(w, "gpt-6-astra")
	}))
	defer newFixed.Close()
	e := testEngine(t, h, business.URL)
	c := testConfig(pool.URL, 42)
	apply(t, e, c)
	waitFor(t, e, "ready")
	h.mu.Lock()
	h.accounts[42].ProxyUrl = newFixed.URL
	h.mu.Unlock()
	start := testStart(42)
	start.ProxyUrl = newFixed.URL
	if _, err := e.ticketForRequest(context.Background(), start, "gpt-6-astra"); err == nil {
		t.Fatal("changed fixed proxy used old ticket")
	}
	waitFor(t, e, "ready")
	if dynamic.Load() != 2 {
		t.Fatalf("did not immediately recollect for changed binding: %d", dynamic.Load())
	}
	if _, err := e.ticketForRequest(context.Background(), start, "gpt-6-astra"); err != nil {
		t.Fatalf("new fixed proxy ticket unavailable: %v", err)
	}
}

func TestFixedRevalidationRejectsRoutingAnd312(t *testing.T) {
	for _, mode := range []string{"mismatch", "312", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			h := testHost(42)
			pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(StateHeader, testState(292))
				completed(w, "gpt-6-astra")
			}))
			defer pool.Close()
			business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "mismatch":
					completed(w, "gpt-5.6-luna")
				case "312":
					w.Header().Set(StateHeader, testState(312))
					completed(w, "gpt-6-astra")
				case "incomplete":
					fmt.Fprint(w, `data: {"type":"response.created","response":{"model":"gpt-6-astra"}}`)
				}
			}))
			defer business.Close()
			e := testEngine(t, h, business.URL)
			apply(t, e, testConfig(pool.URL, 42))
			waitFor(t, e, "cooldown")
			if _, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra"); err == nil {
				t.Fatal("unverified candidate exposed")
			}
			h.mu.Lock()
			n := len(h.values)
			h.mu.Unlock()
			if n != 0 {
				t.Fatal("unverified candidate persisted")
			}
		})
	}
}

type slowKVHost struct {
	*fakeHost
	entered chan struct{}
}

func (h *slowKVHost) KVSet(ctx context.Context, r *pluginv1.KVSetRequest, opts ...grpc.CallOption) (*pluginv1.KVSetResponse, error) {
	close(h.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestSlowPersistenceDoesNotBlockHealthOrConfig(t *testing.T) {
	h := &slowKVHost{fakeHost: testHost(42), entered: make(chan struct{})}
	pool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(StateHeader, testState(292))
		completed(w, "gpt-6-astra")
	}))
	defer pool.Close()
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { completed(w, "gpt-6-astra") }))
	defer business.Close()
	e := newEngine(h, business.URL, 5*time.Millisecond)
	defer e.Close()
	e.mu.Lock()
	e.warmup = 0
	e.mu.Unlock()
	c := testConfig(pool.URL, 42)
	apply(t, e, c)
	select {
	case <-h.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("KV write not reached")
	}
	started := time.Now()
	e.Health(context.Background(), &pluginv1.HealthRequest{})
	c.Enabled = false
	apply(t, e, c)
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("slow KV held global business/status mutex")
	}
	time.Sleep(20 * time.Millisecond)
	e.mu.Lock()
	n := len(e.tickets)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("cancelled persistence committed stale ticket")
	}
}
