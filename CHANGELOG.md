# Changelog

All notable changes to qvd2parquet are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
From 2.0.0 the CLI surface and the conversion defaults are stable: a flag will
not be removed or change its meaning, and a default will not change what an
existing file converts to, outside a major bump. New behaviour arrives behind a
new flag or a new value for an existing one. 2.0.0 is that major bump: it is
the release where the defaults were re-chosen for wide files, and the promise
restarts from it.

## [Unreleased]

### Added

- `--skip-up-to-date` leaves a file alone when the run already produced its
  output, for a folder re-extracted nightly where most inputs have not changed.
  It is not a timestamp comparison: the run keeps a record in
  `.qvd2parquet-manifest.json` under `--out-dir` and skips only when the
  manifest names the output, the entry names this input, the input's size and
  timestamp still match, the output has not been replaced since, and the
  conversion options fingerprint the same. Mtime alone cannot see a changed flag, is fooled by an extract
  copied with its timestamps preserved, and trusts two clocks on a network
  share to agree.
- The fingerprint covers every option that can change what is written,
  including the contents of a `--schema` override rather than only its path,
  the offsets of `--timezone` rather than its name, and the tool's major
  version, which the stability promise makes sufficient. A timezone is
  fingerprinted by what it does because `time.Local` is called `Local` on every
  machine whose `TZ` is unset, and `--timezone Local` writes timestamps against
  the converting machine's zone.
  Options that cannot change the output are excluded by name, so a new option
  counts by default: the mistake it can make is an unnecessary conversion
  rather than a wrong skip.
- `--skip-up-to-date` is selection and `--force` is write permission, so a
  nightly job passes both and a full rerun drops the one flag. Nothing is
  skipped by default, the manifest is written only when the flag is passed, and
  a file that failed is not recorded.
- A wildcard in the last element of an input path is expanded by qvd2parquet
  when the shell has not done it, so `qvd2parquet --out-dir out qvds\CE*.qvd`
  works in `cmd.exe` and PowerShell, neither of which expands for an external
  command. It takes `.qvd` files and directories only, and a wildcard in a
  directory element is refused rather than half-supported.
- `--include-files` and `--exclude-files` narrow what a directory contributes
  to a folder conversion, which `--recursive` needs since no path expansion
  reaches into a tree. Patterns are the usual case-insensitive `*` and `?`,
  matched against the file name with and without its extension, and exclude
  wins over include. They filter a directory's contents only: a file named on
  the command line was meant.
- Files a pattern dropped, and a pattern that reached no file, are both
  reported. A selection that leaves nothing is a usage error naming the
  patterns rather than the message for an empty folder.

### Documentation

- How `cmd.exe` treats `%` in a pattern, which is what makes
  `--encoding "%*_PKEY=delta_byte_array"` match no column when the same line is
  run from a `.bat` file rather than typed at the prompt. `--exclude '%*'` is
  affected the same way.

## [2.2.0] - 2026-08-28

Everything a run reports about itself gets more honest. Two of the release's
threads come from the same 213-column SAP extract that shaped 2.1.0: a profile
is only useful if it renders a value in the type the column is written as, and
a composite primary key gets nothing from the dictionary encoding the writer
defaults to. `--encoding auto` answers the second by measuring rather than
guessing, since whether `delta_byte_array` pays depends on the row order and
nothing in the symbol table reveals it.

The other thread is `--log`, which was accepted and silently ignored on every
single-file conversion, and which could be pointed at a file the run itself
writes. That destroyed the run's own output or its log and still exited 0.

No flag changed meaning and no conversion produces different data, so this is
minor rather than major: `--encoding` and `--encoding auto` change nothing
without being asked for, and the compatibility promise from 2.0.0 holds.

### Added

- An `--exclude` pattern that matches no field is reported instead of passing
  silently. `--columns` already fails on a name that matches nothing, while
  `--exclude` accepted anything, and both ways of writing a pattern wrongly
  look exactly like success: `%` is not the wildcard `%*` and matches only a
  field named `%`, and the name `--field-regex` produces is never what
  `--exclude` sees, since exclusion is decided first. It stays a note rather
  than an error, because one command line is often pointed at a folder of
  tables that do not all carry the same fields. The patterns appear in
  `--inspect`, in the run log, and in a `--log` record as `excludeNoMatch`.
- `--field-regex` says which fields it left alone. A field the expression does
  not match keeps its original name, which is what lets a rule target a subset,
  but on a 213 column SAP extract the two fields that stayed behind are
  invisible among the renamed ones. Runs now close the schema notes with
  `2 of 3 field(s) renamed, 1 unchanged: PlainField`, naming at most five.
  `--schema-report` carries the full list under `fieldRegex`, and a `--log`
  record carries `fieldsRenamed` and `fieldsUnchanged` as counts, since a
  record there is one line per file.
- `--inspect` and `--schema-report` show each column's value range, rendered in
  the type the column is written as. A QVD stores a date as a serial day
  number, so a profile reported `WADAT` as 38365..411241 and a goods-issue date
  in the year 3025 read as an ordinary integer among plausible neighbours. The
  same range now reads `2005-01-13 .. 3025-12-08`, next to a `BUDAT` of
  `2019-12-27 .. 2026-08-31`. Timestamps follow the run's timezone rules, so
  the rendering matches what would be written rather than assuming UTC.
- Decimal columns whose widest value already fills most of the type's range are
  named. A decimal's precision is inferred from its values, so it fits them
  exactly by construction and the question is never whether the data fits; it
  is what a later load has left. On a 213-column SAP extract exactly one column
  qualified, `VV120` at 81% of a `decimal(12,2)`, which one larger value would
  have taken out of range. `--schema-report` carries the `limit` and
  `usedFraction` per column, and the `--log` record carries
  `decimalsNearLimit`, the names alone, since a record there is one line per
  file and stays that way.
- `--encoding` pins a column to a Parquet encoding, as
  `--encoding '%*_PKEY=delta_byte_array'`. A column whose values are nearly all
  distinct gets nothing from the default dictionary: the dictionary page
  overflows, the writer falls back to `PLAIN`, and the column is stored as raw
  bytes with only the compressor working on it. A Qlik composite primary key is
  that column, one distinct value per row. Patterns are wildcards over both the
  output name and the original QVD name, so one rule covers a folder of SAP
  tables whose keys are named per table, and a later rule wins over an earlier
  one. An encoding the column's type cannot carry is refused before the
  conversion starts, naming the ones that fit, and `--inspect` shows what a run
  would pin. Nothing changes without the flag.
- `--encoding auto` measures the choice instead of guessing at it. Whether
  `delta_byte_array` pays depends on the order the rows arrive in, and nothing
  in the symbol table reveals that order, so sampled rows are written through
  the real writer twice, once as the run would today and once with each
  candidate, and the compressed column chunks are compared. Three windows of
  100,000 consecutive rows, at head, middle and tail, land within about a point
  of the whole file, and the estimate converges from above, so it understates a
  win rather than overselling one; a file of 300,000 rows or fewer is measured
  in full. On a 3M row key that arrives in document order the
  sample measured 31% and the conversion wrote 1.8 MiB against the default's
  6.2 MiB; on the same values shuffled nothing is adopted and the run says so.
  `--inspect --encoding auto` reports the measurement without converting, and
  is the only thing that makes inspect read records at all. Over `--out-dir`
  each file is measured on its own, since no pattern can know what a given
  table's key looks like. A column an explicit rule names is not measured at
  all, so the tool never recommends against a decision already taken, and
  `--schema-report` records the encoding each column is actually written with,
  measured or pinned.
- Progress lines say how far along a phase is and roughly how long is left:
  `converted 5000000/20589661 rows (24%) in 3m21s (24875 rows/s, about 10m26s
  left)`. The row total is read from the QVD header before a record is
  decoded, so both come from numbers the run already holds. The throughput
  shown is still the average since the phase started, while the estimate
  follows the recent rate, because a run carries its startup cost in the
  average long after it has found its speed. The quality gate projects
  separately, having its own total and its own speed.

### Fixed

- `--log` now writes a file record and summary for single-file conversions. It
  was accepted but silently ignored outside `--out-dir` mode.
- `--log` is refused when it names a file the run itself writes, in batch mode
  as well as for a single conversion. The log is created with `O_TRUNC`, so a
  collision destroyed whichever of the two was written second while the run
  still reported writing both. A batch was the worse case because none of the
  paths at risk are typed on the command line: the inputs come from expanding
  directories, and every output and per-file report is derived from an input
  under `--out-dir`. Pointing `--log` at an input truncated a 17 KiB QVD to a
  few hundred bytes of JSON Lines reporting that the file it had just destroyed
  was not a QVD, and pointing it at a generated output exited 0 having written
  the Parquet and no log at all. An input the run could not examine counts as
  an input here too: it is reported as a failed file and written to the log, so
  a log allowed to take its path named the file as missing and created it in
  the same breath.
- A batch run reports each file's progress. `--out-dir` passed a nil logger to
  the converter, so it discarded the schema notes, the row counts and the
  quality gate's progress: a folder holding one large table printed a line on
  starting and nothing again until it finished, which on a twenty-million-row
  table is a quarter of an hour of silence. Converting several files at once,
  each line names the file it belongs to, and the writer is serialized so two
  files cannot interleave mid-line.

### Changed

- `--decimal-strict` reports up to three offending values per column instead of
  stopping at the first, with the total so the reader knows how much was not
  shown. One example rarely settles whether a column holds genuine extra
  decimals or float64 representation error, which is the question a reader
  actually has, and finding the rest previously meant leaving the tool.
- A value in those messages is shown as the double actually holds it, not in
  scientific notation. A value whose shortest form reads `8115022364.865` is
  stored as `8115022364.864999771`, and seeing both is what separates
  representation error from a third decimal that is really in the source. The
  message names the step it is not a multiple of, `0.001`, rather than a power
  of ten, and no longer quotes an empty display string for a pure numeric
  symbol that never had one.
- Each phase reports its own duration. The conversion's timing used to arrive
  only as the last `--progress` line, so `--progress 0` left it as the one
  phase that never accounted for itself while the quality gate always did, and
  a slow run could not be attributed without re-running it. `converted N/M
  rows` is now the running count `--progress` governs, and `conversion finished
  in T: N rows` is printed whatever it is set to, matching the gate's line.
- The final line says `overall`. It reports the whole run -- conversion, gate,
  and the rest -- but sat next to a verb about writing, so its figure read as
  the time taken to write the file. On a wide file the gate alone can be the
  larger half of it.

## [2.1.0] - 2026-08-25

The quality gate stops being the slow, silent, uninterruptible part of a wide
conversion. Everything here follows from running 2.0.0 on the SAP extract that
prompted it: the gate is now the default, and on a 20.6M-row, 213-column file
it ran for about twenty minutes at 11% CPU, printing nothing and ignoring
Ctrl-C.

### Added

- The quality gate reads the output back in parallel, splitting it across
  workers by row group the same way decoding is split by chunk. It was entirely
  single-threaded, which on a wide file left the machine idle for minutes at
  the end of every run: on a 213-column, 1M-row fixture the `full` gate takes
  60.9s single-threaded and 9.3s at eight workers, cutting the whole run from
  4.7x the conversion to 2.6x. Row groups are independent, and both the metrics
  and the fingerprint merge in any order -- which is what already let parallel
  decoding validate without reordering -- so the verdict does not depend on the
  worker count. Each worker opens its own handle, for the same reason the
  decode workers do.
- The quality gate reports progress on the `--progress` cadence, which is on by
  default. It previously printed nothing until it finished, so a run that was
  working looked like one that had hung.

- Ctrl-C and `SIGTERM` now shut down gracefully and report themselves as what
  they are. A cancelled run stops at the next chunk boundary, drains what is in
  flight, removes the temporary output and exits with a new code `7`.

  It previously exited `4`, `input error`, with `wrote 234725 rows but the
  header declares 1000000` -- a stopped run has written fewer rows than the
  header declares, which is exactly what a truncated input looks like, so
  pressing Ctrl-C told the user their QVD was corrupt. A cancelled quality gate
  likewise reported a gate failure, as though the output had not matched its
  input, when nobody had finished looking.

  A temporary output that cannot be deleted is now reported and named, instead
  of the failure being discarded and a partial Parquet file left sitting beside
  the real one. The delete is retried briefly first: Windows refuses to remove
  a file while any handle is open, and a virus scanner or the search indexer
  routinely holds one for a moment on a file just written, so the first attempt
  can fail on a file that is about to be perfectly deletable.

  In batch mode the files not yet started are recorded as cancelled too, rather
  than carrying a bare `context.Canceled` that mapped to the input-error code
  and reported unattempted files as unreadable ones. A cancelled batch
  outranks any individual file's verdict in the summary, since the rest were
  never tried.

  The signal handler is written against an explicit channel rather than
  `signal.NotifyContext`, whose stop function cancels the context as well as
  unregistering the handler: a goroutine waiting on `Done` cannot tell a real
  signal from the deferred cleanup of a successful run, and announced a
  cancellation on 37 of 40 successful conversions.

  The quality gate also honours cancellation at all now: it ran on
  `context.Background()`, so Ctrl-C during the read-back did nothing whatsoever
  -- on a wide file, minutes of a signal being ignored. And because
  `signal.NotifyContext` keeps swallowing signals once it has fired, a second
  Ctrl-C did nothing either. The first signal now restores the default handler
  and says so, so an impatient second one stops the process outright.

## [2.0.0] - 2026-08-25

Defaults re-chosen for wide files. A 213-column, 20.6M-row SAP extract was the
case that prompted it: it converted at 23k rows/s while using 16% of a 16-core
Xeon and 36 GB of resident memory. None of the changes below alter the data a
QVD converts to. Two of them are why this is a major release rather than a
minor one:

- `--batch-rows` no longer sets the Parquet row group size, so it has changed
  meaning. If you passed it to control row groups, pass `--row-group-rows`
  instead; if you passed it to control memory, it still does that and now
  defaults to sizing itself.
- `--quality-gate` defaults to `full`, so a conversion whose output does not
  match its input now exits 6 where it previously exited 0. The output was
  already wrong in those cases; the gate is what is new. It also makes a
  default run several times slower -- roughly 4.7x on a wide file, brought down
  to 2.6x in 2.1.0 -- so a scheduled job should either budget for it or name a
  cheaper mode.

### Changed

- `--workers=0` now resolves to one decode worker per two CPUs, with a floor of
  two, instead of one per CPU. Decoding itself scales close to linearly, but it
  is only half the pipeline -- the Parquet writer is a single goroutine -- and
  every worker costs its share of in-flight Arrow memory, which on a wide file
  is roughly `workers * batch-rows * columns * 16 bytes` and dominates resident
  size. A 213-column file on a 16-core machine held around 9.8 GB at one worker
  per CPU against 7.3 GB at four. Half the CPUs keeps about two thirds of the
  decode throughput at half the batches in flight; on a hyper-threaded machine,
  where `runtime.NumCPU()` counts threads, it works out to roughly one worker
  per physical core. This changes only how much of the machine a conversion
  uses, not what any file converts to, so it stays inside the compatibility
  promise above. Pass `--workers` explicitly to override it in either
  direction.
- `--file-workers` divides the same automatic budget, so batch mode and
  single-file mode now agree on how much of the machine to use.

- `--quality-gate` now defaults to `full` instead of `none`. A conversion
  nobody checked is not a conversion anybody can trust, and `full` is the only
  mode that fingerprints values, so it is the only one that catches a value
  which survived the type policy but not the round trip. Two consequences to
  plan for. It is not free, and it costs in two places: the gate reads the whole
  output back and digests every cell, single-threaded in this release, and,
  because `full` is the only mode that fingerprints values, each decode worker
  also digests every value inside the conversion itself. Changing only the mode
  on a 213-column fixture, conversion ran at 106k rows/s under `none`, 114k
  under `numeric` and 62k under `full`, and the whole run took roughly 4.7x as
  long under `full`. Name `basic` or `numeric` when throughput matters: neither
  carries the inline cost. And a conversion that previously exited 0 can now exit 6, because
  the file is checked where it previously was not -- a run that starts failing
  under this default was already producing that output, the gate is only now
  reporting it. Validation still reads the temporary file before the final
  rename, so a failed gate never leaves a final-looking output behind.

- `--batch-rows` and the Parquet row group size are no longer the same number.
  The two were one setting, which made them impossible to tune apart: a batch
  is held per worker and again in the queue to the writer, so it costs about
  `rows * columns * 16 bytes` and wants to shrink on a wide file, while the row
  group is what a reader scans and a dictionary is built over, and shrinking it
  inflates the output. Lowering `--batch-rows` to save memory tripled the file
  on a 213-column fixture, to 486 MiB. Row group size moves to a new
  `--row-group-rows`, still 65536, so row groups hold the same number of rows
  as before.

  What a row group *holds* is a separate matter, and is not changed by this: a
  row group has been filled from whichever chunks finish while it is open ever
  since decoding became parallel, so on a sorted input its statistics already
  covered a wider range than its rows, by a factor that varies from run to run.
  Measured on a 500k-row fixture keyed by an ascending integer, the spans
  covered by the row groups summed to 3.8x and 3.1x the row count on two runs
  under the old coupling, and 2.7x under the new default -- against 1.0x at
  `--workers=1` in both. This is now stated in the README rather than left
  implied by "row order is not preserved".
- `--batch-rows` now defaults to `0`, meaning a row count sized from the file's
  width to hold about 2M cells, between 4096 and 65536 rows. A narrow file
  still batches 65536 rows; a 213-column file batches ~9.4k, so in-flight
  memory stays put instead of growing with width. On that 213-column, 1M-row
  fixture, peak resident size fell from 7.3 GB to 1.8 GB at four workers. The
  throughput effect is the larger one: at a fixed 65536-row batch, going from
  four workers to sixteen bought 10% (52.2k to 57.7k rows/s), because each
  worker carried a batch of `65536 * 213` cells and the machine spent its time
  moving memory; sized by cells the same step goes 54.7k to 95.1k rows/s. The
  batch size was what stopped the workers from scaling. An explicit
  `--batch-rows` is honoured unchanged.

- Row order is now preserved. Decoding still runs in parallel, but records
  reach the Parquet writer in chunk order instead of completion order, so the
  output holds the QVD's rows in their original order whatever the worker
  count. This replaces the previous "row order is not preserved" caveat.

  The reason to want it is row groups. A row group's per-column min/max only
  bound the rows it holds if those rows are contiguous in the source, and under
  completion order they were not: on a 500k-row fixture keyed by an ascending
  integer, the spans covered by the row groups summed to 3.8x and 3.1x the row
  count on two runs, so an engine skipping row groups over a sorted key could
  rule out far fewer than the data allowed. It is now 1.0x, and stable across
  runs.

  A chunk that finishes ahead of its predecessors waits in a reorder buffer,
  bounded by a feeder window of two chunks per worker: without that window a
  slow first chunk would let the other workers pile every remaining chunk into
  memory behind it, and a writer that simply waited for the missing chunk
  instead would fill the results channel and deadlock the slow chunk when it
  finally handed its record over. Measured on a 213-column fixture, ordering
  costs no throughput and slightly less memory, the window being tighter than
  what was previously in flight.

### Fixed

- `BenchmarkDecode` measured four workers in every case above four. The
  fixture is four chunks at the default batch size and `WorkerCount` clamps to
  the chunk count, so the worker-scaling table in the README was reporting a
  plateau that was the clamp rather than the code. The benchmark now uses a
  batch small enough to keep every worker fed, names the automatic case
  `workers=default` instead of `workers=numcpu`, measures one per CPU
  separately, and reports the worker count each case actually ran.

- Decode workers no longer serialize their reads on Windows. Every worker read
  its chunks through the one `*os.File` opened for the input, and Windows -
  unlike Unix `pread`, which needs no lock - implements `ReadAt` by taking that
  descriptor's read and write locks and moving its shared file pointer, so all
  workers queued on a single mutex for every chunk. Each worker now opens its
  own handle, which is a separate kernel file object with its own pointer; the
  handle is reopened from the path rather than duplicated, since a duplicated
  handle shares the very pointer that lock exists to protect. A worker falls
  back to the shared handle if the path cannot be reopened or no longer names
  the same file, so a replaced input is never decoded unvalidated. Unix
  behaviour is unchanged.

## [1.0.1] - 2026-08-23

### Fixed

- A zero-padded number is no longer read as the number. SAP stores its keys as
  fixed-width character codes -- `BELNR` is `CHAR(10)`, so document 100000001
  is written `0100000001` -- and the display string was being compared to the
  numeric side as a number, which made the padding look like formatting. The
  column was typed `int64` and the padding dropped, leaving a key that no
  longer joins back to the source system. A column whose values are integers
  stored beside their own zero-padded digits is now written as `utf8`, as one
  column rather than a number with a `__text` sidecar. Padded decimals and
  dates are unaffected, being formatting and dates rather than codes, and so is
  an explicit `--dual`: the rule is inference, so it fills in for `--dual=auto`
  only and never overrides a side named on the command line.

## [1.0.0] - 2026-08-23

### Changed

- Declared stable: the CLI surface and the conversion defaults are now covered
  by the compatibility promise above, having been validated against the Java
  reference reader, against QlikView- and Qlik Sense-written files, and against
  an independent third-party reader.
- An untyped column whose display strings render its serial as a date or
  timestamp is now read as one in two cases it previously missed, so the value
  arrives typed instead of as a bare Qlik serial beside a `__text` sidecar.
  A symbol carrying no value no longer disqualifies the column: one
  empty-string placeholder among 2,991 dated duals was leaving the taxi files'
  `trip_end_timestamp` as `float64` plus `trip_end_timestamp__text`, though
  that placeholder is written as null either way. And the inference window now
  reaches back to 1600 rather than 1900, because historical series are real
  data -- the Stockholm temperature record starts in 1756. Across the twelve
  QVDs used for validation this takes `__text` sidecars from four to none.
  Labels are unaffected: a month number beside "Jan" still keeps both columns,
  since "Jan" renders nothing about the serial 1.
- A mixed text/number column no longer fails when the numeric symbols are
  integers and every symbol carries its own display string. The file already
  states the text for every value, so `utf8` reproduces all of them and nothing
  is invented. This is what the LEGO `parts.qvd` and `inventory_parts.qvd` hit:
  `part_num` holds `0901` beside the number 901, a code rather than a quantity,
  which reading as 901 would destroy. Of the twelve real QVDs used for
  validation, eleven now convert on the defaults where nine did; the twelfth is
  deliberately corrupt. Decimals beside text, and bare numbers carrying no text,
  still stop -- there a rendering would have to be chosen, and that is the
  caller's call.

## [0.5.0] - 2026-08-22

### Added

- Apache License 2.0. The project shipped without a licence file, which left
  its terms unstated.
- `--timezone=none` writes timestamps with no timezone (Parquet
  `isAdjustedToUTC=false`), preserving the QVD's naive wall clock so that every
  reader shows the same value regardless of where the file is converted or
  read. `naive` is accepted as a synonym.
- A zoned conversion now stamps `tz=UTC` rather than the zone it was given.
  `--timezone` states which zone the input wall clocks were recorded in, not how
  to label the output, and what gets stored once they are on the timeline is a
  UTC instant. Stamping the source zone made Arrow readers render the values
  back in it while engines reading the Parquet type alone -- which carries no
  name -- rendered the instant, so identical bytes showed two different times.
  `--timezone=none` is unaffected and still writes no zone.
- A zoned conversion now reports where it had to alter a wall clock. Twice a
  year a DST change skips an hour, so a reading in it does not exist and gets
  moved, or repeats an hour, so a reading in it has two instants and one is
  chosen. A QVD names no timezone, which makes both changes a consequence of
  the `--timezone` claim rather than of the data, so they are no longer silent.

### Removed

- `IMPLEMENTATION_PLAN.md`. It described the build that has since happened and
  had drifted from the code; the README and CHANGELOG carry what is still true.
  It remains in the git history.

### Changed

- **Breaking.** `--timezone` now defaults to `none`, so timestamps are written
  as the naive wall clock the QVD actually holds and nothing is converted
  unless asked. The previous default, `Local`, interpreted every reading in the
  converting machine's timezone, which made the output depend on where it ran:
  the same QVD produced three different instants on boxes in UTC, New York and
  Tokyo, none of them correct unless the data happened to come from that zone.
  Pass `--timezone=Local` to restore the old behaviour, or name the zone the
  data was recorded in to get true instants.
- `Options.TimezoneName` is now authoritative in `Validate`, which derives
  `Location` and `NaiveTimestamps` from it. Setting one of those fields alone no
  longer leaves the conversion disagreeing with the type.
- Timestamps are now `timestamp[us]` rather than `timestamp[ms]`. A Qlik serial
  resolves to about 0.63us at present-day dates, so milliseconds discarded real
  signal while still carrying the encoding noise that makes a stored `07:15:00`
  surface as `07:14:59.999999`. Rounding to the microsecond keeps the precision
  and removes the noise.

- Releases now ship Linux, Windows and macOS on `amd64` and `arm64` only. The
  32-bit, `arm`, `ppc64le`, `s390x`, `riscv64` and BSD targets were building
  and shipping without being asked for. Another target is a matter of listing
  it in `scripts/build-release.sh` and the two workflow matrices, which a test
  keeps identical.

- The release workflow builds its targets in parallel, one job each, instead of
  looping through them in a single job. Together with the trimmed list that
  takes a release from about twenty minutes to a couple.

## [0.4.0] - 2026-08-22

### Added

- `--out-dir` converts a whole folder: pass files or directories, and each
  `.qvd` becomes a `.parquet` of the same name. A failing file does not stop
  the run; every input is attempted, failures are listed at the end, and the
  exit code reports the most actionable one. `--recursive` descends into
  subdirectories. Two inputs that would produce the same output file are
  refused before anything is written, since `--force` would otherwise silently
  overwrite the first result.

- `--file-workers` converts several files at once and divides the decode
  workers between them, so the total stays near one per CPU. This is why folder
  conversion is built in rather than left to a shell loop: separate processes
  would each start `NumCPU` workers and oversubscribe the machine.

- `--log` writes JSON Lines, one record per file plus a summary, so a finished
  run can be queried with DuckDB or jq rather than read. Each record carries
  row and column counts, output size, elapsed time, throughput, and the quality
  gate's verdict. In batch mode `--schema-report` and `--quality-report` write
  one document per input, named after it.

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

- An empty string symbol is written as **null**, which is how Qlik treats it.
  The substitution is counted and reported. Pass `--empty-as-null=false` to
  keep `""` distinct from null.

- A NaN or infinite value in a date, timestamp, time, integer or decimal
  column is written as **null** rather than failing the conversion. Such a
  value is not something those types can hold, and nothing is lost by nulling
  it. The substitution is counted and reported on stderr and in
  `--schema-report`, so it is never silent. A *finite* value that simply does
  not fit is still an error, because nulling it would discard real data.
  `float64` columns keep NaN and infinity, which they can represent.

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
  abbreviations -- may appear alongside the digits, and a month or weekday name
  that contradicts the value is rejected, so `"Mon, 20 Nov 2010"` beside a
  Saturday keeps its text. The ISO 8601 `T` between date and time is treated as
  punctuation. A clock time uses a narrower list still, since a month or weekday
  name is not something a `time32` value can encode. So `"Due 11/20/2010"` and `"20 Jan 2010"`
  beside a November value both keep their text column. The list errs short: a
  word wrongly rejected costs a redundant column, whereas one wrongly accepted
  drops text that carried information.

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

[Unreleased]: https://github.com/ralforion/qvd2parquet/compare/v2.2.0...HEAD
[2.2.0]: https://github.com/ralforion/qvd2parquet/compare/v2.1.0...v2.2.0
[2.1.0]: https://github.com/ralforion/qvd2parquet/compare/v2.0.0...v2.1.0
[2.0.0]: https://github.com/ralforion/qvd2parquet/compare/v1.0.1...v2.0.0
[1.0.1]: https://github.com/ralforion/qvd2parquet/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/ralforion/qvd2parquet/compare/v0.5.0...v1.0.0
[0.5.0]: https://github.com/ralforion/qvd2parquet/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/ralforion/qvd2parquet/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/ralforion/qvd2parquet/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/ralforion/qvd2parquet/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/ralforion/qvd2parquet/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/ralforion/qvd2parquet/releases/tag/v0.1.0
