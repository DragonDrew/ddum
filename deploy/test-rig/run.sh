#!/usr/bin/env bash
# End-to-end rig. Needs Docker with the Compose
# plugin on a linux/amd64 host; run from the repository root: make e2e.
#
# It starts a local registry, a sample stack, and ddum (bot in
# DISCORD_MODE=fake plus an agent), pushes new images, approves the update
# through the fake Discord gateway, and asserts that every container runs
# the exact approved digest:
#   web  latest          digest update (same tag)
#   lit  1.0.0 -> 1.1.0  version update through a literal image:
#   env  1.0.0 -> 1.1.0  version update through an env-file variable
#
# KEEP=1 leaves everything running for inspection.
set -euo pipefail

REPO=$(pwd)
RIG=${RIG_DIR:-/tmp/ddum-rig}
REG=127.0.0.1:5055
REGNAME=localhost:5055
KEY=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
export RIG_KEY=$KEY RIG_DIR=$RIG RIG_BOT_URL=http://127.0.0.1:18080

log() { printf '\n== %s\n' "$*"; }
fail() { printf '\nE2E FAILED: %s\n' "$*" >&2; exit 1; }

cleanup() {
	if [ "${KEEP:-0}" = 1 ]; then
		log "KEEP=1: leaving the rig running in $RIG"
		return
	fi
	log "cleaning up"
	# By project name, so this works even when $RIG is already gone.
	docker compose -p ddum-rig down -v --remove-orphans >/dev/null 2>&1 || true
	for p in rigapp grpa grpb manualapp shellenv outside; do docker compose -p "$p" down --remove-orphans >/dev/null 2>&1 || true; done
	docker rm -f ddum-rig-lookalike >/dev/null 2>&1 || true
	docker rm -f ddum-rig-registry >/dev/null 2>&1 || true
	# Images the rig built, by ID, so the rollback pins its agent made of
	# them (ddum-rollback/...) go too. The host's own images are untouched.
	docker image ls -q --filter label=ddum.rig=1 | sort -u | xargs -r docker rmi -f >/dev/null 2>&1 || true
	for ref in ddum-bot:rig ddum-agent:rig; do
		id=$(docker image inspect -f '{{.Id}}' "$ref" 2>/dev/null) && docker rmi -f "$id" >/dev/null 2>&1 || true
	done
	rm -rf "$RIG"
}
on_exit() {
	status=$?
	if [ $status -ne 0 ] && [ -f "$RIG/stacks/ddum/compose.yaml" ]; then
		log "logs"
		docker compose -f "$RIG/stacks/ddum/compose.yaml" logs --no-color --tail 80 || true
	fi
	cleanup
	exit $status
}

[ -f go.mod ] && [ -d deploy/test-rig ] || fail "run from the repository root"
cleanup
trap on_exit EXIT
mkdir -p "$RIG/stacks/rigapp" "$RIG/images" "$RIG/bin"

log "building ddum images"
make docker VERSION=rig TAGS=rig >/dev/null

log "building the driver"
docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false \
	-v "$REPO:/src:ro" -v "$RIG/bin:/out" -w /src golang:1.27-alpine3.24 \
	go build -tags rig -o /out/driver ./deploy/test-rig/driver
DRIVER=$RIG/bin/driver

log "starting the registry"
docker run -d --name ddum-rig-registry -p "$REG:5000" registry:2 >/dev/null

# push NAME TAG VERSION: build a tiny image whose content depends on VERSION.
push() {
	local ref="$REGNAME/rig/$1:$2"
	cat >"$RIG/images/Dockerfile" <<'DOCKERFILE'
FROM busybox:1.37
LABEL ddum.rig=1
ARG V
RUN echo "$V" > /version
CMD ["sh", "-c", "trap 'exit 0' TERM; while :; do sleep 1; done"]
DOCKERFILE
	docker build -q --build-arg "V=$1-$2-$3" -t "$ref" "$RIG/images" >/dev/null
	docker push -q "$ref" >/dev/null
	docker image inspect --format '{{index .RepoDigests 0}}' "$ref" | sed 's/.*@//'
}

log "pushing v1 images"
WEB1=$(push web latest v1)
LIT1=$(push lit 1.0.0 v1)
ENV1=$(push env 1.0.0 v1)
GA1=$(push ga 1.0.0 v1)
GB1=$(push gb 1.0.0 v1)

log "starting the sample stack"
cat >"$RIG/stacks/rigapp/compose.yaml" <<EOF
name: rigapp
services:
  web:
    image: $REGNAME/rig/web:latest
  lit:
    image: $REGNAME/rig/lit:1.0.0   # literal tag
  env:
    image: $REGNAME/rig/env:\${ENV_TAG}
EOF
printf '# tag of the env service\nENV_TAG=1.0.0\n' >"$RIG/stacks/rigapp/.env"
docker compose -f "$RIG/stacks/rigapp/compose.yaml" up -d --quiet-pull >/dev/null
# Two stacks coupled by the ddum.group label.
for s in a b; do
	mkdir -p "$RIG/stacks/grp$s"
	printf "name: grp%s
services:
  svc%s:
    image: %s/rig/g%s:1.0.0
    labels:
      ddum.group: rigpair
" "$s" "$s" "$REGNAME" "$s" >"$RIG/stacks/grp$s/compose.yaml"
	docker compose -f "$RIG/stacks/grp$s/compose.yaml" up -d --quiet-pull >/dev/null
done

log "starting ddum (bot in DISCORD_MODE=fake, agent)"
mkdir -p "$RIG/stacks/ddum"
# The bot runs from the registry and lives under COMPOSE_ALLOWED_ROOTS, so it
# can update itself.
docker tag ddum-bot:rig "$REGNAME/rig/ddum-bot:1.0.0"
docker push -q "$REGNAME/rig/ddum-bot:1.0.0" >/dev/null
DDUM="$RIG/stacks/ddum/compose.yaml"
cat >"$DDUM" <<EOF
name: ddum-rig
services:
  bot:
    image: $REGNAME/rig/ddum-bot:1.0.0
    labels:
      ddum.self: "bot"
    network_mode: host
    environment:
      DISCORD_MODE: fake
      DISCORD_TOKEN: fake
      DISCORD_APPLICATION_ID: "100000000000000000"
      DISCORD_GUILD_ID: "100000000000000001"
      NOTIFICATION_CHANNEL_ID: "100000000000000003"
      AUTHORIZED_DISCORD_IDS: "100000000000000002"
      AGENT_SHARED_KEY: "$KEY"
      LISTEN_ADDRESS: 127.0.0.1
      LISTEN_PORT: "18080"
      LOG_LEVEL: debug
    volumes:
      - bot-data:/data
  agent:
    image: ddum-agent:rig
    network_mode: host
    depends_on:
      bot:
        condition: service_healthy
    labels:
      ddum.self: "agent"
    environment:
      BOT_URL: http://127.0.0.1:18080
      AGENT_SHARED_KEY: "$KEY"
      AGENT_ID: "0195f2c4-0000-7000-8000-0000000e2e01"
      HOST_DISPLAY_NAME: rig
      POLL_INTERVAL_SECONDS: "3"
      ENABLED_PROVIDERS: compose,docker
      COMPOSE_ALLOWED_ROOTS: $RIG/stacks
      LOG_LEVEL: debug
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - agent-data:/data
      - $RIG/stacks:$RIG/stacks
volumes:
  bot-data:
  agent-data:
EOF
docker compose -f "$DDUM" up -d --wait >/dev/null

log "approving the host"
"$DRIVER" approve-host
"$DRIVER" cmd policy set scope=global verify_stable_seconds=2 >/dev/null
sleep 5 # one more inventory after approval

log "pushing v2 images"
WEB=$(push web latest v2)
LIT=$(push lit 1.1.0 v2)
ENV=$(push env 1.1.0 v2)
echo "web latest -> $WEB"
echo "lit 1.1.0  -> $LIT"
echo "env 1.1.0  -> $ENV"

log "detecting and approving through the fake Discord gateway"
"$DRIVER" deploy rigapp

log "checking the running digests"
check() { # SERVICE DIGEST
	local id img
	id=$(docker compose -f "$RIG/stacks/rigapp/compose.yaml" ps -q "$1")
	img=$(docker inspect --format '{{.Image}}' "$id")
	docker image inspect --format '{{json .RepoDigests}}' "$img" | grep -q "$2" ||
		fail "$1 does not run $2 ($(docker image inspect --format '{{json .RepoDigests}}' "$img"))"
	echo "ok: $1 runs $2"
}
check web "$WEB"
check lit "$LIT"
check env "$ENV"
grep -q "rig/lit:1.1.0   # literal tag" "$RIG/stacks/rigapp/compose.yaml" || fail "compose.yaml was not edited in place"
grep -q "^ENV_TAG=1.1.0$" "$RIG/stacks/rigapp/.env" || fail ".env was not edited"
grep -q "^# tag of the env service$" "$RIG/stacks/rigapp/.env" || fail ".env comment was lost"
echo "ok: compose.yaml and .env edited in place"

log "rollback after the old tags are deleted and dangling images pruned"
docker rmi "$REGNAME/rig/lit:1.0.0" "$REGNAME/rig/env:1.0.0" >/dev/null
docker image prune -f >/dev/null # dangling images only; the pins keep the old ones
"$DRIVER" rollback rigapp
check web "$WEB1"
check lit "$LIT1"
check env "$ENV1"
grep -q "rig/lit:1.0.0   # literal tag" "$RIG/stacks/rigapp/compose.yaml" || fail "compose.yaml was not restored"
grep -q "^ENV_TAG=1.0.0$" "$RIG/stacks/rigapp/.env" || fail ".env was not restored"
echo "ok: previous digests and file contents restored"

log "two stacks in one group deploy and roll back as one scope"
GA2=$(push ga 1.1.0 v2)
GB2=$(push gb 1.1.0 v2)
"$DRIVER" deploy "group rigpair"
gcheck() { # STACK SERVICE DIGEST
	local id img
	id=$(docker compose -f "$RIG/stacks/$1/compose.yaml" ps -q "$2")
	img=$(docker inspect --format '{{.Image}}' "$id")
	docker image inspect --format '{{json .RepoDigests}}' "$img" | grep -q "$3" || fail "$2 does not run $3"
	echo "ok: $2 runs $3"
}
gcheck grpa svca "$GA2"
gcheck grpb svcb "$GB2"
"$DRIVER" rollback "group rigpair"
gcheck grpa svca "$GA1"
gcheck grpb svcb "$GB1"
grep -q "rig/ga:1.0.0" "$RIG/stacks/grpa/compose.yaml" && grep -q "rig/gb:1.0.0" "$RIG/stacks/grpb/compose.yaml" ||
	fail "the group rollback did not restore both compose files"
echo "ok: one operation deployed both stacks and one rollback restored both"

log "Scheduled deploys: the group's updates wait for their time, then deploy"
"$DRIVER" schedule "group rigpair"
gcheck grpa svca "$GA2"
gcheck grpb svcb "$GB2"
echo "ok: the scheduled deploy waited, then deployed both stacks"

log "A manual update past a pending one clears it, and its old button deploys nothing"
MAN1=$(push man 1.0.0 v1)
mkdir -p "$RIG/stacks/manualapp"
MANUAL="$RIG/stacks/manualapp/compose.yaml"
printf 'name: manualapp\nservices:\n  svc:\n    image: %s/rig/man:1.0.0\n' "$REGNAME" >"$MANUAL"
docker compose -f "$MANUAL" up -d --quiet-pull >/dev/null
sleep 8 # the agent reports the new stack
push man 1.1.0 v2 >/dev/null
read -r CARD APPROVE < <("$DRIVER" offer manualapp)
MAN3=$(push man 1.2.0 v3)
sed -i 's/man:1.0.0/man:1.2.0/' "$MANUAL"
docker compose -f "$MANUAL" up -d --quiet-pull >/dev/null
"$DRIVER" stale manualapp "$CARD" "$APPROVE"
sleep 8 # time for a wrong deploy to start
gcheck manualapp svc "$MAN3"
grep -q "rig/man:1.2.0" "$MANUAL" || fail "the manual edit was overwritten"
echo "ok: 1.2.0 set by hand stayed, and the 1.1.0 offer went away by itself"

log "Files ddum edited still belong to their owner"
for f in "$RIG/stacks/rigapp/compose.yaml" "$RIG/stacks/rigapp/.env" "$RIG/stacks/grpa/compose.yaml"; do
	[ "$(stat -c %u "$f")" = "$(id -u)" ] || fail "$f now belongs to uid $(stat -c %u "$f")"
done
echo "ok: edited and restored files kept their owner"

log "A deploy that would change a stack beyond its image is refused"
push shell 1.0.0 v1 >/dev/null
mkdir -p "$RIG/stacks/shellenv"
SHELLENV="$RIG/stacks/shellenv/compose.yaml"
cat >"$SHELLENV" <<EOF
name: shellenv
services:
  svc:
    image: $REGNAME/rig/shell:1.0.0
    volumes:
      - \${RIG_DATA_ROOT}/data:/data
    environment:
      FROM_SHELL: \${RIG_FROM_SHELL:-unset}
      AGENT_KEY: \${AGENT_SHARED_KEY:-not-available}
EOF
mkdir -p "$RIG/data/data" # or Docker makes it, as root, and cleanup cannot remove it
RIG_DATA_ROOT="$RIG/data" RIG_FROM_SHELL=hello docker compose -f "$SHELLENV" up -d --quiet-pull >/dev/null
BEFORE=$(cat "$SHELLENV")
sleep 8 # the agent reports the new stack
push shell 1.1.0 v2 >/dev/null
"$DRIVER" deploy-refused shellenv "Nothing was changed"
[ "$(cat "$SHELLENV")" = "$BEFORE" ] || fail "the refused deploy edited the file"
docker inspect shellenv-svc-1 --format '{{.Config.Image}}' | grep -q "shell:1.0.0" || fail "the refused deploy changed the container"
echo "ok: refused, with the file and the container untouched"

log "STACK_ENV_ on the agent gives a stack what it needs, and the agent's own variables stay out"
AGENT_FILE=$DDUM
sed -i "s|      LOG_LEVEL: debug\$|      LOG_LEVEL: debug\n      STACK_ENV_RIG_DATA_ROOT: $RIG/data\n      STACK_ENV_RIG_FROM_SHELL: hello|" "$AGENT_FILE"
docker compose -f "$DDUM" up -d --wait agent >/dev/null
sleep 8
"$DRIVER" deploy shellenv >/dev/null
id=$(docker compose -f "$SHELLENV" ps -q svc)
docker inspect "$id" --format '{{range .Mounts}}{{.Source}}{{end}}' | grep -q "^$RIG/data/data$" || fail "the bind mount moved"
docker inspect "$id" --format '{{json .Config.Env}}' | grep -q 'FROM_SHELL=hello' || fail "the variable was lost"
docker inspect "$id" --format '{{json .Config.Env}}' | grep -q 'AGENT_KEY=not-available' || fail "the stack could read the shared key"
docker inspect "$id" --format '{{.Config.Image}}' | grep -q "shell:1.1.0" || fail "the deploy did not happen"
echo "ok: deployed with the same mount and variable, and no shared key"

log "A rollback keeps what was edited after the deploy"
ENVFILE="$RIG/stacks/rigapp/.env"
LIT3=$(push lit 1.2.0 v3)
"$DRIVER" deploy rigapp >/dev/null
gcheck rigapp lit "$LIT3"
printf 'API_KEY=rotated-after-the-deploy\n' >>"$ENVFILE"
echo '# added after the deploy' >>"$RIG/stacks/rigapp/compose.yaml"
"$DRIVER" rollback rigapp >/dev/null
grep -q '^API_KEY=rotated-after-the-deploy$' "$ENVFILE" || fail "the rollback lost a later .env edit"
grep -q '^# added after the deploy$' "$RIG/stacks/rigapp/compose.yaml" || fail "the rollback lost a later compose edit"
grep -q "rig/lit:1.1.0\|rig/lit:1.0.0" "$RIG/stacks/rigapp/compose.yaml" || fail "the rollback did not put the tag back"
if docker compose -f "$DDUM" exec -T agent sh -c 'find /data/snapshots -name "*.env" | grep .' >/dev/null 2>&1; then
	fail "a snapshot keeps a copy of an env file"
fi
echo "ok: later edits stayed, and snapshots hold no copies of files"

log "A stack Compose cannot load from the agent is refused before anything changes (#6)"
OUTSIDE_ENV=$(mktemp)
printf 'X=1\n' >"$OUTSIDE_ENV"
push out 1.0.0 v1 >/dev/null
mkdir -p "$RIG/stacks/outside"
printf 'name: outside\nservices:\n  svc:\n    image: %s/rig/out:1.0.0\n    env_file: %s\n' "$REGNAME" "$OUTSIDE_ENV" >"$RIG/stacks/outside/compose.yaml"
docker compose -f "$RIG/stacks/outside/compose.yaml" up -d --quiet-pull >/dev/null
sleep 8
push out 1.1.0 v2 >/dev/null
"$DRIVER" deploy-refused outside "cannot load this stack"
grep -q "rig/out:1.0.0" "$RIG/stacks/outside/compose.yaml" || fail "the refused deploy edited the file"
rm -f "$OUTSIDE_ENV"
echo "ok: refused, and the reason is on the card"

log "An image's own labels cannot take over a stack"
printf 'FROM busybox:1.37\nLABEL ddum.rig=1 com.docker.compose.project=manualapp com.docker.compose.service=svc com.docker.compose.container-number=2\nCMD ["sleep","600"]\n' >"$RIG/images/Dockerfile"
docker build -q -t ddum-rig-lookalike:1 "$RIG/images" >/dev/null
docker run -d --name ddum-rig-lookalike ddum-rig-lookalike:1 >/dev/null
sleep 8
"$DRIVER" cmd stack show stack=manualapp | grep -q "Deployment: supported" || fail "a look-alike container took the stack over"
docker rm -f ddum-rig-lookalike >/dev/null
echo "ok: the stack is unaffected"

log "Per-agent keys: the bot and agent pinned each other's keys"
docker compose -f "$DDUM" exec -T agent sh -c 'test -s /data/agent_key && test -s /data/bot_key && [ "$(stat -c %a /data/agent_key)" = 600 ]' || fail "keys are missing or readable"
"$DRIVER" cmd host list | grep -q rig || fail "no host"
BOTPORT=127.0.0.1:18080
code=$(curl -s -o /tmp/ddum-rig-unsigned -w '%{http_code}' -D - -X POST "http://$BOTPORT/v1/agent/poll" -H 'X-DDUM-Signature: deadbeef' -H 'X-DDUM-Agent-ID: x' -H 'X-DDUM-Timestamp: 1' -H 'X-DDUM-Protocol: 2' -d '{}' | grep -ci 'x-ddum-bot-signature' || true)
[ "$code" = 0 ] || fail "the bot signed its refusal of an unsigned request"
rm -f /tmp/ddum-rig-unsigned
echo "ok: keys pinned and private, and an unsigned request gets no signature"

# bot_image TAG HEALTHCHECK...: a bot image with a different digest.
bot_image() {
	local tag=$1
	shift
	printf 'FROM %s/rig/ddum-bot:1.0.0\nLABEL ddum.rig=1 rig.version=%s\n%s\n' "$REGNAME" "$tag" "$*" >"$RIG/images/Dockerfile"
	docker build -q -t "$REGNAME/rig/ddum-bot:$tag" "$RIG/images" >/dev/null
	docker push -q "$REGNAME/rig/ddum-bot:$tag" >/dev/null
	docker image inspect --format '{{index .RepoDigests 0}}' "$REGNAME/rig/ddum-bot:$tag" | sed 's/.*@//'
}
bot_runs() { # DIGEST
	local id img
	id=$(docker compose -f "$DDUM" ps -q bot)
	img=$(docker inspect --format '{{.Image}}' "$id")
	docker image inspect --format '{{json .RepoDigests}}' "$img" | grep -q "$1" || fail "the bot does not run $1"
}

log "the bot updates itself and posts the result after it restarts"
BOT2=$(bot_image 1.1.0 "")
"$DRIVER" selfupdate ddum succeeded
bot_runs "$BOT2"
grep -q "rig/ddum-bot:1.1.0" "$DDUM" || fail "the bot's compose file was not edited"
echo "ok: the bot runs $BOT2"

log "a bot update that fails verification is rolled back by the agent"
bot_image 1.2.0 'HEALTHCHECK --interval=2s --timeout=2s --start-period=0s --retries=1 CMD ["/usr/local/bin/ddum-bot", "no-such-command"]' >/dev/null
"$DRIVER" selfupdate ddum rolledback
bot_runs "$BOT2"
grep -q "rig/ddum-bot:1.1.0" "$DDUM" || fail "the bot's compose file was not restored"
echo "ok: the agent restored the bot to $BOT2 on its own"

log "E2E PASSED"
