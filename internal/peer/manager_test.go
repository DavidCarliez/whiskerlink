package peer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
	"github.com/DavidCarliez/whiskerlink/internal/storage"
	"github.com/DavidCarliez/whiskerlink/internal/transfer"
)

type memorySecrets struct {
	mu     sync.Mutex
	values map[string]string
	fail   bool
}

func (s *memorySecrets) Set(k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("credential store locked")
	}
	s.values[k] = v
	return nil
}
func (s *memorySecrets) Get(k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[k]
	if !ok {
		return "", storage.ErrSecretNotFound
	}
	return v, nil
}
func (s *memorySecrets) Delete(k string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, k)
	return nil
}

type fakeTransfers struct {
	mu         sync.Mutex
	manifest   domain.FileManifest
	receives   int
	stopped    int
	transfers  []domain.Transfer
	prepareErr error
}

func (f *fakeTransfers) StartPairedOffer(_ context.Context, _ []string, _, id, _ string) (domain.Session, domain.FileManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.manifest.TransferID = id
	return domain.Session{ID: "offer-session", Token: "offer-token"}, f.manifest, f.prepareErr
}
func (f *fakeTransfers) Inspect(context.Context, string) (domain.FileManifest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.manifest, nil
}
func (f *fakeTransfers) StartReceive(_ context.Context, r transfer.ReceiveRequest) (domain.Transfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.ExpectedManifest != transfer.ManifestDigest(f.manifest) {
		return domain.Transfer{}, errors.New("manifest changed")
	}
	f.receives++
	tr := domain.Transfer{ID: r.TransferID, State: "queued"}
	f.transfers = []domain.Transfer{tr}
	return tr, nil
}
func (f *fakeTransfers) StopOffer(string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped++
	return nil
}
func (f *fakeTransfers) Transfers() []domain.Transfer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Transfer(nil), f.transfers...)
}
func (f *fakeTransfers) Pause(string) error  { return nil }
func (f *fakeTransfers) Cancel(string) error { return nil }

func localManager(t *testing.T, name string, f *fakeTransfers) (*Manager, *storage.Store, *memorySecrets) {
	t.Helper()
	secrets := &memorySecrets{values: map[string]string{}}
	store, err := storage.OpenAt(t.TempDir(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m, err := New(store, f)
	if err != nil {
		t.Fatal(err)
	}
	configureLocal(m)
	if err = m.Enable(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m, store, secrets
}
func configureLocal(m *Manager) {
	m.checkToken = func(s string) error {
		if s == "" {
			return errors.New("missing token")
		}
		return nil
	}
	m.dial = func(ctx context.Context, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}
	m.startTransport = func() (string, func() error, error) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return "", nil, err
		}
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				go m.handle(c)
			}
		}()
		return l.Addr().String(), l.Close, nil
	}
}
func pair(t *testing.T, a, b *Manager) {
	t.Helper()
	invite, err := a.CreateInvite()
	if err != nil {
		t.Fatal(err)
	}
	d, err := b.JoinInvite(invite.URL)
	if err != nil {
		t.Fatal(err)
	}
	if d.Code == "" || d.Code != a.Snapshot().Devices[0].Code {
		t.Fatal("verification codes must match")
	}
	if err = a.Confirm(b.id); err != nil {
		t.Fatal(err)
	}
	a.tick(context.Background())
	if a.Snapshot().Devices[0].State == "paired" || b.Snapshot().Devices[0].State == "paired" {
		t.Fatal("one-sided approval established pairing")
	}
	if err = b.Confirm(a.id); err != nil {
		t.Fatal(err)
	}
	b.tick(context.Background())
	a.tick(context.Background())
	if a.Snapshot().Devices[0].State != "paired" || b.Snapshot().Devices[0].State != "paired" {
		t.Fatal("mutual confirmation did not pair")
	}
}
func TestPairingInboxAcceptanceAndUnpair(t *testing.T) {
	sum := sha256.Sum256([]byte("hello"))
	f := &fakeTransfers{manifest: domain.FileManifest{ProtocolVersion: 1, TransferID: "files", Label: "Hello", Files: []domain.FileEntry{{Path: "hello.txt", Size: 5, SHA256: hex.EncodeToString(sum[:])}}, TotalBytes: 5}}
	a, _, _ := localManager(t, "Laptop", f)
	b, _, _ := localManager(t, "Desktop", f)
	pair(t, a, b)
	d, err := a.Send(b.id, []string{"hello.txt"}, "Hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Snapshot().Inbox) != 0 {
		t.Fatal("queued request unexpectedly delivered without control communication")
	}
	a.tick(context.Background())
	a.tick(context.Background())
	if len(b.Snapshot().Inbox) != 1 || f.receives != 0 {
		t.Fatal("notification must be idempotent and must not write files")
	}
	if _, err = b.Accept(d.ID, t.TempDir(), []string{"hello.txt"}, "rename"); err == nil {
		t.Fatal("accept without inspection succeeded")
	}
	if _, err = b.Inspect(d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Accept(d.ID, t.TempDir(), nil, "rename"); err == nil {
		t.Fatal("empty selection accepted")
	}
	if _, err = b.Accept(d.ID, t.TempDir(), []string{"hello.txt"}, "rename"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Accept(d.ID, t.TempDir(), []string{"hello.txt"}, "rename"); err == nil {
		t.Fatal("duplicate acceptance succeeded")
	}
	f.mu.Lock()
	f.transfers[0].State = "completed"
	f.mu.Unlock()
	b.tick(context.Background())
	a.tick(context.Background())
	if a.Snapshot().Outbox[0].State != "completed" || f.stopped != 1 {
		t.Fatal("completion was not acknowledged or offer was not closed")
	}
	if err = b.Unpair(a.id); err != nil {
		t.Fatal(err)
	}
	if _, err = a.rpc(context.Background(), deviceRecord{Device: Device{ID: b.id}, Token: b.token}, message{Action: "status"}); err == nil {
		t.Fatal("unpaired key retained control access")
	}
}
func TestInviteSingleUseExpiryAndProof(t *testing.T) {
	f := &fakeTransfers{}
	a, _, _ := localManager(t, "A", f)
	b, _, _ := localManager(t, "B", f)
	c, _, _ := localManager(t, "C", f)
	invite, _ := a.CreateInvite()
	u, _ := url.Parse(invite.URL)
	q := u.Query()
	q.Set("secret", randomID())
	u.RawQuery = q.Encode()
	if _, err := b.JoinInvite(u.String()); err == nil {
		t.Fatal("wrong proof succeeded")
	}
	if len(a.Snapshot().Devices) != 0 {
		t.Fatal("wrong proof created a record")
	}
	if _, err := b.JoinInvite(invite.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := c.JoinInvite(invite.URL); err == nil {
		t.Fatal("used invite accepted a second device")
	}
	invite, _ = a.CreateInvite()
	a.mu.Lock()
	a.invite.expires = time.Now().Add(-time.Minute)
	a.mu.Unlock()
	if _, err := c.JoinInvite(invite.URL); err == nil {
		t.Fatal("host expiry not enforced")
	}
}
func TestCredentialsFailClosedAndMetadataPersists(t *testing.T) {
	f := &fakeTransfers{}
	a, store, secrets := localManager(t, "A", f)
	b, _, _ := localManager(t, "B", f)
	pair(t, a, b)
	d, err := a.Send(b.id, []string{filepath.Join(t.TempDir(), "queued.txt")}, "Queued offline")
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(store, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshot().Devices) != 1 || restored.Snapshot().Devices[0].Online || restored.Snapshot().Outbox[0].ID != d.ID {
		t.Fatal("pairing or queue metadata lost")
	}
	rows, _ := store.PeerRecords("paired-device")
	if len(rows) != 1 {
		t.Fatal("missing persisted device")
	}
	if bytes.Contains(rows[0], []byte(b.token)) {
		t.Fatal("device credentials leaked into metadata")
	}
	var saved deviceRecord
	if err = json.Unmarshal(rows[0], &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Token != "" {
		t.Fatal("capability persisted outside credential store")
	}
	secrets.mu.Lock()
	secrets.fail = true
	secrets.mu.Unlock()
	c, _, _ := localManager(t, "C", f)
	invite, _ := c.CreateInvite()
	if _, err = a.JoinInvite(invite.URL); err == nil {
		t.Fatal("pairing ignored credential-store failure")
	}
}
func TestChangedManifestRequiresInspectionAgain(t *testing.T) {
	f := &fakeTransfers{manifest: domain.FileManifest{ProtocolVersion: 1, TransferID: "files", Files: []domain.FileEntry{{Path: "one", Size: 1}}, TotalBytes: 1}}
	a, _, _ := localManager(t, "A", f)
	b, _, _ := localManager(t, "B", f)
	pair(t, a, b)
	d, _ := a.Send(b.id, []string{"one"}, "One")
	a.tick(context.Background())
	if _, err := b.Inspect(d.ID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.manifest.Files[0].Size = 2
	f.mu.Unlock()
	if _, err := b.Accept(d.ID, t.TempDir(), []string{"one"}, "rename"); err == nil {
		t.Fatal("changed manifest accepted")
	}
}

func TestAcceptRecoveryAndMissingIdentity(t *testing.T) {
	f := &fakeTransfers{manifest: domain.FileManifest{ProtocolVersion: 1, TransferID: "files", Files: []domain.FileEntry{{Path: "one", Size: 1}}, TotalBytes: 1}}
	a, _, _ := localManager(t, "A", f)
	b, store, secrets := localManager(t, "B", f)
	pair(t, a, b)
	d, _ := a.Send(b.id, []string{"one"}, "One")
	a.tick(context.Background())
	if _, err := b.Inspect(d.ID); err != nil {
		t.Fatal(err)
	}
	tr, err := b.Accept(d.ID, t.TempDir(), []string{"one"}, "rename")
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	record := b.inbox[d.ID]
	record.State = "accepting" // Simulate a crash before the final inbox metadata write.
	err = b.saveDelivery("inbox", record)
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := New(store, f)
	if err != nil {
		t.Fatal(err)
	}
	got := restored.Snapshot().Inbox[0]
	if got.TransferID != tr.ID || got.State != "queued" {
		t.Fatalf("acceptance not reconciled: %+v", got)
	}
	if _, err = restored.Accept(d.ID, t.TempDir(), []string{"one"}, "rename"); err == nil {
		t.Fatal("recovery allowed a duplicate download")
	}
	if f.receives != 1 {
		t.Fatal("duplicate receive created")
	}
	_ = secrets.Delete("paired-signing-key")
	configureLocal(restored)
	if err = restored.Enable("B"); err == nil {
		t.Fatal("missing identity was silently replaced")
	}
}

func TestTerminalHistoryDoesNotFillInbox(t *testing.T) {
	records := map[string]deliveryRecord{}
	for _, state := range []string{"completed", "declined", "cancelled", "pending", "paused"} {
		records[state] = deliveryRecord{Delivery: Delivery{State: state}}
	}
	if activeDeliveryCount(records) != 2 {
		t.Fatal("terminal history counted against queue capacity")
	}
}
