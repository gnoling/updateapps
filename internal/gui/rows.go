package gui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// columns lays children out left to right at fixed widths; the last one takes
// what's left. The table and its header share it so they line up.
type columns struct{ widths []float32 }

var tableColumns = &columns{widths: []float32{40, 230, 110, 170}}

func (c *columns) MinSize(objs []fyne.CanvasObject) fyne.Size {
	var w, h float32
	for i, o := range objs {
		if i < len(c.widths) {
			w += c.widths[i]
		} else {
			w += o.MinSize().Width
		}
		if mh := o.MinSize().Height; mh > h {
			h = mh
		}
	}
	return fyne.NewSize(w, h)
}

func (c *columns) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	var x float32
	for i, o := range objs {
		w := size.Width - x
		if i < len(c.widths) {
			w = c.widths[i]
		}
		o.Resize(fyne.NewSize(w, size.Height))
		o.Move(fyne.NewPos(x, 0))
		x += w
	}
}

func newHeader(checkAll func(bool)) fyne.CanvasObject {
	bold := func(s string) *widget.Label {
		return widget.NewLabelWithStyle(s, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	}
	return container.New(tableColumns, widget.NewCheck("", checkAll), bold("Name"), bold("Category"), bold("Version"), bold("Status"))
}

func newRowWidget() fyne.CanvasObject {
	label := func() *widget.Label {
		l := widget.NewLabel("")
		l.Truncation = fyne.TextTruncateEllipsis
		return l
	}
	return container.New(tableColumns, widget.NewCheck("", nil), label(), label(), label(), label())
}

func (u *ui) updateRowWidget(id widget.ListItemID, obj fyne.CanvasObject) {
	if id >= len(u.visible) {
		return
	}
	r := u.visible[id]
	c := obj.(*fyne.Container)
	check := c.Objects[0].(*widget.Check)
	check.OnChanged = nil // SetChecked would fire it
	check.SetChecked(r.Checked)
	check.OnChanged = func(on bool) {
		r.Checked = on
		u.updateButtons()
	}
	name, cat, ver, status := c.Objects[1].(*widget.Label), c.Objects[2].(*widget.Label), c.Objects[3].(*widget.Label), c.Objects[4].(*widget.Label)
	name.SetText(r.App.Name)
	cat.SetText(r.App.Category)
	ver.SetText(r.Version())
	status.Importance = widget.MediumImportance
	switch {
	case r.Attention():
		status.Importance = widget.DangerImportance
	case !r.App.IsEnabled() || r.App.Blocked != "":
		status.Importance = widget.LowImportance
	}
	status.SetText(r.Status())
}
