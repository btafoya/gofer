package config

import (
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func TestUserContactTrashStaysInTheOwnersStorage(t *testing.T) {
	system, _, accounts, owners := newServiceAccountFixture(t)
	for _, owner := range []string{"alice", "bob"} {
		seedOwnedPreview(t, accounts, owner, owners[owner].ID)
	}
	inTrash := func(owner string) bool {
		t.Helper()
		var trashed bool
		if err := accounts.WithUser(t.Context(), owner, func(_ *AccountStore, db *storage.DB) error {
			var err error
			trashed, err = db.ContactInTrash(t.Context(), owner, "preview-donor")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return trashed
	}
	if count, err := accounts.TrashContacts(t.Context(), "alice", []string{"preview-donor", "no-such-contact"}); err != nil || count != 1 {
		t.Fatalf("TrashContacts() = %d, %v; want 1", count, err)
	}
	if !inTrash("alice") || inTrash("bob") {
		t.Fatal("trash crossed owners")
	}
	if count, err := accounts.RestoreContacts(t.Context(), "alice", []string{"preview-donor"}); err != nil || count != 1 || inTrash("alice") {
		t.Fatalf("RestoreContacts() = %d, %v; want it back", count, err)
	}

	// Deleting a trashed contact for good runs the usual delete.
	if _, err := accounts.TrashContacts(t.Context(), "alice", []string{"preview-donor"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := accounts.SnapshotContactDelete(t.Context(), "alice", "preview-donor")
	if err != nil {
		t.Fatal(err)
	}
	next, err := accounts.AcknowledgeContactDeleteSource(t.Context(), snapshot, "preview-card")
	if err != nil {
		t.Fatal(err)
	}
	if id, err := accounts.FinishContactDelete(t.Context(), next); err != nil || id != "preview-donor" || inTrash("alice") {
		t.Fatalf("FinishContactDelete() = %q, %v; want it deleted and out of the Trash", id, err)
	}
	var count int
	if err := system.Read().QueryRow(`SELECT COUNT(*) FROM contact_activity_events`).Scan(&count); err != nil || count != 0 {
		t.Fatal("trash activity reached central storage", count, err)
	}
}
