package logind

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
)

// keyboardBacklight runs only on the adapter owner. Saved values survive
// failed writes so a later restore (including Close) can retry them.
func (a *Adapter) keyboardBacklight(ctx context.Context, on bool) {
	if on {
		for name, value := range a.brightness {
			if a.setBrightness(ctx, name, value) {
				delete(a.brightness, name)
			}
		}
		return
	}
	if a.session == "" {
		return
	}
	entries, err := fs.ReadDir(a.leds, ".")
	if err != nil {
		a.log.Warn().Err(err).Msg("keyboard backlight discovery failed")
		return
	}
	found := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.Contains(name, "kbd_backlight") {
			continue
		}
		found = true
		if _, saved := a.brightness[name]; saved {
			continue
		}
		data, err := fs.ReadFile(a.leds, name+"/brightness")
		if err != nil {
			a.log.Warn().Err(err).Str("device", name).Msg("keyboard brightness read failed")
			continue
		}
		value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 32)
		if err != nil {
			a.log.Warn().Err(err).Str("device", name).Msg("invalid keyboard brightness")
			continue
		}
		if a.brightness == nil {
			a.brightness = make(map[string]uint32)
		}
		a.brightness[name] = uint32(value)
		a.setBrightness(ctx, name, 0)
	}
	if !found && !a.noKeyboardLogged {
		a.noKeyboardLogged = true
		a.log.Debug().Msg("no keyboard backlight devices found")
	}
}

func (a *Adapter) setBrightness(ctx context.Context, name string, value uint32) bool {
	_, err := a.call(ctx, a.session, sessionInterface+".SetBrightness", "leds", name, value)
	if err != nil {
		a.log.Warn().Err(err).Str("device", name).Msg("keyboard brightness request failed")
	}
	return err == nil
}
