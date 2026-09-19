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

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestStreamFromOpenAICompatibleModel(t *testing.T) {
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Stream {
			http.Error(w, "expected streaming request", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range []string{"你好，", "世界"} {
			payload := map[string]any{"id": "fixture", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": part}}}}
			encoded, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", encoded)
			w.(http.Flusher).Flush()
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
	a := &WikiAgent{agent: shared}
	var updates []string
	result, err := a.Stream(ctx, "你好", func(text string) error {
		updates = append(updates, text)
		return nil
	})
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
	a := &WikiAgent{agent: streamFixtureAgent{}}
	var updates []string
	result, err := a.Stream(context.Background(), "问题", func(text string) error {
		updates = append(updates, text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"准备", "准备检索", "最", "最终回答"}
	if !reflect.DeepEqual(updates, want) || result.Answer != "最终回答" {
		t.Fatalf("updates=%v, result=%+v", updates, result)
	}
}

func TestStreamStopsWhenConsumerFails(t *testing.T) {
	a := &WikiAgent{agent: streamFixtureAgent{}}
	want := errors.New("client disconnected")
	_, err := a.Stream(context.Background(), "问题", func(string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("stream error = %v, want %v", err, want)
	}
}
