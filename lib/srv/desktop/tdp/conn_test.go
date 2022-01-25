package tdp

import (
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestTDPConnTracksLocalRemoteAddrs verifies that a TDP connection
// uses the underlying local/remote addrs when available.
func TestTDPConnTracksLocalRemoteAddrs(t *testing.T) {
	local := &net.IPAddr{IP: net.ParseIP("192.168.1.2")}
	remote := &net.IPAddr{IP: net.ParseIP("192.168.1.3")}

	for _, test := range []struct {
		desc   string
		conn   io.ReadWriter
		local  net.Addr
		remote net.Addr
	}{
		{
			desc: "implements srv.TrackingConn",
			conn: fakeTrackingConn{
				local:  local,
				remote: remote,
			},
			local:  local,
			remote: remote,
		},
		{
			desc:   "does not implement srv.TrackingConn",
			conn:   &bytes.Buffer{},
			local:  nil,
			remote: nil,
		},
	} {
		t.Run(test.desc, func(t *testing.T) {
			tc := NewConn(test.conn)
			l := tc.LocalAddr()
			r := tc.RemoteAddr()
			require.Equal(t, test.local, l)
			require.Equal(t, test.remote, r)
		})
	}
}

type fakeTrackingConn struct {
	*bytes.Buffer
	local  net.Addr
	remote net.Addr
}

func (f fakeTrackingConn) LocalAddr() net.Addr {
	return f.local
}

func (f fakeTrackingConn) RemoteAddr() net.Addr {
	return f.remote
}

func (f fakeTrackingConn) Close() error { return nil }
