package middleware

import (
	"context"
	"time"

	"github.com/cloudwego/eino/compose"
)

func Timeout(timeout time.Duration) compose.ToolMiddleware {
	return compose.ToolMiddleware{Invokable: func(next compose.InvokableToolEndpoint) compose.InvokableToolEndpoint {
		return func(ctx context.Context, input *compose.ToolInput) (*compose.ToolOutput, error) {
			if timeout <= 0 {
				return next(ctx, input)
			}
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return next(callCtx, input)
		}
	}}
}
