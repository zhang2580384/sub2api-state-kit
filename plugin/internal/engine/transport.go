package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
	"golang.org/x/net/proxy"
)

const (
	// Only explicitly enabled accounts are buffered for model inspection. Match
	// typical large code/image prompts while bounding per-request memory use.
	maxTicketRequestBody = 64 << 20
	maxForwardDuration   = 10 * time.Minute
	maxPooledClients     = 32
)

type pooledClient struct {
	client *http.Client
	used   uint64
}
type clientPool struct {
	mu      sync.Mutex
	clients map[[32]byte]pooledClient
	clock   uint64
}

func newClientPool() *clientPool { return &clientPool{clients: make(map[[32]byte]pooledClient)} }

func (p *clientPool) client(proxyURL, upstreamProxyURL string) (*http.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := sha256.Sum256([]byte(proxyURL + "\x00" + upstreamProxyURL))
	p.clock++
	if item, ok := p.clients[key]; ok {
		item.used = p.clock
		p.clients[key] = item
		return item.client, nil
	}
	client, err := makeHTTPClient(proxyURL, upstreamProxyURL, false)
	if err != nil {
		return nil, err
	}
	if len(p.clients) >= maxPooledClients {
		var oldestKey [32]byte
		oldest := ^uint64(0)
		for k, item := range p.clients {
			if item.used < oldest {
				oldestKey, oldest = k, item.used
			}
		}
		p.clients[oldestKey].client.CloseIdleConnections()
		delete(p.clients, oldestKey)
	}
	p.clients[key] = pooledClient{client: client, used: p.clock}
	return client, nil
}

func (p *clientPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, item := range p.clients {
		item.client.CloseIdleConnections()
		delete(p.clients, key)
	}
}

func freshProbeClient(proxyURL, upstreamProxyURL string) (*http.Client, error) {
	return makeHTTPClient(proxyURL, upstreamProxyURL, true)
}

func makeHTTPClient(proxyURL, upstreamProxyURL string, fresh bool) (*http.Client, error) {
	var proxy func(*http.Request) (*url.URL, error)
	if proxyURL != "" {
		parsed, err := parseProxyURL(proxyURL)
		if err != nil {
			return nil, err
		}
		proxy = http.ProxyURL(parsed)
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	dialContext := dialer.DialContext
	if upstreamProxyURL != "" {
		upstream, err := parseProxyURL(upstreamProxyURL)
		if err != nil {
			return nil, err
		}
		dialContext = dialThroughProxy(upstream, dialer)
	}
	transport := &http.Transport{
		Proxy:                 proxy,
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true,
		DisableKeepAlives:     fresh,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	timeout := maxForwardDuration
	if fresh {
		timeout = 60 * time.Second
	}
	return &http.Client{
		Transport: transport, Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func parseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Fragment != "" || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("invalid proxy configuration")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, errors.New("unsupported proxy protocol")
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("invalid proxy port")
		}
	}
	return u, nil
}

func proxyAddress(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func proxyAuth(u *url.URL) *proxy.Auth {
	if u.User == nil {
		return nil
	}
	username := u.User.Username()
	if username == "" {
		return nil
	}
	password, _ := u.User.Password()
	return &proxy.Auth{User: username, Password: password}
}

func dialThroughProxy(upstream *url.URL, dialer *net.Dialer) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" && network != "tcp4" && network != "tcp6" {
			return nil, errors.New("only TCP proxy dialing is supported")
		}
		switch upstream.Scheme {
		case "http", "https":
			return dialHTTPProxy(ctx, upstream, proxyAddress(upstream), address, dialer)
		case "socks5", "socks5h":
			return dialSOCKS5Proxy(upstream, address, dialer)
		default:
			return nil, errors.New("unsupported upstream proxy protocol")
		}
	}
}

func dialHTTPProxy(ctx context.Context, upstream *url.URL, proxyAddr, targetAddr string, dialer *net.Dialer) (net.Conn, error) {
	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if upstream.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: upstream.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		conn = tlsConn
	}
	var request strings.Builder
	fmt.Fprintf(&request, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: Keep-Alive\r\n", targetAddr, targetAddr)
	if upstream.User != nil {
		username := upstream.User.Username()
		if username != "" {
			password, _ := upstream.User.Password()
			token := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
			fmt.Fprintf(&request, "Proxy-Authorization: Basic %s\r\n", token)
		}
	}
	request.WriteString("\r\n")
	if _, err := io.WriteString(conn, request.String()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("upstream proxy CONNECT rejected: %s", response.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufferedConn{Conn: conn, reader: reader}, nil
}

func dialSOCKS5Proxy(upstream *url.URL, targetAddr string, dialer *net.Dialer) (net.Conn, error) {
	socksDialer, err := proxy.SOCKS5("tcp", proxyAddress(upstream), proxyAuth(upstream), dialer)
	if err != nil {
		return nil, err
	}
	return socksDialer.Dial("tcp", targetAddr)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(data []byte) (int, error) {
	return c.reader.Read(data)
}

func (e *Engine) Forward(stream pluginv1.TransportPlugin_ForwardServer) error {
	started := time.Now()
	ctx, cancel := context.WithTimeout(stream.Context(), maxForwardDuration)
	defer cancel()
	if e.ctx != nil {
		stop := context.AfterFunc(e.ctx, cancel)
		defer stop()
	}
	first, err := recvFrame(ctx, stream)
	if err != nil {
		return sendForwardError(stream, "invalid_request", "Request start is missing", false)
	}
	start := first.GetStart()
	if start == nil {
		return sendForwardError(stream, "invalid_request", "The first frame must be a request start", false)
	}
	u, err := url.Parse(start.Url)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return sendForwardError(stream, "invalid_request", "Invalid upstream URL", false)
	}
	var body io.Reader
	var pipe *io.PipeReader
	var ticket *receipt
	var model string
	var bufferedBodyLength int64
	bufferedBody := false
	rewriteRequested, requestTimezone := e.requestRewriteSettings(start)
	ticketAccount := e.accountEnabled(start)
	if ticketAccount || (rewriteRequested && start.HasBody) {
		data, readErr := readConfiguredBody(ctx, stream, start.HasBody)
		if readErr != nil {
			return sendTicketUnavailable(stream)
		}
		if ticketAccount {
			var request struct {
				Model string `json:"model"`
			}
			if json.Unmarshal(data, &request) != nil || strings.TrimSpace(request.Model) == "" {
				return sendTicketUnavailable(stream)
			}
			model = request.Model
			ticket, err = e.ticketForRequest(ctx, start, model)
			if err != nil {
				e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "ticket_lookup",
					TargetProxy: start.ProxyUrl, Outcome: "unavailable", Error: "state_ticket_unavailable"})
				return sendTicketUnavailable(stream)
			}
		}
		if rewriteRequested {
			if rewritten, changed := rewriteRequestData(data, requestTimezone, time.Now()); changed {
				data = rewritten
			}
		}
		if start.HasBody {
			body = bytes.NewReader(data)
			bufferedBodyLength = int64(len(data))
			bufferedBody = true
		}
	} else if start.HasBody {
		var writer *io.PipeWriter
		pipe, writer = io.Pipe()
		defer pipe.Close()
		body = pipe
		go receiveBody(stream, writer)
	}
	req, err := http.NewRequestWithContext(ctx, start.Method, start.Url, body)
	if err != nil {
		return sendForwardError(stream, "invalid_request", "Invalid upstream request", false)
	}
	for name, values := range start.Headers {
		if values == nil {
			continue
		}
		req.Header[name] = append([]string(nil), values.Values...)
	}
	if start.Host != "" {
		req.Host = start.Host
	}
	if rewriteRequested {
		rewriteAcceptLanguage(req.Header)
	}
	req.ContentLength = start.ContentLength
	if bufferedBody {
		req.ContentLength = bufferedBodyLength
	}
	// Do not make a buffered business POST eligible for transport-level replay.
	req.GetBody = nil
	if !start.HasBody {
		req.ContentLength = 0
	}
	if ticket != nil {
		cookieMode := ticket.TicketMode == ticketModeCookie
		sessionBound := cookieMode || ticket.SessionBound
		// Remove differently cased map keys as well; Set alone canonicalizes only
		// the new key and could leave a caller-supplied duplicate header intact.
		for key := range req.Header {
			if strings.EqualFold(key, StateHeader) || strings.EqualFold(key, "Accept-Encoding") ||
				sessionBound && (strings.EqualFold(key, "Cookie") ||
					strings.EqualFold(key, "session_id") || strings.EqualFold(key, "session-id")) {
				delete(req.Header, key)
			}
		}
		req.Header.Set(StateHeader, ticket.State)
		if sessionBound {
			if ticket.SessionID != "" {
				req.Header.Set("session_id", ticket.SessionID)
				req.Header.Set("session-id", ticket.SessionID)
			}
			if cookie := cookieHeader(ticket.Cookies); cookie != "" {
				req.Header.Set("Cookie", cookie)
			}
		}
		// Completion inspection observes the bytes actually forwarded to the host.
		// Negotiate an uncompressed response only when we inject a ticket; normal
		// passthrough preserves the caller's compression preferences and raw bytes.
		req.Header.Set("Accept-Encoding", "identity")
	}
	targetProxyURL := start.ProxyUrl
	upstreamProxyURL := ""
	if ticket != nil {
		targetProxyURL = ticket.TargetProxyURL
		upstreamProxyURL = ticket.UpstreamProxyURL
	}
	client, err := e.clients.client(targetProxyURL, upstreamProxyURL)
	if err != nil {
		e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "business",
			UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, Outcome: "invalid_proxy"})
		return sendForwardError(stream, "invalid_proxy", "Invalid proxy configuration", false)
	}
	requestStarted := time.Now()
	response, err := client.Do(req)
	if err != nil {
		code, message, requestSent := classifyForwardTransportFailure(err)
		e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "business",
			UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, Outcome: code, Error: code, DurationMS: time.Since(requestStarted).Milliseconds()})
		return sendForwardError(stream, code, message, requestSent)
	}
	defer response.Body.Close()
	if ticket != nil {
		e.updateTicketSession(ticket, response)
	}
	if pipe != nil {
		_ = pipe.Close()
	}
	headers := make(map[string]*pluginv1.HeaderValues, len(response.Header))
	for name, values := range response.Header {
		headers[name] = &pluginv1.HeaderValues{Values: append([]string(nil), values...)}
	}
	if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Start{Start: &pluginv1.ForwardResponseStart{
		StatusCode: int32(response.StatusCode), Status: response.Status, Protocol: response.Proto,
		ProtocolMajor: int32(response.ProtoMajor), ProtocolMinor: int32(response.ProtoMinor),
		Headers: headers, ContentLength: response.ContentLength,
	}}}); err != nil {
		return err
	}
	var observer *completionObserver
	if ticket != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		responseState := response.Header.Get(StateHeader)
		outcome := "headers_received"
		if validState(responseState, 312) {
			outcome = "state_312"
		}
		e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "business",
			UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, StateLength: len(ticket.State), StateClass: stateDiagnosticClass(ticket.State),
			StateFingerprint: stateFingerprint(ticket.State), TicketAgeSeconds: stateAgeSeconds(ticket.State, time.Now()),
			Gateway: ticket.Gateway, CookieFingerprint: cookieFingerprint(ticket.Cookies), SessionBound: ticket.SessionBound,
			HTTPStatus: response.StatusCode, Outcome: outcome, DurationMS: time.Since(requestStarted).Milliseconds()})
		if validState(response.Header.Get(StateHeader), 312) {
			e.invalidate(ticket, "state_312")
		}
		observer = newCompletionObserver(model)
	}
	buffer := make([]byte, 32*1024)
	var received int64
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			chunk := append([]byte(nil), buffer[:n]...)
			if observer != nil {
				observer.Write(chunk)
			}
			if err := stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_BodyChunk{BodyChunk: chunk}}); err != nil {
				return err
			}
			received += int64(n)
			// Consumers may close the transport as soon as response.completed is
			// delivered. Observe that completion now rather than waiting for EOF.
			if observer != nil {
				if complete, matches := observer.Result(); complete && !matches {
					actualModel := observer.ActualModel()
					e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "business_result",
						UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, StateClass: stateDiagnosticClass(ticket.State),
						StateFingerprint: stateFingerprint(ticket.State), TicketAgeSeconds: stateAgeSeconds(ticket.State, time.Now()),
						Gateway: ticket.Gateway, CookieFingerprint: cookieFingerprint(ticket.Cookies), SessionBound: ticket.SessionBound,
						ResponseModel: actualModel, HTTPStatus: response.StatusCode, Outcome: "model_mismatch",
						DurationMS: time.Since(requestStarted).Milliseconds()})
					e.invalidate(ticket, "model_mismatch")
					observer = nil
				}
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				responseModel := ""
				if observer != nil {
					responseModel = observer.ActualModel()
				}
				if ticket != nil {
					e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "business_result",
						UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, StateClass: stateDiagnosticClass(ticket.State),
						StateFingerprint: stateFingerprint(ticket.State), TicketAgeSeconds: stateAgeSeconds(ticket.State, time.Now()),
						Gateway: ticket.Gateway, CookieFingerprint: cookieFingerprint(ticket.Cookies), SessionBound: ticket.SessionBound,
						ResponseModel: responseModel, HTTPStatus: response.StatusCode, Outcome: "upstream_read_error",
						DurationMS: time.Since(requestStarted).Milliseconds()})
				}
				return sendForwardError(stream, "upstream_read", "Upstream response was interrupted", true)
			}
			break
		}
	}
	if observer != nil {
		observer.Finish()
		if complete, matches := observer.Result(); complete && !matches {
			e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "business_result",
				UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, StateClass: stateDiagnosticClass(ticket.State),
				StateFingerprint: stateFingerprint(ticket.State), TicketAgeSeconds: stateAgeSeconds(ticket.State, time.Now()),
				Gateway: ticket.Gateway, CookieFingerprint: cookieFingerprint(ticket.Cookies), SessionBound: ticket.SessionBound,
				ResponseModel: observer.ActualModel(), HTTPStatus: response.StatusCode, Outcome: "model_mismatch",
				DurationMS: time.Since(requestStarted).Milliseconds()})
			e.invalidate(ticket, "model_mismatch")
		} else if complete {
			e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "business_result",
				UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, StateClass: stateDiagnosticClass(ticket.State),
				StateFingerprint: stateFingerprint(ticket.State), TicketAgeSeconds: stateAgeSeconds(ticket.State, time.Now()),
				Gateway: ticket.Gateway, CookieFingerprint: cookieFingerprint(ticket.Cookies), SessionBound: ticket.SessionBound,
				ResponseModel: observer.ActualModel(), HTTPStatus: response.StatusCode, Outcome: "model_match",
				DurationMS: time.Since(requestStarted).Milliseconds()})
		} else {
			e.recordDiagnostic(diagnosticEvent{AccountID: start.AccountId, Model: model, Stage: "business_result",
				UpstreamProxy: upstreamProxyURL, TargetProxy: targetProxyURL, StateClass: stateDiagnosticClass(ticket.State),
				StateFingerprint: stateFingerprint(ticket.State), TicketAgeSeconds: stateAgeSeconds(ticket.State, time.Now()),
				Gateway: ticket.Gateway, CookieFingerprint: cookieFingerprint(ticket.Cookies), SessionBound: ticket.SessionBound,
				ResponseModel: observer.ActualModel(), HTTPStatus: response.StatusCode, Outcome: "incomplete",
				DurationMS: time.Since(requestStarted).Milliseconds()})
		}
	}
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_End{End: &pluginv1.ForwardResponseEnd{
		BytesReceived: received, DurationMs: time.Since(started).Milliseconds(),
	}}})
}

func classifyForwardTransportFailure(err error) (code, message string, requestSent bool) {
	if err == nil {
		return "upstream_transport", "Upstream transport failed", true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "upstream_transport_dns", "Upstream transport failed: DNS lookup failed", false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "upstream_transport_connect", "Upstream transport failed: connection refused", false
	}
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		return "upstream_transport_connect", "Upstream transport failed: network unreachable", false
	}

	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "tls:") || strings.Contains(lower, "x509:") || strings.Contains(lower, "certificate") || strings.Contains(lower, "handshake") {
		return "upstream_transport_tls", "Upstream transport failed: TLS handshake failed", false
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) && (opErr.Op == "dial" || opErr.Op == "proxyconnect") {
		return "upstream_transport_connect", "Upstream transport failed: connection failed", false
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(lower, "timeout") {
		return "upstream_transport_timeout", "Upstream transport failed: timeout", true
	}
	if strings.Contains(lower, "connection reset") || strings.Contains(lower, "broken pipe") {
		return "upstream_transport_reset", "Upstream transport failed: connection reset", true
	}
	return "upstream_transport", "Upstream transport failed", true
}

func (e *Engine) updateTicketSession(receipt *receipt, response *http.Response) {
	if receipt == nil || response == nil || (receipt.TicketMode != ticketModeCookie && !receipt.SessionBound) {
		return
	}
	responseState := strings.TrimSpace(response.Header.Get(StateHeader))
	if responseState != "" && validState(responseState, 312) {
		return
	}
	responseCookies := response.Cookies()
	if responseState == "" && len(responseCookies) == 0 {
		return
	}
	var routeAccountID int64
	var routeSnapshot map[string]string
	var routeTargetGateway string
	e.mu.Lock()
	if e.closed || e.generation != receipt.Generation {
		e.mu.Unlock()
		return
	}
	t := e.tickets[receipt.Key]
	if t == nil || t.Version != receipt.Version || t.ConfigFingerprint != receipt.ConfigFingerprint ||
		(t.TicketMode != ticketModeCookie && !t.SessionBound) {
		e.mu.Unlock()
		return
	}
	if len(responseCookies) > 0 {
		if t.Cookies == nil {
			t.Cookies = map[string]string{}
		}
		mergeResponseCookies(t.Cookies, responseCookies)
	}
	if responseState != "" && validPlanState(responseState, t.Plan, e.config.AllowState780) {
		if _, gatewayReason := routeGatewayAcceptance(responseState, t.Cookies, e.config.TargetGateway); gatewayReason != "" {
			e.mu.Unlock()
			return
		}
		now := time.Now().UTC()
		t.State = responseState
		t.CapturedAt = now
		t.IssuedAt = ticketIssuedAt(responseState, now)
		if a, ok := findAccount(e.config, t.AccountID); ok {
			t.ExpiresAt = t.IssuedAt.Add(effectiveTicketTTLForState(e.config, a, responseState))
		}
		if stateRequiresSession(responseState) {
			t.SessionBound = true
		}
	}
	t.Gateway = gatewayFromCookies(t.Cookies)
	if e.config.RouteCookieReuse {
		routeAccountID = t.AccountID
		routeSnapshot = cloneCookies(t.Cookies)
		routeTargetGateway = e.config.TargetGateway
	}
	e.mu.Unlock()
	if routeAccountID != 0 {
		e.rememberRouteCookies(routeAccountID, routeSnapshot, routeTargetGateway)
	}
}

func readConfiguredBody(ctx context.Context, stream pluginv1.TransportPlugin_ForwardServer, hasBody bool) ([]byte, error) {
	if !hasBody {
		return nil, nil
	}
	var data []byte
	for {
		frame, err := recvFrame(ctx, stream)
		if err != nil {
			return nil, errors.New("incomplete request body")
		}
		switch value := frame.Frame.(type) {
		case *pluginv1.ForwardRequest_BodyChunk:
			if len(value.BodyChunk) > maxTicketRequestBody-len(data) {
				return nil, errors.New("request body exceeds inspection limit")
			}
			data = append(data, value.BodyChunk...)
		case *pluginv1.ForwardRequest_BodyEnd:
			if !value.BodyEnd {
				return nil, errors.New("invalid body end")
			}
			return data, nil
		default:
			return nil, errors.New("unexpected request frame")
		}
	}
}

// Recv itself is unblocked by gRPC when Forward returns. The buffered result
// channel lets the deadline terminate Forward even if the host stalls mid-frame.
func recvFrame(ctx context.Context, stream pluginv1.TransportPlugin_ForwardServer) (*pluginv1.ForwardRequest, error) {
	type result struct {
		frame *pluginv1.ForwardRequest
		err   error
	}
	ch := make(chan result, 1)
	go func() { frame, err := stream.Recv(); ch <- result{frame, err} }()
	select {
	case result := <-ch:
		return result.frame, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func receiveBody(stream pluginv1.TransportPlugin_ForwardServer, writer *io.PipeWriter) {
	defer writer.Close()
	for {
		frame, err := stream.Recv()
		if err != nil {
			_ = writer.CloseWithError(errors.New("incomplete request body"))
			return
		}
		switch value := frame.Frame.(type) {
		case *pluginv1.ForwardRequest_BodyChunk:
			if _, err := writer.Write(value.BodyChunk); err != nil {
				return
			}
		case *pluginv1.ForwardRequest_BodyEnd:
			if !value.BodyEnd {
				_ = writer.CloseWithError(errors.New("invalid body end"))
			}
			return
		default:
			_ = writer.CloseWithError(errors.New("unexpected request frame"))
			return
		}
	}
}

func sendForwardError(stream pluginv1.TransportPlugin_ForwardServer, code, message string, sent bool) error {
	return stream.Send(&pluginv1.ForwardResponse{Frame: &pluginv1.ForwardResponse_Error{Error: &pluginv1.ForwardResponseError{
		Code: code, Message: message, RequestSent: sent,
	}}})
}

func sendTicketUnavailable(stream pluginv1.TransportPlugin_ForwardServer) error {
	return sendForwardError(stream, "state_ticket_unavailable", "STATE ticket is not ready; fail over to an account with a verified ticket", false)
}
