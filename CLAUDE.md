# Project

The task brief below is the source of truth for what this project must do.
README.md describes the architecture and the merge rules.

- Go 1.27 (toolchain auto-downloaded), standard library only. React frontend in `web/`, built with esbuild into `internal/webui/dist` (embedded in the binary; rebuild with `make web` after frontend changes).
- `make test` runs go vet, `go test -race` and the frontend tests (`node --test`); `make fuzz` runs the fuzzers.
- Correctness of the merged market state is the top priority (real money): keep the merge rules fail-closed and cover every rule change with tests in `internal/market`.

@CONTEXT.md
