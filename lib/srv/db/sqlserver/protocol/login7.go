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

import (
	"bytes"
	"encoding/binary"
	"io"

	mssql "github.com/denisenkom/go-mssqldb"
	"github.com/gravitational/trace"
)

// Login7Packet represents a Login7 packet that defines authentication rules
// between the client and the server.
//
// https://docs.microsoft.com/en-us/openspecs/windows_protocols/ms-tds/773a62b6-ee89-4c02-9e5e-344882630aac
type Login7Packet struct {
	packet   Packet
	header   Login7Header
	username string
	database string
}

// Username returns the username from the Login7 packet.
func (p *Login7Packet) Username() string {
	return p.username
}

// Database returns the database from the Login7 packet. May be empty.
func (p *Login7Packet) Database() string {
	return p.database
}

// OptionFlags1 returns the packet's first set of option flags.
func (p *Login7Packet) OptionFlags1() uint8 {
	return p.header.OptionFlags1
}

// OptionFlags2 returns the packet's second set of option flags.
func (p *Login7Packet) OptionFlags2() uint8 {
	return p.header.OptionFlags2
}

// TypeFlags returns the packet's set of type flags.
func (p *Login7Packet) TypeFlags() uint8 {
	return p.header.TypeFlags
}

// Login7Header contains options and offset/length pairs parsed from the Login7
// packet sent by client.
//
// Note: the order of fields in the struct matters as it gets unpacked from the
// binary stream.
type Login7Header struct {
	Length            uint32
	TDSVersion        uint32
	PacketSize        uint32
	ClientProgVer     uint32
	ClientPID         uint32
	ConnectionID      uint32
	OptionFlags1      uint8
	OptionFlags2      uint8
	TypeFlags         uint8
	OptionFlags3      uint8
	ClientTimezone    int32
	ClientLCID        uint32
	IbHostName        uint16 // offset
	CchHostName       uint16 // length
	IbUserName        uint16
	CchUserName       uint16
	IbPassword        uint16
	CchPassword       uint16
	IbAppName         uint16
	CchAppName        uint16
	IbServerName      uint16
	CchServerName     uint16
	IbUnused          uint16
	CbUnused          uint16
	IbCltIntName      uint16
	CchCltIntName     uint16
	IbLanguage        uint16
	CchLanguage       uint16
	IbDatabase        uint16
	CchDatabase       uint16
	ClientID          [6]byte
	IbSSPI            uint16
	CbSSPI            uint16
	IbAtchDBFile      uint16
	CchAtchDBFile     uint16
	IbChangePassword  uint16
	CchChangePassword uint16
	CbSSPILong        uint32
}

// ReadLogin7Packet reads Login7 packet from the reader.
func ReadLogin7Packet(r io.Reader) (*Login7Packet, error) {
	pkt, err := ReadPacket(r)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if pkt.Type != PacketTypeLogin7 {
		return nil, trace.BadParameter("expected Login7 packet, got: %#v", pkt)
	}

	var header Login7Header
	if err := binary.Read(bytes.NewBuffer(pkt.Data), binary.LittleEndian, &header); err != nil {
		return nil, trace.Wrap(err)
	}

	// Decode username and database from the packet. Offset/length are counted
	// from from the beginning of entire packet data (excluding header).
	username, err := mssql.ParseUCS2String(
		pkt.Data[header.IbUserName : header.IbUserName+header.CchUserName*2])
	if err != nil {
		return nil, trace.Wrap(err)
	}
	database, err := mssql.ParseUCS2String(
		pkt.Data[header.IbDatabase : header.IbDatabase+header.CchDatabase*2])
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &Login7Packet{
		packet:   *pkt,
		header:   header,
		username: username,
		database: database,
	}, nil
}

// WriteStreamResponse writes stream response packet to the writer.
func WriteStreamResponse(w io.Writer, tokens []mssql.Token) error {
	var data []byte

	for _, token := range tokens {
		bytes, err := token.Marshal()
		if err != nil {
			return trace.Wrap(err)
		}
		data = append(data, bytes...)
	}

	pkt, err := makePacket(PacketTypeResponse, data)
	if err != nil {
		return trace.Wrap(err)
	}

	_, err = w.Write(pkt)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// WriteErrorResponse writer error response to the client.
func WriteErrorResponse(w io.Writer, err error) error {
	return WriteStreamResponse(w, []mssql.Token{
		&mssql.Error{
			Number:  20002,
			Class:   14,
			Message: err.Error(),
		},
	})
}
