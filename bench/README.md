# Benchmarks

Tools for measuring GoBalancer and the results of each run.

| Path | What it is |
|---|---|
| `run.sh` | Builds everything, starts three backends and the balancer, runs the suite |
| `loadgen/` | Load generator. Checks every reply against its request |
| `backend/` | Small TCP server used as a backend. Replies with its own address, or streams bytes for throughput tests |
| `results/` | One summary per run, plus the raw output it was written from |
| `../ring/ring_test.go` | Ring correctness tests and `go test -bench` benchmarks |

## Running

From the repository root:

```bash
bench/run.sh                 # everything, takes about 4 minutes
bench/run.sh sessions        # one section
bench/run.sh "ring bulk"     # several sections
```

Sections: `ring`, `sessions`, `persistent`, `hold`, `bulk`, `failover`, `soak`.

The script uses ports 9099 (balancer) and 9001 to 9003 (backends), so stop anything else using them first. Raw output is appended to `results/raw-<date>-<host>.txt`, with the commit, OS, CPU and Go version at the top.

The `hold` section opens 3,000 connections by default (`HOLD_N=1000 bench/run.sh hold` to change it). Each connection costs the balancer 6 file descriptors on Linux and 2 on macOS. The script tries to raise `ulimit -n` to 20,000; if that fails, lower `HOLD_N`.

## Writing up a run

Copy the numbers you care about from the raw file into a dated summary such as `results/2026-10-27-sandbox.md`, and note the machine. Any number quoted outside the repo (README, design doc, anywhere else) should point to one of these files.
