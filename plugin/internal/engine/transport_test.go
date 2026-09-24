package engine

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
	"google.golang.org/grpc/metadata"
)

type mockForwardStream struct {
	ctx    context.Context
	in     chan *pluginv1.ForwardRequest
	mu     sync.Mutex
	out    []*pluginv1.ForwardResponse
	onSend func(*pluginv1.ForwardResponse)
}

func (s *mockForwardStream) Context() context.Context   { return s.ctx }
func (*mockForwardStream) SetHeader(metadata.MD) error  { return nil }
func (*mockForwardStream) SendHeader(metadata.MD) error { return nil }
func (*mockForwardStream) SetTrailer(metadata.MD)       {}
func (*mockForwardStream) SendMsg(any) error            { return nil }
func (*mockForwardStream) RecvMsg(any) error            { return nil }
func (s *mockForwardStream) Recv() (*pluginv1.ForwardRequest, error) {
	select {
	case f, ok := <-s.in:
		if !ok {
			return nil, io.EOF
		}
		return f, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}
func (s *mockForwardStream) Send(f *pluginv1.ForwardResponse) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.out = append(s.out, f)
	s.mu.Unlock()
	if s.onSend != nil {
		s.onSend(f)
	}
	return nil
}
func (s *mockForwardStream) frames() []*pluginv1.ForwardResponse {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pluginv1.ForwardResponse(nil), s.out...)
}
func mockStart(url string, body bool, length int64) *pluginv1.ForwardRequestStart {
	return &pluginv1.ForwardRequestStart{Method: http.MethodPost, Url: url, AccountId: 7, Platform: "openai", AccountType: "oauth", HasBody: body, ContentLength: length, Headers: map[string]*pluginv1.HeaderValues{}}
}
func frameStart(s *pluginv1.ForwardRequestStart) *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_Start{Start: s}}
}
func frameBody(b []byte) *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyChunk{BodyChunk: b}}
}
func frameEnd() *pluginv1.ForwardRequest {
	return &pluginv1.ForwardRequest{Frame: &pluginv1.ForwardRequest_BodyEnd{BodyEnd: true}}
}
func fixedStream(start *pluginv1.ForwardRequestStart, body []byte) *mockForwardStream {
	s := &mockForwardStream{ctx: context.Background(), in: make(chan *pluginv1.ForwardRequest, 3)}
	s.in <- frameStart(start)
	if start.HasBody {
		s.in <- frameBody(body)
		s.in <- frameEnd()
	}
	close(s.in)
	return s
}
func forwardingEngine(t *testing.T, enabled bool) *Engine {
	c := DefaultConfig()
	c.Enabled = enabled
	c.Accounts = []AccountConfig{{AccountID: 7, Enabled: true, Plan: "pro", Models: []string{"gpt-test"}}}
	e := &Engine{config: c, clients: newClientPool(), directory: map[int64]bool{7: true}, hostReady: true, tickets: map[string]*ticket{}, revoked: map[string]string{}, records: map[string]*jobRecord{}, wake: make(chan struct{}, 1)}
	t.Cleanup(e.clients.Close)
	return e
}
func addForwardTicket(e *Engine, proxy string) string {
	state := "gAAAAA" + strings.Repeat("a", 286)
	a := e.config.Accounts[0]
	e.tickets[keyFor(7, "gpt-test")] = &ticket{AccountID: 7, Model: "gpt-test", Plan: "pro", State: state, Version: "test-version", ConfigFingerprint: configFingerprint(e.config, a, "gpt-test"), FixedFingerprint: proxyFingerprint(proxy), IdentityFingerprint: stableHeaders(7, nil), CapturedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(20 * time.Minute)}
	return state
}

func TestForwardPreservesHeadersAndRawResponse(t *testing.T) {
	requestBody := []byte("arbitrary non-JSON passthrough body")
	responseBody := []byte{0x1f, 0x8b, 0x08, 0x00, 0xff, 0x09}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		if !bytes.Equal(body, requestBody) || r.ContentLength != int64(len(requestBody)) {
			t.Error("body or length changed")
		}
		if len(r.Header.Values("X-Repeat")) != 2 {
			t.Error("request headers collapsed")
		}
		if r.Header.Get(StateHeader) != "caller-value" {
			t.Error("off mode altered client state")
		}
		w.Header().Add("Set-Cookie", "first=a")
		w.Header().Add("Set-Cookie", "second=b")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(responseBody)
	}))
	defer server.Close()
	start := mockStart(server.URL, true, int64(len(requestBody)))
	start.Headers["X-Repeat"] = &pluginv1.HeaderValues{Values: []string{"one", "two"}}
	start.Headers[StateHeader] = &pluginv1.HeaderValues{Values: []string{"caller-value"}}
	s := fixedStream(start, requestBody)
	e := forwardingEngine(t, false)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	if len(frames) < 3 || frames[0].GetStart().StatusCode != 202 {
		t.Fatalf("unexpected response: %v", frames)
	}
	if got := frames[0].GetStart().Headers["Set-Cookie"]; got == nil || len(got.Values) != 2 {
		t.Fatal("repeated response header lost")
	}
	var got []byte
	for _, f := range frames {
		got = append(got, f.GetBodyChunk()...)
	}
	if !bytes.Equal(got, responseBody) {
		t.Fatalf("response bytes changed: %x", got)
	}
	if frames[len(frames)-1].GetEnd().BytesReceived != int64(len(responseBody)) {
		t.Fatal("incorrect byte count")
	}
}

func TestForwardRewritesRequestEnvironmentAndLanguage(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	expectedDate := time.Now().In(loc).Format("2006-01-02")
	requestBody := []byte(`{"input":[{"content":"<environment_context><current_date>2000-01-01</current_date><timezone>America/Los_Angeles</timezone></environment_context>"},{"type":"user_location","timezone":"America/Los_Angeles"}]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Errorf("read: %v", readErr)
		}
		text := string(body)
		if r.ContentLength != int64(len(body)) {
			t.Errorf("content length = %d, want %d", r.ContentLength, len(body))
		}
		if !strings.Contains(text, "<current_date>"+expectedDate+"</current_date>") ||
			!strings.Contains(text, "<timezone>Asia/Tokyo</timezone>") ||
			!strings.Contains(text, `"timezone":"Asia/Tokyo"`) {
			t.Errorf("request environment was not rewritten: %s", text)
		}
		if got := r.Header.Values("Accept-Language"); len(got) != 1 || got[0] != defaultAcceptLanguage {
			t.Errorf("Accept-Language = %v", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	start := mockStart(server.URL, true, int64(len(requestBody)))
	start.Headers["Accept-Language"] = &pluginv1.HeaderValues{Values: []string{"zh-CN"}}
	s := fixedStream(start, requestBody)
	e := forwardingEngine(t, false)
	e.config.RequestRewriteEnabled = true
	e.config.DefaultRequestTimezone = defaultRequestTimezone
	e.config.Accounts[0].RequestTimezone = "Asia/Tokyo"
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	if len(frames) == 0 || frames[0].GetStart().StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected response: %v", frames)
	}
}

func TestForwardStreamsRequestAndResponseBeforeCompletion(t *testing.T) {
	firstSeen := make(chan struct{})
	releaseResponse := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 5)
		if _, err := io.ReadFull(r.Body, b); err != nil {
			t.Errorf("first body: %v", err)
			return
		}
		close(firstSeen)
		if string(b) != "first" {
			t.Error("wrong first chunk")
		}
		rest, _ := io.ReadAll(r.Body)
		if string(rest) != "second" {
			t.Error("wrong second chunk")
		}
		_, _ = io.WriteString(w, "chunk-one")
		w.(http.Flusher).Flush()
		<-releaseResponse
		_, _ = io.WriteString(w, "chunk-two")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seenResponse := make(chan struct{}, 1)
	s := &mockForwardStream{ctx: ctx, in: make(chan *pluginv1.ForwardRequest, 4), onSend: func(f *pluginv1.ForwardResponse) {
		if len(f.GetBodyChunk()) > 0 {
			select {
			case seenResponse <- struct{}{}:
			default:
			}
		}
	}}
	s.in <- frameStart(mockStart(server.URL, true, -1))
	s.in <- frameBody([]byte("first"))
	e := forwardingEngine(t, false)
	done := make(chan error, 1)
	go func() { done <- e.Forward(s) }()
	select {
	case <-firstSeen:
	case <-ctx.Done():
		close(releaseResponse)
		t.Fatal("request was buffered")
	}
	s.in <- frameBody([]byte("second"))
	s.in <- frameEnd()
	close(s.in)
	select {
	case <-seenResponse:
	case <-ctx.Done():
		close(releaseResponse)
		t.Fatal("response was buffered")
	}
	close(releaseResponse)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var response []byte
	for _, f := range s.frames() {
		response = append(response, f.GetBodyChunk()...)
	}
	if string(response) != "chunk-onechunk-two" {
		t.Fatalf("wrong output %q", response)
	}
}

func TestConfiguredAccountFailsOverWithoutTicket(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer server.Close()
	for _, body := range [][]byte{[]byte(`{"model":"gpt-test"}`), []byte("invalid json"), bytes.Repeat([]byte{'x'}, maxTicketRequestBody+1)} {
		e := forwardingEngine(t, true)
		s := fixedStream(mockStart(server.URL, true, int64(len(body))), body)
		if err := e.Forward(s); err != nil {
			t.Fatal(err)
		}
		frames := s.frames()
		if len(frames) != 1 || frames[0].GetError() == nil || frames[0].GetError().RequestSent || frames[0].GetError().Code != "state_ticket_unavailable" {
			t.Fatal("ticket miss must be safe to fail over")
		}
	}
	if hits.Load() != 0 {
		t.Fatal("configured request reached upstream without verified ticket")
	}
}

func TestForwardOnlyProtectsConfiguredModelsAndAccounts(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get(StateHeader) != "" {
			t.Error("unexpected injected state")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	tests := []struct {
		account     int64
		model       string
		accountType string
	}{{8, "", "oauth"}, {7, "gpt-other", "oauth"}, {7, "", "apikey"}}
	for _, tc := range tests {
		body := []byte(fmt.Sprintf(`{"model":%q}`, tc.model))
		start := mockStart(server.URL, true, int64(len(body)))
		start.AccountId = tc.account
		start.AccountType = tc.accountType
		e := forwardingEngine(t, true)
		if err := e.Forward(fixedStream(start, body)); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 3 {
		t.Fatal("normal account/model forwarding was gated")
	}
}

func TestEnabledAccountUnlistedModelAcceptsLargeBody(t *testing.T) {
	body := []byte(`{"model":"gpt-other","input":"` + strings.Repeat("a", 2<<20) + `"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, body) {
			t.Error("large unlisted-model request was altered")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	e := forwardingEngine(t, true)
	s := fixedStream(mockStart(server.URL, true, int64(len(body))), body)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	if frames := s.frames(); frames[0].GetStart().StatusCode != 200 {
		t.Fatal("large unlisted model was gated")
	}
}

func TestForwardInjectsTicketAndInvalidatesOnModelMismatch(t *testing.T) {
	e := forwardingEngine(t, true)
	state := addForwardTicket(e, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		values := r.Header.Values(StateHeader)
		if len(values) != 1 || values[0] != state {
			t.Error("ticket replacement failed")
		}
		if values := r.Header.Values("Cookie"); len(values) != 1 || values[0] != "caller=legacy" {
			t.Error("legacy ticket mode did not preserve caller cookies")
		}
		if values := r.Header.Values("session_id"); len(values) != 1 || values[0] != "caller-session" {
			t.Error("legacy ticket mode did not preserve caller session_id")
		}
		if values := r.Header.Values("Accept-Encoding"); len(values) != 1 || values[0] != "identity" {
			t.Error("ticket response did not negotiate inspectable uncompressed bytes")
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-other\"}}\n\n")
	}))
	defer server.Close()
	body := []byte(`{"model":"gpt-test"}`)
	start := mockStart(server.URL, true, int64(len(body)))
	start.Headers[StateHeader] = &pluginv1.HeaderValues{Values: []string{"old"}}
	start.Headers["X-Codex-Turn-State"] = &pluginv1.HeaderValues{Values: []string{"other-old"}}
	start.Headers["Cookie"] = &pluginv1.HeaderValues{Values: []string{"caller=legacy"}}
	start.Headers["session_id"] = &pluginv1.HeaderValues{Values: []string{"caller-session"}}
	start.Headers["Accept-Encoding"] = &pluginv1.HeaderValues{Values: []string{"gzip"}}
	start.Headers["accept-encoding"] = &pluginv1.HeaderValues{Values: []string{"br"}}
	if err := e.Forward(fixedStream(start, body)); err != nil {
		t.Fatal(err)
	}
	record := e.records[keyFor(7, "gpt-test")]
	if e.tickets[keyFor(7, "gpt-test")] != nil || record == nil || record.LastError != "model_mismatch" {
		t.Fatal("mismatched completion did not invalidate ticket")
	}
}

func TestForwardUsesAccountStickyProxyFromTicket(t *testing.T) {
	var directHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHits.Add(1)
		http.Error(w, "direct target must not be used", http.StatusBadGateway)
	}))
	defer target.Close()
	var stickyHits atomic.Int32
	sticky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stickyHits.Add(1)
		_, _ = io.WriteString(w, "sticky business")
	}))
	defer sticky.Close()
	stickyURL := "http://sticky-user:sticky-secret@" + strings.TrimPrefix(sticky.URL, "http://")

	e := forwardingEngine(t, true)
	e.config.Accounts[0].EgressMode = egressModePlugin
	e.config.Accounts[0].StickyProxyURL = stickyURL
	addForwardTicket(e, stickyURL)
	body := []byte(`{"model":"gpt-test"}`)
	start := mockStart(target.URL, true, int64(len(body)))
	start.ProxyUrl = "http://127.0.0.1:1"
	s := fixedStream(start, body)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	if directHits.Load() != 0 || stickyHits.Load() != 1 {
		t.Fatalf("business egress direct=%d sticky=%d", directHits.Load(), stickyHits.Load())
	}
	var got []byte
	for _, frame := range s.frames() {
		got = append(got, frame.GetBodyChunk()...)
	}
	if string(got) != "sticky business" {
		t.Fatalf("unexpected body %q", got)
	}
}

func TestForwardInvalidatesCompletedMismatchBeforeEOF(t *testing.T) {
	e := forwardingEngine(t, true)
	addForwardTicket(e, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-other\"}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body := []byte(`{"model":"gpt-test"}`)
	s := fixedStream(mockStart(server.URL, true, int64(len(body))), body)
	s.ctx = ctx
	s.onSend = func(f *pluginv1.ForwardResponse) {
		if len(f.GetBodyChunk()) > 0 {
			cancel()
		}
	}
	_ = e.Forward(s)
	record := e.records[keyFor(7, "gpt-test")]
	if e.tickets[keyFor(7, "gpt-test")] != nil || record == nil || record.LastError != "model_mismatch" {
		t.Fatal("completed mismatch was missed when consumer stopped before EOF")
	}
}

func TestForwardInvalidatesState312ButNotErrorResponses(t *testing.T) {
	for _, status := range []int{200, 401} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			e := forwardingEngine(t, true)
			addForwardTicket(e, "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set(StateHeader, "gAAAAA"+strings.Repeat("b", 306))
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "{}")
			}))
			defer server.Close()
			body := []byte(`{"model":"gpt-test"}`)
			if err := e.Forward(fixedStream(mockStart(server.URL, true, int64(len(body))), body)); err != nil {
				t.Fatal(err)
			}
			_, exists := e.tickets[keyFor(7, "gpt-test")]
			if exists != (status == 401) {
				t.Fatalf("incorrect invalidation at status%d", status)
			}
		})
	}
}

func TestForwardUpstreamReadErrorIsSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\nshort")
	}))
	defer server.Close()
	e := forwardingEngine(t, false)
	s := fixedStream(mockStart(server.URL+"?secret=do-not-echo", false, 0), nil)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	last := frames[len(frames)-1].GetError()
	if last == nil || !last.RequestSent || last.Code != "upstream_read" || strings.Contains(last.Message, "secret") {
		t.Fatalf("incorrect read error: %v", last)
	}
}

func TestForwardCancellationStopsTransport(t *testing.T) {
	entered := make(chan struct{})
	exited := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(exited) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := fixedStream(mockStart(server.URL, false, 0), nil)
	s.ctx = ctx
	e := forwardingEngine(t, false)
	done := make(chan error, 1)
	go func() { done <- e.Forward(s) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Forward ignored cancellation")
	}
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream context was not canceled")
	}
}

func TestForwardShutdownCancelsStalledConfiguredBody(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := forwardingEngine(t, true)
	e.ctx = ctx
	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	s := &mockForwardStream{ctx: streamCtx, in: make(chan *pluginv1.ForwardRequest, 1)}
	s.in <- frameStart(mockStart("http://127.0.0.1:1", true, -1))
	done := make(chan error, 1)
	go func() { done <- e.Forward(s) }()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("engine shutdown ignored while receiving body")
	}
}

func TestForwardDoesNotFollowRedirectOrDisableTLSVerification(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetHits.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	e := forwardingEngine(t, false)
	s := fixedStream(mockStart(redirect.URL, false, 0), nil)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	if targetHits.Load() != 0 || s.frames()[0].GetStart().StatusCode != 307 {
		t.Fatal("business request followed redirect")
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("self-signed TLS accepted") }))
	defer tlsServer.Close()
	s = fixedStream(mockStart(tlsServer.URL, false, 0), nil)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	if len(frames) != 1 || frames[0].GetError() == nil || frames[0].GetError().RequestSent || frames[0].GetError().Code != "upstream_transport_tls" {
		t.Fatal("invalid TLS was not rejected")
	}
}

func TestForwardProxyConnectFailureCanFailOverSafely(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddress := listener.Addr().String()
	_ = listener.Close()

	start := mockStart("http://127.0.0.1:1", false, 0)
	start.ProxyUrl = "http://" + proxyAddress
	s := fixedStream(start, nil)
	e := forwardingEngine(t, false)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	if len(frames) != 1 {
		t.Fatal("unexpected frames")
	}
	failure := frames[0].GetError()
	if failure == nil || failure.RequestSent || failure.Code != "upstream_transport_connect" {
		t.Fatalf("proxy connect failure was not marked safe: %v", failure)
	}
}

func TestForwardInvalidProxyIsNotSentAndDoesNotLeak(t *testing.T) {
	start := mockStart("http://127.0.0.1:1", false, 0)
	start.ProxyUrl = "bogus://user:private-secret@example.test"
	s := fixedStream(start, nil)
	e := forwardingEngine(t, false)
	if err := e.Forward(s); err != nil {
		t.Fatal(err)
	}
	frames := s.frames()
	if len(frames) != 1 {
		t.Fatal("unexpected frames")
	}
	failure := frames[0].GetError()
	if failure == nil || failure.RequestSent || strings.Contains(failure.Message, "private-secret") {
		t.Fatal("invalid proxy error leaks or claims sent")
	}
}

func TestClientPoolBoundedAndProbeClientsAreFresh(t *testing.T) {
	p := newClientPool()
	defer p.Close()
	first, err := p.client("", "")
	if err != nil {
		t.Fatal(err)
	}
	second, _ := p.client("", "")
	if first != second {
		t.Fatal("client not reused")
	}
	for i := 0; i < maxPooledClients+20; i++ {
		if _, err := p.client(fmt.Sprintf("http://user:pass@proxy-%d.invalid:80", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.clients) != maxPooledClients {
		t.Fatal("pool is not bounded")
	}
	for _, scheme := range []string{"http", "https", "socks5", "socks5h"} {
		client, err := freshProbeClient(scheme+"://localhost:1234", "")
		if err != nil {
			t.Fatal(err)
		}
		transport := client.Transport.(*http.Transport)
		if !transport.DisableKeepAlives || !transport.DisableCompression || transport.TLSClientConfig != nil && transport.TLSClientConfig.InsecureSkipVerify {
			t.Fatal("unsafe probe transport")
		}
		if err := client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
			t.Fatal("redirect enabled")
		}
		if transport.MaxConnsPerHost != 0 {
			t.Fatal("transport introduced an account concurrency cap")
		}
	}
}

func TestNestedHTTPProxyChain(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("target server must not be reached directly")
	}))
	defer target.Close()

	var secondProxyHits atomic.Int32
	secondProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondProxyHits.Add(1)
		if !r.URL.IsAbs() {
			t.Error("dynamic proxy did not receive an absolute request URL")
		}
		_, _ = io.WriteString(w, "nested-ok")
	}))
	defer secondProxy.Close()

	firstProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("first proxy method = %s", r.Method)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
			return
		}
		client, _, err := hijacker.Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, client)
			close(done)
		}()
		_, _ = io.Copy(client, upstream)
		<-done
	}))
	defer firstProxy.Close()

	client, err := makeHTTPClient(secondProxy.URL, firstProxy.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "nested-ok" || secondProxyHits.Load() != 1 {
		t.Fatalf("nested HTTP chain failed: status=%d body=%q hits=%d", response.StatusCode, body, secondProxyHits.Load())
	}

	directTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "first-hop-only")
	}))
	defer directTarget.Close()
	firstHopClient, err := makeHTTPClient("", firstProxy.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer firstHopClient.CloseIdleConnections()
	response, err = firstHopClient.Get(directTarget.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err = io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "first-hop-only" {
		t.Fatalf("first-hop-only transport failed: status=%d body=%q", response.StatusCode, body)
	}
}

func TestNestedSOCKS5AuthenticatedFirstProxy(t *testing.T) {
	var secondProxyHits atomic.Int32
	secondProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondProxyHits.Add(1)
		_, _ = io.WriteString(w, "socks-nested-ok")
	}))
	defer secondProxy.Close()

	socksURL := startTestSOCKS5Proxy(t, "first-user", "first-pass")
	client, err := makeHTTPClient(secondProxy.URL, socksURL, true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get("http://example.test/through-chain")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "socks-nested-ok" || secondProxyHits.Load() != 1 {
		t.Fatalf("nested SOCKS5 chain failed: status=%d body=%q hits=%d", response.StatusCode, body, secondProxyHits.Load())
	}
}

func TestNestedSOCKS5ProxyChain(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "socks-chain-ok")
	}))
	defer target.Close()

	firstProxy := startTestSOCKS5Proxy(t, "first-user", "first-pass")
	secondProxy := startTestSOCKS5Proxy(t, "second-user", "second-pass")
	client, err := makeHTTPClient(secondProxy, firstProxy, true)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	response, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "socks-chain-ok" {
		t.Fatalf("nested SOCKS5 chain failed: status=%d body=%q", response.StatusCode, body)
	}
}

func startTestSOCKS5Proxy(t *testing.T, username, password string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveTestSOCKS5(conn, username, password)
		}
	}()
	return "socks5://" + username + ":" + password + "@" + listener.Addr().String()
}

func serveTestSOCKS5(conn net.Conn, username, password string) {
	defer conn.Close()
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 2}); err != nil {
		return
	}
	authHeader := make([]byte, 2)
	if _, err := io.ReadFull(conn, authHeader); err != nil || authHeader[0] != 1 {
		return
	}
	user := make([]byte, int(authHeader[1]))
	if _, err := io.ReadFull(conn, user); err != nil {
		return
	}
	passwordLength := make([]byte, 1)
	if _, err := io.ReadFull(conn, passwordLength); err != nil {
		return
	}
	pass := make([]byte, int(passwordLength[0]))
	if _, err := io.ReadFull(conn, pass); err != nil {
		return
	}
	if string(user) != username || string(pass) != password {
		_, _ = conn.Write([]byte{1, 1})
		return
	}
	if _, err := conn.Write([]byte{1, 0}); err != nil {
		return
	}
	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil || request[0] != 5 || request[1] != 1 {
		return
	}
	var host string
	switch request[3] {
	case 1:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, address); err != nil {
			return
		}
		host = net.IP(address).String()
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return
		}
		address := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, address); err != nil {
			return
		}
		host = string(address)
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return
	}
	upstream, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(portBytes))))
	if err != nil {
		_, _ = conn.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, conn)
		close(done)
	}()
	_, _ = io.Copy(conn, upstream)
	<-done
}
