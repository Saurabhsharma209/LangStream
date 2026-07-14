package webrtcgw

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static/index.html
var staticFS embed.FS

// StaticHandler serves the browser test client (a single self-contained
// HTML/JS page: getUserMedia + RTCPeerConnection + the signaling protocol
// SignalingHandler implements). It's embedded into the binary via
// go:embed rather than read from disk at runtime, so `go build` alone
// produces a fully self-contained gateway binary with no separate static
// asset directory to ship or find at a relative path.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// Only reachable if the embed directive above is broken (e.g. the
		// file was renamed/deleted) -- a build-time-guaranteed invariant,
		// not a runtime condition callers need to handle.
		panic("webrtcgw: static assets not embedded correctly: " + err.Error())
	}
	return http.FileServer(http.FS(sub))
}
