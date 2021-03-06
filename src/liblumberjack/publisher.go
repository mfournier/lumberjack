package liblumberjack

import (
  "bytes"
  "encoding/json"
  zmq "github.com/alecthomas/gozmq"
  "log"
  "math/big"
  "syscall"
  "time"
  "compress/zlib"
  "crypto/rand"
  "sodium"
)

var context *zmq.Context

func init() {
  context, _ = zmq.NewContext()
}

// Forever Faithful Socket
type FFS struct {
  Endpoints []string // set of endpoints available to ship to

  // Socket type; zmq.REQ, etc
  SocketType zmq.SocketType

  // Various timeout values
  SendTimeout time.Duration
  RecvTimeout time.Duration

  endpoint  string      // the current endpoint in use
  socket    *zmq.Socket // the current zmq socket
  connected bool        // are we connected?
}

func (s *FFS) Send(data []byte, flags zmq.SendRecvOption) (err error) {
  for {
    s.ensure_connect()

    pi := zmq.PollItems{zmq.PollItem{Socket: s.socket, Events: zmq.POLLOUT}}
    count, err := zmq.Poll(pi, s.SendTimeout)
    if count == 0 {
      // not ready in time, fail the socket and try again.
      log.Printf("%s: timed out waiting to Send(): %s\n", s.endpoint, err)
      s.fail_socket()
    } else {
      //log.Printf("%s: sending %d payload\n", s.endpoint, len(data))
      err = s.socket.Send(data, flags)
      if err != nil {
        log.Printf("%s: Failed to Send() %d byte message: %s\n",
          s.endpoint, len(data), err)
        s.fail_socket()
      } else {
        // Success!
        break
      }
    }
  }
  return
}

func (s *FFS) Recv(flags zmq.SendRecvOption) (data []byte, err error) {
  s.ensure_connect()

  pi := zmq.PollItems{zmq.PollItem{Socket: s.socket, Events: zmq.POLLIN}}
  count, err := zmq.Poll(pi, s.RecvTimeout)
  if count == 0 {
    // not ready in time, fail the socket and try again.
    s.fail_socket()

    err = syscall.ETIMEDOUT
    log.Printf("%s: timed out waiting to Recv(): %s\n",
      s.endpoint, err)
    return nil, err
  } else {
    data, err = s.socket.Recv(flags)
    if err != nil {
      log.Printf("%s: Failed to Recv() %d byte message: %s\n",
        s.endpoint, len(data), err)
      s.fail_socket()
      return nil, err
    } else {
      // Success!
    }
  }
  return
}

func (s *FFS) Close() (err error) {
  err = s.socket.Close()
  if err != nil {
    return
  }

  s.socket = nil
  s.connected = false
  return nil
}

func (s *FFS) ensure_connect() {
  if s.connected {
    return
  }

  if s.SendTimeout == 0 {
    s.SendTimeout = 1 * time.Second
  }
  if s.RecvTimeout == 0 {
    s.RecvTimeout = 1 * time.Second
  }

  if s.SocketType == 0 {
    log.Panicf("No socket type set on zmq socket")
  }
  if s.socket != nil {
    s.socket.Close()
    s.socket = nil
  }

  var err error
  s.socket, err = context.NewSocket(s.SocketType)
  if err != nil {
    log.Panicf("zmq.NewSocket(%d) failed: %s\n", s.SocketType, err)
  }

  //s.socket.SetSockOptUInt64(zmq.HWM, 1)
  //s.socket.SetSockOptInt(zmq.RCVTIMEO, int(s.RecvTimeout.Nanoseconds() / 1000000))
  //s.socket.SetSockOptInt(zmq.SNDTIMEO, int(s.SendTimeout.Nanoseconds() / 1000000))

  // Abort anything in-flight on a socket that's closed.
  s.socket.SetSockOptInt(zmq.LINGER, 0)

  for !s.connected {
    var max *big.Int = big.NewInt(int64(len(s.Endpoints)))
    i, _ := rand.Int(rand.Reader, max)
    s.endpoint = s.Endpoints[i.Int64()]
    log.Printf("Connecting to %s\n", s.endpoint)
    err := s.socket.Connect(s.endpoint)
    if err != nil {
      log.Printf("%s: Error connecting: %s\n", s.endpoint, err)
      time.Sleep(500 * time.Millisecond)
      continue
    }

    // No error, we're connected.
    s.connected = true
  }
}

func (s *FFS) fail_socket() {
  if !s.connected {
    return
  }
  s.Close()
}

func Publish(input chan []*FileEvent,
             registrar chan []*FileEvent,
             server_list []string,
             public_key [sodium.PUBLICKEYBYTES]byte,
             secret_key [sodium.SECRETKEYBYTES]byte,
             server_timeout time.Duration) {
  var buffer bytes.Buffer
  session := sodium.NewSession(public_key, secret_key)

  socket := FFS{
    Endpoints:   server_list,
    SocketType:  zmq.REQ,
    RecvTimeout: server_timeout,
    SendTimeout: server_timeout,
  }
  //defer socket.Close()

  for events := range input {
    // got a bunch of events, ship them out.
    //log.Printf("Publisher received %d events\n", len(events))

    data, _ := json.Marshal(events)
    // TODO(sissel): check error

    // Compress it
    // A new zlib writer  is used for every payload of events so that any
    // individual payload can be decompressed alone.
    // TODO(sissel): Make compression level tunable
    compressor, _ := zlib.NewWriterLevel(&buffer, 3)
    buffer.Truncate(0)
    _, err := compressor.Write(data)
    err = compressor.Flush()
    compressor.Close()

    //log.Printf("compressed %d bytes\n", buffer.Len())
    // TODO(sissel): check err
    // TODO(sissel): implement security/encryption/etc

    // Send full payload over zeromq REQ/REP
    // TODO(sissel): check error
    //buffer.Write(data)
    ciphertext, nonce := session.Box(buffer.Bytes())

    //log.Printf("plaintext: %d\n", len(data))
    //log.Printf("compressed: %d\n", buffer.Len())
    //log.Printf("ciphertext: %d %v\n", len(ciphertext), ciphertext[:20])
    //log.Printf("nonce: %d\n", len(nonce))

    // TODO(sissel): figure out encoding for ciphertext + nonce
    // TODO(sissel): figure out encoding for ciphertext + nonce

    // Loop forever trying to send.
    // This will cause reconnects/etc on failures automatically
    for {
      err = socket.Send(nonce, zmq.SNDMORE)
      if err != nil {
        continue // send failed, retry!
      }
      err = socket.Send(ciphertext, 0)
      if err != nil {
        continue // send failed, retry!
      }

      data, err = socket.Recv(0)
      // TODO(sissel): Figure out acknowledgement protocol? If any?
      if err == nil {
        break // success!
      }
    }

    // Tell the registrar that we've successfully sent these events
    //registrar <- events
  } /* for each event payload */
} // Publish
