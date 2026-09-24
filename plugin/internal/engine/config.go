package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const PluginID = "io.github.wangyunjeff.sub2api-state-kit"
const Version = "4.4.0"
const StateHeader = "x-codex-turn-state"
const namespace = "state-kit-v1"

const (
	defaultRequestTimezone = "Asia/Singapore"
	defaultAcceptLanguage  = "en-US,en;q=0.9"
	defaultTargetGateway   = "unified-88"
	compatStateLength      = 780
	compatStateTTLSeconds  = 240
)

const (
	egressModeSub2      = "sub2"
	egressModePlugin    = "plugin"
	egressModeGenerator = "generator"
)

const (
	ticketModeLegacy = "legacy"
	ticketModeCookie = "cookie"
)

const (
	captureModeGenerator = "generator"
	captureModeSOCKS5    = "socks5"
)

// Config contains no OAuth credentials. The host owns credential refresh.
type Config struct {
	Enabled                        bool     `json:"enabled"`
	UpstreamProxyID                int64    `json:"upstream_proxy_id"`
	UpstreamProxyURL               string   `json:"upstream_proxy_url"`
	DynamicProxyURL                string   `json:"dynamic_proxy_url"`
	ProxyGeneratorURL              string   `json:"proxy_generator_url"`
	ProxyGeneratorBlockedCountries []string `json:"proxy_generator_blocked_countries"`
	ProxyGeneratorTTLMinutes       int      `json:"proxy_generator_ttl_minutes"`
	PreferPreviousIP               bool     `json:"prefer_previous_ip"`
	AllowState780                  bool     `json:"allow_state_780"`
	State780TTLSeconds             int      `json:"state_780_ttl_seconds"`
	TargetGateway                  string   `json:"target_gateway"`
	RouteCookieReuse               bool     `json:"route_cookie_reuse"`
	MintFingerprintConvergence     bool     `json:"mint_fingerprint_convergence"`
	TicketMode                     string   `json:"ticket_mode"`
	CookieCaptureMode              string   `json:"cookie_capture_mode"`
	CookieCaptureProxyURL          string   `json:"cookie_capture_proxy_url"`
	CookieBusinessProxyURL         string   `json:"cookie_business_proxy_url"`
	CookieTicketTTLSeconds         int      `json:"cookie_ticket_ttl_seconds"`
	StandbyTicketEnabled           bool     `json:"standby_ticket_enabled"`
	StandbyLeadSeconds             int      `json:"standby_lead_seconds"`
	RequestRewriteEnabled          bool     `json:"request_rewrite_enabled"`
	DefaultRequestTimezone         string   `json:"default_request_timezone"`
	// DiagnosticLogEnabled is retained only so configurations saved by v0.3.6
	// remain loadable. Diagnostics are now a UI-scoped live listener and the
	// value is intentionally ignored.
	DiagnosticLogEnabled   bool            `json:"diagnostic_log_enabled,omitempty"`
	TTLMinutes             int             `json:"ttl_minutes"`
	RefreshBeforeSeconds   int             `json:"refresh_before_seconds"`
	RefreshBeforeMinutes   int             `json:"refresh_before_minutes,omitempty"` // legacy v0.3.8
	MaxAttempts            int             `json:"max_attempts"`
	AttemptIntervalSeconds int             `json:"attempt_interval_seconds"`
	CooldownSeconds        int             `json:"cooldown_seconds"`
	Accounts               []AccountConfig `json:"accounts"`
}
type AccountConfig struct {
	AccountID       int64    `json:"account_id"`
	Name            string   `json:"name,omitempty"`
	Email           string   `json:"email,omitempty"`
	ExpiresAt       string   `json:"expires_at,omitempty"`
	Quota           string   `json:"quota,omitempty"`
	Enabled         bool     `json:"enabled"`
	EgressMode      string   `json:"egress_mode"`
	StickyProxyURL  string   `json:"sticky_proxy_url,omitempty"`
	Plan            string   `json:"plan"`
	RequestTimezone string   `json:"request_timezone,omitempty"`
	Models          []string `json:"models"`
}

func DefaultConfig() Config {
	return Config{
		TTLMinutes:                     180,
		RefreshBeforeSeconds:           120,
		MaxAttempts:                    8,
		AttemptIntervalSeconds:         10,
		CooldownSeconds:                300,
		ProxyGeneratorBlockedCountries: []string{"HK"},
		ProxyGeneratorTTLMinutes:       180,
		State780TTLSeconds:             compatStateTTLSeconds,
		TargetGateway:                  defaultTargetGateway,
		RouteCookieReuse:               true,
		MintFingerprintConvergence:     true,
		TicketMode:                     ticketModeLegacy,
		CookieCaptureMode:              captureModeGenerator,
		CookieTicketTTLSeconds:         300,
		StandbyLeadSeconds:             90,
		DefaultRequestTimezone:         defaultRequestTimezone,
		Accounts:                       []AccountConfig{},
	}
}

var modelPattern = regexp.MustCompile(`^gpt-[A-Za-z0-9][A-Za-z0-9._-]{0,94}$`)

// ParseConfig rejects unknown fields, trailing JSON, and invalid ranges without
// echoing user input (which can include authenticated proxy URLs).
func ParseConfig(raw []byte) (Config, error) {
	c := DefaultConfig()
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if len(raw) > 1<<20 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return c, errors.New("configuration must be a JSON object under 1 MiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, errors.New("invalid configuration JSON or unknown field")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return c, errors.New("configuration has trailing JSON")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return c, errors.New("invalid configuration JSON")
	}
	_, hasSeconds := fields["refresh_before_seconds"]
	_, hasMinutes := fields["refresh_before_minutes"]
	if hasMinutes && !hasSeconds {
		if c.RefreshBeforeMinutes < 0 || c.RefreshBeforeMinutes > 3599/60 {
			return c, errors.New("refresh_before_minutes must be nonnegative and less than ttl_minutes")
		}
		c.RefreshBeforeSeconds = c.RefreshBeforeMinutes * 60
	}
	c.RefreshBeforeMinutes = 0
	c.UpstreamProxyURL = strings.TrimSpace(c.UpstreamProxyURL)
	c.DynamicProxyURL = strings.TrimSpace(c.DynamicProxyURL)
	c.ProxyGeneratorURL = strings.TrimSpace(c.ProxyGeneratorURL)
	c.TicketMode = strings.ToLower(strings.TrimSpace(c.TicketMode))
	if c.TicketMode == "" {
		c.TicketMode = ticketModeLegacy
	}
	c.CookieCaptureMode = strings.ToLower(strings.TrimSpace(c.CookieCaptureMode))
	if c.CookieCaptureMode == "" {
		c.CookieCaptureMode = captureModeGenerator
	}
	c.CookieCaptureProxyURL = strings.TrimSpace(c.CookieCaptureProxyURL)
	c.CookieBusinessProxyURL = strings.TrimSpace(c.CookieBusinessProxyURL)
	targetGateway, err := normalizeTargetGateway(c.TargetGateway)
	if err != nil {
		return c, err
	}
	c.TargetGateway = targetGateway
	c.DefaultRequestTimezone = strings.TrimSpace(c.DefaultRequestTimezone)
	if c.DefaultRequestTimezone == "" {
		c.DefaultRequestTimezone = defaultRequestTimezone
	}
	if c.UpstreamProxyID < 0 {
		return c, errors.New("upstream_proxy_id must be nonnegative")
	}
	if c.TicketMode != ticketModeLegacy && c.TicketMode != ticketModeCookie {
		return c, errors.New("ticket_mode must be legacy or cookie")
	}
	if c.CookieCaptureMode != captureModeGenerator && c.CookieCaptureMode != captureModeSOCKS5 {
		return c, errors.New("cookie_capture_mode must be generator or socks5")
	}
	if c.TTLMinutes < 1 || c.TTLMinutes > 180 {
		return c, errors.New("ttl_minutes must be 1..180")
	}
	if c.RefreshBeforeSeconds < 0 || c.RefreshBeforeSeconds >= c.TTLMinutes*60 {
		return c, errors.New("refresh_before_seconds must be nonnegative and less than ttl_minutes")
	}
	if c.MaxAttempts < 1 || c.MaxAttempts > 32 {
		return c, errors.New("max_attempts must be 1..32")
	}
	if c.AttemptIntervalSeconds < 1 || c.AttemptIntervalSeconds > 300 {
		return c, errors.New("attempt_interval_seconds must be 1..300")
	}
	if c.CooldownSeconds < 30 || c.CooldownSeconds > 3600 {
		return c, errors.New("cooldown_seconds must be 30..3600")
	}
	if c.ProxyGeneratorTTLMinutes < 1 || c.ProxyGeneratorTTLMinutes > 180 {
		return c, errors.New("proxy_generator_ttl_minutes must be 1..180")
	}
	if c.State780TTLSeconds < 30 || c.State780TTLSeconds > 1800 {
		return c, errors.New("state_780_ttl_seconds must be 30..1800")
	}
	if c.CookieTicketTTLSeconds < 30 || c.CookieTicketTTLSeconds > 1800 {
		return c, errors.New("cookie_ticket_ttl_seconds must be 30..1800")
	}
	if c.StandbyLeadSeconds < 10 || c.StandbyLeadSeconds > 600 {
		return c, errors.New("standby_lead_seconds must be 10..600")
	}
	if err := validateRequestTimezone(c.DefaultRequestTimezone); err != nil {
		return c, err
	}
	if c.TicketMode == ticketModeCookie && c.StandbyLeadSeconds >= c.CookieTicketTTLSeconds {
		return c, errors.New("standby_lead_seconds must be less than cookie_ticket_ttl_seconds")
	}
	if c.TicketMode == ticketModeLegacy && c.StandbyLeadSeconds >= c.TTLMinutes*60 {
		return c, errors.New("standby_lead_seconds must be less than ttl_minutes")
	}
	if c.AllowState780 && c.StandbyLeadSeconds >= c.State780TTLSeconds {
		return c, errors.New("standby_lead_seconds must be less than state_780_ttl_seconds")
	}
	if err := validateProxy(c.UpstreamProxyURL); err != nil {
		return c, err
	}
	if err := validateProxy(c.DynamicProxyURL); err != nil {
		return c, err
	}
	if err := validateProxy(c.CookieCaptureProxyURL); err != nil {
		return c, err
	}
	if err := validateProxy(c.CookieBusinessProxyURL); err != nil {
		return c, err
	}
	if err := validateProxyGeneratorURL(c.ProxyGeneratorURL); err != nil {
		return c, err
	}
	blockedCountries, err := normalizeBlockedCountries(c.ProxyGeneratorBlockedCountries)
	if err != nil {
		return c, err
	}
	c.ProxyGeneratorBlockedCountries = blockedCountries
	if c.UpstreamProxyID > 0 && c.UpstreamProxyURL == "" {
		return c, errors.New("upstream_proxy_url is required when upstream_proxy_id is set")
	}
	if len(c.Accounts) > 256 {
		return c, errors.New("at most 256 accounts are supported")
	}
	if c.Accounts == nil {
		c.Accounts = []AccountConfig{}
	}
	seen := map[int64]bool{}
	anyEnabled := false
	needsDynamicProxy := false
	needsGenerator := false
	needsCookieCaptureProxy := false
	needsCookieGenerator := false
	total := 0
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if a.AccountID <= 0 || seen[a.AccountID] {
			return c, errors.New("account_id must be positive and unique")
		}
		seen[a.AccountID] = true
		if err := normalizeAccountDisplayFields(a); err != nil {
			return c, err
		}
		a.EgressMode = strings.TrimSpace(a.EgressMode)
		if a.EgressMode == "" {
			a.EgressMode = egressModeSub2
		}
		if a.EgressMode != egressModeSub2 && a.EgressMode != egressModePlugin && a.EgressMode != egressModeGenerator {
			return c, errors.New("egress_mode must be sub2, plugin, or generator")
		}
		a.StickyProxyURL = strings.TrimSpace(a.StickyProxyURL)
		if err := validateProxy(a.StickyProxyURL); err != nil {
			return c, err
		}
		if a.EgressMode == egressModePlugin {
			if a.StickyProxyURL == "" {
				return c, errors.New("sticky_proxy_url is required for plugin egress mode")
			}
			if strings.Contains(a.StickyProxyURL, "{sid}") || strings.Contains(a.StickyProxyURL, "{random}") {
				return c, errors.New("sticky_proxy_url must use a fixed provider session, not {sid} or {random}")
			}
		}
		if a.Plan == "" {
			a.Plan = "pro"
		}
		if a.Plan != "pro" && a.Plan != "team" {
			return c, errors.New("plan must be pro or team")
		}
		a.RequestTimezone = strings.TrimSpace(a.RequestTimezone)
		if a.RequestTimezone == "" {
			a.RequestTimezone = c.DefaultRequestTimezone
		}
		if err := validateRequestTimezone(a.RequestTimezone); err != nil {
			return c, errors.New("account request_timezone is invalid or restricted")
		}
		if a.Models == nil {
			a.Models = []string{"gpt-6-astra"}
		}
		if len(a.Models) == 0 || len(a.Models) > 16 {
			return c, errors.New("each account must have 1..16 models")
		}
		models := map[string]bool{}
		for _, m := range a.Models {
			if !modelPattern.MatchString(m) || models[m] {
				return c, errors.New("models must contain unique exact gpt model IDs")
			}
			models[m] = true
		}
		total += len(a.Models)
		anyEnabled = anyEnabled || a.Enabled
		needsDynamicProxy = needsDynamicProxy || c.TicketMode == ticketModeLegacy && a.Enabled && a.EgressMode == egressModeSub2
		needsGenerator = needsGenerator || c.TicketMode == ticketModeLegacy && a.Enabled && a.EgressMode == egressModeGenerator
		if a.Enabled && c.TicketMode == ticketModeCookie {
			if c.CookieCaptureMode == captureModeSOCKS5 {
				needsCookieCaptureProxy = true
			} else {
				needsCookieGenerator = true
			}
		}
	}
	if total > 1024 {
		return c, errors.New("at most 1024 account/model pairs are supported")
	}
	if c.Enabled && anyEnabled && needsDynamicProxy && c.DynamicProxyURL == "" {
		return c, errors.New("dynamic_proxy_url is required for enabled accounts using sub2 egress")
	}
	if c.Enabled && anyEnabled && needsGenerator && c.ProxyGeneratorURL == "" {
		return c, errors.New("proxy_generator_url is required for enabled accounts using generator egress")
	}
	if c.Enabled && anyEnabled && needsCookieCaptureProxy && c.CookieCaptureProxyURL == "" {
		return c, errors.New("cookie_capture_proxy_url is required for cookie ticket mode using socks5 capture")
	}
	if c.Enabled && anyEnabled && needsCookieGenerator && c.ProxyGeneratorURL == "" {
		return c, errors.New("proxy_generator_url is required for cookie ticket mode using generator capture")
	}
	return c, nil
}

func normalizeBlockedCountries(raw []string) ([]string, error) {
	if len(raw) > 32 {
		return nil, errors.New("proxy_generator_blocked_countries supports at most 32 country codes")
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(raw)+1)
	for _, value := range raw {
		code := strings.ToUpper(strings.TrimSpace(value))
		if code == "" {
			continue
		}
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			return nil, errors.New("proxy_generator_blocked_countries must contain ISO alpha-2 codes")
		}
		if !seen[code] {
			seen[code] = true
			result = append(result, code)
		}
	}
	if !seen["HK"] {
		result = append(result, "HK")
	}
	sort.Strings(result)
	return result, nil
}

func normalizeTargetGateway(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "", "any", "*":
		return "", nil
	}
	match := gatewayNumberPattern.FindStringSubmatch(value)
	if len(match) != 2 || match[0] != value {
		return "", errors.New("target_gateway must be any or unified-N")
	}
	return "unified-" + match[1], nil
}

func normalizeAccountDisplayFields(a *AccountConfig) error {
	var err error
	if a.Name, err = cleanDisplayField(a.Name, 120, "account name"); err != nil {
		return err
	}
	if a.Email, err = cleanDisplayField(a.Email, 254, "account email"); err != nil {
		return err
	}
	if a.Email != "" && !strings.Contains(a.Email, "@") {
		return errors.New("account email must contain @")
	}
	if a.ExpiresAt, err = cleanDisplayField(a.ExpiresAt, 64, "account expires_at"); err != nil {
		return err
	}
	if a.Quota, err = cleanDisplayField(a.Quota, 80, "account quota"); err != nil {
		return err
	}
	return nil
}

func cleanDisplayField(raw string, maxRunes int, label string) (string, error) {
	value := strings.TrimSpace(raw)
	if len([]rune(value)) > maxRunes {
		return "", errors.New(label + " is too long")
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return "", errors.New(label + " contains control characters")
		}
	}
	return value, nil
}

func validateProxy(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > 4096 || strings.ContainsAny(raw, "\r\n\t") {
		return errors.New("invalid proxy URL")
	}
	// Placeholders are accepted in credentials/host and expanded before use.
	replaced := strings.NewReplacer("{sid}", "123456", "{random}", "123456").Replace(raw)
	if strings.ContainsAny(replaced, "{}") {
		return errors.New("only {sid} and {random} proxy placeholders are supported")
	}
	u, err := url.Parse(replaced)
	if err != nil || u.Hostname() == "" || u.Fragment != "" || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("invalid proxy URL")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return errors.New("proxy port must be 1..65535")
		}
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("proxy scheme must be http, https, socks5, or socks5h")
	}
	return nil
}

func validateRequestTimezone(raw string) error {
	if raw == "" || len(raw) > 64 || hasControlChars(raw) {
		return errors.New("request timezone is invalid")
	}
	loc, err := time.LoadLocation(raw)
	if err != nil {
		return errors.New("request timezone is not a valid IANA timezone")
	}
	switch strings.ToLower(raw) {
	case "prc", "roc", "hongkong", "asia/chongqing", "asia/chungking",
		"asia/harbin", "asia/kashgar", "asia/macao", "asia/taipei",
		"asia/shanghai", "asia/urumqi", "asia/hong_kong", "asia/macau":
		return errors.New("request timezone is restricted")
	}
	switch loc.String() {
	case "Asia/Shanghai", "Asia/Urumqi", "Asia/Hong_Kong", "Asia/Macau", "Asia/Taipei":
		return errors.New("request timezone is restricted")
	}
	return nil
}

func hasControlChars(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateProxyGeneratorURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > 8192 || strings.ContainsAny(raw, "\r\n\t") {
		return errors.New("invalid proxy generator URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Fragment != "" || u.User != nil {
		return errors.New("invalid proxy generator URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("proxy generator URL scheme must be http or https")
	}
	return nil
}
func digest(parts ...string) string {
	h := sha256.New()
	for _, s := range parts {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
func configFingerprint(c Config, a AccountConfig, model string) string {
	return digest("v9", c.UpstreamProxyURL, c.DynamicProxyURL, c.ProxyGeneratorURL,
		strings.Join(c.ProxyGeneratorBlockedCountries, ","), strconv.Itoa(c.ProxyGeneratorTTLMinutes),
		c.TicketMode, c.CookieCaptureMode, c.CookieCaptureProxyURL, c.CookieBusinessProxyURL,
		strconv.Itoa(c.CookieTicketTTLSeconds), strconv.FormatBool(c.StandbyTicketEnabled), strconv.Itoa(c.StandbyLeadSeconds),
		strconv.FormatBool(c.RequestRewriteEnabled), c.DefaultRequestTimezone,
		strconv.FormatBool(c.AllowState780), strconv.Itoa(c.State780TTLSeconds),
		c.TargetGateway,
		strconv.FormatBool(c.RouteCookieReuse), strconv.FormatBool(c.MintFingerprintConvergence),
		a.Plan, model, jsonText(struct {
			ID             int64
			TTL            int
			EgressMode     string
			StickyProxyURL string
			Timezone       string
		}{a.AccountID, c.TTLMinutes, a.EgressMode, a.StickyProxyURL, a.RequestTimezone}))
}
func effectiveTicketTTL(c Config, a AccountConfig) time.Duration {
	if c.TicketMode == ticketModeCookie {
		return time.Duration(c.CookieTicketTTLSeconds) * time.Second
	}
	minutes := c.TTLMinutes
	if a.EgressMode == egressModeGenerator && c.ProxyGeneratorTTLMinutes < minutes {
		minutes = c.ProxyGeneratorTTLMinutes
	}
	return time.Duration(minutes) * time.Minute
}
func effectiveTicketTTLForState(c Config, a AccountConfig, state string) time.Duration {
	ttl := effectiveTicketTTL(c, a)
	if validState(state, compatStateLength) {
		compatTTL := time.Duration(c.State780TTLSeconds) * time.Second
		if compatTTL < ttl {
			ttl = compatTTL
		}
	}
	return ttl
}
func effectiveTTLMinutes(c Config, a AccountConfig) int {
	if c.TicketMode == ticketModeCookie {
		return max(1, (c.CookieTicketTTLSeconds+59)/60)
	}
	if a.EgressMode == egressModeGenerator && c.ProxyGeneratorTTLMinutes < c.TTLMinutes {
		return c.ProxyGeneratorTTLMinutes
	}
	return c.TTLMinutes
}
func effectiveRefreshBefore(c Config, a AccountConfig) time.Duration {
	ttl := effectiveTicketTTL(c, a)
	refresh := time.Duration(c.RefreshBeforeSeconds) * time.Second
	if refresh < ttl {
		return refresh
	}
	if ttl <= time.Second {
		return 0
	}
	return ttl - time.Second
}
func effectiveRefreshBeforeForState(c Config, a AccountConfig, state string) time.Duration {
	ttl := effectiveTicketTTLForState(c, a, state)
	refresh := time.Duration(c.RefreshBeforeSeconds) * time.Second
	if refresh < ttl {
		return refresh
	}
	if ttl <= time.Second {
		return 0
	}
	return ttl - time.Second
}
func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }
func cloneCookies(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}
func targetLength(plan string) int {
	if plan == "team" {
		return 332
	}
	return 292
}

func validPlanState(s string, plan string, allowState780 bool) bool {
	if validState(s, targetLength(plan)) {
		return true
	}
	return allowState780 && validState(s, compatStateLength)
}

// STATE is opaque: only token-safe bytes and expected length are checked.
func validState(s string, n int) bool {
	if len(s) != n || !strings.HasPrefix(s, "gAAAAA") {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '=') {
			return false
		}
	}
	return true
}
