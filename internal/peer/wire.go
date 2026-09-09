// Package peer implements mutually confirmed device pairing and an authenticated inbox.
package peer

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/tailscale/tailcat"
)

const controlPort uint16 = 43112
const maxMessage = 64 * 1024

type hello struct {
	Key   string
	Nonce string
}
type envelope struct {
	Key       string
	Nonce     string
	Body      json.RawMessage
	Signature []byte
}
type message struct {
	TargetTokenHash string `json:"targetTokenHash,omitempty"`
	SenderID        string `json:"senderId,omitempty"`
	InviteExpires   string `json:"inviteExpires,omitempty"`
	Action          string `json:"action"`
	Name            string `json:"name,omitempty"`
	Token           string `json:"token,omitempty"`
	InviteID        string `json:"inviteId,omitempty"`
	Proof           string `json:"proof,omitempty"`
	ID              string `json:"id,omitempty"`
	Label           string `json:"label,omitempty"`
	State           string `json:"state,omitempty"`
	Confirmed       bool   `json:"confirmed,omitempty"`
	Code            string `json:"code,omitempty"`
	Error           string `json:"error,omitempty"`
}

func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func publicKey(s string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("invalid device identity")
	}
	return ed25519.PublicKey(b), nil
}
func transcript(direction, serverKey, serverNonce, clientNonce string, body []byte) []byte {
	b, _ := json.Marshal([]string{"whiskerlink-control-v1", direction, serverKey, serverNonce, clientNonce, string(body)})
	return b
}
func readFrame(r *bufio.Reader, v any) error {
	b, err := r.ReadSlice('\n')
	if err != nil {
		return errors.New("invalid or oversized control message")
	}
	return json.Unmarshal(b, v)
}
func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) >= maxMessage {
		return errors.New("control message too large")
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func dialTailcat(ctx context.Context, token string) (net.Conn, error) {
	if _, err := tailcat.ParseConnBlob(tailcat.ConnBlob(token)); err != nil {
		return nil, errors.New("invalid device address")
	}
	c := tailcat.NewClient(tailcat.ConnBlob(token))
	c.Logf = func(string, ...any) {}
	conn, err := c.DialTCPPort(ctx, controlPort)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return &clientConn{Conn: conn, client: c}, nil
}

type clientConn struct {
	net.Conn
	client *tailcat.Client
}

func (c *clientConn) Close() error { _ = c.Conn.Close(); return c.client.Close() }

func (m *Manager) rpc(ctx context.Context, p deviceRecord, req message) (message, error) {
	var res message
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, err := m.dial(ctx, p.Token)
	if err != nil {
		return res, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	r := bufio.NewReaderSize(conn, maxMessage)
	var h hello
	if err = readFrame(r, &h); err != nil {
		return res, err
	}
	if h.Key != p.ID || len(h.Nonce) != 64 {
		return res, errors.New("remote device identity changed")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return res, err
	}
	nonce := randomID()
	m.mu.Lock()
	key := append(ed25519.PrivateKey(nil), m.key...)
	id := m.id
	m.mu.Unlock()
	env := envelope{Key: id, Nonce: nonce, Body: body, Signature: ed25519.Sign(key, transcript("request", h.Key, h.Nonce, nonce, body))}
	if err = writeFrame(conn, env); err != nil {
		return res, err
	}
	var answer envelope
	if err = readFrame(r, &answer); err != nil {
		return res, err
	}
	pub, err := publicKey(p.ID)
	if err != nil {
		return res, err
	}
	if answer.Key != p.ID || answer.Nonce != nonce || !ed25519.Verify(pub, transcript("response", h.Key, h.Nonce, nonce, answer.Body), answer.Signature) {
		return res, errors.New("remote response authentication failed")
	}
	if err = json.Unmarshal(answer.Body, &res); err != nil {
		return res, err
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	if req.Action == "join" {
		code := verificationCode(transcript("pair", h.Key, h.Nonce, nonce, body))
		if code != res.Code {
			return res, errors.New("pairing transcript mismatch")
		}
		res.Code = code
	}
	return res, nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func verificationCode(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%X %X", sum[:3], sum[3:6])
}

func (m *Manager) handle(conn net.Conn) {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		_ = conn.Close()
		return
	}
	m.handlers.Add(1)
	m.mu.Unlock()
	defer m.handlers.Done()
	select {
	case m.slots <- struct{}{}:
		defer func() { <-m.slots }()
	default:
		_ = conn.Close()
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
	m.mu.Lock()
	key := append(ed25519.PrivateKey(nil), m.key...)
	id := m.id
	m.mu.Unlock()
	h := hello{Key: id, Nonce: randomID()}
	if writeFrame(conn, h) != nil {
		return
	}
	var env envelope
	if readFrame(bufio.NewReaderSize(conn, maxMessage), &env) != nil {
		return
	}
	pub, err := publicKey(env.Key)
	if err != nil || len(env.Nonce) != 64 || !ed25519.Verify(pub, transcript("request", h.Key, h.Nonce, env.Nonce, env.Body), env.Signature) {
		return
	}
	var req message
	if json.Unmarshal(env.Body, &req) != nil {
		return
	}
	res, err := m.dispatch(env.Key, req, transcript("pair", h.Key, h.Nonce, env.Nonce, env.Body))
	if err != nil {
		res = message{Error: err.Error()}
	}
	body, _ := json.Marshal(res)
	_ = writeFrame(conn, envelope{Key: id, Nonce: env.Nonce, Body: body, Signature: ed25519.Sign(key, transcript("response", h.Key, h.Nonce, env.Nonce, body))})
}

func validToken(token string) error {
	_, err := tailcat.ParseConnBlob(tailcat.ConnBlob(token))
	if err != nil {
		return fmt.Errorf("invalid Tailcat address")
	}
	return nil
}
