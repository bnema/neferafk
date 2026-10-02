package visual

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// The messages are the ones neferclient v0.2.0 reports through Handler.Error.
func TestLockErrorPolicyEndsOnlyOnKeyboardFailures(t *testing.T) {
	fatal := []error{
		errors.New("neferclient: keymap descriptor unavailable"),
		fmt.Errorf("neferclient: unsupported keymap format %d or size %d", 2, 0),
		fmt.Errorf("neferclient: keymap: %w", errors.New("compile failed")),
		fmt.Errorf("neferclient: keyboard: %w", errors.New("xkb context")),
		fmt.Errorf("neferclient: bind wl_seat: %w", errors.New("gone")),
	}
	benign := []error{
		fmt.Errorf("neferclient: bind cursor shape manager: %w", errors.New("gone")),
		fmt.Errorf("neferclient: dmabuf format table index %d out of range", 9),
		errors.New("neferclient: invalid fractional scale zero"),
		errors.New("timerfd read: resource temporarily unavailable"),
	}
	var reported []error
	l := &locker{cfg: lockConfig{OnError: func(err error) { reported = append(reported, err) }}}
	for _, err := range fatal {
		require.ErrorIs(t, l.errorPolicy(err), err, "%v must end the lock run", err)
	}
	require.Empty(t, reported)
	for _, err := range benign {
		require.NoError(t, l.errorPolicy(err), "%v must not end the lock run", err)
	}
	require.Equal(t, benign, reported)
}
