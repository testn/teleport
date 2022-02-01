/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package protocol

const (
	// PacketTypeResponse is the packet type for server response messages.
	PacketTypeResponse uint8 = 0x04
	// PacketTypeLogin7 is the Login7 packet type.
	PacketTypeLogin7 uint8 = 0x10
	// PacketTypePreLogin is the Pre-Login packet type.
	PacketTypePreLogin uint8 = 0x12

	// packetHeaderSize is the size of the protocol packet header.
	packetHeaderSize = 8

	// packetStatusLast indicates that the packet is the last in the request.
	packetStatusLast uint8 = 0x01

	preLoginOptionVersion    = 0x00
	preLoginOptionEncryption = 0x01
	preLoginOptionInstance   = 0x02
	preLoginOptionThreadID   = 0x03
	preLoginOptionMARS       = 0x04

	// preLoginEncryptionRequired is a Pre-Login option indicating that server
	// does not accept TLS connection (clients connect through TLS tunnel).
	preLoginEncryptionNotSupported = 0x02
)

// preLoginOptions are getting returned to the client during Pre-Login handshake.
//
// SQL Server clients always connect to the local proxy and connections come
// through TLS tunnel.
var preLoginOptions = map[uint8][]byte{
	preLoginOptionVersion:    []uint8{0x0f, 0x00, 0x07, 0xd0, 0x00, 0x00},
	preLoginOptionEncryption: {preLoginEncryptionNotSupported},
	preLoginOptionInstance:   {0x00},
	preLoginOptionThreadID:   {},
	preLoginOptionMARS:       {0x00},
}
