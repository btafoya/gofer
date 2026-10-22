package config

import (
	"context"

	"github.com/cristianadrielbraun/gofer/internal/storage"
)

// TrashContacts moves the owner's contacts to the Trash.
func (s *UserAccountStore) TrashContacts(ctx context.Context, owner string, ids []string) (count int, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		count, err = db.TrashUserContacts(ctx, owner, ids, s.contactSyncOwnerGuard(ctx, owner))
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

// RestoreContacts brings the owner's contacts back from the Trash.
func (s *UserAccountStore) RestoreContacts(ctx context.Context, owner string, ids []string) (count int, err error) {
	err = s.WithUser(ctx, owner, func(_ *AccountStore, db *storage.DB) error {
		count, err = db.RestoreUserContacts(ctx, owner, ids, s.contactSyncOwnerGuard(ctx, owner))
		return err
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
