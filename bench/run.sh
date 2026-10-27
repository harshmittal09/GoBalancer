#!/usr/bin/env bash
# Runs the GoBalancer benchmark suite against the code in this repository.
#
#   bench/run.sh            run every section
#   bench/run.sh sessions   run one section: ring sessions persistent hold bulk failover soak
#
# Output is printed and appended to bench/results/raw-<date>-<host>.txt.
# Needs Go and bash. Works on Linux and macOS.

set -o pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/bench/.bin"
OUT="$ROOT/bench/results/raw-$(date +%Y-%m-%d)-$(hostname -s).txt"
LB=127.0.0.1:9099
DIRECT=127.0.0.1:9001
HOLD_N="${HOLD_N:-3000}"

BPIDS=()
LBPID=""

log() { echo "$*" | tee -a "$OUT"; }
run() { "$@" 2>&1 | tee -a "$OUT"; }

build() {
	mkdir -p "$BIN" "$ROOT/bench/results"
	(cd "$ROOT" &&
		go build -o "$BIN/gobalancer" . &&
		go build -o "$BIN/backend" ./bench/backend &&
		go build -o "$BIN/loadgen" ./bench/loadgen) || {
		echo "build failed" >&2
		exit 1
	}
}

start_backend() { "$BIN/backend" -addr "127.0.0.1:$1" >/dev/null 2>&1 & }

start_backends() {
	BPIDS=()
	for port in 9001 9002 9003; do
		start_backend $port
		BPIDS+=($!)
	done
	sleep 0.5
}

start_lb() {
	"$BIN/gobalancer" >"$BIN/gobalancer.log" 2>&1 &
	LBPID=$!
	sleep 1
}

stop_all() {
	if [ -n "$LBPID" ]; then
		kill "$LBPID" 2>/dev/null
		sleep 0.3
		kill -9 "$LBPID" 2>/dev/null
	fi
	for pid in "${BPIDS[@]}"; do kill "$pid" 2>/dev/null; done
	wait 2>/dev/null
	LBPID=""
	BPIDS=()
	sleep 0.5
}
trap stop_all EXIT

rss() { ps -o rss= -p "$1" | awk '{printf "%.1f MiB", $1/1024}'; }

fds() {
	if [ -d "/proc/$1/fd" ]; then ls "/proc/$1/fd" | wc -l | tr -d ' '; else lsof -p "$1" 2>/dev/null | wc -l | tr -d ' '; fi
}

header() {
	[ -f "$OUT" ] && return
	{
		echo "GoBalancer benchmark run"
		echo "date:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "commit:  $(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
		echo "os:      $(uname -sr)"
		echo "cpus:    $(getconf _NPROCESSORS_ONLN)"
		if [ -r /proc/cpuinfo ]; then
			echo "cpu:     $(awk -F': ' '/model name/{print $2; exit}' /proc/cpuinfo)"
		else
			echo "cpu:     $(sysctl -n machdep.cpu.brand_string 2>/dev/null)"
		fi
		echo "go:      $(go version)"
		echo "files:   ulimit -n $(ulimit -n)"
		echo
	} | tee -a "$OUT"
}

section_ring() {
	log "== ring: correctness and distribution"
	(cd "$ROOT" && run go test -v -run Test ./ring)
	log "== ring: benchmarks"
	(cd "$ROOT" && run go test -run '^$' -bench . -benchmem ./ring)
}

section_sessions() {
	log "== sessions: new connection per request"
	for spec in "1 1000" "10 5000" "100 18000"; do
		set -- $spec
		start_backends
		log "-- direct to backend, concurrency $1"
		run "$BIN/loadgen" -mode sessions -c "$1" -n "$2" -addr "$DIRECT"
		start_lb
		log "-- through GoBalancer, concurrency $1"
		run "$BIN/loadgen" -mode sessions -c "$1" -n "$2" -addr "$LB"
		stop_all
	done
}

section_persistent() {
	log "== persistent: 100 connections x 1000 requests"
	start_backends
	log "-- direct to backend"
	run "$BIN/loadgen" -mode persistent -c 100 -m 1000 -addr "$DIRECT"
	start_lb
	log "-- through GoBalancer"
	run "$BIN/loadgen" -mode persistent -c 100 -m 1000 -addr "$LB"
	stop_all
}

section_hold() {
	log "== hold: $HOLD_N idle connections through GoBalancer"
	start_backends
	start_lb
	log "balancer before: rss $(rss "$LBPID"), open files $(fds "$LBPID")"
	ready="$BIN/hold.ready"
	rm -f "$ready"
	"$BIN/loadgen" -mode hold -n "$HOLD_N" -d 15s -ready "$ready" >"$BIN/hold.out" 2>&1 &
	gen=$!
	for _ in $(seq 240); do
		[ -f "$ready" ] && break
		sleep 0.5
	done
	sleep 2
	log "balancer while holding: rss $(rss "$LBPID"), open files $(fds "$LBPID")"
	wait "$gen"
	tee -a "$OUT" <"$BIN/hold.out"
	stop_all
}

section_bulk() {
	log "== bulk: 2 GiB downloads"
	start_backends
	log "-- direct to backend"
	run "$BIN/loadgen" -mode bulk -p 1 -bytes 2147483648 -addr "$DIRECT"
	run "$BIN/loadgen" -mode bulk -p 8 -bytes 268435456 -addr "$DIRECT"
	start_lb
	log "-- through GoBalancer"
	run "$BIN/loadgen" -mode bulk -p 1 -bytes 2147483648 -addr "$LB"
	run "$BIN/loadgen" -mode bulk -p 8 -bytes 268435456 -addr "$LB"
	stop_all
}

section_failover() {
	log "== failover: 127.0.0.1:9002 killed at ~4s, restarted at ~10s"
	start_backends
	start_lb
	"$BIN/loadgen" -mode failover -c 20 -d 16s >"$BIN/failover.out" 2>&1 &
	gen=$!
	sleep 4
	kill "${BPIDS[1]}"
	sleep 6
	start_backend 9002
	BPIDS[1]=$!
	wait "$gen"
	tee -a "$OUT" <"$BIN/failover.out"
	stop_all
}

section_soak() {
	log "== soak: 100,000 sessions, concurrency 200"
	start_backends
	start_lb
	run "$BIN/loadgen" -mode sessions -c 200 -n 100000 -addr "$LB"
	log "balancer after soak: rss $(rss "$LBPID")"
	stop_all
}

ulimit -n 20000 2>/dev/null || ulimit -n "$(ulimit -Hn)" 2>/dev/null
build
header

sections="${1:-all}"
[ "$sections" = all ] && sections="ring sessions persistent hold bulk failover soak"
for s in $sections; do
	case "$s" in
	ring | sessions | persistent | hold | bulk | failover | soak) "section_$s" ;;
	*)
		echo "unknown section: $s" >&2
		exit 2
		;;
	esac
	log ""
done
