package airplay

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
)

// Scoped restore tokens (DOUBLETAKE_RESTORE_SCOPE) must not touch the legacy
// unscoped token: mirror and extend capture different monitors, and a scope
// save/restore must never leak into the other scope.

func TestRestoreTokenForScopeReadsLegacyFieldWhenUnscoped(t *testing.T) {
	creds := &SavedCredentials{RestoreToken: "legacy"}
	if got := creds.RestoreTokenForScope(""); got != "legacy" {
		t.Fatalf("unscoped read = %q, want legacy", got)
	}
	if got := creds.RestoreTokenForScope("extend"); got != "" {
		t.Fatalf("unpopulated scoped read = %q, want empty", got)
	}
	var nilCreds *SavedCredentials
	if got := nilCreds.RestoreTokenForScope("extend"); got != "" {
		t.Fatalf("nil receiver read = %q, want empty", got)
	}
}

func TestSaveRestoreTokenScopedWritesDoNotTouchLegacyToken(t *testing.T) {
	store, err := NewCredentialStore(filepath.Join(t.TempDir(), "credentials.json"))
	if err != nil {
		t.Fatalf("NewCredentialStore: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := store.Save("device-1", "pair-1", pub, priv); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.SaveRestoreToken("device-1", "legacy-token"); err != nil {
		t.Fatalf("SaveRestoreToken unscoped: %v", err)
	}

	t.Setenv(RestoreScopeEnv, "extend")
	// Saving under the extend scope must leave the legacy field alone and
	// land in the scoped map.
	if err := store.SaveRestoreToken("device-1", "extend-token"); err != nil {
		t.Fatalf("SaveRestoreToken (scoped): %v", err)
	}
	creds := store.Lookup("device-1")
	if creds.RestoreToken != "legacy-token" {
		t.Fatalf("legacy token = %q, want untouched %q", creds.RestoreToken, "legacy-token")
	}
	if got := creds.RestoreTokenForScope("extend"); got != "extend-token" {
		t.Fatalf("scoped token = %q, want %q", got, "extend-token")
	}
	if got := creds.RestoreTokenForScope(""); got != "legacy-token" {
		t.Fatalf("unscoped token after scoped save = %q, want %q", got, "legacy-token")
	}
}

func TestRestoreScopeEnvSelectsReadsAndSurvivesPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	store, err := NewCredentialStore(path)
	if err != nil {
		t.Fatalf("NewCredentialStore: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if err := store.Save("device-1", "pair-1", pub, priv); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if got := RestoreScope(); got != "" {
		t.Fatalf("RestoreScope with env unset = %q, want empty", got)
	}
	if err := store.SaveRestoreToken("device-1", "mirror-token"); err != nil {
		t.Fatalf("SaveRestoreToken unscoped: %v", err)
	}

	t.Setenv(RestoreScopeEnv, "extend")
	if got := RestoreScope(); got != "extend" {
		t.Fatalf("RestoreScope with env set = %q, want extend", got)
	}
	if err := store.SaveRestoreToken("device-1", "extend-token"); err != nil {
		t.Fatalf("SaveRestoreToken scoped: %v", err)
	}

	// A fresh store over the same file (daemon restart) restores both scopes.
	reopened, err := NewCredentialStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	creds := reopened.Lookup("device-1")
	if got := creds.RestoreTokenForScope(""); got != "mirror-token" {
		t.Fatalf("persisted unscoped token = %q, want mirror-token", got)
	}
	if got := creds.RestoreTokenForScope("extend"); got != "extend-token" {
		t.Fatalf("persisted scoped token = %q, want extend-token", got)
	}
}
