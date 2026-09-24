package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	pluginv1 "github.com/zhang2580384/sub2api-state-kit/plugin/internal/pluginapi/v1"
)

var (
	timezoneTagPattern = regexp.MustCompile(`(?is)<timezone>.*?</timezone>`)
	dateTagPattern     = regexp.MustCompile(`(?is)<current_date>.*?</current_date>`)
	languageHeader     = http.CanonicalHeaderKey("Accept-Language")
)

func (e *Engine) requestRewriteSettings(start *pluginv1.ForwardRequestStart) (bool, string) {
	if start == nil || start.Platform != "openai" || start.AccountType != "oauth" {
		return false, ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.config.RequestRewriteEnabled {
		return false, ""
	}
	timezone := e.config.DefaultRequestTimezone
	if account, ok := findAccount(e.config, start.AccountId); ok && account.RequestTimezone != "" {
		timezone = account.RequestTimezone
	}
	if timezone == "" {
		timezone = defaultRequestTimezone
	}
	return true, timezone
}

func rewriteRequestData(data []byte, timezone string, now time.Time) ([]byte, bool) {
	if len(data) == 0 || timezone == "" {
		return data, false
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return data, false
	}
	var root any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if dec.Decode(&root) != nil {
		return data, false
	}
	changed := false
	root = rewriteJSONValue(root, loc, now, &changed)
	if !changed {
		return data, false
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(root) != nil {
		return data, false
	}
	return bytes.TrimSuffix(output.Bytes(), []byte{'\n'}), true
}

func rewriteJSONValue(value any, loc *time.Location, now time.Time, changed *bool) any {
	switch typed := value.(type) {
	case string:
		rewritten, ok := rewriteEnvironmentContext(typed, loc, now)
		if ok {
			*changed = true
			return rewritten
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = rewriteJSONValue(typed[i], loc, now, changed)
		}
		return typed
	case map[string]any:
		userLocation := false
		if kind, ok := typed["type"].(string); ok && kind == "user_location" {
			userLocation = true
		}
		for key, item := range typed {
			typed[key] = rewriteJSONValue(item, loc, now, changed)
			if userLocation && key == "timezone" {
				if _, ok := typed[key].(string); ok {
					typed[key] = loc.String()
					*changed = true
				}
			}
		}
		return typed
	default:
		return value
	}
}

func rewriteEnvironmentContext(value string, loc *time.Location, now time.Time) (string, bool) {
	if !strings.Contains(strings.ToLower(value), "<environment_context") {
		return value, false
	}
	changed := false
	if strings.Contains(strings.ToLower(value), "<timezone>") {
		value = timezoneTagPattern.ReplaceAllString(value, "<timezone>"+loc.String()+"</timezone>")
		changed = true
	}
	if strings.Contains(strings.ToLower(value), "<current_date>") {
		value = dateTagPattern.ReplaceAllString(value, "<current_date>"+now.In(loc).Format("2006-01-02")+"</current_date>")
		changed = true
	}
	return value, changed
}

func rewriteAcceptLanguage(headers http.Header) {
	found := false
	for key := range headers {
		if strings.EqualFold(key, languageHeader) {
			delete(headers, key)
			found = true
		}
	}
	if !found {
		return
	}
	headers.Set(languageHeader, defaultAcceptLanguage)
}
