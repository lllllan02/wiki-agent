package agent

import (
	"context"
	"testing"

	"github.com/lllllan02/wiki-agent/internal/config"
)

func TestWikiAgentEmptyRequest(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.NewDefaults[config.Config]()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.APIKey, cfg.Model.Name, cfg.Model.BaseURL = "test-key", "test-model", "http://127.0.0.1:1/v1"
	wikiAgent, err := NewWikiAgent(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wikiAgent.RunWithHistory(context.Background(), dir, " ", nil); err == nil {
		t.Fatal("expected empty request rejection")
	}
}
