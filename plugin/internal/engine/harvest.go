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
			if validTicket(t, e.config, a, model, now) && t.ExpiresAt.Sub(now) > effectiveRefreshBefore(e.config, a) {
				continue
			}
			c := e.config
			gen := e.generation
			ctx := e.generationCtx
			host := e.host
			e.jobs[k] = gen
			e.wg.Add(1)
			go e.collect(ctx, host, c, a, model, k, gen)
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
func (e *Engine) collect(ctx context.Context, host pluginv1.HostServiceClient, c Config, a AccountConfig, model, k string, gen uint64) {
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
	if restored, status := e.restore(ctx, host, c, a, model, k, gen, identity, fp); restored {
		success = true
		return
	} else if isStopStatus(status) {
		reason = stopReason(status)
		return
	}
	preferPrevious := c.PreferPreviousIP && a.EgressMode == egressModeGenerator
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
		generatorMode := a.EgressMode == egressModeGenerator
		captureEgress := ""
		captureCountry := ""
		if generatorMode {
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
		candidate, status, captureModel, err := e.probe(ctx, identity, model, targetProxyURL, upstreamProxyURL, "")
		if !generatorMode {
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
		if a.EgressMode == egressModeSub2 {
			fixedProxyURL, fixedUpstreamProxyURL, err = accountEgress(c, a, fixed.ProxyUrl, nil)
			if err != nil {
				reason = "account_egress_invalid"
				return
			}
		}
		validationStarted := time.Now()
		fixedEgress := e.lookupEgressIP(ctx, fixedProxyURL, fixedUpstreamProxyURL)
		if generatorMode {
			fixedEgress = e.lookupEgressIPRequired(ctx, fixedProxyURL, fixedUpstreamProxyURL)
		}
		validationEvent := diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "fixed_validation", Attempt: attempt,
			UpstreamProxy: fixedUpstreamProxyURL, TargetProxy: fixedProxyURL, CaptureEgress: captureEgress, FixedEgress: fixedEgress,
			EgressMatch: egressMatch(captureEgress, fixedEgress)}
		if generatorMode && fixedEgress == "" {
			reason = "generator_egress_unavailable"
			validationEvent.Outcome = "egress_unavailable"
			validationEvent.Error = reason
			e.recordDiagnostic(validationEvent)
			continue
		}
		if (a.EgressMode == egressModePlugin || generatorMode) && captureEgress != "" && fixedEgress != "" && captureEgress != fixedEgress {
			reason = "sticky_egress_changed"
			validationEvent.Outcome = "egress_changed"
			e.recordDiagnostic(validationEvent)
			continue
		}
		returned, status, validationModel, err := e.probe(ctx, fixed, model, fixedProxyURL, fixedUpstreamProxyURL, candidate)
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
		validationEvent.Outcome = "accepted"
		e.recordDiagnostic(validationEvent)
		generatedProxyURL := ""
		if generatorMode {
			generatedProxyURL = fixedProxyURL
		}
		t := &ticket{AccountID: a.AccountID, Model: model, Plan: a.Plan, State: candidate, Version: randomID(),
			ConfigFingerprint: fp, FixedFingerprint: proxyFingerprint(fixedProxyURL), GeneratedProxyURL: generatedProxyURL,
			GeneratedEgressIP: captureEgress, IdentityFingerprint: stableIdentity(fixed), CapturedAt: captured,
			ExpiresAt: captured.Add(time.Duration(effectiveTTLMinutes(c, a)) * time.Minute)}
		if e.commit(ctx, host, c, a, model, k, gen, t, true) {
			if generatorMode {
				e.rememberPreviousEgress(ctx, host, a.AccountID, fixedProxyURL, captureEgress)
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

func (e *Engine) restore(ctx context.Context, host pluginv1.HostServiceClient, c Config, a AccountConfig, model, k string, gen uint64, identity *pluginv1.ResolveOutboundIdentityResponse, fp string) (bool, int) {
	e.mu.Lock()
	current := e.tickets[k]
	revoked := e.revoked[k]
	e.mu.Unlock()
	if validTicket(current, c, a, model, time.Now()) {
		return false, 0
	} // Renewal keeps the old usable ticket throughout.
	cc, cancel := context.WithTimeout(ctx, 3*time.Second)
	r, err := host.KVGet(cc, &pluginv1.KVGetRequest{Namespace: namespace, Key: kvKey(a.AccountID, model, fp)})
	cancel()
	if err != nil || r == nil || !r.Found || len(r.Value) > 16*1024 {
		return false, 0
	}
	var t ticket
	if json.Unmarshal(r.Value, &t) != nil {
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
	if a.EgressMode == egressModeGenerator {
		fixedEgress = e.lookupEgressIPRequired(ctx, targetProxyURL, upstreamProxyURL)
	}
	event := diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "restore",
		UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL,
		FixedEgress: fixedEgress}
	if a.EgressMode == egressModeGenerator && (fixedEgress == "" || (t.GeneratedEgressIP != "" && fixedEgress != t.GeneratedEgressIP)) {
		event.Outcome = "egress_changed"
		event.Error = "generator_egress_unavailable"
		if fixedEgress != "" {
			event.Error = "sticky_egress_changed"
		}
		event.DurationMS = time.Since(restoreStarted).Milliseconds()
		e.recordDiagnostic(event)
		return false, 0
	}
	returned, status, responseModel, err := e.probe(ctx, identity, model, targetProxyURL, upstreamProxyURL, t.State)
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
	event.Outcome = "accepted"
	e.recordDiagnostic(event)
	if !e.commit(ctx, host, c, a, model, k, gen, &t, false) {
		return false, status
	}
	if a.EgressMode == egressModeGenerator {
		e.rememberPreviousEgress(ctx, host, a.AccountID, t.GeneratedProxyURL, t.GeneratedEgressIP)
	}
	return true, status
}
func (e *Engine) commit(ctx context.Context, host pluginv1.HostServiceClient, c Config, a AccountConfig, model, k string, gen uint64, t *ticket, persist bool) bool {
	e.persistMu.Lock()
	defer e.persistMu.Unlock()
	valid := func() bool {
		return e.activeLocked(k, gen) && ctx.Err() == nil && e.directory[a.AccountID] && validTicket(t, c, a, model, time.Now()) && e.revoked[k] != t.Version
	}
	e.mu.Lock()
	ok := valid()
	e.mu.Unlock()
	if !ok {
		return false
	}
	if persist {
		cc, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := host.KVSet(cc, &pluginv1.KVSetRequest{Namespace: namespace, Key: kvKey(a.AccountID, model, t.ConfigFingerprint), Value: []byte(jsonText(t)), TtlSeconds: max(1, int64(time.Until(t.ExpiresAt).Seconds()))})
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
	e.tickets[k] = t
	delete(e.revoked, k)
	return true
}

func (e *Engine) probe(ctx context.Context, identity *pluginv1.ResolveOutboundIdentityResponse, model, proxyURL, upstreamProxyURL, state string) (string, int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	payload := map[string]any{"model": model, "store": false, "stream": true, "instructions": "Reply with exactly: pong", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "ping"}}}}}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.probeURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, "", errors.New("probe construction failed")
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
	req.Header.Set("session_id", randomID())
	req.Header.Set("version", "0.153.4")
	req.Header.Set("User-Agent", "codex_cli_rs/0.153.4")
	req.Header.Set("originator", "codex_cli_rs")
	if state != "" {
		req.Header.Set(StateHeader, state)
	}
	req.Close = true
	client, err := freshProbeClient(proxyURL, upstreamProxyURL)
	if err != nil {
		return "", 0, "", errors.New("probe transport unavailable")
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return "", 0, "", errors.New("probe transport failed")
	}
	defer response.Body.Close()
	status := response.StatusCode
	if status != http.StatusOK {
		return "", status, "", errors.New("probe request rejected")
	}
	observer := newCompletionObserver(model)
	buf := make([]byte, 16*1024)
	total := 0
	for {
		n, readErr := response.Body.Read(buf)
		if n > 0 {
			total += n
			if total > 4*1024*1024 {
				return "", status, observer.ActualModel(), errors.New("probe response too large")
			}
			observer.Write(buf[:n])
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", status, observer.ActualModel(), errors.New("probe response interrupted")
		}
	}
	observer.Finish()
	complete, matches := observer.Result()
	if !complete || !matches {
		return "", status, observer.ActualModel(), errors.New("probe did not complete with requested model")
	}
	return strings.TrimSpace(response.Header.Get(StateHeader)), status, observer.ActualModel(), nil
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
