package engine

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Completion inspection is deliberately independent of forwarding: even an
// oversized or malformed event is forwarded untouched, but cannot validate a
// ticket. Each individual buffer is bounded.
const maxObservedFrame = 1 << 20

type completionObserver struct {
	expected                                  string
	line, event, body                         []byte
	lineOverflow, eventOverflow, bodyOverflow bool
	complete, matches                         bool
	actualModel                               string
	outputText                                []byte
	outputOverflow                            bool
	outputTextKeys                            map[string]bool
}

func newCompletionObserver(expected string) *completionObserver {
	return &completionObserver{
		expected:       expected,
		matches:        true,
		outputTextKeys: map[string]bool{},
	}
}

func (o *completionObserver) Write(p []byte) {
	if !o.bodyOverflow {
		if len(o.body)+len(p) > maxObservedFrame {
			o.body, o.bodyOverflow = nil, true
		} else {
			o.body = append(o.body, p...)
		}
	}
	for _, b := range p {
		if b == '\n' {
			o.consumeLine()
			continue
		}
		if o.lineOverflow {
			continue
		}
		if len(o.line) >= maxObservedFrame {
			o.line, o.lineOverflow = nil, true
			continue
		}
		o.line = append(o.line, b)
	}
}

func (o *completionObserver) consumeLine() {
	if o.lineOverflow {
		o.eventOverflow = true
		o.lineOverflow = false
		o.line = o.line[:0]
		return
	}
	line := bytes.TrimSuffix(o.line, []byte{'\r'})
	if len(line) == 0 {
		o.consumeEvent()
	} else if bytes.HasPrefix(line, []byte("data:")) && !o.eventOverflow {
		data := line[5:]
		if len(data) > 0 && data[0] == ' ' {
			data = data[1:]
		}
		if len(o.event)+len(data)+1 > maxObservedFrame {
			o.event, o.eventOverflow = nil, true
		} else {
			o.event = append(o.event, data...)
			o.event = append(o.event, '\n')
		}
	}
	o.line = o.line[:0]
}

func (o *completionObserver) consumeEvent() {
	if !o.eventOverflow && len(o.event) > 0 {
		o.inspect(o.event)
	}
	o.event = o.event[:0]
	o.eventOverflow = false
}

func (o *completionObserver) Finish() {
	if len(o.line) > 0 || o.lineOverflow {
		o.consumeLine()
	}
	o.consumeEvent()
	if !o.bodyOverflow {
		o.inspect(o.body)
	}
}

func (o *completionObserver) Result() (complete bool, matches bool) {
	return o.complete, o.complete && o.matches
}

func (o *completionObserver) inspect(data []byte) {
	if len(data) == 0 || len(data) > maxObservedFrame {
		return
	}
	var value struct {
		Type     string          `json:"type"`
		Object   string          `json:"object"`
		Status   string          `json:"status"`
		Model    string          `json:"model"`
		ItemID   string          `json:"item_id"`
		Text     string          `json:"text"`
		Error    json.RawMessage `json:"error"`
		Response *struct {
			Status string          `json:"status"`
			Model  string          `json:"model"`
			Error  json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &value) != nil {
		return
	}
	if value.Type == "response.output_text.done" && value.Text != "" {
		key := value.ItemID
		if key == "" {
			key = value.Text
		}
		if !o.outputTextKeys[key] {
			o.outputTextKeys[key] = true
			if len(o.outputText)+len(value.Text) > maxObservedFrame {
				o.outputText = nil
				o.outputOverflow = true
			} else if !o.outputOverflow {
				o.outputText = append(o.outputText, value.Text...)
			}
		}
	}
	model := ""
	if value.Type == "response.completed" && value.Response != nil && noResponseError(value.Error) && noResponseError(value.Response.Error) &&
		(value.Response.Status == "completed" || value.Response.Status == "") {
		model = value.Response.Model
	} else if value.Object == "response" && value.Status == "completed" && noResponseError(value.Error) {
		model = value.Model
	}
	if strings.TrimSpace(model) == "" {
		return
	}
	o.complete = true
	if o.actualModel == "" {
		o.actualModel = model
	}
	o.matches = o.matches && model == o.expected
}

func (o *completionObserver) ActualModel() string {
	return o.actualModel
}

func (o *completionObserver) OutputText() string {
	if o.outputOverflow {
		return ""
	}
	return string(o.outputText)
}

func noResponseError(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
