package middleware

import (
	"context"
	"time"

	"github.com/cloudwego/eino/compose"
)

func Timeout(timeout time.Duration) compose.ToolMiddleware {
	return compose.ToolMiddleware{Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
		return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if timeout <= 0 {
				return next(ctx, input)
			}
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			output, err := next(callCtx, input)
			if err != nil {
				return nil, err
			}
			if err := callCtx.Err(); err != nil {
				return nil, err
			}
			return output, nil
		}
	}}
}
