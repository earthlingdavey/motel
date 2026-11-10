"""The notify suite's receiver (e2e/notify/run.sh): stands in for Slack,
Discord, ntfy and Healthchecks on the compose network.

Every POST is one delivery from Alertmanager. It is logged, one line
per alert in it, as

    <epoch ms> <path> <group status> <alertname> <app|-> <alert status> <notify_channel|->

and GET /log returns the lines so far. /broken answers 400, the way a
revoked Discord hook does, so a channel pointed at it fails and
Alertmanager counts the failure. Stdlib only.
"""

import http.server
import json
import threading
import time

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
        out = []
        if isinstance(alerts, list):
            for a in alerts:
                labels = a.get("labels", {})
                out.append("%d %s %s %s %s %s %s" % (
                    now, self.path, body.get("status", "-"),
                    labels.get("alertname", "-"), labels.get("app", "-"),
                    a.get("status", "-"), labels.get("notify_channel", "-")))
        else:
            # Slack's and Discord's bodies carry text, not alerts.
            out.append("%d %s - - - - -" % (now, self.path))
        with LOCK:
            LINES.extend(out)
        code = 400 if self.path.startswith("/broken") else 200
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        with LOCK:
            data = ("\n".join(LINES) + "\n").encode() if LINES else b""
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Sink).serve_forever()
