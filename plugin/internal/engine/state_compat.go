package engine

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	routeSessionTTL = 60 * time.Minute
)

var gatewayNumberPattern = regexp.MustCompile(`unified[-_.]?(\d+)`)

type routeSession struct {
	Cookies   map[string]string `json:"cookies"`
	Gateway   string            `json:"gateway,omitempty"`
	ExpiresAt time.Time         `json:"expires_at,omitempty"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// 780 STATE is Fernet-compatible in observed samples: 0x80 followed by an
// eight-byte big-endian Unix timestamp. Parsing the timestamp does not attempt
// to verify the MAC; it only prevents an old ticket from inheriting a long TTL.
func fernetIssuedAt(state string) (time.Time, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(state))
	if err != nil || len(raw) < 9 || raw[0] != 0x80 {
		return time.Time{}, false
	}
	seconds := binary.BigEndian.Uint64(raw[1:9])
	if seconds < 1_700_000_000 || seconds > uint64(time.Now().Add(5*time.Minute).Unix()) {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0).UTC(), true
}

func ticketIssuedAt(state string, fallback time.Time) time.Time {
	if issuedAt, ok := fernetIssuedAt(state); ok {
		return issuedAt
	}
	return fallback.UTC()
}

func stateRequiresSession(state string) bool {
	return validState(state, compatStateLength)
}

func routeCookies(cookies map[string]string) map[string]string {
	result := map[string]string{}
	for _, name := range []string{"__cflb", "__oailb"} {
		if value := strings.TrimSpace(cookies[name]); value != "" {
			result[name] = value
		}
	}
	if len(result) != 2 {
		return nil
	}
	return result
}

func gatewayFromCookies(cookies map[string]string) string {
	for _, name := range []string{"__oailb", "__cflb"} {
		value := strings.TrimSpace(cookies[name])
		if value == "" {
			continue
		}
		if match := gatewayNumberPattern.FindStringSubmatch(jwtPayload(value)); len(match) == 2 {
			return "unified-" + match[1]
		}
		if match := gatewayNumberPattern.FindStringSubmatch(value); len(match) == 2 {
			return "unified-" + match[1]
		}
	}
	return ""
}

func jwtPayload(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(b)
}

func jwtExpiry(cookies map[string]string) time.Time {
	value := strings.TrimSpace(cookies["__oailb"])
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var payload struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &payload) != nil || payload.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(payload.Exp, 0).UTC()
}

func cookieFingerprint(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+cookies[name])
	}
	return digest(strings.Join(parts, "\x00"))
}

func stateFingerprint(state string) string {
	if strings.TrimSpace(state) == "" {
		return ""
	}
	return digest(state)
}

func stateAgeSeconds(state string, now time.Time) int64 {
	issuedAt, ok := fernetIssuedAt(state)
	if !ok {
		return 0
	}
	age := now.Sub(issuedAt)
	if age < 0 {
		return 0
	}
	return int64(age / time.Second)
}

func routeGatewayAcceptance(state string, cookies map[string]string, targetGateways string) (string, string) {
	gateway := gatewayFromCookies(cookies)
	if !stateRequiresSession(state) || strings.TrimSpace(targetGateways) == "" {
		return gateway, ""
	}
	if len(routeCookies(cookies)) == 0 {
		return gateway, "gateway_unavailable"
	}
	if gateway == "" {
		return "", "gateway_unknown"
	}
	if !gatewayAllowed(gateway, targetGateways) {
		return gateway, "gateway_mismatch"
	}
	return gateway, ""
}

func (e *Engine) preferredRouteCookies(accountID int64, targetGateways string) map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	session, ok := e.routeSessions[accountID]
	if !ok {
		return nil
	}
	if session.ExpiresAt.IsZero() || !session.ExpiresAt.After(time.Now().Add(30*time.Second)) {
		delete(e.routeSessions, accountID)
		return nil
	}
	if !gatewayAllowed(session.Gateway, targetGateways) {
		delete(e.routeSessions, accountID)
		return nil
	}
	return cloneCookies(session.Cookies)
}

func (e *Engine) rememberRouteCookies(accountID int64, cookies map[string]string, targetGateways string) {
	routes := routeCookies(cookies)
	if len(routes) == 0 {
		return
	}
	gateway := gatewayFromCookies(routes)
	if !gatewayAllowed(gateway, targetGateways) {
		return
	}
	now := time.Now().UTC()
	expiresAt := jwtExpiry(routes)
	if expiresAt.IsZero() {
		expiresAt = now.Add(routeSessionTTL)
	}
	if !expiresAt.After(now.Add(30 * time.Second)) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.routeSessions == nil {
		e.routeSessions = map[int64]routeSession{}
	}
	e.routeSessions[accountID] = routeSession{
		Cookies: routes, Gateway: gateway, ExpiresAt: expiresAt, UpdatedAt: now,
	}
}
