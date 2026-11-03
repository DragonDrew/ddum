package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// AgentKeyState is what the bot knows about an agent's key.
type AgentKeyState struct {
	Known    bool   // the agent has a row
	Public   string // the pinned public key; "" until the agent has presented one
	Pending  string // a different key waiting for a person's decision
	Approved bool   // its host is approved and not removed
}

// AgentKey returns the key state of an agent.
func (s *Store) AgentKey(ctx context.Context, agentID string) (AgentKeyState, error) {
	var st AgentKeyState
	err := s.r.QueryRowContext(ctx, `SELECT COALESCE(a.public_key,''), COALESCE(a.pending_public_key,''),
			COALESCE(h.approval = 'approved' AND h.removed_at IS NULL, 0)
		FROM agents a LEFT JOIN hosts h ON h.agent_id = a.id WHERE a.id = ?`, agentID).Scan(&st.Public, &st.Pending, &st.Approved)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.Known = true
	return st, nil
}

// SetPendingAgentKey records a key that differs from the pinned one, from a
// request that proved it holds the shared key. A person decides whether it
// replaces the pinned key. It reports whether the key is new, so the question is
// asked once per key.
func (s *Store) SetPendingAgentKey(ctx context.Context, agentID, key string, at time.Time) (Host, bool, error) {
	var h Host
	var changed bool
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `UPDATE agents SET pending_public_key = ? WHERE id = ? AND COALESCE(pending_public_key,'') <> ?`, key, agentID, key)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		changed = n > 0
		if h, err = getHost(ctx, tx, `h.agent_id = ?`, agentID); err != nil {
			return err
		}
		if changed {
			return s.audit(ctx, tx, Actor{Kind: "agent"}, at, "agent.key_change_requested", "host", h.ID,
				map[string]any{"fingerprint": protocol.KeyFingerprint(key)})
		}
		return nil
	})
	return h, changed, err
}

// AcceptAgentKey makes a host's pending key its pinned key.
func (s *Store) AcceptAgentKey(ctx context.Context, hostID string, by Actor, at time.Time) (Host, error) {
	return s.decideAgentKey(ctx, hostID, by, at, "agent.key_accepted",
		`UPDATE agents SET public_key = pending_public_key, pending_public_key = NULL WHERE id = ? AND pending_public_key IS NOT NULL`)
}

// RejectAgentKey forgets a host's pending key.
func (s *Store) RejectAgentKey(ctx context.Context, hostID string, by Actor, at time.Time) (Host, error) {
	return s.decideAgentKey(ctx, hostID, by, at, "agent.key_rejected",
		`UPDATE agents SET pending_public_key = NULL WHERE id = ? AND pending_public_key IS NOT NULL`)
}

func (s *Store) decideAgentKey(ctx context.Context, hostID string, by Actor, at time.Time, action, update string) (Host, error) {
	var h Host
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		if h, err = getHost(ctx, tx, `h.id = ?`, hostID); err != nil {
			return err
		}
		fingerprint := protocol.KeyFingerprint(h.PendingAgentKey)
		r, err := tx.ExecContext(ctx, update, h.AgentID)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if err := s.audit(ctx, tx, by, at, action, "host", h.ID, map[string]any{"fingerprint": fingerprint}); err != nil {
			return err
		}
		h, err = getHost(ctx, tx, `h.id = ?`, hostID)
		return err
	})
	return h, err
}

const botKeySetting = "bot_signing_key"

// BotSigningKey returns the bot's private key, creating it on first use. It
// lives in the database so that a restored backup keeps the key agents pinned.
func (s *Store) BotSigningKey(ctx context.Context) (ed25519.PrivateKey, error) {
	var encoded string
	found, err := s.Setting(ctx, botKeySetting, &encoded)
	if err != nil {
		return nil, err
	}
	if !found {
		priv, err := protocol.NewKey()
		if err != nil {
			return nil, err
		}
		encoded = base64.RawStdEncoding.EncodeToString(priv)
		// Two callers racing keep the first key written.
		if err := s.WriteTx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO system_settings (key, value_json, updated_at) VALUES (?, json_quote(?), ?)
				ON CONFLICT (key) DO NOTHING`, botKeySetting, encoded, FormatTime(time.Now()))
			return err
		}); err != nil {
			return nil, err
		}
		if _, err := s.Setting(ctx, botKeySetting, &encoded); err != nil {
			return nil, err
		}
	}
	b, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("stored bot key is corrupt")
	}
	return ed25519.PrivateKey(b), nil
}
