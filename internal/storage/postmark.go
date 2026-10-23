package storage

import (
	"context"
	"database/sql"
	"errors"

	mailmessage "github.com/cristianadrielbraun/gofer/internal/mail/message"
)

// migrateV123ToV124 adds Postmark account settings. They stay empty for every
// other provider: the message stream and inbound address are identity fields,
// and the webhook secret is a credential encrypted with the account's kind.
func migrateV123ToV124(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_version`).Scan(&version); err != nil {
		return err
	}
	if version >= 124 {
		return nil
	}
	exists, err := tableExistsTx(tx, "accounts")
	if err != nil {
		return err
	}
	if exists {
		for _, column := range []string{"postmark_message_stream", "postmark_inbound_address"} {
			present, err := columnExistsTx(tx, "accounts", column)
			if err != nil {
				return err
			}
			if !present {
				if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
					return err
				}
			}
		}
		present, err := columnExistsTx(tx, "accounts", "postmark_webhook_secret")
		if err != nil {
			return err
		}
		if !present {
			if _, err := tx.Exec(`ALTER TABLE accounts ADD COLUMN postmark_webhook_secret BLOB`); err != nil {
				return err
			}
		}
	}
	if err := markSchemaVersion(tx, 124); err != nil {
		return err
	}
	return tx.Commit()
}

// EnsurePostmarkFolders creates the fixed local folder set for a Postmark
// account (inbox, drafts, sent, archive, trash). UpsertFolders is idempotent,
// so re-running the bootstrap is harmless.
func EnsurePostmarkFolders(ctx context.Context, db *DB, accountID string) error {
	folders := []UpsertFolderInput{
		{ID: FolderIDForIdentity(accountID, "postmark", "inbox"), Name: "Inbox", Role: "inbox", SortOrder: 0},
		{ID: FolderIDForIdentity(accountID, "postmark", "drafts"), Name: "Drafts", Role: "drafts", SortOrder: 3},
		{ID: FolderIDForIdentity(accountID, "postmark", "sent"), Name: "Sent", Role: "sent", SortOrder: 2},
		{ID: FolderIDForIdentity(accountID, "postmark", "archive"), Name: "Archive", Role: "archive", SortOrder: 4},
		{ID: FolderIDForIdentity(accountID, "postmark", "trash"), Name: "Trash", Role: "trash", SortOrder: 6},
	}
	for i := range folders {
		folders[i].AccountID = accountID
		folders[i].Selectable = true
	}
	return db.UpsertFolders(ctx, folders)
}

// MarkOutgoingSendBounced records a definitive bounced delivery for a send the
// relay already accepted. The record stays so retry-with-confirmation and the
// operations view keep their history.
func (db *DB) MarkOutgoingSendBounced(ctx context.Context, id, errText string) error {
	result, err := db.Write().ExecContext(ctx, `
		UPDATE outgoing_sends
		SET status = ?, last_error = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ? AND status IN (?, ?) AND COALESCE(last_error, '') = ''`,
		OutgoingSendFailed, errText, id, OutgoingSendSent, OutgoingSendAmbiguous)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		// Nothing to mark: unknown id, a pending retry, or an error already recorded.
		return nil
	}
	return nil
}

// PublishPostmarkMessageBody publishes the parsed bodies, raw source and
// attachments of one webhook-ingested message. It is PublishSentBody without
// the Sent-role lock: a Postmark account stores everything locally, and the
// message's folder state row is the only identity that matters.
func (db *DB) PublishPostmarkMessageBody(ctx context.Context, id int64, accountID, messageID string, p *mailmessage.ParsedMessage, textPath, htmlPath string) error {
	if p == nil {
		return errors.New("postmark body is required")
	}
	tx, err := db.Write().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE messages SET body_text_path=?,body_html_path=?,body_html_original_path='',raw_path=?,has_attachments=?,size_bytes=CASE WHEN ?>0 THEN ? ELSE size_bytes END,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_id=? AND internet_message_id=? AND EXISTS(SELECT 1 FROM message_folder_state s WHERE s.message_id=? AND s.is_deleted=0)`, textPath, htmlPath, p.RawPath, len(p.Attachments) > 0, p.Size, p.Size, id, accountID, messageID, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return errors.New("postmark message identity changed")
	}
	for _, table := range []string{"attachments", "message_recipients"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE message_id=?`, id); err != nil {
			return err
		}
	}
	for _, a := range p.Attachments {
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(message_id,filename,content_type,size_bytes,content_id,inline,storage_path) VALUES(?,?,?,?,?,?,?)`, id, a.Filename, a.ContentType, a.Size, a.ContentID, a.Inline, a.BlobPath); err != nil {
			return err
		}
	}
	for _, list := range []struct {
		kind       string
		recipients []mailmessage.Recipient
	}{{"to", p.To}, {"cc", p.CC}} {
		for _, r := range list.recipients {
			if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients(message_id,kind,name,email) VALUES(?,?,?,?)`, id, list.kind, r.Name, r.Email); err != nil {
				return err
			}
		}
	}
	if err := db.reindexMessagesSearchTx(ctx, tx, []int64{id}); err != nil {
		return err
	}
	return tx.Commit()
}
