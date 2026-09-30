# Repository rules

- Go 1.27 only. Normal builds/tests use CGO_ENABLED=0; race tests use CGO_ENABLED=1.
- Lightweight hexagonal architecture: core imports only stdlib and ports; app wires adapters. Owner goroutines own state, no window-state mutex.
- Reuse NeferWL dependency conventions: zerowrap with component on every entry, Mockery v3 testify generated mocks. No handwritten fakes/stubs/spies or swapped function fields.
- Cross-layer seams live in internal/ports; generated mocks in internal/mocks/ports. Adapter-internal seams stay unexported with generated same-package test mocks. Add .mockery.yml entries and verify generation.
- Signed conventional commits only; no remote/publication/installation or live lock/off/suspend/real credential attempt without authorization.
- Credentials travel directly from visual process to the isolated authentication worker, never through policy daemon, render models, logs, argv or debug output.
- Pure Go/purego PAM via purego-pam; no C helper or renderer fallback. Standard advertised Wayland protocols and existing NeferGUI Vulkan/DMA-BUF/explicit-sync rendering only.
- Flat key = value config with independent disableable lock/fade/output-off/suspend deadlines. Returning activity never unlocks.
