package amqp

import (
	"bytes"
	"crypto/tls"
	"io"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Default connection options
const (
	DefaultIdleTimeout  = 1 * time.Minute
	DefaultMaxFrameSize = 512
	DefaultMaxSessions  = 65536
)

// Errors
var (
	ErrTimeout = errorNew("timeout waiting for response")
)

// ConnOption is an function for configuring an AMQP connection.
type ConnOption func(*conn) error

// ConnServerHostname sets the hostname sent in the AMQP
// Open frame and TLS ServerName (if not otherwise set).
//
// This is useful when the AMQP connection will be established
// via a pre-established TLS connection as the server may not
// know which hostname the client is attempting to connect to.
func ConnServerHostname(hostname string) ConnOption {
	return func(c *conn) error {
		c.hostname = hostname
		return nil
	}
}

// ConnTLS toggles TLS negotiation.
//
// Default: false.
func ConnTLS(enable bool) ConnOption {
	return func(c *conn) error {
		c.tlsNegotiation = enable
		return nil
	}
}

// ConnTLSConfig sets the tls.Config to be used during
// TLS negotiation.
//
// This option is for advanced usage, in most scenarios
// providing a URL scheme of "amqps://" or ConnTLS(true)
// is sufficient.
func ConnTLSConfig(tc *tls.Config) ConnOption {
	return func(c *conn) error {
		c.tlsConfig = tc
		c.tlsNegotiation = true
		return nil
	}
}

// ConnIdleTimeout specifies the maximum period between receiving
// frames from the peer.
//
// Resolution is milliseconds. A value of zero indicates no timeout.
// This setting is in addition to TCP keepalives.
//
// Default: 1 minute.
func ConnIdleTimeout(d time.Duration) ConnOption {
	return func(c *conn) error {
		if d < 0 {
			return errorNew("idle timeout cannot be negative")
		}
		c.idleTimeout = d
		return nil
	}
}

// ConnMaxFrameSize sets the maximum frame size that
// the connection will accept.
//
// Must be 512 or greater.
//
// Default: 512.
func ConnMaxFrameSize(n uint32) ConnOption {
	return func(c *conn) error {
		if n < 512 {
			return errorNew("max frame size must be 512 or greater")
		}
		c.maxFrameSize = n
		return nil
	}
}

// ConnConnectTimeout configures how long to wait for the
// server during connection establishment.
//
// Once the connection has been established, ConnIdleTimeout
// applies. If duration is zero, no timeout will be applied.
//
// Default: 0.
func ConnConnectTimeout(d time.Duration) ConnOption {
	return func(c *conn) error { c.connectTimeout = d; return nil }
}

// ConnMaxSessions sets the maximum number of channels.
//
// n must be in the range 1 to 65536.
//
// BUG: Currently this limits how many channels can ever
//      be opened on this connection rather than how many
//      channels can be open at the same time.
//
// Default: 65536.
func ConnMaxSessions(n int) ConnOption {
	return func(c *conn) error {
		if n < 1 {
			return errorNew("max sessions cannot be less than 1")
		}
		if n > 65536 {
			return errorNew("max sessions cannot be greater than 65536")
		}
		c.channelMax = uint16(n - 1)
		return nil
	}
}

// conn is an AMQP connection.
type conn struct {
	net            net.Conn      // underlying connection
	connectTimeout time.Duration // time to wait for reads/writes during conn establishment
	pauseRead      int32         // atomically set to indicate connReader should pause reading from network
	resumeRead     chan struct{} // connReader reads from channel while paused, until channel is closed

	// TLS
	tlsNegotiation bool        // negotiate TLS
	tlsComplete    bool        // TLS negotiation complete
	tlsConfig      *tls.Config // TLS config, default used if nil (ServerName set to Client.hostname)

	// SASL
	saslHandlers map[symbol]stateFunc // map of supported handlers keyed by SASL mechanism, SASL not negotiated if nil
	saslComplete bool                 // SASL negotiation complete

	// local settings
	maxFrameSize uint32        // max frame size to accept
	channelMax   uint16        // maximum number of channels to allow
	hostname     string        // hostname of remote server (set explicitly or parsed from URL)
	idleTimeout  time.Duration // maximum period between receiving frames

	// peer settings
	peerIdleTimeout  time.Duration // maximum period between sending frames
	peerMaxFrameSize uint32        // maximum frame size peer will accept

	// conn state
	errMu     sync.Mutex    // mux holds errMu from start until shutdown completes; operations are sequential before mux is started
	err       error         // error to be returned to client
	doneOnce  sync.Once     // only close done once
	done      chan struct{} // indicates the connection is done
	closeOnce sync.Once

	// mux
	newSession chan newSessionResp // new Sessions are requested from mux by reading off this channel
	delSession chan *Session       // session completion is indicated to mux by sending the Session on this channel
	connErr    chan error          // connReader/Writer notifications of an error

	// connReader
	rxProto chan protoHeader // protoHeaders received by connReader
	rxFrame chan frame       // AMQP frames received by connReader
	rxDone  chan struct{}

	// connWriter
	txFrame chan frame   // AMQP frames to be sent by connWriter
	txBuf   bytes.Buffer // buffer for marshaling frames before transmitting
	txDone  chan struct{}
}

type newSessionResp struct {
	session *Session
	err     error
}

func newConn(netConn net.Conn, opts ...ConnOption) (*conn, error) {
	c := &conn{
		net:              netConn,
		maxFrameSize:     DefaultMaxFrameSize,
		peerMaxFrameSize: DefaultMaxFrameSize,
		channelMax:       DefaultMaxSessions - 1, // -1 because channel-max starts at zero
		idleTimeout:      DefaultIdleTimeout,
		done:             make(chan struct{}),
		connErr:          make(chan error, 2), // buffered to ensure connReader/Writer won't leak
		rxProto:          make(chan protoHeader),
		rxFrame:          make(chan frame),
		rxDone:           make(chan struct{}),
		newSession:       make(chan newSessionResp),
		delSession:       make(chan *Session),
		txFrame:          make(chan frame),
		txDone:           make(chan struct{}),
	}

	// apply options
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *conn) initTLSConfig() {
	// create a new config if not already set
	if c.tlsConfig == nil {
		c.tlsConfig = new(tls.Config)
	}

	// TLS config must have ServerName or InsecureSkipVerify set
	if c.tlsConfig.ServerName == "" && !c.tlsConfig.InsecureSkipVerify {
		c.tlsConfig.ServerName = c.hostname
	}
}

func (c *conn) start() error {
	// start reader
	go c.connReader()

	// run connection establishment state machine
	for state := c.negotiateProto; state != nil; {
		state = state()
	}

	// check if err occurred
	if c.err != nil {
		close(c.txDone) // close here since connWriter hasn't been started yet
		c.Close()
		return c.err
	}

	// start multiplexor and writer
	go c.mux()
	go c.connWriter()

	return nil
}

func (c *conn) Close() error {
	c.closeOnce.Do(func() { c.close() })
	return c.err
}

func (c *conn) close() {
	c.closeDone() // notify goroutines and blocked functions to exit

	// Client.mux holds err lock until shutdown, block until
	// shutdown completes, then return the error (if any)
	c.errMu.Lock()
	defer c.errMu.Unlock()

	// wait for writing to stop, allows it to send the final close frame
	<-c.txDone

	err := c.net.Close()
	if c.err == nil {
		c.err = err
	}

	// check rxDone after closing net, otherwise may block
	// for up to c.idleTimeout
	<-c.rxDone
}

// closeDone closes Client.done if it has not already been closed
func (c *conn) closeDone() {
	c.doneOnce.Do(func() { close(c.done) })
}

// getErr returns conn.err.
//
// Must only be called after conn.done is closed.
func (c *conn) getErr() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// mux is start in it's own goroutine after initial connection establishment.
//  It handles muxing of sessions, keepalives, and connection errors.
func (c *conn) mux() {
	var (
		// create the next session to allocate
		nextSession = newSessionResp{session: newSession(c, 0)}

		// map channels to sessions
		sessionsByChannel       = make(map[uint16]*Session)
		sessionsByRemoteChannel = make(map[uint16]*Session)
	)

	// hold the errMu lock until error or done
	c.errMu.Lock()
	defer c.errMu.Unlock()

	for {
		// check if last loop returned an error
		if c.err != nil {
			c.closeDone()
			return
		}

		select {
		// error from connReader
		case c.err = <-c.connErr:

		// new frame from connReader
		case fr := <-c.rxFrame:
			// lookup session and send to Session.mux
			session, ok := sessionsByRemoteChannel[fr.channel]
			if !ok {
				// if this is a begin, RemoteChannel should be used
				begin, ok := fr.body.(*performBegin)
				if !ok {
					c.err = errorErrorf("unexpected frame: %#v", fr.body)
					continue
				}

				session, ok = sessionsByChannel[begin.RemoteChannel]
				if !ok {
					c.err = errorErrorf("unexpected frame: %#v", fr.body)
					continue
				}

				session.remoteChannel = fr.channel
				sessionsByRemoteChannel[fr.channel] = session
			}

			select {
			case session.rx <- fr:
			case <-c.done:
				return
			}

		// new session request
		//
		// Continually try to send the next session on the channel,
		// then add it to the sessions map. This allows us to control ID
		// allocation and prevents the need to have shared map. Since new
		// sessions are far less frequent than frames being sent to sessions,
		// this avoids the lock/unlock for session lookup.
		case c.newSession <- nextSession:
			if nextSession.err != nil {
				continue
			}

			ch := nextSession.session.channel
			sessionsByChannel[ch] = nextSession.session

			if ch >= c.channelMax {
				nextSession.session = nil
				nextSession.err = errorErrorf("reached connection channel max (%d)", c.channelMax)
				continue
			}

			// create the next session to send
			nextSession.session = newSession(c, ch+1)

		// session deletion
		case s := <-c.delSession:
			// TODO: allow channel number reuse
			delete(sessionsByChannel, s.channel)
			delete(sessionsByRemoteChannel, s.remoteChannel)

		// connection is complete
		case <-c.done:
			return
		}
	}
}

// frameReader returns io.EOF on each read, this allows
// ReadFrom to work with a net.conn without blocking until
// the connection is closed
type frameReader struct {
	r io.Reader // underlying reader
}

func (f *frameReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil {
		return n, err
	}
	return n, io.EOF
}

// connReader reads from the net.Conn, decodes frames, and passes them
// up via the conn.rxFrame and conn.rxProto channels.
func (c *conn) connReader() {
	defer close(c.rxDone)

	buf := new(bytes.Buffer)

	var (
		negotiating     = true      // true during conn establishment, check for protoHeaders
		currentHeader   frameHeader // keep track of the current header, for frames split across multiple TCP packets
		frameInProgress bool        // true if in the middle of receiving data for currentHeader
	)

	// frameReader facilitates reading directly into buf
	fr := &frameReader{r: c.net}

	for {
		// need to read more if buf doesn't contain the complete frame
		// or there's not enough in buf to parse the header
		if frameInProgress || buf.Len() < frameHeaderSize {
			c.net.SetReadDeadline(time.Now().Add(c.idleTimeout))
			_, err := buf.ReadFrom(fr)
			if err != nil {
				if atomic.LoadInt32(&c.pauseRead) == 1 {
					// need to stop reading during TLS negotiation,
					// see conn.startTLS()
					c.pauseRead = 0
					for range c.resumeRead {
						// reads indicate paused, resume on close
					}
					fr.r = c.net // conn wrapped with TLS
					continue
				}

				// check if error was due to close in progress
				select {
				case <-c.done:
					return
				default:
				}

				c.connErr <- err
				return
			}
		}

		// read more if buf doesn't contain enough to parse the header
		if buf.Len() < frameHeaderSize {
			continue
		}

		// during negotiation, check for proto frames
		if negotiating && bytes.Equal(buf.Bytes()[:4], []byte{'A', 'M', 'Q', 'P'}) {
			p, err := parseProtoHeader(buf)
			if err != nil {
				c.connErr <- err
				return
			}

			// negotiation is complete once an AMQP proto frame is received
			if p.ProtoID == protoAMQP {
				negotiating = false
			}

			// send proto header
			select {
			case <-c.done:
				return
			case c.rxProto <- p:
			}

			continue
		}

		// parse the header if a frame isn't in progress
		if !frameInProgress {
			var err error
			currentHeader, err = parseFrameHeader(buf)
			if err != nil {
				c.connErr <- err
				return
			}
			frameInProgress = true
		}

		// check size is reasonable
		if currentHeader.Size > math.MaxInt32 { // make max size configurable
			c.connErr <- errorNew("payload too large")
			return
		}

		bodySize := int(currentHeader.Size - frameHeaderSize)

		// the full frame has been received
		if buf.Len() < bodySize {
			continue
		}
		frameInProgress = false

		// check if body is empty (keepalive)
		if bodySize == 0 {
			continue
		}

		// parse the frame
		payload := bytes.NewBuffer(buf.Next(bodySize))
		parsedBody, err := parseFrameBody(payload)
		if err != nil {
			c.connErr <- err
			return
		}

		// send to mux
		select {
		case <-c.done:
			return
		case c.rxFrame <- frame{channel: currentHeader.Channel, body: parsedBody}:
		}
	}
}

func (c *conn) connWriter() {
	defer close(c.txDone)

	var (
		// keepalives are sent at a rate of 1/2 idle timeout
		keepaliveInterval = c.peerIdleTimeout / 2
		// 0 disables keepalives
		keepalivesEnabled = keepaliveInterval > 0
		// set if enable, nil if not; nil channels block forever
		keepalive <-chan time.Time
	)

	if keepalivesEnabled {
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		keepalive = ticker.C
	}

	var err error
	for {
		if err != nil {
			c.connErr <- err
			return
		}

		select {
		// frame write request
		case fr := <-c.txFrame:
			err = c.writeFrame(fr)
			if err == nil && fr.done != nil {
				close(fr.done)
			}

		// keepalive timer
		case <-keepalive:
			_, err = c.net.Write(keepaliveFrame)
			// It would be slightly more efficient in terms of network
			// resources to reset the timer each time a frame is sent.
			// However, keepalives are small (8 bytes) and the interval
			// is usually on the order of minutes. It does not seem
			// worth it to add extra operations in the write path to
			// avoid. (To properly reset a timer it needs to be stopped,
			// possibly drained, then reset.)

		// connection complete
		case <-c.done:
			// send close
			c.writeFrame(frame{
				typ:  frameTypeAMQP,
				body: &performClose{},
			})
			return
		}
	}
}

// writeFrame writes a frame to the network, may only be used
// by connWriter after initial negotiation.
func (c *conn) writeFrame(fr frame) error {
	if c.connectTimeout != 0 {
		c.net.SetWriteDeadline(time.Now().Add(c.connectTimeout))
	}

	// writeFrame into txBuf
	c.txBuf.Reset()
	err := writeFrame(&c.txBuf, fr)
	if err != nil {
		return err
	}

	// validate the frame isn't exceeding peer's max frame size
	if uint64(c.txBuf.Len()) > uint64(c.peerMaxFrameSize) {
		return errorErrorf("frame larger than peer's max frame size")
	}

	// write to network
	_, err = c.net.Write(c.txBuf.Bytes())
	return err
}

// writeProtoHeader writes an AMQP protocol header to the
// network
func (c *conn) writeProtoHeader(pID protoID) error {
	if c.connectTimeout != 0 {
		c.net.SetWriteDeadline(time.Now().Add(c.connectTimeout))
	}
	_, err := c.net.Write([]byte{'A', 'M', 'Q', 'P', byte(pID), 1, 0, 0})
	return err
}

// keepaliveFrame is an AMQP frame with no body, used for keepalives
var keepaliveFrame = []byte{0x00, 0x00, 0x00, 0x08, 0x02, 0x00, 0x00, 0x00}

// wantWriteFrame is used by sessions and links to send frame to
// connWriter.
func (c *conn) wantWriteFrame(fr frame) {
	select {
	case c.txFrame <- fr:
	case <-c.done:
	}
}

// stateFunc is a state in a state machine.
//
// The state is advanced by returning the next state.
// The state machine concludes when nil is returned.
type stateFunc func() stateFunc

// negotiateProto determines which proto to negotiate next
func (c *conn) negotiateProto() stateFunc {
	// in the order each must be negotiated
	switch {
	case c.tlsNegotiation && !c.tlsComplete:
		return c.exchangeProtoHeader(protoTLS)
	case c.saslHandlers != nil && !c.saslComplete:
		return c.exchangeProtoHeader(protoSASL)
	default:
		return c.exchangeProtoHeader(protoAMQP)
	}
}

type protoID uint8

// protocol IDs received in protoHeaders
const (
	protoAMQP protoID = 0x0
	protoTLS  protoID = 0x2
	protoSASL protoID = 0x3
)

// exchangeProtoHeader performs the round trip exchange of protocol
// headers, validation, and returns the protoID specific next state.
func (c *conn) exchangeProtoHeader(pID protoID) stateFunc {
	// write the proto header
	c.err = c.writeProtoHeader(pID)
	if c.err != nil {
		return nil
	}

	// read response header
	p, err := c.readProtoHeader()
	if err != nil {
		c.err = err
		return nil
	}

	if pID != p.ProtoID {
		c.err = errorErrorf("unexpected protocol header %#00x, expected %#00x", p.ProtoID, pID)
		return nil
	}

	// go to the proto specific state
	switch pID {
	case protoAMQP:
		return c.openAMQP
	case protoTLS:
		return c.startTLS
	case protoSASL:
		return c.negotiateSASL
	default:
		c.err = errorErrorf("unknown protocol ID %#02x", p.ProtoID)
		return nil
	}
}

// readProtoHeader reads a protocol header packet from c.rxProto.
func (c *conn) readProtoHeader() (protoHeader, error) {
	var deadline <-chan time.Time
	if c.connectTimeout != 0 {
		deadline = time.After(c.connectTimeout)
	}
	var p protoHeader
	select {
	case p = <-c.rxProto:
		return p, nil
	case err := <-c.connErr:
		return p, err
	case fr := <-c.rxFrame:
		return p, errorErrorf("unexpected frame %#v", fr)
	case <-deadline:
		return p, ErrTimeout
	}
}

// startTLS wraps the conn with TLS and returns to Client.negotiateProto
func (c *conn) startTLS() stateFunc {
	c.initTLSConfig()

	// convoluted method to pause connReader, explorer simpler alternatives
	c.resumeRead = make(chan struct{})        // 1. create channel
	atomic.StoreInt32(&c.pauseRead, 1)        // 2. indicate should pause
	c.net.SetReadDeadline(time.Time{}.Add(1)) // 3. set deadline to interrupt connReader
	c.resumeRead <- struct{}{}                // 4. wait for connReader to read from chan, indicating paused
	defer close(c.resumeRead)                 // 5. defer connReader resume by closing channel
	c.net.SetReadDeadline(time.Time{})        // 6. clear deadline

	// wrap existing net.Conn and perform TLS handshake
	tlsConn := tls.Client(c.net, c.tlsConfig)
	if c.connectTimeout != 0 {
		tlsConn.SetWriteDeadline(time.Now().Add(c.connectTimeout))
	}
	c.err = tlsConn.Handshake()
	if c.err != nil {
		return nil
	}

	// swap net.Conn
	c.net = tlsConn
	c.tlsComplete = true

	// go to next protocol
	return c.negotiateProto
}

// openAMQP round trips the AMQP open performative
func (c *conn) openAMQP() stateFunc {
	// send open frame
	c.err = c.writeFrame(frame{
		typ: frameTypeAMQP,
		body: &performOpen{
			ContainerID:  string(randBytes(40)),
			Hostname:     c.hostname,
			MaxFrameSize: c.maxFrameSize,
			ChannelMax:   c.channelMax,
			IdleTimeout:  c.idleTimeout,
		},
		channel: 0,
	})
	if c.err != nil {
		return nil
	}

	// get the response
	fr, err := c.readFrame()
	if err != nil {
		c.err = err
		return nil
	}
	o, ok := fr.body.(*performOpen)
	if !ok {
		c.err = errorErrorf("unexpected frame type %T", fr.body)
		return nil
	}

	// update peer settings
	if o.MaxFrameSize > 0 {
		c.peerMaxFrameSize = o.MaxFrameSize
	}
	if o.IdleTimeout > 0 {
		// TODO: reject very small idle timeouts
		c.peerIdleTimeout = o.IdleTimeout
	}
	if o.ChannelMax < c.channelMax {
		c.channelMax = o.ChannelMax
	}

	// connection established, exit state machine
	return nil
}

// negotiateSASL returns the SASL handler for the first matched
// mechanism specified by the server
func (c *conn) negotiateSASL() stateFunc {
	// read mechanisms frame
	fr, err := c.readFrame()
	if err != nil {
		c.err = err
		return nil
	}
	sm, ok := fr.body.(*saslMechanisms)
	if !ok {
		c.err = errorErrorf("unexpected frame type %T", fr.body)
		return nil
	}

	// return first match in c.saslHandlers based on order received
	for _, mech := range sm.Mechanisms {
		if state, ok := c.saslHandlers[mech]; ok {
			return state
		}
	}

	// no match
	c.err = errorErrorf("no supported auth mechanism (%v)", sm.Mechanisms) // TODO: send "auth not supported" frame?
	return nil
}

// saslOutcome processes the SASL outcome frame and return Client.negotiateProto
// on success.
//
// SASL handlers return this stateFunc when the mechanism specific negotiation
// has completed.
func (c *conn) saslOutcome() stateFunc {
	// read outcome frame
	fr, err := c.readFrame()
	if err != nil {
		c.err = err
		return nil
	}
	so, ok := fr.body.(*saslOutcome)
	if !ok {
		c.err = errorErrorf("unexpected frame type %T", fr.body)
		return nil
	}

	// check if auth succeeded
	if so.Code != codeSASLOK {
		c.err = errorErrorf("SASL PLAIN auth failed with code %#00x: %s", so.Code, so.AdditionalData) // implement Stringer for so.Code
		return nil
	}

	// return to c.negotiateProto
	c.saslComplete = true
	return c.negotiateProto
}

// readFrame is used during connection establishment to read a single frame.
//
// After setup, conn.mux handles incoming frames.
func (c *conn) readFrame() (frame, error) {
	var deadline <-chan time.Time
	if c.connectTimeout != 0 {
		deadline = time.After(c.connectTimeout)
	}

	var fr frame
	select {
	case fr = <-c.rxFrame:
		return fr, nil
	case err := <-c.connErr:
		return fr, err
	case p := <-c.rxProto:
		return fr, errorErrorf("unexpected protocol header %#v", p)
	case <-deadline:
		return fr, ErrTimeout // TODO: move to connReader
	}
}
