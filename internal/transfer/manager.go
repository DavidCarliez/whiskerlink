package transfer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tailscale/tailcat"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
	"github.com/DavidCarliez/whiskerlink/internal/storage"
	"github.com/DavidCarliez/whiskerlink/internal/tailnet"
)

type ReceiveRequest struct {
	Token       string   `json:"token"`
	Destination string   `json:"destination"`
	Selected    []string `json:"selected"`
	Collision   string   `json:"collision"`
}

type StartOfferRequest struct {
	Paths      []string `json:"paths"`
	Label      string   `json:"label"`
	Persistent bool     `json:"persistent"`
}

type receiveJob struct {
	token string
	state storage.ReceiveState
}

type offerRuntime struct {
	session domain.Session
	server  *tailcat.Server
	offer   *Offer
}

type Manager struct {
	mu        sync.RWMutex
	transfers map[string]domain.Transfer
	jobs      map[string]receiveJob
	cancels   map[string]context.CancelFunc
	offers    map[string]*offerRuntime
	queue     chan string
	store     *storage.Store
	identity  *tailnet.IdentityStore
	onChange  func()
	closed    chan struct{}
}

func NewManager(store *storage.Store, onChange func()) (*Manager, error) {
	m := &Manager{
		transfers: make(map[string]domain.Transfer), jobs: make(map[string]receiveJob),
		cancels: make(map[string]context.CancelFunc), offers: make(map[string]*offerRuntime),
		queue: make(chan string, 128), store: store, identity: tailnet.NewIdentityStore(store),
		onChange: onChange, closed: make(chan struct{}),
	}
	previous, err := store.ListTransfers(context.Background())
	if err != nil {
		return nil, err
	}
	for _, transfer := range previous {
		if transfer.State == "queued" || transfer.State == "connecting" || transfer.State == "transferring" || transfer.State == "verifying" {
			transfer.State = "interrupted"
			transfer.Error = "The application stopped before this transfer finished."
			transfer.UpdatedAt = time.Now()
			_ = store.UpsertTransfer(context.Background(), transfer)
		}
		m.transfers[transfer.ID] = transfer
	}
	go m.worker()
	return m, nil
}

func (m *Manager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

func (m *Manager) Transfers() []domain.Transfer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Transfer, 0, len(m.transfers))
	for _, transfer := range m.transfers {
		out = append(out, transfer)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

func (m *Manager) Sessions() []domain.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Session, 0, len(m.offers))
	for _, offer := range m.offers {
		out = append(out, offer.session)
	}
	return out
}

func (m *Manager) StartOffer(ctx context.Context, req StartOfferRequest) (domain.Session, error) {
	offer, err := BuildOffer(req.Paths, req.Label)
	if err != nil {
		return domain.Session{}, err
	}
	now := time.Now()
	transfer := domain.Transfer{
		ID: offer.Manifest.TransferID, Direction: domain.TransferSend, Label: offer.Manifest.Label,
		State: "preparing", FilesTotal: len(offer.Manifest.Files), BytesTotal: offer.Manifest.TotalBytes,
		CreatedAt: now, UpdatedAt: now,
	}
	m.setTransfer(transfer)
	offer.SetProgressCallback(func(bytes int64, completed int) {
		m.updateTransfer(transfer.ID, func(current *domain.Transfer) {
			current.BytesTransferred = bytes
			current.FilesCompleted = completed
			if completed == current.FilesTotal {
				current.State = "completed"
			} else {
				current.State = "transferring"
			}
		})
	})

	var server *tailcat.Server
	server = &tailcat.Server{Logf: discardLog, OnTCP: func(port uint16) func(net.Conn) {
		switch port {
		case ProtocolPort:
			return offer.Handle
		case 22:
			if offer.CompatibleDir == "" {
				return nil
			}
			return server.SSHConnHandler(tailcat.SSHOptions{Files: &tailcat.FileService{Dir: offer.CompatibleDir, Mode: tailcat.FileServeRO}})
		default:
			return nil
		}
	}}
	if req.Persistent {
		if err := m.identity.ConfigureServer(server); err != nil {
			_ = offer.Close()
			m.failTransfer(transfer.ID, err)
			return domain.Session{}, err
		}
	}
	if err := server.Start(); err != nil {
		_ = offer.Close()
		m.failTransfer(transfer.ID, err)
		return domain.Session{}, fmt.Errorf("start file offer: %w", err)
	}
	token := string(server.ConnBlob())
	if req.Persistent {
		if err := m.identity.SaveServer(server.Key, token); err != nil {
			_ = server.Close()
			_ = offer.Close()
			m.failTransfer(transfer.ID, err)
			return domain.Session{}, err
		}
	}
	session := domain.Session{
		ID: uuid.NewString(), Kind: domain.SessionFileOffer, Label: offer.Manifest.Label,
		State: "listening", Token: token, RemotePort: ProtocolPort, Transport: "waiting",
		Persistent: req.Persistent, CLICompatible: offer.Manifest.CLICompatible, CreatedAt: now,
	}
	m.mu.Lock()
	m.offers[session.ID] = &offerRuntime{session: session, server: server, offer: offer}
	transfer.State = "waiting"
	transfer.UpdatedAt = time.Now()
	m.transfers[transfer.ID] = transfer
	m.mu.Unlock()
	_ = m.store.UpsertTransfer(context.Background(), transfer)
	m.notify()
	return session, nil
}

func (m *Manager) Inspect(ctx context.Context, token string) (domain.FileManifest, error) {
	if _, err := tailcat.ParseConnBlob(tailcat.ConnBlob(token)); err != nil {
		return domain.FileManifest{}, fmt.Errorf("invalid Tailcat token: %w", err)
	}
	client := tailcat.NewClient(tailcat.ConnBlob(token))
	client.Logf = discardLog
	defer client.Close()
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := client.DialTCPPort(dialCtx, ProtocolPort)
	if err != nil {
		return domain.FileManifest{}, fmt.Errorf("connect to file offer: %w", err)
	}
	defer conn.Close()
	if err := writeJSONLine(conn, request{Version: ProtocolVersion, Action: "manifest"}); err != nil {
		return domain.FileManifest{}, err
	}
	var res response
	if err := readJSONLine(bufio.NewReader(conn), &res); err != nil {
		return domain.FileManifest{}, fmt.Errorf("read file offer: %w", err)
	}
	if !res.OK || res.Manifest == nil {
		return domain.FileManifest{}, errors.New(res.Error)
	}
	if err := validateManifest(*res.Manifest); err != nil {
		return domain.FileManifest{}, err
	}
	return *res.Manifest, nil
}

func validateManifest(manifest domain.FileManifest) error {
	if manifest.ProtocolVersion != ProtocolVersion || manifest.TransferID == "" {
		return errors.New("unsupported file offer")
	}
	seen := make(map[string]bool)
	var total int64
	for _, entry := range manifest.Files {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(entry.Path)))
		if clean != entry.Path || clean == "." || strings.HasPrefix(clean, "../") || filepath.IsAbs(entry.Path) {
			return fmt.Errorf("file offer contains unsafe path %q", entry.Path)
		}
		if seen[entry.Path] || entry.Size < 0 || len(entry.SHA256) != sha256.Size*2 {
			return errors.New("file offer contains invalid metadata")
		}
		seen[entry.Path] = true
		total += entry.Size
		if total < 0 {
			return errors.New("file offer size overflow")
		}
	}
	if total != manifest.TotalBytes {
		return errors.New("file offer total does not match its files")
	}
	return nil
}

func (m *Manager) StartReceive(ctx context.Context, req ReceiveRequest) (domain.Transfer, error) {
	if req.Destination == "" {
		return domain.Transfer{}, errors.New("choose a destination folder")
	}
	if req.Collision == "" {
		req.Collision = "rename"
	}
	if req.Collision != "rename" && req.Collision != "overwrite" && req.Collision != "skip" {
		return domain.Transfer{}, errors.New("invalid collision policy")
	}
	if err := os.MkdirAll(req.Destination, 0o755); err != nil {
		return domain.Transfer{}, fmt.Errorf("create destination: %w", err)
	}
	manifest, err := m.Inspect(ctx, req.Token)
	if err != nil {
		return domain.Transfer{}, err
	}
	selected := selectedEntries(manifest, req.Selected)
	if len(selected) == 0 {
		return domain.Transfer{}, errors.New("select at least one offered file")
	}
	var total int64
	for _, entry := range selected {
		total += entry.Size
	}
	now := time.Now()
	transfer := domain.Transfer{
		ID: uuid.NewString(), Direction: domain.TransferReceive, Label: manifest.Label,
		State: "queued", Destination: req.Destination, FilesTotal: len(selected), BytesTotal: total,
		CreatedAt: now, UpdatedAt: now,
	}
	state := storage.ReceiveState{Manifest: manifest, Selected: req.Selected, Collision: req.Collision}
	if err := m.store.UpsertTransfer(ctx, transfer); err != nil {
		return domain.Transfer{}, err
	}
	if err := m.store.SaveReceiveState(ctx, transfer.ID, state); err != nil {
		return domain.Transfer{}, err
	}
	_ = m.store.Secrets.Set("transfer-token:"+transfer.ID, req.Token)
	m.mu.Lock()
	m.transfers[transfer.ID] = transfer
	m.jobs[transfer.ID] = receiveJob{token: req.Token, state: state}
	m.mu.Unlock()
	m.notify()
	m.queue <- transfer.ID
	return transfer, nil
}

func selectedEntries(manifest domain.FileManifest, selected []string) []domain.FileEntry {
	if len(selected) == 0 {
		return append([]domain.FileEntry(nil), manifest.Files...)
	}
	wanted := make(map[string]bool, len(selected))
	for _, path := range selected {
		wanted[path] = true
	}
	out := make([]domain.FileEntry, 0, len(selected))
	for _, entry := range manifest.Files {
		if wanted[entry.Path] {
			out = append(out, entry)
		}
	}
	return out
}

func (m *Manager) worker() {
	for {
		select {
		case id := <-m.queue:
			m.runReceive(id)
		case <-m.closed:
			return
		}
	}
}

func (m *Manager) runReceive(id string) {
	m.mu.Lock()
	job, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancels[id] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, id)
		m.mu.Unlock()
	}()

	m.updateTransfer(id, func(t *domain.Transfer) { t.State = "connecting"; t.Error = "" })
	client := tailcat.NewClient(tailcat.ConnBlob(job.token))
	client.Logf = discardLog
	defer client.Close()
	entries := selectedEntries(job.state.Manifest, job.state.Selected)
	completed := make(map[string]bool, len(job.state.Completed))
	for _, path := range job.state.Completed {
		completed[path] = true
	}
	for _, entry := range entries {
		if completed[entry.Path] {
			continue
		}
		if ctx.Err() != nil {
			m.updateTransfer(id, func(t *domain.Transfer) { t.State = "paused"; t.Error = "" })
			return
		}
		if err := m.receiveFile(ctx, client, id, job, entry); err != nil {
			if ctx.Err() != nil {
				m.updateTransfer(id, func(t *domain.Transfer) { t.State = "paused"; t.Error = "" })
			} else {
				m.updateTransfer(id, func(t *domain.Transfer) { t.State = "interrupted"; t.Error = err.Error() })
			}
			return
		}
		job.state.Completed = append(job.state.Completed, entry.Path)
		completed[entry.Path] = true
		if err := m.store.SaveReceiveState(context.Background(), id, job.state); err != nil {
			m.updateTransfer(id, func(t *domain.Transfer) { t.State = "interrupted"; t.Error = err.Error() })
			return
		}
		m.mu.Lock()
		m.jobs[id] = job
		m.mu.Unlock()
		m.updateTransfer(id, func(t *domain.Transfer) { t.FilesCompleted = len(job.state.Completed) })
	}
	m.updateTransfer(id, func(t *domain.Transfer) { t.State = "completed"; t.Error = "" })
	_ = m.store.Secrets.Delete("transfer-token:" + id)
	_ = m.store.DeleteReceiveState(context.Background(), id)
	_ = os.RemoveAll(filepath.Join(jobDestination(m, id), ".whiskerlink-partials", id))
}

func jobDestination(m *Manager, id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.transfers[id].Destination
}

func (m *Manager) receiveFile(ctx context.Context, client *tailcat.Client, transferID string, job receiveJob, entry domain.FileEntry) error {
	destination := jobDestination(m, transferID)
	target, err := safeDestination(destination, entry.Path)
	if err != nil {
		return err
	}
	if _, err := os.Stat(target); err == nil {
		switch job.state.Collision {
		case "skip":
			return nil
		case "rename":
			target = availableName(target)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect destination: %w", err)
	}
	partial, err := safeDestination(filepath.Join(destination, ".whiskerlink-partials", transferID), entry.Path+".part")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open partial file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	offset := info.Size()
	if offset > entry.Size {
		if err := file.Truncate(0); err != nil {
			file.Close()
			return err
		}
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		file.Close()
		return err
	}

	conn, err := client.DialTCPPort(ctx, ProtocolPort)
	if err != nil {
		file.Close()
		return fmt.Errorf("connect for %s: %w", entry.Path, err)
	}
	reader := bufio.NewReader(conn)
	if err := writeJSONLine(conn, request{Version: ProtocolVersion, Action: "get", TransferID: job.state.Manifest.TransferID, Path: entry.Path, Offset: offset}); err != nil {
		conn.Close()
		file.Close()
		return err
	}
	var res response
	if err := readJSONLine(reader, &res); err != nil {
		conn.Close()
		file.Close()
		return fmt.Errorf("read transfer response: %w", err)
	}
	if !res.OK || res.Offset != offset || res.Size != entry.Size {
		conn.Close()
		file.Close()
		return fmt.Errorf("sender rejected %s: %s", entry.Path, res.Error)
	}
	m.updateTransfer(transferID, func(t *domain.Transfer) { t.State = "transferring" })
	writer := &progressWriter{writer: file, onWrite: func(n int) {
		m.updateTransfer(transferID, func(t *domain.Transfer) { t.BytesTransferred += int64(n) })
	}}
	_, copyErr := io.CopyN(writer, reader, entry.Size-offset)
	_ = conn.Close()
	if syncErr := file.Sync(); copyErr == nil {
		copyErr = syncErr
	}
	if closeErr := file.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return fmt.Errorf("receive %s: %w", entry.Path, copyErr)
	}
	m.updateTransfer(transferID, func(t *domain.Transfer) { t.State = "verifying" })
	hash, err := hashFile(partial)
	if err != nil {
		return err
	}
	if !strings.EqualFold(hash, entry.SHA256) {
		_ = os.Remove(partial)
		return fmt.Errorf("integrity verification failed for %s", entry.Path)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if job.state.Collision == "overwrite" {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(partial, target); err != nil {
		return fmt.Errorf("finish %s: %w", entry.Path, err)
	}
	_ = os.Chtimes(target, time.Now(), time.Unix(0, entry.ModTimeUnixNano))
	return nil
}

type progressWriter struct {
	writer  io.Writer
	onWrite func(int)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.onWrite(n)
	}
	return n, err
}

func safeDestination(root, relative string) (string, error) {
	if filepath.IsAbs(relative) || filepath.VolumeName(relative) != "" {
		return "", fmt.Errorf("unsafe destination path %q", relative)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe destination path %q", relative)
	}
	return target, nil
}

func availableName(path string) string {
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}

func (m *Manager) Pause(id string) error {
	m.mu.RLock()
	cancel := m.cancels[id]
	m.mu.RUnlock()
	if cancel == nil {
		return errors.New("transfer is not active")
	}
	cancel()
	return nil
}

func (m *Manager) Resume(id string) error {
	m.mu.Lock()
	transfer, ok := m.transfers[id]
	_, active := m.cancels[id]
	job, loaded := m.jobs[id]
	m.mu.Unlock()
	if !ok || active || (transfer.State != "paused" && transfer.State != "interrupted") {
		return errors.New("transfer cannot be resumed")
	}
	if !loaded {
		state, err := m.store.LoadReceiveState(context.Background(), id)
		if err != nil {
			return err
		}
		token, err := m.store.Secrets.Get("transfer-token:" + id)
		if err != nil {
			return errors.New("this transfer token was not persisted; paste the invite and start receiving again")
		}
		job = receiveJob{token: token, state: state}
		m.mu.Lock()
		m.jobs[id] = job
		m.mu.Unlock()
	}
	m.updateTransfer(id, func(t *domain.Transfer) { t.State = "queued"; t.Error = "" })
	m.queue <- id
	return nil
}

func (m *Manager) StopOffer(sessionID string) error {
	m.mu.Lock()
	offer := m.offers[sessionID]
	delete(m.offers, sessionID)
	m.mu.Unlock()
	if offer == nil {
		return errors.New("file offer not found")
	}
	serverErr := offer.server.Close()
	cleanupErr := offer.offer.Close()
	m.notify()
	if serverErr != nil {
		return serverErr
	}
	return cleanupErr
}

func (m *Manager) setTransfer(transfer domain.Transfer) {
	m.mu.Lock()
	m.transfers[transfer.ID] = transfer
	m.mu.Unlock()
	_ = m.store.UpsertTransfer(context.Background(), transfer)
	m.notify()
}

func (m *Manager) updateTransfer(id string, update func(*domain.Transfer)) {
	m.mu.Lock()
	transfer, ok := m.transfers[id]
	if ok {
		update(&transfer)
		transfer.UpdatedAt = time.Now()
		m.transfers[id] = transfer
	}
	m.mu.Unlock()
	if ok {
		_ = m.store.UpsertTransfer(context.Background(), transfer)
		m.notify()
	}
}

func (m *Manager) failTransfer(id string, err error) {
	m.updateTransfer(id, func(t *domain.Transfer) { t.State = "failed"; t.Error = err.Error() })
}

func (m *Manager) Close() {
	close(m.closed)
	m.mu.Lock()
	for _, cancel := range m.cancels {
		cancel()
	}
	offers := m.offers
	m.offers = make(map[string]*offerRuntime)
	m.mu.Unlock()
	for _, offer := range offers {
		_ = offer.server.Close()
		_ = offer.offer.Close()
	}
}

func discardLog(string, ...any) {}
