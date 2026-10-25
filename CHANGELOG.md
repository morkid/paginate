# Changelog

## Unreleased (v1.3.0)

Fixes from a third audit round (C-01…C-12, R-01). Minor version bump: generated SQL changes (LIKE escape) and new default rejections. Code fixes have regression tests in `fixes_test.go`; postgres/mysql output is tested at SQL-generation level only.

### Security

- **Breadth caps (C-01/C-04/C-07):** a multi-column filter (`"a,b,c"`) may name at most 8 columns, `IN`/`NOT IN` lists hold at most 1000 values, a filter tree holds at most 256 nodes, `sort` takes at most 8 columns and `fields` at most 128 distinct names (now de-duplicated). This closes a columns × values memory amplification where a ~100 KB request could allocate gigabytes.
- **`Fields()` projection bypass (C-02):** a client `fields` list that matches none of the server `Fields()` entries now falls back to the server list instead of `SELECT *`.
- **Soft-deleted preloads (C-03):** preloads now keep the caller's soft-delete scope; previously the outer `Unscoped()` leaked deleted associations. A caller-side `Unscoped()`, or an `Unscoped()` inside a preload condition func, still includes them.
- **Hidden-column oracle (R-01, partial):** columns whose destination struct field is tagged `json:"-"` can no longer be filtered, sorted or selected (`Config.AllowHiddenColumns` opts out). Columns simply *absent* from the destination struct are still filterable through the base query; use `Fields()` to restrict those.
- **Log forging (C-12i):** logged DB/JSON errors are kept on one line.
- **`ExtraOperators` (C-12g):** entries containing `XOR`, `DIV` or `MOD` are dropped, as `OR`/`AND` already were.

### Fixed

- `total` was under-counted when exactly one column was selected (gorm `COUNT(col)` skips NULLs) (C-05).
- Integer filter values ≥ 1,000,000 were bound as floats, and integers above 2^53 lost precision. Request JSON is now decoded with `json.Number` when the default decoder is in use and a number has 16+ digits (C-06).
- `[...,["OR"],["OR"],...]` reached the DB as invalid SQL; now rejected (C-08).
- `NOT IN []` returned no rows; it now keeps every row. `NOT IN` lists containing `null` are rejected (C-09).
- POST `filters` given as an object/number/boolean were silently ignored; now rejected fail-closed (C-10).
- LIKE operands: `null`/arrays/objects rejected; numbers keep their literal form (`1000000`, not `1e+06`) (C-12a).
- `IS`/`IS NOT` with a non-null value emitted `IS $1` (postgres syntax error); now `IS [NOT] DISTINCT FROM ?` on postgres and `<=>` on mysql; booleans become `IS TRUE`/`IS FALSE` there (C-12b).
- `ILIKE`/`NOT ILIKE` failed on sqlite/mysql; they now run as `LIKE`/`NOT LIKE` there (C-12c).
- The int32 offset/limit clamp now applies only to 32-bit builds; 64-bit deep pages are no longer wrapped onto one window (C-12d).
- Parsing no longer writes `Method = "GET"` into the caller's `*http.Request`; a GET request with a nil `URL` no longer panics (C-12e).
- `go.mod` declared `go 1.27.3`; it is now `go 1.26.0` + `toolchain go1.27.3`, matching the v1.2.0 notes (C-11).

### Changed (behavior notes for upgraders)

1. **LIKE escape character is now `!`** (`ESCAPE '!'`) on every dialect, replacing the backslash. This avoids breakage under postgres `standard_conforming_strings=off` and mysql `NO_BACKSLASH_ESCAPES` (C-12f). Backslashes in search values are now literal.
2. **Cache key format changed for scoped keys** (scope is length-prefixed, C-12h): flush scoped caches at deploy.
3. Requests over the new breadth caps, using hidden columns, or using the shapes above now return `error: true` with a fixed `error_message`.

## Unreleased (v1.2.0-dev)

Security-focused release covering two independent audit rounds. All prior round-1 findings (F-01…F-21) and round-2 findings (N-01…N-11) are fixed with regression tests.

### Security

- **Operator allowlist (breaking):** `filters` operators are now validated against a strict allowlist (`= != <> < > <= >= LIKE NOT LIKE ILIKE NOT ILIKE IN NOT IN BETWEEN IS IS NOT`); logical connectors only accept `AND`/`OR`. Requests with anything else are rejected fail-closed. Extend via `Config.ExtraOperators` (sanitized — letters/digits/space only).
- **Identifier validation (breaking):** filter columns, sort columns and `fields` entries must match `[A-Za-z_][A-Za-z0-9_]*` per dot/comma segment (override with `Config.ColumnValidator`). Closes identifier/ORDER BY injection on all dialects, including the GORM `A-z` backtick class bug.
- **Page-size bounds:** client `size` is clamped to `[1, Config.MaxSize]` (default 100); negative sizes rejected to defaults; `LIMIT` can no longer be disabled via `size=-1`. Server `DefaultSize` is *not* capped.
- **Overflow-safe pagination:** `page*size` overflow saturates to an empty page, then clamps to int32 range so 32-bit builds cannot wrap to page 0.
- **Fail-closed parse errors:** malformed `filters` JSON, malformed `IN`/`BETWEEN` shapes, non-string operators, illegal filter arities and leading/bare connectors are rejected instead of silently dropping predicates (opt-in `Config.StrictFilters` also rejects dropped garbage elements).
- **Shared-config data race fixed:** `Response()` no longer mutates the shared `*Pagination`/`Config` (clean under `-race`).
- **Error disclosure:** database errors surface as generic `"database error"`; details stay in `Page.RawError` (server-side). Validation rejections always populate `error`/`error_message`.
- **Cache security:** cache keys now include an optional `Config.CacheScope` (required for shared/multi-tenant adapters — one-time warning otherwise), use **SHA-256**, carry a `prefix:` delimiter, and error pages are never cached. `ClearCache` clears exact scopes (appends `:`).
- **Request body limit (fail-closed):** POST bodies capped at 1 MB (net/http and fasthttp); nil-body no longer panics. **Oversized or malformed JSON bodies are rejected with a visible error** (`request body too large` / `invalid request body`) — including under `CustomParamEnabled`, where number-valued bodies (e.g. `{"size":10}`) are a schema mismatch and now reject instead of silently falling back to defaults. Empty bodies remain "no parameters". *(fasthttp still buffers the full body at the transport layer — set server `ReadBufferSize`/max-body limits too.)*
- **LIKE wildcards:** `_` is now escaped like `%`/`\`, with an `ESCAPE` clause on every dialect — **unconditionally**, including no-Statement/programmatic paths (zero-alloc when nothing needs escaping).
- **Filter shape hardening:** empty-array members (`[[]]`, nested variants) rejected in every mode; group build emits a single `1 = 0` instead of ever producing `WHERE ( ( )` or connector-less fragments; trailing `null`/dropped garbage rejected under `StrictFilters`; **filter trees capped at 16 nesting levels** (deep payloads used to overflow the DB expression parser).
- **`ExtraOperators` tightening:** entries are upper-cased, restricted to letters/digits/space, and entries containing the logical connectors `OR`/`AND` (e.g. `"OR 1"`) are dropped — extras are comparison operators only.
- **Soft-delete:** behavior verified and locked by tests — default scope hides soft-deleted rows; caller `Unscoped()` still shows them.

### Added

- `Config`: `MaxSize`, `ExtraOperators`, `ColumnValidator`, `CacheScope`, `StrictFilters`
- Typed warm-cache responses (cache hits restore items into the caller's slice type instead of `map[string]interface{}`)
- Benchmarks (`bench_test.go`) and a security regression suite (`fixes_test.go`, 30+ tests)
- CI: `go vet`, `go test -race`, `GOARCH=386` test leg, `govulncheck` gate; `toolchain go1.27.3`

### Changed (behavior notes for upgraders)

1. **Validation rejections are visible:** `page.Error == true` with `error_message` like `"invalid filter parameter"` even when `ErrorEnabled` is `false` (raw DB text remains gated).
2. **Cache key format changed** (`prefix:md5…` → `prefix:sha256…`): old entries will not be read after upgrade — **flush caches at deploy**.
3. Requests that previously "worked" by silently ignoring bad operators/columns/shapes now return empty pages with an error — audit client input if you relied on the lenient behavior.

### Dependencies

- `go 1.16` → `go 1.26.0` (+ `toolchain go1.27.3`)
- `gorm v1.21.3` → `v1.31.2`, `fasthttp v1.22.0` → `v1.76.0` (CVE-2022-21221 fixed), `klauspost/compress v1.11.12` → `v1.20.3` (CVE-2026-63209 fixed), `strcase v0.1.3` → `v0.3.0`, sqlite driver `v1.1.4` → `v1.6.0` (identifier quoting now escapes backticks)
- `govulncheck ./...`: no known vulnerabilities

### Performance

- Parse path ~34% faster, ~40% fewer bytes, 53 → 43 allocs/op; per-request regex compilation eliminated
