package gui

import (
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
	u.trayUp = trayHostPresent()
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
