package engine

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRewriteRequestTimezoneDateAndUserLocation(t *testing.T) {
	body := []byte(`{"input":[{"content":"<environment_context><current_date>2026-01-01</current_date><timezone>America/Los_Angeles</timezone></environment_context>"},{"type":"user_location","timezone":"America/Los_Angeles"}]}`)
	now := time.Date(2025, 12, 31, 18, 0, 0, 0, time.UTC)
	rewritten, changed := rewriteRequestData(body, "Asia/Singapore", now)
	if !changed {
		t.Fatal("request rewrite did not report a change")
	}
	text := string(rewritten)
	if !strings.Contains(text, "<current_date>2026-01-01</current_date>") ||
		!strings.Contains(text, "<timezone>Asia/Singapore</timezone>") {
		t.Fatalf("environment context was not rewritten: %s", text)
	}
	if strings.Count(text, `"timezone":"Asia/Singapore"`) != 1 {
		t.Fatalf("user_location timezone was not rewritten: %s", text)
	}
}

func TestRewriteAcceptLanguageOnlyChangesExistingHeader(t *testing.T) {
	headers := http.Header{}
	rewriteAcceptLanguage(headers)
	if len(headers.Values("Accept-Language")) != 0 {
		t.Fatal("missing language header was created")
	}
	headers.Set("Accept-Language", "zh-CN")
	headers["accept-language"] = []string{"zh-TW"}
	rewriteAcceptLanguage(headers)
	if got := headers.Get("Accept-Language"); got != defaultAcceptLanguage {
		t.Fatalf("Accept-Language = %q", got)
	}
	if values := headers.Values("accept-language"); len(values) != 1 || values[0] != defaultAcceptLanguage {
		t.Fatalf("mixed-case Accept-Language = %v", values)
	}
}

func TestRestrictedRequestTimezoneIsRejected(t *testing.T) {
	for _, timezone := range []string{"Asia/Hong_Kong", "Asia/Macau", "Asia/Taipei", "Asia/Shanghai"} {
		if err := validateRequestTimezone(timezone); err == nil {
			t.Fatalf("restricted timezone %q was accepted", timezone)
		}
	}
	if err := validateRequestTimezone("Asia/Singapore"); err != nil {
		t.Fatal(err)
	}
}
