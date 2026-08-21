# Changelog

All notable changes to qvd2parquet are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the major version is `0`, a minor bump may change conversion defaults;
those changes are called out under **Changed** with the flag that restores the
previous behaviour.

## [Unreleased]

### Changed

- **`--dual` now defaults to `auto`.** A Qlik dual's display string is written
  as a `${name}__text` column only when it carries something the numeric column
  does not. A localized number such as `1.234,56`, or a date rendered beside a
  column that already encodes it, is redundant and dropped; a label such as
  `Open` beside `1` is kept, and the reason is reported. One informative string
  is enough to keep the column, so the default errs towards preserving data.
  `--dual=numeric`, `text` and `columns` still force a choice.

  Redundancy is judged against the value that will actually be written, not the
  raw payload, so a `MONEY` field carrying `1.234` and displaying `1.23` at
  scale 2 does not produce a text column.

- The banner now carries the year: `(c) 2026, RALFORION d.o.o.`

### Added

- `--inspect` reads the XML header and the symbol tables, prints the schema a
  conversion would produce, and exits without touching the record area. The
  cost is independent of row count: on a 29 MiB, 5-million-row file it reads
  7.9 KiB and finishes in 0.01s, against 1.58s for the full conversion. All
  type policy flags apply, so the report shows what a conversion would write.
  A file the type policy rejects prints the reason plus the raw symbol profiles
  that explain it and exits `3`, which makes it a cheap pre-flight check. The
  report goes to stdout; `--schema-report` also works in inspect mode.

- Qlik's semantic field tags are now read. A field declaring no
  `NumberFormat/Type` but tagged `$date`, `$timestamp`, `$time` or `$interval`
  is resolved to that type. This works for plain numeric fields carrying no
  dual display strings, which no amount of text inspection could identify, and
  it is what Qlik Sense writes. A declared type still wins over a tag.

- A date or timestamp column is now validated when the schema is resolved,
  whatever decided its type -- the declared header, a Qlik tag, or display-string
  inference. A value that cannot be converted fails as a schema policy error
  naming the column and the value, so `--inspect` predicts it and no output file
  is started, instead of the conversion failing part-way through.

- `--infer-dates` (on by default) is the fallback for files that carry no tags:
  a column with no declared type is read as a date or timestamp when every
  display string renders its Excel-style serial value as one. The check is
  format-agnostic and accepts a neighbouring day, since the string was rendered
  in whatever timezone wrote the file and a whole-hour offset can move the
  calendar date. Serials outside roughly 1900 to 2200 are never read as dates,
  blank display strings are not evidence, and a column mixing dates with
  anything else is left alone.

  Only words that belong to a rendered date -- month and weekday names in
  English and German, meridiem markers, ordinal suffixes and timezone
  abbreviations -- may appear alongside the digits, and a month name that
  contradicts the value is rejected. So `"Due 11/20/2010"` and `"20 Jan 2010"`
  beside a November value both keep their text column. The list errs short: a
  word wrongly rejected costs a redundant column, whereas one wrongly accepted
  drops text that carried information.

## [0.3.1] - 2026-08-21

### Fixed

- An explicit `--numeric-promote=decimal` fell back to `float64` unless
  `--decimal-strict` was also set, contradicting the documented contract that
  passing the flag makes decimal a demand. The guard was written when
  `--decimal-strict` still defaulted to `true`; flipping that default in 0.3.0
  silently disabled the demand path. Explicitness alone now decides.
  `--decimal-strict` governs rounding a value that does not fit an established
  scale; having no inferable scale at all means the column is not
  decimal-shaped, which is a different question.
- Binaries built from source reported `0.1.0` while 0.3.0 was tagged, because
  only release archives pass `-ldflags`. A binary produced by
  `go install github.com/ralforion/qvd2parquet/cmd/qvd2parquet@vX.Y.Z` now
  reports its tag from the embedded build info. An `-ldflags` stamp still wins,
  pseudo-versions and `(devel)` are ignored, and the banner renders the same
  string on every build path.

### Documentation

- The output example is captured from a real run rather than hand-edited, and
  the documented flag list is verified against `--help`.

## [0.3.0] - 2026-08-21

### Changed

- **`--numeric-promote` now defaults to `decimal`.** A column carrying
  fractional values resolves to an exact Parquet decimal rather than `float64`.
  This is the right type for the price columns QlikView declares as plain
  `REAL`, where the header carries no usable scale: `nDec` holds a filler value
  (commonly 14) and the display format has no decimal separator. The scale is
  derived from the values themselves — the smallest at which every value is
  exactly representable, bounded at 9 decimal places.

  Pure-integer columns stay `int64`, since `decimal(p,0)` gains nothing, and a
  declared `MONEY`/`FIX` keeps using its own `nDec`.

  When no scale within the bound represents every value, the column really is
  floating point and is written as `float64`. That fallback is silent for the
  default, because a default is a preference rather than a demand; passing
  `--numeric-promote=decimal` explicitly makes it a demand that fails instead.

  Restore the previous behaviour with `--numeric-promote=true`.

  *An inferred scale describes the data actually present, so a later extract
  containing more decimals can resolve the same column to a different scale.
  Pin the column with `--schema` where a stable schema matters.*

- **`--decimal-strict` now defaults to `false`.** A value that does not fit its
  declared scale is rounded to it, half away from zero — the same value Qlik
  displays for a field with `nDec` decimals. Rounding is counted and reported on
  stderr and in `--schema-report`, so it is never silent. A value is still never
  dropped.

  Restore the previous behaviour with `--decimal-strict`, which `--strict` also
  implies.

### Added

- `--exclude` takes comma-separated shell-style wildcard patterns and skips
  matching fields, e.g. `--exclude '%*'` to drop QlikView's internal key fields
  from a SAP extract. Patterns match the original QVD name, before renaming.
  Matching has no path semantics, since QVD field names routinely contain `/`
  and `\`. Excluding every column is an error.
- `--field-regex` rewrites composite field names such as
  `A057-||-DATBI-||-Ende Gültigkeit`. Capture groups named `name` and `comment`
  are used by default, so no other flag is needed; `--field-name` and
  `--field-comment` accept Go regexp templates when named groups are not enough.
  A field the expression does not match keeps its name. The description is
  written as Parquet field metadata and the original QVD name is preserved under
  `qvd.field`, so nothing is lost. Two fields collapsing to one name are
  rejected as a schema policy error, and a regex that would blank every name is
  rejected up front.

## [0.2.0] - 2026-08-21

### Added

- `--numeric-promote=decimal`, resolving `REAL`-declared price columns to exact
  Parquet decimals with the scale inferred from the values. `--numeric-promote`
  became a tri-state (`true`, `false`, `decimal`); the boolean spellings still
  parse. The default remained `float64` in this release.
- Dependabot for Go modules and workflow actions.

## [0.1.0] - 2026-08-21

First release.

### Added

- Convert standard, unencrypted Qlik QVD files to Parquet, preserving useful
  types instead of stringifying everything.
- Exact decimals for `MONEY` and `FIX`, carried as scaled integers end to end so
  no step rounds through a double. Precision and scale are inferred and reported.
- Parallel record decoding. Each worker owns its Arrow builders and reads its
  byte range with `ReadAt`; a single writer goroutine feeds the Parquet writer.
  Chunks are written as workers finish, so **row order is not preserved**. Every
  quality metric is order-independent.
- An explicit mixed-type policy (`--mixed`, `--dual`, `--numeric-promote`,
  `--mixed-string-fallback`) resolved only after symbol profiling, so the schema
  never depends on which rows were seen first.
- Qlik serial date/time conversion with `--timezone`, to `date32`,
  `timestamp[ms]` and `time32[ms]`.
- A four-mode quality gate (`none`, `basic`, `numeric`, `full`) validating the
  written file against metrics collected from the values actually produced.
  `full` uses order-independent multiset fingerprints, so it is valid despite
  unordered chunk delivery.
- `--schema` overrides and `--schema-report`, both validated against the real
  symbols before anything is written.
- `--columns`, `--batch-rows`, `--workers`, `--compression`, `--progress`,
  `--force`, `--strict`, and documented exit codes.
- Output is written to a temporary file and renamed only on success, so a failed
  run never replaces a good Parquet file.
- Binaries for 16 platforms: Linux (amd64, arm64, 386, arm, ppc64le, s390x,
  riscv64), Windows (amd64, arm64, 386), macOS (amd64, arm64), FreeBSD (amd64,
  arm64), NetBSD and OpenBSD (amd64).

### Validation

- Verified against a QVD written by QlikView build 11282: all 77 rows and 9
  columns match the Java reference reader's CSV output exactly.
- The symbol tag layout matches the independent
  [pyqvd](https://pyqvd.readthedocs.io/stable/guide/qvd-file-format.html)
  description of the format.

[Unreleased]: https://github.com/ralforion/qvd2parquet/compare/v0.3.1...HEAD
[0.3.1]: https://github.com/ralforion/qvd2parquet/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/ralforion/qvd2parquet/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ralforion/qvd2parquet/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ralforion/qvd2parquet/releases/tag/v0.1.0
