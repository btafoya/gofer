package handler

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/mail"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	postmark "github.com/mrz1836/postmark"
)

// Relay-only sending: the queue's SMTP branch delivers through
// smtp.postmarkapp.com with three Postmark headers prepended. The same send
// identity round-trips back on bounce and delivery webhooks via Metadata.

// postmarkRelayMIME prepends the stream and identity headers Postmark's relay
// consumes. X-PM-KeepID preserves Gofer's own Message-ID so threading
// survives; X-PM-Metadata-gofer-send-id identifies the queue record in bounces.
func postmarkRelayMIME(cfg *models.AccountConfig, send storage.OutgoingSend) []byte {
	var b bytes.Buffer
	if stream := postmarkNoCRLF(strings.TrimSpace(cfg.PostmarkMessageStream)); stream != "" {
		b.WriteString("X-PM-Message-Stream: " + stream + "\r\n")
	}
	b.WriteString("X-PM-KeepID: true\r\n")
	b.WriteString("X-PM-Metadata-gofer-send-id: " + postmarkNoCRLF(send.ID) + "\r\n")
	b.Write(send.MIMEData)
	return b.Bytes()
}

// postmarkRelaySent caches a delivered send into the local Sent folder and
// finishes the sent-copy queue record without any remote APPEND: a Postmark
// account has no remote mailbox at all.
func (s *userMailDeliveryScope) postmarkRelaySent(ctx context.Context, send storage.OutgoingSend) error {
	var folder string
	if err := s.call(ctx, func(db *storage.DB) error {
		var err error
		folder, _, err = db.GetFolderIDByRole(ctx, s.id, "sent")
		return err
	}); err != nil {
		return err
	}
	var snap outgoingMessageSnapshot
	if err := json.Unmarshal(send.MessageJSON, &snap); err != nil {
		return err
	}
	if folder == "" || snap.MessageID == "" {
		return errors.New("postmark Sent copy identity is unavailable")
	}
	if err := s.cacheSent(ctx, send, folder, snap); err != nil {
		return err
	}
	if err := s.call(ctx, func(db *storage.DB) error { return db.CompleteSentCopy(ctx, send.ID, 0, 0) }); err != nil {
		return err
	}
	s.runner.h.userIMAP.Events().Publish(mail.Event{Type: mail.EventMutation, UserID: s.owner, AccountID: s.id, FolderID: folder})
	return nil
}

// postmarkSentCopy is the routed sent-copy runner's entry for Postmark.
func (s *userMailDeliveryScope) postmarkSentCopy(ctx context.Context, send storage.OutgoingSend) error {
	send.SentCopyStatus = storage.SentCopyCopying
	return s.postmarkRelaySent(ctx, send)
}

// postmarkAccountBootstrap runs once after a Postmark account is created or
// edited: it creates the fixed local folder set, then points the Postmark
// server at this instance's webhook URLs and registers the outbound event
// hooks. Failure is logged, never fatal for the account; the next Updated
// hook or a manual retry fixes it.
func (h *Handler) postmarkAccountBootstrap(ctx context.Context, accountID string) error {
	var err error
	if h.userStorage != nil && h.userAccounts != nil {
		err = h.userAccounts.WithAccountForUser(ctx, "", accountID, func(accounts *config.AccountStore, db *storage.DB) error {
			return storage.EnsurePostmarkFolders(ctx, db, accountID)
		})
	} else if h.db != nil {
		err = storage.EnsurePostmarkFolders(ctx, h.db, accountID)
	}
	if err != nil {
		return err
	}
	if h.HTTPConfig == nil || h.HTTPConfig.BaseURL == "" {
		log.Printf("postmark account %s: webhook base URL unknown; set GOFER_BASE_URL and retry webhook registration", accountID)
		return nil
	}
	if err := h.registerPostmarkWebhooks(ctx, accountID); err != nil {
		log.Printf("postmark account %s: webhook registration failed: %v", accountID, postmarkWebhookSanitize(err))
		return nil
	}
	return nil
}

// registerPostmarkWebhooks makes the Postmark server call Gofer: the server's
// InboundHookUrl plus one outbound hooks webhook on the account's stream.
// Idempotent — existing webhooks for the same URLs are left alone.
func (h *Handler) registerPostmarkWebhooks(ctx context.Context, accountID string) error {
	lease := func(fn func(accounts *config.AccountStore, db *storage.DB) error) error {
		if h.userStorage != nil && h.userAccounts != nil {
			return h.userAccounts.WithAccount(ctx, accountID, func(accounts *config.AccountStore, db *storage.DB, _ string) error {
				return fn(accounts, db)
			})
		}
		return fn(h.accountStore, h.db)
	}
	var cfg *models.AccountConfig
	var secret, token string
	if err := lease(func(accounts *config.AccountStore, db *storage.DB) error {
		var cfgErr error
		if cfg, cfgErr = accounts.GetConfig(ctx, accountID); cfgErr != nil {
			return cfgErr
		}
		if token, cfgErr = accounts.DecryptPassword(ctx, accountID); cfgErr != nil {
			return cfgErr
		}
		secret, cfgErr = accounts.DecryptPostmarkSecret(ctx, accountID)
		return cfgErr
	}); err != nil {
		return err
	}
	if token == "" || secret == "" {
		return errors.New("postmark account credentials are unavailable")
	}
	base := strings.TrimSuffix(h.HTTPConfig.BaseURL, "/")
	inboundURL := base + "/webhooks/postmark/inbound/" + accountID + "/" + secret
	eventsURL := base + "/webhooks/postmark/events/" + accountID + "/" + secret
	client, err := postmarkAccountClient(token)
	if err != nil {
		return err
	}
	return postmarkRegisterHooks(ctx, client, inboundURL, eventsURL, cfg.PostmarkMessageStream)
}

// postmarkAccountClient builds the Postmark API client. Registration and
// verification share the server token.
func postmarkAccountClient(token string) (*postmark.Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("postmark server token is missing")
	}
	return postmark.NewClient(token, token), nil
}

// postmarkRegisterHooks edits the server's inbound hook URL and ensures the
// outbound event webhook exists for the account's stream.
func postmarkRegisterHooks(ctx context.Context, client *postmark.Client, inboundURL, eventsURL, stream string) error {
	current, err := client.GetCurrentServer(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(current.InboundHookURL) != inboundURL {
		current.InboundHookURL = inboundURL
		if _, err := client.EditCurrentServer(ctx, current); err != nil {
			return err
		}
	}
	webhooks, err := client.ListWebhooks(ctx, stream)
	if err != nil {
		return err
	}
	for _, hook := range webhooks {
		if strings.TrimSpace(hook.URL) == eventsURL {
			return nil
		}
	}
	_, err = client.CreateWebhook(ctx, postmark.Webhook{
		URL:           eventsURL,
		MessageStream: stream,
		Triggers: postmark.WebhookTrigger{
			Bounce:        postmark.WebhookTriggerIncContent{Enabled: true},
			Delivery:      postmark.WebhookTriggerEnabled{Enabled: true},
			SpamComplaint: postmark.WebhookTriggerIncContent{Enabled: true},
		},
	})
	return err
}

// verifyPostmarkAccount checks a new account's server token before Gofer
// stores it. A wrong token fails immediately instead of at the first delivery.
func verifyPostmarkAccount(ctx context.Context, token string) error {
	client, err := postmarkAccountClient(token)
	if err != nil {
		return err
	}
	if _, err := client.GetCurrentServer(ctx); err != nil {
		return fmt.Errorf("postmark server token rejected: %w", postmarkWebhookSanitize(err))
	}
	return nil
}

// generatePostmarkWebhookSecret makes the per-account URL secret. 32 bytes of
// entropy, base64url so it is a single path segment.
func generatePostmarkWebhookSecret() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// RegisterPostmarkWebhooks mounts the per-account Postmark webhook routes. They
// are public paths (see auth.isPublicPath): each proves itself with the account
// ID and secret in the path plus the body signature.
func (h *Handler) RegisterPostmarkWebhooks(mux *http.ServeMux) {
	mux.HandleFunc("POST /webhooks/postmark/inbound/{accountID}/{secret}", h.handlePostmarkInbound)
	mux.HandleFunc("POST /webhooks/postmark/events/{accountID}/{secret}", h.handlePostmarkEvents)
}
