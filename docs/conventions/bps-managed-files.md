# BPS managed file references

When durable Files is enabled, typed Responses `input_file.file_id` belongs to the local user/current-group namespace. Unknown or foreign IDs never fall back to a raw upstream reference. Only a native HTTP Responses API-key account with explicit `bps_files`, `bps-files/1`, control URL, service key and backend/subject/generation credentials is eligible. This does not infer support from an OpenAI-compatible base URL.

Resolve local ownership for the whole batch and hold read leases before external traffic. Select a capable account on first reference; any durable binding permanently pins its account and backend identity. Recheck current account eligibility and configuration around the concurrency slot and immediately before generation. Busy, retired, expired, changed identity and uncertain operations fail closed; no account switch or implicit retry may move a file.

Use the shared strict JSON profile and fixture package `internal/pkg/managedfiles`. The final normalized request is the admission template. Batch Admit performs native preflight without BPS activity, then each upload writes `remote_activity_possible` and `started` in one transaction before Ensure. Persist confirmed late copies independently of activation, and preserve unknown outcomes as uncertain. Only exact typed content positions are replaced. The full request controls, owner/group and immutable metadata participate in hashes.

Generation has one transport send. Trusted context owns all five private headers; public copies are stripped. Disable redirects, request replay bodies, idempotency retry hints, protocol repair and handler failover. Perform the final file lifecycle fence before that send. Retain response-drain and existing usage accounting behavior after the send. Proof credentials and admission/binding headers must be redacted from archives.

Chat/Messages file-reference shapes and all WebSocket file-bearing frames are unsupported until their own explicit adapter is implemented. Native Anthropic document sources and opaque tool argument strings keep their protocol meanings. Ordinary inline images retain their existing route; mixed managed-document/inline-image support remains constrained by the private gateway manifest.

Acceptance is official Python and JavaScript SDKs through the same main base URL, real auth/scheduling, gateway and local fake BPS, with real PostgreSQL state. Unit seams and a partial migration harness are development evidence only. Real BPS acceptance and recovery/deletion extensions remain separate gates.
