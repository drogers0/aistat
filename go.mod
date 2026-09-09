module github.com/drogers0/aistat/v2

// The floor is set by the toolchain, not by language features. Go 1.24 is the
// first release whose internal linker emits LC_UUID, without which dyld on
// macOS 26 refuses to launch the CGO_ENABLED=0 darwin release binaries (#38).
// 1.26 rather than 1.27 because staticcheck 2026.2.1 cannot read 1.27 export
// data. Do not lower to match what the code syntactically needs.
go 1.26
