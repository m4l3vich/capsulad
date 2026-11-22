#!/bin/sh

set -e
echo "[*] Choose a new password for your admin account. Input is not shown."

stty -echo
printf "[*] Enter your admin password: "
read -r password1
printf "\n[*] Repeat the same password: "
read -r password2
stty echo
printf "\n"

if [ -z "$password1" ]; then
  echo "[!] Password must not be empty"
  exit 1
fi

if [ "$password1" != "$password2" ]; then
  echo "[!] The passwords do not match. Try again"
  exit 1
fi

printf '%s\n' "$password1" | docker compose exec -T cadence /cadence-server admin password set