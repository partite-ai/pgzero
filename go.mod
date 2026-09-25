module github.com/partite-ai/pgzero

go 1.26.5

replace github.com/tetratelabs/wazero => github.com/mpoindexter/wazero v0.0.0-20260824195113-bbffa6a24693

require (
	github.com/jackc/pgx/v5 v5.11.0
	github.com/partite-ai/wacogo v0.0.0-20260925042705-58815519b9a8
	github.com/tetratelabs/wazero v1.12.0
	golang.org/x/sys v0.44.0
)

require (
	github.com/coreos/go-semver v0.3.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	go.bytecodealliance.org v0.7.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.29.0 // indirect
	golang.org/x/tools v0.44.0 // indirect
)

tool github.com/partite-ai/wacogo/cmd/wacogo-witgen
