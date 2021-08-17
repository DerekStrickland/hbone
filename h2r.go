package hbone

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"

	"golang.org/x/net/http2"
)

// - H2R Server accepts mTLS connection from client, using h2r ALPN
// - Client opens a H2 _server_ handler on the stream, H2R server acts as
// a H2 client.
// - Endpoint is registered in k8s, using IP of the server holding the connection
// - SNI requests on the H2R server are routed to existing connection
// - if a connection is not found on local server, forward based on endpoint.
// DialH2R connects to an H2R tunnel endpoint, and accepts connections from the tunnel
// Not blocking.
func (hc *Endpoint) DialH2R(ctx context.Context, addr string) (*tls.Conn, error) {
	tlsCon, err := hc.dialTLS(ctx, addr)
	if err != nil {
		return nil, err
	}

	go func() {
		hc.hb.h2Server.ServeConn(tlsCon, &http2.ServeConnOpts{
			Context: ctx,
			Handler: &HBoneAcceptedConn{conn: tlsCon, hb: hc.hb},
			BaseConfig: &http.Server{},
		})
		if Debug {
			log.Println("H2RClient closed")
		}
	}()
	if Debug {
		log.Println("H2RClient started ", tlsCon.ConnectionState().ServerName,
			tlsCon.ConnectionState().PeerCertificates[0].URIs)
	}
	return tlsCon, nil
}


func (hb *HBone) HandleH2RSNIConn(conn net.Conn) {
	s := NewBufferReader(conn)
	// will also close the conn ( which is the reader )
	defer s.Close()

	sni, err := ParseTLS(s)
	if err != nil {
		return
	}


	rt := hb.H2R[sni]
	if rt != nil {
		// WIP: send over the established connection
		i, o := io.Pipe()

		r, err := http.NewRequest("POST", "http:///_hbone/mtls", i)
		if err != nil {
			return
		}
		res, err := rt.RoundTrip(r)

		s1 := Stream{
			ID: "sni-o",
			Dst: o,
			Src: s, // The SNI reader, including the initial buffer
		}
		ch := make(chan int)
		go s1.CopyBuffered(ch, true)

		s2 := Stream {
			ID: "sni-i",
			Src: res.Body,
			Dst: conn,
		}
		s2.CopyBuffered(nil, true)

		<- ch

		// TODO: wait for first copy to finish
		if Debug {
			log.Println("H2RSNI done ", s1.Err, s2.Err)
		}
		return
	}

	log.Println("SNI-H2R 404", sni)
	return
}

// GetClientConn is called by http2.Transport, if Transport.RoundTrip is called (
// for example used in a http.Client ). We are using the http2.ClientConn directly,
// but this method may be needed if this library is used as a http client.
func (hb *HBone) GetClientConn(req *http.Request, addr string) (*http2.ClientConn, error) {
	panic("implement me")
}

func (hb *HBone) MarkDead(conn *http2.ClientConn) {
	log.Println("H2C Client con terminated")
}


func (hb *HBone) HandlerH2RConn(conn net.Conn) {
	conf := hb.Auth.TLSConfig

	tls := tls.Server(conn, conf)

	// TODO: replace with handshake with context, timeout
	err := HandshakeTimeout(tls, hb.HandsahakeTimeout, conn)
	if err != nil {
		conn.Close()
		return
	}

	// At this point we have the client identity, and we know it's in the trust domain and right CA.
	// TODO: save the endpoint.

	// TODO: construct the SNI header, save it in the map
	// TODO: validate the trust domain, root cert, etc

	sni := tls.ConnectionState().ServerName
	if Debug {
		log.Println("H2RSNI: accepted ", sni)
	}
	h2rc := &H2RConn{
		SNI: sni,
		h2t: &http2.Transport{

		},
	}
	h2rc.h2t.ConnPool = h2rc

	// not blocking. Will write the 'preface' and start reading.
	// When done, MarkDead on the conn pool in the transport is called.
	rt, err := h2rc.h2t.NewClientConn(tls)
	if err != nil {
		conn.Close()
		return
	}
	h2rc.hc = rt

	hb.H2R[sni] = rt

	// TODO: track the active connections in hb, for close purpose.

	// Conn remains open
}

type H2RConn struct {
	SNI string
	h2t *http2.Transport
	hc  *http2.ClientConn
}

func (h *H2RConn) GetClientConn(req *http.Request, addr string) (*http2.ClientConn, error) {
	return h.hc, nil
}

func (h H2RConn) MarkDead(conn *http2.ClientConn) {
	h.hc = nil
}
