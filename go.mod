module github.com/bnema/neferafk

go 1.27

require (
	github.com/bnema/nefergui v0.0.0
	github.com/bnema/purego-pam v0.0.0
	github.com/bnema/wlturbo v0.3.0
	github.com/bnema/zerowrap v1.4.1
	github.com/godbus/dbus/v5 v5.2.2
	github.com/stretchr/testify v1.12.1
	golang.org/x/sys v0.48.0
)

require (
	github.com/bnema/purego v0.13.0-bnema.1 // indirect
	github.com/bnema/purego-vulkan v0.6.0 // indirect
	github.com/bnema/purego-xkbcommon v0.0.0-20260928071113-e79265dfcdc2 // indirect
	github.com/go-text/typesetting v0.3.5 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/rs/zerolog v1.35.1 // indirect
	github.com/stretchr/objx v0.5.3 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/image v0.46.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)

replace github.com/bnema/purego-pam => ../purego-pam

replace github.com/bnema/nefergui => ../veya/.worktrees/session-lock

replace github.com/bnema/wlturbo => ../wlturbo/.worktrees/session-lock

replace github.com/bnema/purego => ../purego

replace github.com/bnema/purego-vulkan => ../purego-vulkan

replace github.com/bnema/purego-xkbcommon => ../purego-xkbcommon/.worktrees/secret-bytes
