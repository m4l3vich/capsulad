//go:build !unix

package websecurity

// acquireCredentialsLock is a no-op on platforms without flock. The speaker and
// its maintenance tooling are Unix targets; this fallback only keeps the package
// portable for host-side builds.
func acquireCredentialsLock(string) (func(), error) {
	return func() {}, nil
}
