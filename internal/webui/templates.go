package webui

const templates = `{{define "style"}}<style>
:root{color-scheme:light dark;--bg:#f6f7f9;--fg:#14181f;--muted:#5c6675;--card:#fff;--border:#dde2e9;--accent:#0a67d3;--ok:#137c3d;--warn:#9a5c00;--err:#b3261e}
@media(prefers-color-scheme:dark){:root{--bg:#0e1116;--fg:#e8ecf2;--muted:#9aa5b4;--card:#171c24;--border:#2a3341;--accent:#5aa2ff;--ok:#58d68d;--warn:#f0b35b;--err:#ff7b72}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif}
header{position:sticky;top:0;background:var(--card);border-bottom:1px solid var(--border);padding:10px 20px;display:flex;gap:18px;align-items:center;flex-wrap:wrap;z-index:5}
header .brand{font-weight:700;letter-spacing:.2px}header .brand span{color:var(--accent)}
nav{display:flex;gap:12px;flex-wrap:wrap}nav a{color:var(--muted);text-decoration:none;padding:4px 2px;border-bottom:2px solid transparent}
nav a.active{color:var(--fg);border-color:var(--accent)}nav a:hover{color:var(--fg)}
main{max-width:1080px;margin:0 auto;padding:24px 20px 60px}
h1{font-size:24px;margin:0 0 18px}h2{font-size:17px;margin:28px 0 10px}h3{font-size:15px;margin:18px 0 8px}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:14px}
.card{background:var(--card);border:1px solid var(--border);border-radius:10px;padding:16px}
.card .k{color:var(--muted);font-size:12px;text-transform:uppercase;letter-spacing:.06em}
.card .v{font-size:18px;font-weight:600;margin-top:4px;word-break:break-word}
table{width:100%;border-collapse:collapse;background:var(--card);border:1px solid var(--border);border-radius:10px;overflow:hidden}
th,td{text-align:left;padding:9px 12px;border-bottom:1px solid var(--border);vertical-align:top}th{font-size:12px;text-transform:uppercase;letter-spacing:.05em;color:var(--muted);font-weight:600}
tr:last-child td{border-bottom:0}
td form{border:0;padding:0;margin:0;background:none}td form button{margin:0}
td .row{gap:8px}
code,.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px}
pre{background:var(--card);border:1px solid var(--border);border-radius:10px;padding:12px;overflow:auto;font-size:12.5px;line-height:1.45;max-height:420px}
form{background:var(--card);border:1px solid var(--border);border-radius:10px;padding:16px;margin:10px 0}
label{display:block;margin:8px 0;color:var(--fg)}label small{color:var(--muted);display:block}
input[type=text],input[type=password],input[type=number],select{width:100%;max-width:420px;padding:8px 10px;border:1px solid var(--border);border-radius:7px;background:var(--bg);color:var(--fg)}
input[type=range]{width:100%;max-width:420px}
button{background:var(--accent);color:#fff;border:0;border-radius:7px;padding:8px 14px;font-weight:600;cursor:pointer;margin-top:6px}
button.secondary{background:transparent;color:var(--fg);border:1px solid var(--border)}
button.danger{background:var(--err)}
.muted{color:var(--muted)}.ok{color:var(--ok)}.warn{color:var(--warn)}.err{color:var(--err)}
.row{display:flex;gap:14px;flex-wrap:wrap;align-items:center}.spread{justify-content:space-between}
.flash{border-left:4px solid var(--accent);background:var(--card);padding:10px 14px;border-radius:8px;margin:0 0 16px}
.flash.err{border-color:var(--err)}
.pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:12px;border:1px solid var(--border);color:var(--muted)}
.pill.ok{color:var(--ok);border-color:var(--ok)}.pill.err{color:var(--err);border-color:var(--err)}
.light{display:inline-block;width:12px;height:12px;border-radius:50%;border:1px solid var(--border);margin-right:2px}
footer{max-width:1080px;margin:0 auto;padding:20px;color:var(--muted);font-size:12px}
.usernav{display:flex;gap:10px;align-items:center;margin-left:auto}.usernav form{background:none;border:0;padding:0;margin:0}.usernav button{margin:0;padding:4px 10px}
dl{display:grid;grid-template-columns:max-content 1fr;gap:6px 16px;margin:0}dt{color:var(--muted)}dd{margin:0;word-break:break-word}
.wakeword button:disabled{opacity:.55;cursor:not-allowed}.wakeword :focus-visible{outline:2px solid var(--accent);outline-offset:3px}
.wakeword progress:not([hidden]){display:block;width:100%;max-width:420px;height:10px;margin:10px 0;accent-color:var(--accent)}
.wakeword .record-status{margin:14px 0 0}.wakeword details{margin:14px 0}.wakeword summary{cursor:pointer}
.wakeword .table-scroll{overflow-x:auto}.wakeword td form{border:0;padding:0;margin:0;background:none}.wakeword td button{margin:0}
.wakeword [hidden]{display:none}.wakeword .setup-prompt{font-size:20px;font-weight:600;overflow-wrap:anywhere}.wakeword .setup-actions{margin:16px 0}
.days{display:flex;gap:10px;flex-wrap:wrap;align-items:center;margin:10px 0}.days label{display:inline-flex;gap:4px;align-items:center;margin:0}
</style>{{end}}


{{define "login"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Sign in · Capsula</title>
{{template "style" .}}</head><body><main style="max-width:460px;padding-top:80px">
<h1>Sign in</h1>
{{if .Error}}<div class="flash err">{{.Error}}</div>{{end}}
<form method="post" action="/login" autocomplete="on">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<input type="hidden" name="next" value="{{.Next}}">
<label>Administrator password
<input type="password" name="password" autofocus autocomplete="current-password" required></label>
<button>Sign in</button>
</form>
<p class="muted">This UI uses a self-signed TLS certificate; your browser will warn until you verify and trust the fingerprint shown by <code>capsula cert status</code>. Authentication is the local administrator password. API clients may instead send a static <code>X-API-Key</code> or <code>Authorization: Bearer</code> token.</p>
</main></body></html>{{end}}


{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Capsula</title>
{{template "style" .}}</head><body><header><div class="brand">Capsula<span>.</span></div><nav>
<a href="/" class="{{if eq .Nav "overview"}}active{{end}}">Overview</a>
<a href="/settings" class="{{if eq .Nav "settings"}}active{{end}}">Settings</a>
<a href="/wakeword" class="{{if eq .Nav "wakeword"}}active{{end}}">Wakeword</a>
<a href="/diagnostics" class="{{if eq .Nav "diagnostics"}}active{{end}}">Diagnostics</a>
<a href="/access" class="{{if eq .Nav "access"}}active{{end}}">Access</a>
</nav>{{if .User}}<div class="usernav"><span class="muted">{{.User}}</span><form method="post" action="/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="secondary">Sign out</button></form></div>{{end}}</header><main>{{end}}

{{define "foot"}}</main><footer>capsulad {{.Version}} · {{.Platform}}{{if .User}} · signed in as {{.User}}{{end}}</footer></body></html>{{end}}


{{define "flash"}}{{if .Message}}<div class="flash">{{.Message}}</div>{{end}}{{if .Error}}<div class="flash err">{{.Error}}</div>{{end}}{{end}}


{{define "overview"}}{{template "head" .}}{{template "flash" .}}
<h1>Overview</h1>
<div class="cards">
<div class="card"><div class="k">Device</div><div class="v">{{with .Data.Info}}{{.Model}}{{end}}</div><div class="muted mono">{{with .Data.Info}}{{.DeviceId}}{{end}}</div></div>
<div class="card"><div class="k">Bluetooth</div><div class="v">{{with .Data.Bluetooth}}{{if .Present}}{{if .Powered}}On{{else}}Off{{end}}{{else}}No adapter{{end}}{{end}}</div>
<div class="muted">{{with .Data.Bluetooth}}{{if .Present}}{{if .Discoverable}}discoverable{{else}}not discoverable{{end}}{{if .Pairable}} · pairable{{end}} · {{.Adapter}}{{else}}{{.Detail}}{{end}}{{end}}</div></div>
<div class="card"><div class="k">Microphone</div><div class="v">{{with .Data.Audio}}{{if .MicrophoneMuted}}<span class="err">Muted</span>{{else}}<span class="ok">Live</span>{{end}}{{end}}</div>
<div class="muted">{{with .Data.Audio}}{{if .MicrophoneMuteIsHardware}}hardware gate{{else}}software mute only{{end}}{{end}}</div></div>
<div class="card"><div class="k">Volume</div><div class="v">{{with .Data.Audio}}{{pct .MasterVolume}}%{{end}}</div>
<form method="post" action="/settings" class="row">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="group" value="volume"><input type="hidden" name="back" value="/">
<input type="range" name="master_volume" min="0" max="100" value="{{with .Data.Audio}}{{pct .MasterVolume}}{{end}}" oninput="this.nextElementSibling.value=this.value+'%'"><output></output>
<button>Set volume</button></form></div>
</div>

{{with .Data.Bluetooth}}{{if .Present}}
<h2>Bluetooth</h2>
<form method="post" action="/bluetooth" class="row">
<input type="hidden" name="csrf" value="{{$.CSRF}}">
<input type="hidden" name="back" value="/">
<input type="hidden" name="discoverable" value="{{if .Discoverable}}0{{else}}1{{end}}">
<div><strong>{{if .Discoverable}}Discoverable{{else}}Hidden{{end}}</strong>
<div class="muted">{{.Detail}}</div></div>
<button>{{if .Discoverable}}Stop advertising{{else}}Make discoverable{{end}}</button>
</form>
<form method="post" action="/bluetooth/pairable" class="row">
<input type="hidden" name="csrf" value="{{$.CSRF}}">
<input type="hidden" name="back" value="/">
<input type="hidden" name="pairable" value="{{if .Pairable}}0{{else}}1{{end}}">
<div><strong>{{if .Pairable}}Accepting new devices{{else}}Not accepting new devices{{end}}</strong>
<div class="muted">Paired devices reconnect while Bluetooth is on. This switch lets new devices pair while {{if .Discoverable}}discoverable{{else}}hidden{{end}}.</div></div>
<button>{{if .Pairable}}Block new pairings{{else}}Allow new pairings{{end}}</button>
</form>
<h3>Paired devices</h3>
{{if $.Data.BluetoothErr}}<p class="err">{{$.Data.BluetoothErr}}</p>{{end}}
{{if $.Data.Paired}}
<table>
<tr><th>Device</th><th>Address</th><th>State</th><th></th></tr>
{{range $.Data.Paired}}
<tr><td>{{.Name}}</td><td class="mono">{{.Address}}</td>
<td>{{if .Connected}}<span class="ok">connected</span>{{else}}<span class="muted">paired</span>{{end}}</td>
<td><form method="post" action="/bluetooth/unpair" class="row">
<input type="hidden" name="csrf" value="{{$.CSRF}}">
<input type="hidden" name="address" value="{{.Address}}">
<button class="secondary">Unpair</button></form></td></tr>
{{end}}
</table>
{{else}}<p class="muted">No paired devices.</p>{{end}}
{{end}}{{end}}

<h2>Now playing</h2>
{{if .Data.Providers}}{{range .Data.Providers}}
{{$prov := .}}<div class="card"><div class="k">{{.Name}} <span class="mono muted">{{.AppID}}</span></div>
{{if .Err}}<p class="err">{{.Err}}</p>{{end}}
{{range .Players}}
<div class="row spread"><div>
<strong>{{if .Title}}{{.Title}}{{else}}{{if .MediaName}}{{.MediaName}}{{else}}{{.SourceName}}{{end}}{{end}}</strong>
<div class="muted">{{if .Artist}}{{.Artist}}{{end}}{{if .Album}} · {{.Album}}{{end}} <span class="pill">{{.PlaybackState}}</span></div>
</div>
<form method="post" action="/media" class="row">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="app_id" value="{{$prov.AppID}}">
<input type="hidden" name="player_id" value="{{.PlayerId}}">
<button name="action" value="previous" class="secondary" formnovalidate>⏮</button>
<button name="action" value="play" class="secondary" formnovalidate>▶</button>
<button name="action" value="pause" class="secondary" formnovalidate>⏸</button>
<button name="action" value="next" class="secondary" formnovalidate>⏭</button>
</form></div>
{{else}}<p class="muted">No active player.</p>{{end}}
</div>{{end}}{{else}}<p class="muted">No application publishes the media contract.</p>{{end}}

<h2>Installed applications</h2>
<p class="muted">Built-in system apps are packaged with capsulad. They use the same application bus, lifecycle and settings surface as installed packages; only packaging and uninstall rules differ.</p>
<table><tr><th>Application</th><th>Version</th><th>Kind</th><th>State</th><th></th></tr>
{{range .Data.Apps}}{{$app := .}}
<tr><td><a href="/applications/{{.AppId}}">{{.DisplayName}}</a><div class="muted mono">{{.AppId}}</div></td>
<td class="mono">{{.Version}}</td>
<td>{{if .SystemApp}}<span class="pill ok">built-in</span>{{else}}<span class="pill">installed</span>{{end}}</td>
<td>{{range $.Data.Runs}}{{if eq .AppId $app.AppId}}<span class="pill ok">{{.State}}</span> {{end}}{{end}}</td>
<td><a href="/applications/{{.AppId}}">Open</a></td></tr>
{{else}}<tr><td colspan=5 class="muted">No applications installed.</td></tr>{{end}}
</table>
{{template "foot" .}}{{end}}

{{define "settings"}}{{template "head" .}}{{template "flash" .}}
<h1>Settings</h1>
<p class="muted">Changes apply immediately. Brightness "auto" follows the ambient-light sensor{{if not .Data.AmbientAvailable}} (no sensor on this device — auto falls back to the constant value){{end}}.</p>
<p class="muted">Built-in system apps: {{range .Data.Apps}}{{if .SystemApp}}<a href="/applications/{{.AppId}}">{{.DisplayName}}</a> {{end}}{{else}}none{{end}}</p>

{{with .Data.Display}}<h2>Segment display</h2>
<form method="post" action="/settings">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="group" value="display"><input type="hidden" name="back" value="/settings">
<input type="hidden" name="resource_id" value="{{$.Data.Display.ResourceId}}">
<input type="hidden" name="revision" value="{{$.Data.Settings.Revision}}">
<label>Default application
<select name="default_app">{{range $.Data.Apps}}<option value="{{.AppId}}" {{if eq .AppId $.Data.DisplayDefault}}selected{{end}}>{{.DisplayName}}{{if .SystemApp}} (built-in){{end}}</option>{{end}}
<option value="" {{if eq $.Data.DisplayDefault ""}}selected{{end}}>— none —</option></select></label>
<label>Brightness source
<select name="brightness_mode">
<option value="const" {{if not $.Data.DisplayAuto}}selected{{end}}>Constant</option>
<option value="auto" {{if $.Data.DisplayAuto}}selected{{end}}>Auto (light sensor)</option>
</select></label>
<label>Constant brightness: <strong>{{$.Data.DisplayPct}}%</strong>
<input type="range" name="brightness" min="0" max="100" value="{{$.Data.DisplayPct}}"></label>
<button>Save display settings</button>{{if $.Data.DisplayDefault}} <a href="/applications/{{$.Data.DisplayDefault}}">Configure {{$.Data.DisplayDefault}}</a>{{end}}</form>{{end}}

{{with .Data.Ring}}<h2>LED ring</h2>
<p class="muted">Hardware brightness follows the source selected here. The selected ring app owns effect, palette, effect brightness and speed; open its application page to change them.</p>
<form method="post" action="/settings">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="group" value="ring"><input type="hidden" name="back" value="/settings">
<input type="hidden" name="resource_id" value="{{$.Data.Ring.ResourceId}}">
<input type="hidden" name="revision" value="{{$.Data.Settings.Revision}}"><label>Default application
<select name="default_app">{{range $.Data.Apps}}<option value="{{.AppId}}" {{if eq .AppId $.Data.RingDefault}}selected{{end}}>{{.DisplayName}}{{if .SystemApp}} (built-in){{end}}</option>{{end}}
<option value="" {{if eq $.Data.RingDefault ""}}selected{{end}}>— none —</option></select></label>
<label>Brightness source
<select name="brightness_mode">
<option value="const" {{if not $.Data.RingAuto}}selected{{end}}>Constant</option>
<option value="auto" {{if $.Data.RingAuto}}selected{{end}}>Auto (light sensor)</option>
</select></label>
<label>Constant brightness: <strong>{{$.Data.RingPct}}%</strong>
<input type="range" name="brightness" min="0" max="100" value="{{$.Data.RingPct}}"></label>
<button>Save ring settings</button>{{if $.Data.RingDefault}} <a href="/applications/{{$.Data.RingDefault}}">Configure {{$.Data.RingDefault}}</a>{{end}}</form>{{end}}

<h2>Voice assistant</h2>
<form method="post" action="/settings">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="group" value="assistant"><input type="hidden" name="back" value="/settings">
<input type="hidden" name="revision" value="{{.Data.Settings.Revision}}">
<label>Default assistant
<select name="default_assistant_app">
<option value="" {{if eq .Data.Settings.Values.DefaultAssistantAppId ""}}selected{{end}}>— none —</option>
{{range .Data.Apps}}<option value="{{.AppId}}" {{if eq .AppId $.Data.Settings.Values.DefaultAssistantAppId}}selected{{end}}>{{.DisplayName}}{{if .SystemApp}} (built-in){{end}}</option>{{end}}</select></label>
<label><input type="checkbox" name="wakeword_enabled" {{if .Data.Settings.Values.WakewordEnabled}}checked{{end}}> Wakeword enabled</label>
<label>Wakeword model
<select name="wakeword_model">{{range .Data.Models}}<option value="{{.ModelId}}" {{if eq .ModelId $.Data.Settings.Values.WakewordModelId}}selected{{end}}>{{.DisplayName}} — {{if .Available}}available{{else}}unavailable{{end}}</option>{{end}}</select></label>
<label>Touch behaviour
<select name="touch_hold_mode">
<option value="summon" {{if eq (printf "%s" .Data.Settings.Values.TouchHoldMode) "TOUCH_HOLD_SUMMON"}}selected{{end}}>Hold to summon (conversation may outlive the hold)</option>
<option value="talk" {{if eq (printf "%s" .Data.Settings.Values.TouchHoldMode) "TOUCH_HOLD_TO_TALK"}}selected{{end}}>Hold to talk (release ends input)</option>
</select></label>
<button>Save assistant settings</button>{{if .Data.Settings.Values.DefaultAssistantAppId}} <a href="/applications/{{.Data.Settings.Values.DefaultAssistantAppId}}">Configure {{.Data.Settings.Values.DefaultAssistantAppId}}</a>{{end}}</form>

<h2>Microphone</h2>
<form method="post" action="/settings">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="group" value="microphone"><input type="hidden" name="back" value="/settings">
<input type="hidden" name="revision" value="{{.Data.Settings.Revision}}">
<label><input type="checkbox" name="microphone_muted" {{if .Data.Settings.Values.MicrophoneMuted}}checked{{end}}> Mute microphone globally</label>
{{with .Data.Settings.Values.MutedMicrophoneIndicator}}
<label>Hide the muted indication
<select name="indicator_mode">
<option value="never" {{if eq (printf "%s" .Mode) "MUTED_INDICATOR_UNTIL_UNMUTED"}}selected{{end}}>Never — keep it until unmuted</option>
<option value="delay" {{if eq (printf "%s" .Mode) "MUTED_INDICATOR_DELAY"}}selected{{end}}>After a delay</option>
<option value="immediate" {{if eq (printf "%s" .Mode) "MUTED_INDICATOR_IMMEDIATE"}}selected{{end}}>Immediately</option>
</select></label>
<label>Delay in seconds <input type="number" name="indicator_delay_s" min="1" max="3600" value="{{if .DelayMs}}{{secs .DelayMs}}{{else}}30{{end}}"></label>
{{end}}
<button>Save microphone settings</button></form>
<p class="muted">The indication never changes the mute state; app microphone capture still requires a visible indication.</p>

<h2>Device</h2>
<form method="post" action="/settings">
<input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="group" value="device"><input type="hidden" name="back" value="/settings">
<input type="hidden" name="revision" value="{{.Data.Settings.Revision}}">
<label>Timezone (device wall clock)
<input type="text" name="timezone" list="tz-zones" value="{{.Data.Settings.Values.Timezone}}" placeholder="empty follows the firmware's /etc/localtime">
<datalist id="tz-zones"><option value="UTC"><option value="Europe/Moscow"><option value="Asia/Shanghai"><option value="Europe/Berlin"><option value="America/New_York"></datalist></label>
<label>Master volume: <strong>{{pct .Data.Settings.Values.MasterVolume}}%</strong>
<input type="range" name="master_volume" min="0" max="100" value="{{pct .Data.Settings.Values.MasterVolume}}"></label>
<label><input type="checkbox" name="startup_sound_enabled" {{if .Data.Settings.Values.StartupSoundEnabled}}checked{{end}}> Play the startup sound on every boot</label>
<button>Save device settings</button></form>
{{template "foot" .}}{{end}}

{{define "access"}}{{template "head" .}}{{template "flash" .}}
<h1>Access</h1>
<h2>Administrator password</h2>
<form method="post" action="/access/password">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<label>Current password <input type="password" name="current" autocomplete="current-password" required></label>
<label>New password <input type="password" name="next" autocomplete="new-password" required></label>
<label>Confirm new password <input type="password" name="confirm" autocomplete="new-password" required></label>
<button>Change password</button>
</form>
<p class="muted">Changing the password revokes every browser session. Recovery is local-console only: <code>capsula auth set-password</code>.</p>

<h2>Static API keys</h2>
<form method="post" action="/access/keys">
<input type="hidden" name="csrf" value="{{.CSRF}}">
<label>New key name <input type="text" name="name" placeholder="automation" required></label>
<button>Create key</button>
</form>
{{if .Data.NewKey}}<div class="flash"><strong>{{.Data.NewKeyName}}</strong><pre>{{.Data.NewKey}}</pre>This value is shown once and stored only as a hash.</div>{{end}}
<table><tr><th>Name</th><th>Prefix</th><th>Created</th><th>State</th><th></th></tr>
{{range .Data.Keys}}<tr><td class="mono">{{.Name}}</td><td class="mono">{{.Prefix}}…</td><td class="mono">{{date .CreatedAt}}</td>
<td>{{if .Revoked}}<span class="pill err">revoked</span>{{else}}<span class="pill ok">active</span>{{end}}</td>
<td>{{if not .Revoked}}<form method="post" action="/access/keys/revoke"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="name" value="{{.Name}}"><button class="danger">Revoke</button></form>{{end}}</td></tr>
{{else}}<tr><td colspan=5 class="muted">No API keys.</td></tr>{{end}}
</table>
<p class="muted">API keys authenticate the HTTPS surface only. The local Unix-socket gRPC plane is filesystem-local and unaffected.</p>

<h2>TLS certificate</h2>
<div class="card">
<dl><dt>Fingerprint (SHA-256)</dt><dd class="mono">{{.Data.Status.ServerFingerprint}}</dd>
<dt>Valid until</dt><dd class="mono">{{date .Data.Status.ServerExpiry}}</dd>
<dt>Names</dt><dd class="mono">{{range .Data.Status.ServerDNSNames}}{{.}} {{end}}{{range .Data.Status.ServerIPAddresses}}{{.}} {{end}}</dd>
<dt>Listeners</dt><dd class="mono">{{range .Data.Status.ListenAddresses}}{{.}} {{end}}</dd></dl>
<p><a href="/access/cert.pem">Download public certificate (PEM)</a> — trust it in your TLS clients; it replaces the browser warning.</p>
<p class="muted">The certificate is self-signed. Renewal is a local-console operation (<code>capsula cert renew</code>) and needs a daemon restart.</p>
</div>
<p class="muted">Security diagnostics: password set={{.Data.Status.AdminPasswordSet}}, bootstrap complete={{.Data.Status.BootstrapComplete}}, active keys={{.Data.Status.ActiveAPIKeys}}.</p>
{{template "foot" .}}{{end}}

{{define "diagnostics"}}{{template "head" .}}{{template "flash" .}}
<h1>Diagnostics</h1>
<h2>Hardware, firmware, software</h2>
{{with .Data.Info}}<dl>
<dt>Model</dt><dd>{{.Model}}</dd><dt>SoC</dt><dd>{{.Soc}}</dd><dt>Board revision</dt><dd>{{.BoardRevision}}</dd>
<dt>Kernel</dt><dd class="mono">{{.Kernel}}</dd><dt>Firmware</dt><dd>{{.Firmware}}</dd>
<dt>Platform</dt><dd>{{.TargetPlatform}}</dd><dt>Device ID</dt><dd class="mono">{{.DeviceId}}</dd>
<dt>Daemon</dt><dd class="mono">{{.DaemonVersion}}</dd><dt>Boot ID</dt><dd class="mono">{{.BootId}}</dd>
</dl>
<table><tr><th>Subsystem</th><th>Backend</th><th>Status</th><th>Detail</th></tr>
{{range .SubsystemBackends}}<tr><td>{{.Subsystem}}</td><td class="mono">{{.BackendId}}</td>
<td>{{if eq (printf "%s" .Status) "BACKEND_AVAILABLE"}}<span class="pill ok">{{.Status}}</span>{{else if eq (printf "%s" .Status) "BACKEND_UNVERIFIED"}}<span class="pill err">{{.Status}}</span>{{else}}<span class="pill">{{.Status}}</span>{{end}}</td>
<td class="muted">{{.Detail}}</td></tr>{{end}}
</table>{{end}}

<h2>Running applications</h2>
<table><tr><th>App</th><th>Run</th><th>State</th><th>PID</th><th>Runtime</th><th>Sandbox</th><th></th></tr>
{{range .Data.Runs}}<tr><td class="mono">{{.AppId}}</td><td class="mono">{{.RunId}}</td><td>{{.State}}</td><td>{{.Pid}}</td>
<td>{{.Runtime}}</td><td class="muted">{{.StatusDetail}}</td>
<td><form method="post" action="/applications/{{.AppId}}/stop"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="run_id" value="{{.RunId}}"><button class="danger">Stop</button></form></td></tr>
{{else}}<tr><td colspan=7 class="muted">No supervised applications are running.</td></tr>{{end}}
</table>

<h2>Resources and routing</h2>
<table><tr><th>Resource</th><th>Kind</th><th>Routing</th><th>Available</th><th>Selected app</th></tr>
{{range .Data.Resources}}{{$id := .ResourceId}}<tr><td class="mono">{{.ResourceId}}</td><td>{{.Kind}}</td><td>{{.Routing}}</td>
<td>{{if .Available}}<span class="pill ok">yes</span>{{else}}<span class="pill err">no</span>{{end}}</td>
<td class="mono">{{with index $.Data.Routing $id}}{{.SelectedAppId}}{{end}}</td></tr>{{end}}
</table>

<h2>Logs</h2>
<pre>{{range .Data.Logs}}{{.FormatLine}}
{{else}}No log records yet.{{end}}</pre>
{{template "foot" .}}{{end}}

{{define "wakeword"}}{{template "head" .}}{{template "flash" .}}
<h1>Wakeword</h1>
<div class="wakeword">
<p class="muted">Set up a personal wakeword using Capsula's microphone, not your phone or computer. Audio stays on the device. Keep this tab open until setup finishes.</p>
<p id="stepprogress" class="pill" role="status">Step 1 of 5</p>
<h2 id="steptitle" tabindex="-1">Name your wakeword and position Capsula</h2>

<form id="setupform">
<label>Which word should wake Capsula?
<input type="text" id="wordname" placeholder="For example, Маруся" maxlength="100" required aria-describedby="wordhelp"></label>
<p id="wordhelp" class="muted">Choose a distinctive word you can say naturally. Use the same word throughout setup.</p>
<p>Put the speaker where you will normally use it. Leave it there for all recordings, and stand where you would usually speak to it.</p>
<label><input type="checkbox" id="placement" required> Capsula is in its usual place and its microphone is unmuted.</label>
<button>Continue to room noise</button>
</form>

<form id="recordform" method="post" action="/wakeword/record" hidden>
<input type="hidden" name="csrf" value="{{.CSRF}}">
<input type="hidden" name="label" id="recordlabel" value="background">
<input type="hidden" name="duration_ms" id="recordlength" value="8000">
<p id="recordhelp"></p>
<p id="recordprompt" class="setup-prompt"></p>
<p id="takeprogress" class="muted"></p>
<div class="row"><button id="recordbtn" type="submit">Record room noise</button><button type="button" id="retrybtn" class="secondary" hidden>Retry last take</button><button type="button" id="cancelrecord" class="secondary" hidden>Cancel countdown</button></div>
<div class="record-status" role="status" aria-live="polite" aria-atomic="true"><strong id="recordstatus">Ready when you are. Recording starts after a three-second countdown.</strong></div>
<progress id="recordprogress" max="100" value="0" aria-label="Recording progress" hidden></progress>
</form>
<p id="recorderror" class="err" role="alert" hidden></p>

<section id="trainstep" hidden>
<p>Training runs on Capsula against the embedding model. The browser sends only the sample IDs of the takes you selected; no audio or feature data leaves the device. Whole recordings are held out for threshold tuning and a separate final test, and a model is installed only after the word-recall and false-activation gates pass.</p>
<div class="cards">
<div class="card"><div class="k">Room noise</div><div id="quietcount" class="v">0 / 3</div></div>
<div class="card"><div class="k">Unrelated phrases</div><div id="phrasecount" class="v">0 / 4</div></div>
<div class="card"><div class="k">Wakeword takes</div><div id="wordcount" class="v">0 / 8</div></div>
</div>
<p id="trainhelp" class="muted" role="status" aria-live="polite">Complete all recording steps before training.</p>
<p class="muted">The device also needs the license-clean negative corpus at <span class="mono">wakeword/negatives.bin</span>. If it is missing, training stops with an error and the previously installed model stays unchanged.</p>
<p class="warn">This is a personal, experimental model. Installation requires at least 80% recall across held-out wakeword takes and zero false activations on held-out room and unrelated-phrase takes at the 80 ms, two-consecutive-window policy. A few seconds of test audio cannot establish a low live false-activation rate: test on your speaker afterward.</p>
<input type="hidden" id="csrftoken" value="{{.CSRF}}">
<div class="row"><button id="trainbtn" aria-describedby="trainhelp" disabled>Train and install</button><button type="button" id="cancelbtn" class="secondary" hidden>Cancel training</button></div>
<div class="record-status" role="status" aria-live="polite" aria-atomic="true"><strong id="trainstatus">Ready.</strong></div>
<progress id="trainprogress" max="100" value="0" aria-label="Training progress" hidden></progress>
<p id="trainerror" class="err" role="alert" hidden></p>
<details id="trainingdetails" open><summary>Training details</summary><pre id="trainlog">Ready.</pre></details>
</section>
<div class="row setup-actions">
<button type="button" id="backstep" class="secondary" hidden>Back</button>
<button type="button" id="restartsetup" class="secondary" aria-describedby="restarthelp" hidden>Start a new setup</button>
</div>
<p id="restarthelp" class="muted" hidden>Starting a new setup keeps saved recordings, but does not use them for the new word.</p>
<noscript><p class="warn">Enable JavaScript to use guided recording and on-device training.</p></noscript>

<details id="recordings"><summary>Manage recordings <span id="samplecount" class="pill">{{len .Data.Samples}}</span></summary>
<p class="muted">Delete a take if you misspoke. Only takes selected in the current setup are trained; older recordings stay saved but are never used implicitly.</p>
<div class="table-scroll"><table><thead><tr><th>Recorded</th><th>Type</th><th>Length</th><th></th></tr></thead><tbody id="samplelist">
{{range .Data.Samples}}<tr data-sample-id="{{.SampleId}}" data-label="{{.Label}}" data-duration-ms="{{.DurationMs}}"><td><time datetime="{{msdate .RecordedUnixMs}}">{{msdate .RecordedUnixMs}}</time></td><td>{{if eq .Label "word"}}Wakeword{{else}}Background{{end}}</td><td>{{.DurationMs}} ms</td>
<td><form method="post" action="/wakeword/delete"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="sample_id" value="{{.SampleId}}"><button class="secondary" aria-label="Delete {{.Label}} recording from {{msdate .RecordedUnixMs}}">Delete</button></form></td></tr>
{{else}}<tr id="nosamples"><td colspan="4" class="muted">No recordings yet. Follow the setup steps above.</td></tr>{{end}}
</tbody></table></div></details>

<h2>Installed model</h2>
<div class="table-scroll"><table><tr><th>Model</th><th>Language</th><th>Engine</th><th>State</th></tr>
{{range .Data.Models}}<tr><td class="mono">{{.ModelId}}</td><td>{{.Language}}</td><td class="mono">{{.EngineId}}</td>
<td>{{if .Available}}<span class="pill ok">available</span>{{else}}<span class="pill err">unavailable</span>{{end}}</td></tr>
{{else}}<tr><td colspan=4 class="muted">No keyword model installed. Record takes above to create one.</td></tr>{{end}}
</table></div>
<p class="muted">After installing, choose your model and enable wakeword detection in <a href="/settings">Settings</a>.</p>
</div>
<script src="/wakeword/trainer.js"></script>
{{template "foot" .}}{{end}}
{{define "application"}}{{template "head" .}}{{template "flash" .}}
{{$app := .Data.App}}<h1>{{$app.DisplayName}}</h1>
<p class="muted mono">{{$app.AppId}} · v{{$app.Version}} · {{$app.Entrypoint.Runtime}} {{$app.Entrypoint.CpuArchitecture}} {{$app.Entrypoint.BinaryAbi}}</p>
{{if $app.SystemApp}}<p><span class="pill ok">built-in system app</span> <span class="muted">Packaged with capsulad and managed through the normal application bus, lifecycle and settings. It has no external package to uninstall; stop or restart it with the controls below.</span></p>{{end}}
<div class="cards">
<div class="card"><div class="k">Lifecycle</div><div class="v">{{if $app.Background}}background{{else}}on demand{{end}}</div><div class="muted">{{if $app.SingleInstance}}single instance{{else}}multi instance{{end}} · {{if $app.StartOnBoot}}starts at boot{{end}} {{if $app.RestartOnFailure}}restart on failure{{end}}</div></div>
<div class="card"><div class="k">Provides</div><div class="v mono">{{range $app.Provides}}{{.}} {{else}}—{{end}}</div></div>
{{if $app.SystemApp}}<div class="card"><div class="k">Packaging</div><div class="v">Built into capsulad</div><div class="muted mono">no package directory</div></div>{{else}}<div class="card"><div class="k">Entrypoint</div><div class="v mono">{{$app.Entrypoint.RelativePath}}</div><div class="muted mono">sdk {{$app.Entrypoint.FrameworkSdkVersion}}</div></div>{{end}}
</div>

{{with .Data.ClockService}}<h2 id="clock">Clock controls</h2>
<p class="muted">These controls call the application's published clock service over the application bus; every value shown is the app's state. <a href="/applications/{{$app.AppId}}">Refresh</a></p>
{{if $.Data.ClockErr}}<div class="flash err">Clock service: {{$.Data.ClockErr}}</div>
{{else}}{{with $.Data.Clock}}
<div class="cards">
<div class="card"><div class="k">Local time</div><div class="v mono">{{.LocalTimeRFC3339}}</div><div class="muted">{{if .Timezone}}{{.Timezone}}{{else}}device timezone{{end}}</div></div>
{{with .Stopwatch}}<div class="card"><div class="k">Stopwatch</div><div class="v mono">{{clockDur .ElapsedMS}}</div>
<div class="muted"><span class="pill">{{clockStatus .State}}</span></div>
<div class="row">
{{if eq .State "STOPWATCH_STATUS_IDLE"}}
<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="stopwatch_start"><button>Start</button></form>
{{else if eq .State "STOPWATCH_STATUS_RUNNING"}}
<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="stopwatch_pause"><button>Pause</button></form>
<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="stopwatch_reset"><button class="secondary">Reset</button></form>
{{else}}
<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="stopwatch_resume"><button>Resume</button></form>
<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="stopwatch_reset"><button class="secondary">Reset</button></form>
{{end}}
</div></div>{{end}}
</div>

<h3>Timers</h3>
<form method="post" action="/applications/{{$app.AppId}}/clock">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="timer_start">
<div class="row"><label>Duration (seconds)<input type="number" name="duration_s" min="1" max="2592000" value="300" required></label>
<label>Label (optional)<input type="text" name="label" maxlength="64"></label></div>
<button>Start timer</button></form>
{{if .Timers}}<table><tr><th>Timer</th><th>Remaining</th><th>Status</th><th>Started</th><th></th></tr>
{{range .Timers}}<tr><td>{{if .Label}}{{.Label}}{{else}}<span class="muted">{{.TimerID}}</span>{{end}}</td>
<td class="mono">{{clockDur .RemainingMS}}</td><td><span class="pill">{{clockStatus .Status}}</span></td>
<td class="mono">{{if .StartedUnixMS}}{{.StartedUnixMS}}{{else}}—{{end}}</td>
<td>{{if eq .Status "TIMER_STATUS_RUNNING"}}<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="timer_cancel"><input type="hidden" name="timer_id" value="{{.TimerID}}"><button class="secondary">Cancel</button></form>{{end}}</td></tr>
{{end}}</table>{{else}}<p class="muted">No timers.</p>{{end}}

<h3>Alarms</h3>
{{$form := $.Data.AlarmForm}}
<form method="post" action="/applications/{{$app.AppId}}/clock">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="alarm_set">
<input type="hidden" name="alarm_id" value="{{$form.AlarmID}}">
<label>Alarm label<input type="text" name="label" maxlength="64" value="{{$form.Label}}"></label>
<div class="row"><label>Hour<input type="number" name="hour" min="0" max="23" value="{{$form.Hour}}" required></label>
<label>Minute<input type="number" name="minute" min="0" max="59" value="{{$form.Minute}}" required></label>
<label>Alarm timezone<input type="text" name="alarm_timezone" value="{{$form.Timezone}}" placeholder="empty follows the device timezone" list="tz-zones-clock"></label></div>
<datalist id="tz-zones-clock"><option value="UTC"><option value="Europe/Moscow"><option value="Asia/Shanghai"><option value="Europe/Berlin"><option value="America/New_York"></datalist>
<div class="days"><span class="muted">Repeat</span>
<label><input type="checkbox" name="day_0" value="1" {{if clockHasDay $form.Days 0}}checked{{end}}> Mon</label>
<label><input type="checkbox" name="day_1" value="1" {{if clockHasDay $form.Days 1}}checked{{end}}> Tue</label>
<label><input type="checkbox" name="day_2" value="1" {{if clockHasDay $form.Days 2}}checked{{end}}> Wed</label>
<label><input type="checkbox" name="day_3" value="1" {{if clockHasDay $form.Days 3}}checked{{end}}> Thu</label>
<label><input type="checkbox" name="day_4" value="1" {{if clockHasDay $form.Days 4}}checked{{end}}> Fri</label>
<label><input type="checkbox" name="day_5" value="1" {{if clockHasDay $form.Days 5}}checked{{end}}> Sat</label>
<label><input type="checkbox" name="day_6" value="1" {{if clockHasDay $form.Days 6}}checked{{end}}> Sun</label></div>
<p class="muted">No days selected means one-shot: the alarm fires at its next occurrence and then disables itself.</p>
<label><input type="checkbox" name="enabled" {{if $form.Enabled}}checked{{end}}> Enabled</label>
<button>{{if $form.AlarmID}}Save alarm{{else}}Add alarm{{end}}</button>
{{if $form.AlarmID}}<a href="/applications/{{$app.AppId}}">Cancel edit</a>{{end}}
</form>
{{if .Alarms}}<table><tr><th>Alarm</th><th>Time</th><th>Repeat</th><th>Timezone</th><th>Next trigger</th><th>State</th><th></th></tr>
{{range .Alarms}}<tr><td>{{if .Label}}{{.Label}}{{else}}<span class="muted">{{.AlarmID}}</span>{{end}}</td>
<td class="mono">{{printf "%02d" .Hour}}:{{printf "%02d" .Minute}}</td><td>{{clockDays .DaysOfWeek}}</td>
<td class="mono">{{if .EffectiveTimezone}}{{.EffectiveTimezone}}{{else}}device{{end}}</td>
<td class="mono">{{if .NextTriggerRFC3339}}{{.NextTriggerRFC3339}}{{else}}—{{end}}</td>
<td>{{if .Enabled}}<span class="pill ok">enabled</span>{{else}}<span class="pill">disabled</span>{{end}}</td>
<td><div class="row">
<a href="/applications/{{$app.AppId}}?edit={{.AlarmID}}">Edit</a>
{{if .Enabled}}<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="alarm_disable"><input type="hidden" name="alarm_id" value="{{.AlarmID}}"><button class="secondary">Disable</button></form>
{{else}}<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="alarm_enable"><input type="hidden" name="alarm_id" value="{{.AlarmID}}"><button class="secondary">Enable</button></form>{{end}}
<form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="alarm_delete"><input type="hidden" name="alarm_id" value="{{.AlarmID}}"><button class="danger">Delete</button></form>
</div></td></tr>
{{end}}</table>{{else}}<p class="muted">No alarms.</p>{{end}}

{{if .ActiveAlerts}}<h3>Active alerts</h3>
<table><tr><th>Kind</th><th>Label</th><th>Source</th><th></th></tr>
{{range .ActiveAlerts}}<tr><td><span class="pill err">{{clockStatus .Kind}}</span></td><td>{{.Label}}</td><td class="mono">{{.SourceID}}</td>
<td><form method="post" action="/applications/{{$app.AppId}}/clock"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action" value="alert_dismiss"><input type="hidden" name="alert_id" value="{{.AlertID}}"><button class="secondary">Dismiss</button></form></td></tr>
{{end}}</table>{{end}}
{{end}}{{end}}{{end}}

{{with .Data.RingService}}<h2>Ring output</h2>
{{if $.Data.RingErr}}<div class="flash err">Ring service: {{$.Data.RingErr}}</div>
{{else}}{{with $.Data.Ring}}
<div class="cards">
<div class="card"><div class="k">Effect</div><div class="v">{{.Effect}}</div></div>
<div class="card"><div class="k">Palette</div><div class="v">{{.Palette}}</div></div>
<div class="card"><div class="k">Brightness</div><div class="v">{{printf "%.2f" .Brightness}}</div></div>
<div class="card"><div class="k">Speed</div><div class="v">{{printf "%.2f" .Speed}}</div></div>
<div class="card"><div class="k">Baseline</div><div class="v">{{with .Active}}{{if .}}on{{else}}off by schedule{{end}}{{else}}unknown{{end}}</div></div>
</div>
<p class="muted">These are the values the running ring app is rendering. Change effect, palette, brightness, speed or the active window in <a href="#configuration">Configuration</a> below; the app applies them to its normal ring presentation. Outside a configured active window the built-in ring app's baseline is explicitly blank, while volume, mute and alert indications still use the ring.</p>
{{end}}{{end}}{{end}}

{{if not $app.SystemApp}}{{with .Data.Provenance}}<h2>Build provenance</h2>
<div class="card"><div class="k">{{.Status}}</div><p>{{.Detail}}</p>
<dl><dt>Subject SHA-256</dt><dd class="mono">{{.SubjectSha256Hex}}</dd>
<dt>Repository</dt><dd class="mono">{{.VerifiedRepository}}</dd>
<dt>Workflow</dt><dd class="mono">{{.VerifiedWorkflow}}</dd>
<dt>Commit</dt><dd class="mono">{{.VerifiedCommitSha}}</dd></dl>
<p class="muted">Verified provenance attests build origin and integrity under the configured trust policy. It is not a promise that the software is safe.</p></div>{{end}}{{end}}

<h2>Runs</h2>
<table><tr><th>Run</th><th>State</th><th>PID</th><th>Started</th><th>Detail</th><th></th></tr>
{{range .Data.Runs}}<tr><td class="mono">{{.RunId}}</td><td>{{.State}}</td><td>{{.Pid}}</td><td class="mono">{{.StartedMonotonicNs}}</td><td class="muted">{{.StatusDetail}}</td>
<td>{{if $app.CanStop}}<form method="post" action="/applications/{{$app.AppId}}/stop"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="run_id" value="{{.RunId}}"><button class="danger">Stop</button></form>{{else}}<span class="muted">server-managed</span>{{end}}</td></tr>
{{else}}<tr><td colspan=6 class="muted">Not running.</td></tr>{{end}}
</table>

<h2>Launch</h2>
<form method="post" action="/applications/{{$app.AppId}}/launch">
<input type="hidden" name="csrf" value="{{.CSRF}}">
{{range .Data.Args}}<label>{{.Name}} <small>{{.Description}}{{if .Required}} (required){{end}}</small>
<input type="text" name="arg_{{.Name}}" value=""></label>{{else}}<p class="muted">This app declares no launch arguments.</p>{{end}}
<button>Launch / send invocation</button>
</form>

{{with .Data.Schema}}{{if or .Settings .Actions}}<h2 id="configuration">Configuration</h2>
<form method="post" action="/applications/{{$app.AppId}}/settings">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="revision" value="{{$.Data.Snapshot.Revision}}">
{{range .Settings}}{{$def := .}}{{$state := index $.Data.Values .Key}}
<label>{{.Label}}{{if .Description}} <small>{{.Description}}</small>{{end}}{{if .RestartRequired}} <small>Restart the application to apply.</small>{{end}}
{{if eq (printf "%s" .Type) "SETTING_TEXT_MASKED"}}
<input type="password" name="set_{{.Key}}" value="" placeholder="{{if $state}}{{if $state.HasValue}}configured — leave empty to keep{{else}}not set{{end}}{{end}}" autocomplete="new-password">
<small><input type="checkbox" name="clear_{{.Key}}" value="1"> Clear the stored value</small>
{{else if eq (printf "%s" .Type) "SETTING_BOOLEAN"}}
<input type="hidden" name="present_{{.Key}}" value="1">
<span><input type="checkbox" name="set_{{.Key}}" {{if and $state (eq (unq $state.ValueJson) "true")}}checked{{end}}> enabled</span>
{{else if eq (printf "%s" .Type) "SETTING_SELECT"}}
<select name="set_{{.Key}}">{{range .Choices}}<option value="{{.Value}}" {{if and $state (eq (unq $state.ValueJson) .Value)}}selected{{end}}>{{.Label}}</option>{{end}}</select>
{{else if or (eq (printf "%s" .Type) "SETTING_INTEGER") (eq (printf "%s" .Type) "SETTING_NUMBER")}}
<input type="number" name="set_{{.Key}}" value="{{if $state}}{{unq $state.ValueJson}}{{end}}" {{if .HasMin}}min="{{.Min}}"{{end}} {{if .HasMax}}max="{{.Max}}"{{end}} step="{{if eq (printf "%s" .Type) "SETTING_INTEGER"}}1{{else}}any{{end}}">
{{else}}
<input type="text" name="set_{{.Key}}" value="{{if $state}}{{unq $state.ValueJson}}{{end}}">
{{end}}{{end}}
<button>Save configuration</button>
</form>
{{range .Actions}}<form method="post" action="/applications/{{$app.AppId}}/action" class="row">
<input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="action_id" value="{{.ActionId}}">
<div><strong>{{.Label}}</strong>{{if .Description}}<div class="muted">{{.Description}}</div>{{end}}</div>
<button class="secondary">{{.Label}}</button></form>{{end}}
{{end}}{{end}}

{{if .Data.Interfaces}}<h2>Published interfaces</h2>
{{range .Data.Interfaces}}{{$iface := .}}
<div class="card"><div class="k">{{.GrpcService}} · v{{.InterfaceVersion}}</div><p>{{.Summary}}</p>
{{with index $.Data.Described .GrpcService}}<p class="muted">{{.DocumentationMarkdown}}</p>
<table><tr><th>Method</th><th>Summary</th><th>Request</th><th>Response</th></tr>
{{range .Methods}}<tr><td class="mono">{{.RpcName}}</td><td>{{.Summary}}</td><td class="mono">{{.RequestExampleJson}}</td><td class="mono">{{.ResponseExampleJson}}</td></tr>{{end}}
</table>{{end}}</div>{{end}}{{end}}

<h2>Logs</h2><pre>{{range .Data.Logs}}{{.FormatLine}}
{{else}}No log records yet.{{end}}</pre>
{{template "foot" .}}{{end}}

`
