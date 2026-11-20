package accounts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestUniqueAdminAndIndependentPermissions(t *testing.T) {
	store, admin := testStore(t)
	if err := store.DeleteUser(admin.ID, admin.ID); err != ErrInput("admin_immutable") {
		t.Fatalf("administrator delete: %v", err)
	}
	if _, err := store.SaveUser(admin.ID, UserChange{ID: admin.ID, Username: admin.Username, Permissions: admin.Permissions, Disabled: true, Version: admin.Version}); err != ErrInput("admin_immutable") {
		t.Fatalf("administrator disable: %v", err)
	}
	user, err := store.SaveUser(admin.ID, UserChange{Username: "downloadonly", Password: "user-password", Permissions: Permissions{Download: true}})
	if err != nil {
		t.Fatal(err)
	}
	if user.Permissions.Upload || user.Permissions.Manage {
		t.Fatal("permissions were granted implicitly")
	}
	if _, err := store.SaveUser(admin.ID, UserChange{Username: "manager", Password: "manager-password", Permissions: Permissions{Manage: true}}); err != ErrInput("admin_exists") {
		t.Fatalf("second administrator creation returned %v", err)
	}
	encoded, err := json.Marshal(store.List())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "password_hash") || strings.Contains(string(encoded), "revision") {
		t.Fatal("public user list leaked private fields")
	}
}

func TestUserUploadLimitsAndImmutableAdmin(t *testing.T) {
	store, admin := testStore(t)
	if admin.UploadLimitBytes != nil {
		t.Fatal("administrator unexpectedly has a user cap")
	}
	defaultLimit := DefaultUploadLimitBytes
	user, err := store.SaveUser(admin.ID, UserChange{Username: "edgeuser", Password: "edge-password", Permissions: Permissions{Upload: true}, UploadLimitSet: true, UploadLimitBytes: &defaultLimit})
	if err != nil {
		t.Fatal(err)
	}
	if user.UploadLimitBytes == nil || *user.UploadLimitBytes != DefaultUploadLimitBytes {
		t.Fatal("new user did not retain its explicit default limit")
	}
	if _, err := store.SaveUser(admin.ID, UserChange{ID: user.ID, Username: user.Username, Permissions: Permissions{Upload: true, Manage: true}, Version: user.Version}); err != ErrInput("admin_exists") {
		t.Fatalf("granting another manager returned %v", err)
	}
	if _, err := store.SaveUser(admin.ID, UserChange{ID: admin.ID, Username: admin.Username, Permissions: admin.Permissions, Disabled: true, Version: admin.Version}); err != ErrInput("admin_immutable") {
		t.Fatalf("disabling administrator returned %v", err)
	}
	if err := store.DeleteUser(admin.ID, admin.ID); err != ErrInput("admin_immutable") {
		t.Fatalf("deleting administrator returned %v", err)
	}
	if _, err := store.SaveUser(admin.ID, UserChange{ID: user.ID, Username: user.Username, Permissions: user.Permissions, UploadLimitSet: true, UploadLimitBytes: nil, Version: user.Version}); err != nil {
		t.Fatal(err)
	}
	for _, got := range store.List() {
		if got.ID == user.ID && got.UploadLimitBytes != nil {
			t.Fatal("clearing user limit did not persist")
		}
	}
}

func TestMigrateV1Accounts(t *testing.T) {
	adminID, otherID := "admin-id", "other-id"
	saved := database{Version: 1, Users: map[string]record{
		adminID: {User: User{ID: adminID, Username: "owner", Permissions: Permissions{Upload: true, Download: true, Manage: true}, CreatedAt: time.Now().UTC(), Version: 3}, PasswordHash: "owner-hash"},
		otherID: {User: User{ID: otherID, Username: "coreuser", Permissions: Permissions{Upload: true, Download: true, Manage: true}, CreatedAt: time.Now().UTC(), Version: 4}, PasswordHash: "other-hash"},
	}, Sessions: map[string]Session{"old": {UserID: otherID}}}
	if err := migrateV1(&saved, "owner"); err != nil {
		t.Fatal(err)
	}
	if saved.Version != accountDatabaseVersion || saved.AdminID != adminID || len(saved.Sessions) != 0 {
		t.Fatalf("bad migrated database metadata: %+v", saved)
	}
	owner, user := saved.Users[adminID], saved.Users[otherID]
	if owner.PasswordHash != "owner-hash" || owner.UploadLimitBytes != nil || owner.Permissions != (Permissions{Upload: true, Download: true, Manage: true}) {
		t.Fatal("administrator credentials or permissions were not preserved")
	}
	if user.PasswordHash != "other-hash" || user.Permissions != (Permissions{Upload: true, Download: true}) || user.UploadLimitBytes == nil || *user.UploadLimitBytes != DefaultUploadLimitBytes {
		t.Fatalf("other user was not conservatively migrated: %+v", user)
	}
}

func TestMigrateV1RequiresUnambiguousAdmin(t *testing.T) {
	makeState := func(t *testing.T) database {
		t.Helper()
		users := map[string]record{}
		for _, name := range []string{"ownerone", "ownertwo"} {
			id, err := token()
			if err != nil {
				t.Fatal(err)
			}
			users[id] = record{User: User{ID: id, Username: name, Permissions: Permissions{Manage: true}}}
		}
		return database{Version: 1, Users: users, Sessions: map[string]Session{}}
	}
	ambiguous := makeState(t)
	if err := migrateV1(&ambiguous, ""); err == nil {
		t.Fatal("ambiguous admin migration succeeded without a selector")
	}
	state := makeState(t)
	if err := migrateV1(&state, "ownertwo"); err != nil {
		t.Fatalf("explicit administrator selection failed: %v", err)
	}
	if state.Users[state.AdminID].Username != "ownertwo" {
		t.Fatal("migration ignored explicit administrator selection")
	}
	if err := migrateV1(&state, "missing"); err == nil {
		t.Fatal("migration accepted an unknown administrator")
	}
}

func TestOpenMigratesV1AndPreservesAmbiguousDatabase(t *testing.T) {
	store, admin := testStore(t)
	raw, _, err := store.NewSession(admin)
	if err != nil {
		t.Fatal(err)
	}
	state := store.clone()
	state.Version = 1
	state.AdminID = ""
	legacy, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NCLIP_OWNER_USERNAME", "")
	reopened, err := Open(filepath.Dir(store.path), "ignored", "missing-bootstrap-file")
	if err != nil {
		t.Fatal(err)
	}
	if reopened.state.Version != accountDatabaseVersion || reopened.state.AdminID != admin.ID || len(reopened.state.Sessions) != 0 {
		t.Fatalf("v1 database did not migrate: %+v", reopened.state)
	}
	if _, _, ok := reopened.Current(raw); ok {
		t.Fatal("migration retained a legacy session")
	}

	ambiguousDir := t.TempDir()
	ambiguousPath := filepath.Join(ambiguousDir, "accounts.json")
	ambiguousState := state
	secondID := "second-admin-id"
	ambiguousState.Users[secondID] = record{User: User{ID: secondID, Username: "secondadmin", Permissions: Permissions{Upload: true, Download: true, Manage: true}, Version: 1}}
	ambiguous, err := json.Marshal(ambiguousState)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ambiguousPath, ambiguous, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ambiguousDir, "ignored", "missing-bootstrap-file"); err == nil {
		t.Fatal("ambiguous migration succeeded without an owner selector")
	}
	current, err := os.ReadFile(ambiguousPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, ambiguous) {
		t.Fatal("failed migration changed the legacy database")
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
