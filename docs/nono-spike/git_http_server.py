#!/usr/bin/env python3
"""Minimal git-http-backend server (smart HTTP, supports push).
Usage: git_http_server.py <port> <project_root> [certfile]
If certfile is given, serve HTTPS (cert must contain key too, or pass key via env).
"""
import os, sys, subprocess, ssl
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])
ROOT = os.path.abspath(sys.argv[2])
CERT = sys.argv[3] if len(sys.argv) > 3 else None

BACKEND = subprocess.run(["git", "--exec-path"], capture_output=True, text=True).stdout.strip()
BACKEND = os.path.join(BACKEND, "git-http-backend")


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _run(self):
        path = self.path
        qs = ""
        if "?" in path:
            path, qs = path.split("?", 1)
        env = dict(os.environ)
        env.update({
            "GIT_PROJECT_ROOT": ROOT,
            "GIT_HTTP_EXPORT_ALL": "1",
            "PATH_INFO": path,
            "QUERY_STRING": qs,
            "REQUEST_METHOD": self.command,
            "REMOTE_ADDR": self.client_address[0],
            "CONTENT_TYPE": self.headers.get("Content-Type", ""),
            "REMOTE_USER": self.headers.get("Authorization", ""),  # echo auth so we can log it
        })
        body = b""
        cl = self.headers.get("Content-Length")
        if cl is not None:
            body = self.rfile.read(int(cl))
        elif self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            while True:
                line = self.rfile.readline().strip()
                if not line:
                    continue
                size = int(line, 16)
                if size == 0:
                    self.rfile.readline()
                    break
                body += self.rfile.read(size)
                self.rfile.readline()
        env["CONTENT_LENGTH"] = str(len(body))
        proc = subprocess.run([BACKEND], input=body, env=env, capture_output=True)
        out = proc.stdout
        # split CGI headers / body
        sep = out.find(b"\r\n\r\n")
        if sep == -1:
            sep = out.find(b"\n\n")
            hdr, rest = out[:sep], out[sep + 2:]
        else:
            hdr, rest = out[:sep], out[sep + 4:]
        status = 200
        headers = []
        for line in hdr.split(b"\n"):
            line = line.strip()
            if not line:
                continue
            k, _, v = line.partition(b":")
            k = k.decode().strip(); v = v.decode().strip()
            if k.lower() == "status":
                status = int(v.split()[0])
            else:
                headers.append((k, v))
        sys.stderr.write(f"[gitsrv] {self.command} {self.path} auth={self.headers.get('Authorization','-')!r} -> {status} ({len(rest)}B, backend_err={proc.stderr[:120]!r})\n")
        sys.stderr.flush()
        self.send_response(status)
        have_len = any(k.lower() == "content-length" for k, _ in headers)
        for k, v in headers:
            self.send_header(k, v)
        if not have_len:
            self.send_header("Content-Length", str(len(rest)))
        self.end_headers()
        self.wfile.write(rest)

    def do_GET(self):
        self._run()

    def do_POST(self):
        self._run()

    def log_message(self, *a):
        pass


srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
if CERT:
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(CERT)
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
sys.stderr.write(f"[gitsrv] serving {ROOT} on {'https' if CERT else 'http'}://127.0.0.1:{PORT}  backend={BACKEND}\n")
sys.stderr.flush()
srv.serve_forever()
