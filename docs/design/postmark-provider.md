# Design: Postmark provider (inbound + outbound)

Requirements: `docs/requirements/postmark-provider.md` (all decisions locked).
This document names existing code as anchors; nothing speculative is introduced.

## Reuse anchors (the design is mostly these)

| Piece | Anchor | Postmark delta |
|---|---|---|
| Account provisioning | `AccountStore.prepareAccountCreation` branch (config/accounts.go:662) | new `postmark` branch |
| Credential storage | `encrypted_password` / `encrypted_smtp_password` columns + password kinds | reuse; no new credential columns |
| Relay send | existing SMTP branch of `userMailDeliveryScope.send` (handler/user_mail_delivery.go) | prepend `X-PM-*` headers to `send.MIMEData` |
| Sent copy | `cacheSent` (local blob parse pipeline, user_mail_delivery.go:336) | used alone (no IMAP APPEND, no provider cache) |
| Local message ingest | `blobStore.NewMessageVersion` + `mailmessage.ParseMessage` + `UpsertSyncMessages` | inbound path runs the same pipeline |
| Replay idempotency | `UpsertSyncMessages` unique on `(account_id, internet_message_id)` | re-POSTed webhooks upsert, never duplicate |
| Events | `h.userIMAP.Events().Publish(mail.Event{...})` | inbound publishes `EventMutation`; bounces publish send-result events |
| Public webhook surface | Outlook precedent: `httpguard.OutlookWebhookPath` + `isPublicPath` (auth/middleware.go:216); `outlookSubscriptions` base URL + key | same pattern, per-account path |
| Webhook registration client | `github.com/mrz1836/postmark` (Servers.Edit for `InboundHookUrl`, Webhooks API for bounce/delivery) | call at activation |
| Folders | folder-role lookup (`GetFolderIDByRole`) | bootstrap local inbox/drafts/sent/archive/trash rows at activation |

## Data model

`accounts` gains three columns (user-local store migration):

- `postmark_message_stream TEXT NOT NULL DEFAULT ''` — `X-PM-Message-Stream`; empty = Postmark default outbound stream.
- `postmark_inbound_address TEXT NOT NULL DEFAULT ''` — the account's inbound address/forwarding identity, shown in UI.
- `postmark_webhook_secret TEXT NOT NULL DEFAULT ''` — stored encrypted via the existing password machinery (new `passwordKindPostmarkSecret`).

Relay credentials reuse existing columns: `smtp_host`=`smtp.postmarkapp.com`, `smtp_port`=587, `smtp_tls_mode`=starttls, `username`+`encrypted_password`(kind IMAP)=server token, `smtp_username`+`encrypted_smtp_password`=server token. No new tables; the outbound queue schema is untouched (`gofer_send_id` rides in the `X-PM-Metadata-gofer-send-id` header, returned as webhook `Metadata`).

## Component changes (per package)

### internal/providers
`ProviderPostmark = "postmark"`; `IsPostmark(p)` helper alongside the JMAP helper. Worker dispatch treats postmark as **no-sync**: no IMAP/JMAP watcher, no provider sync — only the delivery queue plus webhook ingress.

### internal/config (accounts)
- `prepareAccountCreation` postmark branch: require server token + inbound address; clear IMAP fields; set relay SMTP fields as above; `AuthMethod` stays `plain`.
- Encryption: token into both password kinds; webhook secret into `passwordKindPostmarkSecret`; `DecryptPostmarkSecret` following `DecryptSmtpPassword` shape.
- On activation hook: ensure local folder set (reuse import-time folder creation), then register webhook configs via `mrz1836/postmark` against the instance webhook base URL (same setting pattern as `OutlookWebhookURL`).

### internal/httpguard + internal/auth
`PostmarkWebhookPathBase = "/webhooks/postmark/"` added to `isPublicPath`. Route shapes:

- `POST /webhooks/postmark/inbound/{accountID}/{secret}` — inbound mail.
- `POST /webhooks/postmark/events/{accountID}/{secret}` — bounce/delivery.

`{secret}` is 32 bytes of random base64url per account; an unknown path/secret returns **404**, indistinguishable from a nonexistent route (matches the decided per-account-secret-URL model).

### internal/handler/postmark_webhooks.go
Inbound webhook, all responses must keep Postmark's retry semantics correct:

1. `http.MaxBytesReader` 32 MiB (Postmark allows large mail; oversize → 413, message remains dumpable in Postmark's dashboard).
2. Parse bounded inbound JSON into the `pmInbound`-shaped struct (ports `RawEmail`, `Headers`, attachments with `ContentID` — proven shapes from `gopostmarkinbound` hook.go).
3. Compare the path secret with the account's stored secret (constant time). **Implementation note:** Postmark does not sign webhook bodies, so the original HMAC step was dropped; the 256-bit path secret over HTTPS is the credential. Mismatch → 404.
4. Lease `WithAccountForUser`; reject if account is not active → 404.
5. Mail materialization:
   - `decodeRaw(RawEmail)` plain RFC822 or base64 (port of `gopostmarkinbound` decodeRaw) — normal path.
   - Fallback: synthesize a minimal MIME from parsed fields (headers array minus owned headers, text/html, attachments base64, deterministic boundary), the `buildMIME` pattern.
   - Pipeline: `PinUserFiles` → `NewMessageVersion` → `ParseMessage` → `StoreBodyText/HTML` → `UpsertSyncMessages` into the inbox folder (`RemoteUID=0`, `IsRead=false`) → publish `EventMutation`.
6. Dedup replay → 200 without re-notifying (upsert is a no-op change set; skip event publish when the row was not new).
7. Responses: stored (new or dup) → 200; transient storage/lease error → 500 (Postmark retries ≤10); account deleted → 404; wrong secret → 404.
8. `Received`/`SpamAssassin` headers: stored like any header via raw MIME; no special casing. `MailboxHash`, `Tag`, `StrippedTextReply` ignored (no requirement uses them; YAGNI).

Events webhook: same auth; `RecordType` dispatch:
- `Bounce`: map `Metadata.gofer_send_id` → queue record. Definitive types (HardBounce, SpamComplaint, and their TypeCode families) → mark the send failed via a small storage method `MarkOutgoingSendBounced(ctx, sendID, sanitizedError)` + publish send-result event (status `failed`, bounce description). SoftBounce/Transient → event only (state untouched; Postmark/SMTP retries own these). Unknown/unmapped → log + 200, never dropped silently (requirement).
- `Delivery`: event only (the relay's SMTP 250 already recorded the send). No state change.
- Webhook body bound at 1 MiB; unknown `RecordType` → 200 (Postmark may add types).

### internal/handler/user_mail_delivery.go
- `send()`: postmark branch alongside imap — same snapshot/transport checks, but before `SendRawMessageWithTiming` prepend `X-PM-Message-Stream: <cfg stream>\r\n`, `X-PM-KeepID: true\r\n`, `X-PM-Metadata-gofer-send-id: <send.ID>\r\n` to `MIMEData`. Everything else (claims, retry jitter, ambiguous recovery) untouched.
- `sentCopy()`: postmark → `cacheSent` only (local parse pipeline; no APPEND).
- `draft()`: postmark → no-op `(false, nil)`. Drafts live only in the local drafts folder; the compose path queues no provider draft operation (compose save already skips provider sync when no `drafts` session is wired — verify at implementation; the guard is a provider check, not an interface change).
- Credentials: `Run`'s preflight gains the postmark case — decrypt token once into both `password` and `smtpPassword` (mirrors the imap/plain branch, no SMTP-password fallback ceremony needed).

### internal/config (webhook base URL)
One setting alongside the Outlook one; `PostmarkWebhookURL()` follows `OutlookWebhookURL`'s "follows the setting" test shape. The UI derives and displays all three per-account URLs from it; auto-registration uses the same value. No public-URL auto-detection.

### internal/views (account forms)
Create dialog gains provider=Postmark (server token, display name, email address, inbound address, optional stream). After activation the edit dialog shows the three webhook URLs and a "re-register webhooks" action (idempotent re-run of the registration step). No other UI changes.

## Sequences

**Inbound**: Postmark MX → webhook POST → public path → secret path match → HMAC verify → lease → decode/synthesize MIME → parse pipeline → upsert inbox row (idempotent) → `EventMutation` → SSE → UI refresh. 200.

**Outbound**: compose → send queue (scheduled or immediate) → claim → postmark branch → relay with `X-PM-*` headers → 250 → sent record + local Sent cache → `EventSendResult sent`.

**Bounce**: Postmark → events webhook → HMAC → `Metadata.gofer_send_id` lookup → `MarkOutgoingSendBounced` + event. Unknown type → log only. All webhook failures keep the send record truthful (relay-only: failures come only from SMTP or bounce webhooks).

## Failure matrix (Postmark bounce → Gofer)

| Postmark | Gofer treatment |
|---|---|
| HardBounce / definitive families (unknown user, spam complaint, etc.) | send → failed, error text = Postmark description, event published |
| SoftBounce / Transient | event only |
| Delivery | event only |
| Open/Click/SubscriptionChange | not delivered to the events endpoint (registration registers only Bounce + Delivery hooks) |
| Metadata missing send id | log + 200 |

## Security review

- Public routes authenticate twice: path secret (constant-time) + body HMAC. Path secret alone defeats enumeration; HMAC defeats path leakage (e.g. referer leaks, proxy logs).
- 404 for unknown path/secret — no account-state disclosure.
- A wrong secret is a 404, indistinguishable from an unknown route. No body signature exists to check.
- No tokens/secrets logged; relay errors are sanitized like other outgoing error text (`sanitizeOutgoingErrorText`).
- CSRF not applicable (public endpoint, path-secret-authenticated, no cookies read).
- Webhook handler holds the account lease only for local store work; postmark has no provider calls to block it.

## Test plan

- config: postmark `prepareAccountCreation` branch (ports cleared, relay fields set, token encrypted both kinds, secret kind round-trip).
- delivery: header-prepend content; sent-copy runs `cacheSent` only; `draft()` no-op; credential preflight decrypt.
- webhooks (httptest): happy-path ingest (raw + synthesized fallback), replayed webhook → 200 + no duplicate row + no duplicate event, bad secret → 404, oversize → 413, transient error → 500, events mapping (HardBounce→failed, Delivery→no-op, unknown id→logged), Postmark `RecordType` dispatch.
- registration: `mrz1836/postmark` call sites with a stub transport (Servers.Edit + Webhooks.Create); re-registration idempotency.
- folders: postmark activation creates the fixed local set exactly once.

## Implementation stages (for IMPLEMENTATION_PLAN.md)

1. **Provider plumbing** — providers constant, accounts branch/columns/migration, credential kinds, folder bootstrap. Verify: create+read postmark account row.
2. **Outbound relay** — queue branch, header prepend, sent-copy local-only, draft no-op, credential preflight. Verify: send through a stub SMTP client, headers present end-to-end.
3. **Inbound webhook** — public path, handler, decode/synthesize, ingest pipeline, idempotency, signature. Verify: httptest webhook → inbox row + event; replay → no dup.
4. **Outbound events** — events endpoint, bounce state mapping, `MarkOutgoingSendBounced`. Verify: HardBounce webhook flips queued/sent record + event fires.
5. **Registration + UI** — webhook base URL setting, auto-register at activation, re-register action, create/edit form fields + URL display. Verify: form round-trip; registration calls a stub Postmark API.