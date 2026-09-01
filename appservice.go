package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tailscale/tailcat"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
	"github.com/DavidCarliez/whiskerlink/internal/storage"
	"github.com/DavidCarliez/whiskerlink/internal/tailnet"
	"github.com/DavidCarliez/whiskerlink/internal/transfer"
)

type AppService struct {
	store     *storage.Store
	sessions  *tailnet.Manager
	transfers *transfer.Manager

	emitMu sync.RWMutex
	emit   func(domain.Snapshot)

	pendingMu     sync.Mutex
	pendingInvite string
}

func NewAppService(store *storage.Store) (*AppService, error) {
	service := &AppService{store: store}
	notify := func() { service.emitSnapshot() }
	service.sessions = tailnet.NewManager(store, notify)
	transfers, err := transfer.NewManager(store, notify)
	if err != nil {
		return nil, err
	}
	service.transfers = transfers
	return service, nil
}

func (a *AppService) setEmitter(emit func(domain.Snapshot)) {
	a.emitMu.Lock()
	a.emit = emit
	a.emitMu.Unlock()
}

func (a *AppService) emitSnapshot() {
	a.emitMu.RLock()
	emit := a.emit
	a.emitMu.RUnlock()
	if emit != nil {
		emit(a.Snapshot())
	}
}

func (a *AppService) Snapshot() domain.Snapshot {
	devices, _ := a.store.ListDevices(context.Background())
	sessions := a.sessions.Sessions()
	sessions = append(sessions, a.transfers.Sessions()...)
	return domain.Snapshot{
		Sessions:       append([]domain.Session{}, sessions...),
		Transfers:      append([]domain.Transfer{}, a.transfers.Transfers()...),
		TrustedDevices: append([]domain.TrustedDevice{}, devices...),
	}
}

func (a *AppService) ShareService(req tailnet.ShareServiceRequest) (domain.Session, error) {
	return a.sessions.ShareService(context.Background(), req)
}

func (a *AppService) ConnectService(req tailnet.ConnectServiceRequest) (domain.Session, error) {
	req.Token = strings.TrimSpace(req.Token)
	return a.sessions.ConnectService(context.Background(), req)
}

func (a *AppService) ConnectTrustedService(deviceID, label string, remotePort, localPort uint16, serviceType string) (domain.Session, error) {
	token, err := a.store.DeviceToken(deviceID)
	if err != nil {
		return domain.Session{}, err
	}
	return a.sessions.ConnectService(context.Background(), tailnet.ConnectServiceRequest{
		Label: label, Token: token, RemotePort: remotePort, LocalPort: localPort, ServiceType: serviceType,
	})
}

func (a *AppService) ParseServiceInvite(value string) (domain.ServiceInvite, error) {
	return tailnet.ParseServiceInvite(value)
}

func (a *AppService) TakePendingInvite() string {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	value := a.pendingInvite
	a.pendingInvite = ""
	return value
}

func (a *AppService) queueInvite(value string) {
	a.pendingMu.Lock()
	a.pendingInvite = strings.TrimSpace(value)
	a.pendingMu.Unlock()
}

func (a *AppService) StopSession(id string) error {
	for _, session := range a.transfers.Sessions() {
		if session.ID == id {
			return a.transfers.StopOffer(id)
		}
	}
	return a.sessions.Stop(id)
}

func (a *AppService) StartFileOffer(req transfer.StartOfferRequest) (domain.Session, error) {
	return a.transfers.StartOffer(context.Background(), req)
}

func (a *AppService) InspectFileOffer(token string) (domain.FileManifest, error) {
	return a.transfers.Inspect(context.Background(), strings.TrimSpace(token))
}

func (a *AppService) ParseFileInvite(value string) (domain.FileInvite, error) {
	return transfer.ParseFileInvite(value)
}

func (a *AppService) InspectTrustedFileOffer(deviceID string) (domain.FileManifest, error) {
	token, err := a.store.DeviceToken(deviceID)
	if err != nil {
		return domain.FileManifest{}, err
	}
	return a.transfers.Inspect(context.Background(), token)
}

func (a *AppService) ReceiveFiles(req transfer.ReceiveRequest) (domain.Transfer, error) {
	req.Token = strings.TrimSpace(req.Token)
	return a.transfers.StartReceive(context.Background(), req)
}

func (a *AppService) ReceiveTrustedFiles(deviceID string, req transfer.ReceiveRequest) (domain.Transfer, error) {
	token, err := a.store.DeviceToken(deviceID)
	if err != nil {
		return domain.Transfer{}, err
	}
	req.Token = token
	return a.transfers.StartReceive(context.Background(), req)
}

func (a *AppService) PauseTransfer(id string) error  { return a.transfers.Pause(id) }
func (a *AppService) ResumeTransfer(id string) error { return a.transfers.Resume(id) }

func (a *AppService) AddTrustedDevice(name, token string) (domain.TrustedDevice, error) {
	name = strings.TrimSpace(name)
	token = strings.TrimSpace(token)
	if name == "" {
		return domain.TrustedDevice{}, errors.New("enter a device name")
	}
	if _, err := tailcat.ParseConnBlob(tailcat.ConnBlob(token)); err != nil {
		return domain.TrustedDevice{}, fmt.Errorf("invalid Tailcat token: %w", err)
	}
	hint := token
	if len(hint) > 16 {
		hint = hint[:10] + "…" + hint[len(hint)-5:]
	}
	device := domain.TrustedDevice{ID: uuid.NewString(), Name: name, TokenHint: hint, CreatedAt: time.Now()}
	if err := a.store.AddDevice(context.Background(), device, token); err != nil {
		return domain.TrustedDevice{}, err
	}
	a.emitSnapshot()
	return device, nil
}

func (a *AppService) RemoveTrustedDevice(id string) error {
	if err := a.store.DeleteDevice(context.Background(), id); err != nil {
		return err
	}
	a.emitSnapshot()
	return nil
}

func (a *AppService) shutdown() {
	a.sessions.StopAll()
	a.transfers.Close()
	_ = a.store.Close()
}
