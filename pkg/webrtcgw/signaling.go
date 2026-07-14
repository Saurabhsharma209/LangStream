// Signaling server: a thin WebSocket-based SDP/ICE exchange in front of
// Manager. Wire protocol (JSON messages, one WebSocket connection per
// browser participant):
//
// Client -> server:
//
//	{"type":"join","room":"<id>","role":"caller"|"agent"}
//	{"type":"offer","sdp":"<SDP offer>"}
//	{"type":"candidate","candidate":{"candidate":"...","sdpMid":...,"sdpMLineIndex":...}}
//
// Server -> client:
//
//	{"type":"answer","sdp":"<SDP answer>"}
//	{"type":"candidate","candidate":{...}}   -- this gateway's own ICE candidates, trickled
//	{"type":"error","message":"<...>"}
//
// The client must send exactly one "join" first, then exactly one
// "offer" (this gateway only ever answers, never initiates -- see
// alaw.go's package doc comment for why the browser is always the
// offerer), then any number of "candidate" messages as its own ICE
// gathering produces them (trickle ICE) in either order relative to the
// offer/answer exchange (peer.go's addICECandidate buffers candidates
// that arrive before SetRemoteDescription has happened yet).
package webrtcgw

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

// wireMessage is the superset of every message shape this protocol uses,
// in either direction; fields irrelevant to a given "type" are simply
// left at their zero value.
type wireMessage struct {
	Type      string                   `json:"type"`
	Room      string                   `json:"room,omitempty"`
	Role      string                   `json:"role,omitempty"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	Message   string                   `json:"message,omitempty"`
}

var upgrader = websocket.Upgrader{
	// This gateway is a local test tool (see pkg/webrtcgw's package doc
	// comment and cmd/langstream's webrtc subcommand help text) meant to
	// run on localhost or a developer's own machine/network, not as a
	// public multi-tenant service -- accepting any origin is appropriate
	// here the same way it is in this repo's existing test-server helpers
	// (see e.g. pkg/asr/sarvam_test.go's newFakeSarvamServer), not a
	// production CORS/CSRF posture.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// SignalingHandler returns an http.Handler that upgrades each request to a
// WebSocket and runs the join/offer/answer/ICE exchange against mgr for
// its lifetime. iceServers configures every PeerConnection this handler
// creates (see Manager.Join).
func SignalingHandler(mgr *Manager, iceServers []webrtc.ICEServer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("webrtcgw: websocket upgrade: %v", err)
			return
		}
		defer conn.Close()

		var writeMu sync.Mutex
		writeJSON := func(v wireMessage) error {
			writeMu.Lock()
			defer writeMu.Unlock()
			return conn.WriteJSON(v)
		}

		var joinMsg wireMessage
		if err := conn.ReadJSON(&joinMsg); err != nil {
			return
		}
		if joinMsg.Type != "join" || joinMsg.Room == "" || (joinMsg.Role != "caller" && joinMsg.Role != "agent") {
			_ = writeJSON(wireMessage{Type: "error", Message: `expected {"type":"join","room":"...","role":"caller"|"agent"} first`})
			return
		}
		role := roleCaller
		if joinMsg.Role == "agent" {
			role = roleAgent
		}

		p, _, err := mgr.Join(joinMsg.Room, role, iceServers, func(err error) {
			log.Printf("webrtcgw: room %q (%s): %v", joinMsg.Room, role, err)
		})
		if err != nil {
			_ = writeJSON(wireMessage{Type: "error", Message: err.Error()})
			return
		}
		defer mgr.leave(joinMsg.Room, role)
		defer p.close()

		p.setICEForward(func(c webrtc.ICECandidateInit) {
			_ = writeJSON(wireMessage{Type: "candidate", Candidate: &c})
		})

		for {
			var msg wireMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg.Type {
			case "offer":
				answer, err := p.handleOffer(msg.SDP)
				if err != nil {
					_ = writeJSON(wireMessage{Type: "error", Message: err.Error()})
					continue
				}
				if err := writeJSON(wireMessage{Type: "answer", SDP: answer}); err != nil {
					return
				}
			case "candidate":
				if msg.Candidate == nil {
					continue
				}
				if err := p.addICECandidate(*msg.Candidate); err != nil {
					log.Printf("webrtcgw: room %q (%s): adding ICE candidate: %v", joinMsg.Room, role, err)
				}
			default:
				_ = writeJSON(wireMessage{Type: "error", Message: fmt.Sprintf("unknown message type %q", msg.Type)})
			}
		}
	})
}
