#!/bin/sh
# Outbound-proxy integration check against a real xray. Needs an upstream SOCKS5 proxy reachable
# from the scanner container (see the README: run xray with a socks inbound as "testproxy").
# Run in the compose network, like smoke.sh. Env: PW (ADMIN_PASSWORD), UPSTREAM (default testproxy:1080)
B="${B:-http://scanner:8080}"
UP="${UPSTREAM:-testproxy:1080}"
V=$(curl -s -i -X POST -H 'Content-Type: application/json' -d "{\"password\":\"$PW\"}" $B/api/auth/login \
  | grep -i '^set-cookie' | sed 's/.*ds_session=\([^;]*\);.*/\1/' | tr -d '\r')
C="Cookie: ds_session=$V"
J='Content-Type: application/json'
short() { sed 's/"config":{[^}]*}[^}]*}//' | cut -c1-420; }

echo "=== 1. import a socks5 link (save=true)"
curl -s -H "$C" -H "$J" -X POST -d "{\"text\":\"socks5://$UP#upstream\",\"save\":true}" $B/api/outbounds/import | short; echo
ID=$(curl -s -H "$C" $B/api/outbounds | sed -n 's/.*"items":\[{"id":\([0-9]*\),.*/\1/p')
echo "outbound id: $ID"

echo "=== 2. test it (through the running xray)"
curl -s -H "$C" -X POST $B/api/outbounds/$ID/test; echo

echo "=== 3. test an unsaved config on a temporary xray instance (freedom = direct via xray)"
curl -s -H "$C" -H "$J" -X POST -d '{"config":{"protocol":"freedom"}}' $B/api/outbounds/test-config; echo
echo "--- a config xray itself rejects must be reported, not hang:"
curl -s -H "$C" -H "$J" -X POST -d '{"config":{"protocol":"vless","settings":{"address":"127.0.0.1","port":9,"id":"not-a-uuid"}}}' $B/api/outbounds/test-config; echo

echo "=== 4. list + egresses"
curl -s -H "$C" $B/api/outbounds | short; echo
curl -s -H "$C" $B/api/egresses; echo

echo "=== 5. job through that proxy"
R=$(curl -s -H "$C" -H "$J" -X POST -d "{\"suffix\":\".com\",\"wordlist\":\"user:probe\",\"workers\":2,\"name\":\"via proxy\",\"egress_mode\":\"proxy\",\"proxy_id\":$ID}" $B/api/jobs)
echo "$R" | cut -c1-300
JID=$(echo "$R" | sed -n 's/^{"id":\([0-9]*\),.*/\1/p')
sleep "${WAIT:-20}"
curl -s -H "$C" $B/api/jobs/$JID | sed 's/"created_at.*//' | cut -c1-400; echo
echo "--- diagnostics by egress:"
curl -s -H "$C" "$B/api/diagnostics?hours=1" | sed -n 's/.*"by_egress":\(\[[^]]*\]\).*/\1/p' | cut -c1-600; echo
echo "--- what the proxy handled (check.done lines for egress proxy-$ID):"
curl -s -H "$C" "$B/api/logs?component=check&event=done&egress=proxy-$ID&limit=8" | sed 's/},{/},\n{/g' | cut -c1-260
