package session

// Identity and pairing persistence: the settings the app owns must survive a
// restart byte-for-byte, or every sender has to pair again. These tests pin the
// encode/decode round trip and the migration from the earlier combined store
// format.

import (
	"encoding/base64"
	"testing"

	"capsulad/apps/airplay/internal/pair"
)

func TestDeviceIdentityRoundTrip(t *testing.T) {
	original, err := pair.NewStore("accessory")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	loaded := &pair.Store{}
	if err := loaded.LoadDevice(mustDecode(t, encodeDevice(original))); err != nil {
		t.Fatalf("LoadDevice: %v", err)
	}
	if loaded.Device == nil || loaded.Device.PairingID != original.Device.PairingID {
		t.Fatalf("pairing id = %+v", loaded.Device)
	}
	if string(loaded.Device.LTSK[:]) != string(original.Device.LTSK[:]) {
		t.Fatal("the device secret key changed across a round trip")
	}
}

func TestControllersRoundTripWithPermissions(t *testing.T) {
	store, err := pair.NewStore("accessory")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.SetController("admin", []byte("0123456789abcdef0123456789abcdef"), true)
	store.SetController("user", []byte("abcdef0123456789abcdef0123456789"), false)

	loaded := &pair.Store{}
	if err := loaded.LoadControllers(mustDecode(t, encodeControllers(store))); err != nil {
		t.Fatalf("LoadControllers: %v", err)
	}
	admin, ok := loaded.Controller("admin")
	if !ok || !admin.Admin {
		t.Fatalf("admin record = %+v", admin)
	}
	user, ok := loaded.Controller("user")
	if !ok || user.Admin {
		t.Fatalf("user record = %+v", user)
	}
	if ids := loaded.ControllerIDs(); len(ids) != 2 || ids[0] != "admin" || ids[1] != "user" {
		t.Fatalf("controller ids = %v", ids)
	}
}

func TestLegacyCombinedIdentityKeepsControllers(t *testing.T) {
	store, err := pair.NewStore("accessory")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	store.SetController("legacy", []byte("0123456789abcdef0123456789abcdef"), true)
	combined, err := store.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	raw := base64.StdEncoding.EncodeToString(combined)

	loaded, created, err := loadOrCreateStore(raw, "")
	if err != nil {
		t.Fatalf("loadOrCreateStore: %v", err)
	}
	if created {
		t.Fatal("a stored identity was replaced")
	}
	if loaded.Device.PairingID != "accessory" {
		t.Fatalf("pairing id = %q", loaded.Device.PairingID)
	}
	if record, ok := loaded.Controller("legacy"); !ok || !record.Admin {
		t.Fatalf("legacy controller = %+v", record)
	}

	// A separate pairings setting overrides the controllers from the combined
	// document, which is how the split format migrates forward.
	other, err := pair.NewStore("accessory")
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	other.SetController("current", []byte("abcdef0123456789abcdef0123456789"), false)
	loaded, created, err = loadOrCreateStore(raw, base64.StdEncoding.EncodeToString(mustMarshalControllers(t, other)))
	if err != nil || created {
		t.Fatalf("loadOrCreateStore: %v created=%v", err, created)
	}
	if _, ok := loaded.Controller("legacy"); ok {
		t.Fatal("the pairings setting did not replace the legacy controller records")
	}
	if record, ok := loaded.Controller("current"); !ok || record.Admin {
		t.Fatalf("current controller = %+v", record)
	}
}

func TestCorruptIdentityIsRegenerated(t *testing.T) {
	loaded, created, err := loadOrCreateStore("not-base64", "also-not-base64")
	if err != nil {
		t.Fatalf("loadOrCreateStore: %v", err)
	}
	if !created {
		t.Fatal("a corrupt identity was accepted")
	}
	if loaded.Device == nil || loaded.Device.PairingID == "" || loaded.ControllerCount() != 0 {
		t.Fatalf("regenerated store = %+v", loaded.Device)
	}
}

func mustDecode(t *testing.T, value string) []byte {
	t.Helper()
	blob, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	return blob
}

func mustMarshalControllers(t *testing.T, store *pair.Store) []byte {
	t.Helper()
	blob, err := store.MarshalControllers()
	if err != nil {
		t.Fatalf("MarshalControllers: %v", err)
	}
	return blob
}
