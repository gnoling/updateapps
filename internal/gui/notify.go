package gui

import (
	"os"
	"path/filepath"

	"fyne.io/fyne/v2"
	"github.com/godbus/dbus/v5"
)

// sendNotification talks to the notification server itself: Fyne asks for a
// notification that never expires, and these should.
func (u *ui) sendNotification(title, body string) {
	conn, err := dbus.SessionBus()
	if err != nil {
		u.app.SendNotification(fyne.NewNotification(title, body))
		return
	}
	const serverDefault = int32(-1)
	call := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications").
		Call("org.freedesktop.Notifications.Notify", 0, "updateapps", uint32(0), notificationIcon(),
			title, body, []string{}, map[string]dbus.Variant{}, serverDefault)
	if call.Err != nil {
		u.app.SendNotification(fyne.NewNotification(title, body))
	}
}

// notificationIcon is a file the server can load, or a theme icon name.
func notificationIcon() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "system-software-update"
	}
	path := filepath.Join(dir, "updateapps", "icon.svg")
	if data, err := os.ReadFile(path); err != nil || string(data) != string(iconSVG) {
		if os.MkdirAll(filepath.Dir(path), 0o755) != nil || os.WriteFile(path, iconSVG, 0o644) != nil {
			return "system-software-update"
		}
	}
	return path
}
