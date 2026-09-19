package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cloudwego/eino/adk"
)

var _ adk.CheckPointStore = (*Store)(nil)

// Eino owns the opaque checkpoint encoding. JSON encodes []byte as base64;
// each complete line is one snapshot and the last complete snapshot wins.
type checkpointRecord struct {
	Data []byte `json:"data"`
}

func (s *Store) Get(ctx context.Context, id string) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	path, err := s.checkPointPath(id)
	if err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	reader := bufio.NewReader(f)
	var latest checkpointRecord
	found := false
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		} // Ignore an incomplete write at the tail.
		if err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal(line, &latest); err != nil {
			return nil, false, fmt.Errorf("decode checkpoint %s: %w", id, err)
		}
		found = true
	}
	return latest.Data, found, ctx.Err()
}

func (s *Store) Set(ctx context.Context, id string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.checkPointPath(id)
	if err != nil {
		return err
	}
	line, err := json.Marshal(checkpointRecord{Data: data})
	if err != nil {
		return err
	}
	line = append(line, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// Repair a torn final line before appending the next snapshot.
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	complete := int64(bytes.LastIndexByte(existing, '\n') + 1)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(complete); err != nil {
		return err
	}
	if _, err := f.Seek(complete, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.checkPointPath(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *Store) CheckPointID(wikiRoot, sessionID string) string {
	return NodeID(wikiRoot) + "-" + sessionID
}
func (s *Store) checkPointPath(id string) (string, error) {
	if !validID(id, "n-") {
		return "", fmt.Errorf("invalid checkpoint id")
	}
	return filepath.Join(s.baseDir, dirName, checkpointsDir, id+".jsonl"), nil
}
