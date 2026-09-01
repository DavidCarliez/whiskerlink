package tailnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tailscale/tailcat"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
	"github.com/DavidCarliez/whiskerlink/internal/storage"
)

type ShareServiceRequest struct {
	Label       string `json:"label"`
	LocalHost   string `json:"localHost"`
	LocalPort   uint16 `json:"localPort"`
	RemotePort  uint16 `json:"remotePort"`
	Persistent  bool   `json:"persistent"`
	ServiceType string `json:"serviceType"`
}

type ConnectServiceRequest struct {
	Label       string `json:"label"`
	Token       string `json:"token"`
	RemotePort  uint16 `json:"remotePort"`
	LocalPort   uint16 `json:"localPort"`
	ServiceType string `json:"serviceType"`
}

type runtimeSession struct {
	value     domain.Session
	stop      func() error
	client    *tailcat.Client
	probeOnce sync.Once
}

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*runtimeSession
	identity *IdentityStore
	onChange func()
}

func NewManager(store *storage.Store, onChange func()) *Manager {
	return &Manager{sessions: make(map[string]*runtimeSession), identity: NewIdentityStore(store), onChange: onChange}
}

func (m *Manager) Sessions() []domain.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.value)
	}
	return out
}

func (m *Manager) notify() {
	if m.onChange != nil {
		m.onChange()
	}
}

func validatePort(port uint16, field string) error {
	if port == 0 {
		return fmt.Errorf("%s must be between 1 and 65535", field)
	}
	return nil
}

func (m *Manager) ShareService(ctx context.Context, req ShareServiceRequest) (domain.Session, error) {
	if err := validatePort(req.LocalPort, "local port"); err != nil {
		return domain.Session{}, err
	}
	if err := validatePort(req.RemotePort, "remote port"); err != nil {
		return domain.Session{}, err
	}
	serviceType, err := normalizeServiceType(req.ServiceType)
	if err != nil {
		return domain.Session{}, err
	}
	if req.LocalHost == "" {
		req.LocalHost = "127.0.0.1"
	}
	localTarget := net.JoinHostPort(req.LocalHost, strconv.Itoa(int(req.LocalPort)))
	if req.Label == "" {
		req.Label = "Shared service"
	}

	probe, err := net.DialTimeout("tcp", localTarget, 2*time.Second)
	if err != nil {
		return domain.Session{}, fmt.Errorf("local service is unavailable at %s: %w", localTarget, err)
	}
	_ = probe.Close()

	sessionID := uuid.NewString()
	var server *tailcat.Server
	server = &tailcat.Server{
		Logf: discardLog,
		OnTCP: func(port uint16) func(net.Conn) {
			if port != req.RemotePort {
				return nil
			}
			return func(in net.Conn) {
				out, err := net.DialTimeout("tcp", localTarget, 10*time.Second)
				if err != nil {
					_ = in.Close()
					m.setSessionError(sessionID, fmt.Errorf("local service is unavailable at %s: %w", localTarget, err))
					return
				}
				m.setSessionConnected(sessionID)
				tailcat.ProxyConns(in, out)
			}
		},
	}
	if req.Persistent {
		if err := m.identity.ConfigureServer(server); err != nil {
			return domain.Session{}, err
		}
	}
	if err := server.Start(); err != nil {
		return domain.Session{}, fmt.Errorf("start Tailcat service: %w", err)
	}
	token := string(server.ConnBlob())
	if req.Persistent {
		if err := m.identity.SaveServer(server.Key, token); err != nil {
			_ = server.Close()
			return domain.Session{}, err
		}
	}
	invite, err := EncodeServiceInvite(domain.ServiceInvite{
		Token: token, RemotePort: req.RemotePort, Label: req.Label, ServiceType: serviceType,
	})
	if err != nil {
		_ = server.Close()
		return domain.Session{}, err
	}

	s := domain.Session{
		ID: sessionID, Kind: domain.SessionServiceShare, Label: req.Label,
		State: "listening", Token: token, Invite: invite, RemotePort: req.RemotePort,
		LocalAddress: localTarget, ServiceType: serviceType, Transport: "waiting",
		Persistent: req.Persistent, CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.sessions[s.ID] = &runtimeSession{value: s, stop: server.Close}
	m.mu.Unlock()
	m.notify()
	return s, nil
}

func (m *Manager) ConnectService(ctx context.Context, req ConnectServiceRequest) (domain.Session, error) {
	if _, err := tailcat.ParseConnBlob(tailcat.ConnBlob(req.Token)); err != nil {
		return domain.Session{}, fmt.Errorf("invalid Tailcat token: %w", err)
	}
	if err := validatePort(req.RemotePort, "remote port"); err != nil {
		return domain.Session{}, err
	}
	serviceType, err := normalizeServiceType(req.ServiceType)
	if err != nil {
		return domain.Session{}, err
	}
	if req.Label == "" {
		req.Label = "Remote service"
	}
	bind := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(req.LocalPort)))
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		return domain.Session{}, fmt.Errorf("open local listener: %w", err)
	}
	client := tailcat.NewClient(tailcat.ConnBlob(req.Token))
	client.Logf = discardLog
	ctx, cancel := context.WithCancel(context.Background())
	stop := func() error {
		cancel()
		_ = listener.Close()
		return client.Close()
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	s := domain.Session{
		ID: uuid.NewString(), Kind: domain.SessionServiceLink, Label: req.Label,
		State: "connecting", LocalAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))),
		RemotePort: req.RemotePort, ServiceType: serviceType, Transport: "connecting", CreatedAt: time.Now(),
	}
	runtime := &runtimeSession{value: s, stop: stop, client: client}
	m.mu.Lock()
	m.sessions[s.ID] = runtime
	m.mu.Unlock()
	m.notify()

	go m.acceptLoop(ctx, listener, client, req.RemotePort, s.ID)
	return s, nil
}

func (m *Manager) acceptLoop(ctx context.Context, listener net.Listener, client *tailcat.Client, remotePort uint16, sessionID string) {
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				m.setSessionError(sessionID, err)
			}
			return
		}
		go func(local net.Conn) {
			remote, err := client.DialTCPPort(ctx, remotePort)
			if err != nil {
				_ = local.Close()
				m.setSessionError(sessionID, err)
				return
			}
			m.mu.Lock()
			session := m.sessions[sessionID]
			if session != nil {
				session.value.State = "connected"
				session.value.Transport = "connected"
				session.value.Error = ""
				session.probeOnce.Do(func() { go m.probePath(ctx, client, sessionID) })
			}
			m.mu.Unlock()
			m.notify()
			tailcat.ProxyConns(local, remote)
		}(local)
	}
}

func (m *Manager) probePath(ctx context.Context, client *tailcat.Client, sessionID string) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	res, err := client.DiscoPing(probeCtx)
	if err != nil {
		return
	}
	transport := "DERP"
	if res.Endpoint != "" {
		transport = "direct"
	} else if res.DERPRegionCode != "" {
		transport = "DERP (" + res.DERPRegionCode + ")"
	}
	m.mu.Lock()
	if session := m.sessions[sessionID]; session != nil {
		session.value.State = "connected"
		session.value.Transport = transport
		session.value.LatencyMS = int64(res.LatencySeconds * 1000)
	}
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) setSessionConnected(id string) {
	m.mu.Lock()
	if session := m.sessions[id]; session != nil {
		session.value.State = "connected"
		session.value.Transport = "Tailcat client"
		session.value.Error = ""
	}
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) setSessionError(id string, err error) {
	m.mu.Lock()
	if session := m.sessions[id]; session != nil {
		session.value.State = "error"
		session.value.Error = err.Error()
	}
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) Stop(id string) error {
	m.mu.Lock()
	session := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if session == nil {
		return errors.New("session not found")
	}
	err := session.stop()
	m.notify()
	return err
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = make(map[string]*runtimeSession)
	m.mu.Unlock()
	for _, session := range sessions {
		_ = session.stop()
	}
	m.notify()
}

func discardLog(string, ...any) {}
