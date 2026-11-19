#!/usr/bin/env bash
# End-to-end tests: a headless Pulse app with throwaway data, driven by the
# real CLI and Python helper. Usage: test/e2e/run.sh   (exit 1 on failure)
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="$(mktemp -d)"
export PULSE_HOME="$WORK/home"
unset PULSE_URL PULSE_TOKEN PULSE_RUN PULSE_JOB
BIN="$WORK/bin"; mkdir -p "$BIN" "$PULSE_HOME" "$WORK/proj"
P="$BIN/pulse"
FAILS=0

cleanup() { [ -n "${APP:-}" ] && kill "$APP" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n      %s\n' "$1" "$2"; FAILS=$((FAILS+1)); }
expect() { # expect <name> <actual> <regex>
  if [[ "$2" =~ $3 ]]; then pass "$1"; else fail "$1" "got: $(echo "$2" | head -3)"; fi
}
field() { # field <job> <python expr on run dict r>
  "$P" show "$1" --json 2>/dev/null | python3 -c "import json,sys; r=json.load(sys.stdin); print($2)" 2>/dev/null
}

echo "Building…"
(cd "$ROOT" && go build -o "$P" ./cmd/pulse && go build -o "$BIN/pulse-app" ./cmd/pulse-app) 2>&1 | grep -v "ld: warning" | grep -v "^# pulse" && exit 1
"$BIN/pulse-app" --headless --addr 127.0.0.1:0 > "$WORK/app.log" 2>&1 &
APP=$!
for _ in $(seq 50); do "$P" status >/dev/null 2>&1 && break; sleep 0.1; done
cd "$WORK/proj"

echo "pulse run"
cat > ok.py <<'PY'
import sys, time
for e in range(1, 4):
    for b in range(1, 11):
        sys.stdout.write(f"\rEpoch {e}/3: {b*10:3d}%|{'█'*b}| {b}/10 [00:00<00:00, 9it/s, loss={1/(e*b):.3f}]"); sys.stdout.flush(); time.sleep(0.01)
    print()
PY
"$P" run -n ok -- python3 ok.py > /dev/null; code=$?
expect "exit code of a good command" "$code" '^0$'
expect "status done" "$(field ok 'r["status"]')" '^done$'
expect "epoch bar from tqdm description" "$(field ok '[p["name"] for p in r["progress"]]')" "epoch"
printf 'print("Traceback");print("RuntimeError: boom");import sys;sys.exit(3)\n' > bad.py
"$P" run -n bad -- python3 bad.py > /dev/null 2>&1; code=$?
expect "exit code of a failing command" "$code" '^3$'
expect "failure message is the error" "$(field bad 'r["message"]')" "RuntimeError: boom"

echo "pulse push"
"$P" push dep status=running progress=2/5 current=a.mp4 gb=1.5
expect "progress= is the main bar" "$(field dep 'r["progress"][0]["name"]')" '^main$'
expect "text value kept as a field" "$(field dep 'r["fields"][0]["value"]')" '^a.mp4$'
"$P" push dep --new progress=1/5
expect "--new closes the replaced run" "$(field 'dep#1' 'r["status"], r["message"]')" "stopped.*Replaced by run #2"
"$P" push dep gb=
expect "empty value removes a metric" "$(field dep 'len(r["metrics"])')" '^0$'

echo "stop and alerts"
printf 'import time\nfor i in range(600):\n    print(f"step {i}/600 loss={1/(i+1)}", flush=True); time.sleep(0.05)\n' > long.py
"$P" run -n long -- python3 long.py > /dev/null & RUN=$!
sleep 1.5; "$P" stop long > /dev/null; wait $RUN
expect "pulse stop → stopped" "$(field long 'r["status"]')" '^stopped$'
printf 'import time\nfor i in range(1, 400):\n    print(f"step {i}/400 loss={1/i if i < 20 else float(\"nan\")}", flush=True); time.sleep(0.02)\n' > nan.py
"$P" alert div 'loss is nan' --run stop > /dev/null
"$P" run -n div -- python3 nan.py > /dev/null
expect "NaN alert stops the run" "$(field div 'r["status"]')" '^stopped$'
expect "NaN alert notified" "$(grep -c 'NOTIFY.*div: loss is NaN' "$WORK/app.log")" '^1$'

echo "watch"
printf '{"done": 3, "total": 10}' > p.json
"$P" watch w --file p.json --every 5s --map progress=done/total > /dev/null; sleep 1
printf '{"done": 4,' > p.json; sleep 5.5
expect "half-written file skipped" "$(field w 'r["progress"][0]["cur"]')" '^3'
printf '{"done": 5, "total": 10}' > p.json; sleep 5.5
expect "valid file picked up" "$(field w 'r["progress"][0]["cur"]')" '^5'
"$P" watch rm w > /dev/null

echo "tokens"
TOK=$("$P" remote token tj | grep -o 'pls_[0-9a-f]*')
URL=$(python3 -c "import json; print(json.load(open('$PULSE_HOME/server.json'))['url'])")
expect "job token: own job" "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK" -d progress=1/2 "$URL/api/v1/push/tj")" '^200$'
expect "job token: other job" "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK" -d progress=1/2 "$URL/api/v1/push/other")" '^403$'
expect "job token: no reads" "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK" "$URL/api/v1/runs")" '^403$'

echo "questions"
("$P" ask q "Ship?" yes no > ans.txt 2>/dev/null) & A=$!
sleep 1; QID=$("$P" inbox --json | python3 -c "import json,sys; print(json.load(sys.stdin)[0]['id'])")
"$P" answer "$QID" yes > /dev/null; wait $A
expect "pulse ask prints the answer" "$(cat ans.txt)" '^yes$'
"$P" ask q "Again?" yes no --timeout 2s > /dev/null 2>&1; code=$?
expect "pulse ask times out with exit 3" "$code" '^3$'

echo "python helper"
cat > helper.py <<'PY'
import pulse, time
hits = []
@pulse.on("ping")
def _(): hits.append(1)
for i in pulse.track(range(20), name="step"):
    pulse.log(loss=1 / (i + 1)); time.sleep(0.05)
time.sleep(2)
print("answer:", pulse.ask("OK?", ["go", "stop"], timeout="1m"), "pings:", len(hits))
PY
"$P" action hp ping --send ping > /dev/null
PYTHONPATH="$ROOT/sdk/python/src" "$P" run -n hp -- python3 helper.py > helper.out 2>&1 & RUN=$!
sleep 2; "$P" do hp ping > /dev/null; sleep 2
QID=$("$P" inbox --json | python3 -c "import json,sys; print([a['id'] for a in json.load(sys.stdin) if a['job']=='hp'][0])" 2>/dev/null)
"$P" answer "$QID" go > /dev/null 2>&1; wait $RUN
expect "helper: command handled, question answered" "$(cat helper.out)" "answer: go pings: 1"
expect "helper: metrics reported once" "$(field hp '[m["n"] for m in r["metrics"] if m["name"]=="loss"][0]')" '^20$'

echo
if [ "$FAILS" -eq 0 ]; then echo "All end-to-end checks passed."; else echo "$FAILS check(s) failed."; exit 1; fi
