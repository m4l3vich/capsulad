// Package pair implements the AirPlay 2 / HAP pairing state machines:
// pair-setup (SRP-6a) and pair-verify (X25519 + Ed25519), plus the persistent
// pairing store.
//
// Salt/info/nonce strings and message ordering were confirmed against
// shairport-sync pair_ap/pair_homekit.c (pair_keys_map at :95-125 and the
// request/response builders at :1250-2740).
package pair

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"

	"capsulad/apps/airplay/internal/tlv8"
)

// Device is the accessory identity: an Ed25519 keypair plus pairing id.
type Device struct {
	PairingID string
	LTSK      [32]byte // ed25519 seed
	LTPK      [32]byte // ed25519 public key
}

// private returns the Ed25519 private key derived from the seed.
func (d *Device) private() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(d.LTSK[:])
}

// sign returns an Ed25519 signature over the concatenation of msg parts.
func (d *Device) sign(parts ...[]byte) []byte {
	h := newHasher(parts...)
	return ed25519.Sign(d.private(), h)
}

func newHasher(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Pairing is one known controller: its long-term public key and whether it may
// administer the accessory (add, remove and list other pairings).
type Pairing struct {
	LTPK  []byte
	Admin bool
}

// Store holds the accessory device and the known controllers.
type Store struct {
	Device      *Device
	Controllers map[string]Pairing // pairing id -> key and permissions
}

// NewStore generates a fresh accessory identity with the given pairing id.
func NewStore(pairingID string) (*Store, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	d := &Device{PairingID: pairingID}
	copy(d.LTSK[:], seed)
	copy(d.LTPK[:], pub)
	return &Store{Device: d, Controllers: map[string]Pairing{}}, nil
}

type jsonDevice struct {
	PairingID string `json:"pairing_id"`
	LTSK      string `json:"ltsk"`
	LTPK      string `json:"ltpk"`
}

type jsonPairing struct {
	LTPK  string `json:"ltpk"`
	Admin bool   `json:"admin"`
}

// Marshal serializes the store as JSON.
func (s *Store) Marshal() ([]byte, error) {
	device, err := s.MarshalDevice()
	if err != nil {
		return nil, err
	}
	controllers, err := s.MarshalControllers()
	if err != nil {
		return nil, err
	}
	combined := struct {
		Device      json.RawMessage        `json:"device"`
		Controllers map[string]jsonPairing `json:"controllers"`
	}{}
	if err := json.Unmarshal(device, &combined.Device); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(controllers, &combined.Controllers); err != nil {
		return nil, err
	}
	return json.Marshal(combined)
}

// MarshalDevice serializes only the accessory identity, which is what the
// device_identity setting stores.
func (s *Store) MarshalDevice() ([]byte, error) {
	return json.Marshal(jsonDevice{
		PairingID: s.Device.PairingID,
		LTSK:      base64.StdEncoding.EncodeToString(s.Device.LTSK[:]),
		LTPK:      base64.StdEncoding.EncodeToString(s.Device.LTPK[:]),
	})
}

// MarshalControllers serializes only the controller records, which is what the
// pairings setting stores.
func (s *Store) MarshalControllers() ([]byte, error) {
	out := make(map[string]jsonPairing, len(s.Controllers))
	for id, pairing := range s.Controllers {
		out[id] = jsonPairing{LTPK: base64.StdEncoding.EncodeToString(pairing.LTPK), Admin: pairing.Admin}
	}
	return json.Marshal(out)
}

// LoadDevice applies a device_identity setting value.
func (s *Store) LoadDevice(data []byte) error {
	var device jsonDevice
	if err := json.Unmarshal(data, &device); err != nil {
		return fmt.Errorf("pair: parsing device identity: %w", err)
	}
	seed, err := base64.StdEncoding.DecodeString(device.LTSK)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("pair: invalid device LTSK")
	}
	pub, err := base64.StdEncoding.DecodeString(device.LTPK)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("pair: invalid device LTPK")
	}
	d := &Device{PairingID: device.PairingID}
	copy(d.LTSK[:], seed)
	copy(d.LTPK[:], pub)
	s.Device = d
	return nil
}

// LoadControllers replaces the controller records from a pairings setting
// value. It accepts the legacy form (pairing id -> base64 LTPK string), which
// predates permissions, and treats those controllers as administrators.
func (s *Store) LoadControllers(data []byte) error {
	decoded := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return fmt.Errorf("pair: parsing pairings: %w", err)
	}
	controllers := make(map[string]Pairing, len(decoded))
	for id, raw := range decoded {
		var record jsonPairing
		if err := json.Unmarshal(raw, &record); err == nil && record.LTPK != "" {
			ltpk, err := base64.StdEncoding.DecodeString(record.LTPK)
			if err != nil || len(ltpk) != 32 {
				return fmt.Errorf("pair: invalid LTPK for %q", id)
			}
			controllers[id] = Pairing{LTPK: ltpk, Admin: record.Admin}
			continue
		}
		var legacy string
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return fmt.Errorf("pair: invalid pairing record for %q", id)
		}
		ltpk, err := base64.StdEncoding.DecodeString(legacy)
		if err != nil || len(ltpk) != 32 {
			return fmt.Errorf("pair: invalid LTPK for %q", id)
		}
		controllers[id] = Pairing{LTPK: ltpk, Admin: true}
	}
	s.Controllers = controllers
	return nil
}

// LoadStore parses a store previously produced by Marshal. It is tolerant of
// the older combined format (controllers as bare base64 LTPK strings).
func LoadStore(data []byte) (*Store, error) {
	var raw struct {
		Device      jsonDevice      `json:"device"`
		Controllers json.RawMessage `json:"controllers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pair: parsing store: %w", err)
	}
	device, err := json.Marshal(raw.Device)
	if err != nil {
		return nil, err
	}
	store := &Store{Controllers: map[string]Pairing{}}
	if err := store.LoadDevice(device); err != nil {
		return nil, err
	}
	if len(raw.Controllers) > 0 {
		if err := store.LoadControllers(raw.Controllers); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// ControllerLTPK returns the stored LTPK for a controller id.
func (s *Store) ControllerLTPK(id string) ([]byte, bool) {
	pairing, ok := s.Controllers[id]
	if !ok {
		return nil, false
	}
	return pairing.LTPK, true
}

// Controller returns one controller's record.
func (s *Store) Controller(id string) (Pairing, bool) {
	pairing, ok := s.Controllers[id]
	return pairing, ok
}

// SetController stores a controller's LTPK and permissions.
func (s *Store) SetController(id string, ltpk []byte, admin bool) {
	if s.Controllers == nil {
		s.Controllers = map[string]Pairing{}
	}
	s.Controllers[id] = Pairing{LTPK: append([]byte(nil), ltpk...), Admin: admin}
}

// SetControllerLTPK stores a controller LTPK, keeping any existing permission
// or granting admin when the controller is new (which is what pair-setup
// does).
func (s *Store) SetControllerLTPK(id string, ltpk []byte) {
	admin := true
	if existing, ok := s.Controllers[id]; ok {
		admin = existing.Admin
	}
	s.SetController(id, ltpk, admin)
}

// RemoveController deletes one controller and reports whether it existed.
func (s *Store) RemoveController(id string) bool {
	if _, ok := s.Controllers[id]; !ok {
		return false
	}
	delete(s.Controllers, id)
	return true
}

// ControllerCount reports how many controllers are paired.
func (s *Store) ControllerCount() int { return len(s.Controllers) }

// ControllerIDs returns the paired controller ids in a deterministic order so
// pair-list answers are stable.
func (s *Store) ControllerIDs() []string {
	ids := make([]string, 0, len(s.Controllers))
	for id := range s.Controllers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ---------------------------------------------------------------------------
// shared crypto helpers
// ---------------------------------------------------------------------------

// hkdf32 derives a 32-byte key with HKDF-SHA512.
func hkdf32(ikm []byte, salt, info string) []byte {
	r := hkdf.New(sha512.New, ikm, []byte(salt), []byte(info))
	out := make([]byte, 32)
	if _, err := io.ReadFull(r, out); err != nil {
		panic("pair: hkdf: " + err.Error())
	}
	return out
}

// pairNonce builds the 12-byte ChaCha20-Poly1305 nonce used by HAP pairing:
// four zero bytes followed by the 8-byte message label. Confirmed by
// shairport-sync pair_ap/pair_homekit.c:1406 (memcpy(nonce+4, label, 8)).
func pairNonce(label string) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	copy(n[4:], label)
	return n
}

func seal(key []byte, label string, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, pairNonce(label), plaintext, nil), nil
}

func open(key []byte, label string, ciphertext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.New(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, pairNonce(label), ciphertext, nil)
}

// errorResponse builds a State+Error TLV response.
func errorResponse(state, code byte) []byte {
	return tlv8.Encode(
		tlv8.Item{Tag: tlv8.TagState, Value: []byte{state}},
		tlv8.Item{Tag: tlv8.TagError, Value: []byte{code}},
	)
}
