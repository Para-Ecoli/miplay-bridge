//go:build linux && (mips || mipsle || mips64 || mips64le)

package dlna

// soReusePort is Linux SO_REUSEPORT for the MIPS socket-option numbering
// space, which differs from the asm-generic value.
const soReusePort = 0x0200
