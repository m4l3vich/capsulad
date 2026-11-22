package release

import (
	"context"
	"fmt"
	"path/filepath"
)

// Artifact is a concrete host file ready to push.
type Artifact struct {
	Path   string
	Size   int64
	SHA256 string
}

// AppRef is one app entry's identity and size without downloading it.
type AppRef struct {
	ID   string
	Size int64
}

// Provider resolves the bundle and app archives for one platform.
type Provider interface {
	Version() string
	Bundle(ctx context.Context, platform string) (Artifact, error)
	App(ctx context.Context, platform, id string) (Artifact, error)
	// Apps lists the platform's app entries for size planning. It never
	// downloads an artifact.
	Apps(platform string) ([]AppRef, error)
}

// LocalProvider serves an offline release tree. Every read is verified against
// the manifest, so a tree that changed after discovery is refused.
type LocalProvider struct {
	Root     string
	Manifest *Manifest
}

// NewLocalProvider builds a provider over root.
func NewLocalProvider(root string, m *Manifest) *LocalProvider {
	return &LocalProvider{Root: root, Manifest: m}
}

func (p *LocalProvider) Version() string { return p.Manifest.Version }

func (p *LocalProvider) Bundle(ctx context.Context, platform string) (Artifact, error) {
	pr, err := p.Manifest.For(platform)
	if err != nil {
		return Artifact{}, err
	}
	return p.local(pr.Bundle)
}

func (p *LocalProvider) App(ctx context.Context, platform, id string) (Artifact, error) {
	a, err := p.Manifest.App(platform, id)
	if err != nil {
		return Artifact{}, err
	}
	return p.local(File{Path: a.Path, Size: a.Size, SHA256: a.SHA256})
}

func (p *LocalProvider) Apps(platform string) ([]AppRef, error) {
	pr, err := p.Manifest.For(platform)
	if err != nil {
		return nil, err
	}
	return refs(pr.Apps), nil
}

func (p *LocalProvider) local(f File) (Artifact, error) {
	name, err := SafeJoin(p.Root, f.Path)
	if err != nil {
		return Artifact{}, err
	}
	ok, err := verifyExisting(filepath.FromSlash(name), f)
	if err != nil {
		return Artifact{}, fmt.Errorf("offline artifact %s: %w", f.Path, err)
	}
	if !ok {
		return Artifact{}, fmt.Errorf("offline artifact %s does not match its manifest hash (re-run stage-release or remove release.json to rediscover)", f.Path)
	}
	return Artifact{Path: filepath.FromSlash(name), Size: f.Size, SHA256: f.SHA256}, nil
}

// RemoteProvider downloads manifest entries through a Fetcher.
type RemoteProvider struct {
	Fetcher  *Fetcher
	Manifest *Manifest
}

// NewRemoteProvider builds a downloading provider.
func NewRemoteProvider(f *Fetcher, m *Manifest) *RemoteProvider {
	return &RemoteProvider{Fetcher: f, Manifest: m}
}

func (p *RemoteProvider) Version() string { return p.Manifest.Version }

func (p *RemoteProvider) Bundle(ctx context.Context, platform string) (Artifact, error) {
	pr, err := p.Manifest.For(platform)
	if err != nil {
		return Artifact{}, err
	}
	return p.fetch(ctx, pr.Bundle, BundleMaxBytes)
}

func (p *RemoteProvider) App(ctx context.Context, platform, id string) (Artifact, error) {
	a, err := p.Manifest.App(platform, id)
	if err != nil {
		return Artifact{}, err
	}
	return p.fetch(ctx, File{Path: a.Path, Size: a.Size, SHA256: a.SHA256}, AppMaxBytes)
}

func (p *RemoteProvider) Apps(platform string) ([]AppRef, error) {
	pr, err := p.Manifest.For(platform)
	if err != nil {
		return nil, err
	}
	return refs(pr.Apps), nil
}

func refs(apps []App) []AppRef {
	out := make([]AppRef, len(apps))
	for i, a := range apps {
		out[i] = AppRef{ID: a.ID, Size: a.Size}
	}
	return out
}

func (p *RemoteProvider) fetch(ctx context.Context, f File, max int64) (Artifact, error) {
	name, err := p.Fetcher.Artifact(ctx, p.Manifest.Version, f, max)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Path: name, Size: f.Size, SHA256: f.SHA256}, nil
}

// OfflineProvider returns the natural provider for a directory.
func OfflineProvider(root string) (Provider, *Manifest, error) {
	m, err := LoadDir(root)
	if err != nil {
		return nil, nil, err
	}
	return NewLocalProvider(root, m), m, nil
}
