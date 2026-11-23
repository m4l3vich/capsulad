package ui

import "capsulad/installer/internal/device"

// lang is the interface language. The zero value is the default.
type lang int

const (
	langEN lang = iota
	langRU
)

// catalog holds every user-visible string the interface owns. Keeping it a
// struct makes a missing translation a compile error rather than a runtime
// fallback. Format strings are rendered with fmt.Sprintf.
type catalog struct {
	// Steps and chrome.
	stepFormat         string
	stepLanguageDevice string
	stepFirmware       string
	stepAddress        string
	stepOptions        string
	stepSummary        string
	stepPassword       string
	stepWiFi           string
	stepApps           string
	stepMarusia        string
	stepWorking        string
	stepComplete       string
	stepStopped        string
	stepHostKey        string
	stepAbort          string

	hintDevice         string
	hintFirmware       string
	hintAddressStock   string
	hintAddressAlpine  string
	hintForm           string
	hintFormEditing    string
	hintSummary        string
	hintWorking        string
	hintPassword       string
	hintChangePassword string
	hintWiFi           string
	hintWiFiPass       string
	hintWiFiManual     string
	hintApps           string
	hintMarusia        string
	hintComplete       string
	hintError          string
	hintHostKey        string
	hintAbort          string
	hintScroll         string

	// Step 1.
	deviceHeading            string
	languageLabel            string
	langEnglish              string
	langRussian              string
	deviceLabel              string
	deviceOriginal           string
	deviceMini               string
	supported                string
	notSupported             string
	originalNote             string
	miniNote                 string
	statusCapsulaUnsupported string

	// Step 2.
	firmwareHeading    string
	firmwareStock      string
	firmwareStockNote  string
	firmwareAlpine     string
	firmwareAlpineNote string

	// Step 3.
	addressHeading       string
	addressLabel         string
	sshUserLabel         string
	sshPasswordLabel     string
	keyLabel             string
	addrPlaceholder      string
	sshUserPlaceholder   string
	sshPassPlaceholder   string
	keyPlaceholder       string
	adminPassPlaceholder string
	wifiSSIDPlaceholder  string
	wifiPassPlaceholder  string
	tokenPlaceholder     string
	addressStockNote     string
	addressAlpineNote    string
	statusAddressEmpty   string
	statusAlpineCreds    string

	// Step 4: installation options.
	optionsHeading         string
	passwordHeading        string
	passGenerate           string
	passCustom             string
	passKeep               string
	passRecommended        string
	passCustomHint         string
	passwordCaptured       string
	optJingle              string
	optIdentity            string
	optForce               string
	formContinue           string
	statusIdentityRequired string
	statusSpaceRequired    string
	statusCustomRequired   string
	onOn                   string
	onOff                  string
	statusReenterAddress   string
	optLabelWidth          int
	rowLabelWidth          int

	// Step 5: installation summary.
	installSummaryHeading string
	labelDevice           string
	labelIdentity         string
	labelBackup           string
	labelSpace            string
	noStableSerialNote    string
	backupNotNeeded       string
	backupDetailFormat    string
	spaceFormat           string
	spaceStagedFormat     string
	spaceShortFormat      string
	spaceIdleAppsFormat   string
	spaceDeployerNote     string
	installAction         string
	installActionHint     string
	deployDetachedNote    string

	// Working state.
	opPrepare        string
	opDeploy         string
	opWiFiStatus     string
	opWiFiConnect    string
	opApps           string
	opMarusia        string
	opChangePassword string
	opWorking        string
	connectingWiFi   string

	// Administrator password screens.
	passwordScreenHeading string
	keepingPassword       string
	recoverPassword       string
	onlyTimeShown         string
	copyRequested         string
	storedPassword        string
	statusConfirmStored   string
	changePasswordHeading string
	changePasswordNote    string

	// Wi-Fi.
	wifiHeading              string
	wifiStateLabel           string
	wifiConnectedLabel       string
	wifiSetupAPLabel         string
	wifiPortalLabel          string
	wifiAPPasswordLabel      string
	wifiUnknown              string
	wifiNoScan               string
	wifiManual               string
	wifiJoinFormat           string
	wifiPassphraseLabel      string
	wifiPassphraseNote       string
	wifiManualHeading        string
	wifiSSIDLabel            string
	wifiOpenNetwork          string
	statusNoNetwork          string
	statusEnterSSID          string
	securityOpen             string
	signalExcellent          string
	signalGood               string
	signalFair               string
	signalWeak               string
	logWiFiSkipped           string
	logAcceptDebugging       string
	logNoStableSerial        string
	logKeepingPassword       string
	logWiFiConnectedFormat   string
	logWiFiUnavailableFormat string
	logNoScanManual          string
	logScanFailedFormat      string

	// Apps.
	appsHeading        string
	appsSelectedFormat string
	appsReleaseFormat  string
	appsNoneNote       string
	appDescriptions    map[string]string

	// Marusia.
	marusiaHeading   string
	marusiaNoSession string
	marusiaPaste     string

	// Completion.
	completeHeading      string
	finishedWithErrors   string
	labelWebUI           string
	labelCertFingerprint string
	labelDownload        string
	labelVerify          string
	labelAdmin           string
	labelWiFi            string
	labelMarusia         string
	labelRollback        string
	adminSet             string
	adminKept            string
	wifiSkipped          string
	trustCertNote        string
	marusiaMigrated      string
	marusiaTokenSet      string
	marusiaNoToken       string
	appsSectionHeading   string
	nothingDeleted       string

	// Error screen.
	errorHeading          string
	whatNextHeading       string
	detailsHeading        string
	actionRetry           string
	actionChangeAddress   string
	actionBack            string
	actionBackApps        string
	actionSkipSummary     string
	actionSkipWiFi        string
	actionQuit            string
	deployPartialNote     string
	deployLogHeading      string
	recoveryAddressFormat string
	recoveryWait          string
	recoveryNetworkFormat string
	recoveryHintFormat    string
	recoveryPasswordNote  string
	recoveryPortalFormat  string
	recoveryRetry         string
	appsErrorNote         string
	noDeployNote          string

	// SSH host key.
	hostKeyHeading       string
	hostKeyWaiting       string
	hostLabel            string
	fingerprintLabel     string
	hostKeyTOFU          string
	hostKeyMismatch      string
	trustContinue        string
	refuse               string
	statusHostKeyPinned  string
	statusHostKeyRefused string

	// Stop dialog.
	abortHeading             string
	abortCurrentStepFormat   string
	abortDeployLine1         string
	abortDeployLine2         string
	abortOtherLine1          string
	abortOtherLine2          string
	abortCheckLog            string
	abortKeepWaiting         string
	abortStopQuit            string
	statusFinishedBeforeStop string
	abortStepFallback        string

	// Localized validation messages for install's validators.
	errPasswordShortFormat  string
	errPasswordNewline      string
	errSSIDEmpty            string
	errSSIDStartsDashFormat string
	errSSIDControl          string

	// Platform display names.
	platformLabels map[device.Platform]string
}

func catalogFor(l lang) *catalog {
	if l == langRU {
		return &ruCatalog
	}
	return &enCatalog
}

var enCatalog = catalog{
	stepFormat:         "Step %d · %s",
	stepLanguageDevice: "Language & device",
	stepFirmware:       "Firmware",
	stepAddress:        "Address",
	stepOptions:        "Installation options",
	stepSummary:        "Installation summary",
	stepPassword:       "Admin password",
	stepWiFi:           "Wi-Fi",
	stepApps:           "Apps",
	stepMarusia:        "Marusia",
	stepWorking:        "Working",
	stepComplete:       "Complete",
	stepStopped:        "Stopped",
	stepHostKey:        "SSH host key",
	stepAbort:          "Confirm stop",

	hintDevice:         "↑/↓ select · enter choose/continue · ctrl+c quit",
	hintFirmware:       "↑/↓ select · enter continue · esc back · ctrl+c quit",
	hintAddressStock:   "enter connect · esc back",
	hintAddressAlpine:  "tab/shift+tab fields · enter connect · esc back",
	hintForm:           "↑/↓ move · enter/space change · esc back",
	hintFormEditing:    "enter capture · esc cancel",
	hintSummary:        "enter install · esc back",
	hintWorking:        "pgup/pgdn log · esc stop · ctrl+c stop",
	hintPassword:       "enter continue · space confirm · c copy · n change",
	hintChangePassword: "enter set password · esc back",
	hintWiFi:           "enter join · ↑/↓ select · m manual · r rescan · s skip · esc back",
	hintWiFiPass:       "enter join · esc back",
	hintWiFiManual:     "enter continue · ctrl+o open network · esc back",
	hintApps:           "enter install · space toggle · a all · n none · s skip · esc back",
	hintMarusia:        "enter save token · esc skip",
	hintComplete:       "enter/q quit",
	hintError:          "↑/↓ choose · enter run · q quit",
	hintHostKey:        "y trust · n refuse",
	hintAbort:          "↑/↓ choose · enter confirm · ctrl+c stop now",
	hintScroll:         "↕ pgup/pgdn",

	deviceHeading:            "Select your language and device",
	languageLabel:            "Language",
	langEnglish:              "English (UK)",
	langRussian:              "Русский",
	deviceLabel:              "Device",
	deviceOriginal:           "Capsula (fat)",
	deviceMini:               "Capsula Mini",
	supported:                "soon",
	notSupported:             "not supported",
	originalNote:             "For now capsulad does not support the fat Capsula.",
	miniNote:                 "Jailbroken Rockchip or Amlogic with Alpine Linux or stock software.",
	statusCapsulaUnsupported: "The original Capsula is not supported by capsulad.",

	firmwareHeading:    "What software is your device running?",
	firmwareStock:      "Jailbroken stock (capsula-jb)",
	firmwareStockNote:  "Connects over ADB on port 5555.",
	firmwareAlpine:     "Alpine Linux",
	firmwareAlpineNote: "Connects over SSH as root.",

	addressHeading:       "Device address",
	addressLabel:         "Address",
	sshUserLabel:         "SSH user",
	sshPasswordLabel:     "SSH password",
	keyLabel:             "Private key (optional)",
	addrPlaceholder:      "device address, e.g. 192.168.1.42",
	sshUserPlaceholder:   "root",
	sshPassPlaceholder:   "SSH password (hidden)",
	keyPlaceholder:       "SSH private key path (optional)",
	adminPassPlaceholder: "admin password (hidden)",
	wifiSSIDPlaceholder:  "Wi-Fi network name",
	wifiPassPlaceholder:  "Wi-Fi passphrase (hidden)",
	tokenPlaceholder:     "VK access token (hidden, optional)",
	addressStockNote:     "ADB connects to <address>:5555.",
	addressAlpineNote:    "A password or a private key is required. The host key is pinned on first use.",
	statusAddressEmpty:   "Enter the device address first.",
	statusAlpineCreds:    "Alpine needs an SSH password or a private key.",

	optionsHeading:         "Installation options",
	passwordHeading:        "Administrator password",
	passGenerate:           "Generate a strong password",
	passCustom:             "Set my own",
	passKeep:               "Keep the existing password",
	passRecommended:        "recommended",
	passCustomHint:         "Type the password, then press enter to capture it (esc cancels).",
	passwordCaptured:       "Password captured; it will be sent over stdin.",
	optJingle:              "Play startup jingle after install",
	optIdentity:            "ADB cannot authenticate this device",
	optForce:               "Try to install despite low free space",
	formContinue:           "Continue",
	statusIdentityRequired: "No stable serial: ADB cannot authenticate this device. Select the acknowledgement or go back.",
	statusSpaceRequired:    "Free space is below the deployment floor; select the low-space option or go back.",
	statusCustomRequired:   "Choose 'Set my own', type the password, then press enter.",
	onOn:                   "on",
	onOff:                  "off",
	statusReenterAddress:   "Re-enter the address the device has now; then continue.",
	optLabelWidth:          40,
	rowLabelWidth:          12,

	installSummaryHeading: "Installation summary",
	labelDevice:           "Device",
	labelIdentity:         "Identity",
	labelBackup:           "Backup",
	labelSpace:            "Space",
	noStableSerialNote:    "No stable serial: ADB cannot authenticate this device across a reconnect.",
	backupNotNeeded:       "not needed on Alpine",
	backupDetailFormat:    "%s · %d entries · verified",
	spaceFormat:           "%s available, %s required",
	spaceStagedFormat:     "= 8 MiB reserve + %s daemon growth (staged %s, installed %s)",
	spaceShortFormat:      "Short by %s. Nothing will be deleted.",
	spaceIdleAppsFormat:   "Installed apps not running (they may be relocated manually): %s",
	spaceDeployerNote:     "The deployer refuses rather than freeing space; go back and adjust the options.",
	installAction:         "Install capsulad",
	installActionHint:     "press enter",
	deployDetachedNote:    "Deploy is detached: ADB/SSH drops while the daemon stops, then reconnects.",

	opPrepare:        "Connecting to the device and preparing the installation",
	opDeploy:         "Deploying capsulad (detached; the link may drop)",
	opWiFiStatus:     "Reading Wi-Fi state",
	opWiFiConnect:    "Joining the Wi-Fi network",
	opApps:           "Installing first-party apps",
	opMarusia:        "Configuring Marusia",
	opChangePassword: "Updating the administrator password",
	opWorking:        "Working",
	connectingWiFi:   "Connecting to Wi-Fi",

	passwordScreenHeading: "Your administrator password",
	keepingPassword:       "Keeping the existing administrator password.",
	recoverPassword:       "Recover it with `capsulad-cli auth set-password` in the device shell.",
	onlyTimeShown:         "This is the only time it is shown. Store it in a password manager.",
	copyRequested:         "Copy requested (OSC 52); paste it into your password manager.",
	storedPassword:        "I have stored this password",
	statusConfirmStored:   "Confirm you have stored the password (space), or press c to copy it.",
	changePasswordHeading: "Set a new administrator password",
	changePasswordNote:    "It is sent to the device on stdin and never enters argv or the log.",

	wifiHeading:              "Wi-Fi",
	wifiStateLabel:           "State",
	wifiConnectedLabel:       "Connected",
	wifiSetupAPLabel:         "Setup AP",
	wifiPortalLabel:          "Portal",
	wifiAPPasswordLabel:      "AP password",
	wifiUnknown:              "unknown",
	wifiNoScan:               "No scan results. Press m to enter a network name.",
	wifiManual:               "Enter a network name manually",
	wifiJoinFormat:           "Join %s",
	wifiPassphraseLabel:      "Passphrase",
	wifiPassphraseNote:       "The passphrase goes to the device over stdin; it never enters argv or logs.",
	wifiManualHeading:        "Enter a Wi-Fi network",
	wifiSSIDLabel:            "SSID",
	wifiOpenNetwork:          "Open network (no passphrase)",
	statusNoNetwork:          "No network selected.",
	statusEnterSSID:          "Enter a network name.",
	securityOpen:             "open",
	signalExcellent:          "excellent",
	signalGood:               "good",
	signalFair:               "fair",
	signalWeak:               "weak",
	logWiFiSkipped:           "Wi-Fi skipped by the user",
	logAcceptDebugging:       "On the device, accept the \"Allow USB debugging?\" prompt for this computer.",
	logNoStableSerial:        "warning: this device has no stable serial; ADB cannot authenticate it across a reconnect",
	logKeepingPassword:       "keeping the existing administrator password; verifying it before launch",
	logWiFiConnectedFormat:   "Wi-Fi is already connected: %s (%s)",
	logWiFiUnavailableFormat: "Wi-Fi is not available: %s",
	logNoScanManual:          "this firmware cannot scan; enter the network name manually",
	logScanFailedFormat:      "scan failed: %v",

	appsHeading:        "Choose first-party apps",
	appsSelectedFormat: "Selected: %d",
	appsReleaseFormat:  " · release %s · platform %s",
	appsNoneNote:       "Nothing selected: enter installs no apps. Press a for all, or s to skip.",
	appDescriptions: map[string]string{
		"org.capsulad.airplay":       "Receive audio from Apple devices",
		"org.capsulad.cadence":       "Connect to a self-hosted Cadence assistant",
		"org.capsulad.dumbassistant": "A simple, private voice assistant",
		"org.capsulad.homeassistant": "Bridge the speaker into Home Assistant",
		"org.capsulad.marusia":       "VK Marusia voice assistant",
		"org.capsulad.opencapsula":   "Connect to a self-hosted OpenCapsula server",
		"org.capsulad.pa":            "Stream raw audio to the speaker",
		"org.capsulad.radio":         "Play internet radio stations",
		"org.capsulad.simon":         "A small game for the speaker",
		"org.capsulad.soundboard":    "Play sound effects from the panel",
	},

	marusiaHeading:   "Marusia needs a VK access token",
	marusiaNoSession: "No stock voice-agent session was found, so nothing could be migrated.",
	marusiaPaste:     "Paste a token (hidden) or skip; you can set it later in the Web UI.",

	completeHeading:      "Installation complete",
	finishedWithErrors:   "Installation finished with errors",
	labelWebUI:           "Web UI",
	labelCertFingerprint: "Cert SHA-256",
	labelDownload:        "Download",
	labelVerify:          "Verify",
	labelAdmin:           "Admin",
	labelWiFi:            "Wi-Fi",
	labelMarusia:         "Marusia",
	labelRollback:        "Rollback",
	adminSet:             "password set (shown after deployment)",
	adminKept:            "existing password kept",
	wifiSkipped:          "skipped",
	trustCertNote:        "Trust the certificate after comparing the fingerprint; the Web UI is self-signed.",
	marusiaMigrated:      "stock session migrated; vendor file left in place",
	marusiaTokenSet:      "VK access token stored",
	marusiaNoToken:       "installed without a token",
	appsSectionHeading:   "Apps",
	nothingDeleted:       "Nothing was deleted. Vendor software and files are preserved; the boot hook can be disabled.",

	errorHeading:          "Installation stopped",
	whatNextHeading:       "What next?",
	detailsHeading:        "Details",
	actionRetry:           "Retry this step",
	actionChangeAddress:   "Change the device address",
	actionBack:            "Back",
	actionBackApps:        "Back to the app selection",
	actionSkipSummary:     "Skip to the summary",
	actionSkipWiFi:        "Skip Wi-Fi and continue",
	actionQuit:            "Quit the installer",
	deployPartialNote:     "The device may be part-way through. Reconnect and inspect /tmp/capsulad-installer/deploy.log before retrying.",
	deployLogHeading:      "Deploy log (verbatim, last lines)",
	recoveryAddressFormat: "The device stopped answering at %s. The daemon owns the Wi-Fi interface, so the link drops while it restarts.",
	recoveryWait:          "Wait for the device to come back on the same IP, or connect to its setup access point:",
	recoveryNetworkFormat: "  network: %s",
	recoveryHintFormat:    "  password hint: %s",
	recoveryPasswordNote:  "  password: see the device label or your saved Wi-Fi password",
	recoveryPortalFormat:  "  portal: %s",
	recoveryRetry:         "Then re-enter the device address and retry.",
	appsErrorNote:         "Some selected apps may already be installed; the summary lists each result. Retrying reinstalls the selection.",
	noDeployNote:          "No deployment was started by this step; the device is unchanged.",

	hostKeyHeading:       "New SSH host key",
	hostKeyWaiting:       "Waiting for the host key...",
	hostLabel:            "Host",
	fingerprintLabel:     "SHA-256",
	hostKeyTOFU:          "Trust on first use pins this key under your config directory.",
	hostKeyMismatch:      "A later mismatch is a hard failure.",
	trustContinue:        "Trust and continue",
	refuse:               "Refuse",
	statusHostKeyPinned:  "host key pinned",
	statusHostKeyRefused: "host key refused",

	abortHeading:             "Stop the installation?",
	abortCurrentStepFormat:   "Current step: %s.",
	abortDeployLine1:         "The deployment runs detached on the device, because the daemon owns Wi-Fi.",
	abortDeployLine2:         "Leaving now stops this interface, not that script; the device may finish or fail on its own.",
	abortOtherLine1:          "This cancels the current step before any deployment starts.",
	abortOtherLine2:          "The device is left as it was; the last completed step is in the log.",
	abortCheckLog:            "Check later with: cat /tmp/capsulad-installer/deploy.log",
	abortKeepWaiting:         "Keep waiting",
	abortStopQuit:            "Stop and quit",
	statusFinishedBeforeStop: "That step finished before it could be stopped; nothing was cancelled.",
	abortStepFallback:        "a device step",

	errPasswordShortFormat:  "password must be at least %d characters",
	errPasswordNewline:      "password must not contain a line break",
	errSSIDEmpty:            "SSID is empty",
	errSSIDStartsDashFormat: "SSID %q starts with '-'; enter it manually with a different spelling or choose another network",
	errSSIDControl:          "SSID contains a control character",

	platformLabels: map[device.Platform]string{
		device.PlatformRockchipStock: "Jailbroken stock (Rockchip RK3308)",
		device.PlatformAmlogicStock:  "Jailbroken stock (Amlogic A113X)",
		device.PlatformAmlogicAlpine: "Alpine Linux (Amlogic A113X)",
	},
}

var ruCatalog = catalog{
	stepFormat:         "Шаг %d · %s",
	stepLanguageDevice: "Язык и устройство",
	stepFirmware:       "Прошивка",
	stepAddress:        "Адрес",
	stepOptions:        "Параметры установки",
	stepSummary:        "Сводка установки",
	stepPassword:       "Пароль администратора",
	stepWiFi:           "Wi-Fi",
	stepApps:           "Приложения",
	stepMarusia:        "Marusia",
	stepWorking:        "Выполняется",
	stepComplete:       "Готово",
	stepStopped:        "Остановлено",
	stepHostKey:        "Ключ узла SSH",
	stepAbort:          "Подтверждение остановки",

	hintDevice:         "↑/↓ выбор · enter выбрать/продолжить · ctrl+c выход",
	hintFirmware:       "↑/↓ выбор · enter продолжить · esc назад · ctrl+c выход",
	hintAddressStock:   "enter подключиться · esc назад",
	hintAddressAlpine:  "tab/shift+tab поля · enter подключиться · esc назад",
	hintForm:           "↑/↓ выбор · enter/space изменить · esc назад",
	hintFormEditing:    "enter сохранить · esc отмена",
	hintSummary:        "enter установить · esc назад",
	hintWorking:        "pgup/pgdn журнал · esc остановить · ctrl+c остановить",
	hintPassword:       "enter продолжить · space подтвердить · c копировать · n изменить",
	hintChangePassword: "enter сохранить · esc назад",
	hintWiFi:           "enter подключиться · ↑/↓ выбор · m вручную · r обновить · s пропустить · esc назад",
	hintWiFiPass:       "enter подключиться · esc назад",
	hintWiFiManual:     "enter продолжить · ctrl+o открытая сеть · esc назад",
	hintApps:           "enter установить · space отметить · a все · n ничего · s пропустить · esc назад",
	hintMarusia:        "enter сохранить токен · esc пропустить",
	hintComplete:       "enter/q выход",
	hintError:          "↑/↓ выбор · enter выполнить · q выход",
	hintHostKey:        "y доверять · n отклонить",
	hintAbort:          "↑/↓ выбор · enter подтвердить · ctrl+c остановить сейчас",
	hintScroll:         "↕ pgup/pgdn",

	deviceHeading:            "Выберите язык и устройство",
	languageLabel:            "Язык",
	langEnglish:              "English (UK)",
	langRussian:              "Русский",
	deviceLabel:              "Устройство",
	deviceOriginal:           "Капсула (большая)",
	deviceMini:               "Капсула Мини",
	supported:                "поддерживается",
	notSupported:             "скоро",
	originalNote:             "На данный момент capsulad не поддерживает большую Капсулу.",
	miniNote:                 "Прошитая Rockchip или Amlogic с Alpine Linux или стоковой прошивкой.",
	statusCapsulaUnsupported: "Оригинальная Capsula не поддерживается capsulad.",

	firmwareHeading:    "Какая прошивка сейчас на вашем устройстве?",
	firmwareStock:      "Взломанная стоковая (capsula-jb)",
	firmwareStockNote:  "Подключается по ADB на порту 5555.",
	firmwareAlpine:     "Alpine Linux",
	firmwareAlpineNote: "Подключается по SSH как root.",

	addressHeading:       "Адрес устройства",
	addressLabel:         "Адрес",
	sshUserLabel:         "Пользователь SSH",
	sshPasswordLabel:     "Пароль SSH",
	keyLabel:             "Закрытый ключ (необязательно)",
	addrPlaceholder:      "адрес устройства, напр. 192.168.1.42",
	sshUserPlaceholder:   "root",
	sshPassPlaceholder:   "пароль SSH (скрыт)",
	keyPlaceholder:       "путь к закрытому ключу SSH (необязательно)",
	adminPassPlaceholder: "пароль администратора (скрыт)",
	wifiSSIDPlaceholder:  "имя сети Wi-Fi",
	wifiPassPlaceholder:  "пароль Wi-Fi (скрыт)",
	tokenPlaceholder:     "токен доступа VK (скрыт, необязательно)",
	addressStockNote:     "ADB подключается к <адрес>:5555.",
	addressAlpineNote:    "Нужен пароль или закрытый ключ. Ключ узла закрепляется при первом подключении.",
	statusAddressEmpty:   "Сначала введите адрес устройства.",
	statusAlpineCreds:    "Для Alpine нужен пароль SSH или закрытый ключ.",

	optionsHeading:         "Параметры установки",
	passwordHeading:        "Пароль администратора",
	passGenerate:           "Сгенерировать надёжный пароль",
	passCustom:             "Задать свой",
	passKeep:               "Оставить существующий пароль",
	passRecommended:        "рекомендуется",
	passCustomHint:         "Введите пароль и нажмите enter (esc отменяет).",
	passwordCaptured:       "Пароль принят; он будет передан через stdin.",
	optJingle:              "Вкл. мелодию запуска",
	optIdentity:            "ADB не может подтвердить подлинность устройства",
	optForce:               "Всё равно попытаться установить",
	formContinue:           "Далее",
	statusIdentityRequired: "Нет стабильного серийного номера: ADB не может подтвердить устройство. Отметьте подтверждение или вернитесь назад.",
	statusSpaceRequired:    "В памяти устройства слишком мало свободного места.",
	statusCustomRequired:   "Выберите «Задать свой», введите пароль и нажмите enter.",
	onOn:                   "да",
	onOff:                  "нет",
	statusReenterAddress:   "Введите текущий адрес устройства и продолжите.",
	optLabelWidth:          50,
	rowLabelWidth:          20,

	installSummaryHeading: "Сводка установки",
	labelDevice:           "Устройство",
	labelIdentity:         "Идентификатор",
	labelBackup:           "Резервная копия",
	labelSpace:            "Место",
	noStableSerialNote:    "Нет стабильного серийного номера: ADB не сможет подтвердить устройство после переподключения.",
	backupNotNeeded:       "не требуется для Alpine",
	backupDetailFormat:    "%s · записей: %d · проверено",
	spaceFormat:           "доступно %s, требуется %s",
	spaceStagedFormat:     "= резерв 8 MiB + рост демона %s (подготовлено %s, установлено %s)",
	spaceShortFormat:      "Не хватает %s. Ничего не будет удалено.",
	spaceIdleAppsFormat:   "Установленные приложения не запущены (их можно перенести вручную): %s",
	spaceDeployerNote:     "Средство развёртывания откажется, а не будет освобождать место; вернитесь и измените параметры.",
	installAction:         "Установить capsulad",
	installActionHint:     "нажмите enter",
	deployDetachedNote:    "Развёртывание выполняется отдельно: ADB/SSH отключится, пока демон перезапускается, затем подключение восстановится.",

	opPrepare:        "Подключение к устройству и подготовка установки",
	opDeploy:         "Развёртывание capsulad (отдельно; соединение может прерваться)",
	opWiFiStatus:     "Чтение состояния Wi-Fi",
	opWiFiConnect:    "Подключение к сети Wi-Fi",
	opApps:           "Установка приложений",
	opMarusia:        "Настройка Marusia",
	opChangePassword: "Обновление пароля администратора",
	opWorking:        "Выполняется",
	connectingWiFi:   "Подключение к Wi-Fi",

	passwordScreenHeading: "Ваш пароль администратора",
	keepingPassword:       "Сохраняется существующий пароль администратора.",
	recoverPassword:       "Восстановить можно командой `capsulad-cli auth set-password` в оболочке устройства.",
	onlyTimeShown:         "Пароль показывается только один раз. Сохраните его в менеджере паролей.",
	copyRequested:         "Запрошено копирование (OSC 52); вставьте пароль в менеджер паролей.",
	storedPassword:        "Я сохранил этот пароль",
	statusConfirmStored:   "Подтвердите, что сохранили пароль (space), или нажмите c, чтобы скопировать.",
	changePasswordHeading: "Задайте новый пароль администратора",
	changePasswordNote:    "Он передаётся на устройство через stdin и не попадает в argv или журнал.",

	wifiHeading:              "Wi-Fi",
	wifiStateLabel:           "Состояние",
	wifiConnectedLabel:       "Подключено",
	wifiSetupAPLabel:         "Точка настройки",
	wifiPortalLabel:          "Портал",
	wifiAPPasswordLabel:      "Пароль точки",
	wifiUnknown:              "неизвестно",
	wifiNoScan:               "Нет результатов сканирования. Нажмите m, чтобы ввести имя сети.",
	wifiManual:               "Ввести имя сети вручную",
	wifiJoinFormat:           "Подключение к %s",
	wifiPassphraseLabel:      "Пароль",
	wifiPassphraseNote:       "Пароль передаётся на устройство через stdin; он не попадает в argv или журналы.",
	wifiManualHeading:        "Введите сеть Wi-Fi",
	wifiSSIDLabel:            "SSID",
	wifiOpenNetwork:          "Открытая сеть (без пароля)",
	statusNoNetwork:          "Сеть не выбрана.",
	statusEnterSSID:          "Введите имя сети.",
	securityOpen:             "открытая",
	signalExcellent:          "отлично",
	signalGood:               "хорошо",
	signalFair:               "средне",
	signalWeak:               "слабо",
	logWiFiSkipped:           "Wi-Fi пропущен пользователем",
	logAcceptDebugging:       "На устройстве примите запрос «Разрешить отладку по USB?» для этого компьютера.",
	logNoStableSerial:        "предупреждение: у устройства нет стабильного серийного номера; ADB не сможет подтвердить его после переподключения",
	logKeepingPassword:       "сохраняется существующий пароль администратора; проверка перед запуском",
	logWiFiConnectedFormat:   "Wi-Fi уже подключён: %s (%s)",
	logWiFiUnavailableFormat: "Wi-Fi недоступен: %s",
	logNoScanManual:          "эта прошивка не поддерживает сканирование; введите имя сети вручную",
	logScanFailedFormat:      "сканирование не удалось: %v",

	appsHeading:        "Выберите приложения",
	appsSelectedFormat: "Выбрано: %d",
	appsReleaseFormat:  " · релиз %s · платформа %s",
	appsNoneNote:       "Ничего не выбрано: enter установит ноль приложений. Нажмите a для всех или s, чтобы пропустить.",
	appDescriptions: map[string]string{
		"org.capsulad.airplay":       "Приём звука с устройств Apple",
		"org.capsulad.cadence":       "Подключение к собственному серверу Cadence",
		"org.capsulad.dumbassistant": "Простой приватный голосовой помощник",
		"org.capsulad.homeassistant": "Интеграция колонки с Home Assistant",
		"org.capsulad.marusia":       "Голосовой помощник VK Marusia",
		"org.capsulad.opencapsula":   "Подключение к собственному серверу OpenCapsula",
		"org.capsulad.pa":            "Передача сырого звука на колонку",
		"org.capsulad.radio":         "Воспроизведение интернет-радио",
		"org.capsulad.simon":         "Небольшая игра для колонки",
		"org.capsulad.soundboard":    "Звуковые эффекты с панели",
	},

	marusiaHeading:   "Marusia требует токен доступа VK",
	marusiaNoSession: "Сохранённая сессия голосового агента не найдена, переносить нечего.",
	marusiaPaste:     "Вставьте токен (скрыто) или пропустите; его можно задать позже в Web UI.",

	completeHeading:      "Установка завершена",
	finishedWithErrors:   "Установка завершена с ошибками",
	labelWebUI:           "Web UI",
	labelCertFingerprint: "SHA-256 сертификата",
	labelDownload:        "Скачать",
	labelVerify:          "Проверить",
	labelAdmin:           "Админ",
	labelWiFi:            "Wi-Fi",
	labelMarusia:         "Marusia",
	labelRollback:        "Откат",
	adminSet:             "пароль задан (показан после развёртывания)",
	adminKept:            "существующий пароль сохранён",
	wifiSkipped:          "пропущено",
	trustCertNote:        "Доверьтесь сертификату после сверки отпечатка; Web UI использует самоподписанный сертификат.",
	marusiaMigrated:      "сессия поставщика перенесена; файл поставщика оставлен на месте",
	marusiaTokenSet:      "токен доступа VK сохранён",
	marusiaNoToken:       "установлено без токена",
	appsSectionHeading:   "Приложения",
	nothingDeleted:       "Ничего не удалено. Программы и файлы поставщика сохранены; загрузочный хук можно отключить.",

	errorHeading:          "Установка остановлена",
	whatNextHeading:       "Что дальше?",
	detailsHeading:        "Подробности",
	actionRetry:           "Повторить этот шаг",
	actionChangeAddress:   "Изменить адрес устройства",
	actionBack:            "Назад",
	actionBackApps:        "Вернуться к выбору приложений",
	actionSkipSummary:     "Перейти к сводке",
	actionSkipWiFi:        "Пропустить Wi-Fi и продолжить",
	actionQuit:            "Выйти из установщика",
	deployPartialNote:     "Устройство может быть частично обновлено. Переподключитесь и проверьте /tmp/capsulad-installer/deploy.log перед повтором.",
	deployLogHeading:      "Журнал развёртывания (последние строки)",
	recoveryAddressFormat: "Устройство перестало отвечать по адресу %s. Демон владеет интерфейсом Wi-Fi, поэтому связь прерывается при перезапуске.",
	recoveryWait:          "Дождитесь, пока устройство вернётся на тот же IP, или подключитесь к его точке настройки:",
	recoveryNetworkFormat: "  сеть: %s",
	recoveryHintFormat:    "  подсказка к паролю: %s",
	recoveryPasswordNote:  "  пароль: см. этикетку устройства или сохранённый пароль Wi-Fi",
	recoveryPortalFormat:  "  портал: %s",
	recoveryRetry:         "Затем снова введите адрес устройства и повторите.",
	appsErrorNote:         "Некоторые приложения могли уже установиться; сводка покажет результат каждого. Повтор переустановит выбор.",
	noDeployNote:          "На этом шаге развёртывание не начиналось; устройство не изменено.",

	hostKeyHeading:       "Новый ключ узла SSH",
	hostKeyWaiting:       "Ожидание ключа узла...",
	hostLabel:            "Узел",
	fingerprintLabel:     "SHA-256",
	hostKeyTOFU:          "Доверие при первом использовании закрепит этот ключ в каталоге конфигурации.",
	hostKeyMismatch:      "Последующее несовпадение — критическая ошибка.",
	trustContinue:        "Доверять и продолжить",
	refuse:               "Отклонить",
	statusHostKeyPinned:  "ключ узла закреплён",
	statusHostKeyRefused: "ключ узла отклонён",

	abortHeading:             "Остановить установку?",
	abortCurrentStepFormat:   "Текущий шаг: %s.",
	abortDeployLine1:         "Развёртывание выполняется на устройстве отдельно, потому что демон владеет Wi-Fi.",
	abortDeployLine2:         "Выход остановит только этот интерфейс, а не тот сценарий; устройство может завершить работу или завершиться с ошибкой само.",
	abortOtherLine1:          "Текущий шаг будет отменён до начала развёртывания.",
	abortOtherLine2:          "Устройство останется как было; последний выполненный шаг есть в журнале.",
	abortCheckLog:            "Проверить позже: cat /tmp/capsulad-installer/deploy.log",
	abortKeepWaiting:         "Продолжить ожидание",
	abortStopQuit:            "Остановить и выйти",
	statusFinishedBeforeStop: "Шаг завершился до остановки; ничего не отменено.",
	abortStepFallback:        "шаг устройства",

	errPasswordShortFormat:  "пароль должен содержать не менее %d символов",
	errPasswordNewline:      "пароль не должен содержать перевод строки",
	errSSIDEmpty:            "SSID пуст",
	errSSIDStartsDashFormat: "SSID %q начинается с '-'; введите другое имя или выберите другую сеть",
	errSSIDControl:          "SSID содержит управляющий символ",

	platformLabels: map[device.Platform]string{
		device.PlatformRockchipStock: "Взломанная заводская (Rockchip RK3308)",
		device.PlatformAmlogicStock:  "Взломанная заводская (Amlogic A113X)",
		device.PlatformAmlogicAlpine: "Alpine Linux (Amlogic A113X)",
	},
}
