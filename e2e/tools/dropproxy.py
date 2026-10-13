#!/usr/bin/env python3
"""Reverse proxy in front of a gno.land RPC that forwards every request but
drops the HTTP response of broadcast_tx_* calls (the connection is closed
after the node has received the tx).

It reproduces the "ambiguous broadcast" case katana must record as
`unconfirmed`: gnokey sees a transport error after the tx bytes were sent,
while the tx may still be committed on chain.

usage: dropproxy.py <listen_port> <upstream_url>
"""
import http.server
import sys
import urllib.request

LISTEN = int(sys.argv[1])
UPSTREAM = sys.argv[2].rstrip("/")


class Handler(http.server.BaseHTTPRequestHandler):
    def _forward(self, body):
        req = urllib.request.Request(UPSTREAM + self.path, data=body, method=self.command)
        for k, v in self.headers.items():
            if k.lower() not in ("host", "content-length", "connection"):
                req.add_header(k, v)
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, resp.headers, resp.read()

    def _handle(self):
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n) if n else None
        status, headers, data = self._forward(body)
        if b"broadcast_tx" in (body or b"") or "broadcast_tx" in self.path:
            sys.stderr.write("dropproxy: dropping response of %s\n" % self.path)
            self.close_connection = True
            self.connection.close()
            return
        self.send_response(status)
        for k, v in headers.items():
            if k.lower() not in ("transfer-encoding", "connection", "content-length"):
                self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    do_GET = _handle
    do_POST = _handle

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("127.0.0.1", LISTEN), Handler).serve_forever()
