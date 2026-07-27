# Test fixtures

Every file here is a real `Operation` from a Backrest oplog, re-marshalled through the same
`protojson` encoder the API uses, then scrubbed of identifying detail (hostnames, addresses,
repo GUIDs, plan names, filesystem paths). The *shape* is untouched, which is the point: these
fixtures exist to pin down four properties of proto-JSON that are easy to get wrong by reasoning
alone.

1. `operationBackup.lastStatus` is a **oneof**. A running backup carries `status`; the moment it
   finishes that key is gone and `summary` is there instead. Compare `backup_inprogress.json`
   with `backup_success.json`.
2. **Every int64 is a quoted string** - `"bytesDone": "800995270310"`, and `"id"` too. Doubles
   are not: `percentDone` and `totalDuration` are bare numbers.
3. **Zero values are omitted entirely.** `backup_success.json` has no `filesNew` or `dirsNew`
   because both were 0. Absent has to read as zero, not as malformed.
4. `status` is the **enum name**, not its number, and the numbers are non-contiguous anyway
   (`STATUS_ERROR` is 4, `STATUS_WARNING` is 7). Never map the integer.

`backup_error_nostatus.json` is the fifth case, and the one most likely to panic a decoder:
`operationBackup` is present but empty, because the backup died before restic emitted a single
status line.

`backup_warning_errors.json` is the one **synthetic** fixture. `BackupProgressError` entries are
in the proto but no real oplog on hand had any, so that file is hand-built to the proto's shape.
Treat it as a schema test, not as evidence of what a warning looks like in the field.
