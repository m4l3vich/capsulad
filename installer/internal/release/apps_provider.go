package release

import (
	"context"
	"fmt"
	"path/filepath"
)

// AppsProvider resolves only app archives. It is the app-channel half of a
// split release; it never provides a deployment bundle.
type AppsProvider interface {
	Version() string
	App(ctx context.Context, platform, id string) (Artifact, error)
	Apps(platform string) ([]AppRef, error)
}

// LocalAppsProvider serves apps from an offline tree.
type LocalAppsProvider struct {
	Root     string
	Manifest *AppsManifest
}

func NewLocalAppsProvider(root string, m *AppsManifest) *LocalAppsProvider {
	return &LocalAppsProvider{Root: root, Manifest: m}
}

func (p *LocalAppsProvider) Version() string { return p.Manifest.Version }

func (p *LocalAppsProvider) App(ctx context.Context, platform, id string) (Artifact, error) {
	a, err := p.Manifest.App(platform, id)
	if err != nil {
		return Artifact{}, err
	}
	name, err := SafeJoin(p.Root, a.Path)
	if err != nil {
		return Artifact{}, err
	}
	ok, err := verifyExisting(filepath.FromSlash(name), File{Path: a.Path, Size: a.Size, SHA256: a.SHA256})
	if err != nil {
		return Artifact{}, fmt.Errorf("offline app %s: %w", a.Path, err)
	}
	if !ok {
		return Artifact{}, fmt.Errorf("offline app %s does not match its manifest hash", a.Path)
	}
	return Artifact{Path: filepath.FromSlash(name), Size: a.Size, SHA256: a.SHA256}, nil
}

func (p *LocalAppsProvider) Apps(platform string) ([]AppRef, error) {
	pr, err := p.Manifest.ForApps(platform)
	if err != nil {
		return nil, err
	}
	return refs(pr.Apps), nil
}

// RemoteAppsProvider downloads app archives through a Fetcher rooted at the app
// channel URL.
type RemoteAppsProvider struct {
	Fetcher  *Fetcher
	Manifest *AppsManifest
}

func NewRemoteAppsProvider(f *Fetcher, m *AppsManifest) *RemoteAppsProvider {
	return &RemoteAppsProvider{Fetcher: f, Manifest: m}
}

func (p *RemoteAppsProvider) Version() string { return p.Manifest.Version }

func (p *RemoteAppsProvider) App(ctx context.Context, platform, id string) (Artifact, error) {
	a, err := p.Manifest.App(platform, id)
	if err != nil {
		return Artifact{}, err
	}
	name, err := p.Fetcher.Artifact(ctx, p.Manifest.Version, File{Path: a.Path, Size: a.Size, SHA256: a.SHA256}, AppMaxBytes)
	if err != nil {
		return Artifact{}, err
	}
	return Artifact{Path: name, Size: a.Size, SHA256: a.SHA256}, nil
}

func (p *RemoteAppsProvider) Apps(platform string) ([]AppRef, error) {
	pr, err := p.Manifest.ForApps(platform)
	if err != nil {
		return nil, err
	}
	return refs(pr.Apps), nil
}

// CombinedProvider takes the bundle from the immutable core release and the
// app archives from the independently versioned app channel.
type CombinedProvider struct {
	Core      Provider
	AppSource AppsProvider
}

func NewCombinedProvider(core Provider, apps AppsProvider) *CombinedProvider {
	return &CombinedProvider{Core: core, AppSource: apps}
}

func (p *CombinedProvider) Version() string { return p.Core.Version() }

func (p *CombinedProvider) Bundle(ctx context.Context, platform string) (Artifact, error) {
	return p.Core.Bundle(ctx, platform)
}

func (p *CombinedProvider) App(ctx context.Context, platform, id string) (Artifact, error) {
	return p.AppSource.App(ctx, platform, id)
}

func (p *CombinedProvider) Apps(platform string) ([]AppRef, error) {
	return p.AppSource.Apps(platform)
}
