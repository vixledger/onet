package network

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"time"

	"go.dedis.ch/kyber/v3"
	"go.dedis.ch/kyber/v3/sign/schnorr"
	"go.dedis.ch/kyber/v3/util/encoding"
	"go.dedis.ch/kyber/v3/util/random"
	"go.dedis.ch/onet/v3/log"
)

// About our TLS strategy:
//
// The design goals were:
// 1. use vanilla TLS for conode/conode communication, in order to ease deployment
//    anxiety with industrial partners. ("If it's not TLS, it's not allowed." Sigh.)
// 2. zero config: whatever config we have in an existing private.toml should be enough
// 3. mutual authentication: each side should end up with proof that the other side is
//    holding the secret key associated with the public key they claim to have.
//
// In order to achieve #1, we limit ourselves to TLS 1.2. This means that the TLS
// private key must be different than the conode private key (which may be from a suite
// not supported in TLS 1.2).
//
// In order to achieve #2, we use self-signed TLS certificates, with a private key
// that is created on server boot and stored in RAM. Because the certificates are
// self-signed, there is no need to pay a CA, nor to load the private key
// or certificate from disk.
//
// In order to achieve #3, we include an extension in the certificate, which
// proves that the same entity that holds the TLS private key (i.e. the signing
// key for the self-signed TLS cert) also holds the conode's private key.
// We do this by hashing a nonce provided by the peer (in order to prove that this
// is a fresh challenge response) and the ASN.1 encoded CommonName of the certificate.
// The CN is always the hex-encoded form of the conode's public key.
//
// Because each side needs a nonce which is controlled by the opposite party in the
// mutual authentication, but TLS does not support sending application data before the handshake,
// we need to find places in the normal TLS 1.2 handshake where we can "tunnel" the nonce
// through. On the TLS client side, we use ClientHelloInfo.ServerName. On the
// the TLS server side, we use the ClientCAs field.
//
// There is a risk that with a less customizable TLS implementation than
// Go's, it would not be possible to send the nonces through like this. However,
// for the moment we are not targeting other languages than Go on the conode/conode
// communication channel.

// TODO: Websockets.
// All of this is completely unrelated to HTTPS security on the websocket side. For
// that, we will implement an opt-in Let's Encrypt client in websocket.go.

// certMaker holds the data necessary to make a certificate on the fly
// and give it to crypto/tls via the GetCertificate and
// GetClientCertificate callbacks in the tls.Config structure.
type certMaker struct {
	si      *ServerIdentity
	suite   Suite
	subj    pkix.Name
	subjDer []byte // the subject encoded in ASN.1 DER format
	k       *ecdsa.PrivateKey
}

func newCertMaker(s Suite, si *ServerIdentity) (*certMaker, error) {
	cm := &certMaker{
		si:    si,
		suite: s,
	}

	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	cm.k = k

	// This used to be "CommonName: cm.si.Public.String()", which
	// results in the "old style" CommonName encoding in pubFromCN.
	// This worked ok for ed25519 and nist, but not for bn256.g1. See
	// dedis/onet#485.
	cm.subj = pkix.Name{CommonName: pubToCN(cm.si.Public)}
	der, err := asn1.Marshal(cm.subj.CommonName)
	if err != nil {
		return nil, err
	}
	cm.subjDer = der
	return cm, nil
}

func (cm *certMaker) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return cm.get([]byte(hello.ServerName))
}

func (cm *certMaker) getClientCertificate(req *tls.CertificateRequestInfo) (*tls.Certificate, error) {
	if len(req.AcceptableCAs) == 0 {
		return nil, errors.New("server did not provide a nonce in AcceptableCAs")
	}
	return cm.get(req.AcceptableCAs[0])
}

func (cm *certMaker) get(nonce []byte) (*tls.Certificate, error) {
	if len(nonce) != nonceSize {
		return nil, errors.New("nonce is the wrong size")
	}

	// Create a signature that proves that:
	// 1. since the nonce was generated by the peer,
	// 2. for this public key,
	// 3. we have control of the private key that is associated with the public
	// key named in the CN.
	//
	// Do this using the same standardized ASN.1 marshaling that x509 uses so
	// that anyone trying to check these signatures themselves in another language
	// will be able to easily do so with their own x509 + kyber implementation.
	buf := bytes.NewBuffer(nonce)
	buf.Write(cm.subjDer)
	sig, err := schnorr.Sign(cm.suite, cm.si.GetPrivate(), buf.Bytes())
	if err != nil {
		return nil, err
	}

	// Even though the serial number is not used in the DEDIS signature,
	// we set it to a big random number. This is what TLS clients expect:
	// that two certs from the same issuer with different public keys will
	// have different serial numbers.
	serial := new(big.Int)
	r := random.Bits(128, true, random.New())
	serial.SetBytes(r)

	tmpl := &x509.Certificate{
		BasicConstraintsValid: true,
		MaxPathLen:            1,
		IsCA:                  false,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		NotAfter:              time.Now().Add(2 * time.Hour),
		NotBefore:             time.Now().Add(-5 * time.Minute),
		SerialNumber:          serial,
		SignatureAlgorithm:    x509.ECDSAWithSHA384,
		Subject:               cm.subj,
		ExtraExtensions: []pkix.Extension{
			{
				Id:       oidDedisSig,
				Critical: false,
				Value:    sig,
			},
		},
	}

	cDer, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, cm.k.Public(), cm.k)
	if err != nil {
		return nil, err
	}
	certs, err := x509.ParseCertificates(cDer)
	if err != nil {
		return nil, err
	}
	if len(certs) < 1 {
		return nil, errors.New("no certificate found")
	}

	return &tls.Certificate{
		PrivateKey:  cm.k,
		Certificate: [][]byte{cDer},
		Leaf:        certs[0],
	}, nil
}

// See https://github.com/dedis/Coding/tree/master/mib/cothority.mib
var oidDedisSig = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 51281, 1, 1}

// We want to copy a tls.Config, but it has a sync.Once in it that we
// should not copy. This is ripped from the Go source, where they
// needed to solve the same problem.
//
// See https://github.com/golang/go/issues/12099
func cloneTLSClientConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{}
	}
	return &tls.Config{
		Rand:                     cfg.Rand,
		Time:                     cfg.Time,
		Certificates:             cfg.Certificates,
		NameToCertificate:        cfg.NameToCertificate,
		GetCertificate:           cfg.GetCertificate,
		RootCAs:                  cfg.RootCAs,
		NextProtos:               cfg.NextProtos,
		ServerName:               cfg.ServerName,
		ClientAuth:               cfg.ClientAuth,
		ClientCAs:                cfg.ClientCAs,
		InsecureSkipVerify:       cfg.InsecureSkipVerify,
		CipherSuites:             cfg.CipherSuites,
		PreferServerCipherSuites: cfg.PreferServerCipherSuites,
		ClientSessionCache:       cfg.ClientSessionCache,
		MinVersion:               cfg.MinVersion,
		MaxVersion:               cfg.MaxVersion,
		CurvePreferences:         cfg.CurvePreferences,
	}
}

// NewTLSListener makes a new TCPListener that is configured for TLS.
func NewTLSListener(si *ServerIdentity, suite Suite) (*TCPListener, error) {
	return NewTLSListenerWithListenAddr(si, suite, "")
}

// NewTLSListenerWithListenAddr makes a new TCPListener that is configured
// for TLS and listening on the given address.
// TODO: Why can't we just use NewTCPListener like usual, but detect
// the ConnType from the ServerIdentity?
func NewTLSListenerWithListenAddr(si *ServerIdentity, suite Suite,
	listenAddr string) (*TCPListener, error) {
	tcp, err := NewTCPListenerWithListenAddr(si.Address, suite, listenAddr)
	if err != nil {
		return nil, err
	}

	cfg, err := tlsConfig(suite, si)
	if err != nil {
		return nil, err
	}

	// This callback will be called for every new client, which
	// gives us a chance to set the nonce that will be sent down to them.
	cfg.GetConfigForClient = func(client *tls.ClientHelloInfo) (*tls.Config, error) {
		// Copy the global config, set the nonce in the copy.
		cfg2 := cloneTLSClientConfig(cfg)

		// Go's TLS server calls cfg.ClientCAs.Subjects() in order to
		// form the data the client will eventually find in
		// AcceptableCAs. So we tunnel our nonce through to there
		// from here.
		cfg2.ClientCAs = x509.NewCertPool()
		vrf, nonce := makeVerifier(suite, nil)
		cfg2.VerifyPeerCertificate = vrf
		cfg2.ClientCAs.AddCert(&x509.Certificate{
			RawSubject: nonce,
		})
		log.Lvl2("Got new connection request from:", client.Conn.RemoteAddr().String())
		return cfg2, nil
	}

	// This is "any client cert" because we do not want crypto/tls
	// to run Verify. However, since we provide a VerifyPeerCertificate
	// callback, it will still call us.
	cfg.ClientAuth = tls.RequireAnyClientCert

	tcp.listener = tls.NewListener(tcp.listener, cfg)
	return tcp, nil
}

// NewTLSAddress returns a new Address that has type TLS with the given
// address addr.
func NewTLSAddress(addr string) Address {
	return NewAddress(TLS, addr)
}

// This is the prototype expected in tls.Config.VerifyPeerCertificate.
type verifier func(rawCerts [][]byte, vrf [][]*x509.Certificate) (err error)

// makeVerifier creates the nonce, and also a closure that has access to the nonce
// so that the caller can put the nonce where it needs to go out. When the peer
// gives us a certificate back, crypto/tls calls the verifier with arguments we
// can't control. But the verifier still has access to the nonce because it's in the
// closure.
func makeVerifier(suite Suite, them *ServerIdentity) (verifier, []byte) {
	nonce := mkNonce(suite)
	return func(rawCerts [][]byte, vrf [][]*x509.Certificate) (err error) {
		var cn string
		defer func() {
			if err == nil {
				log.Lvl3("verify cert ->", cn)
			} else {
				log.Lvl3("verify cert ->", err)
			}
		}()

		if len(rawCerts) != 1 {
			return errors.New("expected exactly one certificate")
		}
		certs, err := x509.ParseCertificates(rawCerts[0])
		if err != nil {
			return err
		}
		if len(certs) != 1 {
			return errors.New("expected exactly one certificate")
		}
		cert := certs[0]

		// Check that the certificate is self-signed as expected and not expired.
		self := x509.NewCertPool()
		self.AddCert(cert)
		opts := x509.VerifyOptions{
			Roots: self,
		}
		_, err = cert.Verify(opts)
		if err != nil {
			return err
		}

		// When we know who we are connecting to (e.g. client mode):
		// Check that the CN is the same as the public key.
		if them != nil {
			err = cert.VerifyHostname(pubToCN(them.Public))
			if err != nil {
				println("here", err.Error())
				return err
			}
		}

		// Check that our extension exists.
		var sig []byte
		for _, x := range cert.Extensions {
			if oidDedisSig.Equal(x.Id) {
				sig = x.Value
				break
			}
		}
		if sig == nil {
			return errors.New("DEDIS signature not found")
		}

		// Check that the DEDIS signature is valid w.r.t. si.Public.
		cn = cert.Subject.CommonName
		pub, err := pubFromCN(suite, cn)
		if err != nil {
			return err
		}

		buf := bytes.NewBuffer(nonce)
		subAsn1, err := asn1.Marshal(cn)
		if err != nil {
			return err
		}
		buf.Write(subAsn1)
		err = schnorr.Verify(suite, pub, buf.Bytes(), sig)

		return err
	}, nonce
}

func pubFromCN(suite kyber.Group, cn string) (kyber.Point, error) {
	if len(cn) < 1 {
		return nil, errors.New("commonName is missing a type byte")
	}
	tp := cn[0]

	switch tp {
	case 'Z':
		// New style encoding: unhex and then unmarshal.
		buf, err := hex.DecodeString(cn[1:])
		if err != nil {
			return nil, err
		}
		r := bytes.NewBuffer(buf)

		pub := suite.Point()
		_, err = pub.UnmarshalFrom(r)
		return pub, err

	default:
		// Old style encoding: simply StringHexToPoint
		return encoding.StringHexToPoint(suite, cn)
	}
}

func pubToCN(pub kyber.Point) string {
	w := &bytes.Buffer{}
	pub.MarshalTo(w)
	return "Z" + hex.EncodeToString(w.Bytes())
}

// tlsConfig returns a generic config that has things set as both the server
// and client need them. The returned config is customized after tlsConfig returns.
func tlsConfig(suite Suite, us *ServerIdentity) (*tls.Config, error) {
	cm, err := newCertMaker(suite, us)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		GetCertificate:       cm.getCertificate,
		GetClientCertificate: cm.getClientCertificate,
		// InsecureSkipVerify means that crypto/tls will not be checking
		// the cert for us.
		InsecureSkipVerify: true,
		// Thus, we need to have our own verification function. It
		// needs to be set in the caller, once we know the nonce.
	}, nil
}

// NewTLSConn will open a TCPConn to the given server over TLS.
// It will check that the remote server has proven
// it holds the given Public key by self-signing a certificate
// linked to that key.
func NewTLSConn(us *ServerIdentity, them *ServerIdentity, suite Suite) (conn *TCPConn, err error) {
	log.Lvl2("NewTLSConn to:", them)
	if them.Address.ConnType() != TLS {
		return nil, errors.New("not a tls server")
	}

	if us.GetPrivate() == nil {
		return nil, errors.New("private key is not set")
	}

	cfg, err := tlsConfig(suite, us)
	if err != nil {
		return nil, err
	}
	vrf, nonce := makeVerifier(suite, them)
	cfg.VerifyPeerCertificate = vrf

	netAddr := them.Address.NetworkAddress()
	for i := 1; i <= MaxRetryConnect; i++ {
		var c net.Conn
		cfg.ServerName = string(nonce)
		c, err = tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", netAddr, cfg)
		if err == nil {
			conn = &TCPConn{
				conn:  c,
				suite: suite,
			}
			return
		}
		if i < MaxRetryConnect {
			time.Sleep(WaitRetry)
		}
	}
	if err == nil {
		err = ErrTimeout
	}
	return
}

const nonceSize = 256 / 8

func mkNonce(s Suite) []byte {
	var buf [nonceSize]byte
	random.Bytes(buf[:], s.RandomStream())
	// In order for the nonce to safely pass through cfg.ServerName,
	// it needs to avoid the characters , [ ] and %.
	for bytes.ContainsAny(buf[:], ".[]%") {
		random.Bytes(buf[:], s.RandomStream())
	}
	return buf[:]
}
