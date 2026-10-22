package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/cristianadrielbraun/gofer/internal/config"
	"github.com/cristianadrielbraun/gofer/internal/models"
	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// contactSelectionLimit caps how many contacts "select all" and an export of
// the selection take at once.
const contactSelectionLimit = 10000

// contactBulkDeleteLimit caps one bulk delete request. Each synced contact is
// removed from its address books one by one, so the list sends a large
// selection in several requests.
const contactBulkDeleteLimit = 100

// postedContactIDs reads the distinct contact ids a form posts as "id", and
// refuses none or more than limit.
func postedContactIDs(w http.ResponseWriter, r *http.Request, limit int) ([]string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return nil, false
	}
	var ids []string
	seen := make(map[string]bool)
	for _, id := range r.PostForm["id"] {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > limit {
		http.Error(w, "choose some contacts, but not too many at once", http.StatusBadRequest)
		return nil, false
	}
	return ids, true
}

type contactSelectionItem struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// handleContactIDs lists every contact the list shows under the given
// filters, in list order, with its name and address, so the list can select
// all of them and not just the loaded ones, and write to them.
func (h *Handler) handleContactIDs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := h.userID(ctx)
	result := struct {
		Contacts  []contactSelectionItem `json:"contacts"`
		Truncated bool                   `json:"truncated"`
	}{Contacts: []contactSelectionItem{}}
	err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
		filters := applyContactSortDefaults(h.parseContactFilters(r), r, db.GetUISettings(ctx, userID))
		contacts, truncated, err := db.ListContactsForSelection(ctx, userID, filters, contactSelectionLimit)
		if err != nil {
			return err
		}
		result.Truncated = truncated
		for _, contact := range contacts {
			result.Contacts = append(result.Contacts, contactSelectionItem{ID: contact.ID, Name: contact.Name, Email: strings.TrimSpace(contact.Email)})
		}
		return nil
	})
	if err != nil {
		log.Printf("contact ids unavailable: %v", err)
		http.Error(w, "contacts unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	json.NewEncoder(w).Encode(result)
}

// handleExportSelectedContacts gives the selected contacts, named by id in
// the posted form, as one vCard file. Ids that aren't the user's contacts are
// left out.
func (h *Handler) handleExportSelectedContacts(w http.ResponseWriter, r *http.Request) {
	ids, ok := postedContactIDs(w, r, contactSelectionLimit)
	if !ok {
		return
	}
	ctx := r.Context()
	userID := h.userID(ctx)
	var contacts []models.Contact
	err := h.withUserDB(ctx, userID, func(db *storage.DB) error {
		for _, id := range ids {
			contact, err := db.GetContact(ctx, userID, id)
			if err != nil {
				return err
			}
			if contact != nil {
				contacts = append(contacts, *contact)
			}
		}
		return nil
	})
	if err != nil {
		http.Error(w, "failed to export contacts", http.StatusInternalServerError)
		return
	}
	if len(contacts) == 0 {
		http.NotFound(w, r)
		return
	}
	filename := "gofer-contacts.vcf"
	if len(contacts) == 1 {
		filename = contactVCardFilename(contacts[0])
	}
	serveVCard(w, filename, contacts)
}

// contactBulkDeleteResult says what a bulk delete did: how many contacts went,
// and why each of the others didn't.
type contactBulkDeleteResult struct {
	Deleted int                    `json:"deleted"`
	Failed  []contactDeleteFailure `json:"failed"`
}

type contactDeleteFailure struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// record counts one contact's deletion. One the user no longer has is
// already gone, which is what was asked.
func (result *contactBulkDeleteResult) record(id string, err error) {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		result.Deleted++
		return
	}
	result.Failed = append(result.Failed, contactDeleteFailure{ID: id, Error: contactDeleteFailureMessage(id, err)})
}

func contactDeleteFailureMessage(id string, err error) string {
	var syncErr contactSyncDeleteError
	var provider userContactPreviewProviderError
	switch {
	case errors.As(err, &syncErr), errors.As(err, &provider):
		return "Its address book refused: " + err.Error()
	case errors.Is(err, storage.ErrContactEditChanged), errors.Is(err, config.ErrAccountServicesChanged):
		return "It changed while being deleted. Reload and try again."
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "Deleting it took too long. Try again."
	}
	log.Printf("contacts: bulk delete %s: %v", id, err)
	return "It couldn't be deleted."
}

func writeContactBulkDeleteResult(w http.ResponseWriter, result contactBulkDeleteResult) {
	if result.Failed == nil {
		result.Failed = []contactDeleteFailure{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleDeleteContacts deletes the posted contacts as deleting each alone
// would, going on past any that fail.
func (h *Handler) handleDeleteContacts(w http.ResponseWriter, r *http.Request) {
	ids, ok := postedContactIDs(w, r, contactBulkDeleteLimit)
	if !ok {
		return
	}
	ctx := r.Context()
	userID := h.userID(ctx)
	var result contactBulkDeleteResult
	for _, id := range ids {
		result.record(id, h.deleteContact(ctx, userID, id))
	}
	writeContactBulkDeleteResult(w, result)
}

// handleUserDeleteContacts is handleDeleteContacts with per-user storage:
// each contact goes as handleUserDeleteContact deletes one.
func (h *Handler) handleUserDeleteContacts(w http.ResponseWriter, r *http.Request) {
	ids, ok := postedContactIDs(w, r, contactBulkDeleteLimit)
	if !ok {
		return
	}
	owner := h.userID(r.Context())
	var result contactBulkDeleteResult
	err := h.userIMAP.RunUserServiceWork(r.Context(), owner, func(ctx context.Context) error {
		for _, id := range ids {
			_, err := h.deleteUserContact(ctx, owner, id)
			result.record(id, err)
		}
		return nil
	})
	if err != nil && result.Deleted == 0 && len(result.Failed) == 0 {
		userContactEditError(w, r, err)
		return
	}
	writeContactBulkDeleteResult(w, result)
}
