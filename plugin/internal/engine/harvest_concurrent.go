package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
)

type captureAttemptResult struct {
	Attempt             int
	StartedAt           time.Time
	Identity            *pluginv1.ResolveOutboundIdentityResponse
	TargetProxyURL      string
	UpstreamProxyURL    string
	GeneratorMode       bool
	CaptureEgress       string
	CaptureCountry      string
	SessionID           string
	Cookies             map[string]string
	Candidate           string
	Status              int
	CaptureModel        string
	CapturedAt          time.Time
	Reason              string
	Err                 error
	ReusedCurrentEgress bool
}

// captureCandidates runs the generator/egress/probe portion of minting in
// parallel while sharing one configured attempt budget. The returned cancel
// function stops all workers as soon as a strict ticket is committed.
func (e *Engine) captureCandidates(
	ctx context.Context,
	host pluginv1.HostServiceClient,
	c Config,
	a AccountConfig,
	model, k string,
	gen uint64,
	attemptLimit int,
	preferPrevious bool,
	reuseCurrentEgress bool,
) (<-chan captureAttemptResult, context.CancelFunc) {
	workerCtx, cancel := context.WithCancel(ctx)
	workers := c.MintConcurrency
	if workers < 1 {
		workers = 1
	}
	if workers > attemptLimit {
		workers = attemptLimit
	}
	results := make(chan captureAttemptResult, workers)
	var nextAttempt atomic.Int32
	var firstReuse atomic.Bool
	var lastReasonMu sync.Mutex
	lastReason := ""
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				attempt := int(nextAttempt.Add(1))
				if attempt > attemptLimit || workerCtx.Err() != nil {
					return
				}
				if attempt > workers {
					delay := time.Duration(c.AttemptIntervalSeconds) * time.Second
					lastReasonMu.Lock()
					fast := fastRetryReason(lastReason)
					lastReasonMu.Unlock()
					if fast && delay > time.Second {
						delay = time.Second
					}
					timer := time.NewTimer(delay)
					select {
					case <-timer.C:
					case <-workerCtx.Done():
						timer.Stop()
						return
					}
				}
				result := e.captureOne(workerCtx, host, c, a, model, k, gen, attempt, preferPrevious, reuseCurrentEgress, &firstReuse)
				lastReasonMu.Lock()
				if result.Reason != "" {
					lastReason = result.Reason
				}
				lastReasonMu.Unlock()
				select {
				case results <- result.captureAttemptResult:
				case <-workerCtx.Done():
					return
				}
				if result.Stop {
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	return results, cancel
}

type captureOneResult struct {
	captureAttemptResult
	Stop bool
}

func (e *Engine) captureOne(
	ctx context.Context,
	host pluginv1.HostServiceClient,
	c Config,
	a AccountConfig,
	model, k string,
	gen uint64,
	attempt int,
	preferPrevious bool,
	reuseCurrentEgress bool,
	firstReuse *atomic.Bool,
) captureOneResult {
	result := captureOneResult{captureAttemptResult: captureAttemptResult{
		Attempt:          attempt,
		StartedAt:        time.Now(),
		UpstreamProxyURL: c.UpstreamProxyURL,
		Cookies:          map[string]string{},
	}}
	e.note(k, gen, attempt, "")
	identity, err := resolveIdentity(ctx, host, a.AccountID)
	if err != nil {
		result.Reason = "identity_unavailable"
		result.Err = err
		result.Stop = true
		return result
	}
	result.Identity = identity

	cookieMode := c.TicketMode == ticketModeCookie
	generatorCapture := a.EgressMode == egressModeGenerator
	if cookieMode {
		generatorCapture = c.CookieCaptureMode == captureModeGenerator
	}
	result.GeneratorMode = generatorCapture
	first := firstReuse.CompareAndSwap(false, true)
	reusePrevious := preferPrevious && first
	reuseCurrent := reuseCurrentEgress && first

	if reuseCurrent {
		e.mu.Lock()
		current := e.tickets[k]
		e.mu.Unlock()
		if validTicket(current, c, a, model, time.Now()) {
			result.TargetProxyURL = current.GeneratedProxyURL
			result.ReusedCurrentEgress = result.TargetProxyURL != ""
		}
	}

	if result.TargetProxyURL == "" {
		if cookieMode {
			if c.CookieCaptureMode == captureModeSOCKS5 {
				result.TargetProxyURL = c.CookieCaptureProxyURL
			} else {
				if reusePrevious {
					result.TargetProxyURL = e.preferredPreviousProxy(ctx, host, a.AccountID, k)
				}
				if result.TargetProxyURL == "" {
					result.TargetProxyURL, err = e.generateProxy(ctx, c)
					if err != nil {
						result.Reason = "generator_unavailable"
						result.Err = err
						e.recordDiagnostic(diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
							UpstreamProxy: result.UpstreamProxyURL, Outcome: "generator_failed", Error: result.Reason,
							DurationMS: time.Since(result.StartedAt).Milliseconds()})
						return result
					}
				}
			}
		} else {
			switch a.EgressMode {
			case egressModePlugin:
				result.TargetProxyURL = a.StickyProxyURL
			case egressModeGenerator:
				if reusePrevious {
					result.TargetProxyURL = e.preferredPreviousProxy(ctx, host, a.AccountID, k)
				}
				if result.TargetProxyURL == "" {
					result.TargetProxyURL, err = e.generateProxy(ctx, c)
					if err != nil {
						result.Reason = "generator_unavailable"
						result.Err = err
						e.recordDiagnostic(diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
							UpstreamProxy: result.UpstreamProxyURL, Outcome: "generator_failed", Error: result.Reason,
							DurationMS: time.Since(result.StartedAt).Milliseconds()})
						return result
					}
				}
			default:
				result.TargetProxyURL, err = rotateProxy(c.DynamicProxyURL)
				if err != nil {
					result.Reason = "invalid_dynamic_proxy"
					result.Err = err
					result.Stop = true
					return result
				}
			}
		}
	}

	if cookieMode || generatorCapture {
		info := e.lookupEgressInfo(ctx, result.TargetProxyURL, result.UpstreamProxyURL)
		result.CaptureEgress = info.IP
		result.CaptureCountry = info.Country
		if result.CaptureEgress == "" {
			result.Reason = "generator_egress_unavailable"
			e.recordDiagnostic(diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
				UpstreamProxy: result.UpstreamProxyURL, TargetProxy: result.TargetProxyURL, Outcome: "egress_unavailable", Error: result.Reason})
			return result
		}
		if countryBlocked(result.CaptureCountry, c.ProxyGeneratorBlockedCountries) {
			result.Reason = "generator_region_blocked"
			e.recordDiagnostic(diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
				UpstreamProxy: result.UpstreamProxyURL, TargetProxy: result.TargetProxyURL, CaptureEgress: result.CaptureEgress,
				Outcome: "region_blocked", Error: result.Reason})
			return result
		}
	}

	result.SessionID = randomID()
	if c.RouteCookieReuse {
		result.Cookies = e.preferredRouteCookies(a.AccountID, c.GatewayPolicy, c.TargetGateway)
	}
	result.CapturedAt = time.Now()
	captureResult, err := e.probe(ctx, identity, model, result.TargetProxyURL, result.UpstreamProxyURL, "", result.SessionID, result.Cookies, c.MintFingerprintConvergence)
	result.Candidate = captureResult.State
	result.Status = captureResult.Status
	result.CaptureModel = captureResult.Model
	result.Cookies = captureResult.Cookies
	result.Err = err
	if c.RouteCookieReuse {
		e.rememberRouteCookies(a.AccountID, result.Cookies, c.GatewayPolicy, c.TargetGateway)
	}
	if !generatorCapture && !cookieMode {
		result.CaptureEgress = e.lookupEgressIP(ctx, result.TargetProxyURL, result.UpstreamProxyURL)
	}
	captureEvent := diagnosticEvent{AccountID: a.AccountID, Model: model, Stage: "capture", Attempt: attempt,
		UpstreamProxy: result.UpstreamProxyURL, TargetProxy: result.TargetProxyURL, CaptureEgress: result.CaptureEgress,
		StateLength: len(result.Candidate), StateClass: stateDiagnosticClass(result.Candidate), StateFingerprint: stateFingerprint(result.Candidate),
		TicketAgeSeconds: stateAgeSeconds(result.Candidate, time.Now()), Gateway: gatewayFromCookies(result.Cookies),
		CookieFingerprint: cookieFingerprint(result.Cookies), ResponseModel: result.CaptureModel,
		HTTPStatus: result.Status, DurationMS: time.Since(result.StartedAt).Milliseconds()}
	if err != nil {
		result.Reason = "harvest_failed"
		captureEvent.Outcome = "probe_failed"
		captureEvent.Error = err.Error()
		e.recordDiagnostic(captureEvent)
		result.Stop = isStopStatus(result.Status)
		return result
	}
	if !validPlanState(result.Candidate, a.Plan, c.AllowState780) {
		result.Reason = "unexpected_state_length"
		captureEvent.Outcome = "unexpected_state"
		e.recordDiagnostic(captureEvent)
		return result
	}
	if _, gatewayReason := routeGatewayAcceptance(result.Candidate, result.Cookies, c.GatewayPolicy, c.TargetGateway); gatewayReason != "" {
		result.Reason = gatewayReason
		captureEvent.Outcome = gatewayReason
		captureEvent.Error = gatewayReason
		e.recordDiagnostic(captureEvent)
		return result
	}
	captureEvent.Outcome = "accepted"
	e.recordDiagnostic(captureEvent)
	return result
}
