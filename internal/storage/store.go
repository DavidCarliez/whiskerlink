package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
)

const keyringService = "whiskerlink"

var ErrSecretNotFound = keyring.ErrNotFound

type SecretStore interface {
	Set(name, value string) error
	Get(name string) (string, error)
	Delete(name string) error
}

type KeyringSecrets struct{}

func (KeyringSecrets) Set(name, value string) error    { return keyring.Set(keyringService, name, value) }
func (KeyringSecrets) Get(name string) (string, error) { return keyring.Get(keyringService, name) }
func (KeyringSecrets) Delete(name string) error        { return keyring.Delete(keyringService, name) }

type Store struct {
	db      *sql.DB
	Secrets SecretStore
}

func Open() (*Store, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve config directory: %w", err)
	}
	dir := filepath.Join(configDir, "whiskerlink")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	path := filepath.Join(dir, "state.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open state database: %w", err)
	}
	s := &Store{db: db, Secrets: KeyringSecrets{}}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("protect state database: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS trusted_devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  token_hint TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS transfers (
  id TEXT PRIMARY KEY,
  direction TEXT NOT NULL,
  label TEXT NOT NULL,
  state TEXT NOT NULL,
  destination TEXT NOT NULL DEFAULT '',
  files_total INTEGER NOT NULL,
  files_completed INTEGER NOT NULL,
  bytes_total INTEGER NOT NULL,
  bytes_transferred INTEGER NOT NULL,
  error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS receive_states (
  transfer_id TEXT PRIMARY KEY REFERENCES transfers(id) ON DELETE CASCADE,
  manifest_json BLOB NOT NULL,
  selected_json BLOB NOT NULL,
  collision_policy TEXT NOT NULL
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate state database: %w", err)
	}
	return nil
}

func (s *Store) AddDevice(ctx context.Context, d domain.TrustedDevice, token string) error {
	if err := s.Secrets.Set("device:"+d.ID, token); err != nil {
		return fmt.Errorf("store device token in credential manager: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO trusted_devices(id,name,token_hint,created_at,last_used)
VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET name=excluded.name, token_hint=excluded.token_hint`,
		d.ID, d.Name, d.TokenHint, d.CreatedAt.UnixMilli(), d.LastUsed.UnixMilli())
	if err != nil {
		_ = s.Secrets.Delete("device:" + d.ID)
		return fmt.Errorf("save trusted device: %w", err)
	}
	return nil
}

func (s *Store) DeviceToken(id string) (string, error) {
	token, err := s.Secrets.Get("device:" + id)
	if err != nil {
		return "", fmt.Errorf("load device token from credential manager: %w", err)
	}
	return token, nil
}

func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM trusted_devices WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete trusted device: %w", err)
	}
	if err := s.Secrets.Delete("device:" + id); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete device token: %w", err)
	}
	return nil
}

func (s *Store) ListDevices(ctx context.Context) ([]domain.TrustedDevice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,token_hint,created_at,last_used FROM trusted_devices ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list trusted devices: %w", err)
	}
	defer rows.Close()
	var out []domain.TrustedDevice
	for rows.Next() {
		var d domain.TrustedDevice
		var created, used int64
		if err := rows.Scan(&d.ID, &d.Name, &d.TokenHint, &created, &used); err != nil {
			return nil, fmt.Errorf("scan trusted device: %w", err)
		}
		d.CreatedAt = time.UnixMilli(created)
		if used > 0 {
			d.LastUsed = time.UnixMilli(used)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) UpsertTransfer(ctx context.Context, t domain.Transfer) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO transfers(
 id,direction,label,state,destination,files_total,files_completed,bytes_total,bytes_transferred,error,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
 state=excluded.state,destination=excluded.destination,files_completed=excluded.files_completed,
 bytes_transferred=excluded.bytes_transferred,error=excluded.error,updated_at=excluded.updated_at`,
		t.ID, t.Direction, t.Label, t.State, t.Destination, t.FilesTotal, t.FilesCompleted,
		t.BytesTotal, t.BytesTransferred, t.Error, t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("save transfer: %w", err)
	}
	return nil
}

func (s *Store) ListTransfers(ctx context.Context) ([]domain.Transfer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,direction,label,state,destination,files_total,files_completed,
bytes_total,bytes_transferred,error,created_at,updated_at FROM transfers ORDER BY updated_at DESC LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("list transfers: %w", err)
	}
	defer rows.Close()
	var out []domain.Transfer
	for rows.Next() {
		var t domain.Transfer
		var created, updated int64
		if err := rows.Scan(&t.ID, &t.Direction, &t.Label, &t.State, &t.Destination, &t.FilesTotal,
			&t.FilesCompleted, &t.BytesTotal, &t.BytesTransferred, &t.Error, &created, &updated); err != nil {
			return nil, fmt.Errorf("scan transfer: %w", err)
		}
		t.CreatedAt = time.UnixMilli(created)
		t.UpdatedAt = time.UnixMilli(updated)
		out = append(out, t)
	}
	return out, rows.Err()
}

type ReceiveState struct {
	Manifest  domain.FileManifest
	Selected  []string
	Completed []string
	Collision string
}

type receiveSelection struct {
	Selected  []string `json:"selected"`
	Completed []string `json:"completed"`
}

func (s *Store) SaveReceiveState(ctx context.Context, transferID string, state ReceiveState) error {
	manifest, err := json.Marshal(state.Manifest)
	if err != nil {
		return fmt.Errorf("encode receive manifest: %w", err)
	}
	selected, err := json.Marshal(receiveSelection{Selected: state.Selected, Completed: state.Completed})
	if err != nil {
		return fmt.Errorf("encode selected files: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO receive_states(transfer_id,manifest_json,selected_json,collision_policy)
VALUES(?,?,?,?) ON CONFLICT(transfer_id) DO UPDATE SET
manifest_json=excluded.manifest_json,selected_json=excluded.selected_json,collision_policy=excluded.collision_policy`,
		transferID, manifest, selected, state.Collision)
	if err != nil {
		return fmt.Errorf("save receive state: %w", err)
	}
	return nil
}

func (s *Store) LoadReceiveState(ctx context.Context, transferID string) (ReceiveState, error) {
	var manifest, selected []byte
	var state ReceiveState
	err := s.db.QueryRowContext(ctx, `SELECT manifest_json,selected_json,collision_policy FROM receive_states WHERE transfer_id=?`, transferID).
		Scan(&manifest, &selected, &state.Collision)
	if err != nil {
		return state, fmt.Errorf("load receive state: %w", err)
	}
	if err := json.Unmarshal(manifest, &state.Manifest); err != nil {
		return state, fmt.Errorf("decode receive manifest: %w", err)
	}
	var selection receiveSelection
	if err := json.Unmarshal(selected, &selection); err != nil {
		// Older local state stored the selected path array directly.
		if legacyErr := json.Unmarshal(selected, &state.Selected); legacyErr != nil {
			return state, fmt.Errorf("decode selected files: %w", err)
		}
	} else {
		state.Selected = selection.Selected
		state.Completed = selection.Completed
	}
	return state, nil
}

func (s *Store) DeleteReceiveState(ctx context.Context, transferID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM receive_states WHERE transfer_id=?`, transferID); err != nil {
		return fmt.Errorf("delete receive state: %w", err)
	}
	return nil
}
