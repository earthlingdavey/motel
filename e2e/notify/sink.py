"""The notify suite's receiver (e2e/notify/run.sh): stands in for Slack,
Discord, ntfy and Healthchecks on the compose network.

Every POST is one delivery from Alertmanager. It is logged, one line
per alert in it, as

    <epoch ms> <path> <group status> <alertname> <app|-> <alert status> <notify_channel|-> [<X-Priority>]

and GET /log returns the lines so far; GET /log?mine=1 those that came
from the asker's own address, which is how each of `make e2e-notify`'s
boxes, sharing this sink, reads its own Alertmanager's deliveries and
motel's. /broken answers 400, the way a revoked Discord hook does, so a
channel pointed at it fails and Alertmanager counts the failure. Stdlib
only.
"""

import http.server
import json
import threading
import time
import urllib.parse

LINES = []
LOCK = threading.Lock()


class Sink(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n)
        now = int(time.time() * 1000)
        try:
            body = json.loads(raw)
        except ValueError:
            body = {}
        alerts = body.get("alerts")
        # ntfy's priority, which motel's render sends as a header on an
        # ntfy channel's critical and warning receivers: an eighth field
        # when present.
        prio = self.headers.get("X-Priority")
        tail = " " + prio if prio else ""
        out = []
        if isinstance(alerts, list):
            for a in alerts:
                labels = a.get("labels", {})
                out.append("%d %s %s %s %s %s %s%s" % (
                    now, self.path, body.get("status", "-"),
                    labels.get("alertname", "-"), labels.get("app", "-"),
                    a.get("status", "-"), labels.get("notify_channel", "-"), tail))
        else:
            # Slack's and Discord's bodies carry text, not alerts.
            out.append("%d %s - - - - -" % (now, self.path))
        with LOCK:
            LINES.extend((self.client_address[0], line) for line in out)
        code = 400 if self.path.startswith("/broken") else 200
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        url = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(url.query, keep_blank_values=True)
        # /log or /log?mine=1 alone: any other ask is refused, so a
        # misspelt filter is a 400, never every box's lines.
        if url.path != "/log" or set(query) - {"mine"} or query.get("mine", ["1"]) != ["1"]:
            self.send_response(400)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        mine = "mine" in query
        with LOCK:
            lines = [line for addr, line in LINES if not mine or addr == self.client_address[0]]
        data = ("\n".join(lines) + "\n").encode() if lines else b""
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Sink).serve_forever()
