package daemon

import (
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"capsulad/internal/uichanges"
)

// watchWebCredentials converges browser sessions with credential changes made
// by the local CLI in another process. The write is an atomic rename, so the
// directory is watched and the event is debounced. A password change clears
// the in-memory session store and closes every live socket; API-key edits just
// invalidate the Access topic. No polling is involved.
func (d *Daemon) watchWebCredentials() {
	if d.websec == nil {
		return
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		d.logs.Warnf("webui", "", "credential watcher unavailable: %v", err)
		return
	}
	if err := watcher.Add(filepath.Dir(d.websec.CredentialsPath())); err != nil {
		_ = watcher.Close()
		d.logs.Warnf("webui", "", "credential watcher unavailable: %v", err)
		return
	}
	go func() {
		defer watcher.Close()
		var timer *time.Timer
		var timerC <-chan time.Time
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		for {
			select {
			case <-d.ctx.Done():
				return
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if filepath.Base(event.Name) != "credentials.json" {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if timer == nil {
					timer = time.NewTimer(50 * time.Millisecond)
					timerC = timer.C
				} else {
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					timer.Reset(50 * time.Millisecond)
				}
			case <-timerC:
				timer = nil
				timerC = nil
				passwordChanged, err := d.websec.Reload()
				if err != nil {
					d.logs.Warnf("webui", "", "reload credentials: %v", err)
					continue
				}
				if passwordChanged && d.web != nil {
					d.web.CloseAllSessions()
				}
				d.publishUI(uichanges.Access)
			}
		}
	}()
}
