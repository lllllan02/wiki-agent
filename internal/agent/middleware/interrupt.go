// Package middleware contains Eino ChatModelAgent middlewares.
package middleware

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
	"github.com/lllllan02/wiki-agent/internal/runcontext"
)

// Interrupt pauses at the next model/tool boundary. Eino owns checkpointing
// and resumption; the middleware has no mutable state shared between runs.
type Interrupt struct {
	adk.BaseChatModelAgentMiddleware
}

func pauseRequested(ctx context.Context) (bool, error) {
	if check := runcontext.From(ctx).PauseRequested; check != nil {
		return check()
	}
	return false, nil
}

// Follow the official StatefulInterrupt/GetInterruptState pattern: save tool
// arguments on pause and use those saved arguments when the tool is resumed.
func interruptTool(ctx context.Context, name, args string) (string, error) {
	if interrupted, hasState, saved := tool.GetInterruptState[string](ctx); interrupted && hasState {
		args = saved
	}
	pause, err := pauseRequested(ctx)
	if err != nil {
		return "", err
	}
	if pause {
		return "", tool.StatefulInterrupt(ctx, "paused before tool: "+name, args)
	}
	return args, nil
}

func (*Interrupt) WrapInvokableToolCall(_ context.Context, next adk.InvokableToolCallEndpoint, info *adk.ToolContext) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		args, err := interruptTool(ctx, info.Name, args)
		if err != nil {
			return "", err
		}
		return next(ctx, args, opts...)
	}, nil
}

func (*Interrupt) WrapStreamableToolCall(_ context.Context, next adk.StreamableToolCallEndpoint, info *adk.ToolContext) (adk.StreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
		args, err := interruptTool(ctx, info.Name, args)
		if err != nil {
			return nil, err
		}
		return next(ctx, args, opts...)
	}, nil
}

// Model boundaries also cover pauses requested while a tool is running. The
// completed tool result stays in Eino's state and is not executed again.
func (*Interrupt) WrapModel(_ context.Context, next model.BaseChatModel, _ *adk.ModelContext) (model.BaseChatModel, error) {
	return &interruptModel{next}, nil
}

type interruptModel struct{ model.BaseChatModel }

func interruptBeforeModel(ctx context.Context) error {
	pause, err := pauseRequested(ctx)
	if err != nil {
		return err
	}
	if pause {
		return compose.Interrupt(ctx, "paused before model")
	}
	return nil
}
func (m *interruptModel) Generate(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.Message, error) {
	if err := interruptBeforeModel(ctx); err != nil {
		return nil, err
	}
	return m.BaseChatModel.Generate(ctx, input, opts...)
}
func (m *interruptModel) Stream(ctx context.Context, input []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	if err := interruptBeforeModel(ctx); err != nil {
		return nil, err
	}
	return m.BaseChatModel.Stream(ctx, input, opts...)
}
