package daemon

// The control protocol is newline-delimited JSON over the unix socket: the
// client writes one Request object per line, the daemon replies with one
// Response object per line. This is a Phase 1 skeleton — only "ping" is wired
// — but the request/response shapes and the dispatch switch (handleLine) are
// structured so token/approval methods slot in without reworking the framing.

// Request is one control-protocol call. Method names the operation; the
// remaining fields are method-specific and grow as methods are added.
type Request struct {
	Method string `json:"method"`
}

// Response is one control-protocol reply. OK reports success; Error carries a
// human-readable reason when OK is false. Method-specific result fields (e.g.
// Pong) are added alongside and omitted when empty.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Pong  bool   `json:"pong,omitempty"`
}

// dispatch routes a decoded request to its handler and returns the reply. An
// unknown method yields an error response, never a panic — a malformed or
// future-versioned client must not take the daemon down.
func (d *Daemon) dispatch(req Request) Response {
	switch req.Method {
	case "ping":
		return Response{OK: true, Pong: true}
	default:
		return Response{OK: false, Error: "unknown method: " + req.Method}
	}
}
