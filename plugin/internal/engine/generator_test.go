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
	if len(cfg.ProxyGeneratorBlockedCountries) != 1 || cfg.ProxyGeneratorBlockedCountries[0] != "HK" || cfg.ProxyGeneratorTTLMinutes != 5 {
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
	if generatorCalls.Load() != 1 || probeHits.Load() != 2 || egressHits.Load() != 2 || geoHits.Load() != 1 {
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
	if generatorCalls.Load() != 1 || probeHits.Load() != 3 || egressHits.Load() != 3 {
		t.Fatalf("restore generated another proxy: generator=%d probe=%d egress=%d",
			generatorCalls.Load(), probeHits.Load(), egressHits.Load())
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
