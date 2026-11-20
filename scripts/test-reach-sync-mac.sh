#!/usr/bin/env bash
# Offline regression for daybreak P1-2: reach-sync-mac.sh must never install
# the server's raw text. A fake API serves a malicious raw config (Match exec)
# and a valid JSON document; the result must come from the JSON via
# reach-mac-agent, and `ssh -G` must not run the marker command.
set -Eeuo pipefail
cd "$(dirname "$0")/.."
work="$(mktemp -d /tmp/reach-synctest.XXXXXX)"
srv=""
trap '[ -n "$srv" ] && kill "$srv" 2>/dev/null; trash "$work" 2>/dev/null || true' EXIT
go build -o "$work/reach-mac-agent" ./cmd/reach-mac-agent
marker="$work/PWNED"
cat > "$work/api.py" <<EOF
import http.server, json
RAW = 'Match exec "touch $marker"\n'
DOC = {"schema": 1, "hosts": [{"host": "jack", "hostname": "localhost", "port": 9210, "user": "arthur",
       "proxy_jump": "jason-public", "host_key_alias": "reach-m_abc123", "machine_id": "m_abc123", "observed_state": "online"}]}
class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        if self.path == "/api/admin/ssh-config":
            body = RAW.encode(); ctype = "text/plain"
        elif self.path == "/api/admin/ssh-config.json":
            body = json.dumps(DOC).encode(); ctype = "application/json"
        else:
            self.send_response(404); self.end_headers(); return
        self.send_response(200); self.send_header("Content-Type", ctype); self.end_headers(); self.wfile.write(body)
s = http.server.HTTPServer(("127.0.0.1", 0), H)
print(s.server_address[1], flush=True)
s.serve_forever()
EOF
python3 "$work/api.py" > "$work/port" &
srv=$!
for _ in $(seq 1 50); do [ -s "$work/port" ] && break; sleep 0.1; done
port="$(cat "$work/port")"
printf 'sat_test\n' > "$work/token"
REACH_MAC_AGENT="$work/reach-mac-agent" bash scripts/reach-sync-mac.sh \
  --api-url "http://127.0.0.1:$port" --token-file "$work/token" --out "$work/reach.conf" --no-include
grep -q '^Host jack$' "$work/reach.conf" || { echo "FAIL: JSON host not written"; exit 1; }
if grep -q 'Match' "$work/reach.conf"; then echo "FAIL: raw server text reached the config"; exit 1; fi
ssh -G -F "$work/reach.conf" jack >/dev/null
[ ! -e "$marker" ] || { echo "FAIL: marker command executed"; exit 1; }
echo "ok: reach-sync-mac.sh wrote only template output; raw Match exec ignored"
# Remote plain http is refused before any login or token use (daybreak
# round 3 P2-B).
for bad in http://10.0.0.1:9300 http://localhost.evil.example http://user@127.0.0.1:1; do
  if REACH_MAC_AGENT="$work/reach-mac-agent" bash scripts/reach-sync-mac.sh --api-url "$bad" \
      --token-file "$work/token" --out "$work/bad.conf" --no-include >/dev/null 2>&1; then
    echo "FAIL: accepted $bad"; exit 1
  fi
done
echo "ok: reach-sync-mac.sh refuses non-loopback http API URLs"
