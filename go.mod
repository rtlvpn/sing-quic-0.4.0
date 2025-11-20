module github.com/sagernet/sing-quic

go 1.24.0

toolchain go1.24.1

require (
	github.com/gofrs/uuid/v5 v5.3.2
	github.com/sagernet/quic-go v0.52.0-beta.1
	github.com/sagernet/sing v0.6.7
	github.com/xtaci/kcp-go/v5 v5.6.36
	golang.org/x/crypto v0.37.0
	golang.org/x/exp v0.0.0-20240506185415-9bf2ced13842
)

//replace github.com/sagernet/quic-go => github.com/rtlvpn/quic-go-0.49.0-beta.1 v0.0.0-20250523070155-97c126947dcb

require (
	github.com/klauspost/cpuid/v2 v2.2.6 // indirect
	github.com/klauspost/reedsolomon v1.12.0 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/quic-go/qpack v0.5.1 // indirect
	github.com/tjfoc/gmsm v1.4.1 // indirect
	golang.org/x/mod v0.20.0 // indirect
	golang.org/x/net v0.38.0 // indirect
	golang.org/x/sync v0.13.0 // indirect
	golang.org/x/sys v0.32.0 // indirect
	golang.org/x/text v0.24.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	golang.org/x/tools v0.24.0 // indirect
)
