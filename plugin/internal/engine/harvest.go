package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
)

func (e *Engine) loop() {
	defer close(e.done)
	timer := time.NewTicker(e.tick)
	defer timer.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-timer.C:
		case <-e.wake:
		}
		e.refreshDirectory()
		e.schedule()
	}
}
func (e *Engine) refreshDirectory() {
	e.mu.Lock()
	host := e.host
	due := time.Since(e.directoryAt) >= 30*time.Second
	e.mu.Unlock()
	if host == nil || !due {
		return
	}
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	res, err := host.ListAccounts(ctx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
	cancel()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.directoryAt = time.Now()
	if err != nil || res == nil {
		e.directory = map[int64]bool{}
		e.directoryError = "host account directory unavailable"
		return
	}
	d := map[int64]bool{}
	for _, id := range res.AccountIds {
		if id > 0 {
			d[id] = true
		}
	}
	e.directory = d
	e.directoryError = ""
}
func (e *Engine) schedule() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.config.Enabled || e.host == nil || time.Now().Before(e.activeAfter) {
		return
	}
	now := time.Now()
	for _, a := range e.config.Accounts {
		if !a.Enabled || !e.directory[a.AccountID] {
			continue
		}
		for _, model := range a.Models {
			k := keyFor(a.AccountID, model)
			if _, running := e.jobs[k]; running {
				continue
			}
			r := e.records[k]
			if r != nil && r.CooldownUntil.After(now) {
				continue
			}
			t := e.tickets[k]
			if !validTicket(t, e.config, a, model, now) {
				expectedFingerprint := proxyFingerprint(expectedBusinessProxyURL(e.config, a, ""))
				if standby := e.standbyTickets[k]; validTicket(standby, e.config, a, model, now) &&
					standbyMatches(standby, t, expectedFingerprint, "") {
					e.tickets[k] = standby
					delete(e.standbyTickets, k)
					delete(e.revoked, k)
					continue
				} else if standby != nil {
					delete(e.standbyTickets, k)
				}
			} else {
				lead := effectiveRefreshBefore(e.config, a)
				standbyEnabled := e.config.TicketMode == ticketModeCookie && e.config.StandbyTicketEnabled
				if standbyEnabled {
					lead = time.Duration(e.config.StandbyLeadSeconds) * time.Second
					if validTicket(e.standbyTickets[k], e.config, a, model, now) {
						continue
					}
				}
				if t.ExpiresAt.Sub(now) > lead {
					continue
				}
			}
			standby := e.config.TicketMode == ticketModeCookie && e.config.StandbyTicketEnabled && validTicket(t, e.config, a, model, now)
			if !validTicket(t, e.config, a, model, now) {
				standby = false
			}
			c := e.config
			gen := e.generation
			ctx := e.generationCtx
			host := e.host
			e.jobs[k] = gen
			e.wg.Add(1)
			go e.collect(ctx, host, c, a, model, k, gen, standby)
		}
	}
}
func (e *Engine) activeLocked(k string, gen uint64) bool {
	current, ok := e.jobs[k]
	return !e.closed && e.generation == gen && ok && current == gen
}
func (e *Engine) note(k string, gen uint64, attempt int, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.activeLocked(k, gen) {
		return
	}
	r := e.records[k]
	if r == nil {
		r = &jobRecord{}
		e.records[k] = r
	}
	r.Attempts = attempt
	r.LastError = reason
}
func (e *Engine) collect(ctx context.Context, host pluginv1.HostServiceClient, c Config, a AccountConfig, model, k string, gen uint64, standby bool) {
	defer e.wg.Done()
	success := false
	reason := "attempts_exhausted"
	defer func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if !e.activeLocked(k, gen) {
			return
		}
		delete(e.jobs, k)
		r := e.records[k]
		if r == nil {
			r = &jobRecord{}
			e.records[k] = r
		}
		if success {
			r.CooldownUntil = time.Time{}
			r.LastError = ""
		} else if ctx.Err() == nil {
			r.LastError = reason
			r.CooldownUntil = time.Now().Add(time.Duration(c.CooldownSeconds) * time.Second)
		}
	}()
	select {
	case e.semaphore <- struct{}{}:
		defer func() { <-e.semaphore }()
	case <-ctx.Done():
		return
	}
	identity, err := resolveIdentity(ctx, host, a.AccountID)
	if err != nil {
		reason = "identity_unavailable"
		return
	}
	fp := configFingerprint(c, a, model)
	// Restored tickets are revalidated through the current business proxy before
	// reuse. This also prevents a failed persistence deletion reviving a bad ticket.
	if restored, status := e.restore(ctx, host, c, a, model, k, gen, identity, fp, standby); restored {
		success = true
		return
	} else if isStopStatus(status) {
		reason = stopReason(status)
		return
	}
	cookieMode := c.TicketMode == ticketModeCookie
	generatorCapture := a.EgressMode == egressModeGenerator
	if cookieMode {
		generatorCapture = c.CookieCaptureMode == captureModeGenerator
	}
	preferPrevious := c.PreferPreviousIP && generatorCapture
	attemptLimit := c.MaxAttempts
	if preferPrevious {
		// Reusing the previous egress is an optimization attempt. It must not
		// consume one of the configured generator acquisition attempts.
		attemptLimit++
	}
	reusePrevious := preferPrevious
	for attempt := 1; attempt <= attemptLimit; attempt++ {
		if ctx.Err() != nil {
			return
		}
		e.note(k, gen, attempt, "")
		if attempt > 1 {
			select {
			case <-time.After(time.Duration(c.AttemptIntervalSeconds) * time.Second):
			case <-ctx.Done():
				return
			}
		}
		identity, err = resolveIdentity(ctx, host, a.AccountID)
		if err != nil {
			reason = "identity_unavailable"
			return
		}
		targetProxyURL := ""
		upstreamProxyURL := c.UpstreamProxyURL
		if cookieMode {
			if c.CookieCaptureMode == captureModeSOCKS5 {
				targetProxyURL = c.CookieCaptureProxyURL
			} else {
				if reusePrevious {
					reusePrevious = false
					targetProxyURL = e.preferredPreviousProxy(ctx, host, a.AccountID, k)
				}
				if targetProxyURL == "" {
					targetProxyURL, err = e.generateProxy(ctx, c)
					if err != nil {
						reason = "generator_unavailable"
						e.recordDiagnostic(diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
							UpstreamProxy: upstreamProxyURL, Outcome: "generator_failed", Error: reason})
						continue
					}
				}
			}
		} else {
			switch a.EgressMode {
			case egressModePlugin:
				targetProxyURL = a.StickyProxyURL
			case egressModeGenerator:
				if reusePrevious {
					reusePrevious = false
					targetProxyURL = e.preferredPreviousProxy(ctx, host, a.AccountID, k)
				}
				if targetProxyURL == "" {
					targetProxyURL, err = e.generateProxy(ctx, c)
					if err != nil {
						reason = "generator_unavailable"
						e.recordDiagnostic(diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
							UpstreamProxy: upstreamProxyURL, Outcome: "generator_failed", Error: reason})
						continue
					}
				}
			default:
				targetProxyURL, err = rotateProxy(c.DynamicProxyURL)
				if err != nil {
					reason = "invalid_dynamic_proxy"
					return
				}
			}
		}
		generatorMode := generatorCapture
		captureEgress := ""
		captureCountry := ""
		if cookieMode || generatorMode {
			captureEgress = e.lookupEgressIPRequired(ctx, targetProxyURL, upstreamProxyURL)
			if captureEgress == "" {
				reason = "generator_egress_unavailable"
				e.recordDiagnostic(diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
					UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, Outcome: "egress_unavailable", Error: reason})
				continue
			}
			captureCountry = e.lookupEgressCountry(ctx, targetProxyURL, upstreamProxyURL, captureEgress)
			if countryBlocked(captureCountry, c.ProxyGeneratorBlockedCountries) {
				reason = "generator_region_blocked"
				e.recordDiagnostic(diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
					UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, CaptureEgress: captureEgress, Outcome: "region_blocked", Error: reason})
				continue
			}
		}
		captured := time.Now()
		captureStarted := time.Now()
		sessionID := randomID()
		cookies := map[string]string{}
		captureResult, err := e.probe(ctx, identity, model, targetProxyURL, upstreamProxyURL, "", sessionID, cookies)
		candidate, status, captureModel := captureResult.State, captureResult.Status, captureResult.Model
		cookies = captureResult.Cookies
		if !generatorMode && !cookieMode {
			captureEgress = e.lookupEgressIP(ctx, targetProxyURL, upstreamProxyURL)
		}
		captureEvent := diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
			UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, CaptureEgress: captureEgress,
			StateLength: len(candidate), StateClass: stateDiagnosticClass(candidate), ResponseModel: captureModel,
			HTTPStatus: status, DurationMS: time.Since(captureStarted).Milliseconds()}
		if err != nil {
			reason = "harvest_failed"
			captureEvent.Outcome = "probe_failed"
			captureEvent.Error = err.Error()
			e.recordDiagnostic(captureEvent)
			if isStopStatus(status) {
				reason = stopReason(status)
				return
			}
			continue
		}
		if !validState(candidate, targetLength(a.Plan)) {
			reason = "unexpected_state_length"
			captureEvent.Outcome = "unexpected_state"
			e.recordDiagnostic(captureEvent)
			continue
		}
		captureEvent.Outcome = "accepted"
		e.recordDiagnostic(captureEvent)
		// Resolve fresh credentials again and validate through the same account
		// egress that will serve business traffic. Proxy credentials are never
		// copied into the ticket.
		fixed, err := resolveIdentity(ctx, host, a.AccountID)
		if err != nil {
			reason = "identity_unavailable"
			return
		}
		if stableIdentity(identity) != stableIdentity(fixed) {
			reason = "identity_changed"
			continue
		}
		fixedProxyURL := targetProxyURL
		fixedUpstreamProxyURL := upstreamProxyURL
		if cookieMode || a.EgressMode == egressModeSub2 {
			fixedProxyURL, fixedUpstreamProxyURL, err = accountEgress(c, a, fixed.ProxyUrl, nil)
			if err != nil {
				reason = "account_egress_invalid"
				return
			}
		}
		validationStarted := time.Now()
		fixedEgress := e.lookupEgressIP(ctx, fixedProxyURL, fixedUpstreamProxyURL)
		if cookieMode || generatorMode {
			fixedEgress = e.lookupEgressIPRequired(ctx, fixedProxyURL, fixedUpstreamProxyURL)
		}
		validationEvent := diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "fixed_validation", Attempt: attempt,
			UpstreamProxy: fixedUpstreamProxyURL, TargetProxy: fixedProxyURL, CaptureEgress: captureEgress, FixedEgress: fixedEgress,
			EgressMatch: egressMatch(captureEgress, fixedEgress)}
		if (cookieMode || generatorMode) && fixedEgress == "" {
			reason = "business_egress_unavailable"
			validationEvent.Outcome = "egress_unavailable"
			validationEvent.Error = reason
			e.recordDiagnostic(validationEvent)
			continue
		}
		if cookieMode {
			fixedCountry := e.lookupEgressCountry(ctx, fixedProxyURL, fixedUpstreamProxyURL, fixedEgress)
			if countryBlocked(fixedCountry, c.ProxyGeneratorBlockedCountries) {
				reason = "business_region_blocked"
				validationEvent.Outcome = "region_blocked"
				validationEvent.Error = reason
				e.recordDiagnostic(validationEvent)
				continue
			}
		}
		if !cookieMode && (a.EgressMode == egressModePlugin || generatorMode) && captureEgress != "" && fixedEgress != "" && captureEgress != fixedEgress {
			reason = "sticky_egress_changed"
			validationEvent.Outcome = "egress_changed"
			e.recordDiagnostic(validationEvent)
			continue
		}
		validationResult, err := e.probe(ctx, fixed, model, fixedProxyURL, fixedUpstreamProxyURL, candidate, sessionID, cookies)
		returned, status, validationModel := validationResult.State, validationResult.Status, validationResult.Model
		cookies = validationResult.Cookies
		validationEvent.StateLength = len(returned)
		validationEvent.StateClass = stateDiagnosticClass(returned)
		validationEvent.ResponseModel = validationModel
		validationEvent.HTTPStatus = status
		validationEvent.DurationMS = time.Since(validationStarted).Milliseconds()
		if err != nil || validState(returned, 312) {
			reason = "fixed_proxy_validation_failed"
			validationEvent.Outcome = "validation_failed"
			if validState(returned, 312) {
				validationEvent.Outcome = "state_312"
			}
			if err != nil {
				validationEvent.Error = err.Error()
			}
			e.recordDiagnostic(validationEvent)
			if isStopStatus(status) {
				reason = stopReason(status)
				return
			}
			continue
		}
		if cookieMode && (sessionID == "" || len(cookies) == 0) {
			reason = "cookie_session_incomplete"
			validationEvent.Outcome = "session_incomplete"
			validationEvent.Error = reason
			e.recordDiagnostic(validationEvent)
			continue
		}
		validationEvent.Outcome = "accepted"
		e.recordDiagnostic(validationEvent)
		generatedProxyURL := ""
		if generatorMode && !cookieMode {
			generatedProxyURL = fixedProxyURL
		}
		t := &ticket{AccountID: a.AccountID, Model: model, Plan: a.Plan, State: candidate, Version: randomID(),
			ConfigFingerprint: fp, FixedFingerprint: proxyFingerprint(fixedProxyURL), GeneratedProxyURL: generatedProxyURL,
			GeneratedEgressIP: captureEgress, IdentityFingerprint: stableIdentity(fixed), CapturedAt: captured,
			ExpiresAt: captured.Add(effectiveTicketTTL(c, a)), TicketMode: c.TicketMode,
			CaptureProxyURL: targetProxyURL, CaptureEgressIP: captureEgress, BusinessEgressIP: fixedEgress,
			Cookies: cookies, SessionID: sessionID}
		if e.commit(ctx, host, c, a, model, k, gen, t, true, standby) {
			if generatorCapture {
				e.rememberPreviousEgress(ctx, host, a.AccountID, targetProxyURL, captureEgress)
			}
			success = true
			return
		}
		reason = "ticket_persistence_failed"
		return
	}
}
func isStopStatus(status int) bool { return status == 401 || status == 403 || status == 429 }
func stopReason(status int) string {
	switch status {
	case 401:
		return "upstream_unauthorized"
	case 403:
		return "upstream_forbidden"
	case 429:
		return "upstream_rate_limited"
	}
	return "upstream_rejected"
}
func resolveIdentity(ctx context.Context, host pluginv1.HostServiceClient, id int64) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := host.ResolveOutboundIdentity(c, &pluginv1.ResolveOutboundIdentityRequest{AccountId: id})
	if err != nil || r == nil || !r.Found || r.AccountId != id || r.Platform != "openai" || r.AccountType != "oauth" || r.Token == "" {
		return nil, errors.New("account identity unavailable")
	}
	return r, nil
}
func stableIdentity(r *pluginv1.ResolveOutboundIdentityResponse) string {
	if r == nil {
		return ""
	}
	return stableHeaders(r.AccountId, r.Headers)
}
func stableHeaders(id int64, headers map[string]*pluginv1.HeaderValues) string {
	var account string
	for k, v := range headers {
		if strings.EqualFold(k, "Chatgpt-Account-Id") && v != nil {
			account = strings.Join(v.Values, "\x00")
		}
	}
	return digest(fmt.Sprint(id), account)
}

func (e *Engine) restore(ctx context.Context, host pluginv1.HostServiceClient, c Config, a AccountConfig, model, k string, gen uint64, identity *pluginv1.ResolveOutboundIdentityResponse, fp string, standby bool) (bool, int) {
	e.mu.Lock()
	current := e.tickets[k]
	revoked := e.revoked[k]
	e.mu.Unlock()
	if validTicket(current, c, a, model, time.Now()) {
		return false, 0
	} // Renewal keeps the old usable ticket throughout.
	key := kvKey(a.AccountID, model, fp)
	alternateKey := standbyKVKey(a.AccountID, model, fp)
	if standby {
		key, alternateKey = alternateKey, key
	}
	readTicket := func(valueKey string) (ticket, bool) {
		cc, cancel := context.WithTimeout(ctx, 3*time.Second)
		r, err := host.KVGet(cc, &pluginv1.KVGetRequest{Namespace: namespace, Key: valueKey})
		cancel()
		if err != nil || r == nil || !r.Found || len(r.Value) > 16*1024 {
			return ticket{}, false
		}
		var t ticket
		if json.Unmarshal(r.Value, &t) != nil {
			return ticket{}, false
		}
		return t, true
	}
	t, ok := readTicket(key)
	if !ok {
		t, ok = readTicket(alternateKey)
	}
	if !ok {
		return false, 0
	}
	targetProxyURL, upstreamProxyURL, err := accountEgress(c, a, identity.ProxyUrl, &t)
	if err != nil {
		return false, 0
	}
	expectedFingerprint := proxyFingerprint(targetProxyURL)
	if !validTicket(&t, c, a, model, time.Now()) || t.Version == revoked || t.FixedFingerprint != expectedFingerprint || t.IdentityFingerprint != stableIdentity(identity) {
		return false, 0
	}
	restoreStarted := time.Now()
	fixedEgress := e.lookupEgressIP(ctx, targetProxyURL, upstreamProxyURL)
	if c.TicketMode == ticketModeCookie || a.EgressMode == egressModeGenerator {
		fixedEgress = e.lookupEgressIPRequired(ctx, targetProxyURL, upstreamProxyURL)
	}
	event := diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "restore",
		UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL,
		FixedEgress: fixedEgress}
	if (c.TicketMode == ticketModeCookie || a.EgressMode == egressModeGenerator) && fixedEgress == "" {
		event.Outcome = "egress_unavailable"
		event.Error = "business_egress_unavailable"
		event.DurationMS = time.Since(restoreStarted).Milliseconds()
		e.recordDiagnostic(event)
		return false, 0
	}
	if c.TicketMode == ticketModeCookie {
		fixedCountry := e.lookupEgressCountry(ctx, targetProxyURL, upstreamProxyURL, fixedEgress)
		if countryBlocked(fixedCountry, c.ProxyGeneratorBlockedCountries) {
			event.Outcome = "region_blocked"
			event.Error = "business_region_blocked"
			event.DurationMS = time.Since(restoreStarted).Milliseconds()
			e.recordDiagnostic(event)
			return false, 0
		}
	} else if a.EgressMode == egressModeGenerator && t.GeneratedEgressIP != "" && fixedEgress != t.GeneratedEgressIP {
		event.Outcome = "egress_changed"
		event.Error = "sticky_egress_changed"
		event.DurationMS = time.Since(restoreStarted).Milliseconds()
		e.recordDiagnostic(event)
		return false, 0
	}
	probeResult, err := e.probe(ctx, identity, model, targetProxyURL, upstreamProxyURL, t.State, t.SessionID, t.Cookies)
	returned, status, responseModel := probeResult.State, probeResult.Status, probeResult.Model
	event.StateLength = len(returned)
	event.StateClass = stateDiagnosticClass(returned)
	event.ResponseModel = responseModel
	event.HTTPStatus = status
	event.DurationMS = time.Since(restoreStarted).Milliseconds()
	if err != nil || validState(returned, 312) {
		event.Outcome = "validation_failed"
		if validState(returned, 312) {
			event.Outcome = "state_312"
		}
		if err != nil {
			event.Error = err.Error()
		}
		e.recordDiagnostic(event)
		return false, status
	}
	if returned != "" {
		t.State = returned
	}
	t.Cookies = probeResult.Cookies
	t.BusinessEgressIP = fixedEgress
	event.Outcome = "accepted"
	e.recordDiagnostic(event)
	if !e.commit(ctx, host, c, a, model, k, gen, &t, false, standby) {
		return false, status
	}
	if c.TicketMode == ticketModeLegacy && a.EgressMode == egressModeGenerator {
		e.rememberPreviousEgress(ctx, host, a.AccountID, t.GeneratedProxyURL, t.GeneratedEgressIP)
	}
	return true, status
}
func (e *Engine) commit(ctx context.Context, host pluginv1.HostServiceClient, c Config, a AccountConfig, model, k string, gen uint64, t *ticket, persist bool, standby bool) bool {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	valid := func() bool {
		return e.activeLocked(k, gen) && ctx.Err() == nil && e.directory[a.AccountID] && validTicket(t, c, a, model, time.Now()) && e.revoked[k] != t.Version
	}
	e.mu.Lock()
	ok := valid()
	targetStandby := standby && validTicket(e.tickets[k], c, a, model, time.Now())
	e.mu.Unlock()
	if !ok {
		return false
	}
	if persist {
		persistKey := kvKey(a.AccountID, model, t.ConfigFingerprint)
		if targetStandby {
			persistKey = standbyKVKey(a.AccountID, model, t.ConfigFingerprint)
		}
		cc, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := host.KVSet(cc, &pluginv1.KVSetRequest{Namespace: namespace, Key: persistKey, Value: []byte(jsonText(t)), TtlSeconds: max(1, int64(time.Until(t.ExpiresAt).Seconds()))})
		cancel()
		if err != nil {
			return false
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !valid() {
		return false
	}
	if standby && validTicket(e.tickets[k], c, a, model, time.Now()) {
		e.standbyTickets[k] = t
	} else {
		e.tickets[k] = t
		delete(e.standbyTickets, k)
	}
	delete(e.revoked, k)
	return true
}

type ticketProbeResult struct {
	State     string
	Status    int
	Model     string
	Cookies   map[string]string
	SessionID string
}

func (e *Engine) probe(ctx context.Context, identity *pluginv1.ResolveOutboundIdentityResponse, model, proxyURL, upstreamProxyURL, state, sessionID string, cookies map[string]string) (ticketProbeResult, error) {
	result := ticketProbeResult{Cookies: cloneCookies(cookies), SessionID: sessionID}
	if result.Cookies == nil {
		result.Cookies = map[string]string{}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	payload := map[string]any{"model": model, "store": false, "stream": true, "instructions": "Reply with exactly: pong", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}}}}}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.probeURL, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("probe construction failed")
	}
	for name, values := range identity.Headers {
		if values == nil {
			continue
		}
		for _, v := range values.Values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("Authorization", "Bearer "+identity.Token)
	req.Header.Del(StateHeader)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	if sessionID == "" {
		sessionID = randomID()
		result.SessionID = sessionID
	}
	req.Header.Set("session_id", sessionID)
	req.Header.Set("version", "0.153.4")
	req.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
	req.Header.Set("originator", "codex_cli_rs")
	if cookie := cookieHeader(result.Cookies); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if state != "" {
		req.Header.Set(StateHeader, state)
	}
	req.Close = true
	client, err := freshProbeClient(proxyURL, upstreamProxyURL)
	if err != nil {
		return result, errors.New("probe transport unavailable")
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return result, errors.New("probe transport failed")
	}
	defer response.Body.Close()
	status := response.StatusCode
	result.Status = status
	mergeResponseCookies(result.Cookies, response.Cookies())
	if status != http.StatusOK {
		return result, errors.New("probe request rejected")
	}
	observer := newCompletionObserver(model)
	buf := make([]byte, 16*1024)
	total := 0
	for {
		n, readErr := response.Body.Read(buf)
		if n > 0 {
			total += n
			if total > 4*1024*1024 {
				result.Model = observer.ActualModel()
				return result, errors.New("probe response too large")
			}
			observer.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			result.Model = observer.ActualModel()
			return result, errors.New("probe response interrupted")
		}
	}
	observer.Finish()
	result.Model = observer.ActualModel()
	complete, matches := observer.Result()
	if !complete || !matches {
		return result, errors.New("probe did not complete with requested model")
	}
	result.State = strings.TrimSpace(response.Header.Get(StateHeader))
	return result, nil
}

func cookieHeader(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+cookies[name])
	}
	return strings.Join(parts, "; ")
}

func mergeResponseCookies(jar map[string]string, cookies []*http.Cookie) {
	if jar == nil {
		return
	}
	for _, cookie := range cookies {
		if cookie == nil || strings.TrimSpace(cookie.Name) == "" {
			continue
		}
		if cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && cookie.Expires.Before(time.Now())) {
			delete(jar, cookie.Name)
			continue
		}
		jar[cookie.Name] = cookie.Value
	}
}

var sidPattern = regexp.MustCompile(`(?i)(-sid-)[^-]+`)

func rotateProxy(raw string) (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", errors.New("random session unavailable")
	}
	sid := fmt.Sprint(n.Int64() + 100000)
	expanded := strings.NewReplacer("{sid}", sid, "{random}", sid).Replace(raw)
	u, err := url.Parse(expanded)
	if err != nil {
		return "", errors.New("invalid proxy")
	}
	// 1024proxy's sticky-session username is explicitly rotated once per attempt.
	if (strings.HasSuffix(strings.ToLower(u.Hostname()), ".1024proxy.io") || strings.EqualFold(u.Hostname(), "1024proxy.io")) && u.User != nil {
		name := u.User.Username()
		if sidPattern.MatchString(name) {
			name = sidPattern.ReplaceAllString(name, "${1}"+sid)
		} else {
			name += "-sid-" + sid
		}
		if pass, ok := u.User.Password(); ok {
			u.User = url.UserPassword(name, pass)
		} else {
			u.User = url.User(name)
		}
	}
	if err = validateProxy(u.String()); err != nil {
		return "", err
	}
	return u.String(), nil
}
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cryptographic randomness unavailable")
	}
	return hex.EncodeToString(b[:])
}
