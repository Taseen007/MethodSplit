# MethodSplit — Step-by-Step Build Plan

**How to use this plan**
- Do the phases **in order**. Each phase ends with **one commit + push**. Stop after each phase, check that it works, then move on.
- Each phase has a **Concept** (what to understand first), **Tasks**, and **Done when** (tests that must pass before committing).
- ⚠️ = critical. Don't skip it.
- Rough time: **4–6 weeks part-time.** A 1–2 week estimate is too optimistic for this full scope.

---

## Folder structure (target)

```
methodsplit/
├── cmd/
│   ├── gateway/main.go          # the gateway binary
│   ├── payments-api/main.go     # mock payment backend (MODE=read|write)
│   ├── msclient/main.go         # CLI that signs requests (HMAC)
│   └── simulator/main.go        # traffic generator for ML data (Phase 11)
├── internal/
│   ├── config/                  # load + validate gateway.yaml / merchants.yaml
│   ├── gateway/                 # route table, pools, round-robin, health checks, proxy
│   ├── middleware/              # requestid, bodylimit, auth, ratelimit, idempotency, cache, ryw, mlguard
│   ├── ml/                      # features (Redis windows) + iforest scorer
│   ├── payments/                # backend handlers + Postgres store
│   ├── redisx/                  # Redis client + Lua scripts
│   └── metrics/                 # Prometheus metrics
├── configs/
│   ├── gateway.yaml
│   ├── gateway.loadtest.yaml
│   └── merchants.yaml           # demo keys only
├── migrations/001_init.sql
├── deploy/
│   ├── docker-compose.yml
│   ├── postgres/primary/        # init script, pg_hba additions
│   ├── postgres/replica/        # entrypoint with pg_basebackup
│   ├── prometheus/prometheus.yml
│   └── grafana/                 # provisioning + dashboard JSON
├── ml/                          # Python: train.py, evaluate.py, export.py, requirements.txt, testdata/
├── models/                      # exported iforest-vN.json
├── loadtest/                    # k6 scripts (E1, E2, E5)
├── tests/integration/           # E3, E4 correctness tests (Go)
├── docs/ARCHITECTURE.md, BUILD_PLAN.md, RESULTS.md
├── .github/workflows/ci.yml
├── .devcontainer/devcontainer.json
├── Makefile
├── .env.example
├── go.mod
├── LICENSE (MIT)
└── README.md
```

**Go libraries (all free and open source):** standard library (`net/http`, `net/http/httputil`, `log/slog`, `crypto/hmac`), `github.com/redis/go-redis/v9`, `github.com/jackc/pgx/v5`, `github.com/prometheus/client_golang`, `gopkg.in/yaml.v3`, `github.com/google/uuid`.
**Python:** `scikit-learn`, `numpy`, `pandas`.

---

## Phase 0 — Project skeleton and CI

**Concept:** Go modules, Makefiles, what CI is.

Tasks
- [x] `go mod init github.com/<username>/methodsplit` (Go 1.25+; `net/http` method-aware routing needs 1.22+, and pgx/v5 needs 1.25+)
- [x] Create the folder structure above (empty `main.go` files that just start)
- [x] `Makefile` targets: `up`, `down`, `logs`, `test`, `itest`, `lint`, `loadtest`, `seed`
- [x] `.gitignore` (include `.env`, `logs/`, `*.jsonl`), `.env.example`, MIT `LICENSE`
- [x] `.github/workflows/ci.yml`: `go vet ./...`, `go test ./...`
- [x] Copy `README.md`, `docs/ARCHITECTURE.md`, and this file into the repo

**Done when:** CI is green on GitHub.
**Commit:** `chore: project skeleton, Makefile and CI`

---

## Phase 1 — Payments backend + Postgres primary

**Concept:** database transactions, `SELECT ... FOR UPDATE`, why money is stored as integers.

Tasks
- [x] `migrations/001_init.sql` (tables from ARCHITECTURE §11) plus two demo merchants
- [x] `docker-compose.yml` with `pg-primary` and `payments-write-1`
- [x] Endpoints in `payments-api`:
  - `POST /v1/payments` (create), `POST /v1/payments/{id}/execute`, `POST /v1/payments/{id}/refund`
  - `GET /v1/payments/{id}`, `GET /v1/payments?limit=`, `GET /v1/balance`, `GET /healthz`
- [x] Reads the merchant from `X-Merchant-ID`
- [x] ⚠️ Stores the idempotency record **in the same transaction** as the change (`ON CONFLICT` → return the stored response)
- [x] Status transitions guarded (invalid transition → `409`)
- [x] Unit tests for handlers and the store

**Done when:** create → execute → refund works with curl, and sending the same `Idempotency-Key` twice creates **one** payment.
**Commit:** `feat(payments): payment API with transactional idempotency on Postgres primary`

---

## Phase 2 — Read replica + read mode

**Concept:** streaming replication, replication lag, read-only replicas.

Tasks
- [x] Primary config: `wal_level=replica`, `replicator` role, `pg_hba` replication line
- [x] Replica entrypoint: if the data dir is empty, run `pg_basebackup ... -R -X stream`, then start
- [x] `payments-api` with `MODE=read` connects to the replica DSN and returns `405` for writes
- [x] Read-mode `/healthz` returns `503` if replica lag is over 10s — lag is `now() - pg_last_xact_replay_timestamp()`, but only counted when `pg_last_wal_receive_lsn() != pg_last_wal_replay_lsn()` (real backlog); otherwise 0, so an idle-but-caught-up replica never false-positives as unhealthy
- [x] Add `payments-read-1` and `payments-read-2` to Compose (internal network only)

**Done when:** a row inserted on the primary shows up on the replica, and `SELECT pg_is_in_recovery();` returns `true` on the replica.
**Commit:** `feat(db): streaming replica and read-mode payments instances`

---

## Phase 3 — Gateway core (routing, pools, health)

**Concept:** reverse proxies, `httputil.ReverseProxy`, round-robin, health checks.

Tasks
- [x] Load and validate `configs/gateway.yaml` (fail fast on bad config)
- [x] Middleware: request ID (`X-Request-ID`), panic recovery, JSON access log (`slog`), body limit 1 MB
- [x] Route table matching method + path pattern; `HEAD` behaves like `GET`; `404` vs `405` + `Allow`
- [x] `OPTIONS` answered at the gateway (CORS preflight)
- [x] Pools with round-robin over healthy targets; active health checks (5s interval, 2 fails / 2 successes)
- [x] `strong` reads go to the write pool; read pool fallback to write pool if it's empty
- [x] `split_enabled: false` sends everything to the write pool (for experiment E2)
- [x] ⚠️ Never retry writes; retry a GET once only on connection errors
- [x] Response header `X-MethodSplit-Pool: read|write`
- [x] Standard error JSON: `{"error":{"code":"...","message":"...","request_id":"..."}}`

**Done when:** GETs land on read instances and POSTs on write (check the header). `docker stop payments-read-1` causes no errors, only a short blip.
**Commit:** `feat(gateway): method+path routing, pools, round-robin and health checks`

---

## Phase 4 — HMAC authentication + signing CLI

**Concept:** HMAC, why constant-time comparison matters, replay attacks.

Tasks
- [x] `configs/merchants.yaml` with demo keys and secrets
- [x] Auth middleware per ARCHITECTURE §6 (canonical string, 300s window, `hmac.Equal`)
- [x] ⚠️ Delete the client's `X-Merchant-ID`, then set the gateway's own
- [x] Overwrite `X-Forwarded-For`
- [x] `cmd/msclient`: `msclient -key mk_demo_a -X POST -d '{...}' -idem order-1 /v1/payments`
- [x] Unit tests: valid, bad signature, old timestamp, missing headers, spoofed `X-Merchant-ID`

**Done when:** every test passes and msclient requests succeed through the gateway.
**Commit:** `feat(auth): HMAC request signing and msclient CLI`

---

## Phase 5 — Redis + rate limiting

**Concept:** sliding-window counters, why Lua scripts make the check atomic, fail-open vs fail-closed.

Tasks
- [x] Add Redis to Compose; `internal/redisx` client with short timeouts
- [x] Lua sliding-window script; limits: per-IP pre-auth, per-merchant read, per-merchant write
- [x] `429` + `Retry-After` + `RateLimit-*` headers
- [x] ⚠️ Redis failure policy: reads fail open (plus metric), writes fail closed (`503`)
- [x] `configs/gateway.loadtest.yaml` with a higher per-IP limit
- [x] Integration tests against real Redis (CI service container)

**Done when:** `429` is returned exactly when the limit is crossed, and stopping Redis makes GETs still work and POSTs return `503`.
**Commit:** `feat(ratelimit): Redis sliding-window limits with explicit failure policy`

---

## Phase 6 — Gateway idempotency

**Concept:** race conditions, atomic `SET NX`, TTL locks.

Tasks
- [x] Implement the state machine from ARCHITECTURE §8 (new / processing / completed / mismatch)
- [x] Missing or invalid key on a required route → `400`
- [x] Body hash check → `422` on mismatch; `409` while processing; replay with `Idempotency-Replayed: true`
- [x] 5xx or timeout → delete the record; PROCESSING TTL 30s; COMPLETED TTL 24h
- [x] **Integration test E3:** 100 goroutines send the same POST at once

**Done when:** ⚠️ E3 creates **exactly one** payment row on every run (10 runs).
**Commit:** `feat(idempotency): Redis idempotency layer with conflict and mismatch handling`

---

## Phase 7 — Safe caching + read-your-writes

**Concept:** cache invalidation with version counters, replication lag.

Tasks
- [x] Cache middleware per ARCHITECTURE §9 (merchant in the key, version counter, 200 only, size cap)
- [x] After a successful write: `INCR cachever:{m}` and `SET ryw:{m} 1 EX 5`
- [x] `eventual` reads check `ryw:{m}` → route to the write pool while it's set
- [x] `X-Cache` header
- [x] **Integration test E4:** create → list 1,000 times with RYW on and off; count stale lists
- [x] Test: merchant B never receives merchant A's cached response

**Done when:** stale reads with RYW on = 0, and the cache-isolation test passes.
**Commit:** `feat(cache): merchant-scoped versioned cache and read-your-writes routing`

---

## Phase 8 — Observability

**Concept:** Prometheus metric types (counter, gauge, histogram), p95/p99.

Tasks
- [x] Metrics from ARCHITECTURE §13 at `/metrics`
- [x] Prometheus and Grafana OSS in Compose; provisioned data source + dashboard JSON committed
- [x] Dashboard panels: RPS by class/pool, p95 latency, cache hit ratio, 429s, idempotency results, upstream health

**Done when:** `localhost:3000` shows live panels while traffic runs.
**Commit:** `feat(observability): Prometheus metrics and Grafana dashboard`

---

## Phase 9 — Load tests and results (non-ML)

**Concept:** latency percentiles, why CPU limits make results repeatable.

Tasks
- [x] Add `cpus:` limits to backend containers in Compose
- [x] k6 scripts with HMAC signing (`k6/crypto`): E1 overhead, E2 split on/off, E5 cache on/off
- [x] Create `docs/RESULTS.md`: machine specs, versions, commands, raw output, tables, measured replication lag
- [x] Put the real E1–E5 numbers in the README Results section

**Done when:** every number in the README has a matching row in `RESULTS.md`.
**Commit:** `test(load): k6 experiments E1-E5 and results report`

> ✅ **Checkpoint:** after Phase 9 the project is a complete, strong backend/DevOps project. Everything after this is the ML layer.

---

## Phase 10 — ML feature extraction + feature log

**Concept:** sliding-window features, HyperLogLog (approximate distinct counting).

Tasks
- [x] `internal/ml/features.go`: six 10s buckets, one pipelined read, update after the response (ARCHITECTURE §12.2)
- [x] Count auth failures per claimed `X-Api-Key`
- [x] Write `logs/features.jsonl` (mode `off` still logs)
- [x] Measure the added latency of feature reading

**Done when:** the log file fills with correct-looking feature rows while msclient traffic runs.
**Commit:** `feat(ml): per-key windowed features and feature logging`

---

## Phase 11 — Traffic simulator and dataset

**Concept:** synthetic data and its limits; train/test separation.

Tasks
- [x] `cmd/simulator` with profiles: normal merchants, burst flood, low-and-slow bot, ID enumeration, credential stuffing, retry storm
- [x] Each profile uses its own API keys; write `ml/data/labels.json` (key → profile)
- [x] ⚠️ Separate runs: **train** (seed A, attack intensity set 1) and **test** (seed B, different intensities)
- [x] Save the logs as `ml/data/train.jsonl` and `ml/data/test.jsonl` (or a script that regenerates them if they're too big for Git)

**Done when:** both datasets exist and are labeled.
**Commit:** `feat(sim): traffic simulator and labeled train/test datasets`

---

## Phase 12 — Train, export, Go scorer, parity test

**Concept:** how an Isolation Forest scores points (shorter path = more unusual).

Tasks
- [x] `ml/train.py`: `IsolationForest(n_estimators=100, max_samples=256)` on normal-only training rows
- [x] `ml/evaluate.py`: choose the threshold for FPR ≤ 1% on validation; compute the baseline (static limits) on the same data
- [x] `ml/export.py`: model JSON with ⚠️ the feature-subset mapping and `offset_`; write `ml/testdata/golden.json`
- [x] `internal/ml/iforest.go` scorer (float32 comparisons) + parity test (`1e-6`) + Go benchmark
- [x] Model reload on `SIGHUP`

**Done when:** ⚠️ the parity test passes and the benchmark number is recorded.
**Commit:** `feat(ml): Isolation Forest training, JSON export and pure-Go scorer with parity test`

---

## Phase 13 — Shadow mode → enforce + evaluation

**Concept:** shadow deployment, false positives vs false negatives.

Tasks
- [x] `mlguard` middleware with modes `off | shadow | enforce` (ARCHITECTURE §12.6)
- [x] Run the test traffic in **shadow** first and check the flagged list
- [x] Switch to **enforce** and check that normal merchants aren't hurt
- [x] Fill the E6 table (baseline vs model, per attack type, FPR, p95 added latency) in `RESULTS.md` and the README
- [x] Metrics: `ms_ml_score`, `ms_ml_flagged_total`, `ms_ml_model_info`

**Done when:** E6 is filled with honest numbers, including attack types where the model does **not** win.
**Commit:** `feat(ml): shadow/enforce anomaly guard and evaluation vs static baseline`

---

## Phase 14 — Polish and free demo

Tasks
- [x] `.devcontainer/devcontainer.json` so the project runs in GitHub Codespaces
- [ ] Demo GIF/video (free tools, e.g. OBS Studio), Grafana screenshots — needs a human at a screen recorder; not something that can be produced from inside this environment
- [x] Final README pass: results, limitations, diagrams
- [x] Resume bullets written **only from measured numbers** (drafted from `docs/RESULTS.md`; see chat history)

**Done when:** a stranger can clone the repo, run `make up`, and reproduce E3 by following the README.
**Verified 2026-10-24:** `make up` equivalent (`docker compose up -d --build`) brought up the full stack from a clean state; all 12 integration tests in `tests/integration` passed live, including `TestIdempotency_ConcurrentDuplicateCreate` (E3). `go vet`, `gofmt`, and all unit tests are clean.
**Commit:** `docs: final README, results and Codespaces demo`
