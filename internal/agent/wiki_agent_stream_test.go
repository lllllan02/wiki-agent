package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

const testMaxElapsed = time.Minute

func TestStreamFromOpenAICompatibleModel(t *testing.T) {
	firstReceived := make(chan struct{})
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Stream {
			http.Error(w, "expected streaming request", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, part := range []string{"你好，", "世界"} {
			payload := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": part}}}}
			encoded, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
			w.(http.Flusher).Flush()
			if i == 0 {
				select {
				case <-firstReceived:
				case <-time.After(2 * time.Second):
					t.Error("first chunk buffered until completion")
				case <-r.Context().Done():
					return
				}
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer modelServer.Close()
	ctx := context.Background()
	model, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{APIKey: "fixture", Model: "fixture", BaseURL: modelServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{Name: "fixture", Model: model})
	if err != nil {
		t.Fatal(err)
	}
	a := &WikiAgent{agent: shared, maxElapsed: testMaxElapsed}
	var updates []string
	result, err := a.Run(ctx, RunRequest{Message: "你好", OnEvent: func(event StreamEvent) error {
		if event.Message != nil {
			updates = append(updates, event.Message.Content)
			if len(updates) == 1 {
				close(firstReceived)
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "你好，世界" || !reflect.DeepEqual(updates, []string{"你好，", "你好，世界"}) {
		t.Fatalf("updates=%v, result=%+v", updates, result)
	}
}

type streamFixtureAgent struct{}

func (streamFixtureAgent) Name(context.Context) string        { return "fixture" }
func (streamFixtureAgent) Description(context.Context) string { return "fixture" }
func (streamFixtureAgent) Run(_ context.Context, input *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		if !input.EnableStreaming {
			return
		}
		for _, chunks := range [][]string{{"准备", "检索"}, {"最", "终回答"}} {
			messages := make([]*schema.Message, 0, len(chunks))
			for _, chunk := range chunks {
				messages = append(messages, schema.AssistantMessage(chunk, nil))
			}
			generator.Send(&adk.AgentEvent{Output: &adk.AgentOutput{MessageOutput: &adk.MessageVariant{
				Role: schema.Assistant, IsStreaming: true, MessageStream: schema.StreamReaderFromArray(messages),
			}}})
		}
	}()
	return iter
}

func TestStreamReplacesEarlierAssistantTurn(t *testing.T) {
	a := &WikiAgent{agent: streamFixtureAgent{}, maxElapsed: testMaxElapsed}
	var updates []string
	result, err := a.Run(context.Background(), RunRequest{Message: "问题", OnEvent: func(event StreamEvent) error {
		if event.Message != nil {
			updates = append(updates, event.Message.Content)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"准备", "准备检索", "最", "最终回答"}
	if !reflect.DeepEqual(updates, want) || result.Answer != "最终回答" {
		t.Fatalf("updates=%v, result=%+v", updates, result)
	}
}

func TestStreamStopsWhenConsumerFails(t *testing.T) {
	a := &WikiAgent{agent: streamFixtureAgent{}, maxElapsed: testMaxElapsed}
	want := errors.New("client disconnected")
	_, err := a.Run(context.Background(), RunRequest{Message: "问题", OnEvent: func(StreamEvent) error { return want }})
	if !errors.Is(err, want) {
		t.Fatalf("stream error = %v, want %v", err, want)
	}
}

type runContextFixtureAgent struct {
	metadata runcontext.Metadata
}

func (a *runContextFixtureAgent) Name(context.Context) string        { return "fixture" }
func (a *runContextFixtureAgent) Description(context.Context) string { return "fixture" }
func (a *runContextFixtureAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	a.metadata = runcontext.From(ctx)
	iter, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		generator.Send(&adk.AgentEvent{Output: &adk.AgentOutput{MessageOutput: &adk.MessageVariant{
			Role:    schema.Assistant,
			Message: schema.AssistantMessage("ok", nil),
		}}})
	}()
	return iter
}

func TestStreamAddsRunIDToContext(t *testing.T) {
	fixture := &runContextFixtureAgent{}
	a := &WikiAgent{agent: fixture, maxElapsed: testMaxElapsed}
	ctx := runcontext.With(context.Background(), runcontext.Metadata{SessionID: "session-a", WikiRoot: "/tmp/wiki"})
	result, err := a.Run(ctx, RunRequest{Message: "问题", OnEvent: func(StreamEvent) error { return nil }})
	if err != nil || result.Answer != "ok" {
		t.Fatalf("stream result=%+v err=%v", result, err)
	}
	if fixture.metadata.SessionID != "session-a" || fixture.metadata.WikiRoot != "/tmp/wiki" || fixture.metadata.RunID == "" {
		t.Fatalf("metadata not propagated with run id: %+v", fixture.metadata)
	}
}

type timeoutFixtureAgent struct{}

func (timeoutFixtureAgent) Name(context.Context) string        { return "fixture" }
func (timeoutFixtureAgent) Description(context.Context) string { return "fixture" }
func (timeoutFixtureAgent) Run(ctx context.Context, _ *adk.AgentInput, _ ...adk.AgentRunOption) *adk.AsyncIterator[*adk.AgentEvent] {
	iter, generator := adk.NewAsyncIteratorPair[*adk.AgentEvent]()
	go func() {
		defer generator.Close()
		<-ctx.Done()
		generator.Send(&adk.AgentEvent{Err: ctx.Err()})
	}()
	return iter
}

func TestStreamMarksElapsedLimit(t *testing.T) {
	a := &WikiAgent{agent: timeoutFixtureAgent{}, maxElapsed: time.Millisecond}
	_, err := a.Run(context.Background(), RunRequest{Message: "问题", OnEvent: func(StreamEvent) error { return nil }})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v, want context deadline exceeded", err)
	}
}

func TestStreamPropagatesExternalCancellation(t *testing.T) {
	a := &WikiAgent{agent: timeoutFixtureAgent{}, maxElapsed: testMaxElapsed}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.Run(ctx, RunRequest{Message: "问题", OnEvent: func(StreamEvent) error { return nil }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want context canceled", err)
	}
}
