package engine

import (
	"context"
	"encoding/json"
	"time"

	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
)

const previousEgressTTL = 24 * time.Hour

func validPreviousEgress(v previousEgress, accountID int64) bool {
	if v.AccountID != accountID || v.ProxyURL == "" || v.UpdatedAt.IsZero() {
		return false
	}
	u, err := parseProxyURL(v.ProxyURL)
	return err == nil && u.User == nil && u.Port() != ""
}

func (e *Engine) loadPreviousEgress(ctx context.Context, host pluginv1.HostServiceClient, accountID int64) (previousEgress, bool) {
	e.mu.Lock()
	cached, ok := e.previous[accountID]
	e.mu.Unlock()
	if ok && validPreviousEgress(cached, accountID) {
		return cached, true
	}
	if host == nil {
		return previousEgress{}, false
	}
	cc, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r, err := host.KVGet(cc, &pluginv1.KVGetRequest{Namespace: namespace, Key: previousEgressKey(accountID)})
	if err != nil || r == nil || !r.Found || len(r.Value) > 4096 {
		return previousEgress{}, false
	}
	var value previousEgress
	if json.Unmarshal(r.Value, &value) != nil || !validPreviousEgress(value, accountID) {
		return previousEgress{}, false
	}
	e.mu.Lock()
	e.previous[accountID] = value
	e.mu.Unlock()
	return value, true
}

func (e *Engine) preferredPreviousProxy(ctx context.Context, host pluginv1.HostServiceClient, accountID int64, key string) string {
	if value, ok := e.loadPreviousEgress(ctx, host, accountID); ok {
		return value.ProxyURL
	}
	e.mu.Lock()
	t := e.tickets[key]
	e.mu.Unlock()
	if t != nil && t.AccountID == accountID && t.GeneratedProxyURL != "" {
		u, err := parseProxyURL(t.GeneratedProxyURL)
		if err == nil && u.User == nil && u.Port() != "" {
			return t.GeneratedProxyURL
		}
	}
	return ""
}

func (e *Engine) rememberPreviousEgress(ctx context.Context, host pluginv1.HostServiceClient, accountID int64, proxyURL, egressIP string) {
	if host == nil {
		return
	}
	value := previousEgress{AccountID: accountID, ProxyURL: proxyURL, EgressIP: egressIP, UpdatedAt: time.Now().UTC()}
	if !validPreviousEgress(value, accountID) {
		return
	}
	cc, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := host.KVSet(cc, &pluginv1.KVSetRequest{
		Namespace: namespace, Key: previousEgressKey(accountID), Value: []byte(jsonText(value)),
		TtlSeconds: int64(previousEgressTTL.Seconds()),
	}); err != nil {
		return
	}
	e.mu.Lock()
	e.previous[accountID] = value
	e.mu.Unlock()
}
