package storage

import (
	"context"
	"encoding/json"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
)

// CreateReceive commits transfer metadata and resumable state together.
func (s *Store) CreateReceive(t domain.Transfer, state ReceiveState) error {
	manifest, err := json.Marshal(state.Manifest)
	if err != nil {
		return err
	}
	selected, err := json.Marshal(receiveSelection{Selected: state.Selected, Completed: state.Completed})
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO transfers(id,direction,label,state,destination,files_total,files_completed,bytes_total,bytes_transferred,error,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, t.ID, t.Direction, t.Label, t.State, t.Destination, t.FilesTotal, t.FilesCompleted, t.BytesTotal, t.BytesTransferred, t.Error, t.CreatedAt.UnixMilli(), t.UpdatedAt.UnixMilli())
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO receive_states(transfer_id,manifest_json,selected_json,collision_policy) VALUES(?,?,?,?)`, t.ID, manifest, selected, state.Collision)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Peer records contain metadata only; capabilities and keys stay in Secrets.
func (s *Store) SavePeerRecord(kind, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(context.Background(), `INSERT INTO peer_records(kind,id,value) VALUES(?,?,?) ON CONFLICT(kind,id) DO UPDATE SET value=excluded.value`, kind, id, data)
	return err
}
func (s *Store) PeerRecords(kind string) ([]json.RawMessage, error) {
	rows, err := s.db.QueryContext(context.Background(), `SELECT value FROM peer_records WHERE kind=? ORDER BY id`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(b))
	}
	return out, rows.Err()
}
func (s *Store) DeletePeerRecord(kind, id string) error {
	_, err := s.db.ExecContext(context.Background(), `DELETE FROM peer_records WHERE kind=? AND id=?`, kind, id)
	return err
}
