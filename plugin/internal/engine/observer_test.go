package engine

import (
	"strings"
	"testing"
)

func TestCompletionObserverRequiresSuccessfulActualModel(t *testing.T) {
	tests := []struct {
		name, body        string
		complete, matches bool
	}{
		{"completed SSE", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-test\"}}\n\n", true, true},
		{"wrong actual model", `data: {"type":"response.completed","response":{"status":"completed","model":"gpt-other"}}` + "\n\n", true, false},
		{"failed completion", `data: {"type":"response.completed","response":{"status":"failed","model":"gpt-test"}}` + "\n\n", false, false},
		{"completed with error", `data: {"type":"response.completed","response":{"status":"completed","model":"gpt-test","error":{"code":"failed"}}}` + "\n\n", false, false},
		{"missing actual model", `data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n", false, false},
		{"created is not complete", `data: {"type":"response.created","response":{"status":"completed","model":"gpt-test"}}` + "\n\n", false, false},
		{"plain JSON", `{"object":"response","status":"completed","model":"gpt-test"}`, true, true},
		{"incomplete JSON", `{"object":"response","status":"in_progress","model":"gpt-test"}`, false, false},
		{"snapshot must match exactly", `{"object":"response","status":"completed","model":"gpt-test-2026-09-01"}`, true, false},
		{"garbage", `data: [DONE]` + "\n\n", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for chunkSize := 1; chunkSize <= len(tc.body)+1; chunkSize += 13 {
				o := newCompletionObserver("gpt-test")
				for pos := 0; pos < len(tc.body); pos += chunkSize {
					end := pos + chunkSize
					if end > len(tc.body) {
						end = len(tc.body)
					}
					o.Write([]byte(tc.body[pos:end]))
				}
				o.Finish()
				complete, matches := o.Result()
				if complete != tc.complete || matches != tc.matches {
					t.Fatalf("chunks %d: got (%v,%v), want (%v,%v)", chunkSize, complete, matches, tc.complete, tc.matches)
				}
			}
		})
	}
}

func TestCompletionObserverOversizeIsBoundedAndRecoversAtNextEvent(t *testing.T) {
	o := newCompletionObserver("gpt-test")
	o.Write([]byte("data: " + strings.Repeat("x", maxObservedFrame+100) + "\n\n"))
	if len(o.body) > maxObservedFrame || len(o.line) > maxObservedFrame || len(o.event) > maxObservedFrame {
		t.Fatal("unbounded observer buffer")
	}
	o.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-test\"}}\r\n\r\n"))
	o.Finish()
	if complete, matches := o.Result(); !complete || !matches {
		t.Fatal("did not recover after ignored oversized event")
	}
}

func TestCompletionObserverMismatchCannotBeHiddenByLaterMatch(t *testing.T) {
	o := newCompletionObserver("gpt-test")
	for _, model := range []string{"gpt-other", "gpt-test"} {
		o.Write([]byte(`data: {"type":"response.completed","response":{"model":"` + model + `"}}` + "\n\n"))
	}
	o.Finish()
	if complete, matches := o.Result(); !complete || matches {
		t.Fatal("later match hid a mismatch")
	}
}

func TestCompletionObserverCapturesFinalOutputOnce(t *testing.T) {
	body := "event: response.output_text.done\n" +
		"data: {\"type\":\"response.output_text.done\",\"item_id\":\"msg-1\",\"text\":\"iPhone 17\"}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"model\":\"gpt-test\"}}\n\n"
	for chunkSize := 1; chunkSize <= len(body)+1; chunkSize += 7 {
		o := newCompletionObserver("gpt-test")
		for pos := 0; pos < len(body); pos += chunkSize {
			end := pos + chunkSize
			if end > len(body) {
				end = len(body)
			}
			o.Write([]byte(body[pos:end]))
		}
		o.Finish()
		if got := o.OutputText(); got != "iPhone 17" {
			t.Fatalf("chunks %d: output = %q", chunkSize, got)
		}
	}
}

func TestQualityAnswerMatchesConfiguredValues(t *testing.T) {
	for _, tc := range []struct {
		name, output, accepted string
		want                   bool
	}{
		{"case insensitive", "iPhone 17 Pro", "17", true},
		{"alternate accepted value", "iPhone 17 Pro", "16,iphone 17", true},
		{"missing", "iPhone 16 Pro", "17", false},
		{"empty output", "", "17", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := qualityAnswerMatches(tc.output, tc.accepted); got != tc.want {
				t.Fatalf("qualityAnswerMatches(%q,%q) = %v; want %v", tc.output, tc.accepted, got, tc.want)
			}
		})
	}
}
