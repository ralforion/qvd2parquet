# qvd2parquet

A fast command-line converter from Qlik QVD files to Parquet.

```sh
qvd2parquet input.qvd output.parquet
```

It reads standard, unencrypted QVD files, preserves useful Parquet types
instead of stringifying everything, keeps `MONEY` and `FIX` columns as exact
Parquet decimals, decodes records in parallel, and streams batches into Parquet
row groups so large files never need to be materialized in memory.

## Install

### Prebuilt binaries

Download the archive for your platform from the
[releases page](https://github.com/ralforion/qvd2parquet/releases), unpack it,
and put `qvd2parquet` on your `PATH`. Verify the download against
`SHA256SUMS`:

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing
```

Binaries are pure Go and statically linked (`CGO_ENABLED=0`), so they have no
runtime dependencies. Supported platforms:

| OS | Architectures |
| --- | --- |
| Linux | `amd64`, `arm64`, `386`, `arm`, `ppc64le`, `s390x`, `riscv64` |
| Windows | `amd64`, `arm64`, `386` |
| macOS | `amd64` (Intel), `arm64` (Apple silicon) |
| FreeBSD | `amd64`, `arm64` |
| NetBSD / OpenBSD | `amd64` |

On Windows, run `qvd2parquet.exe` from PowerShell or `cmd`. macOS may quarantine
a downloaded binary; clear it with
`xattr -d com.apple.quarantine qvd2parquet`.

### From source

Requires Go 1.25 or newer. That floor comes from `github.com/apache/arrow-go/v18`;
the `go` directive in `go.mod` is a minimum, not a pin, so the module also
builds with 1.26 and 1.27. CI tests against both the 1.25 floor and the newest
release, and release binaries are built with the newest toolchain.

```sh
go install github.com/ralforion/qvd2parquet/cmd/qvd2parquet@latest
```

or

```sh
go build -o qvd2parquet ./cmd/qvd2parquet
```

### Building all release artifacts

```sh
./scripts/build-release.sh                        # version from git describe
VERSION=v1.2.3 ./scripts/build-release.sh          # explicit version
PLATFORMS="linux/amd64 windows/amd64" ./scripts/build-release.sh
```

This writes `.tar.gz` (Unix) and `.zip` (Windows) archives plus `SHA256SUMS`
into `dist/`. Pushing a `v*` tag runs the same script in CI and publishes a
GitHub release.

## Usage

```text
qvd2parquet [options] input.qvd output.parquet

  -columns name1,name2       Convert only these columns
  -mixed error               Mixed-type strategy: error|string|promote|dual-columns
  -dual numeric              Dual strategy: numeric|text|columns
  -numeric-promote decimal   Numeric widening: decimal | true (float64) | false
  -mixed-string-fallback     Convert otherwise-invalid mixed columns to string
  -decimal-source auto       Decimal extraction: auto|text|numeric
  -decimal-strict            Fail instead of rounding when a value does not fit its scale
  -compression zstd          Parquet compression: zstd|snappy|gzip|uncompressed
  -batch-rows 65536          Rows per Arrow batch and Parquet row group
  -workers 0                 Decode workers, 0 means runtime.NumCPU()
  -timezone Local            Local|UTC|IANA timezone name
  -schema path.json          Explicit schema override
  -schema-report path.json   Write the inferred schema/profile report
  -quality-gate none         Validation mode: none|basic|numeric|full
  -quality-report path.json  Write the post-conversion quality report
  -quality-tolerance 1e-9    Relative tolerance for floating-point quality checks
  -quality-abs-tolerance 0   Absolute tolerance for floating-point quality checks
  -progress 1000000          Log every N rows, 0 disables progress
  -force                     Overwrite an existing output file
  -strict                    Enable strict validation defaults
```

### Output

The conversion writes the Parquet file; everything else — the identification
banner, the per-column schema decisions, progress and the final summary — goes
to **stderr**. **stdout stays empty**, so `qvd2parquet` composes safely in
pipelines and shell substitutions.

```text
$ qvd2parquet --timezone UTC --quality-gate numeric sales.qvd sales.parquet
qvd2parquet 0.1.0  (c) RALFORION d.o.o.
qvd2parquet: sales.qvd: table "products", 77 rows, 7 bytes/record, 9 of 9 columns selected
qvd2parquet: read 412 symbols in 1ms; records start at offset 8973
qvd2parquet: schema: Einkaufspreis: REAL with 75 double symbols, written as float64
qvd2parquet: schema: Produktname: 77 text symbols, written as utf8
qvd2parquet: schema: Listenpreis: 25 integer and 35 double symbols promoted to float64
qvd2parquet: converted 77/77 rows in 2ms (42445 rows/s)
qvd2parquet: quality gate numeric finished in 1ms: passed
qvd2parquet: wrote sales.parquet: 77 rows, 9 columns, 4.9 KiB in 12ms (6227 rows/s)
```

One `schema:` line is printed per output column, explaining exactly why each
type was chosen. That is the first thing to read when a mixed-type column
fails; `--schema-report` writes the same reasoning as JSON.

Print the version and exit with `--version`:

```text
$ qvd2parquet --version
qvd2parquet 0.1.0  (c) RALFORION d.o.o.
```

### Examples

Convert with reproducible timestamps and validated numerics:

```sh
qvd2parquet --timezone UTC --quality-gate numeric --quality-report out.quality.json \
  sales.qvd sales.parquet
```

Convert a subset of a very large QVD with bounded memory:

```sh
qvd2parquet --columns CustomerID,OrderDate,Amount --batch-rows 16384 --workers 4 \
  orders.qvd orders.parquet
```

Strip QlikView's internal key fields and shorten composite SAP field names,
keeping the description as a column comment:

```sh
qvd2parquet \
  --exclude '%*' \
  --field-regex '^[^-]*-\|\|-(?P<name>[^-]*)-\|\|-(?P<comment>.*)$' \
  A057.qvd a057.parquet
```

Understand why a column resolved the way it did:

```sh
qvd2parquet --schema-report schema.json sales.qvd sales.parquet
```

Verify the output with an external reader:

```sh
duckdb -c "describe select * from read_parquet('sales.parquet')"
duckdb -c "select count(*) as n from read_parquet('sales.parquet')"
```

## Selecting and renaming fields

`--columns` keeps only the named fields. `--exclude` drops fields matching
shell-style wildcard patterns (`*` and `?`, case-insensitive), which is the
usual way to strip QlikView's internal key fields from a SAP extract:

```sh
qvd2parquet --exclude '%*' A057.qvd a057.parquet
```

Patterns match the field's **original** QVD name, before any renaming, so they
describe what you see in the source file. Excluding every column is an error.

QVD field names from SAP extracts are often composite, packing the table, the
technical name and a description into one string:

```text
A057-||-DATBI-||-Ende Gültigkeit
```

`--field-regex` rewrites them. Name the capture groups `name` and `comment` and
no other flag is needed:

```sh
qvd2parquet \
  --exclude '%*' \
  --field-regex '^[^-]*-\|\|-(?P<name>[^-]*)-\|\|-(?P<comment>.*)$' \
  A057.qvd a057.parquet
```

```text
qvd2parquet: excluded 2 column(s) by pattern: %A057_PKEY, %SYS_TS
qvd2parquet: schema: A057-||-DATBI-||-Ende Gültigkeit: INTEGER with 2 integer symbols, written as int64; written as "DATBI" with comment "Ende Gültigkeit"
qvd2parquet: schema: A057-||-KBETR-||-Betrag: REAL with 2 double symbols promoted to decimal(4,2); scale 2 inferred from values; written as "KBETR" with comment "Betrag"
```

The result carries the description as Parquet field metadata, and keeps the
original QVD name so nothing is lost:

```text
DATBI  int64             {"comment": "Ende Gültigkeit", "qvd.field": "A057-||-DATBI-||-Ende Gültigkeit"}
KSCHL  string            {"comment": "Konditionsart",   "qvd.field": "A057-||-KSCHL-||-Konditionsart"}
KBETR  decimal128(4, 2)  {"comment": "Betrag",          "qvd.field": "A057-||-KBETR-||-Betrag"}
```

Rules worth knowing:

- A field the expression does not match keeps its original name and gets no
  comment, so one rule can target a subset of the fields.
- `--field-name` and `--field-comment` accept Go regexp templates (`$1`,
  `${name}`) when named groups are not enough, e.g. `--field-name '${1}_${2}'`.
- Two fields that collapse to the same output name are rejected as a schema
  policy error rather than producing a duplicate Parquet column.
- A regex with no `name` group and no explicit `--field-name` is rejected up
  front, since it would blank every column name.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | CLI usage error |
| 2 | unsupported QVD feature |
| 3 | schema/type policy error |
| 4 | input read/decode error |
| 5 | output/write error |
| 6 | quality gate failure |

## Type resolution

The Parquet schema is resolved only after every selected column's symbol table
has been read and profiled. That makes mixed-type behaviour explicit instead of
producing an unstable schema that depends on which rows were seen first.

| QVD field | Parquet / Arrow type |
| --- | --- |
| `INTEGER` with integer symbols | `int64` |
| `REAL` with double symbols | `decimal128(p, s)`, scale inferred from the values; `float64` if no exact scale exists |
| `MONEY`, `FIX` | `decimal128(p, s)` — never `float64` |
| `DATE` | `date32` (days since the Unix epoch) |
| `TIMESTAMP` | `timestamp[ms, tz=<--timezone>]` |
| `TIME` | `time32[ms]` (milliseconds since midnight) |
| `ASCII` or text-only symbols | `utf8` |
| all-null column | nullable `utf8` |

Every output column is nullable.

### Mixed-type columns

A QVD column can hold several symbol encodings at once. Some combinations are
harmless, some are not:

- `int + null`, `float + null` — fine.
- `int + float` — resolved to an exact decimal by default
  (`--numeric-promote=decimal`), or widened to `float64` with
  `--numeric-promote=true`.
- dual numeric + display string — a normal Qlik concept, resolved by `--dual`.
- number + unrelated text — never silently made numeric.

`--mixed` selects what happens to the last case:

| Value | Behaviour |
| --- | --- |
| `error` (default) | fail with the counts of each symbol kind and the flag that would fix it |
| `string` | write the whole column as UTF-8; the display string wins for duals |
| `promote` | keep numerics numeric and pure text as text; still fail on number + text unless `--mixed-string-fallback` |
| `dual-columns` | write the numeric side under the original name and the display side as `${name}__text` |

`--dual` selects which side of a Qlik dual is written: `numeric` (default),
`text`, or `columns` for both. `--mixed=dual-columns` implies `--dual=columns`.

Output column names must be unique. If a generated `${name}__text` column would
collide with a real source column of that name, the conversion fails with a
schema policy error rather than writing an ambiguous Parquet schema.

The defaults — `--mixed=error --numeric-promote=decimal --dual=numeric
--mixed-string-fallback=false` — stop an ETL job on unexpected schema drift
while still handling the common `int + float` and dual-numeric cases.

### Numeric widening

`--numeric-promote` controls how a numeric column that is **not** already a
declared `MONEY`/`FIX` is widened:

| Value | Behaviour |
| --- | --- |
| `decimal` (default) | resolve any column carrying fractional values to an exact decimal |
| `true` | widen `int + float` to `float64` |
| `false` | refuse to widen; a mixed numeric column is a policy error |

`--numeric-promote=decimal` exists because QlikView often declares a price as
plain `REAL`. In that case the header carries no usable scale — `nDec` holds a
filler value (commonly 14) and the display format has no decimal separator — so
the scale is derived from the values themselves: the smallest scale at which
every value is exactly representable, up to 9 decimals.

```text
$ qvd2parquet products.qvd out.parquet          # decimal is the default
qvd2parquet: schema: Einkaufspreis: REAL with 75 double symbols promoted to decimal(5,2); scale 2 inferred from values
qvd2parquet: schema: Listenpreis: REAL with 25 integer and 35 double symbols promoted to decimal(5,2); scale 2 inferred from values
qvd2parquet: schema: MengeAufLager: INTEGER with 51 integer symbols, written as int64
```

Pure-integer columns are left as `int64`, since `decimal(p,0)` would gain
nothing, and a declared `MONEY`/`FIX` keeps using its own `nDec` rather than a
value-inferred scale.

If no scale within 9 decimals represents every value, the column really is
floating point and is written as `float64`. That fallback is silent by default,
because the default is a preference rather than a demand and `float64` is what
the column would have resolved to anyway. Passing `--numeric-promote=decimal`
explicitly turns it into a demand: the conversion then fails with a schema
policy error naming the column, so you can pin it with `--schema`.

**Caveat worth knowing.** An inferred scale describes the data actually
present, so a later extract containing a value with more decimals can resolve
the same column to a different scale. For a pipeline that must produce a
stable schema across runs, pin the column instead:

```json
{ "columns": { "Listenpreis": { "type": "decimal", "precision": 18, "scale": 2 } } }
```

### Exact decimals

`MONEY` and `FIX` are always written as Parquet decimals. Values are carried as
scaled integers end to end, so no step of the pipeline rounds through a double.

- Scale comes from `NumberFormat/nDec`, or is inferred from display strings when
  `nDec` is absent. If neither is available the conversion fails and asks for a
  `--schema` override.
- Precision is inferred from the scaled values actually present, sign excluded.
  Exceeding decimal128's 38 digits fails with a clear message.
- `--decimal-source=auto` (the default) prefers the dual display string, which
  preserves decimal intent better than the binary double, and falls back to
  scaling the numeric payload. `text` and `numeric` force one source.
- By default a value that does not fit the declared scale is **rounded** to it,
  half away from zero — the same value Qlik itself displays for a field with
  `nDec` decimals. Rounding is counted and reported, both on stderr and in
  `--schema-report`, so it is never silent:

  ```text
  qvd2parquet: schema: Amount: MONEY, written as decimal(9,2); ...; 3 value(s) rounded to scale 2 (--decimal-strict=false)
  qvd2parquet: note: 3 decimal value(s) were rounded to their column's scale; pass --decimal-strict to fail instead
  ```

- `--decimal-strict` restores the stricter behaviour: the conversion fails
  naming the column and the offending value. Use it in a pipeline where an
  unexpected precision change must stop the job. `--strict` implies it.
- A value is never dropped. Turning an inexact value into a null would lose
  data that no later check could recover, since the quality metrics describe
  the converted value.

The schema report records the resolved precision and scale and whether the
digits came from display strings, numeric payloads, or both.

### Schema overrides

```json
{
  "columns": {
    "Amount": { "type": "decimal", "precision": 18, "scale": 4 },
    "CustomerID": { "type": "string" }
  }
}
```

Supported types: `string`, `int64`, `float64`, `date32`, `timestamp`, `time`,
`decimal`. Overrides are validated against the actual symbols before anything is
written, so pinning a column holding doubles to `int64`, or a text column to
`date32`, fails as a schema policy error (exit code 3) rather than silently
truncating or failing part-way through the conversion.

A pinned `timestamp` carries the run's `--timezone`, so its Parquet metadata
always matches the timezone the values were converted with.

### Dates and times

Qlik stores dates and times as serial day numbers where `25569` is
1970-01-01. `--timezone` decides how a serial timestamp's wall-clock reading is
mapped onto an instant:

- `Local` (default) matches the Java reference reader's behaviour.
- `UTC` is recommended for reproducible ETL, since the output no longer depends
  on the machine's timezone.
- Any IANA name (`Europe/Berlin`) also works.

`date32` and `time32[ms]` are timezone independent. Rounding follows the Java
reference reader's `Math.round`.

## Parallel decoding

The record area is fixed-width, so once the symbol tables are read it can be
split into contiguous row ranges and decoded concurrently. Each worker owns its
Arrow builders, reads its byte range with `ReadAt`, and emits one Arrow record
plus chunk-local quality metrics. A single writer goroutine feeds the Parquet
writer, which is not safe for concurrent use.

**Row order is not preserved.** Chunks are written as workers finish, so the
Parquet file holds the same multiset of rows as the QVD but not in the same
physical order. Every quality metric is order-independent, so validation is
unaffected. Do not rely on physical row order in downstream queries.

On a failure the context is cancelled, in-flight chunks are drained, and the
temporary output is removed.

## Quality gate

`--quality-gate` validates the written Parquet file against metrics collected
from the values the converter actually produced. Validation always reads the
temporary file *before* the final rename, so a failed gate never leaves a
final-looking output behind.

| Mode | What it checks |
| --- | --- |
| `none` (default) | nothing |
| `basic` | the file opens; row count, column names, and types match the resolved schema; per-column null counts match |
| `numeric` | everything in `basic` plus sum, min, max (and sum of squares for floats) per numeric, decimal, date, timestamp and time column |
| `full` | everything in `numeric` plus order-independent `sha256` value fingerprints per column |

Integer, decimal and date/time aggregates are compared exactly — decimal sums
use scaled-integer arithmetic, with no floating-point tolerance. Floating-point
sums use both tolerances:

```text
abs(a-b) <= absTolerance || abs(a-b) <= relTolerance * max(abs(a), abs(b), 1)
```

`full` mode builds a multiset fingerprint (row count, XOR of digests, and a
modular sum of digests) rather than an ordered stream hash, so it is valid
despite unordered chunk delivery. Nulls are marked explicitly in the digest, so
a null never collides with a zero or an empty string.

`--quality-report` is written on success and on failure.

Recommended production setting:

```sh
--quality-gate=numeric --quality-report out.quality.json
```

## Performance

Measured on an Apple M3 Max (16 cores) over a 200k-row synthetic fixture with
integer, high-cardinality string, decimal, date and nullable double columns.

Decode only (no Parquet writing):

| Workers | Rows/s |
| --- | --- |
| 1 | 5.5M |
| 2 | 8.1M |
| 4 | 14.7M |
| 8 | 14.2M |
| NumCPU | 15.5M |

Scaling flattens around 4-8 workers on this fixture because symbol resolution
becomes memory-bound. The `--workers=0` default (one per CPU) is a good starting
point; lower it to reduce peak memory.

Full pipeline including Parquet writing:

| Compression | Rows/s | Output size |
| --- | --- | --- |
| zstd | 3.3M | 101 KB |
| snappy | 3.1M | 235 KB |
| uncompressed | 3.2M | 2.4 MB |

`zstd` is both the smallest and the fastest here, which is why it is the default.

Quality gate overhead (100k rows, full pipeline):

| Mode | Rows/s | Overhead vs `none` |
| --- | --- | --- |
| `none` | 2.8M | — |
| `basic` | 2.0M | ~1.4x |
| `numeric` | 2.0M | ~1.4x |
| `full` | 0.48M | ~5.8x |

`basic` and `numeric` cost about the same because both must read the whole
Parquet file back. `full` adds a `sha256` digest per cell, which dominates.

Batch size (`--batch-rows`) peaks around 16k-64k rows; larger batches trade
throughput for memory.

Reproduce:

```sh
go test ./internal/convert -run XXX -bench . -benchtime 3x
/usr/bin/time -l ./qvd2parquet input.qvd output.parquet   # macOS
/usr/bin/time -v ./qvd2parquet input.qvd output.parquet   # Linux
```

### Memory

Peak memory is roughly:

- the symbol tables of the selected columns, plus
- `workers * batch-rows` of Arrow builder memory, plus
- Parquet writer buffers, plus
- one `batch-rows * RecordByteSize` scratch buffer per worker.

Nothing scales with the total row count. To reduce peak memory on a large file,
use `--columns` to skip wide string columns, then lower `--batch-rows` and
`--workers`.

## Not supported in v1

- Encrypted QVD files.
- Writing QVD files.
- Nested Parquet output.
- Locale-specific display formatting.
- Disk-backed symbol table caching.
- Preserving the QVD's physical row order.

## Development

```sh
go test ./...              # unit and integration tests
go test -race ./...        # the parallel decoder is race-tested
go vet ./...
```

See `testdata/README.md` for fixture setup.

Package layout:

| Package | Responsibility |
| --- | --- |
| `internal/qvd` | QVD format parsing only — header, symbols, bit unpacking, Qlik time. No Parquet dependency. |
| `internal/convert` | Profiling, schema resolution, type policy, decimals, parallel decoding, quality metrics. |
| `internal/parquetwrite` | Arrow/Parquet writer, temporary output and atomic rename. |
| `internal/qvdtest` | Synthetic QVD builder for tests and benchmarks. |
| `cmd/qvd2parquet` | CLI parsing, progress logging, orchestration. |

### Relationship to the Java reader

The behaviour is modelled on the Java `QvdReader` in `../qvd-reader`, with two
deliberate differences:

- Symbol tags `0x05` and `0x06` always carry a display string after the numeric
  payload, and this reader always consumes it. The Java reader skips the string
  for `INTEGER`/`REAL` fields, which desynchronizes the symbol stream.
- Record bits are extracted per field from `BitOffset`/`BitWidth` instead of
  being accumulated bit by bit with a range scan. The results are identical; the
  unit tests check the fast path against a direct port of the Java loop.

Symbol tables are read sequentially, using each field's declared `Length` to
advance, exactly as the Java reader does. The per-field `Offset` in the header
is not used for seeking.

The symbol tag layout matches the independent
[pyqvd](https://pyqvd.readthedocs.io/stable/guide/qvd-file-format.html)
description of the format, which likewise documents `0x05`/`0x06` as a numeric
payload *followed by* a null-terminated string.

`TestRealQVDProducts` converts a QVD written by QlikView build 11282 and
compares every cell against the CSV the Java reader produced from the same
file. All 77 rows and 9 columns match exactly.
