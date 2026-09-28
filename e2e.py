import json, threading, time, urllib.request

BASE = "http://127.0.0.1:8787"
MODEL = "deepseek:deepseek-flash"

def call(path, token, body=None, method=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method or ("POST" if data else "GET"))
    req.add_header("Authorization", "Bearer " + token)
    if data:
        req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=30) as r:
        raw = r.read()
        return json.loads(raw) if raw else None

token = json.load(urllib.request.urlopen(BASE + "/v1/bootstrap", timeout=10))["token"]
info = call("/v1/sessions", token, {"model": MODEL})
sid = info["id"]
print("session:", sid, "state:", info["state"], "model:", info.get("model"))

events = []
def listen():
    req = urllib.request.Request(BASE + f"/events?session={sid}")
    req.add_header("Cookie", "marxagent_token=" + token)
    req.add_header("Accept", "text/event-stream")
    with urllib.request.urlopen(req, timeout=90) as r:
        for raw in r:
            line = raw.decode("utf-8").strip()
            if line.startswith("data:"):
                try:
                    events.append(json.loads(line[5:].strip()))
                except Exception as e:
                    events.append({"type": "UNPARSEABLE", "raw": line[:80]})

t = threading.Thread(target=listen, daemon=True); t.start()
time.sleep(1.0)   # the window is attached before the message goes in

call(f"/v1/sessions/{sid}/messages", token, {"text": "只回复两个字：收到", "model": MODEL})

deadline = time.time() + 60
while time.time() < deadline:
    if any(e.get("type") == "done" for e in events):
        break
    time.sleep(0.3)

from collections import Counter
print("event types:", dict(Counter(e.get("type") for e in events)))
partials = [e for e in events if e.get("type") == "partial"]
print("partial count:", len(partials), "sample:", json.dumps(partials[0], ensure_ascii=False)[:160] if partials else "none")
sc = [e for e in events if e.get("type") == "state_change"]
print("state_change messages:", [(e["data"]["message"]["role"], "".join(b.get("text","") for b in e["data"]["message"]["content"])[:40]) for e in sc])
sess = [e for e in events if e.get("type") == "session"]
print("session events:", len(sess), "last state:", sess[-1]["session"]["state"] if sess else None, "title:", sess[-1]["session"].get("title") if sess else None)
errs = [e for e in events if e.get("type") == "error"]
print("errors:", json.dumps(errs, ensure_ascii=False)[:300] if errs else "none")

full = call(f"/v1/sessions/{sid}", token)
print("history:", [(m["role"], "".join(b.get("text","") for b in m["content"])[:60]) for m in full["messages"]])
print("SESSION_ID=" + sid)
