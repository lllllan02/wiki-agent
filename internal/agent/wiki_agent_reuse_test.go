package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/config"
	toolregistry "github.com/lllllan02/wiki-agent/internal/tool/registry"
)

type fixtureContextKey struct{}

type runFixtureTool struct{}

func (t runFixtureTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "fixture__read"}, nil
}
func (t runFixtureTool) InvokableRun(ctx context.Context, _ string, _ ...tool.Option) (string, error) {
	return ctx.Value(fixtureContextKey{}).(string), nil
}

// 同一组标准工具注册到多个 Agent；并发调用只从上下文读取本轮状态。
func TestSingleAgentIsolatesRunTools(t *testing.T) {
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []*schema.Message `json:"messages"`
			Tools    []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if len(request.Tools) != 1 {
			t.Errorf("unexpected tools: %+v", request.Tools)
			w.WriteHeader(500)
			return
		}
		last := request.Messages[len(request.Messages)-1]
		msg := schema.AssistantMessage(last.Content, nil)
		if last.Role != schema.Tool {
			msg = schema.AssistantMessage("", []schema.ToolCall{{ID: "call", Type: "function", Function: schema.FunctionCall{Name: request.Tools[0].Function.Name, Arguments: "{}"}}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": msg}}})
	}))
	defer modelServer.Close()
	ctx := context.Background()
	cfg, err := config.NewDefaults[config.Config]()
	if err != nil {
		t.Fatal(err)
	}
	model, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{APIKey: "fixture", Model: "fixture", BaseURL: modelServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	tools := []tool.BaseTool{runFixtureTool{}}
	makeAgent := func() adk.Agent {
		agentConfig := &adk.ChatModelAgentConfig{Name: "fixture", Model: model, MaxIterations: cfg.Agent.MaxSteps}
		toolregistry.RegisterTools(agentConfig, tools, compose.ToolMiddleware{
			Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
				return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
					// 验证动态注入的工具同样经过 Agent 的原生中间件，并保留每轮 context。
					output, err := next(ctx, input)
					if err != nil {
						return nil, err
					}
					return &compose.ToolOutput{Result: "middleware:" + output.Result}, nil
				}
			},
		})
		shared, err := adk.NewChatModelAgent(ctx, agentConfig)
		if err != nil {
			t.Fatal(err)
		}
		return shared
	}
	agents := []adk.Agent{makeAgent(), makeAgent()}
	run := func(shared adk.Agent, name, want string) {
		t.Helper()
		runCtx := context.WithValue(ctx, fixtureContextKey{}, want)
		runner := adk.NewRunner(runCtx, adk.RunnerConfig{Agent: shared})
		iter := runner.Run(runCtx, []*schema.Message{schema.UserMessage("read")})
		answer := ""
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			if event.Err != nil {
				t.Error(event.Err)
				return
			}
			if event.Output != nil && event.Output.MessageOutput != nil {
				msg, err := event.Output.MessageOutput.GetMessage()
				if err != nil {
					t.Error(err)
					return
				}
				if msg != nil && msg.Role == schema.Assistant {
					answer = msg.Content
				}
			}
		}
		if answer != "middleware:"+want {
			t.Errorf("%s: got %q, want %q", name, answer, "middleware:"+want)
		}
	}
	for round := 0; round < 2; round++ {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				run(agents[i%len(agents)], fmt.Sprintf("read_%d", i), fmt.Sprintf("wiki_%d_round_%d", i, round))
			}(i)
		}
		wg.Wait()
	}
}
