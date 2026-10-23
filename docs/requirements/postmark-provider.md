# Requirements: Postmark provider (inbound + outbound)

Date: 2026-10-22 — discovery complete; design/spec decisions deferred to `/sc:design`.

## Goal

Add Postmark (postmarkapp.com) as a full mail provider in Gofer, alongside
`imap`, `jmap`, `gmail`, and `outlook`: a `postmark` account can receive mail
via Postmark's inbound webhook and send mail via Postmark's SMTP relay.

## Decided (user confirmations)

1. **Role**: full mail provider — receiving and sending, per-account.
2. **Outbound path**: Postmark SMTP relay (`smtp.postmarkapp.com`, server token
   as username/password). Existing raw-MIME send path (smtpclient, envelope,
   retries, ambiguous states, scheduled send) is reused essentially unchanged;
   calendar-notification and calendar-reply sends keep working.
3. **Inbound path**: Postmark inbound webhook POST, routed to an account by a
   per-account secret webhook URL (account ID + secret in the path). The URL is
   pasted into the Postmark server's inbound webhook settings.
4. **Outbound event webhooks**: bounce and delivery events are in the first
   pass and update the existing outbound queue states/events. Postmark API
   access uses the `github.com/mrz1836/postmark` library (MIT, zero external
   runtime dependencies; fork of keighl/postmark, covers bounces, webhooks
   config, messages, servers, message streams).
5. **Inbound storage**: the Postmark inbound webhook carries `RawEmail` (full
   raw source, plain RFC822 or base64 — the encoding is undocumented; see
   `decodeRaw` in Brian's `gopostmarkinbound`). Gofer stores the raw source
   when usable and synthesizes MIME from parsed webhook fields (headers,
   bodies, attachments — attachments carry `ContentID` for inline images) only
   as a fallback. This mirrors `gopostmarkinbound`'s raw-or-`buildMIME`
   pattern. Decision confirms the earlier "synthesize MIME" choice with the
   raw source as the fidelity win.
6. **Webhook setup**: at account activation Gofer auto-registers the inbound,
   bounce, and delivery webhook configs on that Postmark server via the
   Webhooks API (`mrz1836/postmark`), pointing at the generated secret URLs.
7. **Folders**: fixed local set for `postmark` accounts — inbox, drafts, sent,
   archive, trash. No remote folders.
8. **Stream selection**: relay sends prepend `X-PM-Message-Stream: <stream>`
   with a per-account setting (default `outbound`), so a Postmark server can
   split streams without code changes (pattern from `gopostmarkinbound`'s
   `relayMail`).
9. **Relay-only sending**: no JSON-API send fallback. Relay failures route
   into the existing queue retry/ambiguous machinery like any SMTP failure.
   (User explicitly declined the `submitViaAPI` fallback that
   `gopostmarkinbound` carries.) `X-PM-KeepID: true` is always sent so Postmark
   preserves Gofer's own Message-ID.

## Functional requirements

- **Account type**
  - New provider `postmark` with its own credentials and settings: Postmark
    server token (shared by SMTP relay and REST API), inbound address/domain,
    webhook signature secret, generated webhook URL.
  - `postmark` does not fit the existing required-fields rule
    (IMAP host / SMTP host / username / password); account create/edit needs
    its own form path and validation (precedent: the JMAP form path).
  - Server token stored encrypted like other credentials. Token should be
    verified against the Postmark API before an account is activated.
- **Sending**
  - Sends from a `postmark` account go through the Postmark SMTP relay, reusing
    the existing SMTP send branch (raw MIME, envelope, claim/retry/ambiguous
    flow, scheduled send, sent-copy caching, mail events). Relay sends prepend
    `X-PM-Message-Stream` (per-account setting, default `outbound`) and
    `X-PM-KeepID: true`. Relay-only: no JSON send fallback.
  - Failed-sent, retry, and event publication behavior matches the imap branch.
- **Receiving**
  - Inbound webhook endpoint(s), usable without a logged-in session
    (token/signature-authenticated), accepting Postmark's inbound JSON payload.
  - Each `postmark` account exposes its own secret webhook URL; a request for a
    missing/wrong secret is indistinguishable from an unknown route.
  - The 256-bit secret in the path is the credential (constant-time compare, HTTPS). Postmark does not sign webhook bodies, so no HMAC is checked.
  - Processing is idempotent: Postmark retries non-200 responses up to 10
    times; replaying the same message must not duplicate it.
  - Received mail lands in the account's inbox with bodies, attachments
    (including inline images via `ContentID`), headers (including threading
    references) and participates in existing mail events / SSE notification
    flow, threads, and search like other accounts. The raw `RawEmail` source
    is preserved when the webhook provides it; synthesized MIME is fallback.
  - `postmark` accounts are push-fed only: no polling/IDLE/push watchers.
- **Outbound events**
  - Bounce and delivery webhooks (per-account secret URLs, Postmark's outbound
    webhook settings) update existing outbound send records: durable
    hard-bounce marks the corresponding send failed; delivery confirms
    delivery. Events publish like existing send-result events.
  - Relay sends carry a custom `X-PM-Metadata-gofer-send-id` header; Postmark
    returns it in bounce/delivery webhook payloads as `Metadata`, which maps
    events to `OutgoingSend` records directly. Unmapped events are
    recorded/logged, never silently dropped.
- **Lifecycle**
  - Account deletion drains the account like other providers (no remote
    mailbox to clean up beyond what exists).
  - `POSTMARK_API_TEST`-style sandbox behavior applies only to the REST API,
    not the relay; test hooks must not produce delivered mail unintentionally.

## Non-functional requirements

- No new dependencies beyond `github.com/mrz1836/postmark`; it must stay in the
  existing vendoring/update flow and pass `go mod tidy`.
- Webhook handling bounded (payload size limits) and never blocks the sending
  queue; failures return statuses that steer Postmark's retry schedule.
- Credential and secret values never appear in logs or in rendered pages.
- Works in all three auth modes (open / personal protected / managed), webhook
  routes authenticated by secret+signature, not by session.

## User stories / acceptance criteria

- As a user I can add a `postmark` account by pasting my server token and
  inbound address; Gofer verifies the token, then shows me the webhook URLs to
  paste into Postmark (inbound + outbound event hooks).
- As a user I can send an email from the `postmark` account with attachments,
  inline images, scheduling, and calendar invites, and see it in Sent.
- As a user, mail sent to my Postmark inbound address/forwarding domain shows
  up in my Gofer inbox, threads correctly, and can be moved/trashed/exported
  like any other mail.
- As a user, when an outbound message bounces hard, the send status shows the
  failure and a bounce event in the account activity; delivery events confirm
  successful sends.
- As an operator, replaying/duplicating webhook POSTs (Postmark's retry
  behavior or an attacker probing) neither duplicates mail nor discloses data.

## Open questions (design-time verification only — no decisions pending)

1. Confirm `X-PM-Metadata-gofer-send-id` set on relay sends round-trips as
   `Metadata` in bounce/delivery payloads (documented for SMTP on
   postmarkapp.com/smtp; webhook example shows a `Metadata` field — verify with
   a live webhook test during design).
2. Confirm which outbound event types fire for relay-sent mail
   (`gopostmarkinbound` already consumes Delivered/Bounce events for its
   relay sends — strong evidence; enumerate the set Gofer reacts to).
3. Calendar MIME (METHOD:REQUEST) over the relay: `gopostmarkinbound` sends
   ordinary text/html+attachments; Gofer additionally sends calendar invites
   through relay — spot-check one invite end-to-end during design.

Reference implementation (patterns proven live, by Brian):
`/home/btafoya/projects/gopostmarkinbound` — `decodeRaw`/`pmInbound` (hook.go),
`relayMail` X-PM headers (submit.go), `buildMIME` deterministic rebuild
(mime.go), webhook basic-auth endpoint hardening (hookAuth).