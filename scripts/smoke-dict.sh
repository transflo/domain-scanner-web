#!/bin/sh
# Classification sanity check with known names (real network). Same invocation as smoke.sh.
# Expected: google/example/wikipedia/nic/switch => registered, the zzqx*/qqzz* names => available.
B="${B:-http://scanner:8080}"
WAIT="${WAIT:-25}"
V=$(curl -s -i -X POST -H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" $B/api/auth/login \
  | grep -i '^set-cookie' | sed 's/.*ds_session=\([^;]*\);.*/\1/' | tr -d '\r')
C="Cookie: ds_session=$V"

printf 'google\nexample\nwikipedia\nnic\nswitch\nzzqxnotexistabc123\nqqzzpxnotexist4567\n' > /tmp/probe.txt
echo "upload wordlist:"
curl -s -H "$C" -F "name=probe" -F "file=@/tmp/probe.txt" $B/api/wordlists
echo

for sfx in com ch; do
  R=$(curl -s -H "$C" -X POST -H 'Content-Type: application/json' \
    -d "{\"suffix\":\".$sfx\",\"wordlist\":\"user:probe\",\"workers\":2,\"name\":\"probe .$sfx\"}" $B/api/jobs)
  ID=$(echo "$R" | sed -n 's/^{"id":\([0-9]*\),.*/\1/p')
  echo "job $ID (.$sfx) created"
  eval "ID_$sfx=$ID"
done
sleep "$WAIT"
for sfx in com ch; do
  eval "ID=\$ID_$sfx"
  echo "--- job $ID (.$sfx)"
  curl -s -H "$C" $B/api/jobs/$ID | sed 's/"created_at.*//'
  echo
  echo "available:"
  curl -s -H "$C" "$B/api/results?job_id=$ID&status=available" | sed 's/},{/},\n{/g'
  echo
  echo "unknown:"
  curl -s -H "$C" "$B/api/results?job_id=$ID&status=unknown" | sed 's/},{/},\n{/g'
  echo
done
