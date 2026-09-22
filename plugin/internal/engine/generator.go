package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxProxyGeneratorResponse = 64 << 10

type egressInfo struct {
	IP      string
	Country string
}

// generateProxy asks the configured provider endpoint for one sticky proxy
// endpoint. The request itself goes through the configured first-hop proxy.
func (e *Engine) generateProxy(ctx context.Context, c Config) (string, error) {
	if c.ProxyGeneratorURL == "" {
		return "", errors.New("proxy generator URL is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ProxyGeneratorURL, nil)
	if err != nil {
		return "", errors.New("proxy generator request is invalid")
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "sub2api-state-kit-generator/1")
	client, err := freshProbeClient("", c.UpstreamProxyURL)
	if err != nil {
		return "", errors.New("proxy generator transport is unavailable")
	}
	defer client.CloseIdleConnections()
	response, err := client.Do(req)
	if err != nil {
		return "", errors.New("proxy generator request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("proxy generator rejected the request")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProxyGeneratorResponse+1))
	if err != nil || len(body) > maxProxyGeneratorResponse {
		return "", errors.New("proxy generator response is invalid")
	}
	return parseGeneratedProxy(body)
}

func parseGeneratedProxy(body []byte) (string, error) {
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if !strings.Contains(line, "://") {
			line = "http://" + line
		}
		u, err := parseProxyURL(line)
		if err != nil || u.User != nil || u.Port() == "" {
			continue
		}
		return u.String(), nil
	}
	return "", errors.New("proxy generator returned no usable host:port")
}

// lookupEgressInfo resolves the current IP and country through one generated
// proxy. Keeping both values in one request avoids an extra public-IP round
// trip during ticket renewal.
func (e *Engine) lookupEgressInfo(ctx context.Context, proxyURL, upstreamProxyURL string) egressInfo {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for _, template := range e.geoURLs {
		endpoint := strings.ReplaceAll(template, "{ip}", "")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("User-Agent", "sub2api-state-kit-geo/2")
		client, err := freshProbeClient(proxyURL, upstreamProxyURL)
		if err != nil {
			continue
		}
		response, err := client.Do(req)
		if err != nil {
			client.CloseIdleConnections()
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<10))
		response.Body.Close()
		client.CloseIdleConnections()
		if readErr != nil || response.StatusCode != http.StatusOK {
			continue
		}
		if info := parseEgressInfo(body, ""); info.IP != "" && info.Country != "" {
			return info
		}
	}
	return egressInfo{}
}

// lookupEgressCountry resolves the country of the generated egress IP through
// the generated proxy. Unknown results are rejected by the caller.
func (e *Engine) lookupEgressCountry(ctx context.Context, proxyURL, upstreamProxyURL, ip string) string {
	if ip == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for _, template := range e.geoURLs {
		endpoint := strings.ReplaceAll(template, "{ip}", url.PathEscape(ip))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Accept-Encoding", "identity")
		req.Header.Set("User-Agent", "sub2api-state-kit-geo/1")
		client, err := freshProbeClient(proxyURL, upstreamProxyURL)
		if err != nil {
			continue
		}
		response, err := client.Do(req)
		if err != nil {
			client.CloseIdleConnections()
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 32<<10))
		response.Body.Close()
		client.CloseIdleConnections()
		if readErr != nil || response.StatusCode != http.StatusOK {
			continue
		}
		if country := parseEgressCountry(body, ip); country != "" {
			return country
		}
	}
	return ""
}

func parseEgressInfo(body []byte, expectedIP string) egressInfo {
	var ipAPI struct {
		Status      string `json:"status"`
		CountryCode string `json:"countryCode"`
		Query       string `json:"query"`
	}
	if json.Unmarshal(body, &ipAPI) == nil && ipAPI.Status == "success" &&
		(expectedIP == "" || ipAPI.Query == "" || ipAPI.Query == expectedIP) {
		return egressInfo{IP: safeEgressIP(ipAPI.Query), Country: normalizeCountryCode(ipAPI.CountryCode)}
	}
	var ipWho struct {
		Success     bool   `json:"success"`
		CountryCode string `json:"country_code"`
		IP          string `json:"ip"`
	}
	if json.Unmarshal(body, &ipWho) == nil && ipWho.Success &&
		(expectedIP == "" || ipWho.IP == "" || ipWho.IP == expectedIP) {
		return egressInfo{IP: safeEgressIP(ipWho.IP), Country: normalizeCountryCode(ipWho.CountryCode)}
	}
	return egressInfo{}
}

func parseEgressCountry(body []byte, expectedIP string) string {
	return parseEgressInfo(body, expectedIP).Country
}

func normalizeCountryCode(value string) string {
	code := strings.ToUpper(strings.TrimSpace(value))
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	return code
}

func countryBlocked(country string, blocked []string) bool {
	country = normalizeCountryCode(country)
	if country == "" {
		return true
	}
	for _, value := range blocked {
		if strings.EqualFold(strings.TrimSpace(value), country) {
			return true
		}
	}
	return false
}
