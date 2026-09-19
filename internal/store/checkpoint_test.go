package store

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestCheckpointJSONLRoundTrip(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := NewIn(dir)
	id := s.CheckPointID(t.TempDir(), NewSessionID())
	snapshots := [][]byte{[]byte("first"), bytes.Repeat([]byte{0, 1, 255}, 100000)}
	for _, data := range snapshots {
		if err := s.Set(ctx, id, data); err != nil {
			t.Fatal(err)
		}
	}
	path, _ := s.checkPointPath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("expected two JSONL records, got %d", len(lines))
	}
	for i, line := range lines {
		var record checkpointRecord
		if err := json.Unmarshal(line, &record); err != nil || !bytes.Equal(record.Data, snapshots[i]) {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	restored := NewIn(dir)
	data, ok, err := restored.Get(ctx, id)
	if err != nil || !ok || !bytes.Equal(data, snapshots[1]) {
		t.Fatalf("latest snapshot: exists=%v err=%v", ok, err)
	}
	// A crashed append must not destroy the last committed snapshot.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"data":"unfinished`)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	data, ok, err = restored.Get(ctx, id)
	if err != nil || !ok || !bytes.Equal(data, snapshots[1]) {
		t.Fatalf("torn append: exists=%v err=%v", ok, err)
	}
	if err := restored.Set(ctx, id, []byte("repaired")); err != nil {
		t.Fatal(err)
	}
	data, ok, err = restored.Get(ctx, id)
	if err != nil || !ok || string(data) != "repaired" {
		t.Fatalf("repair: %q %v %v", data, ok, err)
	}
	if err := restored.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := restored.Get(ctx, id); err != nil || ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
}

func TestCheckpointRejectsCorruptCompleteRecord(t *testing.T) {
	s := NewIn(t.TempDir())
	id := s.CheckPointID(t.TempDir(), NewSessionID())
	if err := s.Set(context.Background(), id, []byte("valid")); err != nil {
		t.Fatal(err)
	}
	path, _ := s.checkPointPath(id)
	if err := os.WriteFile(path, []byte("invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(context.Background(), id); err == nil {
		t.Fatal("silently accepted corrupt checkpoint")
	}
}
