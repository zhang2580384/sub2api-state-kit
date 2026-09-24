// Package engine implements the public Sub2API transport plugin contract.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	hcplugin "github.com/hashicorp/go-plugin"
	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
	"google.golang.org/grpc"
)

type Engine struct {
	pluginv1.UnimplementedTransportPluginServer
	mu               sync.Mutex
	persistMu        sync.Mutex
	config           Config
	generation       uint64
	ctx              context.Context
	cancel           context.CancelFunc
	generationCtx    context.Context
	generationCancel context.CancelFunc
	wake             chan struct{}
	done             chan struct{}
	wg               sync.WaitGroup
	closed           bool
	broker           *hcplugin.GRPCBroker
	host             pluginv1.HostServiceClient
	hostConn         *grpc.ClientConn
	hostReady        bool
	directory        map[int64]bool
	schedulable      map[int64]bool
	directoryAt      time.Time
	directoryError   string
	tickets          map[string]*ticket
	standbyTickets   map[string]*ticket
	previous         map[int64]previousEgress
	routeSessions    map[int64]routeSession
	records          map[string]*jobRecord
	jobs             map[string]uint64
	revoked          map[string]string
	diagnostics      []diagnosticEvent
	diagnosticSeq    uint64
	diagnosticPollAt time.Time
	diagnosticBurst  int
	diagnosticUntil  time.Time
	semaphore        chan struct{}
	clients          *clientPool
	probeURL         string
	egressURL        string
	geoURLs          []string
	tick             time.Duration
	warmup           time.Duration
	activeAfter      time.Time
}
type jobRecord struct {
	Attempts      int
	LastError     string
	CooldownUntil time.Time
}
type ticket struct {
	AccountID           int64             `json:"account_id"`
	Model               string            `json:"model"`
	Plan                string            `json:"plan"`
	State               string            `json:"state"`
	Version             string            `json:"version"`
	ConfigFingerprint   string            `json:"config_fingerprint"`
	FixedFingerprint    string            `json:"fixed_fingerprint"`
	GeneratedProxyURL   string            `json:"generated_proxy_url,omitempty"`
	GeneratedEgressIP   string            `json:"generated_egress_ip,omitempty"`
	IdentityFingerprint string            `json:"identity_fingerprint"`
	CapturedAt          time.Time         `json:"captured_at"`
	IssuedAt            time.Time         `json:"issued_at,omitempty"`
	ExpiresAt           time.Time         `json:"expires_at"`
	TicketMode          string            `json:"ticket_mode,omitempty"`
	SessionBound        bool              `json:"session_bound,omitempty"`
	Gateway             string            `json:"gateway,omitempty"`
	CaptureProxyURL     string            `json:"-"`
	CaptureEgressIP     string            `json:"capture_egress_ip,omitempty"`
	BusinessEgressIP    string            `json:"business_egress_ip,omitempty"`
	Cookies             map[string]string `json:"cookies,omitempty"`
	SessionID           string            `json:"session_id,omitempty"`
}
type previousEgress struct {
	AccountID int64     `json:"account_id"`
	ProxyURL  string    `json:"proxy_url"`
	EgressIP  string    `json:"egress_ip,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}
type receipt struct {
	State, Version, Key, ConfigFingerprint string
	TargetProxyURL, UpstreamProxyURL       string
	TicketMode                             string
	SessionBound                           bool
	Gateway                                string
	Cookies                                map[string]string
	SessionID                              string
	Generation                             uint64
}
type statusTicket struct {
	AccountID               int64  `json:"account_id"`
	Model                   string `json:"model"`
	Plan                    string `json:"plan"`
	TicketMode              string `json:"ticket_mode"`
	State                   string `json:"state"`
	Status                  string `json:"status"`
	RemainingSeconds        int64  `json:"remaining_seconds"`
	ExpiresAt               string `json:"expires_at"`
	StandbyReady            bool   `json:"standby_ready"`
	StandbyRemainingSeconds int64  `json:"standby_remaining_seconds"`
	StandbyExpiresAt        string `json:"standby_expires_at,omitempty"`
	LastError               string `json:"last_error"`
	Attempts                int    `json:"attempts"`
}
type statusSnapshot struct {
	HostReady            bool              `json:"host_ready"`
	AccountIDs           []int64           `json:"account_ids"`
	AccountCatalog       []statusAccount   `json:"account_catalog"`
	Tickets              []statusTicket    `json:"tickets"`
	DiagnosticsEnabled   bool              `json:"diagnostics_enabled"`
	DiagnosticsListening bool              `json:"diagnostics_listening"`
	Diagnostics          []diagnosticEvent `json:"diagnostics,omitempty"`
	Message              string            `json:"message"`
}
type statusAccount struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	ExpiresAt string `json:"expires_at"`
	Quota     string `json:"quota"`
}

func New() *Engine {
	return newEngine(nil, "https://chatgpt.com/backend-api/codex/responses", time.Second)
}

// Tests may inject a host and loopback probe endpoint; production configuration
// deliberately has no endpoint override and never receives OAuth tokens from UI.
func newEngine(host pluginv1.HostServiceClient, probeURL string, tick time.Duration) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	gc, gcancel := context.WithCancel(ctx)
	e := &Engine{
		config: DefaultConfig(), ctx: ctx, cancel: cancel, generationCtx: gc, generationCancel: gcancel,
		wake: make(chan struct{}, 1), done: make(chan struct{}), host: host, hostReady: host != nil,
		directory: map[int64]bool{}, schedulable: map[int64]bool{},
		tickets: map[string]*ticket{}, standbyTickets: map[string]*ticket{}, records: map[string]*jobRecord{},
		previous: map[int64]previousEgress{}, jobs: map[string]uint64{}, revoked: map[string]string{}, semaphore: make(chan struct{}, 4),
		routeSessions: map[int64]routeSession{},
		clients:       newClientPool(), probeURL: probeURL, egressURL: "https://api.ipify.org?format=json",
		geoURLs: []string{
			"http://ip-api.com/json/{ip}?fields=status,countryCode,query",
			"https://ipwho.is/{ip}",
		},
		tick: tick, warmup: 5 * time.Second,
	}
	go e.loop()
	return e
}
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		<-e.done
		return
	}
	e.closed = true
	e.cancel()
	e.generationCancel()
	conn := e.hostConn
	e.mu.Unlock()
	<-e.done
	e.wg.Wait()
	e.clients.Close()
	if conn != nil {
		_ = conn.Close()
	}
}
func (e *Engine) SetHostBroker(b *hcplugin.GRPCBroker) { e.mu.Lock(); e.broker = b; e.mu.Unlock() }
func (e *Engine) InitHostServices(ctx context.Context, r *pluginv1.InitHostServicesRequest) (*pluginv1.InitHostServicesResponse, error) {
	if r == nil || r.HostServiceApiVersion < 1 || r.HostServiceApiVersion > pluginv1.HostServiceAPIVersion {
		return &pluginv1.InitHostServicesResponse{Message: "unsupported host service API"}, nil
	}
	e.mu.Lock()
	b := e.broker
	closed := e.closed
	e.mu.Unlock()
	if b == nil || closed {
		return &pluginv1.InitHostServicesResponse{Message: "host broker unavailable"}, nil
	}
	conn, err := b.Dial(r.HostServiceId)
	if err != nil {
		return &pluginv1.InitHostServicesResponse{Message: "host service connection failed"}, nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		conn.Close()
		return &pluginv1.InitHostServicesResponse{Message: "plugin stopped"}, nil
	}
	old := e.hostConn
	e.hostConn = conn
	e.host = pluginv1.NewHostServiceClient(conn)
	e.hostReady = true
	e.directoryAt = time.Time{}
	e.mu.Unlock()
	if old != nil {
		old.Close()
	}
	e.notify()
	return &pluginv1.InitHostServicesResponse{Ready: true, Message: "host services connected"}, nil
}
func (e *Engine) GetInfo(context.Context, *pluginv1.GetInfoRequest) (*pluginv1.GetInfoResponse, error) {
	return &pluginv1.GetInfoResponse{PluginId: PluginID, PluginVersion: Version, ProtocolVersion: 1, TransportApiVersion: 1, Capabilities: []string{"openai.oauth.outbound_transport.v1"}}, nil
}
func (e *Engine) Health(context.Context, *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	e.observeDiagnosticPollLocked(now)
	s := e.snapshotLocked(now)
	return &pluginv1.HealthResponse{Healthy: !e.closed, Message: s.Message, StatusJson: jsonText(s)}, nil
}
func (e *Engine) ValidateConfig(_ context.Context, r *pluginv1.ValidateConfigRequest) (*pluginv1.ValidateConfigResponse, error) {
	if r == nil {
		r = &pluginv1.ValidateConfigRequest{}
	}
	c, err := ParseConfig(r.ConfigJson)
	if err != nil {
		return &pluginv1.ValidateConfigResponse{Message: err.Error()}, nil
	}
	return &pluginv1.ValidateConfigResponse{Valid: true, Message: "configuration valid", NormalizedConfigJson: []byte(jsonText(c))}, nil
}
func (e *Engine) ApplyConfig(_ context.Context, r *pluginv1.ApplyConfigRequest) (*pluginv1.ApplyConfigResponse, error) {
	if r == nil {
		r = &pluginv1.ApplyConfigRequest{}
	}
	c, err := ParseConfig(r.ConfigJson)
	if err != nil {
		return &pluginv1.ApplyConfigResponse{Message: err.Error()}, nil
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return &pluginv1.ApplyConfigResponse{Message: "plugin stopped"}, nil
	}
	if jsonText(c) == jsonText(e.config) {
		e.mu.Unlock()
		return &pluginv1.ApplyConfigResponse{Applied: true, Message: "configuration unchanged"}, nil
	}
	e.generationCancel()
	e.generationCtx, e.generationCancel = context.WithCancel(e.ctx)
	e.generation++
	e.config = c
	e.activeAfter = time.Now().Add(e.warmup)
	e.jobs = map[string]uint64{}
	e.records = map[string]*jobRecord{}
	e.diagnostics = nil
	// Remove memory entries no longer matching configuration. Persisted entries are
	// keyed by fingerprint and expire naturally; switching configuration cannot use them.
	for k, t := range e.tickets {
		a, ok := findAccount(c, t.AccountID)
		if !ok || !contains(a.Models, t.Model) || t.ConfigFingerprint != configFingerprint(c, a, t.Model) {
			delete(e.tickets, k)
		}
	}
	for k, t := range e.standbyTickets {
		a, ok := findAccount(c, t.AccountID)
		if !ok || !contains(a.Models, t.Model) || t.ConfigFingerprint != configFingerprint(c, a, t.Model) {
			delete(e.standbyTickets, k)
		}
	}
	for id := range e.previous {
		if _, ok := findAccount(c, id); !ok {
			delete(e.previous, id)
		}
	}
	for id := range e.routeSessions {
		if _, ok := findAccount(c, id); !ok {
			delete(e.routeSessions, id)
		}
	}
	e.mu.Unlock()
	e.clients.Close()
	e.notify()
	return &pluginv1.ApplyConfigResponse{Applied: true, Message: "configuration applied"}, nil
}
func (e *Engine) TestConfig(_ context.Context, r *pluginv1.TestConfigRequest) (*pluginv1.TestConfigResponse, error) {
	if r == nil {
		r = &pluginv1.TestConfigRequest{}
	}
	c, err := ParseConfig(r.ConfigJson)
	if err != nil {
		return &pluginv1.TestConfigResponse{Message: err.Error()}, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.snapshotLocked(time.Now())
	result := &pluginv1.TestConfigResponse{StatusJson: jsonText(s)}
	if jsonText(c) != jsonText(e.config) {
		result.Message = "save configuration before checking"
		return result, nil
	}
	if !e.hostReady {
		result.Message = "host services not ready"
		return result, nil
	}
	if c.Enabled {
		for _, a := range c.Accounts {
			if a.Enabled && !e.directory[a.AccountID] {
				result.Message = "an enabled account is not in the host account directory"
				return result, nil
			}
		}
	}
	result.Success = true
	result.Message = "saved configuration and cached host account bindings are valid; no proxy or model request was sent"
	return result, nil
}
func findAccount(c Config, id int64) (AccountConfig, bool) {
	for _, a := range c.Accounts {
		if a.AccountID == id {
			return a, true
		}
	}
	return AccountConfig{}, false
}
func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
func keyFor(id int64, model string) string { return fmt.Sprintf("%d.%s", id, model) }
func kvKey(id int64, model, fp string) string {
	return fmt.Sprintf("ticket.%d.%s", id, digest(model, fp))
}
func standbyKVKey(id int64, model, fp string) string {
	return fmt.Sprintf("ticket.standby.%d.%s", id, digest(model, fp))
}
func previousEgressKey(id int64) string  { return fmt.Sprintf("egress.%d", id) }
func proxyFingerprint(raw string) string { return digest("business-proxy-v1", strings.TrimSpace(raw)) }
func accountEgress(c Config, a AccountConfig, hostProxyURL string, t *ticket) (string, string, error) {
	if c.TicketMode == ticketModeCookie {
		if c.CookieBusinessProxyURL == "" {
			return "", c.UpstreamProxyURL, nil
		}
		if err := validateProxy(c.CookieBusinessProxyURL); err != nil {
			return "", "", errors.New("cookie business proxy is invalid")
		}
		return c.CookieBusinessProxyURL, c.UpstreamProxyURL, nil
	}
	if a.EgressMode == egressModePlugin {
		if a.StickyProxyURL == "" {
			return "", "", errors.New("account sticky proxy is not configured")
		}
		return a.StickyProxyURL, c.UpstreamProxyURL, nil
	}
	if a.EgressMode == egressModeGenerator {
		if t == nil || t.GeneratedProxyURL == "" {
			return "", c.UpstreamProxyURL, errors.New("generated sticky proxy is unavailable")
		}
		return t.GeneratedProxyURL, c.UpstreamProxyURL, nil
	}
	if err := validateProxy(hostProxyURL); err != nil {
		return "", "", errors.New("business proxy invalid")
	}
	return hostProxyURL, "", nil
}
func expectedBusinessProxyURL(c Config, a AccountConfig, hostProxyURL string) string {
	if c.TicketMode == ticketModeCookie {
		return c.CookieBusinessProxyURL
	}
	if a.EgressMode == egressModePlugin {
		return a.StickyProxyURL
	}
	return hostProxyURL
}
func (e *Engine) accountEnabled(start *pluginv1.ForwardRequestStart) bool {
	if start == nil || start.Platform != "openai" || start.AccountType != "oauth" {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := findAccount(e.config, start.AccountId)
	return !e.closed && e.config.Enabled && ok && a.Enabled
}

func (e *Engine) ticketForRequest(_ context.Context, start *pluginv1.ForwardRequestStart, model string) (*receipt, error) {
	if start == nil || start.Platform != "openai" || start.AccountType != "oauth" {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	a, ok := findAccount(e.config, start.AccountId)
	if !e.config.Enabled || !ok || !a.Enabled {
		return nil, nil
	}
	if model == "" {
		return nil, errors.New("configured account request has no inspectable model")
	}
	if !contains(a.Models, model) {
		return nil, nil
	}
	k := keyFor(start.AccountId, model)
	t := e.tickets[k]
	expectedProxyURL := expectedBusinessProxyURL(e.config, a, start.ProxyUrl)
	if e.config.TicketMode == ticketModeLegacy && a.EgressMode == egressModeGenerator && t != nil {
		expectedProxyURL = t.GeneratedProxyURL
	}
	expectedFingerprint := proxyFingerprint(expectedProxyURL)
	if t != nil && (t.FixedFingerprint != expectedFingerprint || t.IdentityFingerprint != stableHeaders(start.AccountId, start.Headers)) {
		delete(e.tickets, k)
		t = nil
	}
	if !validTicket(t, e.config, a, model, time.Now()) {
		if standby := e.standbyTickets[k]; validTicket(standby, e.config, a, model, time.Now()) &&
			standbyMatches(standby, t, expectedFingerprint, stableHeaders(start.AccountId, start.Headers)) {
			t = standby
			e.tickets[k] = standby
			delete(e.standbyTickets, k)
			delete(e.revoked, k)
		} else if standby != nil {
			delete(e.standbyTickets, k)
		}
	}

	if !e.closed && e.hostReady && e.directory[start.AccountId] && validTicket(t, e.config, a, model, time.Now()) && t.FixedFingerprint == expectedFingerprint && t.IdentityFingerprint == stableHeaders(start.AccountId, start.Headers) && e.revoked[k] != t.Version {
		targetProxyURL, upstreamProxyURL, err := accountEgress(e.config, a, start.ProxyUrl, t)
		if err != nil {
			return nil, err
		}
		return &receipt{
			State: t.State, Version: t.Version, Key: k, ConfigFingerprint: t.ConfigFingerprint,
			TargetProxyURL: targetProxyURL, UpstreamProxyURL: upstreamProxyURL, TicketMode: t.TicketMode,
			SessionBound: t.SessionBound, Gateway: t.Gateway,
			Cookies: cloneCookies(t.Cookies), SessionID: t.SessionID, Generation: e.generation,
		}, nil
	}
	e.notify()
	return nil, errors.New("verified STATE unavailable; acquisition is running in the background")
}
func validTicket(t *ticket, c Config, a AccountConfig, model string, now time.Time) bool {
	if t == nil || t.AccountID != a.AccountID || t.Model != model || t.Plan != a.Plan ||
		t.ConfigFingerprint != configFingerprint(c, a, model) || !validPlanState(t.State, a.Plan, c.AllowState780) ||
		t.Version == "" || t.FixedFingerprint == "" || t.IdentityFingerprint == "" ||
		t.CapturedAt.IsZero() || t.CapturedAt.After(now.Add(time.Minute)) ||
		!t.ExpiresAt.After(t.CapturedAt) || !now.Before(t.ExpiresAt) {
		return false
	}
	if !t.IssuedAt.IsZero() && (t.IssuedAt.After(now.Add(time.Minute)) || t.IssuedAt.After(t.CapturedAt.Add(time.Minute))) {
		return false
	}
	if c.TicketMode == ticketModeCookie || t.SessionBound {
		if t.SessionID == "" || len(t.Cookies) == 0 {
			return false
		}
	}
	if c.TicketMode == ticketModeCookie {
		if t.TicketMode != ticketModeCookie {
			return false
		}
	} else {
		if a.EgressMode == egressModeGenerator {
			if t.GeneratedProxyURL == "" || validateProxy(t.GeneratedProxyURL) != nil {
				return false
			}
		} else if t.GeneratedProxyURL != "" {
			return false
		}
	}
	anchor := t.CapturedAt
	if !t.IssuedAt.IsZero() {
		anchor = t.IssuedAt
	}
	ttl := t.ExpiresAt.Sub(anchor)
	return ttl > 0 && ttl <= effectiveTicketTTLForState(c, a, t.State)
}

func standbyMatches(standby, identitySource *ticket, expectedFingerprint, expectedIdentity string) bool {
	if standby == nil || standby.FixedFingerprint != expectedFingerprint {
		return false
	}
	if expectedIdentity == "" && identitySource != nil {
		expectedIdentity = identitySource.IdentityFingerprint
	}
	return expectedIdentity != "" && standby.IdentityFingerprint == expectedIdentity
}

func (e *Engine) invalidate(r *receipt, reason string) {
	if r == nil || (reason != "model_mismatch" && reason != "state_312") {
		return
	}
	e.mu.Lock()
	t := e.tickets[r.Key]
	if e.closed || !e.config.Enabled || e.generation != r.Generation || t == nil || t.Version != r.Version || t.ConfigFingerprint != r.ConfigFingerprint {
		e.mu.Unlock()
		return
	}
	delete(e.tickets, r.Key)
	e.revoked[r.Key] = r.Version
	rec := e.records[r.Key]
	if rec == nil {
		rec = &jobRecord{}
		e.records[r.Key] = rec
	}
	rec.LastError = reason
	host := e.host
	e.mu.Unlock()
	e.notify()
	if host == nil {
		return
	}
	// Serialize persistence mutations without holding the business/status mutex.
	// A newly committed ticket wins over an old response waiting to delete KV.
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	e.mu.Lock()
	current := e.tickets[r.Key]
	newer := current != nil && current.Version != r.Version
	e.mu.Unlock()
	if newer {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, 2*time.Second)
	_, err := host.KVDelete(ctx, &pluginv1.KVDeleteRequest{Namespace: namespace, Key: kvKey(t.AccountID, t.Model, t.ConfigFingerprint)})
	cancel()
	if err != nil {
		e.mu.Lock()
		if rec := e.records[r.Key]; rec != nil && e.tickets[r.Key] == nil {
			rec.LastError = reason + "_persistence_failed"
		}
		e.mu.Unlock()
	}
}

func (e *Engine) notify() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) snapshotLocked(now time.Time) statusSnapshot {
	listening := e.diagnosticsListeningLocked(now)
	s := statusSnapshot{HostReady: e.hostReady, AccountIDs: []int64{}, AccountCatalog: []statusAccount{}, Tickets: []statusTicket{}, DiagnosticsEnabled: listening, DiagnosticsListening: listening, Message: "STATE disabled; requests use the account business proxy"}
	if listening && len(e.diagnostics) > 0 {
		s.Diagnostics = append([]diagnosticEvent(nil), e.diagnostics...)
	}
	for id := range e.directory {
		s.AccountIDs = append(s.AccountIDs, id)
	}
	sort.Slice(s.AccountIDs, func(i, j int) bool { return s.AccountIDs[i] < s.AccountIDs[j] })
	for _, a := range e.config.Accounts {
		s.AccountCatalog = append(s.AccountCatalog, statusAccount{AccountID: a.AccountID, Name: a.Name, Email: a.Email, ExpiresAt: a.ExpiresAt, Quota: a.Quota})
	}
	if e.config.Enabled {
		s.Message = "STATE active only for explicitly enabled account/model pairs"
	}
	if !e.hostReady {
		s.Message = "waiting for host services"
	}
	if e.directoryError != "" {
		s.Message = e.directoryError
	}
	for _, a := range e.config.Accounts {
		for _, model := range a.Models {
			k := keyFor(a.AccountID, model)
			t := e.tickets[k]
			standby := e.standbyTickets[k]
			r := e.records[k]
			row := statusTicket{AccountID: a.AccountID, Model: model, Plan: a.Plan, TicketMode: e.config.TicketMode, State: "queued"}
			valid := validTicket(t, e.config, a, model, now)
			if t != nil {
				row.ExpiresAt = t.ExpiresAt.UTC().Format(time.RFC3339)
				if valid {
					row.RemainingSeconds = int64(t.ExpiresAt.Sub(now).Seconds())
				}
			}
			if validTicket(standby, e.config, a, model, now) {
				row.StandbyReady = true
				row.StandbyRemainingSeconds = int64(standby.ExpiresAt.Sub(now).Seconds())
				row.StandbyExpiresAt = standby.ExpiresAt.UTC().Format(time.RFC3339)
			}
			if r != nil {
				row.Attempts = r.Attempts
				row.LastError = r.LastError
			}
			switch {
			case !e.config.Enabled || !a.Enabled:
				row.State = "disabled"
			case !e.hostReady:
				row.State = "waiting_host"
			case !e.directory[a.AccountID]:
				row.State = "waiting_account"
			case e.jobs[k] == e.generation && e.generation != 0:
				if valid {
					row.State = "renewing"
				} else {
					row.State = "harvesting"
				}
			case valid:
				row.State = "ready"
			case r != nil && r.CooldownUntil.After(now):
				row.State = "cooldown"
			case t != nil:
				row.State = "expired"
			}
			row.Status = row.State
			s.Tickets = append(s.Tickets, row)
		}
	}
	return s
}
