# Local Files lifecycle

Files are owned by UserID and scoped to the currently authorized GroupID. APIKeyID records provenance. Pagination uses immutable `(created_at,id)` behind an owner-scoped public file ID cursor; expired or retired cursor metadata remains usable.

Reserve maximum bytes and user/global upload slots before parsing the first multipart part. Staging and tombstone resources keep their charges until physical removal is confirmed. Renew and publish compare database lease and fixed upload deadline; publish actual metadata and retention in one CAS. Long uploads recheck uncached key/user/group/IP/subscription authorization before becoming ready. Storage failures return503, explicit upload deadline408, slot capacity429, and lost lease409.

Published files expire immediately in read/list/reference admission, independently of worker timing. Retirement increments lifecycle version and fixes the read drain deadline once. Existing live read tokens may renew only to their database grant and fixed drain cap. Service readers conservatively anchor database remaining duration before SQL and close/release when canceled, expired or renewal fails even without another Read.

`remote_activity_possible` is monotonic write-ahead evidence. Managed implementations set it under the file lock before any external side effect. False permits local-only cleanup; true requires the real same-transaction lifecycle port. Nil, failed or malformed port results fail closed. All compound transactions lock file before binding/operation and read leases. Local cleanup uses independent token/version/lease CAS, removes bytes outside SQL, then confirms quota release; retry after unlink is idempotent. Original bytes metadata and permanent remote facts remain. Local cleanup never claims remote DELETE success or returns a fabricated public `deleted:true`.

Periodic work is bounded and advances past blocked IDs; staging crash recovery remains separate from published retirement. Disabling Files preserves metadata and the private volume. Actual managed-ledger and dual-SDK integration are separate acceptance gates from local HTTP/PG/store tests.
