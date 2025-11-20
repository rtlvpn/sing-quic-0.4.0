package kcp

import (
	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
)

// PacketWriter is a helper interface for writing UDP packets
type PacketWriter interface {
	WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error
}

// PacketReader is a helper interface for reading UDP packets
type PacketReader interface {
	ReadPacket(buffer *buf.Buffer) (M.Socksaddr, error)
}
