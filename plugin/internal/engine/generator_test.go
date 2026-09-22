package engine

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseGeneratedProxy(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"hostPort", "128.14.147.102:10082\n", "http://128.14.147.102:10082"},
		{"httpURL", "http://proxy.example:8080\n", "http://proxy.example:8080"},
		{"httpsURL", "https://proxy.example:443\n", "https://proxy.example:443"},
		{"firstUsableLine", "not-a-proxy\nproxy.example:1080\nproxy.example:1081\n", "http://proxy.example:1080"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGeneratedProxy([]byte(tc.body))
			if err != nil || got != tc.want {
				t.Fatalf("parseGeneratedProxy() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, body := range []string{"", "proxy.example", "socks5://user:pass@proxy.example:1080", "http://proxy.example:0"} {
		if got, err := parseGeneratedProxy([]byte(body)); err == nil {
			t.Fatalf("accepted invalid generated proxy %q as %q", body, got)
		}
	}
}

func TestGeneratorConfigAndCountryFilter(t *testing.T) {
	cfg := DefaultConfig()
	if len(cfg.ProxyGeneratorBlockedCountries) != 1 || cfg.ProxyGeneratorBlockedCountries[0] != "HK" || cfg.ProxyGeneratorTTLMinutes != 180 {
		t.Fatalf("unexpected generator defaults: %+v", cfg)
	}

	parsed, err := ParseConfig([]byte(`{
		"enabled":true,
		"proxy_generator_url":"http://generator.example/gen?zone=custom&ptype=1",
		"proxy_generator_blocked_countries":["us"," hk ","US",""],
		"proxy_generator_ttl_minutes":7,
		"accounts":[{"account_id":1,"enabled":true,"egress_mode":"generator"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ProxyGeneratorTTLMinutes != 7 || strings.Join(parsed.ProxyGeneratorBlockedCountries, ",") != "HK,US" {
		t.Fatalf("generator configuration was not normalized: %+v", parsed)
	}

	for _, raw := range []string{
		`{"proxy_generator_url":"ftp://generator.example/gen"}`,
		`{"proxy_generator_url":"http://user:pass@generator.example/gen"}`,
		`{"proxy_generator_url":"http://generator.example/gen#fragment"}`,
		`{"proxy_generator_blocked_countries":["HKG"]}`,
		`{"proxy_generator_blocked_countries":["H1"]}`,
		`{"proxy_generator_ttl_minutes":0}`,
		`{"proxy_generator_ttl_minutes":181}`,
		`{"enabled":true,"accounts":[{"account_id":1,"enabled":true,"egress_mode":"generator"}]}`,
	} {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid generator config %s", raw)
		}
	}
}

func TestParseEgressCountryAndBlocking(t *testing.T) {
	if got := parseEgressCountry([]byte(`{"status":"success","countryCode":"us","query":"203.0.113.9"}`), "203.0.113.9"); got != "US" {
		t.Fatalf("ip-api country = %q", got)
	}
	if got := parseEgressCountry([]byte(`{"success":true,"country_code":"gb","ip":"203.0.113.9"}`), "203.0.113.9"); got != "GB" {
		t.Fatalf("ipwho country = %q", got)
	}
	if got := parseEgressCountry([]byte(`{"status":"success","countryCode":"US","query":"198.51.100.1"}`), "203.0.113.9"); got != "" {
		t.Fatalf("accepted mismatched lookup IP: %q", got)
	}
	if !countryBlocked("HK", []string{"HK"}) || !countryBlocked("", []string{"HK"}) {
		t.Fatal("blocked or unknown country was accepted")
	}
	if countryBlocked("us", []string{"HK"}) {
		t.Fatal("allowed country was blocked")
	}
}

func TestGeneratorEgressCollectionPersistenceAndRestore(t *testing.T) {
	h := testHost(42)
	state := testState(292)
	var generatorCalls, probeHits, egressHits, geoHits atomic.Int32
	generatedProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			egressHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ip":"203.0.113.9"}`)
		case "geo.test":
			geoHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"US","query":"203.0.113.9"}`)
		default:
			probeHits.Add(1)
			w.Header().Set(StateHeader, state)
			completed(w, "gpt-6-astra")
		}
	}))
	defer generatedProxy.Close()

	generator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generatorCalls.Add(1)
		_, _ = fmt.Fprintln(w, strings.TrimPrefix(generatedProxy.URL, "http://"))
	}))
	defer generator.Close()

	newGeneratorEngine := func() *Engine {
		e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
		e.egressURL = "http://egress.test/ip"
		e.geoURLs = []string{"http://geo.test/json/{ip}"}
		return e
	}
	c := testConfig(generatedProxy.URL, 42)
	c.Accounts[0].EgressMode = egressModeGenerator
	c.ProxyGeneratorURL = generator.URL
	c.ProxyGeneratorTTLMinutes = 5
	c.MaxAttempts = 1

	e := newGeneratorEngine()
	apply(t, e, c)
	waitFor(t, e, "ready")
	if generatorCalls.Load() != 1 || probeHits.Load() != 2 || egressHits.Load() != 1 || geoHits.Load() != 1 {
		t.Fatalf("unexpected generator flow calls generator=%d probe=%d egress=%d geo=%d",
			generatorCalls.Load(), probeHits.Load(), egressHits.Load(), geoHits.Load())
	}
	receipt, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || receipt == nil {
		t.Fatalf("generator ticket unavailable: %v", err)
	}
	wantProxy := "http://" + strings.TrimPrefix(generatedProxy.URL, "http://")
	if receipt.TargetProxyURL != wantProxy {
		t.Fatalf("business target = %q; want %q", receipt.TargetProxyURL, wantProxy)
	}
	e.mu.Lock()
	stored := e.tickets[keyFor(42, "gpt-6-astra")]
	if stored == nil || stored.GeneratedProxyURL != wantProxy || time.Until(stored.ExpiresAt) > 5*time.Minute {
		e.mu.Unlock()
		t.Fatalf("generated proxy was not persisted in the ticket: %+v", stored)
	}
	e.mu.Unlock()

	e.Close()
	e2 := newGeneratorEngine()
	apply(t, e2, c)
	waitFor(t, e2, "ready")
	if generatorCalls.Load() != 1 || probeHits.Load() != 3 || egressHits.Load() != 2 {
		t.Fatalf("restore generated another proxy: generator=%d probe=%d egress=%d",
			generatorCalls.Load(), probeHits.Load(), egressHits.Load())
	}
}

func TestGeneratorPreferPreviousIPReusesAccountEgress(t *testing.T) {
	h := testHost(42)
	state := testState(292)
	var generatorCalls, probeHits atomic.Int32
	generatedProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ip":"203.0.113.9"}`)
		case "geo.test":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"US","query":"203.0.113.9"}`)
		default:
			probeHits.Add(1)
			w.Header().Set(StateHeader, state)
			completed(w, "gpt-6-astra")
		}
	}))
	defer generatedProxy.Close()
	generator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		generatorCalls.Add(1)
		_, _ = fmt.Fprintln(w, strings.TrimPrefix(generatedProxy.URL, "http://"))
	}))
	defer generator.Close()

	e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(generatedProxy.URL, 42)
	c.Accounts[0].EgressMode = egressModeGenerator
	c.ProxyGeneratorURL = generator.URL
	c.ProxyGeneratorTTLMinutes = 5
	c.PreferPreviousIP = true
	c.MaxAttempts = 1
	apply(t, e, c)
	waitFor(t, e, "ready")

	first, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || first == nil {
		t.Fatalf("initial generator ticket unavailable: %v", err)
	}
	key := keyFor(42, "gpt-6-astra")
	e.mu.Lock()
	oldVersion := e.tickets[key].Version
	e.tickets[key].ExpiresAt = time.Now().Add(2 * time.Second)
	e.mu.Unlock()
	e.notify()

	var renewed *receipt
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		current := e.tickets[key]
		if current != nil && current.Version != oldVersion {
			renewed = &receipt{TargetProxyURL: current.GeneratedProxyURL}
		}
		e.mu.Unlock()
		if renewed != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if renewed == nil || renewed.TargetProxyURL != first.TargetProxyURL {
		t.Fatalf("previous egress was not reused: first=%q renewed=%+v", first.TargetProxyURL, renewed)
	}
	if generatorCalls.Load() != 1 {
		t.Fatalf("previous egress reuse called generator %d times", generatorCalls.Load())
	}
	if probeHits.Load() != 4 {
		t.Fatalf("previous egress reuse probe calls = %d; want 4", probeHits.Load())
	}
	h.mu.Lock()
	cached := append([]byte(nil), h.values[namespace+"/"+previousEgressKey(42)]...)
	h.mu.Unlock()
	if len(cached) == 0 || strings.Contains(string(cached), "test-token") {
		t.Fatalf("previous egress cache missing or contains credentials: %s", cached)
	}
}

func TestLegacyStandbyReusesCurrentGeneratedEgress(t *testing.T) {
	h := testHost(42)
	state := testState(292)
	var generatorCalls, probeHits atomic.Int32
	generatedProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = io.WriteString(w, `{"ip":"203.0.113.9"}`)
		case "geo.test":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"US","query":"203.0.113.9"}`)
		default:
			probeHits.Add(1)
			w.Header().Set(StateHeader, state)
			completed(w, "gpt-6-astra")
		}
	}))
	defer generatedProxy.Close()
	generator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		generatorCalls.Add(1)
		_, _ = fmt.Fprintln(w, strings.TrimPrefix(generatedProxy.URL, "http://"))
	}))
	defer generator.Close()

	e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(generatedProxy.URL, 42)
	c.Accounts[0].EgressMode = egressModeGenerator
	c.ProxyGeneratorURL = generator.URL
	c.ProxyGeneratorTTLMinutes = 5
	c.TTLMinutes = 5
	c.RefreshBeforeSeconds = 30
	c.MaxAttempts = 1
	c.StandbyTicketEnabled = true
	c.StandbyLeadSeconds = 60
	apply(t, e, c)
	waitFor(t, e, "ready")

	key := keyFor(42, "gpt-6-astra")
	e.mu.Lock()
	mainVersion := e.tickets[key].Version
	mainProxy := e.tickets[key].GeneratedProxyURL
	e.tickets[key].ExpiresAt = time.Now().Add(30 * time.Second)
	e.mu.Unlock()
	e.notify()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		standby := e.standbyTickets[key]
		currentVersion := e.tickets[key].Version
		e.mu.Unlock()
		if standby != nil {
			if currentVersion != mainVersion {
				t.Fatal("standby preparation replaced the still-valid main ticket")
			}
			if standby.GeneratedProxyURL != mainProxy {
				t.Fatalf("standby egress = %q; want current egress %q", standby.GeneratedProxyURL, mainProxy)
			}
			if generatorCalls.Load() != 1 {
				t.Fatalf("standby preparation called generator %d times; want current egress reuse only", generatorCalls.Load())
			}
			if probeHits.Load() != 4 {
				t.Fatalf("standby probe calls = %d; want initial capture/validation plus standby capture/validation", probeHits.Load())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("legacy standby ticket was not prepared")
}

func TestLegacyStandbyFallsBackToNewEgressAfterCurrentFails(t *testing.T) {
	h := testHost(42)
	state := testState(292)
	var standbyPhase, switchProxy atomic.Bool
	var generatorCount atomic.Int32
	firstProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = io.WriteString(w, `{"ip":"203.0.113.9"}`)
		case "geo.test":
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"US","query":"203.0.113.9"}`)
		default:
			if standbyPhase.Load() {
				w.Header().Set(StateHeader, testState(312))
			} else {
				w.Header().Set(StateHeader, state)
			}
			completed(w, "gpt-6-astra")
		}
	}))
	defer firstProxy.Close()
	secondProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = io.WriteString(w, `{"ip":"203.0.113.10"}`)
		case "geo.test":
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"US","query":"203.0.113.10"}`)
		default:
			w.Header().Set(StateHeader, state)
			completed(w, "gpt-6-astra")
		}
	}))
	defer secondProxy.Close()
	generator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		generatorCount.Add(1)
		target := firstProxy.URL
		if switchProxy.Load() {
			target = secondProxy.URL
		}
		_, _ = fmt.Fprintln(w, strings.TrimPrefix(target, "http://"))
	}))
	defer generator.Close()

	e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(firstProxy.URL, 42)
	c.Accounts[0].EgressMode = egressModeGenerator
	c.ProxyGeneratorURL = generator.URL
	c.ProxyGeneratorTTLMinutes = 5
	c.TTLMinutes = 5
	c.RefreshBeforeSeconds = 30
	c.MaxAttempts = 1
	c.AttemptIntervalSeconds = 10
	c.StandbyTicketEnabled = true
	c.StandbyLeadSeconds = 60
	apply(t, e, c)
	waitFor(t, e, "ready")

	key := keyFor(42, "gpt-6-astra")
	e.mu.Lock()
	mainProxy := e.tickets[key].GeneratedProxyURL
	e.tickets[key].ExpiresAt = time.Now().Add(30 * time.Second)
	e.mu.Unlock()
	standbyPhase.Store(true)
	switchProxy.Store(true)
	e.notify()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		standby := e.standbyTickets[key]
		e.mu.Unlock()
		if standby != nil {
			if standby.GeneratedProxyURL == mainProxy {
				t.Fatal("standby did not fall back to a fresh generated egress")
			}
			if generatorCount.Load() != 2 {
				t.Fatalf("generator calls = %d; want initial plus fallback", generatorCount.Load())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("standby fallback did not produce a ticket")
}

func TestGeneratorRejectsIPChangeDuringSameEgressValidation(t *testing.T) {
	h := testHost(42)
	state := testState(292)
	var egressHits atomic.Int32
	generatedProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			egressHits.Add(1)
			_, _ = io.WriteString(w, `{"ip":"203.0.113.10"}`)
		case "geo.test":
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"US","query":"203.0.113.9"}`)
		default:
			w.Header().Set(StateHeader, state)
			completed(w, "gpt-6-astra")
		}
	}))
	defer generatedProxy.Close()
	generator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, strings.TrimPrefix(generatedProxy.URL, "http://"))
	}))
	defer generator.Close()

	e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(generatedProxy.URL, 42)
	c.Accounts[0].EgressMode = egressModeGenerator
	c.ProxyGeneratorURL = generator.URL
	c.MaxAttempts = 1
	apply(t, e, c)
	waitFor(t, e, "cooldown")
	if egressHits.Load() != 1 {
		t.Fatalf("validation IP checks = %d; want 1", egressHits.Load())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if ticket := e.tickets[keyFor(42, "gpt-6-astra")]; ticket != nil {
		t.Fatal("ticket with rotated validation egress was accepted")
	}
}

func TestGeneratorPreferPreviousIPFallsBackWhenEgressFails(t *testing.T) {
	h := testHost(42)
	state := testState(292)
	var rejectFirst atomic.Bool
	var generatorCalls, firstProbeHits atomic.Int32
	firstProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = io.WriteString(w, `{"ip":"203.0.113.9"}`)
		case "geo.test":
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"US","query":"203.0.113.9"}`)
		default:
			firstProbeHits.Add(1)
			if rejectFirst.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set(StateHeader, state)
			completed(w, "gpt-6-astra")
		}
	}))
	defer firstProxy.Close()
	secondProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = io.WriteString(w, `{"ip":"198.51.100.24"}`)
		case "geo.test":
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"US","query":"198.51.100.24"}`)
		default:
			w.Header().Set(StateHeader, state)
			completed(w, "gpt-6-astra")
		}
	}))
	defer secondProxy.Close()
	generator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := generatorCalls.Add(1)
		target := firstProxy.URL
		if call > 1 {
			target = secondProxy.URL
		}
		_, _ = fmt.Fprintln(w, strings.TrimPrefix(target, "http://"))
	}))
	defer generator.Close()

	e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(firstProxy.URL, 42)
	c.Accounts[0].EgressMode = egressModeGenerator
	c.ProxyGeneratorURL = generator.URL
	c.ProxyGeneratorTTLMinutes = 5
	c.PreferPreviousIP = true
	c.MaxAttempts = 1
	c.AttemptIntervalSeconds = 1
	apply(t, e, c)
	waitFor(t, e, "ready")

	first, err := e.ticketForRequest(context.Background(), testStart(42), "gpt-6-astra")
	if err != nil || first == nil || first.TargetProxyURL != firstProxy.URL {
		t.Fatalf("initial generator ticket unavailable: %+v %v", first, err)
	}
	rejectFirst.Store(true)
	key := keyFor(42, "gpt-6-astra")
	e.mu.Lock()
	oldVersion := e.tickets[key].Version
	e.tickets[key].ExpiresAt = time.Now().Add(2 * time.Second)
	e.mu.Unlock()
	e.notify()

	var renewed *receipt
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		current := e.tickets[key]
		if current != nil && current.Version != oldVersion {
			renewed = &receipt{TargetProxyURL: current.GeneratedProxyURL}
		}
		e.mu.Unlock()
		if renewed != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if renewed == nil || renewed.TargetProxyURL != secondProxy.URL {
		t.Fatalf("failed previous egress did not fall back to generator: %+v", renewed)
	}
	if generatorCalls.Load() != 2 {
		t.Fatalf("generator calls after fallback = %d; want 2", generatorCalls.Load())
	}
	if firstProbeHits.Load() != 3 {
		t.Fatalf("previous egress probes = %d; want initial capture, validation, and renewal failure", firstProbeHits.Load())
	}
}

func TestGeneratorRejectsBlockedCountry(t *testing.T) {
	h := testHost(42)
	var generatorCalls, probeHits atomic.Int32
	generatedProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Hostname() {
		case "egress.test":
			_, _ = io.WriteString(w, `{"ip":"203.0.113.9"}`)
		case "geo.test":
			_, _ = io.WriteString(w, `{"status":"success","countryCode":"HK","query":"203.0.113.9"}`)
		default:
			probeHits.Add(1)
			completed(w, "gpt-6-astra")
		}
	}))
	defer generatedProxy.Close()
	generator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generatorCalls.Add(1)
		_, _ = fmt.Fprintln(w, strings.TrimPrefix(generatedProxy.URL, "http://"))
	}))
	defer generator.Close()

	e := testEngine(t, h, "http://chatgpt.example/backend-api/codex/responses")
	e.egressURL = "http://egress.test/ip"
	e.geoURLs = []string{"http://geo.test/json/{ip}"}
	c := testConfig(generatedProxy.URL, 42)
	c.Accounts[0].EgressMode = egressModeGenerator
	c.ProxyGeneratorURL = generator.URL
	c.MaxAttempts = 1
	apply(t, e, c)
	waitFor(t, e, "cooldown")
	if generatorCalls.Load() != 1 || probeHits.Load() != 0 {
		t.Fatalf("blocked generator egress reached upstream: generator=%d probe=%d", generatorCalls.Load(), probeHits.Load())
	}
	e.mu.Lock()
	record := e.records[keyFor(42, "gpt-6-astra")]
	e.mu.Unlock()
	if record == nil || record.LastError != "generator_region_blocked" {
		t.Fatalf("unexpected blocked-region record: %+v", record)
	}
}
