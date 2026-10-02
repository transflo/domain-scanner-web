#!/bin/sh
# End-to-end API smoke test. Run inside the compose network, e.g.:
#   docker run --rm --network domain-scanner_default -e PW=... -v "$PWD/scripts:/s" \
#     --entrypoint sh curlimages/curl /s/smoke.sh
# Env: B (base url, default http://scanner:8080), PW (ADMIN_PASSWORD), WAIT (seconds to let the scan run)
# Note: the session cookie is passed by hand because curl's cookie jar does not resend cookies
# for single-label host names such as "scanner".
B="${B:-http://scanner:8080}"
WAIT="${WAIT:-20}"
code() { curl -s -o /dev/null -w "%{http_code}" "$@"; }

echo "unauthenticated /api/jobs      : $(code $B/api/jobs)   (want 401)"
echo "health (public)                : $(code $B/api/health)   (want 200)"
echo "login with wrong password      : $(code -X POST -H 'Content-Type: application/json' -d '{"password":"nope-nope"}' $B/api/auth/login)   (want 401)"

V=$(curl -s -i -X POST -H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" $B/api/auth/login \
  | grep -i '^set-cookie' | sed 's/.*ds_session=\([^;]*\);.*/\1/' | tr -d '\r')
[ -n "$V" ] && echo "login                          : got session cookie" || echo "login                          : FAILED"
C="Cookie: ds_session=$V"
echo "authenticated /api/jobs        : $(code -H "$C" $B/api/jobs)   (want 200)"

echo "--- create job"
curl -s -H "$C" -X POST -H 'Content-Type: application/json' \
  -d '{"suffix":".li","pattern":"d","length":2,"workers":4,"delay_ms":0,"name":"smoke .li 2d"}' $B/api/jobs
echo
sleep "$WAIT"
echo "--- job 1"
curl -s -H "$C" $B/api/jobs/1
echo
echo "--- results (available, first 5)"
curl -s -H "$C" "$B/api/results?status=available&limit=5"
echo
echo "--- logs (info+, last 8)"
curl -s -H "$C" "$B/api/logs?level=info&limit=8"
echo
