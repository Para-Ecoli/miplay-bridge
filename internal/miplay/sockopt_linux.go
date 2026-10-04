//go:build linux && !mips && !mipsle && !mips64 && !mips64le

package miplay

// soReusePort is Linux SO_REUSEPORT. The Go syscall package only exports it
// on some architectures (amd64 and 386 are missing it), so the asm-generic
// value is replicated here to keep the mDNS listener dependency-free.
const soReusePort = 0x0f
