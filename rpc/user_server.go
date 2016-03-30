package rpc

import (
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	rpc "github.com/keybase/go-framed-msgpack-rpc"
	protocol "github.com/keybase/gregor/protocol/go"
)

type connectionArgs struct {
	c  *connection
	id connectionID
}

type perUIDServer struct {
	uid        protocol.UID
	conns      map[connectionID]*connection
	lastConnID connectionID

	parentShutdownCh chan protocol.UID
	parentConfirmCh  chan confirmUIDShutdownArgs
	newConnectionCh  chan *connectionArgs
	sendBroadcastCh  chan messageArgs
	tryShutdownCh    chan bool
	closeListenCh    chan error
}

func newPerUIDServer(uid protocol.UID, parentShutdownCh chan protocol.UID, parentConfirmCh chan confirmUIDShutdownArgs) *perUIDServer {
	s := &perUIDServer{
		uid:              uid,
		conns:            make(map[connectionID]*connection),
		newConnectionCh:  make(chan *connectionArgs),
		sendBroadcastCh:  make(chan messageArgs),
		tryShutdownCh:    make(chan bool, 1), // buffered so it can receive inside serve()
		closeListenCh:    make(chan error),
		parentShutdownCh: parentShutdownCh,
		parentConfirmCh:  parentConfirmCh,
	}

	go s.serve()

	return s
}

func (s *perUIDServer) logError(prefix string, err error) {
	if err == nil {
		return
	}
	log.Printf("[uid %x] %s error: %s", s.uid, prefix, err)
}

func (s *perUIDServer) serve() {
	for {
		select {
		case a := <-s.newConnectionCh:
			s.logError("addConn", s.addConn(a))
		case a := <-s.sendBroadcastCh:
			s.broadcast(a)
		case <-s.closeListenCh:
			s.checkClosed()
			if s.tryShutdown() {
				return
			}
		case <-s.tryShutdownCh:
			if s.tryShutdown() {
				return
			}
		}
	}
}

func (s *perUIDServer) addConn(a *connectionArgs) error {
	a.c.xprt.AddCloseListener(s.closeListenCh)
	s.conns[a.id] = a.c
	s.lastConnID = a.id
	return nil
}

func (s *perUIDServer) broadcast(a messageArgs) {
	var errMsgs []string
	for id, conn := range s.conns {
		log.Printf("uid %x broadcast to %d", s.uid, id)
		oc := protocol.OutgoingClient{Cli: rpc.NewClient(conn.xprt, nil)}
		if err := oc.BroadcastMessage(a.c, a.m); err != nil {
			errMsgs = append(errMsgs, fmt.Sprintf("[connection %d]: %s", id, err))

			if s.isConnDown(err) {
				s.removeConnection(conn, id)
			}
		}
	}

	if len(errMsgs) == 0 {
		a.retCh <- nil
		return
	}
	a.retCh <- errors.New(strings.Join(errMsgs, ", "))

	if len(s.conns) == 0 {
		s.tryShutdownCh <- true
	}
}

// tryShutdown checks if it is ok to shutdown.  Returns true if it
// is ok.
func (s *perUIDServer) tryShutdown() bool {
	// make sure no connections have been added
	if len(s.conns) != 0 {
		log.Printf("tried shutdown, but %d conns for %x", len(s.conns), s.uid)
		return false
	}

	// confirm with the server that it is ok to shutdown
	ok := make(chan bool)
	args := confirmUIDShutdownArgs{
		uid:        s.uid,
		lastConnID: s.lastConnID,
		ok:         ok,
	}
	s.parentConfirmCh <- args
	if <-ok == false {
		log.Printf("tried shutdown, but parent server didn't allow it")
		return false
	}

	log.Printf("shutting down perUIDServer for %x", s.uid)
	// tell parent that the server for this uid is shutting down
	s.parentShutdownCh <- s.uid
	return true
}

func (s *perUIDServer) checkClosed() {
	log.Printf("uid server %x: received connection closed message, checking all connections", s.uid)
	for id, conn := range s.conns {
		if conn.xprt.IsConnected() {
			continue
		}
		log.Printf("uid server %x: connection %d closed", s.uid, id)
		s.removeConnection(conn, id)
	}
}

func (s *perUIDServer) isConnDown(err error) bool {
	if IsSocketClosedError(err) {
		return true
	}
	if err == io.EOF {
		return true
	}
	return false
}

func (s *perUIDServer) removeConnection(conn *connection, id connectionID) {
	log.Printf("uid server %x: removing connection %d", s.uid, id)
	conn.close()
	delete(s.conns, id)
}
