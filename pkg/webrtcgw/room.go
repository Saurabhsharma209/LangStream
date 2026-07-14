package webrtcgw

import (
	"context"
	"fmt"
	"log"
	"sync"

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
}

// Manager creates and tracks Rooms by ID. It is safe for concurrent use --
// multiple browsers may call Join concurrently (e.g. both participants'
// initial WebSocket connections racing in).
type Manager struct {
	newSession SessionFactory

	mu    sync.Mutex
	rooms map[string]*Room
}

// NewManager returns a Manager that builds each room's Session via
// newSession (see SessionFactory).
func NewManager(newSession SessionFactory) *Manager {
	return &Manager{newSession: newSession, rooms: make(map[string]*Room)}
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

	m.mu.Lock()
	room, ok := m.rooms[roomID]
	if !ok {
		ctx, cancel := context.WithCancel(context.Background())
		sess, err := m.newSession(ctx)
		if err != nil {
			cancel()
			m.mu.Unlock()
			return nil, nil, fmt.Errorf("webrtcgw: creating session for room %q: %w", roomID, err)
		}
		room = &Room{id: roomID, session: sess, ctx: ctx, cancel: cancel}
		m.rooms[roomID] = room
	}
	m.mu.Unlock()

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

	p.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed || state == webrtc.PeerConnectionStateDisconnected {
			m.leave(roomID, role)
		}
	})

	return p, room, nil
}

// leave removes role's peer from roomID. If both peers are now gone, the
// room's Session is closed and the room itself is removed from the
// Manager -- a room with zero participants has no reason to keep its
// ASR/MT/TTS connections alive.
func (m *Manager) leave(roomID string, role peerRole) {
	m.mu.Lock()
	room, ok := m.rooms[roomID]
	if !ok {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()

	room.mu.Lock()
	switch role {
	case roleCaller:
		room.caller = nil
	case roleAgent:
		room.agent = nil
	}
	empty := room.caller == nil && room.agent == nil
	room.mu.Unlock()

	if empty {
		m.mu.Lock()
		delete(m.rooms, roomID)
		m.mu.Unlock()

		room.cancel()
		if err := room.session.Close(); err != nil {
			log.Printf("webrtcgw: room %q: closing session: %v", roomID, err)
		}
	}
}

// RoomCount returns the number of currently active rooms. Exposed for
// tests and for the observability dashboard's room-count metric.
func (m *Manager) RoomCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rooms)
}
