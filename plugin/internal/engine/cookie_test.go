package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
)

func TestCookieModeSplitsCaptureAndBusinessAndQueuesStandby(t *testing.T) {
	h := testHost(42)
	var captureCalls, businessCalls atomic.Int32
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"203.0.113.1"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"203.0.113.1"}`))
			return
		}
		if r.Header.Get(StateHeader) != "" {
			t.Error("cookie capture unexpectedly carried STATE")
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("cookie capture unexpectedly carried a cookie")
		}
		if r.Header.Get("session_id") == "" {
			t.Error("cookie capture omitted session_id")
		}
		captureCalls.Add(1)
		setTargetRouteCookies(w)
		http.SetCookie(w, &http.Cookie{Name: "capture", Value: "one"})
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer capture.Close()

	var businessSessions = map[string]bool{}
	business := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = w.Write([]byte(`{"ip":"198.51.100.2"}`))
			return
		case "geo.test":
			_, _ = w.Write([]byte(`{"status":"success","countryCode":"US","query":"198.51.100.2"}`))
			return
		}
		sessionID := r.Header.Get("session_id")
		if sessionID == "" {
			t.Error("cookie business request omitted session_id")
		}
		if strings.Contains(r.Header.Get("Cookie"), "caller=") {
			t.Errorf("cookie business request retained caller cookie: %q", r.Header.Get("Cookie"))
		}
		if !strings.Contains(r.Header.Get("Cookie"), "capture=one") {
			t.Errorf("cookie business request omitted capture cookie: %q", r.Header.Get("Cookie"))
		}
		if r.Header.Get(StateHeader) == "" {
			t.Error("cookie business request omitted STATE")
		}
		businessSessions[sessionID] = true
		businessCalls.Add(1)
		setTargetRouteCookies(w)
		http.SetCookie(w, &http.Cookie{Name: "business", Value: "two"})
		w.Header().Set(StateHeader, testState(780))
		completed(w, "gpt-6-astra")
	}))
	defer business.Close()

	e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(capture.URL, 42)
	c.TicketMode = ticketModeCookie
	c.AllowState780 = true
	c.CookieCaptureMode = captureModeSOCKS5
	c.CookieCaptureProxyURL = capture.URL
	c.CookieBusinessProxyURL = business.URL
	c.RouteCookieReuse = false
	c.CookieTicketTTLSeconds = 30
	c.StandbyTicketEnabled = true
	c.StandbyLeadSeconds = 29
	c.AttemptIntervalSeconds = 1
	apply(t, e, c)
	waitFor(t, e, "ready")
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		key := keyFor(42, "gpt-6-astra")
		ready := e.standbyTickets[key] != nil
		e.mu.Unlock()
		if ready {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if captureCalls.Load() < 2 || businessCalls.Load() < 2 {
		t.Fatalf("standby ticket was not queued: capture=%d business=%d", captureCalls.Load(), businessCalls.Load())
	}

	receipt, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || receipt == nil {
		t.Fatalf("cookie ticket unavailable: %v", err)
	}
	if receipt.TargetProxyURL != business.URL {
		t.Fatalf("cookie business target = %q; want %q", receipt.TargetProxyURL, business.URL)
	}
	if receipt.TicketMode != ticketModeCookie || receipt.SessionID == "" ||
		receipt.Cookies["capture"] != "one" || receipt.Cookies["business"] != "two" {
		t.Fatalf("cookie session was not preserved: %+v", receipt)
	}
	if !businessSessions[receipt.SessionID] {
		t.Fatal("business validation did not use the capture session_id")
	}

	body := []byte(`{"model":"gpt-6-astra","stream":true}`)
	start := testStart(42)
	start.Method = http.MethodPost
	start.Url = "http://chatgpt.example/backend-api/codex/responses"
	start.HasBody = true
	start.ContentLength = int64(len(body))
	start.Headers["Cookie"] = &pluginv1.HeaderValues{Values: []string{"caller=bad"}}
	start.Headers["session_id"] = &pluginv1.HeaderValues{Values: []string{"caller-session"}}
	stream := fixedStream(start, body)
	if err := e.Forward(stream); err != nil {
		t.Fatalf("cookie business forward failed: %v", err)
	}
	if businessCalls.Load() < 3 {
		t.Fatalf("cookie business request did not reach business egress: %d", businessCalls.Load())
	}
	e.mu.Lock()
	updated := e.tickets[keyFor(42, "gpt-6-astra")]
	updatedCookies := ""
	if updated != nil {
		updatedCookies = cookieHeader(updated.Cookies)
	}
	e.mu.Unlock()
	if !strings.Contains(updatedCookies, "business=two") {
		t.Fatalf("business response cookies were not merged: %q", updatedCookies)
	}
}

func TestCookieConfigRejectsInvalidModesAndBounds(t *testing.T) {
	for _, raw := range []string{
		`{"ticket_mode":"other"}`,
		`{"cookie_capture_mode":"other"}`,
		`{"cookie_ticket_ttl_seconds":29}`,
		`{"ticket_mode":"cookie","standby_lead_seconds":301,"cookie_ticket_ttl_seconds":300}`,
		`{"enabled":true,"ticket_mode":"cookie","cookie_capture_mode":"socks5","accounts":[{"account_id":1,"enabled":true}]}`,
		`{"enabled":true,"ticket_mode":"cookie","cookie_capture_mode":"generator","accounts":[{"account_id":1,"enabled":true}]}`,
	} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Errorf("accepted invalid cookie config: %s", raw)
		}
	}
	valid, err := ParseConfig([]byte(`{
		"enabled":true,
		"ticket_mode":"cookie",
		"cookie_capture_mode":"socks5",
		"cookie_capture_proxy_url":"socks5h://capture.example:1080",
		"cookie_business_proxy_url":"http://business.example:8080",
		"dynamic_proxy_url":"http://dynamic.example:8080",
		"cookie_ticket_ttl_seconds":300,
		"standby_ticket_enabled":true,
		"standby_lead_seconds":90,
		"accounts":[{"account_id":1,"enabled":true,"egress_mode":"sub2"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if valid.TicketMode != ticketModeCookie || valid.CookieCaptureMode != captureModeSOCKS5 || !valid.StandbyTicketEnabled {
		t.Fatalf("cookie config not normalized: %+v", valid)
	}
	if effectiveTicketTTL(valid, valid.Accounts[0]) != 300*time.Second {
		t.Fatal("cookie ticket TTL did not use seconds")
	}
}

func TestCookieReceiptNeverPersistsProxyCredentials(t *testing.T) {
	state := ticket{
		State:           testState(292),
		SessionID:       "session",
		Cookies:         map[string]string{"capture": "one"},
		CaptureProxyURL: "socks5h://user:secret@capture.example:1080",
		CaptureEgressIP: "203.0.113.1",
	}
	raw := jsonText(state)
	if strings.Contains(raw, "capture.example") || strings.Contains(raw, "secret") {
		t.Fatalf("ticket JSON leaked credentials: %s", raw)
	}
}

func TestCookieBusinessResponseRollsStateForward(t *testing.T) {
	c := DefaultConfig()
	c.TicketMode = ticketModeCookie
	c.AllowState780 = true
	a := AccountConfig{AccountID: 7, Plan: "pro", Enabled: true, Models: []string{"gpt-6-astra"}}
	current := &ticket{
		AccountID: 7, Model: "gpt-6-astra", Plan: "pro", State: testState(780), Version: "version",
		ConfigFingerprint: configFingerprint(c, a, "gpt-6-astra"), TicketMode: ticketModeCookie,
		Cookies: map[string]string{"__cf_bm": "old", "__cflb": "lb", "__oailb": "unified-88"}, SessionID: "session",
	}
	e := &Engine{config: c, tickets: map[string]*ticket{keyFor(7, "gpt-6-astra"): current}}
	receipt := &receipt{
		Key: keyFor(7, "gpt-6-astra"), Version: current.Version, ConfigFingerprint: current.ConfigFingerprint,
		TicketMode: ticketModeCookie,
	}
	response := &http.Response{Header: make(http.Header)}
	response.Header.Set(StateHeader, testState(780))
	response.Header.Add("Set-Cookie", "__cf_bm=next; Path=/; Secure")
	e.updateTicketSession(receipt, response)

	if current.State != testState(780) {
		t.Fatalf("state did not roll forward: %q", current.State)
	}
	if current.Cookies["__cf_bm"] != "next" {
		t.Fatalf("__cf_bm did not roll forward: %q", current.Cookies["__cf_bm"])
	}
}

func TestStandbyPromotionRequiresBusinessEgressAndIdentity(t *testing.T) {
	businessProxy := "http://business.example:8080"
	fingerprint := proxyFingerprint(businessProxy)
	main := &ticket{FixedFingerprint: fingerprint, IdentityFingerprint: "identity-one"}
	standby := &ticket{FixedFingerprint: fingerprint, IdentityFingerprint: "identity-one"}

	if !standbyMatches(standby, main, fingerprint, "") {
		t.Fatal("matching standby ticket was rejected")
	}
	if standbyMatches(&ticket{FixedFingerprint: proxyFingerprint("http://other.example:8080"), IdentityFingerprint: "identity-one"}, main, fingerprint, "") {
		t.Fatal("standby ticket with a different business egress was accepted")
	}
	if standbyMatches(&ticket{FixedFingerprint: fingerprint, IdentityFingerprint: "identity-two"}, main, fingerprint, "") {
		t.Fatal("standby ticket with a different account identity was accepted")
	}
	if standbyMatches(standby, nil, fingerprint, "") {
		t.Fatal("standby ticket without an identity reference was accepted")
	}
	if !standbyMatches(standby, nil, fingerprint, "identity-one") {
		t.Fatal("standby ticket with an explicit request identity was rejected")
	}
}

func TestCookieTicketRequiresSessionAndCookie(t *testing.T) {
	c := DefaultConfig()
	c.TicketMode = ticketModeCookie
	c.AllowState780 = true
	a := AccountConfig{AccountID: 7, Plan: "pro", Enabled: true, Models: []string{"gpt-6-astra"}}
	now := time.Now()
	ticket := ticket{
		AccountID: 7, Model: "gpt-6-astra", Plan: "pro", State: testState(780), Version: "version",
		ConfigFingerprint: configFingerprint(c, a, "gpt-6-astra"), FixedFingerprint: "fixed",
		IdentityFingerprint: "identity", CapturedAt: now.Add(-time.Second), ExpiresAt: now.Add(30 * time.Second),
		TicketMode: ticketModeCookie,
	}
	if validTicket(&ticket, c, a, "gpt-6-astra", now) {
		t.Fatal("cookie ticket without session_id or cookies was accepted")
	}
	ticket.SessionID = "session"
	if validTicket(&ticket, c, a, "gpt-6-astra", now) {
		t.Fatal("cookie ticket without cookies was accepted")
	}
	ticket.Cookies = map[string]string{"__cflb": "lb", "__oailb": "unified-88"}
	if !validTicket(&ticket, c, a, "gpt-6-astra", now) {
		t.Fatal("complete cookie ticket was rejected")
	}
}
