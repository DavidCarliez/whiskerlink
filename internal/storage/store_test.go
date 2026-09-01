package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
)

func TestTransferAndResumeStatePersistence(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	store, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().Truncate(time.Millisecond)
	transfer := domain.Transfer{
		ID: "transfer-1", Direction: domain.TransferReceive, Label: "Logs", State: "paused",
		Destination: filepath.Join(config, "downloads"), FilesTotal: 1, BytesTotal: 42,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.UpsertTransfer(context.Background(), transfer); err != nil {
		t.Fatal(err)
	}
	state := ReceiveState{
		Manifest: domain.FileManifest{ProtocolVersion: 1, TransferID: "remote-1", Label: "Logs", Files: []domain.FileEntry{{Path: "logs.txt", Size: 42}}, TotalBytes: 42},
		Selected: []string{"logs.txt"}, Completed: []string{"logs.txt"}, Collision: "rename",
	}
	if err := store.SaveReceiveState(context.Background(), transfer.ID, state); err != nil {
		t.Fatal(err)
	}

	transfers, err := store.ListTransfers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(transfers) != 1 || transfers[0].ID != transfer.ID || transfers[0].State != "paused" {
		t.Fatalf("unexpected transfers: %+v", transfers)
	}
	loaded, err := store.LoadReceiveState(context.Background(), transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Manifest.TransferID != "remote-1" || len(loaded.Selected) != 1 || len(loaded.Completed) != 1 || loaded.Collision != "rename" {
		t.Fatalf("unexpected receive state: %+v", loaded)
	}

	info, err := os.Stat(filepath.Join(config, "whiskerlink", "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("database permissions = %o, want 600", got)
	}
}
