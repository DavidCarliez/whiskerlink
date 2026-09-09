package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
	"github.com/DavidCarliez/whiskerlink/internal/storage"
	"github.com/DavidCarliez/whiskerlink/internal/tailnet"
	"github.com/DavidCarliez/whiskerlink/internal/transfer"
	"github.com/google/uuid"
	"github.com/tailscale/tailcat"
)

type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Code      string    `json:"code,omitempty"`
	Confirmed bool      `json:"confirmed"`
	Online    bool      `json:"online"`
	LastSeen  time.Time `json:"lastSeen"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type deviceRecord struct {
	Device
	Token           string `json:"-"`
	RemoteConfirmed bool   `json:"remoteConfirmed"`
}
type Delivery struct {
	ID         string    `json:"id"`
	DeviceID   string    `json:"deviceId"`
	DeviceName string    `json:"deviceName"`
	Label      string    `json:"label"`
	State      string    `json:"state"`
	Error      string    `json:"error,omitempty"`
	TransferID string    `json:"transferId,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}
type deliveryRecord struct {
	Delivery
	Paths     []string `json:"paths,omitempty"`
	Digest    string   `json:"digest,omitempty"`
	Token     string   `json:"-"`
	SessionID string   `json:"-"`
}
type Snapshot struct {
	Enabled bool       `json:"enabled"`
	Name    string     `json:"name"`
	Error   string     `json:"error,omitempty"`
	Devices []Device   `json:"devices"`
	Inbox   []Delivery `json:"inbox"`
	Outbox  []Delivery `json:"outbox"`
}
type Invitation struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type invitation struct {
	id, secret string
	expires    time.Time
}
type transferEngine interface {
	StartPairedOffer(context.Context, []string, string, string, string) (domain.Session, domain.FileManifest, error)
	Inspect(context.Context, string) (domain.FileManifest, error)
	StartReceive(context.Context, transfer.ReceiveRequest) (domain.Transfer, error)
	StopOffer(string) error
	Transfers() []domain.Transfer
	Pause(string) error
	Cancel(string) error
}
type Manager struct {
	mu                         sync.Mutex
	operations                 sync.Mutex
	store                      *storage.Store
	transfers                  transferEngine
	devices                    map[string]deviceRecord
	inbox, outbox              map[string]deliveryRecord
	key                        ed25519.PrivateKey
	id, name, token, lastError string
	enabled                    bool
	invite                     invitation
	stopServer                 func() error
	cancel                     context.CancelFunc
	wg                         sync.WaitGroup
	handlers                   sync.WaitGroup
	closing                    bool
	slots                      chan struct{}
	dial                       func(context.Context, string) (net.Conn, error)
	startTransport             func() (string, func() error, error)
	checkToken                 func(string) error
}

func New(store *storage.Store, transfers transferEngine) (*Manager, error) {
	m := &Manager{store: store, transfers: transfers, devices: map[string]deviceRecord{}, inbox: map[string]deliveryRecord{}, outbox: map[string]deliveryRecord{}, slots: make(chan struct{}, 16), dial: dialTailcat, checkToken: validToken}
	m.startTransport = m.startTailcat
	m.name, _ = os.Hostname()
	if m.name == "" {
		m.name = "My computer"
	}
	for _, kind := range []string{"paired-device", "inbox", "outbox"} {
		rows, err := store.PeerRecords(kind)
		if err != nil {
			return nil, err
		}
		for _, b := range rows {
			if kind == "paired-device" {
				var d deviceRecord
				if err = json.Unmarshal(b, &d); err != nil {
					return nil, err
				}
				d.Online = false
				m.devices[d.ID] = d
			} else {
				var d deliveryRecord
				if err = json.Unmarshal(b, &d); err != nil {
					return nil, err
				}
				if d.State == "accepting" {
					d.State = "pending"
					for _, tr := range transfers.Transfers() {
						if tr.ID == d.TransferID {
							d.State = tr.State
						}
					}
				}
				if kind == "inbox" {
					m.inbox[d.ID] = d
				} else {
					m.outbox[d.ID] = d
				}
			}
		}
	}
	rows, err := store.PeerRecords("peer-settings")
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		var s Snapshot
		if err = json.Unmarshal(rows[0], &s); err != nil {
			return nil, err
		}
		m.name = s.Name
	}
	return m, nil
}

// Restore starts the endpoint only after this user has previously enabled pairing.
func (m *Manager) Restore() {
	rows, err := m.store.PeerRecords("peer-settings")
	if err == nil && len(rows) > 0 {
		var s Snapshot
		if json.Unmarshal(rows[0], &s) == nil && s.Enabled {
			if err = m.Enable(s.Name); err != nil {
				m.mu.Lock()
				m.lastError = err.Error()
				m.mu.Unlock()
			}
		}
	}
}
func (m *Manager) startTailcat() (string, func() error, error) {
	if len(m.devices) > 0 {
		for _, name := range []string{"identity:paired-control", "identity-token:paired-control"} {
			if _, err := m.store.Secrets.Get(name); err != nil {
				return "", nil, fmt.Errorf("paired control identity unavailable; restore OS credentials: %w", err)
			}
		}
	}
	server := &tailcat.Server{Logf: func(string, ...any) {}, OnTCP: func(port uint16) func(net.Conn) {
		if port == controlPort {
			return m.handle
		}
		return nil
	}}
	identity := tailnet.NewNamedIdentityStore(m.store, "paired-control")
	if err := identity.ConfigureServer(server); err != nil {
		return "", nil, err
	}
	if err := server.Start(); err != nil {
		return "", nil, err
	}
	token := string(server.ConnBlob())
	if err := identity.SaveServer(server.Key, token); err != nil {
		_ = server.Close()
		return "", nil, err
	}
	return token, server.Close, nil
}
func (m *Manager) Enable(name string) error {
	m.operations.Lock()
	defer m.operations.Unlock()
	name = strings.TrimSpace(name)
	if len(name) == 0 || len(name) > 80 {
		return errors.New("device name must contain 1 to 80 characters")
	}
	m.mu.Lock()
	already := m.enabled
	m.mu.Unlock()
	if !already {
		encoded, err := m.store.Secrets.Get("paired-signing-key")
		var key ed25519.PrivateKey
		if errors.Is(err, storage.ErrSecretNotFound) {
			if len(m.devices) > 0 {
				return errors.New("pairing identity is missing; restore the OS credentials before reconnecting paired devices")
			}
			_, key, err = ed25519.GenerateKey(rand.Reader)
			if err == nil {
				err = m.store.Secrets.Set("paired-signing-key", hex.EncodeToString(key))
			}
		} else if err == nil {
			var b []byte
			b, err = hex.DecodeString(encoded)
			key = ed25519.PrivateKey(b)
		}
		if err != nil {
			return fmt.Errorf("load paired device credentials: %w", err)
		}
		if len(key) != ed25519.PrivateKeySize {
			return errors.New("invalid stored device identity")
		}
		m.mu.Lock()
		m.key = key
		m.id = hex.EncodeToString(key.Public().(ed25519.PublicKey))
		m.mu.Unlock()
		token, stop, err := m.startTransport()
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.token = token
		m.stopServer = stop
		m.mu.Unlock()
	}
	if err := m.store.SavePeerRecord("peer-settings", "local", Snapshot{Enabled: true, Name: name}); err != nil {
		if !already {
			_ = m.stopServer()
		}
		return err
	}
	m.mu.Lock()
	m.enabled = true
	m.name = name
	m.lastError = ""
	m.mu.Unlock()
	if !already {
		ctx, cancel := context.WithCancel(context.Background())
		m.mu.Lock()
		m.cancel = cancel
		m.mu.Unlock()
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			ticker := time.NewTicker(4 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					m.tick(ctx)
				}
			}
		}()
	}
	return nil
}
func (m *Manager) Close() {
	m.mu.Lock()
	m.closing = true
	m.enabled = false
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.wg.Wait()
	m.mu.Lock()
	stop := m.stopServer
	m.enabled = false
	m.mu.Unlock()
	if stop != nil {
		_ = stop()
	}
	m.handlers.Wait()
}
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Snapshot{Enabled: m.enabled, Name: m.name, Error: m.lastError, Devices: []Device{}, Inbox: []Delivery{}, Outbox: []Delivery{}}
	for _, p := range m.devices {
		d := p.Device
		d.Online = d.Online && time.Since(d.LastSeen) < 30*time.Second
		s.Devices = append(s.Devices, d)
	}
	for _, d := range m.inbox {
		s.Inbox = append(s.Inbox, d.Delivery)
	}
	for _, d := range m.outbox {
		s.Outbox = append(s.Outbox, d.Delivery)
	}
	sort.Slice(s.Devices, func(i, j int) bool { return s.Devices[i].Name < s.Devices[j].Name })
	sort.Slice(s.Inbox, func(i, j int) bool { return s.Inbox[i].CreatedAt.After(s.Inbox[j].CreatedAt) })
	sort.Slice(s.Outbox, func(i, j int) bool { return s.Outbox[i].CreatedAt.After(s.Outbox[j].CreatedAt) })
	return s
}
func (m *Manager) CreateInvite() (Invitation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled {
		return Invitation{}, errors.New("enable paired devices first")
	}
	m.invite = invitation{id: randomID(), secret: randomID(), expires: time.Now().Add(10 * time.Minute)}
	q := url.Values{"v": {"1"}, "token": {m.token}, "key": {m.id}, "id": {m.invite.id}, "secret": {m.invite.secret}, "name": {m.name}, "expires": {m.invite.expires.UTC().Format(time.RFC3339)}}
	return Invitation{URL: "whiskerlink://pair?" + q.Encode(), ExpiresAt: m.invite.expires}, nil
}
func pairingProof(secret, inviter, joiner, token, name, id, targetHash, expires string) string {
	b, _ := json.Marshal([]string{"whiskerlink-pair-v1", inviter, joiner, token, name, id, targetHash, expires})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(b)
	return hex.EncodeToString(mac.Sum(nil))
}
func (m *Manager) JoinInvite(value string) (Device, error) {
	m.operations.Lock()
	defer m.operations.Unlock()
	if len(value) > 4096 {
		return Device{}, errors.New("pairing invite too large")
	}
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Scheme != "whiskerlink" || u.Host != "pair" || u.Path != "" {
		return Device{}, errors.New("invalid pairing invite")
	}
	q := u.Query()
	expires, err := time.Parse(time.RFC3339, q.Get("expires"))
	if err != nil || time.Now().After(expires) || q.Get("v") != "1" || len(q.Get("secret")) != 64 || len(q.Get("id")) != 64 {
		return Device{}, errors.New("pairing invite expired or unsupported")
	}
	if _, err = publicKey(q.Get("key")); err != nil {
		return Device{}, err
	}
	if err = m.checkToken(q.Get("token")); err != nil {
		return Device{}, err
	}
	m.mu.Lock()
	if !m.enabled {
		m.mu.Unlock()
		return Device{}, errors.New("enable paired devices first")
	}
	id, token, name := m.id, m.token, m.name
	_, exists := m.devices[q.Get("key")]
	m.mu.Unlock()
	if id == q.Get("key") || exists {
		return Device{}, errors.New("device is already paired or pending; remove it before pairing again")
	}
	p := deviceRecord{Device: Device{ID: q.Get("key"), Name: q.Get("name"), State: "pending", ExpiresAt: expires}, Token: q.Get("token")}
	req := message{Action: "join", Name: name, Token: token, SenderID: id, TargetTokenHash: tokenHash(p.Token), InviteExpires: q.Get("expires"), InviteID: q.Get("id"), Proof: pairingProof(q.Get("secret"), p.ID, id, token, name, q.Get("id"), tokenHash(p.Token), q.Get("expires"))}
	res, err := m.rpc(context.Background(), p, req)
	if err != nil {
		return Device{}, err
	}
	p.Name = res.Name
	p.Code = res.Code
	m.mu.Lock()
	defer m.mu.Unlock()
	if err = m.saveDevice(p); err != nil {
		return Device{}, err
	}
	return p.Device, nil
}

// saveDevice is called with mu held, so disk and in-memory authorization change together.
func (m *Manager) saveDevice(p deviceRecord) error {
	if p.Token != "" {
		if err := m.store.Secrets.Set("paired-token:"+p.ID, p.Token); err != nil {
			return err
		}
		p.Token = ""
	}
	if err := m.store.SavePeerRecord("paired-device", p.ID, p); err != nil {
		return err
	}
	m.devices[p.ID] = p
	return nil
}
func (m *Manager) Confirm(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.devices[id]
	if !ok || p.State != "pending" || time.Now().After(p.ExpiresAt) {
		return errors.New("pairing request expired or missing")
	}
	p.Confirmed = true
	if p.RemoteConfirmed {
		p.State = "paired"
		p.Code = ""
	}
	return m.saveDevice(p)
}
func (m *Manager) dispatch(id string, req message, pairTranscript []byte) (message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled {
		return message{}, errors.New("paired devices disabled")
	}
	p, exists := m.devices[id]
	if req.Action == "join" {
		if exists || id == m.id || len(m.devices) >= 32 || time.Now().After(m.invite.expires) || req.InviteID != m.invite.id || len(req.Name) == 0 || len(req.Name) > 80 {
			return message{}, errors.New("pairing invite unavailable")
		}
		if req.TargetTokenHash != tokenHash(m.token) || req.SenderID != id || req.InviteExpires != m.invite.expires.UTC().Format(time.RFC3339) {
			return message{}, errors.New("pairing endpoint does not match the invitation")
		}
		proof := pairingProof(m.invite.secret, m.id, id, req.Token, req.Name, req.InviteID, req.TargetTokenHash, req.InviteExpires)
		if !hmac.Equal([]byte(proof), []byte(req.Proof)) {
			return message{}, errors.New("pairing proof rejected")
		}
		if err := m.checkToken(req.Token); err != nil {
			return message{}, err
		}
		code := verificationCode(pairTranscript)
		p = deviceRecord{Device: Device{ID: id, Name: req.Name, State: "pending", Code: code, ExpiresAt: m.invite.expires}, Token: req.Token}
		if err := m.saveDevice(p); err != nil {
			return message{}, err
		}
		m.invite = invitation{}
		return message{Name: m.name, Code: code}, nil
	}
	if !exists || (p.State != "paired" && time.Now().After(p.ExpiresAt)) {
		return message{}, errors.New("device is not paired")
	}
	if req.Action == "confirm" || req.Action == "status" {
		if req.Action == "confirm" {
			p.RemoteConfirmed = true
			if p.Confirmed {
				p.State = "paired"
				p.Code = ""
			}
		}
		p.Online = true
		p.LastSeen = time.Now()
		if err := m.saveDevice(p); err != nil {
			return message{}, err
		}
		return message{State: p.State, Confirmed: p.Confirmed}, nil
	}
	if p.State != "paired" {
		return message{}, errors.New("confirm the verification code on both computers first")
	}
	switch req.Action {
	case "offer":
		if _, err := uuid.Parse(req.ID); err != nil {
			return message{}, errors.New("invalid request id")
		}
		if len(req.Label) > 120 {
			return message{}, errors.New("label too long")
		}
		if err := m.checkToken(req.Token); err != nil {
			return message{}, err
		}
		if d, ok := m.inbox[req.ID]; ok {
			if d.DeviceID != id {
				return message{}, errors.New("request belongs to another device")
			}
			return message{State: d.State}, nil
		}
		if activeDeliveryCount(m.inbox) >= 200 {
			return message{}, errors.New("inbox has 200 unfinished requests; receive or decline them first")
		}
		d := deliveryRecord{Delivery: Delivery{ID: req.ID, DeviceID: id, DeviceName: p.Name, Label: req.Label, State: "pending", CreatedAt: time.Now()}, Token: req.Token}
		if err := m.store.Secrets.Set("inbox-token:"+d.ID, d.Token); err != nil {
			return message{}, err
		}
		if err := m.saveDelivery("inbox", d); err != nil {
			return message{}, err
		}
		return message{State: "pending"}, nil
	case "delivery-status":
		d, ok := m.inbox[req.ID]
		if !ok || d.DeviceID != id {
			return message{}, errors.New("request not found")
		}
		return message{State: d.State}, nil
	default:
		return message{}, errors.New("unsupported control action")
	}
}
func (m *Manager) saveDelivery(kind string, d deliveryRecord) error {
	if err := m.store.SavePeerRecord(kind, d.ID, d); err != nil {
		return err
	}
	if kind == "inbox" {
		m.inbox[d.ID] = d
	} else {
		m.outbox[d.ID] = d
	}
	return nil
}

func (m *Manager) Send(id string, paths []string, label string) (Delivery, error) {
	if len(paths) == 0 || len(paths) > 100 || len(label) > 120 {
		return Delivery{}, errors.New("choose files or a folder and a label of at most 120 characters")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.devices[id]
	if !m.enabled || !ok || p.State != "paired" {
		return Delivery{}, errors.New("choose a paired device")
	}
	if activeDeliveryCount(m.outbox) >= 200 {
		return Delivery{}, errors.New("outbox has 200 unfinished requests; wait for recipients to receive or decline them")
	}
	if label == "" {
		label = "Files from " + m.name
	}
	d := deliveryRecord{Delivery: Delivery{ID: uuid.NewString(), DeviceID: id, DeviceName: p.Name, Label: label, State: "queued", CreatedAt: time.Now()}, Paths: append([]string(nil), paths...)}
	if err := m.saveDelivery("outbox", d); err != nil {
		return Delivery{}, err
	}
	return d.Delivery, nil
}

func (m *Manager) Inspect(id string) (domain.FileManifest, error) {
	m.operations.Lock()
	defer m.operations.Unlock()
	m.mu.Lock()
	d, ok := m.inbox[id]
	p := m.devices[d.DeviceID]
	m.mu.Unlock()
	if !ok || d.State != "pending" || p.State != "paired" {
		return domain.FileManifest{}, errors.New("incoming request is no longer available")
	}
	token, err := m.store.Secrets.Get("inbox-token:" + id)
	if err != nil {
		return domain.FileManifest{}, err
	}
	manifest, err := m.transfers.Inspect(context.Background(), token)
	if err != nil {
		return manifest, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d.Digest = transfer.ManifestDigest(manifest)
	err = m.saveDelivery("inbox", d)
	return manifest, err
}
func (m *Manager) Accept(id, destination string, selected []string, collision string) (domain.Transfer, error) {
	m.operations.Lock()
	defer m.operations.Unlock()
	m.mu.Lock()
	d, ok := m.inbox[id]
	p := m.devices[d.DeviceID]
	m.mu.Unlock()
	if !ok || d.State != "pending" || d.Digest == "" || p.State != "paired" || len(selected) == 0 {
		return domain.Transfer{}, errors.New("inspect the incoming request and select files first")
	}
	token, err := m.store.Secrets.Get("inbox-token:" + id)
	if err != nil {
		return domain.Transfer{}, err
	}
	if d.TransferID == "" {
		d.TransferID = uuid.NewString()
	}
	d.State = "accepting"
	m.mu.Lock()
	err = m.saveDelivery("inbox", d)
	m.mu.Unlock()
	if err != nil {
		return domain.Transfer{}, err
	}
	tr, err := m.transfers.StartReceive(context.Background(), transfer.ReceiveRequest{TransferID: d.TransferID, Token: token, Destination: destination, Selected: selected, Collision: collision, ExpectedManifest: d.Digest})
	if err != nil {
		m.mu.Lock()
		d.State = "pending"
		saveErr := m.saveDelivery("inbox", d)
		m.mu.Unlock()
		return tr, errors.Join(err, saveErr)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	d.State = "accepted"
	d.TransferID = tr.ID
	err = m.saveDelivery("inbox", d)
	return tr, err
}
func (m *Manager) Decline(id string) error {
	m.operations.Lock()
	defer m.operations.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.inbox[id]
	if !ok || d.State != "pending" {
		return errors.New("request is no longer pending")
	}
	d.State = "declined"
	if err := m.saveDelivery("inbox", d); err != nil {
		return err
	}
	return m.deleteSecret("inbox-token:" + id)
}
func (m *Manager) Unpair(id string) error {
	m.operations.Lock()
	defer m.operations.Unlock()
	m.mu.Lock()
	if err := m.store.DeletePeerRecord("paired-device", id); err != nil {
		m.mu.Unlock()
		return err
	}
	delete(m.devices, id)
	var cleanupErrors []error
	stops := []string{}
	pauses := []string{}
	for _, d := range m.outbox {
		if d.DeviceID == id {
			if d.SessionID != "" {
				stops = append(stops, d.SessionID)
			}
			d.State = "cancelled"
			d.SessionID = ""
			cleanupErrors = append(cleanupErrors, m.saveDelivery("outbox", d))
			cleanupErrors = append(cleanupErrors, m.forgetOffer(d.ID))
		}
	}
	for _, d := range m.inbox {
		if d.DeviceID == id {
			if d.TransferID != "" {
				pauses = append(pauses, d.TransferID)
			}
			if d.State != "completed" {
				d.State = "cancelled"
				cleanupErrors = append(cleanupErrors, m.saveDelivery("inbox", d))
			}
			cleanupErrors = append(cleanupErrors, m.deleteSecret("inbox-token:"+d.ID))
		}
	}
	m.mu.Unlock()
	for _, session := range stops {
		cleanupErrors = append(cleanupErrors, m.transfers.StopOffer(session))
	}
	for _, tr := range pauses {
		cleanupErrors = append(cleanupErrors, m.transfers.Cancel(tr))
	}
	cleanupErrors = append(cleanupErrors, m.deleteSecret("paired-token:"+id))
	return errors.Join(cleanupErrors...)
}

func (m *Manager) deleteSecret(name string) error {
	err := m.store.Secrets.Delete(name)
	if errors.Is(err, storage.ErrSecretNotFound) {
		return nil
	}
	return err
}

func activeDeliveryCount(records map[string]deliveryRecord) int {
	n := 0
	for _, d := range records {
		if d.State != "completed" && d.State != "declined" && d.State != "cancelled" {
			n++
		}
	}
	return n
}
func (m *Manager) forgetOffer(id string) error {
	return errors.Join(m.deleteSecret("identity:paired-offer:"+id), m.deleteSecret("identity-token:paired-offer:"+id))
}

func (m *Manager) tick(ctx context.Context) {
	m.operations.Lock()
	defer m.operations.Unlock()
	m.mu.Lock()
	peers := []deviceRecord{}
	for _, p := range m.devices {
		if p.State == "pending" && time.Now().After(p.ExpiresAt) {
			if err := m.store.DeletePeerRecord("paired-device", p.ID); err != nil {
				m.lastError = err.Error()
				continue
			}
			delete(m.devices, p.ID)
			if err := m.deleteSecret("paired-token:" + p.ID); err != nil {
				m.lastError = err.Error()
			}
			continue
		}
		peers = append(peers, p)
	}
	m.mu.Unlock()
	for _, p := range peers {
		if ctx.Err() != nil {
			return
		}
		token, err := m.store.Secrets.Get("paired-token:" + p.ID)
		if err != nil {
			continue
		}
		p.Token = token
		action := "status"
		if p.Confirmed {
			action = "confirm"
		}
		res, err := m.rpc(ctx, p, message{Action: action})
		m.mu.Lock()
		current, exists := m.devices[p.ID]
		if exists {
			current.Online = err == nil
			if err == nil {
				current.LastSeen = time.Now()
				current.RemoteConfirmed = res.Confirmed
				if current.Confirmed && res.Confirmed {
					current.State = "paired"
					current.Code = ""
				}
				_ = m.saveDevice(current)
			}
		}
		m.mu.Unlock()
		if err != nil || !exists || current.State != "paired" {
			continue
		}
		m.deliver(ctx, p)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, tr := range m.transfers.Transfers() {
		for _, d := range m.inbox {
			if d.TransferID == tr.ID && d.State != tr.State && d.State != "cancelled" {
				d.State = tr.State
				d.Error = tr.Error
				_ = m.saveDelivery("inbox", d)
				if d.State == "completed" {
					if err := m.deleteSecret("inbox-token:" + d.ID); err != nil {
						m.lastError = err.Error()
					}
				}
			}
		}
	}
}
func (m *Manager) deliver(ctx context.Context, p deviceRecord) {
	m.mu.Lock()
	queue := []deliveryRecord{}
	for _, d := range m.outbox {
		if d.DeviceID == p.ID && d.State != "completed" && d.State != "declined" && d.State != "cancelled" {
			queue = append(queue, d)
		}
	}
	m.mu.Unlock()
	for _, d := range queue {
		if ctx.Err() != nil {
			return
		}
		if d.SessionID == "" {
			session, manifest, err := m.transfers.StartPairedOffer(ctx, d.Paths, d.Label, d.ID, d.Digest)
			if err != nil {
				m.mu.Lock()
				d.Error = err.Error()
				_ = m.saveDelivery("outbox", d)
				m.mu.Unlock()
				continue
			}
			d.SessionID = session.ID
			d.Token = session.Token
			d.Digest = transfer.ManifestDigest(manifest)
			m.mu.Lock()
			err = m.saveDelivery("outbox", d)
			m.mu.Unlock()
			if err != nil {
				_ = m.transfers.StopOffer(session.ID)
				continue
			}
		}
		res, err := m.rpc(ctx, p, message{Action: "offer", ID: d.ID, Label: d.Label, Token: d.Token})
		if err != nil {
			d.Error = "Delivery pending. Retrying when the device is reachable."
		} else {
			d.State = res.State
			d.Error = ""
		}
		if d.State == "completed" || d.State == "declined" {
			if err := m.transfers.StopOffer(d.SessionID); err != nil {
				d.Error = err.Error()
			}
			if err := m.forgetOffer(d.ID); err != nil {
				d.Error = err.Error()
			}
			d.SessionID = ""
		}
		m.mu.Lock()
		_ = m.saveDelivery("outbox", d)
		m.mu.Unlock()
	}
}
