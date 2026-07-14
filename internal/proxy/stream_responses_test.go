package proxy

import (
	"strings"
	"testing"
)

func TestResponsesStreamTransformerTypeFieldFallback(t *testing.T) {
	// Gateways that omit SSE "event:" lines put the type in JSON only.
	st := &ResponsesStreamTransformer{}
	events, err := st.TranslateBlock(`data: {"type":"response.output_text.delta","delta":"Hi"}`)
	if err != nil {
		t.Fatalf("TranslateBlock: %v", err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, `"type":"message_start"`) {
		t.Fatalf("expected message_start, got %v", events)
	}
	if !strings.Contains(joined, `"text":"Hi"`) {
		t.Fatalf("expected text delta, got %v", events)
	}

	events, err = st.TranslateBlock(`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}`)
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	joined = strings.Join(events, "\n")
	if !strings.Contains(joined, `"type":"message_stop"`) {
		t.Fatalf("expected message_stop, got %v", events)
	}
	if !strings.Contains(joined, `"input_tokens":1`) {
		t.Fatalf("expected usage, got %v", events)
	}
}

func TestResponsesStreamTransformerReasoningSummaryDelta(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	events, err := st.TranslateBlock(`event: response.reasoning_summary_text.delta
data: {"delta":"thinking..."}`)
	if err != nil {
		t.Fatalf("TranslateBlock: %v", err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, `"type":"thinking"`) && !strings.Contains(joined, `"thinking":"thinking..."`) {
		t.Fatalf("expected thinking delta, got %v", events)
	}
}

func TestResponsesStreamTransformerDoneWithoutDeltas(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	events, err := st.TranslateBlock(`event: response.output_item.done
data: {"output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"full text"}]}}`)
	if err != nil {
		t.Fatalf("TranslateBlock: %v", err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, `"text":"full text"`) {
		t.Fatalf("expected full text from done item, got %v", events)
	}
}

func TestResponsesStreamTransformerCompletedOutputFallback(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	events, err := st.TranslateBlock(`event: response.completed
data: {"response":{"id":"resp_1","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compact summary"}]}],"usage":{"input_tokens":100,"output_tokens":12}}}`)
	if err != nil {
		t.Fatalf("TranslateBlock: %v", err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, `"text":"compact summary"`) {
		t.Fatalf("expected text from completed response output, got %v", events)
	}
	if !strings.Contains(joined, `"type":"message_stop"`) {
		t.Fatalf("expected completed stream to stop, got %v", events)
	}
}

func TestResponsesStreamTransformerOutputTextDoneFallback(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	events, err := st.TranslateBlock(`event: response.output_text.done
data: {"output_index":0,"content_index":0,"text":"compact summary"}`)
	if err != nil {
		t.Fatalf("TranslateBlock: %v", err)
	}
	if joined := strings.Join(events, "\n"); !strings.Contains(joined, `"text":"compact summary"`) {
		t.Fatalf("expected text from output_text.done, got %v", events)
	}
}

func TestResponsesStreamTransformerCompletedDoesNotRepeatDoneText(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	if _, err := st.TranslateBlock(`event: response.output_item.done
data: {"output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compact summary"}]}}`); err != nil {
		t.Fatalf("done: %v", err)
	}
	events, err := st.TranslateBlock(`event: response.completed
data: {"response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compact summary"}]}]}}`)
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if joined := strings.Join(events, "\n"); strings.Contains(joined, `"text":"compact summary"`) {
		t.Fatalf("completed response repeated text already emitted by done item: %v", events)
	}
}

func TestResponsesStreamTransformerCompletedWithoutOutputIsError(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	events, err := st.TranslateBlock(`event: response.completed
data: {"response":{"id":"resp_empty","status":"completed","output":[]}}`)
	if err != nil {
		t.Fatalf("TranslateBlock: %v", err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, `event: error`) ||
		!strings.Contains(joined, `completed without assistant output`) {
		t.Fatalf("empty completed response should surface an error, got %v", events)
	}
	if strings.Contains(joined, `"type":"message_stop"`) {
		t.Fatalf("empty completed response must not become an empty success: %v", events)
	}
}

func TestResponsesStreamTransformerDoesNotRepeatDoneTextAfterDeltas(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	for _, block := range []string{
		`event: response.output_text.delta
data: {"output_index":0,"delta":"Hi again! "}`,
		`event: response.output_text.delta
data: {"output_index":0,"delta":"What would you like to work on?"}`,
	} {
		if _, err := st.TranslateBlock(block); err != nil {
			t.Fatalf("delta: %v", err)
		}
	}

	events, err := st.TranslateBlock(`event: response.output_item.done
data: {"output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hi again! What would you like to work on?"}]}}`)
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	joined := strings.Join(events, "\n")
	if strings.Contains(joined, `"text":"Hi again! What would you like to work on?"`) {
		t.Fatalf("done item repeated text already emitted as deltas: %v", events)
	}
}

func TestResponsesStreamTransformerDoesNotRepeatDoneReasoningAfterDeltas(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	if _, err := st.TranslateBlock(`event: response.reasoning_summary_text.delta
data: {"output_index":0,"delta":"thinking..."}`); err != nil {
		t.Fatalf("delta: %v", err)
	}
	events, err := st.TranslateBlock(`event: response.output_item.done
data: {"output_index":0,"item":{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking..."}]}}`)
	if err != nil {
		t.Fatalf("done: %v", err)
	}
	if joined := strings.Join(events, "\n"); strings.Contains(joined, `"thinking":"thinking..."`) {
		t.Fatalf("done item repeated reasoning already emitted as deltas: %v", events)
	}
}

func TestResponsesStreamTransformerFailedSurfacesAnthropicError(t *testing.T) {
	st := &ResponsesStreamTransformer{}
	events, err := st.TranslateBlock(`event: response.failed
data: {"response":{"id":"resp_x","status":"failed","error":{"message":"upstream compact failed"}}}`)
	if err != nil {
		t.Fatalf("TranslateBlock: %v", err)
	}
	joined := strings.Join(events, "\n")
	if !strings.Contains(joined, `event: error`) || !strings.Contains(joined, `"message":"upstream compact failed"`) {
		t.Fatalf("failed stream should surface its error, got %v", events)
	}
	if strings.Contains(joined, `"type":"message_stop"`) {
		t.Fatalf("failed stream must not masquerade as a successful empty message: %v", events)
	}
	if tail := st.finish("end_turn"); len(tail) != 0 {
		t.Fatalf("failed stream should suppress EOF success fallback, got %v", tail)
	}
}
