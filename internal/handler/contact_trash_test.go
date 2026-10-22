package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/providers"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func postContactIDs(t *testing.T, h *Handler, handle http.HandlerFunc, target string, ids ...string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	for _, id := range ids {
		form.Add("id", id)
	}
	w := httptest.NewRecorder()
	handle(w, contactSelectionRequest(http.MethodPost, target, "owner", form))
	return w
}

func TestTrashedContactsComeBackOnRestoreAndListInTheTrash(t *testing.T) {
	h, ids := contactSelectionFixture(t)
	postContactIDs(t, h, h.handleTrashContacts, "/api/contacts/bulk-delete", ids["Ines Park"], ids["Tamsin Ray"])

	w := httptest.NewRecorder()
	h.handleContactIDs(w, contactSelectionRequest(http.MethodGet, "/api/contacts/ids?trash=1&sort_by=name&sort_order=asc", "owner", nil))
	var listed struct{ Contacts []contactSelectionItem }
	if json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Contacts) != 2 || listed.Contacts[0].Name != "Ines Park" {
		t.Fatalf("trash ids = %d %s, want the two trashed", w.Code, w.Body.String())
	}

	w = postContactIDs(t, h, h.handleRestoreContacts, "/api/contacts/trash/restore", ids["Ines Park"], ids["Wren Hollis"])
	var restored struct{ Restored int }
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &restored) != nil || restored.Restored != 1 {
		t.Fatalf("restore = %d %s, want 1 restored", w.Code, w.Body.String())
	}
	if contact, _ := h.db.GetContact(t.Context(), "owner", ids["Ines Park"]); contact == nil {
		t.Fatal("restored contact isn't back")
	}
	if trashed, _ := h.db.ContactInTrash(t.Context(), "owner", ids["Tamsin Ray"]); !trashed {
		t.Fatal("a contact not restored left the Trash")
	}
}

func TestSingleDeleteMovesTheContactToTheTrash(t *testing.T) {
	h, ids := contactSelectionFixture(t)
	r := contactSelectionRequest(http.MethodPost, "/api/contacts/"+ids["Oren Vale"]+"/delete", "owner", nil)
	r.SetPathValue("id", ids["Oren Vale"])
	w := httptest.NewRecorder()
	h.handleDeleteContact(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("delete = %d %s, want a redirect", w.Code, w.Body.String())
	}
	if trashed, err := h.db.ContactInTrash(t.Context(), "owner", ids["Oren Vale"]); err != nil || !trashed {
		t.Fatalf("in the Trash = %v, %v; want true", trashed, err)
	}
}

func TestATrashedContactCantBeEditedUntilRestored(t *testing.T) {
	h, ids := contactSelectionFixture(t)
	postContactIDs(t, h, h.handleTrashContacts, "/api/contacts/bulk-delete", ids["Ines Park"])
	form := url.Values{"name": {"Ines Park-Ode"}, "email": {"ines@example.com"}}
	w := httptest.NewRecorder()
	h.handleSaveContact(w, contactSelectionRequest(http.MethodPost, "/api/contacts?id="+ids["Ines Park"], "owner", form))
	if w.Code != http.StatusConflict {
		t.Fatalf("saving a trashed contact = %d %s, want 409", w.Code, w.Body.String())
	}
	if _, err := h.db.SaveContact(t.Context(), "owner", models.Contact{ID: ids["Ines Park"], Name: "Ines Park-Ode", Email: "ines@example.com"}); err == nil {
		t.Fatal("storage saved over a trashed contact")
	}
	contact, err := h.db.GetContactIncludingTrash(t.Context(), "owner", ids["Ines Park"])
	if err != nil || contact == nil || contact.Name != "Ines Park" {
		t.Fatalf("trashed contact = %#v, %v; want it unchanged", contact, err)
	}
}

func TestDeleteForeverOnlyTakesContactsInTheTrash(t *testing.T) {
	h, ids := contactSelectionFixture(t)
	postContactIDs(t, h, h.handleTrashContacts, "/api/contacts/bulk-delete", ids["Ines Park"])
	w := postContactIDs(t, h, h.handleDeleteContactsForever, "/api/contacts/trash/delete", ids["Ines Park"], ids["Tamsin Ray"], ids["Wren Hollis"])
	var result contactBulkDeleteResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("delete forever = %d %s", w.Code, w.Body.String())
	}
	// Another user's contact is no different from one already gone; one out
	// of the Trash stays.
	if result.Deleted != 2 || len(result.Failed) != 1 || result.Failed[0].ID != ids["Tamsin Ray"] {
		t.Fatalf("result = %#v, want 2 done and Tamsin Ray refused", result)
	}
	if contact, _ := h.db.GetContactIncludingTrash(t.Context(), "owner", ids["Ines Park"]); contact != nil {
		t.Fatal("contact deleted for good is still there")
	}
	if contact, _ := h.db.GetContact(t.Context(), "owner", ids["Tamsin Ray"]); contact == nil {
		t.Fatal("a contact out of the Trash was deleted")
	}
	if contact, _ := h.db.GetContact(t.Context(), "stranger", ids["Wren Hollis"]); contact == nil {
		t.Fatal("another user's contact was deleted")
	}

	single := func(name string) int {
		r := contactSelectionRequest(http.MethodPost, "/api/contacts/"+ids[name]+"/delete-forever", "owner", nil)
		r.SetPathValue("id", ids[name])
		w := httptest.NewRecorder()
		h.handleDeleteContactForever(w, r)
		return w.Code
	}
	if code := single("Tamsin Ray"); code != http.StatusConflict {
		t.Fatalf("deleting a contact out of the Trash for good = %d, want 409", code)
	}
	if code := single("Ines Park"); code != http.StatusNotFound {
		t.Fatalf("deleting a deleted contact again = %d, want 404", code)
	}
	postContactIDs(t, h, h.handleTrashContacts, "/api/contacts/bulk-delete", ids["Oren Vale"])
	if code := single("Oren Vale"); code != http.StatusSeeOther {
		t.Fatalf("deleting a trashed contact for good = %d, want a redirect", code)
	}

	tooMany := make([]string, contactBulkDeleteLimit+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("contact-%d", i)
	}
	if w := postContactIDs(t, h, h.handleDeleteContactsForever, "/api/contacts/trash/delete", tooMany...); w.Code != http.StatusBadRequest {
		t.Fatalf("over the batch limit = %d, want 400", w.Code)
	}
}

// A synced contact stays in its address book while in the Trash, and goes
// from it once deleted for good, by hand or after the retention period.
func TestTrashedSyncedContactLeavesItsAddressBookOnlyWhenDeletedForGood(t *testing.T) {
	allowLocalDAVServers(t)
	ctx := context.Background()
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		deleted = append(deleted, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	db, store := newCardDAVDeleteTestStore(t)
	if _, err := db.Write().Exec(`INSERT INTO accounts (id, user_id, email_address, display_name) VALUES ('acc', 'default', 'me@example.com', 'Me')`); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveContactSyncConfig(ctx, "default", "acc", models.ContactSyncConfig{
		AccountID: "acc", UserID: "default", Provider: providers.ProviderCardDAV, Enabled: true,
		AddressBookURL: server.URL + "/book/",
		AddressBooks:   []models.ContactAddressBook{{Name: "Personal", URL: server.URL + "/book/", Default: true}},
		Username:       "user",
	}, "pass"); err != nil {
		t.Fatal(err)
	}
	cfg, err := store.GetContactSyncConfig(ctx, "default", "acc")
	if err != nil {
		t.Fatal(err)
	}
	synced := func(name, email, file string) string {
		t.Helper()
		id, _, err := db.UpsertSyncedContact(ctx, "default", "acc", name, email)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.UpsertContactSource(ctx, storage.ContactSource{ContactID: id, UserID: "default", Provider: providers.ProviderCardDAV, AccountID: "acc",
			AddressBookID: cfg.AddressBooks[0].ID, RemoteID: server.URL + "/book/" + file, Etag: `"1"`}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	byHand := synced("Pia Strand", "pia@example.com", "pia.vcf")
	old := synced("Bo Kerr", "bo@example.com", "bo.vcf")
	recent := synced("Lu Fenn", "lu@example.com", "lu.vcf")
	h := &Handler{db: db, accountStore: store}

	if _, err := h.moveContactsToTrash(ctx, "default", []string{byHand, old, recent}); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 0 {
		t.Fatalf("address book deletes on trashing = %v, want none", deleted)
	}

	if err := h.deleteContactForever(ctx, "default", byHand); err != nil {
		t.Fatalf("deleteContactForever() error = %v", err)
	}
	if strings.Join(deleted, ",") != "/book/pia.vcf" {
		t.Fatalf("address book deletes = %v, want only pia", deleted)
	}

	if _, err := db.Write().Exec(`UPDATE contact_profiles SET trashed_at=? WHERE id=?`, time.Now().UTC().Add(-storage.ContactTrashRetention-time.Hour).Format("2006-01-02 15:04:05"), old); err != nil {
		t.Fatal(err)
	}
	h.purgeAllContactTrash(ctx)
	if strings.Join(deleted, ",") != "/book/pia.vcf,/book/bo.vcf" {
		t.Fatalf("address book deletes = %v, want bo purged and lu kept", deleted)
	}
	if contact, _ := db.GetContactIncludingTrash(ctx, "default", old); contact != nil {
		t.Fatal("purged contact is still there")
	}
	if trashed, _ := db.ContactInTrash(ctx, "default", recent); !trashed {
		t.Fatal("a contact trashed recently was purged")
	}
}
