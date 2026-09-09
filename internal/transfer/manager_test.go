package transfer

import (
	"context"
	"testing"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
	"github.com/DavidCarliez/whiskerlink/internal/storage"
	"github.com/google/uuid"
)

type testSecrets map[string]string

func (s testSecrets) Set(k, v string) error { s[k] = v; return nil }
func (s testSecrets) Get(k string) (string, error) {
	if v, ok := s[k]; ok {
		return v, nil
	}
	return "", storage.ErrSecretNotFound
}
func (s testSecrets) Delete(k string) error { delete(s, k); return nil }

func TestStableReceiveRecoveryAndPermanentCancellation(t *testing.T) {
	secrets := testSecrets{}
	store, err := storage.OpenAt(t.TempDir(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id := uuid.NewString()
	tr := domain.Transfer{ID: id, Direction: domain.TransferReceive, State: "queued"}
	state := storage.ReceiveState{Manifest: domain.FileManifest{TransferID: "offer"}}
	if err = store.CreateReceive(tr, state); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateReceive(tr, state); err == nil {
		t.Fatal("duplicate receive inserted")
	}
	secrets["transfer-token:"+id] = "test-only"
	m, err := NewManager(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	got, err := m.StartReceive(context.Background(), ReceiveRequest{TransferID: id})
	if err != nil || got.ID != id || got.State != "interrupted" {
		t.Fatalf("existing receive not recovered: %+v, %v", got, err)
	}
	if len(m.Transfers()) != 1 {
		t.Fatal("recovery created another transfer")
	}
	if err = m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if err = m.Resume(id); err == nil {
		t.Fatal("cancelled transfer resumed")
	}
	if _, err = secrets.Get("transfer-token:" + id); err == nil {
		t.Fatal("resume credential retained")
	}
	if _, err = store.LoadReceiveState(context.Background(), id); err == nil {
		t.Fatal("resume state retained")
	}
	if m.Transfers()[0].State != "cancelled" {
		t.Fatal("cancellation not persisted")
	}
}
