package webrtcgw

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"

	"github.com/exotel/langstream/pkg/langstream"
)

// SessionFactory builds a fresh langstream.Session for one new room. It is
// supplied by the caller (see cmd/langstream's webrtc subcommand) so this
// package stays independent of how backends are selected/configured --
// exactly the same separation cmd/langstream/main.go's newSession already
// draws for the demo/serve/duplex subcommands.
type SessionFactory func(ctx context.Context) (*langstream.Session, error)

// ErrRoleTaken is returned by Manager.Join when the requested role
// ("caller" or "agent") in the given room already has a connected peer --
// a room is exactly two participants, one per role, never more.
var ErrRoleTaken = fmt.Errorf("webrtcgw: role already taken in this room")

// DefaultRoomIdleTimeout is how long a room is allowed to exist with
// fewer than two peers (i.e. waiting on a second participant who may
// never show up -- a dead link shared, a browser crash before joining,
// someone simply abandoning the tab) before it is automatically closed
// by expireIncomplete. Chosen generously: comfortably long enough for a
// real user to open a browser tab and click "join" even on a slow
// connection, short enough that an abandoned room doesn't hold a live
// ASR/MT/TTS Session (and its goroutines/buffers) open indefinitely.
const DefaultRoomIdleTimeout = 2 * time.Minute

// Room is one live two-user translated call: exactly one "caller" peer,
// one "agent" peer, and the langstream.Session bridging them (caller
// speech translated for the agent to hear, and vice versa -- see
// peer.go's startInboundBridge/startOutboundBridge and
// pkg/langstream/session.go's Session doc comment for the exact
// direction mapping).
type Room struct {
	id      string
	session *langstream.Session

	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	caller *peer
	agent  *peer
	// closed is set the first time this room is torn down, whether via a
	// normal both-peers-left cleanup (leave) or an idle-timeout expiry
	// (expireIncomplete) that fired before a second peer ever joined.
	// Guarding on it makes both cleanup paths mutually exclusive and
	// idempotent -- see leave and expireIncomplete's doc comments for why
	// both are reachable for the same room.
	closed bool
	// idleTimer fires expireIncomplete if this room never reaches two
	// peers within the Manager's idleTimeout. It is stopped (see Join)
	// the moment a room becomes full, and is nil if idle timeouts are
	// disabled (Manager.idleTimeout <= 0).
	idleTimer *time.Timer
}

// ManagerOption configures optional Manager behavior; see WithIdleTimeout
// and WithMaxRooms. NewManager's own signature is left untouched (still
// just a SessionFactory) so existing callers -- cmd/langstream's webrtc
// subcommand, this package's own tests -- keep compiling unchanged and
// get DefaultRoomIdleTimeout (and no room cap) applied automatically.
type ManagerOption func(*Manager)

// WithIdleTimeout overrides DefaultRoomIdleTimeout: a room that never
// reaches two peers within d is automatically closed (see
// expireIncomplete). d <= 0 disables the idle timeout entirely, meaning a
// room lives forever once created regardless of how many peers ever join
// -- this restores the pre-hardening behavior, and is mainly useful for
// tests that want full manual control over room teardown.
func WithIdleTimeout(d time.Duration) ManagerOption {
	return func(m *Manager) { m.idleTimeout = d }
}

// WithMaxRooms caps the number of rooms that may exist concurrently. Once
// the cap is reached, Manager.Join rejects any attempt to create a
// brand-new room (an error is returned to the caller, e.g. surfaced to
// the browser as a signaling "error" message); joining an *existing*
// room as its second peer is never rejected by this limit, since it does
// not increase the room count. n <= 0 (the default) means unlimited --
// this exists purely as a defense against unbounded resource exhaustion
// from repeated abandoned or malicious room-creation attempts, not a
// limit real/expected usage should ever hit.
func WithMaxRooms(n int) ManagerOption {
	return func(m *Manager) { m.maxRooms = n }
}

// Manager creates and tracks Rooms by ID. It is safe for concurrent use --
// multiple browsers may call Join concurrently (e.g. both participants'
// initial WebSocket connections racing in).
type Manager struct {
	newSession SessionFactory

	idleTimeout time.Duration
	maxRooms    int

	mu    sync.Mutex
	rooms map[string]*Room
}

// NewManager returns a Manager that builds each room's Session via
// newSession (see SessionFactory). DefaultRoomIdleTimeout and an
// unlimited room count apply unless overridden via opts (see
// WithIdleTimeout, WithMaxRooms).
func NewManager(newSession SessionFactory, opts ...ManagerOption) *Manager {
	m := &Manager{
		newSession:  newSession,
		rooms:       make(map[string]*Room),
		idleTimeout: DefaultRoomIdleTimeout,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Join adds a new peer with the given role to roomID, creating the room
// (and its Session) if this is the first participant. iceServers is
// passed straight through to the underlying PeerConnection's
// configuration (e.g. a STUN server for real-world NAT traversal; may be
// empty for same-host/loopback testing). onErr is called (from internal
// goroutines) if the bridge hits a runtime error after Join returns
// successfully; a nil onErr defaults to logging via the standard log
// package.
func (m *Manager) Join(roomID string, role peerRole, iceServers []webrtc.ICEServer, onErr func(error)) (*peer, *Room, error) {
	if onErr == nil {
		onErr = func(err error) { log.Printf("webrtcgw: room %q: %v", roomID, err) }
	}

	room, err := m.roomFor(roomID)
	if err != nil {
		return nil, nil, err
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	switch role {
	case roleCaller:
		if room.caller != nil {
			return nil, nil, ErrRoleTaken
		}
	case roleAgent:
		if room.agent != nil {
			return nil, nil, ErrRoleTaken
		}
	}

	p, err := newPeer(role, iceServers)
	if err != nil {
		return nil, nil, fmt.Errorf("webrtcgw: creating peer for room %q: %w", roomID, err)
	}

	switch role {
	case roleCaller:
		room.caller = p
		p.startInboundBridge(room.ctx, room.session.PushCallerAudio, onErr)
		p.startOutboundBridge(room.ctx, room.session.CallerHearsAudio(), onErr)
	case roleAgent:
		room.agent = p
		p.startInboundBridge(room.ctx, room.session.PushAgentAudio, onErr)
		p.startOutboundBridge(room.ctx, room.session.AgentHearsAudio(), onErr)
	}

	// Restored during today's idle-timeout hardening pass: this
	// registration was dropped in an earlier draft of that change, which
	// would have reintroduced a leak on the opposite side of the one
	// being fixed -- a *full* two-peer room whose media path dies at the
	// ICE/DTLS level (not a clean WS close) would never be cleaned up,
	// since leave was otherwise only reachable via signaling.go's
	// WS-close defer. m.leave is idempotent against a room already
	// closed (see leave's room.closed check above), so this is safe to
	// fire even if expireIncomplete or a normal WS-close leave already
	// tore the room down concurrently.
	p.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
			m.leave(roomID, role)
		}
	})

	// The room just became full: it no longer needs its idle timeout,
	// which exists solely to reclaim rooms that never reach this state.
	// Stopping it here (rather than leaving it to fire harmlessly later)
	// means expireIncomplete's "already full" check is just a defensive
	// backstop for the race against a timer that already fired, not the
	// primary mechanism.
	if room.caller != nil && room.agent != nil && room.idleTimer != nil {
		room.idleTimer.Stop()
		room.idleTimer = nil
	}

	return p, room, nil
}

// roomFor returns the Room for roomID, creating it (and its Session, and
// its idle-expiry timer) if this is the first participant. maxRooms (if
// set via WithMaxRooms) is only checked when a *new* room would be
// created -- joining an existing room never fails this check.
//
// If roomFor observes an existing room that has already been closed
// (room.closed) -- which can happen if expireIncomplete's idle timeout
// fires concurrently with a second peer's Join arriving, in the narrow
// window between this Manager's own map lookup and locking that room's
// mu -- it retries the whole lookup rather than adding a peer to a room
// whose Session has already been torn down. The retry naturally lands on
// a freshly created room (expireIncomplete has already removed the stale
// entry from m.rooms by that point, or will have by the time the retry's
// own map lookup runs).
func (m *Manager) roomFor(roomID string) (*Room, error) {
	for {
		m.mu.Lock()
		room, ok := m.rooms[roomID]
		if ok {
			m.mu.Unlock()

			room.mu.Lock()
			closed := room.closed
			room.mu.Unlock()
			if closed {
				continue
			}
			return room, nil
		}

		if m.maxRooms > 0 && len(m.rooms) >= m.maxRooms {
			m.mu.Unlock()
			return nil, fmt.Errorf("webrtcgw: max concurrent rooms (%d) reached, rejecting new room %q", m.maxRooms, roomID)
		}

		ctx, cancel := context.WithCancel(context.Background())
		sess, err := m.newSession(ctx)
		if err != nil {
			cancel()
			m.mu.Unlock()
			return nil, fmt.Errorf("webrtcgw: creating session for room %q: %w", roomID, err)
		}
		room = &Room{id: roomID, session: sess, ctx: ctx, cancel: cancel}
		m.rooms[roomID] = room
		m.mu.Unlock()

		if m.idleTimeout > 0 {
			room.mu.Lock()
			room.idleTimer = time.AfterFunc(m.idleTimeout, func() { m.expireIncomplete(roomID, room) })
			room.mu.Unlock()
		}

		return room, nil
	}
}

// expireIncomplete is scheduled via time.AfterFunc when a room is first
// created, firing once the Manager's idleTimeout elapses. If by then the
// room still doesn't have both a caller and an agent, it is force-closed
// exactly like a normal empty-room cleanup (see leave): its Session is
// closed, its ctx is cancelled (stopping every inbound/outbound bridge
// goroutine started by peer.go for whichever peer did show up -- ctx
// cancellation unblocks startOutboundBridge immediately, and closing that
// lone peer's own PeerConnection below unblocks startInboundBridge's
// blocking track.ReadRTP call the same way a real disconnect would), and
// that lone peer's own PeerConnection is closed too, since with the room
// gone nobody else is ever going to do so for it.
//
// If the room already reached two peers before this fired (Join stops
// the timer the moment that happens) or was already closed by a
// concurrent call, this is a no-op -- both are ordinary, expected races,
// not errors.
func (m *Manager) expireIncomplete(roomID string, room *Room) {
	room.mu.Lock()
	if room.closed || (room.caller != nil && room.agent != nil) {
		room.mu.Unlock()
		return
	}
	room.closed = true
	callerPeer, agentPeer := room.caller, room.agent
	room.caller, room.agent = nil, nil
	room.mu.Unlock()

	m.mu.Lock()
	// Only remove this exact Room instance: in the pathological case
	// where roomFor's stale-room retry already replaced this entry with a
	// brand new Room under the same roomID by the time this fires, that
	// new room must be left alone.
	if m.rooms[roomID] == room {
		delete(m.rooms, roomID)
	}
	m.mu.Unlock()

	room.cancel()
	if callerPeer != nil {
		_ = callerPeer.close()
	}
	if agentPeer != nil {
		_ = agentPeer.close()
	}
	if err := room.session.Close(); err != nil {
		log.Printf("webrtcgw: room %q: idle timeout: closing session: %v", roomID, err)
	}
	log.Printf("webrtcgw: room %q: closed -- idle timeout elapsed without both participants joining", roomID)
}

// leave removes role's peer from roomID. If both peers are now gone, the
// room's Session is closed, its idle timer (if still pending) is
// stopped, and the room itself is removed from the Manager -- a room
// with zero participants has no reason to keep its ASR/MT/TTS
// connections alive.
func (m *Manager) leave(roomID string, role peerRole) {
	m.mu.Lock()
	room, ok := m.rooms[roomID]
	if !ok {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	room.mu.Lock()
	if room.closed {
		// Already torn down -- most likely expireIncomplete beat us to
		// it (its own forced peer.close() call is what triggered this
		// leave in the first place, via OnConnectionStateChange). Nothing
		// left to do.
		room.mu.Unlock()
		return
	}
	switch role {
	case roleCaller:
		room.caller = nil
	case roleAgent:
		room.agent = nil
	}
	empty := room.caller == nil && room.agent == nil
	if empty {
		room.closed = true
	}
	idleTimer := room.idleTimer
	room.mu.Unlock()

	if !empty {
		return
	}

	// A room can become empty here before it was ever full (e.g. the one
	// peer who joined leaves again before a second peer ever arrives) --
	// its idle timer, if still pending, is now redundant; stop it so it
	// doesn't fire expireIncomplete against a room already gone.
	if idleTimer != nil {
		idleTimer.Stop()
	}

	m.mu.Lock()
	if m.rooms[roomID] == room {
		delete(m.rooms, roomID)
	}
	m.mu.Unlock()

	room.cancel()
	if err := room.session.Close(); err != nil {
		log.Printf("webrtcgw: room %q: closing session: %v", roomID, err)
	}
}

// RoomCount returns the number of currently active rooms. Exposed for
// tests and for the observability dashboard's room-count metric.
func (m *Manager) RoomCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}
