package visual

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/bnema/zerowrap"
	"github.com/stretchr/testify/require"
)

func TestWorkerEnvironmentIsAnAllowlistOfPresentVariables(t *testing.T) {
	host := map[string]string{
		"HOME": "/home/u", "PATH": "/usr/bin", "USER": "u", "GNUPGHOME": "/g", "GPG_TTY": "/dev/pts/1",
		"WAYLAND_DISPLAY": "wayland-1", "SSH_AUTH_SOCK": "/s", "NEFERAFK_PIN": "123456", "LC_ALL": "",
	}
	env := workerEnvironment(func(k string) (string, bool) { v, ok := host[k]; return v, ok })
	require.Equal(t, []string{"HOME=/home/u", "PATH=/usr/bin", "USER=u", "LC_ALL=", "GNUPGHOME=/g", "GPG_TTY=/dev/pts/1"}, env)
	for _, e := range env {
		require.NotContains(t, e, "WAYLAND")
		require.NotContains(t, e, "SSH_")
		require.NotContains(t, e, "PIN")
	}
	require.Empty(t, workerEnvironment(func(string) (string, bool) { return "", false }))
}

func TestLockConfigReportsSkippedOutputWithComponent(t *testing.T) {
	var buf bytes.Buffer
	log := zerowrap.New(zerowrap.Config{Output: &buf})
	ctl := newAuthController(testLog(), nil, boot(), time.Minute)
	cfg := newLockConfig(log, ctl, make(chan struct{}, 1))
	require.NotNil(t, cfg.OnOutputError)
	cfg.OnOutputError("HDMI-A-1", errors.New("surface failed"))
	out := buf.String()
	require.Contains(t, out, "visual-lock")
	require.Contains(t, out, "HDMI-A-1")
	require.Contains(t, out, "surface failed")
	require.Contains(t, out, "WRN")
}
