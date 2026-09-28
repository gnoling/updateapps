package gui

import (
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"github.com/godbus/dbus/v5"

	"github.com/gnoling/updateapps/internal/engine"
)

// setupTray adds the tray icon and menu. Closing the window only hides it
// when a tray host is there to bring it back.
func (u *ui) setupTray() {
	desk, ok := u.app.(desktop.App)
	if !ok {
		return
	}
	u.trayMenu = fyne.NewMenu("updateapps",
		fyne.NewMenuItem("Show updateapps", u.showWindow),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Check for updates", func() { u.run(engine.ModeCheck, u.model.Runnable(), fromTray) }),
		fyne.NewMenuItem("Update all", func() { u.run(engine.ModeUpdate, u.model.Runnable(), fromTray) }),
		fyne.NewMenuItemSeparator(),
		fyne.NewMenuItem("Quit", u.quit),
	)
	desk.SetSystemTrayMenu(u.trayMenu)
	desk.SetSystemTrayIcon(icon)
	desk.SetSystemTrayWindow(u.win)
	u.tray = true
}

// trayWait is how long a hidden start waits for a tray: at login this
// program can start before the panel.
const trayWait = 45 * time.Second

// awaitTray shows the window if no tray turns up to reach it through.
func (u *ui) awaitTray() {
	for deadline := time.Now().Add(trayWait); u.tray && time.Now().Before(deadline); time.Sleep(time.Second) {
		if trayHostPresent() {
			return
		}
	}
	fyne.Do(u.showWindow)
}

// trayHostPresent asks the session bus whether anything shows status
// notifier icons (the freedesktop tray protocol Fyne uses).
func trayHostPresent() bool {
	conn, err := dbus.SessionBus()
	if err != nil {
		return false
	}
	v, err := conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").
		GetProperty("org.kde.StatusNotifierWatcher.IsStatusNotifierHostRegistered")
	if err != nil {
		return false
	}
	on, _ := v.Value().(bool)
	return on
}
