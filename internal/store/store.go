package store

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
)

const (
	dirName        = ".wiki-agent"
	sessionsDir    = "sessions"
	statesDir      = "states"
	checkpointsDir = "checkpoints"
)

type Store struct {
	mu      sync.Mutex
	baseDir string
}

type Message struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	NodeID    string `json:"node_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	// ReceivedSeq is assigned only to incoming user messages. It is the
	// durable ordering used by the session coordinator; timestamps are not.
	ReceivedSeq int       `json:"received_seq,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Session struct {
	ID           string    `json:"id"`
	NodeID       string    `json:"node_id"`
	Title        string    `json:"title"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type RunStatus string

const (
	RunStatusIdle     RunStatus = "idle"
	RunStatusRunning  RunStatus = "running"
	RunStatusPaused   RunStatus = "paused"
	RunStatusCanceled RunStatus = "canceled"
	RunStatusFailed   RunStatus = "failed"
)

type PauseStatus string

const (
	PauseStatusNone      PauseStatus = "none"
	PauseStatusRequested PauseStatus = "requested"
	PauseStatusPaused    PauseStatus = "paused"
)

type SessionState struct {
	SessionID            string      `json:"session_id"`
	NodeID               string      `json:"node_id"`
	WikiRoot             string      `json:"wiki_root"`
	ReceivedCount        int         `json:"received_count"`
	AcceptedCount        int         `json:"accepted_count"`
	AcceptedThroughSeq   int         `json:"accepted_through_seq"`
	EffectiveFromSeq     int         `json:"effective_from_seq,omitempty"`
	IntentRevision       int         `json:"intent_revision"`
	ActiveRunID          int64       `json:"active_run_id,omitempty"`
	ActiveIntentRevision int         `json:"active_intent_revision,omitempty"`
	CheckpointRevision   int         `json:"checkpoint_revision,omitempty"`
	CheckpointAccepted   int         `json:"checkpoint_accepted_through_seq,omitempty"`
	RunStatus            RunStatus   `json:"run_status"`
	PauseStatus          PauseStatus `json:"pause_status"`
	CurrentVisibleAnswer string      `json:"current_visible_answer,omitempty"`
	UpdatedAt            time.Time   `json:"updated_at"`
}

type StateUpdate func(*SessionState)

func New() *Store {
	baseDir, err := os.Getwd()
	if err != nil {
		baseDir = "."
	}
	return NewIn(baseDir)
}

func NewIn(baseDir string) *Store {
	return &Store{baseDir: baseDir}
}

func NewSessionID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("s-%d", time.Now().UTC().UnixNano())
	}
	return "s-" + hex.EncodeToString(bytes[:])
}

func NewMessageID() string {
	var bytes [12]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("m-%d", time.Now().UTC().UnixNano())
	}
	return "m-" + hex.EncodeToString(bytes[:])
}

func NodeID(root string) string {
	normalized, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		normalized = strings.TrimSpace(root)
	}
	sum := sha256.Sum256([]byte(normalized))
	return "n-" + hex.EncodeToString(sum[:8])
}

func (s *Store) CreateSession(wikiRoot, sessionID string) error {
	wikiRoot = strings.TrimSpace(wikiRoot)
	if wikiRoot == "" {
		return fmt.Errorf("wiki root is required")
	}
	if !validID(sessionID, "s-") {
		return fmt.Errorf("invalid session id")
	}
	path := s.SessionPath(wikiRoot, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return s.updateSessionStateLocked(wikiRoot, sessionID, func(state *SessionState) {})
	}
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return s.updateSessionStateLocked(wikiRoot, sessionID, func(state *SessionState) {})
}

func (s *Store) AppendMessage(wikiRoot string, message Message) error {
	wikiRoot = strings.TrimSpace(wikiRoot)
	if wikiRoot == "" {
		return fmt.Errorf("wiki root is required")
	}
	if !validID(message.SessionID, "s-") {
		return fmt.Errorf("invalid session id")
	}
	if !validID(message.ID, "m-") {
		return fmt.Errorf("invalid message id")
	}
	if message.Role != "user" && message.Role != "assistant" {
		return fmt.Errorf("invalid message role")
	}
	nodeID := NodeID(wikiRoot)
	if message.NodeID == "" {
		message.NodeID = nodeID
	}
	if message.NodeID != nodeID {
		return fmt.Errorf("message node does not match wiki root")
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	path := s.SessionPath(wikiRoot, message.SessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(message); err != nil {
		return err
	}
	return s.updateSessionStateLocked(wikiRoot, message.SessionID, func(state *SessionState) {})
}

// ReceiveUserMessage durably records a user message before it is handed to a
// coordinator. Receiving is deliberately separate from accepting: a restart
// must retain messages that arrived while a run was still winding down.
func (s *Store) ReceiveUserMessage(wikiRoot string, message Message) (Message, error) {
	if strings.TrimSpace(wikiRoot) == "" {
		return Message{}, fmt.Errorf("wiki root is required")
	}
	if !validID(message.SessionID, "s-") || !validID(message.ID, "m-") {
		return Message{}, fmt.Errorf("invalid message or session id")
	}
	if message.Role != "user" {
		return Message{}, fmt.Errorf("received message must have user role")
	}
	nodeID := NodeID(wikiRoot)
	if message.NodeID == "" {
		message.NodeID = nodeID
	}
	if message.NodeID != nodeID {
		return Message{}, fmt.Errorf("message node does not match wiki root")
	}
	if message.CreatedAt.IsZero() {
		message.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.SessionPath(wikiRoot, message.SessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return Message{}, err
	}
	state, err := s.loadSessionStateLocked(wikiRoot, message.SessionID, s.StatePath(wikiRoot, message.SessionID))
	if err != nil {
		return Message{}, err
	}
	message.ReceivedSeq = state.ReceivedCount + 1
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return Message{}, err
	}
	if err := json.NewEncoder(file).Encode(message); err != nil {
		_ = file.Close()
		return Message{}, err
	}
	if err := file.Close(); err != nil {
		return Message{}, err
	}
	state.ReceivedCount = message.ReceivedSeq
	state.SessionID = message.SessionID
	state.NodeID = nodeID
	state.WikiRoot = strings.TrimSpace(wikiRoot)
	state.UpdatedAt = time.Now().UTC()
	if err := s.writeSessionStateLocked(s.StatePath(wikiRoot, message.SessionID), state); err != nil {
		return Message{}, err
	}
	return message, nil
}

// AcceptUserMessage serializes application of a received instruction. An
// append retains the current goal; replace moves the effective context start.
func (s *Store) AcceptUserMessage(wikiRoot, sessionID string, sequence int, replace bool) (SessionState, error) {
	if sequence < 1 {
		return SessionState{}, fmt.Errorf("invalid received sequence")
	}
	var accepted SessionState
	err := s.UpdateSessionState(wikiRoot, sessionID, func(state *SessionState) {
		if sequence <= state.AcceptedThroughSeq {
			accepted = *state
			return
		}
		// A coordinator must not skip an earlier received instruction.
		if sequence != state.AcceptedThroughSeq+1 {
			return
		}
		state.AcceptedThroughSeq = sequence
		state.AcceptedCount = sequence
		state.IntentRevision++
		if replace {
			state.EffectiveFromSeq = sequence
		}
		accepted = *state
	})
	if err != nil {
		return SessionState{}, err
	}
	if accepted.AcceptedThroughSeq != sequence {
		return SessionState{}, fmt.Errorf("received messages must be accepted in order")
	}
	return accepted, nil
}

func (s *Store) UpdateVisibleAnswerIfCurrent(wikiRoot, sessionID string, runID int64, revision int, answer string) (bool, error) {
	updated := false
	err := s.UpdateSessionState(wikiRoot, sessionID, func(state *SessionState) {
		if state.ActiveRunID == runID && state.ActiveIntentRevision == revision && state.IntentRevision == revision {
			state.CurrentVisibleAnswer = answer
			updated = true
		}
	})
	return updated, err
}

func (s *Store) LoadMessages(wikiRoot, sessionID string) ([]Message, error) {
	if !validID(sessionID, "s-") {
		return nil, fmt.Errorf("invalid session id")
	}
	file, err := os.Open(s.SessionPath(wikiRoot, sessionID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var messages []Message
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var message Message
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, scanner.Err()
}

func (s *Store) LoadAgentHistory(wikiRoot, sessionID string) ([]*schema.Message, error) {
	messages, err := s.LoadMessages(wikiRoot, sessionID)
	if err != nil {
		return nil, err
	}
	history := make([]*schema.Message, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "user":
			history = append(history, schema.UserMessage(message.Content))
		case "assistant":
			history = append(history, schema.AssistantMessage(message.Content, nil))
		}
	}
	return history, nil
}

// LoadExecutionHistory returns the context preceding a received user message.
// When a task was explicitly replaced, messages before EffectiveFromSeq remain
// visible in the transcript but are intentionally excluded from model input.
func (s *Store) LoadExecutionHistory(wikiRoot, sessionID string, beforeSeq, effectiveFromSeq int) ([]*schema.Message, error) {
	messages, err := s.LoadMessages(wikiRoot, sessionID)
	if err != nil {
		return nil, err
	}
	history := make([]*schema.Message, 0, len(messages))
	for _, message := range messages {
		if message.Role == "user" {
			if message.ReceivedSeq >= beforeSeq {
				continue
			}
			if effectiveFromSeq > 0 && message.ReceivedSeq > 0 && message.ReceivedSeq < effectiveFromSeq {
				continue
			}
			history = append(history, schema.UserMessage(message.Content))
			continue
		}
		// Assistant output produced before a replacement belongs to the audit
		// transcript, not the replacement task's model context.
		if effectiveFromSeq == 0 || beforeSeq <= effectiveFromSeq {
			history = append(history, schema.AssistantMessage(message.Content, nil))
		}
	}
	return history, nil
}

func (s *Store) ListSessions(wikiRoot string) ([]Session, error) {
	nodeID := NodeID(wikiRoot)
	dir := filepath.Join(s.baseDir, dirName, sessionsDir, nodeID)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sessions []Session
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
		if !validID(sessionID, "s-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		messages, err := s.LoadMessages(wikiRoot, sessionID)
		if err != nil {
			return nil, err
		}
		session := Session{
			ID:        sessionID,
			NodeID:    nodeID,
			Title:     "新对话",
			CreatedAt: info.ModTime().UTC(),
			UpdatedAt: info.ModTime().UTC(),
		}
		if len(messages) > 0 {
			session.MessageCount = len(messages)
			session.CreatedAt = messages[0].CreatedAt
			session.UpdatedAt = messages[len(messages)-1].CreatedAt
			for _, message := range messages {
				if message.Role == "user" && strings.TrimSpace(message.Content) != "" {
					session.Title = sessionTitle(message.Content)
					break
				}
			}
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, nil
}

func (s *Store) DeleteSession(wikiRoot, sessionID string) error {
	if !validID(sessionID, "s-") {
		return fmt.Errorf("invalid session id")
	}
	for _, path := range []string{s.SessionPath(wikiRoot, sessionID), s.StatePath(wikiRoot, sessionID), filepath.Join(s.baseDir, dirName, checkpointsDir, s.CheckPointID(wikiRoot, sessionID)+".jsonl")} {
		err := os.Remove(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Store) SessionPath(wikiRoot, sessionID string) string {
	return filepath.Join(s.baseDir, dirName, sessionsDir, NodeID(wikiRoot), sessionID+".jsonl")
}

func (s *Store) StatePath(wikiRoot, sessionID string) string {
	return filepath.Join(s.baseDir, dirName, statesDir, NodeID(wikiRoot), sessionID+".json")
}

func (s *Store) LoadSessionState(wikiRoot, sessionID string) (SessionState, error) {
	if !validID(sessionID, "s-") {
		return SessionState{}, fmt.Errorf("invalid session id")
	}
	path := s.StatePath(wikiRoot, sessionID)
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.loadSessionStateLocked(wikiRoot, sessionID, path)
	if err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func (s *Store) UpdateSessionState(wikiRoot, sessionID string, update StateUpdate) error {
	if !validID(sessionID, "s-") {
		return fmt.Errorf("invalid session id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateSessionStateLocked(wikiRoot, sessionID, update)
}

func (s *Store) updateSessionStateLocked(wikiRoot, sessionID string, update StateUpdate) error {
	path := s.StatePath(wikiRoot, sessionID)
	state, err := s.loadSessionStateLocked(wikiRoot, sessionID, path)
	if err != nil {
		return err
	}
	if update != nil {
		update(&state)
	}
	state.SessionID = sessionID
	state.NodeID = NodeID(wikiRoot)
	state.WikiRoot = strings.TrimSpace(wikiRoot)
	state.UpdatedAt = time.Now().UTC()
	if state.RunStatus == "" {
		state.RunStatus = RunStatusIdle
	}
	if state.PauseStatus == "" {
		state.PauseStatus = PauseStatusNone
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return s.writeSessionStateLocked(path, state)
}

func (s *Store) writeSessionStateLocked(path string, state SessionState) error {
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func (s *Store) loadSessionStateLocked(wikiRoot, sessionID, path string) (SessionState, error) {
	state := SessionState{
		SessionID:   sessionID,
		NodeID:      NodeID(wikiRoot),
		WikiRoot:    strings.TrimSpace(wikiRoot),
		RunStatus:   RunStatusIdle,
		PauseStatus: PauseStatusNone,
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return SessionState{}, err
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(&state); err != nil {
		return SessionState{}, err
	}
	return state, nil
}

func validID(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) {
		return false
	}
	for _, ch := range id[len(prefix):] {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
			return false
		}
	}
	return true
}

func sessionTitle(content string) string {
	title := strings.Join(strings.Fields(content), " ")
	const limit = 32
	if len([]rune(title)) <= limit {
		return title
	}
	runes := []rune(title)
	return string(runes[:limit]) + "..."
}
