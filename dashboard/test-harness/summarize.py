# /// script
# requires-python = ">=3.11"
# ///
"""Turn run-compare.sh JSON into a markdown comparison table.
   uv run summarize.py <results-dir>"""
import json
import sys
from pathlib import Path

d = Path(sys.argv[1])
runs = {n: json.loads((d / f"{n}-all.json").read_text()) for n in ["before", "after", "after-oldsrv"] if (d / f"{n}-all.json").exists()}
names = list(runs)


def row(label, get):
    cells = []
    for n in names:
        try:
            cells.append(str(get(runs[n])))
        except (KeyError, TypeError):
            cells.append("—")
    print(f"| {label} | " + " | ".join(cells) + " |")


print("| metric | " + " | ".join(names) + " |")
print("|---|" + "---|" * len(names))
for k in ["load_desktop_40", "load_desktop_40_gzip", "load_phone_40", "load_phone_40_gzip", "load_desktop_200"]:
    row(f"{k}: fleet visible ms", lambda r, k=k: r[k]["fleetVisibleMs"])
    row(f"{k}: FCP ms", lambda r, k=k: r[k]["fcp"])
    row(f"{k}: transfer KB", lambda r, k=k: r[k]["transferKB"])
    row(f"{k}: API requests", lambda r, k=k: r[k]["apiTotal"])
    row(f"{k}: long tasks (max ms)", lambda r, k=k: f'{r[k]["longTasks"]} ({r[k]["longTaskMaxMs"]})')
    row(f"{k}: DOM nodes / heap MB", lambda r, k=k: f'{r[k]["nodes"]} / {r[k]["heapMB"]}')
for k in ["idle_realistic_desktop", "heartbeat_50ps_10s_desktop", "heartbeat_50ps_10s_phone_cpu4x", "flap_20ps_10s_desktop",
          "flap_20ps_10s_desktop_detail_open", "flap_20ps_10s_phone_cpu4x", "flap_20ps_10s_slowapi_desktop"]:
    row(f"{k}: API requests", lambda r, k=k: r[k]["apiTotal"])
    row(f"{k}: max in flight", lambda r, k=k: r[k]["maxInflight"])
    row(f"{k}: long tasks (max ms)", lambda r, k=k: f'{r[k]["longTasks"]} ({r[k]["longTaskMaxMs"]})')
    row(f"{k}: DOM mutations", lambda r, k=k: r[k]["domMutations"])
    row(f"{k}: main-thread task ms", lambda r, k=k: r[k]["delta"]["taskMs"])
row("phone tap-to-detail during flap storm, ms", lambda r: r["phone_tap_to_detail_during_flap_ms"])
row("leak: heap MB t=0 -> t=120s", lambda r: f'{r["leak"]["samples"][0]["heapMB"]} -> {r["leak"]["samples"][-1]["heapMB"]}')
row("leak: DOM nodes t=0 -> t=120s", lambda r: f'{r["leak"]["samples"][0]["nodes"]} -> {r["leak"]["samples"][-1]["nodes"]}')
row("leak: listeners t=0 -> t=120s", lambda r: f'{r["leak"]["samples"][0]["listeners"]} -> {r["leak"]["samples"][-1]["listeners"]}')
row("10 SSE drops: open streams after", lambda r: r["leak"]["reconnect10"]["sseClientsNow"])
row("10 SSE drops: API calls during reconnects", lambda r: json.dumps(r["leak"]["reconnect10"]["apiCalls"]))
