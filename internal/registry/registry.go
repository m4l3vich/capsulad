// Package registry discovers installed application packages and their
// published interface descriptors. The package tree is daemon-owned and
// mounted read-only into each app sandbox; apps never modify it.
package registry

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"capsulad/internal/artifact"
	"capsulad/internal/logbus"
	"capsulad/internal/manifest"
)

type Interface struct {
	Service    string
	Version    string
	Summary    string
	Docs       string
	Descriptor []byte
	Methods    []Method
}

type Method struct {
	Name string
	// InputType and OutputType are fully-qualified protobuf message names, if
	// the descriptor is well formed.
	InputType       string
	OutputType      string
	ClientStreaming bool
	ServerStreaming bool
}

type App struct {
	Manifest   *manifest.Manifest
	Dir        string
	Interfaces []Interface
}

func (a *App) ID() string { return a.Manifest.ID }

type Registry struct {
	root string
	logs *logbus.Bus

	mu   sync.RWMutex
	apps map[string]*App
	// builtins are applications compiled into the daemon. They share the same
	// lookup and listing surface as installed packages but have no package
	// directory and cannot be installed or uninstalled.
	builtins map[string]*App
}

func Open(root string, logs *logbus.Bus) (*Registry, error) {
	r := &Registry{root: root, logs: logs, apps: map[string]*App{}, builtins: map[string]*App{}}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("apps root: %w", err)
	}
	if err := r.Rescan(); err != nil {
		return nil, err
	}
	return r, nil
}

// Rescan reloads every installed package. A malformed package is skipped and
// logged; it must not prevent other apps from starting.
func (r *Registry) Rescan() error {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return err
	}
	r.mu.RLock()
	reserved := make(map[string]bool, len(r.builtins))
	for id := range r.builtins {
		reserved[id] = true
	}
	r.mu.RUnlock()
	apps := map[string]*App{}
	for _, e := range entries {
		// Staging and retired package directories are dot-prefixed so an
		// interrupted install can never be mistaken for an installed app.
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(r.root, e.Name())
		mf := filepath.Join(dir, artifact.ManifestFile)
		if _, err := os.Stat(mf); err != nil {
			continue
		}
		m, err := manifest.Load(dir, mf)
		if err != nil {
			r.logs.Warnf("registry", "", "skipping package in %s: %v", e.Name(), err)
			continue
		}
		if reserved[m.ID] {
			r.logs.Warnf("registry", m.ID, "ignoring package %s: the ID belongs to a built-in system application", dir)
			continue
		}
		if m.ID != e.Name() {
			r.logs.Warnf("registry", m.ID, "package directory %q does not match app id %q; using manifest id", e.Name(), m.ID)
		}
		app := &App{Manifest: m, Dir: dir}
		for _, decl := range m.Interfaces {
			iface, err := loadInterface(dir, decl)
			if err != nil {
				r.logs.Warnf("registry", m.ID, "interface %s: %v", decl.Service, err)
				continue
			}
			app.Interfaces = append(app.Interfaces, iface)
		}
		if existing, ok := apps[m.ID]; ok {
			r.logs.Warnf("registry", m.ID, "duplicate installed app; keeping %s and ignoring %s", existing.Dir, dir)
			continue
		}
		apps[m.ID] = app
	}
	r.mu.Lock()
	r.apps = apps
	r.mu.Unlock()
	return nil
}

func loadInterface(dir string, decl manifest.Interface) (Interface, error) {
	iface := Interface{Service: decl.Service, Version: decl.Version}
	if decl.Descriptor != "" {
		path := filepath.Join(dir, decl.Descriptor)
		data, err := os.ReadFile(path)
		if err != nil {
			return iface, fmt.Errorf("descriptor: %w", err)
		}
		if err := validateDescriptorSet(data); err != nil {
			return iface, fmt.Errorf("descriptor %s: %w", decl.Descriptor, err)
		}
		iface.Descriptor = data
		iface.Methods = methodsForService(data, decl.Service)
		if len(iface.Methods) == 0 {
			return iface, fmt.Errorf("service %s is not present in %s", decl.Service, decl.Descriptor)
		}
	}
	if decl.Docs != "" {
		data, err := os.ReadFile(filepath.Join(dir, decl.Docs))
		if err != nil {
			return iface, fmt.Errorf("docs: %w", err)
		}
		iface.Docs = string(data)
		iface.Summary = firstNonEmptyLine(iface.Docs)
	}
	return iface, nil
}

func validateDescriptorSet(data []byte) error {
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &set); err != nil {
		return err
	}
	if len(set.File) == 0 {
		return fmt.Errorf("descriptor set is empty")
	}
	// Ensure the payload is a plausible descriptor set rather than arbitrary
	// bytes that happen to decode: at least one file must name a message.
	for _, f := range set.File {
		if f.GetName() == "" {
			return fmt.Errorf("descriptor entry without a file name")
		}
	}
	return nil
}

// MethodsForService exposes the method list for one service in a serialized
// FileDescriptorSet. Compiled-in providers build their discovery metadata from
// generated descriptors at runtime and use this to match installed packages.
func MethodsForService(data []byte, serviceName string) []Method {
	return methodsForService(data, serviceName)
}

func methodsForService(data []byte, serviceName string) []Method {
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(data, &set); err != nil {
		return nil
	}
	for _, f := range set.File {
		for _, s := range f.Service {
			full := s.GetName()
			if f.GetPackage() != "" {
				full = f.GetPackage() + "." + s.GetName()
			}
			if full != serviceName {
				continue
			}
			out := make([]Method, 0, len(s.Method))
			for _, m := range s.Method {
				out = append(out, Method{
					Name:            m.GetName(),
					InputType:       strings.TrimPrefix(m.GetInputType(), "."),
					OutputType:      strings.TrimPrefix(m.GetOutputType(), "."),
					ClientStreaming: m.GetClientStreaming(),
					ServerStreaming: m.GetServerStreaming(),
				})
			}
			return out
		}
	}
	return nil
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#"))
		if line != "" {
			return line
		}
	}
	return ""
}

func (r *Registry) Get(appID string) (*App, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if a, ok := r.builtins[appID]; ok {
		return a, true
	}
	a, ok := r.apps[appID]
	return a, ok
}

// RegisterBuiltin adds a compiled-in application to the same identity space as
// installed packages. It returns an error when the ID is already present: a
// package must never silently shadow a system app or the reverse.
func (r *Registry) RegisterBuiltin(app *App) error {
	if app == nil || app.Manifest == nil {
		return fmt.Errorf("builtin application is required")
	}
	if app.Manifest.Entrypoint.Runtime != manifest.RuntimeBuiltin {
		return fmt.Errorf("builtin %s must use the builtin runtime", app.Manifest.ID)
	}
	if err := app.Manifest.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.builtins[app.Manifest.ID]; ok {
		return fmt.Errorf("builtin application %s is already registered", app.Manifest.ID)
	}
	if _, ok := r.apps[app.Manifest.ID]; ok {
		return fmt.Errorf("installed package %s collides with a builtin application", app.Manifest.ID)
	}
	r.builtins[app.Manifest.ID] = app
	return nil
}

// IsBuiltin reports whether an app ID is owned by the daemon.
func (r *Registry) IsBuiltin(appID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.builtins[appID]
	return ok
}

func (r *Registry) List() []*App {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*App, 0, len(r.apps)+len(r.builtins))
	for _, a := range r.apps {
		out = append(out, a)
	}
	for _, a := range r.builtins {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// InterfacesFor returns interfaces published by an app.
func (a *App) InterfacesFor(service string) (Interface, bool) {
	for _, i := range a.Interfaces {
		if i.Service == service {
			return i, true
		}
	}
	return Interface{}, false
}

// Install admits a validated package directory or .tar.gz archive and installs
// it.
func (r *Registry) Install(src string) (*App, error) {
	stage, err := os.MkdirTemp(r.root, ".install-")
	if err != nil {
		return nil, fmt.Errorf("stage package: %w", err)
	}
	defer os.RemoveAll(stage)
	pkg, err := artifact.OpenIn(stage, src)
	if err != nil {
		return nil, err
	}
	defer pkg.Close()
	return r.InstallPackage(pkg)
}

// InstallPackage installs an already-opened artifact. The new tree is staged
// inside the apps root, the installed directory is moved aside, the staged
// tree is renamed into place and only then is the old directory removed: a
// failed install or update leaves the previous installation intact and never
// exposes a partial package.
//
// The caller's staged tree is consumed: it is renamed into the apps root when
// it already lives on that filesystem, so a package is only ever copied once
// on the 78.5 MiB data partition.
func (r *Registry) InstallPackage(pkg *artifact.Package) (*App, error) {
	if pkg == nil || pkg.Manifest == nil {
		return nil, fmt.Errorf("package is required")
	}
	m := pkg.Manifest
	if r.IsBuiltin(m.ID) {
		return nil, fmt.Errorf("app %s is a built-in system application and cannot be replaced", m.ID)
	}
	target := filepath.Join(r.root, m.ID)
	stage, err := os.MkdirTemp(r.root, ".stage-")
	if err != nil {
		return nil, fmt.Errorf("stage package: %w", err)
	}
	defer os.RemoveAll(stage)
	staged := filepath.Join(stage, "pkg")
	if err := os.Rename(pkg.Dir, staged); err != nil {
		copied, copyErr := artifact.OpenIn(stage, pkg.Dir)
		if copyErr != nil {
			return nil, fmt.Errorf("stage package: %w", copyErr)
		}
		defer copied.Close()
		staged = copied.Dir
	}

	previous := ""
	if _, err := os.Stat(target); err == nil {
		retired, err := os.MkdirTemp(r.root, ".previous-")
		if err != nil {
			return nil, fmt.Errorf("stage previous package: %w", err)
		}
		previous = retired
		if err := os.Rename(target, filepath.Join(previous, "pkg")); err != nil {
			_ = os.RemoveAll(previous)
			return nil, fmt.Errorf("set aside installed %s: %w", m.ID, err)
		}
	}
	if previous != "" {
		defer os.RemoveAll(previous)
	}
	if err := os.Rename(staged, target); err != nil {
		if previous != "" {
			_ = os.Rename(filepath.Join(previous, "pkg"), target)
		}
		return nil, fmt.Errorf("activate %s: %w", m.ID, err)
	}
	if err := r.Rescan(); err != nil {
		return nil, err
	}
	app, ok := r.Get(m.ID)
	if !ok {
		return nil, fmt.Errorf("installed app %s was not discovered", m.ID)
	}
	return app, nil
}

// Uninstall removes an installed package directory.
func (r *Registry) Uninstall(appID string) error {
	if r.IsBuiltin(appID) {
		return fmt.Errorf("app %s is a built-in system application and cannot be uninstalled", appID)
	}
	app, ok := r.Get(appID)
	if !ok {
		return fmt.Errorf("app %s is not installed", appID)
	}
	if err := os.RemoveAll(app.Dir); err != nil {
		return err
	}
	return r.Rescan()
}
