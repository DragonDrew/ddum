// Package httpapi serves the agent-facing HTTP API.
package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/DragonDrew/ddum/internal/bot/domain"
	"github.com/DragonDrew/ddum/internal/bot/store"
	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Store is the persistence the API needs.
type Store interface {
	Ping(ctx context.Context) error
	RecordPoll(ctx context.Context, p store.Poll) (store.PollResult, error)
	ApplyInventory(ctx context.Context, agentID string, inv protocol.Inventory, at time.Time) (store.InventoryResult, error)
	DispatchCommands(ctx context.Context, hostID string, leaseSeconds int, at time.Time) ([]store.Operation, error)
	RecordEvents(ctx context.Context, agentID, opID string, events []protocol.Event, at time.Time) (int, *store.Transition, error)
	RunningOperationIDs(ctx context.Context, hostID string) ([]string, error)
	InterruptLostOperation(ctx context.Context, id string, at time.Time) (store.Operation, bool, error)
	AgentKey(ctx context.Context, agentID string) (store.AgentKeyState, error)
	SetPendingAgentKey(ctx context.Context, agentID, key string, at time.Time) (store.Host, bool, error)
	BotSigningKey(ctx context.Context) (ed25519.PrivateKey, error)
}

// Notifier posts agent-related notices to Discord. Implementations must not
// block for long; errors are theirs to log.
type Notifier interface {
	HostRegistered(ctx context.Context, h store.Host, collision bool)
	HostOnline(ctx context.Context, h store.Host)
	ClockSkew(ctx context.Context, agentID, hostName string, skew time.Duration)
	OperationFinished(ctx context.Context, op store.Operation)
	Drifted(ctx context.Context, drifts []store.DriftEvent)
	MergeSuggested(ctx context.Context, suggestions []store.MergeSuggestion)
	RefreshStacks(ctx context.Context, stackIDs []string)
	AgentKeyChanged(ctx context.Context, h store.Host, fingerprint string)
}

// ClockSkewNoticeInterval limits clock-skew notices to one per agent per
// hour.
const ClockSkewNoticeInterval = time.Hour

// LostOperationAfter is how long polls must leave out an operation the agent
// accepted before the bot stops waiting for it. It counts from the
// first such poll, not from the operation's last event: an agent whose bot was
// down still has the final event to send, and sends it right after its first
// poll.
const LostOperationAfter = 10 * time.Minute

// Server holds the dependencies of the agent-facing API.
type Server struct {
	db     Store
	notify Notifier
	key    []byte
	log    *slog.Logger
	now    func() time.Time

	skewMu    sync.Mutex
	skewNoted map[string]time.Time

	absentMu sync.Mutex
	absent   map[string]map[string]time.Time // host ID → operation ID → first poll that left it out

	authMu  sync.Mutex
	nonces  map[string]time.Time // recent request nonces, to refuse replays
	refused map[string]time.Time // remote → when a refused request was last logged

	// LeaseSeconds is COMMAND_LEASE_SECONDS (default 120).
	LeaseSeconds int
	// AfterFinish, if set, runs after an operation finishes (auto-rollback).
	//. It must not block.
	AfterFinish func(op store.Operation)
	// AfterInventory, if set, runs after each applied inventory (the
	// auto-update engine re-evaluates eligibility). It must not block.
	AfterInventory func()
}

// New returns the API server.
func New(db Store, notify Notifier, key []byte, log *slog.Logger, now func() time.Time) *Server {
	return &Server{db: db, notify: notify, key: key, log: log, now: now, skewNoted: map[string]time.Time{},
		absent: map[string]map[string]time.Time{}, nonces: map[string]time.Time{}, refused: map[string]time.Time{}, LeaseSeconds: 120}
}

// Handler returns the HTTP handler with all routes registered.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("POST "+protocol.PathPoll, s.signed(protocol.MaxBodyBytes, s.poll))
	mux.Handle("PUT "+protocol.PathInventory, s.signed(protocol.MaxInventoryBodyBytes, s.inventory))
	mux.Handle("POST "+protocol.BasePath+"/agent/operations/{id}/events", s.signed(protocol.MaxBodyBytes, s.events))
	return mux
}

// healthz is unauthenticated and returns no details.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := s.db.Ping(ctx); err != nil {
		s.log.Warn("health check failed", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unavailable"))
		return
	}
	w.Write([]byte("ok"))
}

// request is a verified agent request.
type request struct {
	agentID   string
	body      []byte
	at        time.Time
	opID      string // {id} path value, for operation endpoints
	publicKey string // the key to pin, when none is pinned yet
}

// answer is how replies to one verified request are signed.
type answer struct {
	legacy bool   // protocol 1, for an approved agent with no key yet
	bound  string // the request signature the response is bound to
}

// reply is what a handler returns: a status and a JSON value.
type reply struct {
	status int
	body   any
}

func errReply(status int, code, msg string) reply {
	return reply{status, protocol.ErrorResponse{Error: protocol.ErrorBody{Code: code, Message: msg}}}
}

// signed wraps an agent endpoint: it limits the body, verifies the request
// before anything else, checks the protocol version, and signs what it
// answers. A request that fails verification gets no signature: the bot's
// signature would otherwise vouch for whatever request signature someone copied
// onto it.
func (s *Server) signed(limit int64, h func(context.Context, request) reply) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentID := r.Header.Get(protocol.HeaderAgentID)
		if agentID == "" || r.Header.Get(protocol.HeaderTimestamp) == "" || r.Header.Get(protocol.HeaderSignature) == "" {
			s.refuse(w, r, protocol.ErrMissingHeaders)
			return
		}
		state, err := s.db.AgentKey(r.Context(), agentID)
		if err != nil {
			s.log.Error("read agent key", "agent_id", agentID, "err", err)
			writePlain(w, errReply(http.StatusInternalServerError, protocol.CodeInternal, ""))
			return
		}
		if !state.Known {
			limit = min(limit, protocol.MaxBodyBytes) // an unknown agent has no inventory to send yet
		}
		now := s.now()
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writePlain(w, errReply(http.StatusRequestEntityTooLarge, protocol.CodeTooLarge,
					"body exceeds "+strconv.FormatInt(limit, 10)+" bytes"))
				return
			}
			writePlain(w, errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "could not read body"))
			return
		}

		version, verr := strconv.Atoi(r.Header.Get(protocol.HeaderProtocol))
		var an answer
		var publicKey string
		if verr == nil && version == 1 {
			var done bool
			if an, done = s.verifyLegacy(w, r, state, body, now); done {
				return
			}
		} else {
			var done bool
			if an, publicKey, done = s.verifyAgent(w, r, state, body, now); done {
				return
			}
		}

		// Protocol version: header, and body field when present.
		var probe struct {
			ProtocolVersion *int   `json:"protocol_version"`
			AgentID         string `json:"agent_id"`
		}
		json.Unmarshal(body, &probe)
		if probe.ProtocolVersion != nil && verr == nil && *probe.ProtocolVersion != version {
			verr = errors.New("header and body disagree")
		}
		if verr != nil || !protocol.SupportsVersion(version) {
			s.write(w, r, an, reply{http.StatusUpgradeRequired, protocol.ErrorResponse{Error: protocol.ErrorBody{
				Code: protocol.CodeProtocolUnsupported, Message: "unsupported protocol version",
				Supported: []int{protocol.MinSupportedVersion, protocol.MaxSupportedVersion},
			}}})
			return
		}
		if _, err := ids.Parse(agentID); err != nil {
			s.write(w, r, an, errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "agent ID is not a UUID"))
			return
		}
		if probe.AgentID != "" && probe.AgentID != agentID {
			s.write(w, r, an, errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "agent_id does not match the signed header"))
			return
		}
		s.write(w, r, an, h(r.Context(), request{agentID: agentID, body: body, at: now, opID: r.PathValue("id"), publicKey: publicKey}))
	})
}

// verifyAgent checks a protocol 2 request. It reports done when it has answered.
//
//   - With a pinned key, the request stands on the agent's signature alone.
//   - With none yet, the shared key vouches for the key offered, which is pinned.
//   - With a different key, the shared key vouches for the request but not for
//     the change: a person decides, and the agent waits.
func (s *Server) verifyAgent(w http.ResponseWriter, r *http.Request, state store.AgentKeyState, body []byte, now time.Time) (an answer, publicKey string, done bool) {
	agentID := r.Header.Get(protocol.HeaderAgentID)
	offered := r.Header.Get(protocol.HeaderAgentKey)
	an.bound = r.Header.Get(protocol.HeaderAgentSignature)
	if err := protocol.VerifyAgentV2(r, offered, body); err != nil {
		s.refuse(w, r, err)
		return an, "", true
	}
	if offered != state.Public {
		if err := protocol.VerifySharedV2(r, s.key, body); err != nil {
			s.refuse(w, r, err)
			return an, "", true
		}
	}
	if err := protocol.CheckTimestamp(r, now); err != nil {
		var skew *protocol.ClockSkewError
		if !errors.As(err, &skew) {
			s.refuse(w, r, err)
			return an, "", true
		}
		s.clockSkew(r.Context(), agentID, body, skew.Skew)
		s.write(w, r, an, reply{http.StatusUnauthorized, protocol.ErrorResponse{Error: protocol.ErrorBody{
			Code: protocol.CodeClockSkew, Message: err.Error(), ServerTime: now.UTC().Format(time.RFC3339),
		}}})
		return an, "", true
	}
	if !s.freshNonce(agentID, r.Header.Get(protocol.HeaderNonce), now) {
		s.refuse(w, r, errors.New("nonce missing or already used"))
		return an, "", true
	}
	if state.Public != "" && offered != state.Public {
		s.keyChanged(r.Context(), agentID, offered, now)
		s.write(w, r, an, errReply(http.StatusForbidden, protocol.CodeAgentKeyChanged,
			"this agent's key is not the one the bot pinned; accept the new key in Discord"))
		return an, "", true
	}
	if state.Public == "" {
		publicKey = offered
	}
	return an, publicKey, false
}

// verifyLegacy checks a protocol 1 request. Only an approved agent with no key
// yet may use it, so that upgrading the bot does not cut off agents waiting to
// be upgraded by hand. Nothing new can register this way, and an agent that has
// presented a key can never fall back.
func (s *Server) verifyLegacy(w http.ResponseWriter, r *http.Request, state store.AgentKeyState, body []byte, now time.Time) (answer, bool) {
	an := answer{legacy: true, bound: r.Header.Get(protocol.HeaderSignature)}
	if err := protocol.VerifyHTTPRequest(r, s.key, body, now); err != nil {
		var skew *protocol.ClockSkewError
		if errors.As(err, &skew) {
			s.clockSkew(r.Context(), r.Header.Get(protocol.HeaderAgentID), body, skew.Skew)
			s.write(w, r, an, reply{http.StatusUnauthorized, protocol.ErrorResponse{Error: protocol.ErrorBody{
				Code: protocol.CodeClockSkew, Message: err.Error(), ServerTime: now.UTC().Format(time.RFC3339),
			}}})
			return an, true
		}
		s.refuse(w, r, err)
		return an, true
	}
	if state.Public != "" || !state.Approved {
		s.write(w, r, an, reply{http.StatusUpgradeRequired, protocol.ErrorResponse{Error: protocol.ErrorBody{
			Code: protocol.CodeProtocolUnsupported, Message: "protocol 1 is only accepted from approved agents without a key",
			Supported: []int{2, protocol.MaxSupportedVersion},
		}}})
		return an, true
	}
	return an, false
}

// freshNonce remembers a nonce for as long as its request's timestamp could be
// accepted, and reports whether it was new. Nonces are kept in memory, so a bot
// restart forgets them; the timestamp window still applies.
func (s *Server) freshNonce(agentID, nonce string, now time.Time) bool {
	if nonce == "" {
		return false
	}
	key := agentID + " " + nonce
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if _, seen := s.nonces[key]; seen {
		return false
	}
	if len(s.nonces) > 4096 {
		for k, t := range s.nonces {
			if now.Sub(t) > 2*protocol.MaxClockSkew {
				delete(s.nonces, k)
			}
		}
	}
	s.nonces[key] = now
	return true
}

// keyChanged asks a person about a new key, once per key.
func (s *Server) keyChanged(ctx context.Context, agentID, key string, now time.Time) {
	h, changed, err := s.db.SetPendingAgentKey(ctx, agentID, key, now)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("record agent key change", "agent_id", agentID, "err", err)
		}
		return
	}
	if changed {
		s.log.Warn("an agent presented a different key", "agent_id", agentID, "host", h.DisplayName, "fingerprint", protocol.KeyFingerprint(key))
		if s.notify != nil {
			s.notify.AgentKeyChanged(ctx, h, protocol.KeyFingerprint(key))
		}
	}
}

// refuse answers a request that failed verification, with no signature. It
// logs at most one such request per minute per remote address.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, err error) {
	host, _, splitErr := net.SplitHostPort(r.RemoteAddr)
	if splitErr != nil {
		host = r.RemoteAddr
	}
	now := time.Now()
	s.authMu.Lock()
	last, seen := s.refused[host]
	log := !seen || now.Sub(last) >= time.Minute
	if log {
		if len(s.refused) > 1024 {
			clear(s.refused)
		}
		s.refused[host] = now
	}
	s.authMu.Unlock()
	if log {
		s.log.Warn("rejected unsigned or badly signed request", "path", r.URL.Path, "remote", r.RemoteAddr, "err", err)
	}
	writePlain(w, errReply(http.StatusUnauthorized, protocol.CodeBadSignature, "signature verification failed"))
}

// writePlain sends a reply with no signature, for a request not verified.
func writePlain(w http.ResponseWriter, rep reply) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	json.NewEncoder(w).Encode(rep.body)
}

// write marshals rep, signs it for the request it answers, and sends it.
func (s *Server) write(w http.ResponseWriter, r *http.Request, an answer, rep reply) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rep.body); err != nil {
		s.log.Error("encode response", "err", err)
		rep = errReply(http.StatusInternalServerError, protocol.CodeInternal, "")
		buf.Reset()
		enc.Encode(rep.body)
	}
	out := buf.Bytes()
	if an.legacy {
		protocol.SignHTTPResponse(w.Header(), s.key, an.bound, out, s.now())
	} else {
		priv, err := s.db.BotSigningKey(r.Context())
		if err != nil {
			s.log.Error("read the bot's key", "err", err)
			writePlain(w, errReply(http.StatusInternalServerError, protocol.CodeInternal, ""))
			return
		}
		protocol.SignHTTPResponseV2(w.Header(), priv, s.key, an.bound, rep.status, out, s.now())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	w.Write(out)
}

// clockSkew posts a notice for a validly signed request with a bad clock,
// at most once per agent per hour.
func (s *Server) clockSkew(ctx context.Context, agentID string, body []byte, skew time.Duration) {
	var probe struct {
		HostName string `json:"host_name"`
	}
	json.Unmarshal(body, &probe)
	s.log.Warn("rejected request: agent clock skew", "agent_id", agentID, "host_name", probe.HostName, "skew", skew.Round(time.Second).String())

	now := s.now()
	s.skewMu.Lock()
	last, seen := s.skewNoted[agentID]
	notify := !seen || now.Sub(last) >= ClockSkewNoticeInterval
	if notify {
		s.skewNoted[agentID] = now
	}
	s.skewMu.Unlock()
	if notify && s.notify != nil {
		s.notify.ClockSkew(ctx, agentID, probe.HostName, skew)
	}
}

func (s *Server) poll(ctx context.Context, r request) reply {
	var req protocol.PollRequest
	if err := json.Unmarshal(r.body, &req); err != nil {
		return errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "invalid JSON: "+err.Error())
	}
	name := req.HostName
	if name == "" {
		name = r.agentID
	}
	res, err := s.db.RecordPoll(ctx, store.Poll{
		AgentID:         r.agentID,
		AgentVersion:    req.AgentVersion,
		ProtocolVersion: req.ProtocolVersion,
		Capabilities:    req.Capabilities,
		PollInterval:    req.PollIntervalSeconds,
		HostName:        name,
		Docker:          req.Docker,
		PublicKey:       r.publicKey,
		At:              r.at,
	})
	if err != nil {
		s.log.Error("record poll", "agent_id", r.agentID, "err", err)
		return errReply(http.StatusInternalServerError, protocol.CodeInternal, "")
	}
	if res.Removed {
		return errReply(http.StatusForbidden, protocol.CodeAgentRemoved, "this agent's host was removed in Discord")
	}
	log := s.log.With("agent_id", r.agentID, "host_id", res.Host.ID)
	if res.Registered {
		log.Info("new agent registered; host pending approval", "host_name", name)
		if s.notify != nil {
			s.notify.HostRegistered(ctx, res.Host, res.Collision)
		}
	}
	if res.CameOnline {
		log.Info("host back online")
		if s.notify != nil {
			s.notify.HostOnline(ctx, res.Host)
		}
	}
	s.findLostOperations(ctx, res.Host.ID, req.ActiveOperations, r.at)
	return reply{http.StatusOK, protocol.PollResponse{
		ServerTime:         r.at.UTC().Format(time.RFC3339),
		InventoryRequested: res.Host.InventoryHash == "" || res.Host.InventoryHash != req.InventoryHash,
		Commands:           s.commands(ctx, res.Host, r.at),
	}}
}

// findLostOperations ends operations the polling agent has stopped reporting
// for LostOperationAfter. An agent that lost its journal never reports their
// end, and an accepted operation cannot be cancelled, so otherwise its host
// would never run another one. Times are kept in memory, so a bot restart
// only starts the wait again.
func (s *Server) findLostOperations(ctx context.Context, hostID string, active []protocol.ActiveOperation, at time.Time) {
	ids, err := s.db.RunningOperationIDs(ctx, hostID)
	if err != nil {
		s.log.Error("list running operations", "host_id", hostID, "err", err)
		return
	}
	var lost []string
	s.absentMu.Lock()
	absent := map[string]time.Time{} // operations that ended drop out here
	for _, id := range ids {
		if slices.ContainsFunc(active, func(a protocol.ActiveOperation) bool { return a.OperationID == id }) {
			continue
		}
		since, seen := s.absent[hostID][id]
		switch {
		case !seen:
			absent[id] = at
		case at.Sub(since) >= LostOperationAfter:
			lost = append(lost, id)
		default:
			absent[id] = since
		}
	}
	s.absent[hostID] = absent
	s.absentMu.Unlock()
	for _, id := range lost {
		op, ended, err := s.db.InterruptLostOperation(ctx, id, at)
		if err != nil {
			s.log.Error("interrupt lost operation", "operation_id", id, "err", err)
			continue
		}
		if !ended {
			continue
		}
		s.log.Warn("the agent no longer reports this operation; ended it", "operation_id", id, "host_id", hostID)
		if s.notify != nil {
			s.notify.OperationFinished(ctx, op)
		}
		if s.AfterFinish != nil {
			s.AfterFinish(op)
		}
	}
}

func (s *Server) inventory(ctx context.Context, r request) reply {
	var inv protocol.Inventory
	if err := json.Unmarshal(r.body, &inv); err != nil {
		return errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "invalid JSON: "+err.Error())
	}
	if inv.InventoryHash == "" {
		return errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "inventory_hash is required")
	}
	for _, st := range inv.Stacks {
		if st.Fingerprint == "" {
			return errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "stack without fingerprint")
		}
		for _, c := range st.Containers {
			if c.IdentityKey == "" {
				return errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "container without identity_key in "+st.Fingerprint)
			}
		}
	}
	res, err := s.db.ApplyInventory(ctx, r.agentID, inv, r.at)
	switch {
	case errors.Is(err, store.ErrHostRemoved):
		return errReply(http.StatusForbidden, protocol.CodeAgentRemoved, "this agent's host was removed in Discord")
	case errors.Is(err, store.ErrUnknownAgent):
		return errReply(http.StatusConflict, protocol.CodeConflict, "unknown agent: poll first")
	case err != nil:
		s.log.Error("apply inventory", "agent_id", r.agentID, "err", err)
		return errReply(http.StatusInternalServerError, protocol.CodeInternal, "")
	}
	if len(res.Drifts) > 0 && s.notify != nil {
		s.notify.Drifted(ctx, res.Drifts)
	}
	if len(res.MergeSuggestions) > 0 && s.notify != nil {
		s.notify.MergeSuggested(ctx, res.MergeSuggestions)
	}
	if len(res.ChangedStacks) > 0 && s.notify != nil {
		s.notify.RefreshStacks(ctx, res.ChangedStacks)
	}
	if s.AfterInventory != nil {
		s.AfterInventory()
	}
	s.log.Debug("inventory applied", "agent_id", r.agentID, "host_id", res.Host.ID,
		"stacks_created", res.StacksCreated, "stacks_missing", res.StacksMissing,
		"containers_added", res.ContainersAdded, "containers_missing", res.ContainersMissed)
	return reply{http.StatusOK, struct{}{}}
}

// commands dispatches the host's next operation. A pending host never
// receives one; dispatch errors only delay it to the next poll.
func (s *Server) commands(ctx context.Context, h store.Host, at time.Time) []protocol.Command {
	out := []protocol.Command{}
	if h.Approval != domain.ApprovalApproved {
		return out
	}
	ops, err := s.db.DispatchCommands(ctx, h.ID, s.LeaseSeconds, at)
	if err != nil {
		s.log.Error("dispatch commands", "host_id", h.ID, "err", err)
		return out
	}
	for _, op := range ops {
		out = append(out, protocol.Command{OperationID: op.ID, Type: op.Kind,
			LeaseExpiresAt: op.LeaseExpiresAt.UTC().Format(time.RFC3339), Payload: json.RawMessage(op.PayloadJSON)})
		s.log.Info("operation dispatched", "operation_id", op.ID, "host_id", h.ID, "attempt", op.Attempt)
	}
	return out
}

func (s *Server) events(ctx context.Context, r request) reply {
	if _, err := ids.Parse(r.opID); err != nil {
		return errReply(http.StatusNotFound, protocol.CodeNotFound, "unknown operation")
	}
	var req protocol.EventsRequest
	if err := json.Unmarshal(r.body, &req); err != nil {
		return errReply(http.StatusBadRequest, protocol.CodeInvalidRequest, "invalid JSON: "+err.Error())
	}
	acked, tr, err := s.db.RecordEvents(ctx, r.agentID, r.opID, req.Events, r.at)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errReply(http.StatusNotFound, protocol.CodeNotFound, "unknown operation")
	case err != nil:
		s.log.Error("record events", "operation_id", r.opID, "err", err)
		return errReply(http.StatusInternalServerError, protocol.CodeInternal, "")
	}
	if tr != nil {
		s.log.Info("operation state", "operation_id", r.opID, "from", tr.From, "to", tr.Operation.State)
		if tr.Finished && s.notify != nil {
			s.notify.OperationFinished(ctx, tr.Operation)
		}
		if tr.Finished && s.AfterFinish != nil {
			s.AfterFinish(tr.Operation)
		}
	}
	return reply{http.StatusOK, protocol.EventsResponse{AckedSeq: acked}}
}
