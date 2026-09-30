.PHONY: build test vet race mocks mocks-check fakes-check check pkg install

build:
	CGO_ENABLED=0 go build ./...
test:
	CGO_ENABLED=0 go test ./...
vet:
	CGO_ENABLED=0 go vet ./...
race:
	CGO_ENABLED=1 go test -race ./...
mocks:
	mockery
mocks-check: mocks
	git diff --exit-code -- internal/mocks ':(glob)internal/**/*_mock_test.go'
	@test -z "$$(git ls-files --others --exclude-standard -- internal/mocks ':(glob)internal/**/*_mock_test.go')"
fakes-check:
	@rc=0; grep -rniE '^\s*type\s+\w*(fake|stub|spy|dummy|mock)\w*|^\s*\w*(fake|stub|spy|dummy|mock)\w*(\[[^]]*\])?\s+(struct|interface)\b' --include='*_test.go' --exclude='*_mock_test.go' internal cmd 2>/dev/null || rc=$$?; \
	[ $$rc -eq 1 ] || { [ $$rc -eq 0 ] && printf '%s\n' 'handwritten test double: generate with Mockery v3' >&2; exit 1; }
check: build vet test fakes-check
	CGO_ENABLED=0 staticcheck ./...

# Arch package of the committed HEAD (packaging/arch/PKGBUILD). Go modules
# come from the module proxy in prepare(); the build itself runs offline.
# pacman-ordered version: 0.0.0.r<commits>.g<hash>; a tag replaces 0.0.0.
pkg: SHELL := bash
pkg: .SHELLFLAGS := -eo pipefail -c
pkg:
	@test -z "$$(git status --porcelain)" || echo "warning: uncommitted changes are not packaged" >&2
	v=$$(t=$$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//; s/-/_/g'); \
		echo "$${t:-0.0.0}.r$$(git rev-list --count HEAD).g$$(git rev-parse --short HEAD)"); \
	d=$$(mktemp -d /tmp/neferafk-pkg.XXXXXX); trap 'rm -rf "$$d"' EXIT; \
	git archive --prefix=neferafk-$$v/ -o "$$d/neferafk-$$v.tar.gz" HEAD; \
	cp packaging/arch/PKGBUILD "$$d/"; \
	cd "$$d" && sed -i "s/^pkgver=.*/pkgver=$$v/; s/^sha256sums=.*/sha256sums=('$$(sha256sum *.tar.gz | cut -d' ' -f1)')/" PKGBUILD; \
	makepkg -f --noconfirm; mkdir -p $(CURDIR)/dist; rm -f $(CURDIR)/dist/neferafk-*.pkg.tar.zst; mv *.pkg.tar.zst $(CURDIR)/dist/
	@ls dist/*.pkg.tar.zst

# Build the package, then install it with pacman (asks for sudo).
install: pkg
	sudo pacman -U dist/neferafk-*.pkg.tar.zst
