// Package runcontext 在 Web、Agent 和工具之间传递本次运行的标识与目录。
package runcontext

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Metadata 不包含消息历史；未来由 Agent 按 SessionID 从存储中读取历史。
type Metadata struct {
	SessionID string
	WikiRoot  string
	RunID     string
}

type metadataKey struct{}

func With(ctx context.Context, metadata Metadata) context.Context {
	return context.WithValue(ctx, metadataKey{}, metadata)
}

func From(ctx context.Context) Metadata {
	metadata, _ := ctx.Value(metadataKey{}).(Metadata)
	return metadata
}

func WithRunID(ctx context.Context) context.Context {
	metadata := From(ctx)
	metadata.RunID = newRunID()
	return With(ctx, metadata)
}

func newRunID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405.000000000")
	}
	return hex.EncodeToString(bytes[:])
}
