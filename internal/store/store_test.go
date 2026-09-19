package store

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestAppendAndLoadMessages(t *testing.T) {
	baseDir := t.TempDir()
	store := NewIn(baseDir)
	root := t.TempDir()
	sessionID := NewSessionID()
	if err := store.AppendMessage(root, Message{ID: NewMessageID(), SessionID: sessionID, Role: "user", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(root, Message{ID: NewMessageID(), SessionID: sessionID, Role: "assistant", Content: "world"}); err != nil {
		t.Fatal(err)
	}
	messages, err := store.LoadMessages(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Role != "user" || messages[0].Content != "hello" || messages[1].Role != "assistant" || messages[1].Content != "world" {
		t.Fatalf("messages: %+v", messages)
	}
	if messages[0].SessionID != sessionID || messages[1].SessionID != sessionID {
		t.Fatalf("session mismatch: %+v", messages)
	}
	if messages[0].NodeID != NodeID(root) || messages[1].NodeID != NodeID(root) {
		t.Fatalf("node mismatch: %+v", messages)
	}
	if got := store.SessionPath(root, sessionID); !strings.HasPrefix(got, baseDir) {
		t.Fatalf("session not stored under service root: %s", got)
	}
	history, err := store.LoadAgentHistory(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Role != schema.User || history[0].Content != "hello" || history[1].Role != schema.Assistant || history[1].Content != "world" {
		t.Fatalf("agent history: %+v", history)
	}
	state, err := store.LoadSessionState(root, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if state.ReceivedCount != 0 || state.IntentRevision != 0 || state.RunStatus != RunStatusIdle || state.PauseStatus != PauseStatusNone {
		t.Fatalf("state not updated from messages: %+v", state)
	}
}

func TestReceiveAndAcceptUserMessageAreOrdered(t *testing.T) {
	s := NewIn(t.TempDir())
	root, sessionID := t.TempDir(), NewSessionID()
	if err := s.CreateSession(root, sessionID); err != nil {
		t.Fatal(err)
	}
	first, err := s.ReceiveUserMessage(root, Message{ID: NewMessageID(), SessionID: sessionID, Role: "user", Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ReceiveUserMessage(root, Message{ID: NewMessageID(), SessionID: sessionID, Role: "user", Content: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ReceivedSeq != 1 || second.ReceivedSeq != 2 {
		t.Fatalf("sequences: %d %d", first.ReceivedSeq, second.ReceivedSeq)
	}
	if _, err := s.AcceptUserMessage(root, sessionID, second.ReceivedSeq, false); err == nil {
		t.Fatal("accepted messages out of order")
	}
	if _, err := s.AcceptUserMessage(root, sessionID, first.ReceivedSeq, false); err != nil {
		t.Fatal(err)
	}
	state, err := s.AcceptUserMessage(root, sessionID, second.ReceivedSeq, true)
	if err != nil {
		t.Fatal(err)
	}
	if state.ReceivedCount != 2 || state.AcceptedThroughSeq != 2 || state.IntentRevision != 2 || state.EffectiveFromSeq != 2 {
		t.Fatalf("state: %+v", state)
	}
}

func TestAppendRejectsInvalidSessionID(t *testing.T) {
	err := NewIn(t.TempDir()).AppendMessage(t.TempDir(), Message{ID: NewMessageID(), SessionID: "../bad", Role: "user", Content: "hello"})
	if err == nil {
		t.Fatal("accepted invalid session id")
	}
}

func TestListAndDeleteSessions(t *testing.T) {
	store := NewIn(t.TempDir())
	root := t.TempDir()
	first, second := NewSessionID(), NewSessionID()
	if err := store.CreateSession(root, first); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(root, second); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(root, Message{ID: NewMessageID(), SessionID: first, Role: "user", Content: "first conversation title"}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMessage(root, Message{ID: NewMessageID(), SessionID: second, Role: "user", Content: "second conversation title"}); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].NodeID != NodeID(root) || sessions[0].MessageCount != 1 {
		t.Fatalf("sessions: %+v", sessions)
	}
	if err := store.DeleteSession(root, sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	sessions, err = store.ListSessions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("delete did not update sessions: %+v", sessions)
	}
}
