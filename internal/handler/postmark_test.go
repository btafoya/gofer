package handler

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/config"
	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
	"github.com/cristianadrielbraun/gofer/internal/store"
)

func TestPostmarkSecretMatches(t *testing.T) {
	if !postmarkSecretMatches("s3cret", "s3cret") {
		t.Fatal("matching secret rejected")
	}
	if postmarkSecretMatches("s3cret", "other") || postmarkSecretMatches("", "") || postmarkSecretMatches("s3cret", "") {
		t.Fatal("wrong or empty secret accepted")
	}
}

func TestDecodePostmarkRawPlainAndBase64(t *testing.T) {
	raw := "From: a@example.com\r\nTo: b@example.com\r\nSubject: hi\r\n\r\nbody\r\n"
	if got, ok := decodePostmarkRaw(raw); !ok || string(got) != raw {
		t.Fatalf("plain raw: ok=%v", ok)
	}
	if got, ok := decodePostmarkRaw(base64.StdEncoding.EncodeToString([]byte(raw))); !ok || string(got) != raw {
		t.Fatalf("base64 raw: ok=%v", ok)
	}
	if _, ok := decodePostmarkRaw("not a message"); ok {
		t.Fatal("garbage accepted as a message")
	}
}

// Synthesized MIME must survive the real parse pipeline, inline image included.
func TestPostmarkMIMEFallbackKeepsInlineImage(t *testing.T) {
	p := &postmarkInbound{
		Subject: "Hello", MessageID: "abc@example.com", Date: "Mon, 02 Jan 2006 15:04:05 +0000",
		TextBody: "text", HtmlBody: "<p>html</p>",
		FromFull: postmarkAddr{Email: "a@example.com", Name: "A"},
		ToFull:   []postmarkAddr{{Email: "b@example.com"}},
	}
	p.Attachments = append(p.Attachments, struct{ Name, Content, ContentType, ContentID string }{
		"pic.png", base64.StdEncoding.EncodeToString([]byte("PNGDATA")), "image/png", "cid123"})
	raw := postmarkMIME(p)
	if !postmarkIsMessage(raw) {
		t.Fatalf("synthesized MIME is not a message:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("Content-ID: <cid123>")) || !bytes.Contains(raw, []byte("Message-ID: <abc@example.com>")) {
		t.Fatalf("missing Content-ID or Message-ID:\n%s", raw)
	}
	if got := mailmessage.ParseMessageIDs(string(raw)); len(got) == 0 {
		t.Fatal("no message id parsed")
	}
}

func TestPostmarkRelayMIMEHeaders(t *testing.T) {
	cfg := &models.AccountConfig{PostmarkMessageStream: "broadcast"}
	out := string(postmarkRelayMIME(cfg, storage.OutgoingSend{ID: "send-1", MIMEData: []byte("Subject: x\r\n\r\nb")}))
	for _, want := range []string{"X-PM-Message-Stream: broadcast\r\n", "X-PM-KeepID: true\r\n", "X-PM-Metadata-gofer-send-id: send-1\r\n", "Subject: x"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	out = string(postmarkRelayMIME(&models.AccountConfig{}, storage.OutgoingSend{ID: "s", MIMEData: []byte("S: x\r\n\r\n")}))
	if strings.Contains(out, "X-PM-Message-Stream") {
		t.Fatal("empty stream must not emit a stream header")
	}
}

func TestIsPostmarkHardBounce(t *testing.T) {
	for _, hard := range []string{"HardBounce", "SpamComplaint", "BadEmailAddress"} {
		if !isPostmarkHardBounce(hard) {
			t.Fatalf("%s should be hard", hard)
		}
	}
	for _, soft := range []string{"SoftBounce", "Transient", "", "AutoResponder"} {
		if isPostmarkHardBounce(soft) {
			t.Fatalf("%s should not transition the send", soft)
		}
	}
}

func postmarkIngestFixture(t *testing.T) (*Handler, *storage.DB, string, string) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized, name) VALUES ('owner','owner','owner','Owner')`); err != nil {
		t.Fatal(err)
	}
	accounts, err := config.NewAccountStore(db, []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "path-secret-value"
	account, err := accounts.CreateAccount(t.Context(), "owner", &models.CreateAccountRequest{
		Provider: "postmark", EmailAddress: "me@example.com", DisplayName: "Me",
		Password: "server-token", PostmarkInboundAddress: "In@Example.com", PostmarkWebhookSecret: secret,
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	h := New(db, accounts, nil, store.NewBlobStore(filepath.Join(t.TempDir(), "blobs")), nil, "")
	return h, db, account.ID, secret
}

func postmarkPost(h *Handler, route, accountID, secret, body string) int {
	mux := http.NewServeMux()
	h.RegisterPostmarkWebhooks(mux)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/postmark/"+route+"/"+accountID+"/"+secret, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestPostmarkInboundWebhookStoresOnceAndRejectsWrongSecret(t *testing.T) {
	h, db, id, secret := postmarkIngestFixture(t)
	raw := "Message-ID: <hook-1@example.com>\r\nFrom: Alice <alice@example.com>\r\nTo: me@example.com\r\nSubject: Hello there\r\nDate: Mon, 02 Jan 2006 15:04:05 +0000\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhi from postmark\r\n"
	payload, _ := json.Marshal(map[string]any{
		"From": "alice@example.com", "FromFull": map[string]string{"Email": "alice@example.com", "Name": "Alice"},
		"ToFull": []map[string]string{{"Email": "me@example.com"}}, "Subject": "Hello there",
		"MessageID": "pm-uuid", "Date": "Mon, 02 Jan 2006 15:04:05 +0000", "RawEmail": raw,
	})

	if code := postmarkPost(h, "inbound", id, "wrong-secret", string(payload)); code != http.StatusNotFound {
		t.Fatalf("wrong secret = %d, want 404", code)
	}
	for i := 0; i < 2; i++ { // Postmark retries; a replay must not duplicate.
		if code := postmarkPost(h, "inbound", id, secret, string(payload)); code != http.StatusOK {
			t.Fatalf("delivery %d = %d, want 200", i, code)
		}
	}
	var count int
	var textPath sql.NullString
	if err := db.Read().QueryRow(`SELECT COUNT(*), MAX(body_text_path) FROM messages WHERE account_id=? AND internet_message_id='<hook-1@example.com>'`, id).Scan(&count, &textPath); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !textPath.Valid {
		t.Fatalf("rows = %d, body path valid = %v; want 1 row with a stored body", count, textPath.Valid)
	}
	var role string
	if err := db.Read().QueryRow(`SELECT f.role FROM message_folder_state s JOIN folders f ON f.id=s.folder_id JOIN messages m ON m.id=s.message_id WHERE m.account_id=?`, id).Scan(&role); err != nil || role != "inbox" {
		t.Fatalf("folder role = %q, %v; want inbox", role, err)
	}
}

func TestPostmarkEventsWebhookAcceptsUnknownRecordsAndRejectsWrongSecret(t *testing.T) {
	h, _, id, secret := postmarkIngestFixture(t)
	body := `{"RecordType":"Bounce","Type":"HardBounce","Metadata":{"gofer_send_id":"nope"}}`
	if code := postmarkPost(h, "events", id, "wrong", body); code != http.StatusNotFound {
		t.Fatalf("wrong secret = %d, want 404", code)
	}
	for _, b := range []string{body, `{"RecordType":"Delivery"}`, `{"RecordType":"SomethingNew"}`} {
		if code := postmarkPost(h, "events", id, secret, b); code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", b, code)
		}
	}
}
