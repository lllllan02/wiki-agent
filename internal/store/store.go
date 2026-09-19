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
	dirName     = ".wiki-agent"
	sessionsDir = "sessions"
)

type Store struct {
	mu      sync.Mutex
	baseDir string
}

type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	NodeID    string    `json:"node_id"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type Session struct {
	ID           string    `json:"id"`
	NodeID       string    `json:"node_id"`
	Title        string    `json:"title"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

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
		return nil
	}
	if err != nil {
		return err
	}
	return file.Close()
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
	return encoder.Encode(message)
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
	err := os.Remove(s.SessionPath(wikiRoot, sessionID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) SessionPath(wikiRoot, sessionID string) string {
	return filepath.Join(s.baseDir, dirName, sessionsDir, NodeID(wikiRoot), sessionID+".jsonl")
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
