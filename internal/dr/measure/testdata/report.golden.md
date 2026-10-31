# DR drill report: shop

- Report: `drill-20260101T120000Z-0a1b2c`
- Context: `kind-k8scli-lab`
- Actor: everton
- Mode: apps, databases
- Started: 2026-01-01T11:59:55Z
- Finished: 2026-01-01T12:06:50Z
- Disaster declared (T0): 2026-01-01T12:00:00Z
- Outcome: **recovered**
- Verdict against the targets: **PASS**
- Runs: down `run-down`, up `run-up`

## Result at a glance

| Objective | Measured | Target | Verdict | Kind |
|-----------|----------|--------|---------|------|
| RTO (all applications) | 6m40s | 10m00s | pass | measured |
| RPO (worst figure) | 3h00m00s | 4h00m00s | pass | estimated |

RTO is measured from T0 until every application passes its health check. An *estimated* RPO is the age
of the newest backup at T0: nothing was restored to prove it. A *measured* RPO comes from the heartbeat.

## Timeline

| Stage | At (UTC) | Since T0 |
|-------|----------|----------|
| Disaster declared (T0) | 12:00:00 | 0s |
| Recovery started | 12:01:00 | 1m00s |
| Data layer restored (machines, nodes, volumes available) | 12:02:00 | 2m00s |
| Databases up | 12:03:00 | 3m00s |
| Applications started | 12:05:50 | 5m50s |
| Every application healthy | 12:06:40 | 6m40s |

## Applications

| Application | Tier | Health | RTO | RTO target | RPO | RPO target | Verdict |
|-------------|------|--------|-----|------------|-----|------------|---------|
| shop/api | 1 | pods-ready | 4m00s | 5m00s | 3h00m00s (estimated) | 4h00m00s | pass |
| shop/web | 2 | pods-ready | 6m40s | 10m00s | 3h00m00s (estimated) | 4h00m00s | pass |

## Databases

| Database | Kind | RTO | RTO target | RPO | RPO target | Verdict |
|----------|------|-----|------------|-----|------------|---------|
| shop/db | cnpg | 3m00s | 5m00s | 1h30m00s (estimated) | 4h00m00s | pass |

## RPO figures

| Subject | Namespace | Data-loss window | Kind | Source | Note |
|---------|-----------|------------------|------|--------|------|
| api | shop | 3h00m00s | estimated | backup:shop | newest completed backup finished at 2026-01-01T09:00:00Z; no restore was tested |
| api | shop | 20m00s | measured | heartbeat:data | includes clock skew |
| db | shop | 1h30m00s | estimated | cnpg-backup | - |
| web | shop | 3h00m00s | estimated | backup:shop | - |

## State after the drill

The replica counts and readiness after the drill match the baseline taken before it.

| Application | Baseline | After |
|-------------|----------|-------|
| shop/api | api 2/2 | api 2/2 |
| shop/web | web 3/3 | web 3/3 |

## Notices

- no backup was found for namespace billing

