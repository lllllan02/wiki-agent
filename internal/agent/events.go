package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

// collectEvents is the equivalent of the official example's PrintAndCollect:
// forward messages and return the interrupt separately from the final answer.
func collectEvents(ctx context.Context, iter *adk.AsyncIterator[*adk.AgentEvent], report EventReporter) (*Result, error) {
	result := &Result{}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		event, ok := iter.Next()
		if !ok {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Err != nil {
			return nil, event.Err
		}
		if event.Action != nil && event.Action.Interrupted != nil {
			result.Interrupt = event.Action.Interrupted
		}
		if event.Output == nil || event.Output.MessageOutput == nil {
			continue
		}
		message, err := consumeMessage(ctx, event.Output.MessageOutput, report)
		if err != nil {
			return nil, err
		}
		if message == nil {
			continue
		}
		// Tool output and intermediate tool-call text are not a final answer.
		if message.Role == schema.Assistant && len(message.ToolCalls) == 0 && strings.TrimSpace(message.Content) != "" {
			result.Answer = message.Content
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if result.Interrupt == nil && strings.TrimSpace(result.Answer) == "" {
		return nil, fmt.Errorf("model returned an empty final answer")
	}
	return result, nil
}

// Emit cumulative snapshots as chunks arrive. Each model turn starts a fresh
// snapshot; tool-call fragments are merged by Eino rather than concatenated by hand.
func consumeMessage(ctx context.Context, output *adk.MessageVariant, report EventReporter) (*schema.Message, error) {
	emit := func(message *schema.Message) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if message != nil && report != nil {
			return report(StreamEvent{Message: message})
		}
		return nil
	}
	if !output.IsStreaming {
		return output.Message, emit(output.Message)
	}
	stream := output.MessageStream
	defer stream.Close()
	var message *schema.Message
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return message, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		if chunk == nil {
			continue
		}
		if message == nil {
			message, err = schema.ConcatMessages([]*schema.Message{chunk})
		} else {
			message, err = schema.ConcatMessages([]*schema.Message{message, chunk})
		}
		if err != nil {
			return nil, err
		}
		if message.Role == "" {
			message.Role = output.Role
		}
		if err := emit(message); err != nil {
			return nil, err
		}
	}
}
