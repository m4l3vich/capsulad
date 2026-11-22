package install

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

// GeneratedPasswordLength is comfortably above the 20 character floor the
// installer promises.
const GeneratedPasswordLength = 24

// MinCustomPasswordLength is the floor for a password the user types. It is
// lower than GeneratedPasswordLength so an operator's existing long password
// is never rejected for being shorter than our generator.
const MinCustomPasswordLength = 8

// passwordAlphabet omits visually ambiguous characters (I/l/1, O/0) because
// the password is read from a terminal and typed on a phone.
const passwordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// GeneratePassword returns a cryptographically random password.
func GeneratePassword(n int) (string, error) {
	if n < GeneratedPasswordLength {
		n = GeneratedPasswordLength
	}
	var b strings.Builder
	b.Grow(n)
	max := big.NewInt(int64(len(passwordAlphabet)))
	for b.Len() < n {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate password: %w", err)
		}
		b.WriteByte(passwordAlphabet[idx.Int64()])
	}
	return b.String(), nil
}

// ValidatePassword checks a user-supplied password without echoing it.
func ValidatePassword(p string) error {
	if len(p) < MinCustomPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinCustomPasswordLength)
	}
	if strings.ContainsAny(p, "\r\n") {
		return fmt.Errorf("password must not contain a line break")
	}
	return nil
}

// DefaultConfigDir is the installer's per-user state root. It holds the SSH
// known-hosts file and pre-change backups.
func DefaultConfigDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "capsulad-installer"), nil
}

// DefaultCacheDir is where downloaded release artifacts are cached.
func DefaultCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "capsulad-installer"), nil
}
