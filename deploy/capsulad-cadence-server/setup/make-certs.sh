set -eu
umask 077
HOST="${CADENCE_HOST:?}"
apk add -q --no-cache openssl
mkdir -p /certs /out

echo ""

# a literal address gets an IP SAN, anything else a DNS one
if printf '%s' "$HOST" | grep -Eq '^[0-9]{1,3}(\.[0-9]{1,3}){3}$'; then
  SAN="IP:$HOST"
elif printf '%s' "$HOST" | grep -q ':'; then
  SAN="IP:$HOST"
else
  SAN="DNS:$HOST"
fi

# --- master key: seals provider keys in the database, 32 random bytes, base64
if [ -s /keys/master.key ]; then
  echo "[!] Master key present, leaving it alone (replacing it loses every stored provider key)"
else
  head -c 32 /dev/urandom | base64 > /keys/master.key
  chmod 600 /keys/master.key
  # the server runs as nonroot (uid 65532) and refuses a key readable by anyone else
  chown 65532:65532 /keys/master.key
  echo "[*] Generated ./data/keys/master.key"
fi

# --- tls: one CA, a server cert for $HOST, one client cert
if [ -s /certs/ca.crt ] && [ -s /certs/server.crt ] && [ -s /certs/server.key ]; then
  echo "[!] Certs already issued: $(openssl x509 -in /certs/server.crt -noout -subject)"
  echo "[!] If you REALLY need to regenerate keys, delete the ./data/certs and ./data/client directories first"
else
  cd /certs
  echo "[*] Generating CA... "
  openssl req -quiet -x509 -newkey rsa:2048 -nodes -days 3650 \
    -subj '/CN=cadence-ca' -keyout ca.key -out ca.crt

  echo "[*] Generating server key for $HOST... "
  openssl req -quiet -newkey rsa:2048 -nodes -subj "/CN=$HOST" -keyout server.key -out server.csr
  printf 'subjectAltName=%s\nextendedKeyUsage=serverAuth\n' "$SAN" > server.ext

  echo "[*] Generating server cert... "
  openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
    -days 3650 -extfile server.ext -out server.crt

  rm -f server.csr server.ext client.ext
  chmod 600 ca.key server.key
  chmod 644 ca.crt server.crt
fi

# --- what curl / the browser needs, on the host
install -m 644 /certs/ca.crt /out/ca.crt
echo ""
echo "[*] DONE: CA public key exported to ./data/ca.crt"