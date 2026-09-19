package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	agentmiddleware "github.com/lllllan02/wiki-agent/internal/agent/middleware"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
	"github.com/lllllan02/wiki-agent/internal/store"
	middleware "github.com/lllllan02/wiki-agent/internal/tool/middleware"
)

type fixtureModel struct {
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (m *fixtureModel) WithTools([]*schema.ToolInfo) (model.ToolCallingChatModel, error) {
	return m, nil
}
func (m *fixtureModel) Generate(ctx context.Context, input []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.calls.Add(1)
	if m.started != nil {
		close(m.started)
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if input[len(input)-1].Role == schema.Tool {
		return schema.AssistantMessage("done", nil), nil
	}
	return schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1", Type: "function", Function: schema.FunctionCall{Name: "read", Arguments: `{"path":"saved"}`}}}), nil
}
func (m *fixtureModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, input, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}

type fixtureTool struct {
	calls     atomic.Int32
	interrupt bool
	pause     *atomic.Bool
}

func (*fixtureTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: "read", Desc: "read", ParamsOneOf: schema.NewParamsOneOfByParams(map[string]*schema.ParameterInfo{"path": {Type: schema.String}})}, nil
}
func (t *fixtureTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	t.calls.Add(1)
	if t.interrupt {
		interrupted, hasState, saved := tool.GetInterruptState[string](ctx)
		if !interrupted {
			return "", tool.StatefulInterrupt(ctx, "paused", args)
		}
		if !hasState || saved != `{"path":"saved"}` {
			return "", errors.New("saved arguments lost")
		}
	}
	if t.pause != nil {
		t.pause.Store(true)
	}
	return "content", nil
}
func newInterruptAgent(t *testing.T, m *fixtureModel, toolImpl *fixtureTool) *WikiAgent {
	t.Helper()
	a, err := adk.NewChatModelAgent(context.Background(), &adk.ChatModelAgentConfig{
		Name: "fixture", Model: m, Handlers: []adk.ChatModelAgentMiddleware{&agentmiddleware.Interrupt{}},
		ToolsConfig: adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{
			Tools: []tool.BaseTool{toolImpl}, ToolCallMiddlewares: []compose.ToolMiddleware{middleware.ContractMiddleware()},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &WikiAgent{agent: a, maxElapsed: time.Minute}
}
func metadata(t *testing.T) (runcontext.Metadata, string) {
	t.Helper()
	dir := t.TempDir()
	s := store.NewIn(dir)
	return runcontext.Metadata{CheckpointID: s.CheckPointID(t.TempDir(), store.NewSessionID()), CheckpointStore: s}, dir
}

func TestToolInterruptResumeWithRecreatedAgentAndStore(t *testing.T) {
	meta, dir := metadata(t)
	firstModel, firstTool := &fixtureModel{}, &fixtureTool{interrupt: true}
	result, err := newInterruptAgent(t, firstModel, firstTool).Run(runcontext.With(context.Background(), meta), RunRequest{Message: "read"})
	if err != nil || result.Interrupt == nil || result.Answer != "" {
		t.Fatalf("interrupt: result=%+v err=%v", result, err)
	}
	if firstModel.calls.Load() != 1 || firstTool.calls.Load() != 1 {
		t.Fatal("execution continued past interrupt")
	}
	// Recreate both agent and disk store to exercise persisted state, not memory.
	meta.Resume, meta.CheckpointStore = true, store.NewIn(dir)
	secondModel, secondTool := &fixtureModel{}, &fixtureTool{interrupt: true}
	result, err = newInterruptAgent(t, secondModel, secondTool).Run(runcontext.With(context.Background(), meta), RunRequest{})
	if err != nil || result.Interrupt != nil || result.Answer != "done" {
		t.Fatalf("resume: result=%+v err=%v", result, err)
	}
	if secondModel.calls.Load() != 1 || secondTool.calls.Load() != 1 {
		t.Fatal("resume replayed earlier model step")
	}
}

func TestMiddlewarePauseResume(t *testing.T) {
	meta, dir := metadata(t)
	m := &fixtureModel{started: make(chan struct{}), release: make(chan struct{})}
	toolImpl := &fixtureTool{}
	var pause atomic.Bool
	meta.PauseRequested = func() (bool, error) { return pause.Load(), nil }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan *Result, 1)
	failures := make(chan error, 1)
	a := newInterruptAgent(t, m, toolImpl)
	go func() {
		result, err := a.Run(runcontext.With(ctx, meta), RunRequest{Message: "read"})
		done <- result
		failures <- err
	}()
	<-m.started
	pause.Store(true)
	close(m.release)
	result, err := <-done, <-failures
	if err != nil || result.Interrupt == nil {
		t.Fatalf("pause: result=%+v err=%v", result, err)
	}
	if toolImpl.calls.Load() != 0 {
		t.Fatal("tool executed after pause")
	}
	pause.Store(false)
	meta.Resume, meta.CheckpointStore = true, store.NewIn(dir)
	result, err = newInterruptAgent(t, &fixtureModel{}, toolImpl).Run(runcontext.With(ctx, meta), RunRequest{})
	if err != nil || result.Answer != "done" || toolImpl.calls.Load() != 1 {
		t.Fatalf("resume: result=%+v err=%v calls=%d", result, err, toolImpl.calls.Load())
	}
}

func TestPauseBeforeModel(t *testing.T) {
	meta, dir := metadata(t)
	meta.PauseRequested = func() (bool, error) { return true, nil }
	m, toolImpl := &fixtureModel{}, &fixtureTool{}
	result, err := newInterruptAgent(t, m, toolImpl).Run(runcontext.With(context.Background(), meta), RunRequest{Message: "read"})
	if err != nil || result.Interrupt == nil || m.calls.Load() != 0 {
		t.Fatalf("pause: result=%+v err=%v calls=%d", result, err, m.calls.Load())
	}
	meta.Resume, meta.CheckpointStore, meta.PauseRequested = true, store.NewIn(dir), nil
	result, err = newInterruptAgent(t, m, toolImpl).Run(runcontext.With(context.Background(), meta), RunRequest{})
	if err != nil || result.Answer != "done" || toolImpl.calls.Load() != 1 {
		t.Fatalf("resume: result=%+v err=%v", result, err)
	}
}

func TestRejectIncompleteCheckpointConfiguration(t *testing.T) {
	for _, meta := range []runcontext.Metadata{{Resume: true}, {CheckpointID: "missing"}, {CheckpointStore: store.NewIn(t.TempDir())}} {
		_, err := newInterruptAgent(t, &fixtureModel{}, &fixtureTool{}).Run(runcontext.With(context.Background(), meta), RunRequest{Message: "read"})
		if err == nil {
			t.Fatalf("accepted invalid metadata: %+v", meta)
		}
	}
}

func TestPauseAfterToolDoesNotReplayCompletedTool(t *testing.T) {
	meta, dir := metadata(t)
	var pause atomic.Bool
	meta.PauseRequested = func() (bool, error) { return pause.Load(), nil }
	m, toolImpl := &fixtureModel{}, &fixtureTool{pause: &pause}
	result, err := newInterruptAgent(t, m, toolImpl).Run(runcontext.With(context.Background(), meta), RunRequest{Message: "read"})
	if err != nil || result.Interrupt == nil || toolImpl.calls.Load() != 1 || m.calls.Load() != 1 {
		t.Fatalf("pause: result=%+v err=%v", result, err)
	}
	pause.Store(false)
	meta.Resume, meta.CheckpointStore = true, store.NewIn(dir)
	result, err = newInterruptAgent(t, m, toolImpl).Run(runcontext.With(context.Background(), meta), RunRequest{})
	if err != nil || result.Answer != "done" || toolImpl.calls.Load() != 1 || m.calls.Load() != 2 {
		t.Fatalf("resume replayed work: result=%+v err=%v tools=%d model=%d", result, err, toolImpl.calls.Load(), m.calls.Load())
	}
}
