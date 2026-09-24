package engine

import (
	"encoding/base64"
	"net/http"
	"testing"
	"time"
)

func TestFernetIssuedAtAndStateTTL(t *testing.T) {
	issuedAt := time.Now().Add(-45 * time.Second).Truncate(time.Second)
	state := testFernetState(compatStateLength, issuedAt)
	got, ok := fernetIssuedAt(state)
	if !ok || !got.Equal(issuedAt) {
		t.Fatalf("fernet issued at = %v, %v; want %v", got, ok, issuedAt)
	}

	c := DefaultConfig()
	a := AccountConfig{AccountID: 7, Plan: "pro", Enabled: true, Models: []string{"gpt-6-astra"}}
	if ttl := effectiveTicketTTLForState(c, a, state); ttl != 240*time.Second {
		t.Fatalf("780 TTL = %s; want 4m", ttl)
	}
	if age := stateAgeSeconds(state, time.Now()); age < 44 || age > 47 {
		t.Fatalf("state age = %d; want about 45s", age)
	}
}

func TestRouteCookieSessionReuse(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"host":"chat.gateway.unified-88.api.openai.com","exp":4102444800}`))
	cookies := map[string]string{"__cflb": "lb-one", "__oailb": "a." + payload + ".c"}
	e := &Engine{}
	e.rememberRouteCookies(42, cookies, gatewayPolicyAllow, "unified-15,unified-88,unified-180")
	got := e.preferredRouteCookies(42, gatewayPolicyAllow, "unified-15,unified-88,unified-180")
	if got["__cflb"] != "lb-one" || got["__oailb"] != cookies["__oailb"] {
		t.Fatalf("route cookies were not reused: %+v", got)
	}
	if gateway := gatewayFromCookies(got); gateway != "unified-88" {
		t.Fatalf("gateway = %q; want unified-88", gateway)
	}
	if e.preferredRouteCookies(42, gatewayPolicyAny, "") == nil {
		t.Fatal("any-gateway policy did not reuse a valid route pair")
	}
	if e.preferredRouteCookies(42, gatewayPolicyAllow, "unified-15") != nil {
		t.Fatal("off-target route pair was reused")
	}
	if e.preferredRouteCookies(42, gatewayPolicyDeny, "unified-88") != nil {
		t.Fatal("deny policy reused a denied route pair")
	}
	e.rememberRouteCookies(42, cookies, gatewayPolicyAllow, "unified-15,unified-88,unified-180")
	if e.preferredRouteCookies(42, gatewayPolicyDeny, "unified-15,unified-180") == nil {
		t.Fatal("deny policy did not reuse an allowed route pair")
	}
}

func TestRouteGatewayAcceptance(t *testing.T) {
	onTarget := map[string]string{"__cflb": "lb", "__oailb": "unified-88"}
	if gateway, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), onTarget, gatewayPolicyAllow, "unified-15,unified-88,unified-180"); gateway != "unified-88" || reason != "" {
		t.Fatalf("on-target gateway rejected: gateway=%q reason=%q", gateway, reason)
	}
	offTarget := map[string]string{"__cflb": "lb", "__oailb": "unified-15"}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), offTarget, gatewayPolicyAllow, "unified-88"); reason != "gateway_mismatch" {
		t.Fatalf("off-target gateway reason = %q; want gateway_mismatch", reason)
	}
	alternate := map[string]string{"__cflb": "lb", "__oailb": "unified-15"}
	if gateway, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), alternate, gatewayPolicyAllow, "unified-15,unified-88,unified-180"); gateway != "unified-15" || reason != "" {
		t.Fatalf("alternate allowed gateway rejected: gateway=%q reason=%q", gateway, reason)
	}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), onTarget, gatewayPolicyDeny, "unified-88"); reason != "gateway_mismatch" {
		t.Fatalf("denied gateway reason = %q; want gateway_mismatch", reason)
	}
	if gateway, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), alternate, gatewayPolicyDeny, "unified-88"); gateway != "unified-15" || reason != "" {
		t.Fatalf("deny policy rejected an unlisted gateway: gateway=%q reason=%q", gateway, reason)
	}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), map[string]string{"__cf_bm": "only"}, gatewayPolicyAllow, "unified-88"); reason != "gateway_unavailable" {
		t.Fatalf("missing route pair reason = %q; want gateway_unavailable", reason)
	}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), map[string]string{"__cf_bm": "present", "__oailb": "unified-88"}, gatewayPolicyAllow, "unified-88"); reason != "gateway_unavailable" {
		t.Fatalf("missing __cflb reason = %q; want gateway_unavailable", reason)
	}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), map[string]string{"__cf_bm": "present", "__cflb": "lb"}, gatewayPolicyAllow, "unified-88"); reason != "gateway_unavailable" {
		t.Fatalf("missing __oailb reason = %q; want gateway_unavailable", reason)
	}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), map[string]string{"__cf_bm": "only"}, gatewayPolicyDeny, "unified-88"); reason != "gateway_unavailable" {
		t.Fatalf("deny policy accepted a missing route pair: %q", reason)
	}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), map[string]string{"__cf_bm": "only"}, gatewayPolicyAny, ""); reason != "gateway_unavailable" {
		t.Fatalf("any policy accepted a missing route pair: %q", reason)
	}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), map[string]string{"__cflb": "lb", "__oailb": "unknown"}, gatewayPolicyDeny, "unified-88"); reason != "gateway_unknown" {
		t.Fatalf("deny policy accepted an unknown gateway: %q", reason)
	}
	if _, reason := routeGatewayAcceptance(testFernetState(780, time.Now()), offTarget, gatewayPolicyAny, ""); reason != "" {
		t.Fatalf("any-gateway policy rejected: %q", reason)
	}
}

func TestValidTicketAllowsSmallIssuedAtClockSkew(t *testing.T) {
	c := DefaultConfig()
	c.AllowState780 = true
	a := AccountConfig{AccountID: 7, Plan: "pro", Enabled: true, Models: []string{"gpt-6-astra"}}
	now := time.Now().UTC()
	issuedAt := now.Add(30 * time.Second).Truncate(time.Second)
	ticket := &ticket{
		AccountID: 7, Model: "gpt-6-astra", Plan: "pro", State: testFernetState(780, issuedAt),
		Version: "version", ConfigFingerprint: configFingerprint(c, a, "gpt-6-astra"), FixedFingerprint: "fixed",
		IdentityFingerprint: "identity", CapturedAt: now, IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(240 * time.Second),
		SessionBound: true, SessionID: "session",
		Cookies: map[string]string{"__cflb": "lb", "__oailb": "unified-88"},
	}
	if !validTicket(ticket, c, a, "gpt-6-astra", now) {
		t.Fatal("small issued-at clock skew was rejected")
	}
	ticket.ExpiresAt = issuedAt.Add(241 * time.Second)
	if validTicket(ticket, c, a, "gpt-6-astra", now) {
		t.Fatal("ticket longer than the 780 TTL was accepted")
	}
}

func TestLegacy780TicketRequiresSessionAndRollsForward(t *testing.T) {
	c := DefaultConfig()
	c.AllowState780 = true
	a := AccountConfig{AccountID: 7, Plan: "pro", Enabled: true, Models: []string{"gpt-6-astra"}}
	now := time.Now().UTC()
	issuedAt := now.Add(-20 * time.Second).Truncate(time.Second)
	current := &ticket{
		AccountID: 7, Model: "gpt-6-astra", Plan: "pro", State: testFernetState(780, issuedAt),
		Version: "version", ConfigFingerprint: configFingerprint(c, a, "gpt-6-astra"), FixedFingerprint: "fixed",
		IdentityFingerprint: "identity", CapturedAt: now, IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(240 * time.Second),
		SessionBound: true, SessionID: "session",
		Cookies: map[string]string{"__cf_bm": "old", "__cflb": "lb", "__oailb": "unified-88"},
	}
	if !validTicket(current, c, a, "gpt-6-astra", now) {
		t.Fatal("complete legacy 780 ticket was rejected")
	}
	missingCookies := *current
	missingCookies.Cookies = nil
	if validTicket(&missingCookies, c, a, "gpt-6-astra", now) {
		t.Fatal("legacy 780 ticket without cookies was accepted")
	}
	c.GatewayPolicy = gatewayPolicyAny
	c.TargetGateway = ""
	for name, cookies := range map[string]map[string]string{
		"missing __cflb":  {"__cf_bm": "present", "__oailb": "unified-88"},
		"missing __oailb": {"__cf_bm": "present", "__cflb": "lb"},
	} {
		t.Run(name, func(t *testing.T) {
			missingRouteCookie := *current
			missingRouteCookie.Cookies = cookies
			missingRouteCookie.ConfigFingerprint = configFingerprint(c, a, "gpt-6-astra")
			if validTicket(&missingRouteCookie, c, a, "gpt-6-astra", now) {
				t.Fatal("legacy 780 ticket without a complete route pair was accepted")
			}
		})
	}

	e := &Engine{config: c, tickets: map[string]*ticket{keyFor(7, "gpt-6-astra"): current}}
	receipt := &receipt{
		Key: keyFor(7, "gpt-6-astra"), Version: current.Version, ConfigFingerprint: current.ConfigFingerprint,
		TicketMode: ticketModeLegacy, SessionBound: true,
	}
	response := &http.Response{Header: make(http.Header)}
	response.Header.Set(StateHeader, testFernetState(780, now.Truncate(time.Second)))
	response.Header.Add("Set-Cookie", "__cf_bm=next; Path=/; Secure")
	e.updateTicketSession(receipt, response)
	if current.Cookies["__cf_bm"] != "next" {
		t.Fatalf("legacy 780 cookie did not roll forward: %q", current.Cookies["__cf_bm"])
	}
}
