// Package runcontext 在 Web、Agent 和工具之间传递本次运行的标识与目录。
package runcontext

import "context"

// Metadata 不包含消息历史；未来由 Agent 按 SessionID 从存储中读取历史。
type Metadata struct {
	SessionID string
	WikiRoot  string
}

type metadataKey struct{}

func With(ctx context.Context, metadata Metadata) context.Context {
	return context.WithValue(ctx, metadataKey{}, metadata)
}

func From(ctx context.Context) Metadata {
	metadata, _ := ctx.Value(metadataKey{}).(Metadata)
	return metadata
}
