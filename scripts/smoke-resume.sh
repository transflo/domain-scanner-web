#!/bin/sh
# Restart/resume check. Run in the compose network (see smoke.sh), in two phases with a
# `docker compose restart scanner` in between:
#   MODE=start  -> logs in, creates a long dictionary job, lets it run for WAIT seconds, prints it
#   MODE=check  -> logs in, prints the job again after WAIT seconds, then cancels it
# Env: PW (ADMIN_PASSWORD), MODE, JOB (for check), WAIT, B
B="${B:-http://scanner:8080}"
WAIT="${WAIT:-20}"
V=$(curl -s -i -X POST -H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" $B/api/auth/login \
  | grep -i '^set-cookie' | sed 's/.*ds_session=\([^;]*\);.*/\1/' | tr -d '\r')
C="Cookie: ds_session=$V"
show() { curl -s -H "$C" $B/api/jobs/$1 | sed 's/.*"status":"\([a-z]*\)","cursor":\([0-9]*\),"total":\([0-9]*\),"checked":\([0-9]*\),"available":\([0-9]*\),"unknown":\([0-9]*\),"registered":\([0-9]*\).*/status=\1 cursor=\2\/\3 checked=\4 available=\5 unknown=\6 registered=\7/'; }

if [ "$MODE" = "start" ]; then
  R=$(curl -s -H "$C" -X POST -H 'Content-Type: application/json' \
    -d '{"suffix":".com","wordlist":"builtin:google-10000-english","workers":2,"delay_ms":300,"name":"resume test"}' $B/api/jobs)
  ID=$(echo "$R" | sed -n 's/^{"id":\([0-9]*\),.*/\1/p')
  echo "created job $ID"
  sleep "$WAIT"
  echo "before restart: $(show $ID)"
else
  echo "after restart (immediately): $(show $JOB)"
  sleep "$WAIT"
  echo "after restart (+${WAIT}s):   $(show $JOB)"
  curl -s -H "$C" -X POST $B/api/jobs/$JOB/cancel >/dev/null
  sleep 2
  echo "cancelled:                   $(show $JOB)"
fi
