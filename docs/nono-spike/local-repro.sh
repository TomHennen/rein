#!/usr/bin/env bash
# Self-contained local reproduction of the nono git-push chunked-hang finding.
# Tests nono's PROXY TRANSPORT only (no Landlock needed). Target is a local
# git-http-backend, so it needs NO GitHub credentials and touches NO real repo.
#
# Prereqs: `nono` on PATH (cargo install nono-cli, needs rustc >= 1.95),
# python3, git, openssl, and root (writes /etc/hosts + system CA trust).
#
# Expected result: small + Content-Length pushes LAND; chunked pushes HANG.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
WORK="$(mktemp -d)"; cd "$WORK"
echo "workdir: $WORK"
cleanup() { kill "${NONO_PID:-0}" "${GSRV_PID:-0}" 2>/dev/null; }
trap cleanup EXIT
export GIT_AUTHOR_NAME=spike GIT_AUTHOR_EMAIL=s@l GIT_COMMITTER_NAME=spike GIT_COMMITTER_EMAIL=s@l

# --- EC P-256 CA (nono's `ring` rejects RSA CA keys), one CA for everything ---
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out ca.key 2>/dev/null
openssl req -x509 -new -key ca.key -out ca.crt -subj "/CN=spike-ca" -days 2 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout srv.key -out srv.csr -subj "/CN=gitspike.test" 2>/dev/null
openssl x509 -req -in srv.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out srv.crt -days 2 \
  -extfile <(printf "subjectAltName=DNS:gitspike.test") 2>/dev/null
cat srv.crt srv.key > srv.pem
grep -q gitspike.test /etc/hosts || echo "127.0.0.1 gitspike.test" >> /etc/hosts
cp ca.crt /usr/local/share/ca-certificates/spike-ca.crt && update-ca-certificates >/dev/null 2>&1
cat /etc/ssl/certs/ca-certificates.crt ca.crt > cabundle.pem

# --- upstream bare repo + working repo with small/big branches ---
git init -q --bare upstream.git; git -C upstream.git config http.receivepack true
head -c 20971520 /dev/urandom > big.bin; echo hello > small.txt
git init -q work; ( cd work
  cp ../small.txt .; git add .; git commit -qm base; git branch smallbranch
  git checkout -q -b bigbranch; cp ../big.bin .; git add big.bin; git commit -qm 20MiB )

# --- start local HTTPS git server (the "upstream") ---
python3 "$HERE/git_http_server.py" 8443 "$WORK" srv.pem 2>gitsrv.log & GSRV_PID=$!
sleep 1

# --- start nono proxy with the injecting profile + our CA ---
export SPIKE_TOKEN="$(printf 'x-access-token:SPIKETOKEN123' | base64)"
export SSL_CERT_FILE="$WORK/cabundle.pem"
nono proxy --profile "$HERE/spike-profile.json" --pass spikepass \
  --proxy-ca-cert ca.crt --proxy-ca-key ca.key --port 8900 >nono.log 2>&1 & NONO_PID=$!
sleep 3
kill -0 "$NONO_PID" 2>/dev/null || { echo "nono failed to start:"; tail -5 nono.log; exit 1; }

PX="http://x:spikepass@127.0.0.1:8900"
gp() { git -c http.proxy="$PX" -c http.sslCAInfo="$WORK/ca.crt" "$@"; }
unset HTTPS_PROXY https_proxy ALL_PROXY; export no_proxy='' NO_PROXY=''
run() { local name="$1" tmout="$2"; shift 2
  echo -n "  $name: "
  if timeout "$tmout" "$@" >/tmp/p.out 2>&1; then echo "LANDED"; else echo "HANG/FAIL (exit $?)"; fi
}
cd work
echo "=== push matrix through nono ==="
run "small (Content-Length)"        30 gp push https://gitspike.test:8443/upstream.git smallbranch:refs/heads/s1
run "20MiB chunked"                 40 gp -c http.postBuffer=1024     push https://gitspike.test:8443/upstream.git bigbranch:refs/heads/big_chunked
run "20MiB Content-Length"          40 gp -c http.postBuffer=52428800 push https://gitspike.test:8443/upstream.git bigbranch:refs/heads/big_cl
echo "=== landed refs on upstream ==="
gp ls-remote https://gitspike.test:8443/upstream.git 2>/dev/null | grep -E 'refs/heads' || true
echo "=== injected auth seen at upstream (proves injection) ==="
grep -Eo "receive-pack auth='[^']*'" "$WORK/gitsrv.log" | tail -3
echo "done. logs: $WORK/{nono.log,gitsrv.log}"
