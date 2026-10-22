package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cristianadrielbraun/gofer/internal/auth"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

func contactSelectionFixture(t *testing.T) (*Handler, map[string]string) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "gofer.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Write().Exec(`INSERT INTO users (id, username, username_normalized) VALUES ('owner', 'owner', 'owner'), ('stranger', 'stranger', 'stranger')`); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, c := range []struct{ user, name, email string }{
		{"owner", "Ines Park", "ines@example.com"},
		{"owner", "Oren Vale", ""},
		{"owner", "Tamsin Ray", "tamsin@example.com"},
		{"stranger", "Wren Hollis", "wren@example.com"},
	} {
		saved, err := db.SaveContact(t.Context(), c.user, models.Contact{Name: c.name, Email: c.email})
		if err != nil {
			t.Fatalf("SaveContact(%s) error = %v", c.name, err)
		}
		ids[c.name] = saved.ID
	}
	return &Handler{db: db}, ids
}

func contactSelectionRequest(method, target, user string, form url.Values) *http.Request {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	return r.WithContext(auth.ContextWithUser(r.Context(), &auth.User{ID: user}))
}

func TestContactIDsListTheFilteredContactsOfTheUser(t *testing.T) {
	h, _ := contactSelectionFixture(t)
	list := func(target string) []contactSelectionItem {
		t.Helper()
		w := httptest.NewRecorder()
		h.handleContactIDs(w, contactSelectionRequest(http.MethodGet, target, "owner", nil))
		var result struct {
			Contacts  []contactSelectionItem `json:"contacts"`
			Truncated bool                   `json:"truncated"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Truncated {
			t.Fatalf("GET %s = %d %s", target, w.Code, w.Body.String())
		}
		return result.Contacts
	}
	all := list("/api/contacts/ids?sort_by=name&sort_order=asc")
	var names []string
	for _, contact := range all {
		names = append(names, contact.Name)
	}
	if strings.Join(names, ",") != "Ines Park,Oren Vale,Tamsin Ray" {
		t.Fatalf("contacts = %v, want the owner's three by name", names)
	}
	if all[0].Email != "ines@example.com" || all[1].Email != "" {
		t.Fatalf("contacts = %#v, want each with its address", all)
	}
	if found := list("/api/contacts/ids?q=tamsin"); len(found) != 1 || found[0].Name != "Tamsin Ray" {
		t.Fatalf("search = %#v, want only Tamsin Ray", found)
	}
}

func TestExportOfSelectedContactsLeavesOutOtherUsersContacts(t *testing.T) {
	h, ids := contactSelectionFixture(t)
	export := func(user string, picked ...string) *httptest.ResponseRecorder {
		form := url.Values{}
		for _, name := range picked {
			form.Add("id", ids[name])
		}
		w := httptest.NewRecorder()
		h.handleExportSelectedContacts(w, contactSelectionRequest(http.MethodPost, "/api/contacts/export", user, form))
		return w
	}
	w := export("owner", "Ines Park", "Tamsin Ray", "Wren Hollis")
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("export = %d %v", w.Code, w.Header())
	}
	if !strings.Contains(body, "Ines Park") || !strings.Contains(body, "Tamsin Ray") || strings.Contains(body, "Wren Hollis") || strings.Contains(body, "Oren Vale") {
		t.Fatalf("export = %q, want only the owner's two picked contacts", body)
	}
	if w := export("owner", "Wren Hollis"); w.Code != http.StatusNotFound {
		t.Fatalf("exporting only another user's contact = %d, want 404", w.Code)
	}
	if w := export("owner"); w.Code != http.StatusBadRequest {
		t.Fatalf("exporting nothing = %d, want 400", w.Code)
	}
}

func TestBulkDeleteRemovesOnlyTheUsersPickedContacts(t *testing.T) {
	h, ids := contactSelectionFixture(t)
	form := url.Values{}
	for _, name := range []string{"Ines Park", "Tamsin Ray", "Wren Hollis"} {
		form.Add("id", ids[name])
	}
	w := httptest.NewRecorder()
	h.handleDeleteContacts(w, contactSelectionRequest(http.MethodPost, "/api/contacts/bulk-delete", "owner", form))
	var result contactBulkDeleteResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
		t.Fatalf("bulk delete = %d %s", w.Code, w.Body.String())
	}
	// Another user's contact is no different from one already gone.
	if result.Deleted != 3 || len(result.Failed) != 0 {
		t.Fatalf("result = %#v, want 3 done and no failures", result)
	}
	exists := func(user, name string) bool {
		t.Helper()
		contact, err := h.db.GetContact(t.Context(), user, ids[name])
		if err != nil {
			t.Fatalf("GetContact(%s) error = %v", name, err)
		}
		return contact != nil
	}
	if exists("owner", "Ines Park") || exists("owner", "Tamsin Ray") {
		t.Fatal("a picked contact is still there")
	}
	if !exists("owner", "Oren Vale") {
		t.Fatal("a contact that wasn't picked was deleted")
	}
	if !exists("stranger", "Wren Hollis") {
		t.Fatal("another user's contact was deleted")
	}

	tooMany := url.Values{}
	for i := 0; i <= contactBulkDeleteLimit; i++ {
		tooMany.Add("id", fmt.Sprintf("contact-%d", i))
	}
	w = httptest.NewRecorder()
	h.handleDeleteContacts(w, contactSelectionRequest(http.MethodPost, "/api/contacts/bulk-delete", "owner", tooMany))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("over the batch limit = %d, want 400", w.Code)
	}
}
