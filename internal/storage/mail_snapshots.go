package storage

import (
	"context"
	"errors"
)

// MailSnapshot is what a run actually received. The message body is never stored: the
// operator needs the sender, the subject and the verification link to understand a step,
// not the whole mail, and keeping it out bounds the blast radius of a leak.
type MailSnapshot struct {
	ID           string `json:"id"`
	RunID        string `json:"run_id"`
	Step         string `json:"step"`
	MailboxAlias string `json:"mailbox_alias"`
	Sender       string `json:"sender"`
	Subject      string `json:"subject"`
	Link         string `json:"link"`
	ReceivedAtMS int64  `json:"received_at_ms"`
	CreatedAtMS  int64  `json:"created_at_ms"`
}

// InsertMailSnapshot records one received message. It is idempotent per run and message.
func (s *Store) InsertMailSnapshot(ctx context.Context, snap MailSnapshot) error {
	if snap.RunID == "" || snap.Step == "" {
		return errors.New("storage: mail snapshot requires a run and a step")
	}
	if snap.ID == "" {
		snap.ID = s.newID()
	}
	_, err := s.writeDB.ExecContext(ctx, `INSERT INTO mail_snapshots
		(id,run_id,step,mailbox_alias,sender,subject,link,received_ms,created_ms)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		snap.ID, snap.RunID, snap.Step, snap.MailboxAlias, snap.Sender, snap.Subject,
		snap.Link, snap.ReceivedAtMS, snap.CreatedAtMS)
	return err
}

// ListMailSnapshots returns the snapshots of one run, oldest first.
func (s *Store) ListMailSnapshots(ctx context.Context, runID string) ([]MailSnapshot, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT id,run_id,step,mailbox_alias,sender,subject,
		link,received_ms,created_ms FROM mail_snapshots WHERE run_id = ? ORDER BY created_ms ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MailSnapshot, 0, 4)
	for rows.Next() {
		var snap MailSnapshot
		if err := rows.Scan(&snap.ID, &snap.RunID, &snap.Step, &snap.MailboxAlias, &snap.Sender,
			&snap.Subject, &snap.Link, &snap.ReceivedAtMS, &snap.CreatedAtMS); err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}
