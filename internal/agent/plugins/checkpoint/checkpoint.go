// Package checkpoint adds cooperative pause and resume to an Eino ADK Runner.
// Agent implementations keep ownership of their messages and event loop.
package checkpoint

import (
	"context"
	"fmt"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

type CancelRegistrar func(func() error)

type Plugin struct {
	ID            string
	Store         adk.CheckPointStore
	Resume        bool
	OnCancelReady CancelRegistrar
}

// Register attaches checkpoint persistence to the Runner configuration.
func (p *Plugin) Register(config *adk.RunnerConfig) {
	if p != nil {
		config.CheckPointStore = p.Store
	}
}

// Start supplies the checkpoint option and selects Run or Resume. The caller
// continues consuming Eino events and handling individual messages itself.
func (p *Plugin) Start(ctx context.Context, runner *adk.Runner, messages []*schema.Message) (*adk.AsyncIterator[*adk.AgentEvent], error) {
	if p == nil {
		return runner.Run(ctx, messages), nil
	}
	if p.ID == "" || p.Store == nil {
		return nil, fmt.Errorf("checkpoint id and store are required")
	}
	cancelOption, cancelFn := adk.WithCancel()
	options := []adk.AgentRunOption{cancelOption, adk.WithCheckPointID(p.ID)}
	var iter *adk.AsyncIterator[*adk.AgentEvent]
	if p.Resume {
		var err error
		iter, err = runner.Resume(ctx, p.ID, options...)
		if err != nil {
			return nil, err
		}
	} else {
		iter = runner.Run(ctx, messages, options...)
	}
	if p.OnCancelReady != nil {
		p.OnCancelReady(func() error {
			handle, accepted := cancelFn(
				adk.WithAgentCancelMode(adk.CancelAfterChatModel|adk.CancelAfterToolCalls),
				adk.WithAgentCancelTimeout(2*time.Second),
				adk.WithRecursive(),
			)
			if !accepted || handle == nil {
				return adk.ErrExecutionEnded
			}
			return handle.Wait()
		})
	}
	return iter, nil
}
