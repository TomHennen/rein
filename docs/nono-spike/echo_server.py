#!/usr/bin/env python3
"""Byte-counting sink: reads a POST body fully (Content-Length or chunked),
replies 200 JSON {"received_bytes": N, "auth": "..."}. For the raw >16MiB test."""
import sys, json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_POST(self):
        n = 0
        cl = self.headers.get("Content-Length")
        if cl is not None:
            remaining = int(cl)
            while remaining > 0:
                chunk = self.rfile.read(min(65536, remaining))
                if not chunk:
                    break
                n += len(chunk); remaining -= len(chunk)
            framing = "content-length"
        elif self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            while True:
                line = self.rfile.readline().strip()
                if not line:
                    continue
                size = int(line, 16)
                if size == 0:
                    self.rfile.readline(); break
                n += len(self.rfile.read(size)); self.rfile.readline()
            framing = "chunked"
        else:
            framing = "none"
        body = json.dumps({"received_bytes": n, "framing": framing,
                           "auth": self.headers.get("Authorization", "-")}).encode()
        sys.stderr.write(f"[echo] POST {self.path} framing={framing} bytes={n} auth={self.headers.get('Authorization','-')!r}\n")
        sys.stderr.flush()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
sys.stderr.write(f"[echo] sink on http://127.0.0.1:{PORT}\n"); sys.stderr.flush()
srv.serve_forever()
