package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testStore(t *testing.T) (*Store, Authentication) {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "bootstrap")
	if err := os.WriteFile(secret, []byte("test-account-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(dir, "accounts"), "admin", secret)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := store.Authenticate("ADMIN", "test-account-password")
	if err != nil {
		t.Fatal(err)
	}
	return store, auth
}

func TestPrivatePersistenceAndSessionRevocation(t *testing.T) {
	store, admin := testStore(t)
	user, err := store.SaveUser(admin.ID, UserChange{Username: "reader", Password: "reader-password", Permissions: Permissions{Download: true}})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := store.Authenticate("reader", "reader-password")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.NewSession(auth)
	if err != nil {
		t.Fatal(err)
	}
	if current, _, ok := store.Current(raw); !ok || current.Permissions != (Permissions{Download: true}) {
		t.Fatal("wrong reader permissions")
	}
	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), raw) || strings.Contains(string(data), "reader-password") {
		t.Fatal("raw session/password persisted")
	}
	reopened, err := Open(filepath.Dir(store.path), "ignored", "missing-bootstrap-file")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reopened.Current(raw); !ok {
		t.Fatal("session did not survive restart")
	}
	if _, err := store.SaveUser(admin.ID, UserChange{ID: user.ID, Username: "reader", Permissions: Permissions{Upload: true}, Version: user.Version}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.Current(raw); ok {
		t.Fatal("permission change did not revoke session")
	}
	if _, _, err := store.NewSession(auth); err == nil {
		t.Fatal("stale password authentication created a new session")
	}
}

func TestLastManagerAndIndependentPermissions(t *testing.T) {
	store, admin := testStore(t)
	if err := store.DeleteUser(admin.ID, admin.ID); err != ErrInput("last_manager") {
		t.Fatalf("last manager delete: %v", err)
	}
	if _, err := store.SaveUser(admin.ID, UserChange{ID: admin.ID, Username: "admin", Disabled: true, Version: admin.Version}); err != ErrInput("last_manager") {
		t.Fatalf("last manager disable: %v", err)
	}
	manager, err := store.SaveUser(admin.ID, UserChange{Username: "manager", Password: "manager-password", Permissions: Permissions{Manage: true}})
	if err != nil {
		t.Fatal(err)
	}
	if manager.Permissions.Upload || manager.Permissions.Download {
		t.Fatal("management silently granted transfer permissions")
	}
	if err := store.DeleteUser(manager.ID, admin.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUser(manager.ID, manager.ID); err != ErrInput("last_manager") {
		t.Fatalf("last remaining manager: %v", err)
	}
	encoded, err := json.Marshal(store.List())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "password_hash") || strings.Contains(string(encoded), "revision") {
		t.Fatal("public user list leaked private fields")
	}
}

func TestPasswordResetDisableAndWriteFailure(t *testing.T) {
	store, admin := testStore(t)
	user, err := store.SaveUser(admin.ID, UserChange{Username: "uploader", Password: "before-password", Permissions: Permissions{Upload: true}})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := store.Authenticate("uploader", "before-password")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := store.NewSession(auth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveUser(admin.ID, UserChange{ID: user.ID, Username: "uploader", Password: "after-password", Permissions: user.Permissions, Version: user.Version}); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := store.Current(raw); ok {
		t.Fatal("password reset kept an old session")
	}
	if _, err := store.Authenticate("uploader", "before-password"); err == nil {
		t.Fatal("old password still works")
	}
	if _, err := store.SaveUser(admin.ID, UserChange{ID: user.ID, Username: "uploader", Permissions: user.Permissions, Disabled: true, Version: user.Version + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate("uploader", "after-password"); err == nil {
		t.Fatal("disabled account signed in")
	}
	store.path = filepath.Join(t.TempDir(), "missing-parent", "accounts.json")
	if _, err := store.SaveUser(admin.ID, UserChange{ID: user.ID, Username: "changed", Version: user.Version + 2}); err == nil {
		t.Fatal("write failure was hidden")
	}
	for _, current := range store.List() {
		if current.ID == user.ID && current.Username != "uploader" {
			t.Fatal("failed write changed live state")
		}
	}
}
