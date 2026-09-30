package auth

import "golang.org/x/sys/unix"

// secretBuffer owns a dedicated anonymous page, not a Go heap page shared with
// GC/runtime objects. The logical capacity is fixed (PIN 32, credential 512).
// mlock is best effort; no promise covers transient codec/PAM bridge copies.
type secretBuffer struct {
	page   []byte
	data   []byte
	locked bool
}

func newSecret(size int) (*secretBuffer, error) {
	if size < 1 || size > 512 {
		return nil, ErrProtocol
	}
	page, err := unix.Mmap(-1, 0, unix.Getpagesize(), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return nil, errSource
	}
	return &secretBuffer{page: page, data: page[:size:size], locked: unix.Mlock(page) == nil}, nil
}

func (b *secretBuffer) close() {
	if b == nil || b.page == nil {
		return
	}
	clear(b.page)
	if b.locked {
		unix.Munlock(b.page)
	}
	unix.Munmap(b.page)
	b.page, b.data = nil, nil
}
