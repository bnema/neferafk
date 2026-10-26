package logind

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/bnema/neferafk/internal/ports"
	"github.com/stretchr/testify/mock"
)

func TestKeyboardBacklightSaveRestoreAndClose(t *testing.T) {
	for _, initial := range []string{"0", "3"} {
		t.Run(initial, func(t *testing.T) {
			b := newMockbus(t)
			a := newAdapter(b, ports.SystemRequirements{Session: true})
			a.owner = testOwner
			a.session = testSession
			a.leds = fstest.MapFS{"tpacpi::kbd_backlight/brightness": &fstest.MapFile{Data: []byte(initial)}, "other/brightness": &fstest.MapFile{Data: []byte("9")}}
			value := uint32(0)
			if initial == "3" {
				value = 3
			}
			b.EXPECT().call(mock.Anything, testOwner, testSession, sessionInterface+".SetBrightness", []any{"leds", "tpacpi::kbd_backlight", uint32(0)}).Return(nil, nil).Twice()
			// A repeated off keeps the first saved value and re-sends 0.
			a.keyboardBacklight(context.Background(), false)
			a.keyboardBacklight(context.Background(), false)
			b.EXPECT().call(mock.Anything, testOwner, testSession, sessionInterface+".SetBrightness", []any{"leds", "tpacpi::kbd_backlight", value}).Return(nil, errors.New("temporary")).Once()
			a.keyboardBacklight(context.Background(), true)
			b.EXPECT().call(mock.Anything, testOwner, testSession, sessionInterface+".SetBrightness", []any{"leds", "tpacpi::kbd_backlight", value}).Return(nil, nil).Once()
			b.EXPECT().close().Return(nil).Once()
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			a.keyboardBacklight(context.Background(), true)
			if len(a.brightness) != 0 {
				t.Fatal(a.brightness)
			}
		})
	}
}

func TestKeyboardBacklightNoDevicesOrInvalidBrightness(t *testing.T) {
	for _, files := range []fstest.MapFS{{}, {"asus::kbd_backlight/brightness": &fstest.MapFile{Data: []byte("bad")}}, {"dell::kbd_backlight/other": &fstest.MapFile{}}} {
		b := newMockbus(t)
		a := newAdapter(b, ports.SystemRequirements{})
		a.owner = testOwner
		a.session = testSession
		a.leds = files
		a.keyboardBacklight(context.Background(), true)
		a.keyboardBacklight(context.Background(), false)
		a.keyboardBacklight(context.Background(), false)
		if len(a.brightness) != 0 {
			t.Fatal(a.brightness)
		}
	}
}

func TestKeyboardBacklightFailedCycleStillTurnsOffNextTime(t *testing.T) {
	b := newMockbus(t)
	a := newAdapter(b, ports.SystemRequirements{})
	a.owner = testOwner
	a.session = testSession
	a.leds = fstest.MapFS{"asus::kbd_backlight/brightness": &fstest.MapFile{Data: []byte("2")}}
	set := func(value uint32, err error) {
		b.EXPECT().call(mock.Anything, testOwner, testSession, sessionInterface+".SetBrightness", []any{"leds", "asus::kbd_backlight", value}).Return(nil, err).Once()
	}
	set(0, errors.New("off failed"))
	a.keyboardBacklight(context.Background(), false)
	set(2, errors.New("restore failed"))
	a.keyboardBacklight(context.Background(), true)
	set(0, nil)
	a.keyboardBacklight(context.Background(), false)
	set(2, nil)
	a.keyboardBacklight(context.Background(), true)
	if len(a.brightness) != 0 {
		t.Fatal(a.brightness)
	}
}

func TestKeyboardBacklightMultipleDevicesAndFailedOff(t *testing.T) {
	b := newMockbus(t)
	a := newAdapter(b, ports.SystemRequirements{})
	a.owner = testOwner
	a.session = testSession
	a.leds = fstest.MapFS{
		"asus::kbd_backlight/brightness": &fstest.MapFile{Data: []byte("2\n")},
		"dell::kbd_backlight/brightness": &fstest.MapFile{Data: []byte("4\n")},
	}
	for name, value := range map[string]uint32{"asus::kbd_backlight": 2, "dell::kbd_backlight": 4} {
		b.EXPECT().call(mock.Anything, testOwner, testSession, sessionInterface+".SetBrightness", []any{"leds", name, uint32(0)}).Return(nil, errors.New("off failed")).Once()
		b.EXPECT().call(mock.Anything, testOwner, testSession, sessionInterface+".SetBrightness", []any{"leds", name, value}).Return(nil, nil).Once()
	}
	a.keyboardBacklight(context.Background(), false)
	a.keyboardBacklight(context.Background(), true)
	if len(a.brightness) != 0 {
		t.Fatal(a.brightness)
	}
}
