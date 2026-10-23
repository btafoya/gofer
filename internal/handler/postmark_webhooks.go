package handler

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/quotedprintable"
	"net/http"
	netmail "net/mail"
	"strings"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/google/uuid"
)

// Postmark inbound and outbound-event webhooks. The routes carry the account
// ID and a per-account secret that must match the stored one. Responses keep Postmark's retry rules:
// 200 accepted, 500 retryable, 413 too large, 404 for everything that must stay invisible, including a wrong secret.

const (
	postmarkInboundMaxBytes = 32 << 20 // Postmark accepts large messages; oversize stays dumpable in their dashboard.
	postmarkEventsMaxBytes  = 1 << 20
)

type postmarkInbound struct {
	FromName, From, OriginalRecipient, Subject, MessageID, Date string
	MailboxHash, Tag, TextBody, HtmlBody, StrippedTextReply     string
	RawEmail                                                    string
	FromFull                                                    postmarkAddr
	ToFull, CcFull                                              []postmarkAddr
	Headers                                                     []struct{ Name, Value string }
	Attachments                                                 []struct {
		Name, Content, ContentType, ContentID string
	}
}

type postmarkAddr struct{ Email, Name string }

func (p *postmarkInbound) header(name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// decodePostmarkRaw accepts RawEmail as plain RFC822 or base64 (the encoding
// is undocumented; ported from proven Postmark client code).
func decodePostmarkRaw(raw string) ([]byte, bool) {
	source := []byte(raw)
	if postmarkIsMessage(source) {
		return source, true
	}
	if decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(raw), "")); err == nil && postmarkIsMessage(decoded) {
		return decoded, true
	}
	return nil, false
}

func postmarkIsMessage(value []byte) bool {
	parsed, err := netmail.ReadMessage(bytes.NewReader(value))
	return err == nil && len(parsed.Header) > 0
}

func postmarkNoCRLF(value string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(value)
}

// postmarkMIME synthesizes a minimal MIME message from the webhook's parsed
// fields when RawEmail is unusable. Boundaries derive from the message ID.
func postmarkMIME(p *postmarkInbound) []byte {
	var b bytes.Buffer
	header := func(key, value string) {
		if value = postmarkNoCRLF(strings.TrimSpace(value)); value != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", key, value)
		}
	}
	addressList := func(addrs []postmarkAddr) string {
		out := make([]string, 0, len(addrs))
		for _, a := range addrs {
			if a.Email != "" {
				out = append(out, (&netmail.Address{Name: a.Name, Address: a.Email}).String())
			}
		}
		return strings.Join(out, ", ")
	}
	messageID := p.internetMessageID()
	header("Date", p.dateHeader())
	header("From", addressList([]postmarkAddr{p.fromAddr()}))
	header("To", addressList(p.ToFull))
	header("Cc", addressList(p.CcFull))
	header("Reply-To", p.header("Reply-To"))
	header("Subject", mime.QEncoding.Encode("utf-8", p.Subject))
	for _, h := range p.Headers {
		name := strings.ToLower(strings.TrimSpace(h.Name))
		switch {
		case name == "", h.Value == "":
		case strings.HasPrefix(name, "content-") || name == "mime-version":
		case name == "date" || name == "from" || name == "to" || name == "cc" || name == "subject" ||
			name == "message-id" || name == "in-reply-to" || name == "references":
		default:
			header(h.Name, h.Value)
		}
	}
	if messageID != "" {
		header("Message-ID", messageID)
	}
	if reply := p.header("In-Reply-To"); reply != "" {
		header("In-Reply-To", reply)
	}
	if refs := p.header("References"); refs != "" {
		header("References", refs)
	}
	header("MIME-Version", "1.0")

	body := func(w *bytes.Buffer, contentType, text string) {
		fmt.Fprintf(w, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", contentType)
		q := quotedprintable.NewWriter(w)
		_, _ = q.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")))
		_ = q.Close()
		w.WriteString("\r\n")
	}
	alternative := p.internetMessageID()
	mixed := "=_postmark_mixed_" + alternative
	altBoundary := "=_postmark_alt_" + alternative
	writeText := func(w *bytes.Buffer) {
		if p.HtmlBody == "" {
			body(w, "text/plain", p.TextBody)
			return
		}
		fmt.Fprintf(w, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", altBoundary)
		for _, part := range [][2]string{{"text/plain", p.TextBody}, {"text/html", p.HtmlBody}} {
			fmt.Fprintf(w, "--%s\r\n", altBoundary)
			body(w, part[0], part[1])
		}
		fmt.Fprintf(w, "--%s--\r\n", altBoundary)
	}
	if len(p.Attachments) == 0 {
		writeText(&b)
		return b.Bytes()
	}
	fmt.Fprintf(&b, "Content-Type: multipart/mixed; boundary=\"%s\"\r\n\r\n--%s\r\n", mixed, mixed)
	writeText(&b)
	for _, a := range p.Attachments {
		contentType := a.ContentType
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		name := a.Name
		if name == "" {
			name = "attachment"
		}
		contentID := strings.Trim(a.ContentID, "<>")
		disposition := "attachment"
		if contentID != "" {
			disposition = "inline"
		}
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s\r\nContent-Disposition: %s\r\nContent-Transfer-Encoding: base64\r\n", mixed,
			mime.FormatMediaType(contentType, map[string]string{"name": name}), mime.FormatMediaType(disposition, map[string]string{"filename": name}))
		if contentID != "" {
			fmt.Fprintf(&b, "Content-ID: <%s>\r\n", contentID)
		}
		b.WriteString("\r\n")
		// The webhook's Content field is already base64; wrap it for transport.
		encoded := strings.Join(strings.Fields(a.Content), "")
		for len(encoded) > 76 {
			b.WriteString(encoded[:76] + "\r\n")
			encoded = encoded[76:]
		}
		b.WriteString(encoded + "\r\n")
	}
	fmt.Fprintf(&b, "--%s--\r\n", mixed)
	return b.Bytes()
}

func (p *postmarkInbound) fromAddr() postmarkAddr {
	if p.FromFull.Email != "" {
		return p.FromFull
	}
	name, _ := netmail.ParseAddress(postmarkNoCRLF(p.From))
	if name == nil {
		return postmarkAddr{}
	}
	return postmarkAddr{Email: name.Address, Name: name.Name}
}

func (p *postmarkInbound) internetMessageID() string {
	if value := strings.TrimSpace(p.header("Message-ID")); value != "" {
		id := mailmessage.ParseMessageIDs(value)
		if len(id) > 0 {
			return value
		}
		return "<" + strings.Trim(value, "<>") + ">"
	}
	// Postmark's own ID is a bare UUID; Gofer's identity needs an "@".
	value := strings.Trim(strings.TrimSpace(p.MessageID), "<>")
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "@") {
		value += "@postmark.gofer"
	}
	return "<" + value + ">"
}

func (p *postmarkInbound) dateHeader() string {
	if _, err := netmail.ParseDate(p.Date); err == nil {
		return p.Date
	}
	return time.Now().UTC().Format(time.RFC1123Z)
}

// postmarkSecretMatches compares the path secret with the account's stored one.
// Postmark does not sign webhook bodies; the 256-bit secret in the URL is the
// credential (HTTPS only), compared in constant time.
func postmarkSecretMatches(stored, presented string) bool {
	return stored != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(presented)) == 1
}

func (h *Handler) postmarkEvents() *mail.EventBus {
	if h.userIMAP != nil {
		return h.userIMAP.Events()
	}
	if h.syncer != nil {
		return h.syncer.Events()
	}
	return nil
}

// postmarkLease resolves a webhook's account without a session. Shared mode
// keeps one database; routed mode goes through the central directory.
func (h *Handler) postmarkLease(ctx context.Context, accountID string, fn func(db *storage.DB, accounts *config.AccountStore, owner string) error) error {
	if h.userStorage != nil && h.userAccounts != nil {
		return h.userAccounts.WithAccount(ctx, accountID, func(accounts *config.AccountStore, db *storage.DB, owner string) error {
			return fn(db, accounts, owner)
		})
	}
	var owner string
	err := h.db.Read().QueryRowContext(ctx, `SELECT user_id FROM accounts WHERE id = ? AND COALESCE(is_deleting, 0) = 0`, accountID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return storage.ErrAccountRoute
	}
	if err != nil {
		return err
	}
	return fn(h.db, h.accountStore, owner)
}

func (h *Handler) handlePostmarkInbound(w http.ResponseWriter, r *http.Request) {
	accountID, secret := r.PathValue("accountID"), r.PathValue("secret")
	if accountID == "" || secret == "" {
		http.NotFound(w, r)
		return
	}
	if h.blobStore == nil {
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, postmarkInboundMaxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "webhook payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unreadable webhook payload", http.StatusBadRequest)
		return
	}
	var verified bool
	if err := h.postmarkLease(r.Context(), accountID, func(db *storage.DB, accounts *config.AccountStore, owner string) error {
		token, err := accounts.DecryptPostmarkSecret(r.Context(), accountID)
		if err != nil {
			return err
		}
		verified = postmarkSecretMatches(token, secret)
		return nil
	}); err != nil || !verified {
		if err != nil && !errors.Is(err, storage.ErrAccountRoute) {
			log.Printf("postmark webhook %s: %v", accountID, postmarkWebhookSanitize(err))
		}
		http.NotFound(w, r)
		return
	}
	var payload postmarkInbound
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "unparsable webhook payload", http.StatusBadRequest)
		return
	}
	if payload.FromFull.Email == "" && payload.From == "" {
		http.Error(w, "webhook payload has no sender", http.StatusBadRequest)
		return
	}
	if err := h.storePostmarkInbound(r.Context(), accountID, payload); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("postmark inbound %s: %v", accountID, postmarkWebhookSanitize(err))
		http.Error(w, "storage failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

type postmarkWebhookEvents struct {
	RecordType string
	MessageID  string
	Type       string
	Subject    string
	Email      string
	BouncedAt  string
	Metadata   map[string]any
}

func (h *Handler) handlePostmarkEvents(w http.ResponseWriter, r *http.Request) {
	accountID, secret := r.PathValue("accountID"), r.PathValue("secret")
	if accountID == "" || secret == "" {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, postmarkEventsMaxBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "webhook payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "unreadable webhook payload", http.StatusBadRequest)
		return
	}
	var verified bool
	if err := h.postmarkLease(r.Context(), accountID, func(db *storage.DB, accounts *config.AccountStore, owner string) error {
		token, err := accounts.DecryptPostmarkSecret(r.Context(), accountID)
		if err != nil {
			return err
		}
		verified = postmarkSecretMatches(token, secret)
		return nil
	}); err != nil || !verified {
		if err != nil && !errors.Is(err, storage.ErrAccountRoute) {
			log.Printf("postmark events %s: %v", accountID, postmarkWebhookSanitize(err))
		}
		http.NotFound(w, r)
		return
	}
	var payload postmarkWebhookEvents
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "unparsable webhook payload", http.StatusBadRequest)
		return
	}
	h.recordPostmarkEvent(accountID, &payload)
	w.WriteHeader(http.StatusOK)
}

// recordPostmarkEvent maps an outbound event onto the queue. Hard failures
// mark the send; everything else stays informational.
func (h *Handler) recordPostmarkEvent(accountID string, payload *postmarkWebhookEvents) {
	owner := ""
	if err := h.postmarkLease(context.Background(), accountID, func(db *storage.DB, accounts *config.AccountStore, leaseOwner string) error {
		owner = leaseOwner
		return nil
	}); err != nil {
		log.Printf("postmark event %s: %v", accountID, postmarkWebhookSanitize(err))
		return
	}
	b := h.postmarkEvents()
	if b == nil {
		return
	}
	switch strings.ToLower(payload.RecordType) {
	case "bounce":
		sendID, _ := payload.Metadata["gofer_send_id"].(string)
		sendID = strings.TrimSpace(sendID)
		description := strings.TrimSpace(payload.Type)
		if payload.Subject != "" {
			description += ": " + postmarkNoCRLF(payload.Subject)
		}
		var marked bool
		if sendID != "" && isPostmarkHardBounce(payload.Type) {
			err := h.postmarkLease(context.Background(), accountID, func(db *storage.DB, accounts *config.AccountStore, owner string) error {
				return db.MarkOutgoingSendBounced(context.Background(), sendID, sanitizeOutgoingErrorText("bounce "+description))
			})
			marked = err == nil
			if err != nil {
				log.Printf("postmark bounce %s: %v", accountID, postmarkWebhookSanitize(err))
			}
		}
		status := "bounced"
		if marked {
			status = "failed"
		}
		b.Publish(mail.Event{Type: mail.EventSendResult, UserID: owner, AccountID: accountID, Status: status, Error: sanitizeOutgoingErrorText(description), Payload: map[string]any{"send_id": sendID, "bounce_type": payload.Type}})
	case "delivery":
		sendID, _ := payload.Metadata["gofer_send_id"].(string)
		b.Publish(mail.Event{Type: mail.EventSendResult, UserID: owner, AccountID: accountID, Status: "delivered", Payload: map[string]any{"send_id": strings.TrimSpace(sendID), "postmark_message_id": payload.MessageID}})
	default:
		// Unknown record types are logged, not dropped: the webhook stays quiet.
		log.Printf("postmark event %s: unhandled record type %q", accountID, payload.RecordType)
	}
}

// isPostmarkHardBounce reports whether a bounce type transitions the send
// rather than staying informational. Transient/soft bounces belong to the
// relay's own retry story.
func isPostmarkHardBounce(bounceType string) bool {
	switch strings.ToLower(strings.TrimSpace(bounceType)) {
	case "hardbounce", "spamcomplaint", "bademailaddress", "inactiverecipient", "templaterenderingfailed", "mailontimeout":
		return true
	default:
		return false
	}
}

// storePostmarkInbound materializes one webhook message into the account's
// inbox. RawEmail wins for fidelity; synthesized MIME is the fallback. The
// internet message ID makes Postmark's retries idempotent.
func (h *Handler) storePostmarkInbound(ctx context.Context, accountID string, payload postmarkInbound) error {
	raw, ok := decodePostmarkRaw(payload.RawEmail)
	if !ok {
		raw = postmarkMIME(&payload)
	}
	messageID := payload.internetMessageID()
	if ok {
		// The stored message's own header is its identity, as for any parsed mail.
		if parsed, err := netmail.ReadMessage(bytes.NewReader(raw)); err == nil {
			if ids := mailmessage.ParseMessageIDs(parsed.Header.Get("Message-ID")); len(ids) > 0 {
				messageID = "<" + strings.Trim(ids[0], "<>") + ">"
			}
		}
	}
	if messageID == "" {
		// Postmark always supplies one; the fallback keeps malformed payloads
		// storable instead of retry-looping.
		messageID = "<postmark-" + uuid.NewString() + "@gofer.local>"
	}
	var err error
	if err = h.postmarkLease(ctx, accountID, func(db *storage.DB, accounts *config.AccountStore, owner string) error {
		folder, _, err := db.GetFolderIDByRole(ctx, accountID, "inbox")
		if err != nil {
			return err
		}
		if folder == "" {
			if err := storage.EnsurePostmarkFolders(ctx, db, accountID); err != nil {
				return err
			}
			if folder, _, err = db.GetFolderIDByRole(ctx, accountID, "inbox"); err != nil || folder == "" {
				return fmt.Errorf("postmark inbox folder is unavailable")
			}
		}
		sent := time.Now().UTC()
		if date, parseErr := netmail.ParseDate(payload.Date); parseErr == nil {
			sent = date
		}
		sender := payload.fromAddr()
		to, cc := postmarkRecipients(payload.ToFull), postmarkRecipients(payload.CcFull)
		if len(to) == 0 && strings.TrimSpace(payload.OriginalRecipient) != "" {
			to = append(to, storage.Recipient{Email: strings.ToLower(strings.TrimSpace(payload.OriginalRecipient))})
		}
		inReplyTo := ""
		if ids := mailmessage.ParseMessageIDs(payload.header("In-Reply-To")); len(ids) > 0 {
			inReplyTo = ids[0]
		}
		references := payload.header("References")
		sync := []storage.SyncMessage{{
			AccountID: accountID, FolderID: folder, MessageID: messageID, InReplyTo: inReplyTo, References: references,
			Subject: payload.Subject, FromName: sender.Name, FromEmail: sender.Email,
			DateSent: sent, DateSentFallback: true, Snippet: mailmessage.GenerateSnippet(payload.TextBody, []byte(payload.HtmlBody)),
			IsRead: false, ToRecipients: to, CCRecipients: cc, SizeBytes: int64(len(raw)),
		}}
		if err := db.UpsertSyncMessages(ctx, sync); err != nil {
			return err
		}
		localID, err := db.GetMessageLocalIDByInternetIDInternal(ctx, accountID, messageID)
		if err != nil || localID == 0 {
			if err == nil {
				err = fmt.Errorf("postmark message row is missing after upsert")
			}
			return err
		}
		if textPath, htmlPath, err := db.GetMessageBodyPaths(ctx, localID); err == nil && (textPath.Valid || htmlPath.Valid) {
			// Replayed webhook: the message is already materialized.
			return nil
		}
		release, err := h.blobStore.PinUserFiles(ctx, owner)
		if err != nil {
			return err
		}
		defer release()
		candidate, err := h.blobStore.NewMessageVersion()
		if err != nil {
			return err
		}
		committed := false
		defer func() {
			if !committed {
				_ = candidate.DeleteMessage(accountID, localID)
			}
		}()
		parsed, err := mailmessage.ParseMessage(ctx, bytes.NewReader(raw), candidate, accountID, localID)
		if err != nil {
			return err
		}
		if parsed.ParseError != nil {
			return parsed.ParseError
		}
		text, html := "", ""
		if parsed.TextBody != "" {
			text, err = candidate.StoreBodyText(ctx, accountID, localID, []byte(parsed.TextBody))
			if err != nil {
				return err
			}
		}
		if len(parsed.HTMLBody) > 0 {
			html, err = candidate.StoreBodyHTML(ctx, accountID, localID, mailmessage.SanitizeHTML(parsed.HTMLBody))
			if err != nil {
				return err
			}
		}
		if err := db.PublishPostmarkMessageBody(ctx, localID, accountID, messageID, parsed, text, html); err != nil {
			return err
		}
		committed = true
		if bus := h.postmarkEvents(); bus != nil {
			bus.Publish(mail.Event{Type: mail.EventMutation, UserID: owner, AccountID: accountID, FolderID: folder})
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func postmarkRecipients(addrs []postmarkAddr) []storage.Recipient {
	out := make([]storage.Recipient, 0, len(addrs))
	for _, a := range addrs {
		if a.Email != "" {
			out = append(out, storage.Recipient{Name: a.Name, Email: a.Email})
		}
	}
	return out
}

// postmarkWebhookSanitize keeps account tokens and secrets out of logs the
// same way other provider errors are redacted.
func postmarkWebhookSanitize(err error) error {
	text := sanitizeOutgoingErrorText(err.Error())
	return errors.New(text)
}
